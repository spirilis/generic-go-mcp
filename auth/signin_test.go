package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spirilis/generic-go-mcp/config"
)

// fakeGitHubServer is a GitHub (or GitHub Enterprise Server) over HTTP: the OAuth endpoints at
// the web root, the REST API under /api/v3. Organizations come back two to a page, linked by
// Link headers, so a login reads more than one page.
type fakeGitHubServer struct {
	srv        *httptest.Server
	id         int64
	login      string
	orgs       []string
	teamsError int // HTTP status /user/teams answers, if not 0
	userError  int // HTTP status /user answers, if not 0
}

func newFakeGitHubServer(t *testing.T, id int64, login string, orgs ...string) *fakeGitHubServer {
	t.Helper()
	f := &fakeGitHubServer{id: id, login: login, orgs: orgs}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("code") != "gh-code" {
			writeJSON(w, map[string]string{"error": "bad_verification_code"})
			return
		}
		writeJSON(w, map[string]string{"access_token": "gh-token", "token_type": "bearer"})
	})
	mux.HandleFunc("/api/v3/user", func(w http.ResponseWriter, r *http.Request) {
		if f.userError != 0 {
			w.WriteHeader(f.userError)
			return
		}
		writeJSON(w, map[string]any{"id": f.id, "login": f.login, "avatar_url": "https://avatars/" + f.login})
	})
	mux.HandleFunc("/api/v3/user/orgs", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gh-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		page := 0
		fmt.Sscan(r.URL.Query().Get("page"), &page)
		var out []map[string]any
		for i := page * 2; i < len(f.orgs) && i < page*2+2; i++ {
			out = append(out, map[string]any{"login": f.orgs[i]})
		}
		if (page+1)*2 < len(f.orgs) {
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/v3/user/orgs?per_page=100&page=%d>; rel="next", <x>; rel="last"`, f.srv.URL, page+1))
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("/api/v3/user/teams", func(w http.ResponseWriter, r *http.Request) {
		if f.teamsError != 0 {
			w.WriteHeader(f.teamsError)
			writeJSON(w, map[string]string{"message": "Must have admin rights"})
			return
		}
		writeJSON(w, []map[string]any{{"slug": "devs", "organization": map[string]any{"login": "acme"}}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHubServer) provider(name string) config.ProviderConfig {
	return config.ProviderConfig{Name: name, Type: config.ProviderTypeGitHub, DisplayName: "GHE",
		ClientID: "ghe-id", ClientSecret: "ghe-secret", BaseURL: f.srv.URL, APIURL: f.srv.URL + "/api/v3"}
}

// A GitHub provider on an Enterprise Server: its own authorize and API URLs, every page of
// organizations, and a failing teams call leaves teams out (logged) instead of failing the login
// or being read as "no teams" from an error body.
func TestGitHubEnterpriseProvider(t *testing.T) {
	f := newFakeGitHubServer(t, 7, "octocat", "o1", "o2", "o3", "o4", "o5")
	f.teamsError = http.StatusForbidden
	p := newGitHubProvider(mustResolveProvider(t, f.provider("ghe")))

	authURL, err := p.AuthCodeURL(context.Background(), testIssuer+"/callback", "st", "n", "c")
	if err != nil || !strings.HasPrefix(authURL, f.srv.URL+"/login/oauth/authorize?") || !strings.Contains(authURL, "state=st") {
		t.Fatalf("AuthCodeURL = %q, %v", authURL, err)
	}
	id, err := p.Exchange(context.Background(), "gh-code", testIssuer+"/callback", "", "")
	if err != nil {
		t.Fatal(err)
	}
	want := Identity{Provider: "ghe", Issuer: f.srv.URL, Subject: "7", Username: "octocat", AvatarURL: "https://avatars/octocat",
		Groups: []string{"o1", "o2", "o3", "o4", "o5"}}
	if !reflect.DeepEqual(id, want) {
		t.Errorf("identity = %+v\nwant       %+v", id, want)
	}

	f.teamsError = 0
	if id, _ = p.Exchange(context.Background(), "gh-code", "", "", ""); !strings.Contains(strings.Join(id.Groups, ","), "acme/devs") {
		t.Errorf("groups = %v, want the team too", id.Groups)
	}
	if _, err := p.Exchange(context.Background(), "wrong", "", "", ""); err == nil {
		t.Error("a refused code was accepted")
	}
	f.userError = http.StatusUnauthorized
	if _, err := p.Exchange(context.Background(), "gh-code", "", "", ""); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a failing /user: %v", err)
	}
}

func mustResolveProvider(t *testing.T, p config.ProviderConfig) config.ProviderConfig {
	t.Helper()
	a := config.AuthConfig{Providers: []config.ProviderConfig{p},
		Access: &config.AccessConfig{Rules: []config.AccessRule{{Provider: p.Name, Anyone: true}}}}
	res, err := a.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	return res.Providers[0]
}

// Users are kept apart by (provider, issuer, subject): the same login on github.com and on an
// Enterprise Server are two users, and a GitHub login renamed and claimed by someone else does
// not inherit the old account's record (or its consents).
func TestUserRecordsKeyedByProviderIdentity(t *testing.T) {
	ghe := newFakeGitHubServer(t, 7, "octocat")
	svc := newConsentTestService(t, func(c *config.AuthConfig) {
		c.GitHub = config.GitHubConfig{ClientID: "gh-id", ClientSecret: "gh-secret"}
		c.Providers = []config.ProviderConfig{ghe.provider("ghe")}
		c.Access = &config.AccessConfig{Rules: []config.AccessRule{
			{Provider: "github", Anyone: true}, {Provider: "ghe", Anyone: true}}}
	})
	gh := &fakeGitHub{id: 42, login: "octocat"}
	setGitHubTransport(svc, gh)

	login := func(provider string) *User {
		t.Helper()
		id := generateSecureToken(8)
		if err := svc.storage.StoreAuthRequest(context.Background(), &PendingAuthRequest{
			ID: id, ClientID: "trusted-1", RedirectURI: "https://trusted.example.com/cb", Provider: provider,
			CodeChallenge: strings.Repeat("a", 43), CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		svc.handleCallback(rec, httptest.NewRequest(http.MethodGet, "/callback?code=gh-code&state="+id, nil))
		loc, _ := url.Parse(rec.Header().Get("Location"))
		code, err := svc.storage.GetAuthCode(context.Background(), loc.Query().Get("code"))
		if err != nil {
			t.Fatalf("%s login: %d %s %s", provider, rec.Code, rec.Header().Get("Location"), rec.Body)
		}
		u, err := svc.storage.GetUser(context.Background(), code.UserID)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}

	dotcom, enterprise := login("github"), login("ghe")
	if dotcom.ID == enterprise.ID {
		t.Fatal("github.com and the Enterprise Server octocat share a user record")
	}
	if dotcom.GitHubLogin != "octocat" || enterprise.GitHubLogin != "" || enterprise.Username != "octocat" {
		t.Errorf("only the provider named github owns the login index: %+v / %+v", dotcom, enterprise)
	}
	if again := login("github"); again.ID != dotcom.ID {
		t.Error("a second login made a new record")
	}

	// A legacy record under the same login but another GitHub account id. The account
	// signing in has never signed in before (id 43), so nothing but the login fallback could
	// find a record for it.
	if err := svc.storage.StoreUser(context.Background(), &User{ID: "u-old", GitHubLogin: "renamed", GitHubID: 99}); err != nil {
		t.Fatal(err)
	}
	gh.login, gh.id = "renamed", 43
	if u := login("github"); u.ID == "u-old" || u.GitHubID != 43 {
		t.Errorf("a different GitHub account took over a legacy record by login: %+v", u)
	}
	if old, err := svc.storage.GetUser(context.Background(), "u-old"); err != nil || old.GitHubID != 99 || !old.LastLoginAt.IsZero() {
		t.Errorf("the legacy record changed: %+v, %v", old, err)
	}
	// Its own account (in a fresh store state) does take it over, and keeps its id.
	if err := svc.storage.StoreUser(context.Background(), &User{ID: "u-old2", GitHubLogin: "renamed2", GitHubID: 99}); err != nil {
		t.Fatal(err)
	}
	gh.login, gh.id = "renamed2", 99
	if u := login("github"); u.ID != "u-old2" || u.LastLoginAt.IsZero() {
		t.Errorf("the legacy record's own account got %+v, want u-old2 with a snapshot", u)
	}

	// The issuer a login records is the one the startup check compares against, so the store
	// these logins wrote must start again, also when the baseURL is written with a slash.
	svc.Close()
	cfg := *svc.config
	cfg.Providers = []config.ProviderConfig{ghe.provider("ghe")}
	cfg.Providers[0].BaseURL += "/"
	restarted, err := NewAuthService(&cfg)
	if err != nil {
		t.Fatalf("restart over the store real logins wrote: %v", err)
	}
	restarted.Close()
}

// A pre-v0.12 record is a github.com user. Once the provider named github is an Enterprise
// Server, the same login and numeric id there is someone else, and must not take the record
// (and its consents) over.
func TestLegacyRecordFallbackNeedsGitHubDotCom(t *testing.T) {
	ghe := newFakeGitHubServer(t, 7, "octocat")
	tests := []struct {
		name     string
		provider config.ProviderConfig // named github
		issuer   string
		wantOld  bool
	}{
		{"github.com", config.ProviderConfig{Name: "github", Type: config.ProviderTypeGitHub, ClientID: "id", ClientSecret: "s"},
			"https://github.com", true},
		{"github is now an Enterprise Server", ghe.provider("github"), ghe.srv.URL, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newConsentTestService(t, func(c *config.AuthConfig) {
				c.Providers = []config.ProviderConfig{tt.provider}
				c.Access = &config.AccessConfig{Rules: []config.AccessRule{{Provider: "github", Anyone: true}}}
			})
			if err := svc.storage.StoreUser(context.Background(), &User{ID: "u-old", GitHubLogin: "octocat", GitHubID: 7}); err != nil {
				t.Fatal(err)
			}
			u, err := svc.userForIdentity(context.Background(),
				Identity{Provider: "github", Issuer: tt.issuer, Subject: "7", Username: "octocat"})
			if err != nil {
				t.Fatal(err)
			}
			if gotOld := u.ID == "u-old"; gotOld != tt.wantOld {
				t.Errorf("took the legacy record = %v, want %v", gotOld, tt.wantOld)
			}
		})
	}
}

var requestIDPattern = regexp.MustCompile(`name="request" value="([^"]+)"`)

