package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spirilis/generic-go-mcp/config"
)

// The GitHub leg of the flow (/callback's code exchange and user lookup) cannot run in a unit
// test, so these tests enter at grantOrAskConsent — the point every successful callback reaches
// once the user has signed in and passed the allowlist.

const testIssuer = "https://mcp.example.com"

func newConsentTestService(t *testing.T, mutate func(*config.AuthConfig)) *AuthService {
	t.Helper()
	cfg := &config.AuthConfig{
		Enabled: true,
		Issuer:  testIssuer,
		Storage: config.StorageConfig{DBPath: filepath.Join(t.TempDir(), "auth.db")},
		Clients: []config.StaticClient{{
			ClientID:     "trusted-1",
			ClientSecret: "s3cret",
			Name:         "Operator-registered client",
			RedirectURIs: []string{"https://trusted.example.com/cb"},
		}},
	}
	if mutate != nil {
		mutate(cfg)
	}
	svc, err := NewAuthService(cfg)
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}
	t.Cleanup(func() { svc.Close() })
	return svc
}

func testUser(t *testing.T, svc *AuthService) *User {
	t.Helper()
	u := &User{ID: "user-1", GitHubLogin: "octocat", GitHubID: 1}
	if err := svc.storage.StoreUser(context.Background(), u); err != nil {
		t.Fatalf("StoreUser: %v", err)
	}
	return u
}

// register drives /register and returns the new client's ID, failing the test on rejection.
func register(t *testing.T, svc *AuthService, name string, redirectURIs ...string) string {
	t.Helper()
	rec := registerRaw(t, svc, name, redirectURIs...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("/register status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp ClientRegistrationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("registration response: %v", err)
	}
	return resp.ClientID
}

func registerRaw(t *testing.T, svc *AuthService, name string, redirectURIs ...string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(ClientRegistrationRequest{ClientName: name, RedirectURIs: redirectURIs})
	req := httptest.NewRequest(http.MethodPost, "/register", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	svc.handleClientRegistration(rec, req)
	return rec
}

// pendingAuthorization drives /authorize for clientID and returns the pending request it parked,
// exactly as /callback would load it.
func pendingAuthorization(t *testing.T, svc *AuthService, clientID, redirectURI string) *PendingAuthRequest {
	t.Helper()
	rec := authorizeGET(t, svc, url.Values{
		"response_type":  {"code"},
		"client_id":      {clientID},
		"redirect_uri":   {redirectURI},
		"state":          {"client-state"},
		"code_challenge": {strings.Repeat("a", 43)},
	})
	if rec.Code != http.StatusFound {
		t.Fatalf("/authorize status = %d, body %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("/authorize Location: %v", err)
	}
	authReq, err := svc.storage.GetAuthRequest(context.Background(), loc.Query().Get("state"))
	if err != nil {
		t.Fatalf("pending auth request not stored: %v", err)
	}
	return authReq
}

func completeCallback(svc *AuthService, authReq *PendingAuthRequest, user *User) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/callback", nil)
	rec := httptest.NewRecorder()
	svc.grantOrAskConsent(rec, req, authReq, user)
	return rec
}

var consentIDPattern = regexp.MustCompile(`name="consent_id" value="([^"]+)"`)

// consentScreen asserts rec is a consent screen and returns its consent ID and binding cookie.
func consentScreen(t *testing.T, rec *httptest.ResponseRecorder) (string, *http.Cookie) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 consent screen (Location %q)", rec.Code, rec.Header().Get("Location"))
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("consent screen must not redirect anywhere, got Location %q", loc)
	}
	m := consentIDPattern.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("no consent_id in body:\n%s", rec.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "__Host-mcp_consent" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("consent screen did not set the __Host-mcp_consent binding cookie")
	}
	return m[1], cookie
}

