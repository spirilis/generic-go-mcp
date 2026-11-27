package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/spirilis/generic-go-mcp/config"
)

// fakeIdP is an OpenID Provider in-process: discovery, JWKS, token and userinfo endpoints over
// TLS, issuing real RS256 and ES256 id_tokens. The test plays the browser: it reads the
// authorization URL the library built and calls authorize to get a code.
type fakeIdP struct {
	t   *testing.T
	srv *httptest.Server

	rsaKey, rsaKey2 *rsa.PrivateKey
	ecKey           *ecdsa.PrivateKey
	clientID        string
	clientSecret    string

	mu          sync.Mutex
	down        bool                                // discovery and JWKS answer 500
	issuer      string                              // discovery's issuer; default the server URL
	jwksURI     string                              // discovery's jwks_uri; default <server>/jwks
	userinfoURI string                              // discovery's userinfo_endpoint; default <server>/userinfo
	rotated     bool                                // JWKS also publishes rsaKey2 ("rsa2")
	sub         string                              // subject of issued tokens
	claims      map[string]any                      // extra id_token claims
	mutate      func(h, c map[string]any) (key any) // last say over a token; returns the signing key
	userinfo    map[string]any                      // userinfo response; nil -> {sub}
	codes       map[string]fakeGrant                // issued codes
	lastAuth    string                              // client authentication seen at /token
	jwksHits    atomic.Int32                        //
	jwksGate    chan struct{}                       // when set, the next JWKS response waits for it to close, after reading the keys
	jwksHeld    chan struct{}                       // receives once a JWKS response is waiting on jwksGate
	discovery   atomic.Int32                        //
}

type fakeGrant struct{ nonce, challenge, redirectURI string }

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	return startFakeIdP(t, true)
}

// newPlainFakeIdP is the fake IdP over plain http on 127.0.0.1: a local IdP in development.
func newPlainFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	return startFakeIdP(t, false)
}

func startFakeIdP(t *testing.T, useTLS bool) *fakeIdP {
	t.Helper()
	idp := &fakeIdP{t: t, clientID: "mcp-client", clientSecret: "idp-secret", sub: "user-sub-1",
		codes: map[string]fakeGrant{}}
	var err error
	if idp.rsaKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		t.Fatal(err)
	}
	if idp.rsaKey2, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		t.Fatal(err)
	}
	if idp.ecKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", idp.handleDiscovery)
	mux.HandleFunc("/jwks", idp.gatedJWKS)
	mux.HandleFunc("/token", idp.handleToken)
	mux.HandleFunc("/userinfo", idp.handleUserinfo)
	if useTLS {
		idp.srv = httptest.NewTLSServer(mux)
	} else {
		idp.srv = httptest.NewServer(mux)
	}
	t.Cleanup(idp.srv.Close)
	return idp
}

func (idp *fakeIdP) set(fn func(*fakeIdP)) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	fn(idp)
}

// caFile writes the server's certificate where a provider's caFile can name it.
func (idp *fakeIdP) caFile() string {
	path := filepath.Join(idp.t.TempDir(), "ca.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.srv.Certificate().Raw})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		idp.t.Fatal(err)
	}
	return path
}

func (idp *fakeIdP) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	idp.discovery.Add(1)
	idp.mu.Lock()
	defer idp.mu.Unlock()
	if idp.down {
		http.Error(w, "down", http.StatusInternalServerError)
		return
	}
	iss, jwks, userinfo := idp.srv.URL, idp.srv.URL+"/jwks", idp.srv.URL+"/userinfo"
	if idp.issuer != "" {
		iss = idp.issuer
	}
	if idp.userinfoURI != "" {
		userinfo = idp.userinfoURI
	}
	if idp.jwksURI != "" {
		jwks = idp.jwksURI
	}
	writeJSON(w, map[string]any{
		"issuer":                 iss,
		"authorization_endpoint": idp.srv.URL + "/authorize?tenant=t1",
		"token_endpoint":         idp.srv.URL + "/token",
		"userinfo_endpoint":      userinfo,
		"jwks_uri":               jwks,
	})
}