func serve(svc *AuthService, req *http.Request) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	svc.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func postForm(path string, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// newTwoProviderService has GitHub (legacy block, faked in-process) and the fake IdP as "corp".
func newTwoProviderService(t *testing.T, idp *fakeIdP, dbPath string, rules ...config.AccessRule) *AuthService {
	t.Helper()
	svc := newOIDCService(t, idp, dbPath, func(c *config.AuthConfig) {
		c.GitHub = config.GitHubConfig{ClientID: "gh-id", ClientSecret: "gh-secret"}
		c.Access.IdentityMaxAge = time.Hour
		if rules != nil {
			c.Access.Rules = rules
		}
	})
	setGitHubTransport(svc, &fakeGitHub{login: "octocat"})
	return svc
}

// The whole flow with two providers: /authorize -> chooser -> /authorize/provider -> IdP ->
// /callback -> consent -> /token -> a request through Middleware carrying the roles. Then the
// refresh grant: refused once the login is older than identityMaxAge, and once the rule that
// admitted the user is gone.
func TestEndToEndOIDCSignIn(t *testing.T) {
	idp := newFakeIdP(t)
	dbPath := filepath.Join(t.TempDir(), "auth.db")
	svc := newTwoProviderService(t, idp, dbPath)

	// A self-registered public client, so the consent screen is in the path.
	reg := registerJSON(t, svc, `{"redirect_uris":["https://app.example.com/cb"],"client_name":"App","token_endpoint_auth_method":"none"}`)
	var client ClientRegistrationResponse
	if err := json.Unmarshal(reg.Body.Bytes(), &client); err != nil || client.ClientID == "" {
		t.Fatalf("register: %s", reg.Body)
	}
	verifier := strings.Repeat("v", 43)

	// /authorize: two providers, so the chooser.
	rec := serve(svc, httptest.NewRequest(http.MethodGet, "/authorize?"+url.Values{
		"response_type": {"code"}, "client_id": {client.ClientID}, "redirect_uri": {"https://app.example.com/cb"},
		"state": {"client-state"}, "code_challenge": {s256(verifier)}, "code_challenge_method": {"S256"},
	}.Encode(), nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Continue with Corp SSO") ||
		!strings.Contains(rec.Body.String(), "Continue with GitHub") {
		t.Fatalf("chooser: %d %s", rec.Code, rec.Body)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "form-action") {
		t.Errorf("chooser CSP = %q", csp)
	}
	m := requestIDPattern.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("no request id in the chooser:\n%s", rec.Body)
	}

	// The choice: off to the IdP, with PKCE, a nonce and the configured extra parameters.
	rec = serve(svc, postForm("/authorize/provider", url.Values{"request": {m[1]}, "provider": {"corp"}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("choose: %d %s", rec.Code, rec.Body)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	q := loc.Query()
	if q.Get("state") != m[1] || q.Get("redirect_uri") != testIssuer+"/callback" || q.Get("prompt") != "select_account" ||
		q.Get("tenant") != "t1" || q.Get("scope") != "openid profile email" || q.Get("nonce") == "" {
		t.Errorf("authorization request = %v", q)
	}
	code := idp.authorize(loc.String())

	// Back at /callback: the dynamic client gets the consent screen, naming the provider.
	rec = serve(svc, httptest.NewRequest(http.MethodGet, "/callback?code="+code+"&state="+m[1], nil))
	consentID, cookie := consentScreen(t, rec)
	if !strings.Contains(rec.Body.String(), "signed in to Corp SSO as <strong>alice</strong>") {
		t.Errorf("consent screen:\n%s", rec.Body)
	}
	rec = postConsent(svc, consentID, "approve", cookie)
	loc, _ = url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusSeeOther || loc.Query().Get("state") != "client-state" {
		t.Fatalf("approve: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	// /token with the client's PKCE verifier.
	tok := tokenPOST(t, svc, url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"client_id": {client.ClientID}, "redirect_uri": {"https://app.example.com/cb"}, "code_verifier": {verifier}})
	var tr TokenResponse
	if err := json.Unmarshal(tok.Body.Bytes(), &tr); err != nil || tr.AccessToken == "" {
		t.Fatalf("token: %d %s", tok.Code, tok.Body)
	}

	// /mcp: the principal carries the roles the group rule grants.
	rec, p := serveAuthed(t, svc, tr.AccessToken)
	if rec.Code != http.StatusOK || p == nil {
		t.Fatalf("mcp: %d %s", rec.Code, rec.Body)
	}
	if p.ID != "corp:user-sub-1" || p.Username != "alice" || !reflect.DeepEqual(p.Roles, []string{"admin", "writer"}) ||
		!reflect.DeepEqual(p.Groups, []string{"mcp-admins", "staff"}) || !p.EmailVerified {
		t.Errorf("principal = %+v", p)
	}

	refresh := func(svc *AuthService, rt string) (*httptest.ResponseRecorder, TokenResponse) {
		rec := tokenPOST(t, svc, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {client.ClientID}})
		var out TokenResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out
	}
	rec, tr = refresh(svc, tr.RefreshToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body)
	}

	// The login is older than identityMaxAge (1h): refused, so the user goes back to the IdP.
	u, err := svc.storage.GetUser(context.Background(), p.UserID)
	if err != nil {
		t.Fatal(err)
	}
	loginAt := u.LastLoginAt
	u.LastLoginAt = time.Now().Add(-2 * time.Hour)
	if err := svc.storage.StoreUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if rec, _ := refresh(svc, tr.RefreshToken); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Fatalf("refresh past identityMaxAge: %d %s", rec.Code, rec.Body)
	}
	u.LastLoginAt = loginAt
	if err := svc.storage.StoreUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}

	// The operator removes the rule: on restart the same refresh token and access token stop working.
	svc.Close()
	svc = newTwoProviderService(t, idp, dbPath, config.AccessRule{Provider: "github", Anyone: true})
	if rec, _ := refresh(svc, tr.RefreshToken); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Fatalf("refresh after the rule was removed: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := serveAuthed(t, svc, tr.AccessToken); rec.Code != http.StatusForbidden {
		t.Fatalf("mcp after the rule was removed: %d", rec.Code)
	}
}

// What the chooser and /callback refuse.
func TestSignInFlowRefusals(t *testing.T) {
	idp := newFakeIdP(t)
	svc := newTwoProviderService(t, idp, "")
	c := storeTestClient(t, svc)
	rec := startAuthorize(t, svc, c)
	m := requestIDPattern.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("chooser: %d %s", rec.Code, rec.Body)
	}
	reqID := m[1]

	tests := []struct {
		name     string
		req      *http.Request
		status   int
		location string // substring of Location, if a redirect
	}{
		{"chooser GET", httptest.NewRequest(http.MethodGet, "/authorize/provider?request="+reqID+"&provider=corp", nil), http.StatusMethodNotAllowed, ""},
		{"unknown provider", postForm("/authorize/provider", url.Values{"request": {reqID}, "provider": {"evil"}}), http.StatusBadRequest, ""},
		{"unknown request", postForm("/authorize/provider", url.Values{"request": {"nope"}, "provider": {"corp"}}), http.StatusBadRequest, ""},
		{"oversized form", postForm("/authorize/provider", url.Values{"request": {strings.Repeat("r", 10<<10)}}), http.StatusBadRequest, ""},
		// Never left the chooser: no code can belong to this request (which is used up).
		{"callback before a choice", httptest.NewRequest(http.MethodGet, "/callback?code=x&state="+reqID, nil), http.StatusBadRequest, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(svc, tt.req)
			if rec.Code != tt.status || !strings.Contains(rec.Header().Get("Location"), tt.location) {
				t.Fatalf("status = %d, Location %q, body %s", rec.Code, rec.Header().Get("Location"), rec.Body)
			}
		})
	}

	// The user cancels at the IdP: access_denied back to the client, without the IdP's text.
	m = requestIDPattern.FindStringSubmatch(startAuthorize(t, svc, c).Body.String())
	if rec := serve(svc, postForm("/authorize/provider", url.Values{"request": {m[1]}, "provider": {"corp"}})); rec.Code != http.StatusSeeOther {
		t.Fatalf("choose: %d", rec.Code)
	}
	rec = serve(svc, httptest.NewRequest(http.MethodGet, "/callback?error=access_denied&error_description=<script>&state="+m[1], nil))
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://app.example.com/cb?") ||
		!strings.Contains(loc, "error=access_denied") || strings.Contains(loc, "%3Cscript") {
		t.Errorf("cancelled sign-in: %d %q", rec.Code, loc)
	}

	// A user no rule admits is refused at the callback.
	idp.set(func(idp *fakeIdP) { idp.claims = map[string]any{"groups": []string{"staff"}} })
	m = requestIDPattern.FindStringSubmatch(startAuthorize(t, svc, c).Body.String())
	rec = serve(svc, postForm("/authorize/provider", url.Values{"request": {m[1]}, "provider": {"corp"}}))
	code := idp.authorize(rec.Header().Get("Location"))
	rec = serve(svc, httptest.NewRequest(http.MethodGet, "/callback?code="+code+"&state="+m[1], nil))
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=access_denied") {
		t.Errorf("unadmitted user: %d %q", rec.Code, loc)
	}
}