func postConsent(svc *AuthService, consentID, decision string, cookie *http.Cookie) *httptest.ResponseRecorder {
	form := url.Values{"consent_id": {consentID}, "decision": {decision}}
	req := httptest.NewRequest(http.MethodPost, "/consent", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
	rec := httptest.NewRecorder()
	svc.handleConsent(rec, req)
	return rec
}

// The attack this screen exists for: a client registered to the attacker's host must not receive
// a code for the user just because the user's browser was sent through /authorize.
func TestSelfRegisteredClientGetsConsentScreenNotACode(t *testing.T) {
	svc := newConsentTestService(t, nil)
	user := testUser(t, svc)
	clientID := register(t, svc, "Totally Legit", "https://evil.example/cb")

	rec := completeCallback(svc, pendingAuthorization(t, svc, clientID, "https://evil.example/cb"), user)
	consentScreen(t, rec)

	body := rec.Body.String()
	if !strings.Contains(body, "https://evil.example") {
		t.Error("consent screen must name the destination host")
	}
	if !strings.Contains(body, "octocat") {
		t.Error("consent screen must name the signed-in GitHub user")
	}
	if !strings.Contains(body, "registered itself") {
		t.Error("consent screen must flag a self-registered client's name as unverified")
	}

	h := rec.Header()
	for header, want := range map[string]string{
		"X-Frame-Options": "DENY",
		"Cache-Control":   "no-store",
		"Referrer-Policy": "no-referrer",
	} {
		if got := h.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP %q must forbid framing", csp)
	}
	if strings.Contains(csp, "form-action") {
		t.Errorf("CSP %q must not set form-action: Chrome applies it to the post-approval redirect", csp)
	}
}

func TestApprovedConsentIssuesCodeAndIsRemembered(t *testing.T) {
	svc := newConsentTestService(t, nil)
	user := testUser(t, svc)
	clientID := register(t, svc, "Claude", "https://claude.example/cb")

	consentID, cookie := consentScreen(t, completeCallback(svc,
		pendingAuthorization(t, svc, clientID, "https://claude.example/cb"), user))

	rec := postConsent(svc, consentID, "approve", cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve status = %d, want 303; body %s", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != "https://claude.example/cb" {
		t.Errorf("redirected to %q, want the registered redirect_uri", got)
	}
	if loc.Query().Get("state") != "client-state" {
		t.Errorf("state = %q, want the client's state echoed", loc.Query().Get("state"))
	}
	code, err := svc.storage.GetAuthCode(context.Background(), loc.Query().Get("code"))
	if err != nil || code.UserID != user.ID || code.ClientID != clientID {
		t.Fatalf("issued code not stored for this user and client: %+v, %v", code, err)
	}

	// The same user and client again: straight through, no second screen.
	rec = completeCallback(svc, pendingAuthorization(t, svc, clientID, "https://claude.example/cb"), user)
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "https://claude.example/cb?") {
		t.Fatalf("remembered consent: status %d Location %q, want 302 to the client", rec.Code, rec.Header().Get("Location"))
	}
}

func TestConsentIsPerClient(t *testing.T) {
	svc := newConsentTestService(t, nil)
	user := testUser(t, svc)
	good := register(t, svc, "Claude", "https://claude.example/cb")
	if err := svc.storage.StoreConsent(context.Background(), user.ID, good); err != nil {
		t.Fatal(err)
	}

	// A second registration — the attacker's — does not inherit the first client's consent.
	evil := register(t, svc, "Claude", "https://evil.example/cb")
	consentScreen(t, completeCallback(svc, pendingAuthorization(t, svc, evil, "https://evil.example/cb"), user))
}

func TestDeniedConsentRedirectsAccessDenied(t *testing.T) {
	svc := newConsentTestService(t, nil)
	user := testUser(t, svc)
	clientID := register(t, svc, "App", "https://app.example/cb")
	consentID, cookie := consentScreen(t, completeCallback(svc,
		pendingAuthorization(t, svc, clientID, "https://app.example/cb"), user))

	rec := postConsent(svc, consentID, "deny", cookie)
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://app.example/cb?") || !strings.Contains(loc, "error=access_denied") {
		t.Fatalf("deny: status %d Location %q, want access_denied at the redirect_uri", rec.Code, loc)
	}
	if strings.Contains(loc, "code=") {
		t.Fatalf("deny must not issue a code: %q", loc)
	}
	if ok, _ := svc.storage.HasConsent(context.Background(), user.ID, clientID); ok {
		t.Error("deny must not record consent")
	}
}

// The approval must come from the browser that was shown the screen. A wrong or missing cookie is
// refused without consuming the pending consent, so it cannot be used to burn a real user's
// approval either.
func TestConsentRequiresTheBindingCookie(t *testing.T) {
	svc := newConsentTestService(t, nil)
	user := testUser(t, svc)
	clientID := register(t, svc, "App", "https://app.example/cb")
	consentID, cookie := consentScreen(t, completeCallback(svc,
		pendingAuthorization(t, svc, clientID, "https://app.example/cb"), user))

	if rec := postConsent(svc, consentID, "approve", nil); rec.Code != http.StatusForbidden {
		t.Errorf("no cookie: status %d, want 403", rec.Code)
	}
	forged := &http.Cookie{Name: cookie.Name, Value: "not-the-binding"}
	if rec := postConsent(svc, consentID, "approve", forged); rec.Code != http.StatusForbidden {
		t.Errorf("wrong cookie: status %d, want 403", rec.Code)
	}

	if rec := postConsent(svc, consentID, "approve", cookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("right cookie after refused attempts: status %d, want 303", rec.Code)
	}
	if rec := postConsent(svc, consentID, "approve", cookie); rec.Code != http.StatusBadRequest {
		t.Errorf("replayed consent_id: status %d, want 400", rec.Code)
	}
}