// gatedJWKS answers like handleJWKS, but holds the response (already built from the keys of
// the moment) while jwksGate is set: a key fetch caught in flight.
func (idp *fakeIdP) gatedJWKS(w http.ResponseWriter, r *http.Request) {
	rec := httptest.NewRecorder()
	idp.handleJWKS(rec, r)
	idp.mu.Lock()
	gate, held := idp.jwksGate, idp.jwksHeld
	idp.jwksGate = nil // only the first request is held
	idp.mu.Unlock()
	if gate != nil {
		held <- struct{}{}
		<-gate
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

func (idp *fakeIdP) handleJWKS(w http.ResponseWriter, r *http.Request) {
	idp.jwksHits.Add(1)
	idp.mu.Lock()
	defer idp.mu.Unlock()
	if idp.down {
		http.Error(w, "down", http.StatusInternalServerError)
		return
	}
	b64 := base64.RawURLEncoding.EncodeToString
	rsaJWK := func(kid string, k *rsa.PublicKey) map[string]any {
		return map[string]any{"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
			"n": b64(k.N.Bytes()), "e": b64(big.NewInt(int64(k.E)).Bytes())}
	}
	point, _ := idp.ecKey.PublicKey.ECDH()
	raw := point.Bytes() // 0x04 || x || y
	keys := []any{
		rsaJWK("rsa1", &idp.rsaKey.PublicKey),
		map[string]any{"kty": "EC", "kid": "ec1", "crv": "P-256", "x": b64(raw[1:33]), "y": b64(raw[33:])},
		// Published by real IdPs alongside signing keys; must be skipped, not choked on.
		map[string]any{"kty": "RSA", "kid": "enc1", "use": "enc", "n": "AQAB", "e": "AQAB"},
		map[string]any{"kty": "OKP", "kid": "ed1", "crv": "Ed25519", "x": "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"},
	}
	if idp.rotated {
		keys = append(keys, rsaJWK("rsa2", &idp.rsaKey2.PublicKey))
	}
	writeJSON(w, map[string]any{"keys": keys})
}

// authorize plays the IdP's authorization page for the URL the library redirected to, and
// returns the code it would send back.
func (idp *fakeIdP) authorize(rawURL string) string {
	idp.t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil || !strings.HasPrefix(rawURL, idp.srv.URL+"/authorize?") {
		idp.t.Fatalf("not an authorization URL of this IdP: %q", rawURL)
	}
	q := u.Query()
	if q.Get("client_id") != idp.clientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
		idp.t.Fatalf("bad authorization request: %v", q)
	}
	return idp.grant(q.Get("nonce"), q.Get("code_challenge"), q.Get("redirect_uri"))
}

func (idp *fakeIdP) grant(nonce, challenge, redirectURI string) string {
	code := generateSecureToken(16)
	idp.set(func(idp *fakeIdP) { idp.codes[code] = fakeGrant{nonce, challenge, redirectURI} })
	return code
}

func (idp *fakeIdP) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "authorization_code" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid_request"})
		return
	}
	idp.mu.Lock()
	defer idp.mu.Unlock()

	// Client authentication, as the provider's tokenEndpointAuthMethod says.
	id, secret, basic := r.BasicAuth()
	switch {
	case basic:
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
		idp.lastAuth = "client_secret_basic"
	case r.PostForm.Get("client_secret") != "":
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		idp.lastAuth = "client_secret_post"
	default:
		id = r.PostForm.Get("client_id")
		idp.lastAuth = "none"
	}
	if id != idp.clientID || (idp.lastAuth != "none" && secret != idp.clientSecret) {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]string{"error": "invalid_client"})
		return
	}

	g, ok := idp.codes[r.PostForm.Get("code")]
	delete(idp.codes, r.PostForm.Get("code"))
	if !ok || g.redirectURI != r.PostForm.Get("redirect_uri") || s256(r.PostForm.Get("code_verifier")) != g.challenge {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid_grant"})
		return
	}

	now := time.Now().Unix()
	h := map[string]any{"alg": "RS256", "kid": "rsa1", "typ": "JWT"}
	c := map[string]any{"iss": idp.srv.URL, "sub": idp.sub, "aud": idp.clientID, "exp": now + 300,
		"iat": now, "nonce": g.nonce, "preferred_username": "alice", "email": "alice@example.com",
		"email_verified": true, "name": "Alice", "groups": []string{"mcp-admins", "staff"}}
	for k, v := range idp.claims {
		c[k] = v
	}
	var key any = idp.rsaKey
	if idp.mutate != nil {
		key = idp.mutate(h, c)
	}
	writeJSON(w, map[string]any{"access_token": "at-" + idp.sub, "token_type": "Bearer",
		"id_token": signJWT(h, c, key)})
}

func (idp *fakeIdP) handleUserinfo(w http.ResponseWriter, r *http.Request) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer at-"+idp.sub {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	info := idp.userinfo
	if info == nil {
		info = map[string]any{"sub": idp.sub}
	}
	writeJSON(w, info)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// signJWT signs a compact JWS. key: *rsa.PrivateKey (RS256), *ecdsa.PrivateKey (ES256),
