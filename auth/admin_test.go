package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spirilis/generic-go-mcp/config"
)

// newAdminTestService builds a service whose users are octocat (github, granted admin by the
// new-form rules) and jdoe (corp, admitted with no role), and stores both.
func newAdminTestService(t *testing.T, legacy bool, opts ...Option) *AuthService {
	t.Helper()
	cfg := &config.AuthConfig{
		Enabled: true,
		Issuer:  testIssuer,
		Storage: config.StorageConfig{DBPath: filepath.Join(t.TempDir(), "auth.db")},
		GitHub:  config.GitHubConfig{ClientID: "gh-id", ClientSecret: "gh-secret"},
	}
	if legacy {
		cfg.Allowlist = config.AllowlistConfig{Users: []string{"octocat"}}
	} else {
		cfg.Providers = []config.ProviderConfig{{Name: "corp", Type: config.ProviderTypeOIDC,
			IssuerURL: "https://login.example.com", ClientID: "corp-client"}}
		cfg.Access = &config.AccessConfig{Rules: []config.AccessRule{
			{Provider: "github", Users: []string{"octocat"}, Roles: []string{"admin"}},
			{Provider: "corp", EmailDomains: []string{"example.com"}},
		}}
	}
	svc, err := NewAuthService(cfg, append([]Option{WithRoles(testRoles...)}, opts...)...)
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}
	t.Cleanup(func() { svc.Close() })

	now := time.Now()
	gh := &User{ID: "u-admin", GitHubLogin: "octocat", GitHubID: 1}
	setIdentitySnapshot(gh, Identity{Provider: "github", Issuer: "https://github.com", Subject: "1", Username: "octocat"}, now)
	corp := &User{ID: "u-plain"}
	setIdentitySnapshot(corp, Identity{Provider: "corp", Issuer: "https://login.example.com", Subject: "sub-1",
		Email: "jdoe@example.com", EmailVerified: true}, now)
	for _, u := range []*User{gh, corp} {
		if err := svc.storage.StoreUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	return svc
}

// The admin API mints and lists static clients. Before v0.12.0 the only way in was the
// GitHub allowlist, so admission was the gate; the new form can admit whole domains, so it
// takes a declared role, and without one the API is not served at all.
func TestAdminRoutesRequireAdminRole(t *testing.T) {
	const body = `{"client_name":"made by admin","redirect_uris":["https://admin.example/cb"]}`
	tests := []struct {
		name       string
		legacy     bool
		opts       []Option
		user       string
		wantRoute  bool
		wantStatus int
	}{
		{"legacy form: every admitted user, as before", true, nil, "u-admin", true, http.StatusCreated},
		{"new form, role holder", false, []Option{WithAdminRole("admin")}, "u-admin", true, http.StatusCreated},
		{"new form, admitted without the role", false, []Option{WithAdminRole("admin")}, "u-plain", true, http.StatusForbidden},
		{"new form without WithAdminRole: not served", false, nil, "u-admin", false, 0},
		// Legacy rules grant no roles, so naming one closes the API to everyone.
		{"legacy form with WithAdminRole", true, []Option{WithAdminRole("admin")}, "u-admin", true, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newAdminTestService(t, tt.legacy, tt.opts...)
			mux := http.NewServeMux()
			svc.RegisterAdminRoutes(mux)

			for _, method := range []string{http.MethodPost, http.MethodGet} {
				req := httptest.NewRequest(method, "/admin/clients", strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+tokenFor(t, svc, tt.user))
				if _, pattern := mux.Handler(req); (pattern != "") != tt.wantRoute {
					t.Fatalf("%s: route pattern = %q, want served = %v", method, pattern, tt.wantRoute)
				}
				if !tt.wantRoute {
					continue
				}
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				want := tt.wantStatus
				if method == http.MethodGet && want == http.StatusCreated {
					want = http.StatusOK
				}
				if rec.Code != want {
					t.Errorf("%s: status = %d %s, want %d", method, rec.Code, rec.Body, want)
				}
			}
		})
	}
}

func TestAdminRoleMustBeDeclared(t *testing.T) {
	cfg := &config.AuthConfig{
		Enabled: true,
		Issuer:  testIssuer,
		Storage: config.StorageConfig{DBPath: filepath.Join(t.TempDir(), "auth.db")},
	}
	if svc, err := NewAuthService(cfg, WithRoles(testRoles...), WithAdminRole("root")); err == nil {
		svc.Close()
		t.Fatal("NewAuthService accepted an undeclared admin role")
	} else if !strings.Contains(err.Error(), "WithAdminRole") {
		t.Errorf("error = %v, want it to name WithAdminRole", err)
	}
}
