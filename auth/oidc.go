package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spirilis/generic-go-mcp/config"
	"github.com/spirilis/generic-go-mcp/logging"
)

// Timing of discovery and key fetching. The fields of oidcProvider that carry them exist so
// tests need not sleep.
const (
	oidcFetchTimeout       = 10 * time.Second // one discovery or JWKS request
	oidcMinBackoff         = 1 * time.Second  // first retry after a failed fetch
	oidcMaxBackoff         = 5 * time.Minute
	oidcKeysMaxAge         = 1 * time.Hour   // JWKS refreshed this often
	oidcKidRefreshInterval = 1 * time.Minute // at most one unknown-kid refresh per interval
	oidcReadyWait          = 5 * time.Second // how long a sign-in waits for a provider starting up
)

// oidcDiscovery is the part of an OpenID Provider's discovery document the library uses.
type oidcDiscovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// fatalProviderError is a discovery failure that retrying cannot fix without the operator: the
// document names another issuer, or keys on a host it may not use. The provider stays
// unavailable until the server restarts with a corrected config (or IdP).
type fatalProviderError struct{ err error }

func (e fatalProviderError) Error() string { return e.err.Error() }
func (e fatalProviderError) Unwrap() error { return e.err }

// oidcProvider signs users in with any OpenID Connect provider: authorization code flow with
// PKCE (S256) and a nonce, id_token verified against the provider's published keys.
//
// Discovery and the key set are fetched lazily: the first sign-in starts a goroutine (on the
// service's context, not the request's) that fetches them, retries with backoff while the IdP
// is unreachable, then refreshes the keys every hour and, at most once a minute, when a token
// names a key it does not have. Requests only wait on that goroutine, briefly; a provider that
// is not ready answers ErrProviderUnavailable, so an IdP outage never stops the server starting
// and never ties up request handlers.
type oidcProvider struct {
	cfg    config.ProviderConfig
	client *http.Client
	bg     *background
	start  sync.Once
	kick   chan struct{} // asks the fetch loop for a key refresh now

	// Timing; the oidc* constants unless a test changes them before first use.
	fetchTimeout, minBackoff, maxBackoff, keysMaxAge, kidRefreshInterval, readyWait time.Duration
	now                                                                             func() time.Time

	mu             sync.Mutex
	meta           *oidcDiscovery
	keys           []jwk
	fatal          error         // set once; the provider then never becomes ready
	lastErr        error         // the last fetch failure, for logs and 503 detail
	changed        chan struct{} // closed (and replaced) after every fetch attempt
	lastKidRefresh time.Time

	// Fetch generations: fetchStarted counts fetches begun, fetchDone is the generation of the
	// last one finished. refreshKeys waits for a fetch that began after it asked, since one
	// already in flight may have read the key set before the IdP rotated.
	fetchStarted, fetchDone uint64
}