// []byte (HS256), or nil (an empty signature, for alg none).
func signJWT(h, c map[string]any, key any) string {
	hj, _ := json.Marshal(h)
	cj, _ := json.Marshal(c)
	b64 := base64.RawURLEncoding.EncodeToString
	in := b64(hj) + "." + b64(cj)
	digest := sha256.Sum256([]byte(in))
	var sig []byte
	switch k := key.(type) {
	case *rsa.PrivateKey:
		sig, _ = rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, digest[:])
	case *ecdsa.PrivateKey:
		r, s, _ := ecdsa.Sign(rand.Reader, k, digest[:])
		sig = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	case []byte:
		mac := hmac.New(sha256.New, k)
		mac.Write([]byte(in))
		sig = mac.Sum(nil)
	}
	return in + "." + b64(sig)
}

// newOIDCService builds a service with the fake IdP as provider "corp" (and whatever mutate
// adds), with the provider's timing shortened so nothing sleeps for long.
func newOIDCService(t *testing.T, idp *fakeIdP, dbPath string, mutate func(*config.AuthConfig)) *AuthService {
	t.Helper()
	if dbPath == "" {
		dbPath = filepath.Join(t.TempDir(), "auth.db")
	}
	cfg := &config.AuthConfig{
		Enabled: true,
		Issuer:  testIssuer,
		Storage: config.StorageConfig{DBPath: dbPath},
		Providers: []config.ProviderConfig{{
			Name: "corp", Type: config.ProviderTypeOIDC, DisplayName: "Corp SSO",
			IssuerURL: idp.srv.URL, ClientID: idp.clientID, ClientSecret: idp.clientSecret,
			AuthParams: map[string]string{"prompt": "select_account"},
		}},
		Access: &config.AccessConfig{Rules: []config.AccessRule{
			{Provider: "corp", Groups: []string{"mcp-admins"}, Roles: []string{"admin", "writer"}},
		}},
	}
	if idp.srv.TLS != nil {
		cfg.Providers[0].CAFile = idp.caFile()
	}
	if mutate != nil {
		mutate(cfg)
	}
	svc, err := NewAuthService(cfg, WithRoles(testRoles...))
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}
	t.Cleanup(func() { svc.Close() })
	fastTiming(oidcOf(t, svc, "corp"))
	return svc
}

func oidcOf(t *testing.T, svc *AuthService, name string) *oidcProvider {
	t.Helper()
	p, ok := svc.providerByName[name].(*oidcProvider)
	if !ok {
		t.Fatalf("provider %q is %T", name, svc.providerByName[name])
	}
	return p
}

// fastTiming shortens a provider's waits. Only before its first use: the fetch loop reads them.
func fastTiming(p *oidcProvider) {
	p.minBackoff, p.maxBackoff = 10*time.Millisecond, 50*time.Millisecond
	p.readyWait = 2 * time.Second
}