// With one provider /authorize goes straight to it; the request records the provider, a nonce
// and an upstream verifier.
func TestAuthorizeSingleProviderRedirects(t *testing.T) {
	idp := newFakeIdP(t)
	svc := newOIDCService(t, idp, "", nil)
	rec := startAuthorize(t, svc, storeTestClient(t, svc))
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	pending, err := svc.storage.GetAuthRequest(context.Background(), loc.Query().Get("state"))
	if err != nil {
		t.Fatal(err)
	}
	if pending.Provider != "corp" || pending.Nonce != loc.Query().Get("nonce") ||
		s256(pending.UpstreamVerifier) != loc.Query().Get("code_challenge") || pending.UpstreamVerifier == pending.CodeChallenge {
		t.Errorf("pending = %+v, query %v", pending, loc.Query())
	}
}

// identityMaxAge 0 means the refresh token lifetime; a pre-v0.12 record with no login time
// is refreshed until its next login records one.
func TestRefreshIdentityAge(t *testing.T) {
	svc := newRulesService(t, config.AccessRule{Provider: "github", Users: []string{"octocat"}})
	if got := svc.maxIdentityAge(); got != RefreshTokenTTL {
		t.Errorf("default identity age = %v, want %v", got, RefreshTokenTTL)
	}
	client := storeTestClient(t, svc)
	tests := []struct {
		name          string
		login         time.Time // zero: a record from before v0.12.0
		legacyAgeFrom time.Time // on the refresh token presented
		wantOK        bool
	}{
		{"fresh login", time.Now().Add(-time.Hour), time.Time{}, true},
		{"past the refresh token lifetime", time.Now().Add(-RefreshTokenTTL - time.Hour), time.Time{}, false},
		{"a login outranks the legacy age", time.Now().Add(-time.Hour), time.Now().Add(-RefreshTokenTTL - time.Hour), true},
		{"pre-v0.12, first refresh after the upgrade", time.Time{}, time.Time{}, true},
		{"pre-v0.12, chain within the age", time.Time{}, time.Now().Add(-time.Hour), true},
		{"pre-v0.12, chain past the age", time.Time{}, time.Now().Add(-RefreshTokenTTL - time.Hour), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := &User{ID: "u", GitHubLogin: "octocat", GitHubID: 1}
			if !tt.login.IsZero() {
				setIdentitySnapshot(u, Identity{Provider: "github", Issuer: "https://github.com", Username: "octocat"}, tt.login)
			}
			if err := svc.storage.StoreUser(context.Background(), u); err != nil {
				t.Fatal(err)
			}
			rt, err := svc.tokenService.generateRefreshToken("u", client.ClientID, "", "", tt.legacyAgeFrom)
			if err != nil {
				t.Fatal(err)
			}
			before := time.Now()
			rec := tokenPOST(t, svc, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt.Token}, "client_id": {client.ClientID}})
			if (rec.Code == http.StatusOK) != tt.wantOK {
				t.Fatalf("status = %d %s", rec.Code, rec.Body)
			}
			if !tt.wantOK {
				return
			}
			// The rotated token carries the chain's age for a legacy user (starting it on the
			// first refresh), and none once a login has been recorded.
			var resp TokenResponse
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			next, err := svc.storage.GetRefreshToken(context.Background(), resp.RefreshToken)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case !tt.login.IsZero():
				if !next.LegacyAgeFrom.IsZero() {
					t.Errorf("LegacyAgeFrom = %v after a recorded login, want none", next.LegacyAgeFrom)
				}
			case tt.legacyAgeFrom.IsZero():
				if next.LegacyAgeFrom.Before(before.Add(-time.Second)) || next.LegacyAgeFrom.After(time.Now()) {
					t.Errorf("LegacyAgeFrom = %v, want the time of this refresh", next.LegacyAgeFrom)
				}
			case !next.LegacyAgeFrom.Equal(tt.legacyAgeFrom):
				t.Errorf("LegacyAgeFrom = %v, want it carried over as %v", next.LegacyAgeFrom, tt.legacyAgeFrom)
			}
			if got, _ := svc.storage.GetUser(context.Background(), "u"); !got.LastLoginAt.Equal(u.LastLoginAt) {
				t.Errorf("the refresh grant rewrote the user's LastLoginAt: %v", got.LastLoginAt)
			}
		})
	}
}