func newOIDCProvider(p config.ProviderConfig, bg *background) (*oidcProvider, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if p.CAFile != "" {
		// A local file the operator controls: unlike an unreachable IdP, failing to read it is
		// a configuration error, and refuses to start.
		pem, err := os.ReadFile(p.CAFile)
		if err != nil {
			return nil, fmt.Errorf("auth provider %q: caFile: %w", p.Name, err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("auth provider %q: caFile %s holds no PEM certificate", p.Name, p.CAFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &oidcProvider{
		cfg:                p,
		client:             &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: noRedirects},
		bg:                 bg,
		kick:               make(chan struct{}, 1),
		fetchTimeout:       oidcFetchTimeout,
		minBackoff:         oidcMinBackoff,
		maxBackoff:         oidcMaxBackoff,
		keysMaxAge:         oidcKeysMaxAge,
		kidRefreshInterval: oidcKidRefreshInterval,
		readyWait:          oidcReadyWait,
		now:                time.Now,
		changed:            make(chan struct{}),
	}, nil
}

func (p *oidcProvider) Name() string        { return p.cfg.Name }
func (p *oidcProvider) DisplayName() string { return p.cfg.DisplayName }
func (p *oidcProvider) Type() string        { return config.ProviderTypeOIDC }

// ready returns the discovery document and keys, starting the fetch loop on first use and
// waiting up to readyWait (or until ctx ends) for a fetch in progress.
func (p *oidcProvider) ready(ctx context.Context) (*oidcDiscovery, []jwk, error) {
	p.start.Do(func() { p.bg.Go(p.run) })
	deadline := time.NewTimer(p.readyWait)
	defer deadline.Stop()
	for waited := false; ; waited = true {
		p.mu.Lock()
		meta, keys, fatal, lastErr, changed := p.meta, p.keys, p.fatal, p.lastErr, p.changed
		p.mu.Unlock()
		switch {
		case meta != nil && keys != nil:
			return meta, keys, nil
		case fatal != nil:
			return nil, nil, fmt.Errorf("%w: %s: %v", ErrProviderUnavailable, p.cfg.Name, fatal)
		case waited || lastErr != nil:
			// An attempt has failed (while we waited, or before): the next is only after a
			// backoff, longer than a browser should hang during an outage.
			return nil, nil, fmt.Errorf("%w: %s: %v", ErrProviderUnavailable, p.cfg.Name, lastErr)
		}
		select {
		case <-changed:
		case <-deadline.C:
			return nil, nil, fmt.Errorf("%w: %s: still fetching its configuration", ErrProviderUnavailable, p.cfg.Name)
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("%w: %s: %v", ErrProviderUnavailable, p.cfg.Name, ctx.Err())
		}
	}
}

// run is the fetch loop: discovery and keys until they succeed (backing off between failures),
// then a key refresh every keysMaxAge or when kicked.
func (p *oidcProvider) run(ctx context.Context) {
	backoff := p.minBackoff
	for {
		err := p.fetch(ctx)
		var fatal fatalProviderError
		if errors.As(err, &fatal) {
			logging.Error("OIDC provider disabled: its discovery document cannot be trusted; fix it and restart",
				"provider", p.cfg.Name, "error", err)
			return
		}
		wait, kick := p.keysMaxAge, p.kick
		if err != nil {
			logging.Warn("OIDC provider fetch failed; retrying", "provider", p.cfg.Name, "in", backoff, "error", err)
			wait, kick = backoff, nil // a kick must not cut a backoff short
			backoff = min(2*backoff, p.maxBackoff)
		} else {
			backoff = p.minBackoff
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-kick:
			t.Stop()
		}
	}
}

// fetch reads the discovery document (until it has one) and the key set, then wakes waiters.
// A failed key refresh keeps the keys already held.
func (p *oidcProvider) fetch(ctx context.Context) (err error) {
	ctx, cancel := context.WithTimeout(ctx, p.fetchTimeout)
	defer cancel()

	p.mu.Lock()
	p.fetchStarted++
	gen, meta := p.fetchStarted, p.meta
	p.mu.Unlock()

	// What this fetch learnt is published together with its generation and the wake-up, so a
	// waiter never sees the new state without knowing which fetch produced it.
	var newMeta *oidcDiscovery
	var keys []jwk
	defer func() {
		p.mu.Lock()
		if newMeta != nil {
			p.meta = newMeta // kept even if the keys failed: the retry need not rediscover
		}
		if err == nil {
			p.keys = keys
		}
		p.lastErr = err
		var fatal fatalProviderError
		if errors.As(err, &fatal) {
			p.fatal = err
		}
		p.fetchDone = gen
		close(p.changed)
		p.changed = make(chan struct{})
		p.mu.Unlock()
	}()

	if meta == nil {
		if newMeta, err = p.discover(ctx); err != nil {
			return err
		}
		meta = newMeta
	}

	body, err := p.get(ctx, meta.JWKSURI)
	if err != nil {
		return fmt.Errorf("jwks: %w", err)
	}
	keys, err = parseJWKS(body)
	return err
}

// discover fetches and checks the discovery document.
func (p *oidcProvider) discover(ctx context.Context) (*oidcDiscovery, error) {
	body, err := p.get(ctx, strings.TrimSuffix(p.cfg.IssuerURL, "/")+"/.well-known/openid-configuration")
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	var d oidcDiscovery
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	// OIDC Discovery 4.3: the document must name exactly the issuer it was fetched for. id_tokens
	// are checked against the configured issuerURL, so a mismatch here is either a typo
	// (trailing slash, http vs https) or a document that is not the IdP's.
	if d.Issuer != p.cfg.IssuerURL {
		return nil, fatalProviderError{fmt.Errorf("discovery: issuer %q is not the configured issuerURL %q", d.Issuer, p.cfg.IssuerURL)}
	}
	if err := p.checkTrustedHost("jwks_uri", d.JWKSURI); err != nil {
		return nil, fatalProviderError{err}
	}
	// The userinfo response is unsigned and its claims are merged over the id_token's, so it
	// decides admission as much as the keys do and is held to the same hosts.
	if p.cfg.UserInfo {
		if err := p.checkTrustedHost("userinfo_endpoint", d.UserinfoEndpoint); err != nil {
			return nil, fatalProviderError{fmt.Errorf("userInfo is enabled: %w", err)}
		}
	}
	// The browser is sent to the authorization endpoint and the code (with the client secret)
	// to the token endpoint, but what comes back is only trusted once the id_token verifies,
	// so these may be on any host: some IdPs serve them from one other than the issuer's.
	for _, ep := range []struct{ name, url string }{
		{"authorization_endpoint", d.AuthorizationEndpoint},
		{"token_endpoint", d.TokenEndpoint},
	} {
		if err := p.checkEndpoint(ep.url); err != nil {
			return nil, fmt.Errorf("discovery: %s: %w", ep.name, err)
		}
	}
	return &d, nil
}

// checkTrustedHost holds an endpoint to the issuer's host, or a host the operator listed in
// allowedJWKSHosts. Keys are the whole of id_token trust: a discovery document that can point
// them anywhere would make a compromise of that document enough to forge logins. Hosts are
// compared without their port.
func (p *oidcProvider) checkTrustedHost(field, raw string) error {
	if err := p.checkEndpoint(raw); err != nil {
		return fmt.Errorf("discovery: %s: %w", field, err)
	}
	u, _ := url.Parse(raw)
	iss, _ := url.Parse(p.cfg.IssuerURL)
	host := strings.ToLower(u.Hostname())
	if host == strings.ToLower(iss.Hostname()) || slices.Contains(p.cfg.AllowedJWKSHosts, host) {
		return nil
	}
	return fmt.Errorf("discovery: %s host %q is not the issuer's host; list it in allowedJWKSHosts if it is genuine", field, host)
}

// checkEndpoint accepts an absolute https URL. http is accepted only on a loopback host, and
// only when the issuerURL itself is http on loopback (a local IdP in development): otherwise a
// discovery document could aim codes, secrets and bearer tokens at local services.
func (p *oidcProvider) checkEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not an absolute URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if iss, err := url.Parse(p.cfg.IssuerURL); err == nil && iss.Scheme == "http" && isLoopback(iss) && isLoopback(u) {
			return nil
		}
	}
	return fmt.Errorf("%q must be an https URL", raw)
}

// get fetches a provider document: 200 only, body capped.
func (p *oidcProvider) get(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := readCapped(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", rawURL, resp.StatusCode)
	}
	return body, nil
}

// refreshKeys asks the fetch loop for the key set now, unless it was asked within
// kidRefreshInterval, and waits for the result. It reports whether a refresh happened.
// The rate limit stops tokens with made-up kids from turning into a flood of JWKS requests.
func (p *oidcProvider) refreshKeys(ctx context.Context) bool {
	p.mu.Lock()
	if !p.lastKidRefresh.IsZero() && p.now().Sub(p.lastKidRefresh) < p.kidRefreshInterval {
		p.mu.Unlock()
		return false
	}
	p.lastKidRefresh = p.now()
	want, changed := p.fetchStarted+1, p.changed
	p.mu.Unlock()

	select {
	case p.kick <- struct{}{}:
	default: // a refresh is already queued
	}
	t := time.NewTimer(p.readyWait)
	defer t.Stop()
	for {
		select {
		case <-changed:
		case <-t.C:
			return false
		case <-ctx.Done():
			return false
		}
		p.mu.Lock()
		done := p.fetchDone >= want
		changed = p.changed
		p.mu.Unlock()
		if done {
			return true
		}
	}
}

// AuthCodeURL builds the authorization request: code flow, PKCE S256, nonce, and the
// operator's extra parameters (which config already kept off the reserved names).
func (p *oidcProvider) AuthCodeURL(ctx context.Context, callback, state, nonce, pkceChallenge string) (string, error) {
	meta, _, err := p.ready(ctx)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(meta.AuthorizationEndpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for k, v := range p.cfg.AuthParams {
		q.Set(k, v)
	}
	q.Set("response_type", "code")
	q.Set("client_id", p.cfg.ClientID)
	q.Set("redirect_uri", callback)
	q.Set("scope", strings.Join(p.cfg.Scopes, " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", pkceChallenge)
	q.Set("code_challenge_method", PKCEMethodS256)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Exchange redeems the code at the token endpoint, verifies the id_token, merges the userinfo
// claims when configured, and maps the claims to an Identity.
func (p *oidcProvider) Exchange(ctx context.Context, code, callback, verifier, nonce string) (Identity, error) {
	meta, keys, err := p.ready(ctx)
	if err != nil {
		return Identity{}, err
	}
	tok, err := p.redeem(ctx, meta, code, callback, verifier)
	if err != nil {
		return Identity{}, err
	}

	audiences := []string{p.cfg.ClientID}
	if p.cfg.Audience != "" {
		audiences = append(audiences, p.cfg.Audience)
	}
	check := idTokenCheck{
		issuer:    p.cfg.IssuerURL,
		clientID:  p.cfg.ClientID,
		audiences: audiences,
		nonce:     nonce,
		algs:      p.cfg.AllowedAlgorithms,
		now:       p.now(),
	}
	claims, err := verifyIDToken(tok.IDToken, keys, check)
	if errors.Is(err, errUnknownKID) && p.refreshKeys(ctx) {
		p.mu.Lock()
		keys = p.keys
		p.mu.Unlock()
		claims, err = verifyIDToken(tok.IDToken, keys, check)
	}
	if err != nil {
		return Identity{}, err
	}

	if p.cfg.UserInfo {
		if err := p.mergeUserInfo(ctx, meta, tok.AccessToken, claims); err != nil {
			return Identity{}, err
		}
	}
	return p.identity(claims), nil
}

type oidcTokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// redeem posts the code (and the PKCE verifier) to the token endpoint, authenticating as the
// configured tokenEndpointAuthMethod says.
func (p *oidcProvider) redeem(ctx context.Context, meta *oidcDiscovery, code, callback, verifier string) (*oidcTokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {callback},
		"code_verifier": {verifier},
	}
	switch p.cfg.TokenEndpointAuthMethod {
	case "client_secret_post":
		form.Set("client_id", p.cfg.ClientID)
		form.Set("client_secret", p.cfg.ClientSecret)
	case "none":
		form.Set("client_id", p.cfg.ClientID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if p.cfg.TokenEndpointAuthMethod == "client_secret_basic" {
		// RFC 6749 2.3.1: both are form-urlencoded before they go into the header.
		req.SetBasicAuth(url.QueryEscape(p.cfg.ClientID), url.QueryEscape(p.cfg.ClientSecret))
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, err := readCapped(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("token endpoint: %w", err)
	}
	var tok oidcTokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("token endpoint: HTTP %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || tok.Error != "" {
		return nil, fmt.Errorf("token endpoint: HTTP %d: %s %s", resp.StatusCode, tok.Error, tok.ErrorDesc)
	}
	if tok.IDToken == "" {
		return nil, errors.New("token endpoint returned no id_token (is the openid scope granted?)")
	}
	return &tok, nil
}

// idTokenOnlyClaims are the claims that describe the id_token itself; userinfo cannot change them.
var idTokenOnlyClaims = map[string]bool{
	"iss": true, "sub": true, "aud": true, "exp": true, "iat": true, "nbf": true,
	"nonce": true, "azp": true, "auth_time": true, "at_hash": true, "c_hash": true, "jti": true,
}

// mergeUserInfo adds the userinfo endpoint's claims to claims. Its sub must be the id_token's
// (OIDC Core 5.3.4): the access token could otherwise belong to someone else.
func (p *oidcProvider) mergeUserInfo(ctx context.Context, meta *oidcDiscovery, accessToken string, claims map[string]any) error {
	if accessToken == "" {
		return errors.New("userinfo: the token endpoint returned no access_token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, meta.UserinfoEndpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("userinfo: %w", err)
	}
	defer resp.Body.Close()
	body, err := readCapped(resp.Body)
	if err != nil {
		return fmt.Errorf("userinfo: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("userinfo: HTTP %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/jwt") {
		return errors.New("userinfo: signed (application/jwt) responses are not supported")
	}
	var info map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&info); err != nil {
		return fmt.Errorf("userinfo: %w", err)
	}
	if sub, _ := info["sub"].(string); sub == "" || sub != claims["sub"] {
		return errors.New("userinfo: sub does not match the id_token")
	}
	for k, v := range info {
		if !idTokenOnlyClaims[k] {
			claims[k] = v
		}
	}
	return nil
}

// identity maps verified claims to an Identity through the configured claim paths.
func (p *oidcProvider) identity(claims map[string]any) Identity {
	c := p.cfg.Claims
	id := Identity{
		Provider: p.cfg.Name,
		Issuer:   p.cfg.IssuerURL,
		Groups:   []string{},
	}
	id.Subject, _ = claims["sub"].(string)
	if v, ok := claimPath(claims, c.Username); ok {
		id.Username = claimString(v)
	}
	if v, ok := claimPath(claims, c.Email); ok {
		id.Email = claimString(v)
	}
	if v, ok := claimPath(claims, c.Name); ok {
		id.Name = claimString(v)
	}
	// email_verified speaks for the standard email claim only; another claim mapped as the
	// email (upn, mail) is not what it verified.
	if v, ok := claims["email_verified"]; ok && c.Email == "email" {
		id.EmailVerified = claimBool(v)
	}
	id.AvatarURL, _ = claims["picture"].(string)
	if v, ok := claimPath(claims, c.Groups); ok {
		id.Groups = claimStrings(v)
	}
	return id
}

// claimPath looks up a dotted path ("realm_access.roles") in claims. A key that itself contains
// dots, such as a namespaced "https://example.com/groups" claim, is matched whole first: at
// each level the longest key that matches wins.
func claimPath(m map[string]any, path string) (any, bool) {
	if path == "" {
		return nil, false
	}
	if v, ok := m[path]; ok {
		return v, true
	}
	for i := len(path) - 1; i > 0; i-- {
		if path[i] != '.' {
			continue
		}
		if sub, ok := m[path[:i]].(map[string]any); ok {
			if v, ok := claimPath(sub, path[i+1:]); ok {
				return v, true
			}
		}
	}
	return nil, false
}

func claimString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case json.Number:
		return s.String()
	}
	return ""
}

// claimBool reads a boolean claim. Some IdPs (Cognito, for one) send email_verified as the
// string "true".
func claimBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(b, "true")
	}
	return false
}

// claimStrings reads a groups claim: an array of strings, or a single string.
func claimStrings(v any) []string {
	out := []string{}
	switch g := v.(type) {
	case string:
		if g != "" {
			out = append(out, g)
		}
	case []any:
		for _, e := range g {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func isLoopback(u *url.URL) bool {
	h := u.Hostname()
	ip := net.ParseIP(h)
	return h == "localhost" || (ip != nil && ip.IsLoopback())
}

// s256 is the PKCE S256 challenge of a verifier.
func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