func countBucket(t *testing.T, svc *AuthService, bucket string) int {
	t.Helper()
	n := 0
	if err := boltOf(t, svc).view(func(tx *bolt.Tx) error {
		n = tx.Bucket([]byte(bucket)).Stats().KeyN
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// exchange runs one code exchange against the provider directly, with a fresh verifier.
func exchange(t *testing.T, idp *fakeIdP, p *oidcProvider, grantNonce, expectNonce string) (Identity, error) {
	t.Helper()
	verifier := generateSecureToken(32)
	code := idp.grant(grantNonce, s256(verifier), testIssuer+"/callback")
	return p.Exchange(context.Background(), code, testIssuer+"/callback", verifier, expectNonce)
}

// Every id_token check, each failing on its own.
func TestOIDCIDTokenVerification(t *testing.T) {
	idp := newFakeIdP(t)
	svc := newOIDCService(t, idp, "", nil)
	p := oidcOf(t, svc, "corp")
	other, _ := rsa.GenerateKey(rand.Reader, 2048)

	tests := []struct {
		name    string
		mutate  func(h, c map[string]any) any
		nonce   string // expected by the service; "" = the one granted
		wantErr string
	}{
		{"RS256", nil, "", ""},
		{"ES256", func(h, c map[string]any) any { h["alg"], h["kid"] = "ES256", "ec1"; return idp.ecKey }, "", ""},
		{"no kid, key found by type", func(h, c map[string]any) any { delete(h, "kid"); return idp.rsaKey }, "", ""},
		{"several audiences, azp is us", func(h, c map[string]any) any {
			c["aud"], c["azp"] = []string{"api", idp.clientID}, idp.clientID
			return idp.rsaKey
		}, "", ""},
		{"expired within leeway", func(h, c map[string]any) any { c["exp"] = time.Now().Add(-30 * time.Second).Unix(); return idp.rsaKey }, "", ""},
		{"fractional dates", func(h, c map[string]any) any { c["iat"] = float64(time.Now().Unix()) + 0.5; return idp.rsaKey }, "", ""},

		{"wrong issuer", func(h, c map[string]any) any { c["iss"] = "https://evil.example.com"; return idp.rsaKey }, "", "issuer"},
		{"wrong audience", func(h, c map[string]any) any { c["aud"] = "someone-else"; return idp.rsaKey }, "", "audience"},
		{"several audiences, no azp", func(h, c map[string]any) any { c["aud"] = []string{"api", idp.clientID}; return idp.rsaKey }, "", "authorized party"},
		{"azp is another client", func(h, c map[string]any) any { c["azp"] = "other"; return idp.rsaKey }, "", "authorized party"},
		{"expired", func(h, c map[string]any) any { c["exp"] = time.Now().Add(-2 * time.Minute).Unix(); return idp.rsaKey }, "", "expired"},
		{"no exp", func(h, c map[string]any) any { delete(c, "exp"); return idp.rsaKey }, "", "no exp"},
		{"not yet valid", func(h, c map[string]any) any { c["nbf"] = time.Now().Add(2 * time.Minute).Unix(); return idp.rsaKey }, "", "not valid yet"},
		{"issued in the future", func(h, c map[string]any) any { c["iat"] = time.Now().Add(2 * time.Minute).Unix(); return idp.rsaKey }, "", "future"},
		{"bad nonce", nil, "another-nonce", "nonce"},
		{"no sub", func(h, c map[string]any) any { delete(c, "sub"); return idp.rsaKey }, "", "no sub"},
		{"unknown kid", func(h, c map[string]any) any { h["kid"] = "ghost"; return other }, "", "not in the provider's key set"},
		{"right kid, wrong key", func(h, c map[string]any) any { return other }, "", "does not verify"},
		{"alg none", func(h, c map[string]any) any { h["alg"] = "none"; return nil }, "", `algorithm "none"`},
		{"HS256 keyed with the public key", func(h, c map[string]any) any {
			h["alg"] = "HS256"
			return x509.MarshalPKCS1PublicKey(&idp.rsaKey.PublicKey)
		}, "", `algorithm "HS256"`},
		{"ES256 header on an RSA kid", func(h, c map[string]any) any { h["alg"] = "ES256"; return idp.ecKey }, "", "not in the provider's key set"},
		{"crit header", func(h, c map[string]any) any { h["crit"] = []string{"exp"}; return idp.rsaKey }, "", "critical"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutate := tt.mutate
			idp.set(func(idp *fakeIdP) {
				idp.mutate = func(h, c map[string]any) any {
					if mutate == nil {
						return idp.rsaKey
					}
					return mutate(h, c)
				}
			})
			nonce := tt.nonce
			if nonce == "" {
				nonce = "n1"
			}
			id, err := exchange(t, idp, p, "n1", nonce)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Exchange: %v", err)
				}
				if id.Subject != idp.sub || id.Provider != "corp" || id.Issuer != idp.srv.URL {
					t.Errorf("identity = %+v", id)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Exchange error = %v, want one mentioning %q", err, tt.wantErr)
			}
		})
	}
}

// A signature is over the header and payload as sent: a payload swapped in from another token
// does not verify.
func TestVerifyIDTokenForgedPayload(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	keys := []jwk{{kid: "k", key: &key.PublicKey}}
	now := time.Now().Unix()
	claims := func(sub string) map[string]any {
		return map[string]any{"iss": "https://i", "sub": sub, "aud": "c", "exp": now + 60, "iat": now, "nonce": "n"}
	}
	check := idTokenCheck{issuer: "https://i", clientID: "c", audiences: []string{"c"}, nonce: "n",
		algs: []string{"RS256"}, now: time.Now()}
	good := signJWT(map[string]any{"alg": "RS256", "kid": "k"}, claims("alice"), key)
	if _, err := verifyIDToken(good, keys, check); err != nil {
		t.Fatalf("genuine token: %v", err)
	}
	evil := strings.Split(signJWT(map[string]any{"alg": "RS256", "kid": "k"}, claims("mallory"), nil), ".")
	parts := strings.Split(good, ".")
	if _, err := verifyIDToken(parts[0]+"."+evil[1]+"."+parts[2], keys, check); err == nil ||
		!strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("forged payload: %v", err)
	}
}

// audience accepts one more aud value besides the client id.
func TestOIDCExtraAudience(t *testing.T) {
	idp := newFakeIdP(t)
	svc := newOIDCService(t, idp, "", func(c *config.AuthConfig) { c.Providers[0].Audience = "mcp-api" })
	p := oidcOf(t, svc, "corp")
	idp.set(func(idp *fakeIdP) {
		idp.mutate = func(h, c map[string]any) any { c["aud"] = "mcp-api"; return idp.rsaKey }
	})
	if _, err := exchange(t, idp, p, "n", "n"); err != nil {
		t.Fatalf("aud = audience: %v", err)
	}
}

