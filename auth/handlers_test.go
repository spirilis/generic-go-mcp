package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spirilis/generic-go-mcp/config"
)

// newTestAuthService builds an AuthService backed by a throwaway BoltDB, with one
// confidential client registered (secret "s3cret", one registered redirect_uri).
func newTestAuthService(t *testing.T) (*AuthService, *RegisteredClient) {
	t.Helper()
	cfg := &config.AuthConfig{
		Enabled: true,
		Issuer:  "https://mcp.example.com",
		Storage: config.StorageConfig{DBPath: filepath.Join(t.TempDir(), "auth.db")},
	}
	svc, err := NewAuthService(cfg)
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}
	t.Cleanup(func() { svc.Close() })

	client := &RegisteredClient{
		ClientID:                "client-1",
		ClientSecret:            hashSecret("s3cret"),
		ClientName:              "Test",
		RedirectURIs:            []string{"https://app.example.com/cb"},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "client_secret_post",
		IsStatic:                true,
		CreatedAt:               time.Now(),
	}
	if err := svc.storage.StoreClient(context.Background(), client); err != nil {
		t.Fatalf("StoreClient: %v", err)
	}
	return svc, client
}

func authorizeGET(t *testing.T, svc *AuthService, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	// A panic here (the pre-fix http.Redirect(w, nil, ...) bug) propagates and fails the test.
	svc.handleAuthorize(rec, req)
	return rec
}

// G1: a denied authorize with a VALID registered client + redirect_uri must redirect the
// error back to the redirect_uri — and must not panic (the regression).
func TestAuthorizeDeniedRedirectsWithoutPanic(t *testing.T) {
	svc, _ := newTestAuthService(t)
	rec := authorizeGET(t, svc, url.Values{
		"response_type": {"token"}, // unsupported -> error
		"client_id":     {"client-1"},
		"redirect_uri":  {"https://app.example.com/cb"},
		"state":         {"xyz"},
	})
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "error=unsupported_response_type") {
		t.Errorf("Location = %q, want it to carry error=unsupported_response_type", loc)
	}
	if !strings.HasPrefix(loc, "https://app.example.com/cb?") {
		t.Errorf("Location = %q, want redirect back to the registered redirect_uri", loc)
	}
	if !strings.Contains(loc, "state=xyz") {
		t.Errorf("Location = %q, want the state echoed back", loc)
	}
}

// G1: an unregistered redirect_uri must NOT be redirected to (open-redirect / error leak).
// It is reported directly instead.
func TestAuthorizeInvalidRedirectURIReportedDirectly(t *testing.T) {
	svc, _ := newTestAuthService(t)
	rec := authorizeGET(t, svc, url.Values{
		"response_type":  {"code"},
		"client_id":      {"client-1"},
		"redirect_uri":   {"https://evil.example.com/cb"}, // not registered
		"code_challenge": {strings.Repeat("a", 43)},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (must not redirect to an unregistered URI)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q, want empty (no redirect to an unvalidated URI)", loc)
	}
	var oe OAuthError
	if err := json.Unmarshal(rec.Body.Bytes(), &oe); err != nil {
		t.Fatalf("body not an OAuthError: %v", err)
	}
	if oe.Error != "invalid_request" {
		t.Errorf("error = %q, want invalid_request", oe.Error)
	}
}

// G1: an unknown client is reported directly, not redirected.
func TestAuthorizeUnknownClientReportedDirectly(t *testing.T) {
	svc, _ := newTestAuthService(t)
	rec := authorizeGET(t, svc, url.Values{
		"response_type": {"code"},
		"client_id":     {"nope"},
		"redirect_uri":  {"https://app.example.com/cb"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var oe OAuthError
	_ = json.Unmarshal(rec.Body.Bytes(), &oe)
	if oe.Error != "invalid_client" {
		t.Errorf("error = %q, want invalid_client", oe.Error)
	}
}

func tokenPOST(t *testing.T, svc *AuthService, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	svc.handleToken(rec, req)
	return rec
}

// G2: a confidential client presenting the wrong secret is rejected with invalid_client.
func TestTokenGrantRejectsWrongClientSecret(t *testing.T) {
	svc, _ := newTestAuthService(t)
	rec := tokenPOST(t, svc, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"whatever"},
		"client_id":     {"client-1"},
		"client_secret": {"WRONG"},
		"redirect_uri":  {"https://app.example.com/cb"},
		"code_verifier": {strings.Repeat("a", 43)},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var oe OAuthError
	_ = json.Unmarshal(rec.Body.Bytes(), &oe)
	if oe.Error != "invalid_client" {
		t.Errorf("error = %q, want invalid_client (secret must be verified)", oe.Error)
	}
}

// G2: an empty secret for a confidential client is also rejected (empty stored secret is the
// only thing that means "public"; this client has one).
func TestTokenGrantRejectsMissingClientSecret(t *testing.T) {
	svc, _ := newTestAuthService(t)
	rec := tokenPOST(t, svc, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {"whatever"},
		"client_id":    {"client-1"},
		"redirect_uri": {"https://app.example.com/cb"},
	})
	var oe OAuthError
	_ = json.Unmarshal(rec.Body.Bytes(), &oe)
	if oe.Error != "invalid_client" {
		t.Errorf("error = %q, want invalid_client", oe.Error)
	}
}

// G2: with the CORRECT secret, client authentication passes and the flow proceeds far enough
// to fail on the (bogus) authorization code instead — proving the secret gate was cleared.
func TestTokenGrantAcceptsCorrectClientSecret(t *testing.T) {
	svc, _ := newTestAuthService(t)
	rec := tokenPOST(t, svc, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"bogus-code"},
		"client_id":     {"client-1"},
		"client_secret": {"s3cret"},
		"redirect_uri":  {"https://app.example.com/cb"},
		"code_verifier": {strings.Repeat("a", 43)},
	})
	var oe OAuthError
	_ = json.Unmarshal(rec.Body.Bytes(), &oe)
	if oe.Error != "invalid_grant" {
		t.Errorf("error = %q, want invalid_grant (secret accepted, then bad code)", oe.Error)
	}
}