func TestExpiredConsentIsRefused(t *testing.T) {
	svc := newConsentTestService(t, nil)
	binding := "binding-secret"
	err := svc.storage.StorePendingConsent(context.Background(), &PendingConsent{
		ID:          "expired-1",
		BindingHash: hashSecret(binding),
		UserID:      "user-1",
		Request:     PendingAuthRequest{ClientID: "c", RedirectURI: "https://app.example/cb"},
		ExpiresAt:   time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := postConsent(svc, "expired-1", "approve", &http.Cookie{Name: "__Host-mcp_consent", Value: binding})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expired consent: status %d, want 400", rec.Code)
	}
	if _, err := svc.storage.TakePendingConsent(context.Background(), "expired-1", hashSecret(binding)); err != ErrConsentNotFound {
		t.Errorf("expired consent should have been deleted, got %v", err)
	}
}

func TestConsentEndpointIsPostOnly(t *testing.T) {
	svc := newConsentTestService(t, nil)
	rec := httptest.NewRecorder()
	svc.handleConsent(rec, httptest.NewRequest(http.MethodGet, "/consent?consent_id=x&decision=approve", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /consent: status %d, want 405", rec.Code)
	}
}

// Clients from config were vetted by the operator and skip the screen. Clients created through
// /admin/clients are also IsStatic, but no operator reviewed them, so they do not.
func TestOnlyConfigClientsSkipConsent(t *testing.T) {
	svc := newConsentTestService(t, nil)
	user := testUser(t, svc)

	rec := completeCallback(svc, pendingAuthorization(t, svc, "trusted-1", "https://trusted.example.com/cb"), user)
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "https://trusted.example.com/cb?code=") {
		t.Fatalf("config client: status %d Location %q, want 302 with a code", rec.Code, rec.Header().Get("Location"))
	}

	err := svc.storage.StoreClient(context.Background(), &RegisteredClient{
		ClientID:     "admin-made",
		ClientSecret: hashSecret("x"),
		ClientName:   "Admin-created",
		RedirectURIs: []string{"https://admin.example/cb"},
		IsStatic:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	consentScreen(t, completeCallback(svc, pendingAuthorization(t, svc, "admin-made", "https://admin.example/cb"), user))
}

func TestConsentScreenEscapesClientName(t *testing.T) {
	svc := newConsentTestService(t, nil)
	user := testUser(t, svc)
	clientID := register(t, svc, `<script>alert("x")</script>`, "https://app.example/cb")

	rec := completeCallback(svc, pendingAuthorization(t, svc, clientID, "https://app.example/cb"), user)
	consentScreen(t, rec)
	if strings.Contains(rec.Body.String(), "<script>alert") {
		t.Fatal("client_name reached the page unescaped")
	}
	if !strings.Contains(rec.Body.String(), "&lt;script&gt;") {
		t.Error("expected the escaped client_name on the page")
	}
}

func TestRegistrationRejectsUnsafeRedirectURIs(t *testing.T) {
	svc := newConsentTestService(t, nil)
	for _, uri := range []string{
		"javascript:alert(1)",
		"data:text/html,hi",
		"https://app.example/cb#frag",
		"/relative/cb",
	} {
		if rec := registerRaw(t, svc, "x", uri); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", uri, rec.Code)
		}
	}
	// Loopback http and native-app custom schemes remain legitimate.
	register(t, svc, "cli", "http://localhost:33418/callback")
	register(t, svc, "native", "com.example.app:/oauth/callback")
}

func TestRegistrationHonorsAllowedRedirectHosts(t *testing.T) {
	svc := newConsentTestService(t, func(c *config.AuthConfig) {
		c.Registration.AllowedRedirectHosts = []string{"claude.ai", "LocalHost", "[::1]"}
	})
	register(t, svc, "web", "https://claude.ai/api/mcp/auth_callback")
	register(t, svc, "web-upper", "https://CLAUDE.AI/api/mcp/auth_callback")
	register(t, svc, "cli", "http://localhost:33418/callback")
	register(t, svc, "cli6", "http://[::1]:33418/callback")

	for _, uri := range []string{
		"https://evil.example/cb",
		"https://claude.ai.evil.example/cb",
		"com.example.app:/oauth/callback", // no host, so nothing to match
	} {
		if rec := registerRaw(t, svc, "x", uri); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400 under the allowlist", uri, rec.Code)
		}
	}
	// One bad URI rejects the whole registration.
	if rec := registerRaw(t, svc, "x", "https://claude.ai/cb", "https://evil.example/cb"); rec.Code != http.StatusBadRequest {
		t.Errorf("mixed redirect_uris: status %d, want 400", rec.Code)
	}
}

func TestAllowedRedirectHostsMustBeBareHostnames(t *testing.T) {
	for _, entry := range []string{"https://claude.ai", "claude.ai:443", "claude.ai/cb", "", "  "} {
		cfg := &config.AuthConfig{
			Enabled:      true,
			Issuer:       testIssuer,
			Storage:      config.StorageConfig{DBPath: filepath.Join(t.TempDir(), "auth.db")},
			Registration: config.RegistrationConfig{AllowedRedirectHosts: []string{entry}},
		}
		if svc, err := NewAuthService(cfg); err == nil {
			svc.Close()
			t.Errorf("entry %q: NewAuthService succeeded, want a configuration error", entry)
		}
	}
}