// The token endpoint sees the configured client authentication, and the PKCE verifier (the
// fake IdP refuses a code whose verifier does not match its challenge).
func TestOIDCTokenEndpointAuthMethods(t *testing.T) {
	for _, method := range []string{"client_secret_basic", "client_secret_post", "none"} {
		t.Run(method, func(t *testing.T) {
			idp := newFakeIdP(t)
			svc := newOIDCService(t, idp, "", func(c *config.AuthConfig) {
				c.Providers[0].TokenEndpointAuthMethod = method
				if method == "none" {
					c.Providers[0].ClientSecret = ""
				}
			})
			p := oidcOf(t, svc, "corp")
			if _, err := exchange(t, idp, p, "n", "n"); err != nil {
				t.Fatalf("Exchange: %v", err)
			}
			if idp.lastAuth != method {
				t.Errorf("IdP saw %q", idp.lastAuth)
			}
			code := idp.grant("n", s256(generateSecureToken(32)), testIssuer+"/callback")
			if _, err := p.Exchange(context.Background(), code, testIssuer+"/callback", generateSecureToken(32), "n"); err == nil {
				t.Error("a code redeemed with the wrong PKCE verifier was accepted")
			}
		})
	}
}

// Claims are found by dotted paths; groups may be a string or an array; userinfo is merged
// when configured and must be about the same sub.
func TestOIDCClaimMapping(t *testing.T) {
	tests := []struct {
		name     string
		claims   config.ClaimsConfig
		userInfo bool
		token    map[string]any
		info     map[string]any
		want     Identity
		wantErr  string
	}{
		{name: "defaults", want: Identity{Username: "alice", Email: "alice@example.com", EmailVerified: true,
			Name: "Alice", Groups: []string{"mcp-admins", "staff"}}},
		{name: "nested path", claims: config.ClaimsConfig{Groups: "realm_access.roles"},
			token: map[string]any{"realm_access": map[string]any{"roles": []string{"r1", "r2"}}},
			want:  Identity{Username: "alice", Email: "alice@example.com", EmailVerified: true, Name: "Alice", Groups: []string{"r1", "r2"}}},
		{name: "namespaced claim with dots", claims: config.ClaimsConfig{Groups: "https://example.com/groups", Username: "cognito:username"},
			token: map[string]any{"https://example.com/groups": "solo", "cognito:username": "al"},
			want:  Identity{Username: "al", Email: "alice@example.com", EmailVerified: true, Name: "Alice", Groups: []string{"solo"}}},
		{name: "string email_verified", token: map[string]any{"email_verified": "true", "groups": 7},
			want: Identity{Username: "alice", Email: "alice@example.com", EmailVerified: true, Name: "Alice", Groups: []string{}}},
		{name: "unverified email", token: map[string]any{"email_verified": false},
			want: Identity{Username: "alice", Email: "alice@example.com", Name: "Alice", Groups: []string{"mcp-admins", "staff"}}},
		// email_verified is about the standard email claim, not whatever claims.email names.
		{name: "email from another claim is unverified", claims: config.ClaimsConfig{Email: "upn"},
			token: map[string]any{"upn": "boss@example.com", "email_verified": true},
			want:  Identity{Username: "alice", Email: "boss@example.com", Name: "Alice", Groups: []string{"mcp-admins", "staff"}}},
		{name: "userinfo merged", userInfo: true,
			info: map[string]any{"sub": "user-sub-1", "groups": []string{"from-userinfo"}, "iss": "https://evil.example.com"},
			want: Identity{Username: "alice", Email: "alice@example.com", EmailVerified: true, Name: "Alice", Groups: []string{"from-userinfo"}}},
		{name: "userinfo about someone else", userInfo: true, info: map[string]any{"sub": "user-sub-2"}, wantErr: "sub does not match"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idp := newFakeIdP(t)
			svc := newOIDCService(t, idp, "", func(c *config.AuthConfig) {
				c.Providers[0].Claims = tt.claims
				c.Providers[0].UserInfo = tt.userInfo
			})
			idp.set(func(idp *fakeIdP) { idp.claims, idp.userinfo = tt.token, tt.info })
			id, err := exchange(t, idp, oidcOf(t, svc, "corp"), "n", "n")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.want.Provider, tt.want.Issuer, tt.want.Subject = "corp", idp.srv.URL, "user-sub-1"
			if !jsonEqual(id, tt.want) {
				t.Errorf("identity = %+v\nwant       %+v", id, tt.want)
			}
		})
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// A discovery document that names another issuer, or keys on another host, disables the
// provider for good: no retries, every sign-in 503.
func TestOIDCDiscoveryRefused(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(idp *fakeIdP)
		wantErr string
	}{
		{"issuer mismatch", func(idp *fakeIdP) { idp.issuer = idp.srv.URL + "/" }, "not the configured issuerURL"},
		{"jwks on another host", func(idp *fakeIdP) {
			idp.jwksURI = strings.Replace(idp.srv.URL, "127.0.0.1", "localhost", 1) + "/jwks"
		}, "not the issuer's host"},
		{"jwks over http", func(idp *fakeIdP) {
			idp.jwksURI = strings.Replace(idp.srv.URL, "https://", "http://", 1) + "/jwks"
		}, "must be an https URL"},
		// Its claims are merged unsigned over the id_token's, so it is held to the keys' hosts.
		{"userinfo on another host", func(idp *fakeIdP) {
			idp.userinfoURI = "https://attacker.example/userinfo"
		}, "userinfo_endpoint host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idp := newFakeIdP(t)
			idp.set(tt.setup)
			svc := newOIDCService(t, idp, "", func(c *config.AuthConfig) { c.Providers[0].UserInfo = true })
			p := oidcOf(t, svc, "corp")
			for i := 0; i < 3; i++ {
				_, err := p.AuthCodeURL(context.Background(), testIssuer+"/callback", "s", "n", "c")
				if !errors.Is(err, ErrProviderUnavailable) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("AuthCodeURL error = %v, want unavailable: %q", err, tt.wantErr)
				}
			}
			time.Sleep(100 * time.Millisecond) // several backoffs' worth
			if n := idp.discovery.Load(); n != 1 {
				t.Errorf("discovery fetched %d times; a fatal mismatch must not be retried", n)
			}
			if n := idp.jwksHits.Load(); n != 0 {
				t.Errorf("JWKS fetched %d times from an untrusted document", n)
			}
		})
	}
}

func TestOIDCJWKSHostCheck(t *testing.T) {
	const https, local = "https://login.example.com/realms/x", "http://127.0.0.1:8081/realms/x"
	tests := []struct {
		issuer, uri string
		ok          bool
	}{
		{https, "https://login.example.com/certs", true},
		{https, "https://LOGIN.example.com:8443/certs", true},
		{https, "https://keys.example.net/jwks", true},
		{https, "https://evil.example.com/certs", false},
		{https, "https://login.example.com.evil.com/certs", false},
		{https, "http://login.example.com/certs", false},
		{https, "", false},
		// A local http IdP may serve http on loopback; an https issuer may not be pointed at it.
		{local, "http://127.0.0.1:8081/certs", true},
		{local, "http://localhost:8081/certs", false}, // not the issuer's host
		{https, "http://127.0.0.1/certs", false},
	}
	for _, tt := range tests {
		p := &oidcProvider{cfg: config.ProviderConfig{IssuerURL: tt.issuer, AllowedJWKSHosts: []string{"keys.example.net"}}}
		if err := p.checkTrustedHost("jwks_uri", tt.uri); (err == nil) != tt.ok {
			t.Errorf("%s %q: err = %v, want ok = %v", tt.issuer, tt.uri, err, tt.ok)
		}
	}
}

// A local IdP over plain http on loopback works end to end, userinfo included: its issuerURL is
// http on loopback, so its endpoints may be too.
func TestOIDCLocalHTTPIdP(t *testing.T) {
	idp := newPlainFakeIdP(t)
	svc := newOIDCService(t, idp, "", func(c *config.AuthConfig) { c.Providers[0].UserInfo = true })
	p := oidcOf(t, svc, "corp")
	authURL, err := p.AuthCodeURL(context.Background(), testIssuer+"/callback", "s", "n", "c")
	if err != nil || !strings.HasPrefix(authURL, "http://127.0.0.1:") {
		t.Fatalf("AuthCodeURL = %q, %v", authURL, err)
	}
	id, err := exchange(t, idp, p, "n", "n")
	if err != nil || id.Subject != "user-sub-1" || id.Issuer != idp.srv.URL {
		t.Fatalf("identity = %+v, %v", id, err)
	}
}

// The authorization and token endpoints may be on any host, but http (even on loopback) only
// for a local http issuer: a discovery document must not aim the client secret at local services.
func TestOIDCEndpointScheme(t *testing.T) {
	const https, local = "https://login.example.com", "http://localhost:8081"
	tests := []struct {
		issuer, uri string
		ok          bool
	}{
		{https, "https://oauth2.example.net/token", true},
		{https, "http://127.0.0.1:9000/token", false},
		{https, "http://login.example.com/token", false},
		{local, "http://127.0.0.1:9000/token", true},
		{local, "http://idp.example.com/token", false},
		{https, "/token", false},
	}
	for _, tt := range tests {
		p := &oidcProvider{cfg: config.ProviderConfig{IssuerURL: tt.issuer}}
		if err := p.checkEndpoint(tt.uri); (err == nil) != tt.ok {
			t.Errorf("%s %q: err = %v, want ok = %v", tt.issuer, tt.uri, err, tt.ok)
		}
	}
}

// The IdP down when the first sign-in comes: 503 (and no stored request), retried in the
// background with backoff, and sign-in works once it is back. The service started regardless.
func TestOIDCProviderRecovers(t *testing.T) {
	idp := newFakeIdP(t)
	idp.set(func(idp *fakeIdP) { idp.down = true })
	svc := newOIDCService(t, idp, "", nil)
	c := storeTestClient(t, svc)

	if rec := startAuthorize(t, svc, c); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("IdP down: status = %d %s, want 503", rec.Code, rec.Body)
	}
	if n := countBucket(t, svc, BucketAuthRequests); n != 0 {
		t.Fatalf("%d requests stored while the IdP is down", n)
	}

	idp.set(func(idp *fakeIdP) { idp.down = false })
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := startAuthorize(t, svc, c)
		if rec.Code == http.StatusFound {
			idp.authorize(rec.Header().Get("Location")) // a well-formed request to the IdP
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider never recovered: %d %s", rec.Code, rec.Body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := idp.discovery.Load(); n < 2 {
		t.Errorf("discovery fetched %d times, want retries", n)
	}
}

// A token signed with a key the provider has not seen makes it refresh the JWKS, at most once
// per interval, so made-up kids cannot turn into a flood of key fetches.
func TestOIDCKeyRotation(t *testing.T) {
	idp := newFakeIdP(t)
	svc := newOIDCService(t, idp, "", nil)
	p := oidcOf(t, svc, "corp")
	now := time.Now()
	p.now = func() time.Time { return now }
	if _, err := exchange(t, idp, p, "n", "n"); err != nil {
		t.Fatal(err)
	}
	hits := idp.jwksHits.Load()

	// The IdP rotates: the new key is fetched on demand.
	idp.set(func(idp *fakeIdP) {
		idp.rotated = true
		idp.mutate = func(h, c map[string]any) any { h["kid"] = "rsa2"; return idp.rsaKey2 }
	})
	if _, err := exchange(t, idp, p, "n", "n"); err != nil {
		t.Fatalf("token from the rotated key: %v", err)
	}
	if got := idp.jwksHits.Load(); got != hits+1 {
		t.Fatalf("JWKS fetched %d more times, want 1", got-hits)
	}

	// A made-up kid within the interval: refused without another fetch.
	ghost, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp.set(func(idp *fakeIdP) {
		idp.mutate = func(h, c map[string]any) any { h["kid"] = "ghost"; return ghost }
	})
	if _, err := exchange(t, idp, p, "n", "n"); !errors.Is(err, errUnknownKID) {
		t.Fatalf("unknown kid: %v", err)
	}
	if got := idp.jwksHits.Load(); got != hits+1 {
		t.Fatalf("JWKS fetched again within the rate limit")
	}

	// After the interval, one more fetch, still refused.
	now = now.Add(2 * time.Minute)
	if _, err := exchange(t, idp, p, "n", "n"); !errors.Is(err, errUnknownKID) {
		t.Fatalf("unknown kid: %v", err)
	}
	if got := idp.jwksHits.Load(); got != hits+2 {
		t.Fatalf("JWKS fetched %d more times, want 2", got-hits)
	}
}

// An unknown kid must be looked up by a fetch that began after it was seen. A fetch already in
// flight may have read the key set from before the rotation; settling for it would refuse the
// new key, and the rate limit would then refuse it for a minute.
func TestOIDCKeyRefreshIgnoresFetchInFlight(t *testing.T) {
	idp := newFakeIdP(t)
	svc := newOIDCService(t, idp, "", nil)
	p := oidcOf(t, svc, "corp")
	if _, err := exchange(t, idp, p, "n", "n"); err != nil {
		t.Fatal(err)
	}

	// A periodic refresh reads the old key set, and is held before it is published.
	gate, held := make(chan struct{}), make(chan struct{}, 1)
	idp.set(func(idp *fakeIdP) { idp.jwksGate, idp.jwksHeld = gate, held })
	p.kick <- struct{}{}
	<-held

	// The IdP rotates meanwhile, and a token signed with the new key arrives.
	idp.set(func(idp *fakeIdP) {
		idp.rotated = true
		idp.mutate = func(h, c map[string]any) any { h["kid"] = "rsa2"; return idp.rsaKey2 }
	})
	result := make(chan error, 1)
	go func() {
		_, err := exchange(t, idp, p, "n", "n")
		result <- err
	}()
	// Once the exchange has asked for a refresh, let the stale fetch finish.
	for asked := false; !asked; {
		p.mu.Lock()
		asked = !p.lastKidRefresh.IsZero()
		p.mu.Unlock()
		if !asked {
			time.Sleep(time.Millisecond)
		}
	}
	close(gate)
	if err := <-result; err != nil {
		t.Fatalf("token from the rotated key: %v", err)
	}
}

func TestParseJWKS(t *testing.T) {
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	b64 := base64.RawURLEncoding.EncodeToString
	tests := []struct {
		name, doc, wantErr string
		wantKeys           int
	}{
		{"skips what it cannot use", `{"keys":[{"kty":"oct","k":"c2VjcmV0"},{"kty":"EC","crv":"P-384","x":"AA","y":"AA"},` +
			`{"kty":"RSA","use":"enc","n":"AQAB","e":"AQAB"}]}`, "no RS256 or ES256", 0},
		{"small RSA key", `{"keys":[{"kty":"RSA","kid":"s","n":"` + b64(small.N.Bytes()) + `","e":"AQAB"}]}`, "below 2048", 0},
		{"point off the curve", `{"keys":[{"kty":"EC","crv":"P-256","x":"` + b64(make([]byte, 32)) + `","y":"` +
			b64(append(make([]byte, 31), 1)) + `"}]}`, "jwks: key 0", 0},
		{"not JSON", `nope`, "jwks", 0},
	}
	for _, tt := range tests {
		keys, err := parseJWKS([]byte(tt.doc))
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%s: error = %v, want %q", tt.name, err, tt.wantErr)
			}
			continue
		}
		if err != nil || len(keys) != tt.wantKeys {
			t.Errorf("%s: %d keys, %v", tt.name, len(keys), err)
		}
	}
}

func storeTestClient(t *testing.T, svc *AuthService) *RegisteredClient {
	t.Helper()
	// Static and fresh, or the startup sweep may delete it as an unused registration.
	c := &RegisteredClient{ClientID: "c1", RedirectURIs: []string{"https://app.example.com/cb"}, ClientName: "App",
		IsStatic: true, CreatedAt: time.Now()}
	if err := svc.storage.StoreClient(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func startAuthorize(t *testing.T, svc *AuthService, c *RegisteredClient) *httptest.ResponseRecorder {
	t.Helper()
	return authorizeGET(t, svc, url.Values{"response_type": {"code"}, "client_id": {c.ClientID},
		"redirect_uri": {c.RedirectURIs[0]}, "state": {"client-state"},
		"code_challenge": {s256(strings.Repeat("v", 43))}, "code_challenge_method": {"S256"}})
}

// Provider responses are capped, redirects are not followed (they would carry the request past
// the host checks), and only 200 is accepted.
func TestOIDCFetchLimits(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat(" ", maxProviderResponseBytes+1)))
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ok", http.StatusFound)
	})
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{}")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	bg := newBackground()
	defer bg.Close()
	p, err := newOIDCProvider(config.ProviderConfig{Name: "x", IssuerURL: srv.URL}, bg)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ path, wantErr string }{
		{"/ok", ""},
		{"/big", "exceeds"},
		{"/redirect", "HTTP 302"},
	}
	for _, tt := range tests {
		_, err := p.get(context.Background(), srv.URL+tt.path)
		if (tt.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
			t.Errorf("%s: err = %v, want %q", tt.path, err, tt.wantErr)
		}
	}
}

// caFile is a local file the operator controls: unreadable or not PEM refuses to start, unlike
// an unreachable IdP.
func TestOIDCBadCAFileRefusesStart(t *testing.T) {
	notPEM := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ca := range []string{filepath.Join(t.TempDir(), "missing.pem"), notPEM} {
		_, err := NewAuthService(&config.AuthConfig{
			Storage: config.StorageConfig{DBPath: filepath.Join(t.TempDir(), "auth.db")},
			Providers: []config.ProviderConfig{{Name: "corp", Type: "oidc", IssuerURL: "https://login.example.com",
				ClientID: "c", CAFile: ca}},
			Access: &config.AccessConfig{Rules: []config.AccessRule{{Provider: "corp", Anyone: true}}},
		})
		if err == nil || !strings.Contains(err.Error(), "caFile") {
			t.Errorf("caFile %s: err = %v", ca, err)
		}
	}
}
