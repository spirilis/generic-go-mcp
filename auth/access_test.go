package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spirilis/generic-go-mcp/config"
)

var testRoles = []Role{
	{Name: "admin", Description: "manage everything"},
	{Name: "writer", Description: "create and edit"},
	{Name: "reader"},
}

// newRulesService builds an AuthService with a GitHub and an OIDC provider and the given
// rules (the new config form, so nothing is admitted without a rule).
func newRulesService(t *testing.T, rules ...config.AccessRule) *AuthService {
	t.Helper()
	cfg := &config.AuthConfig{
		Enabled: true,
		Issuer:  "https://mcp.example.com",
		Storage: config.StorageConfig{DBPath: filepath.Join(t.TempDir(), "auth.db")},
		GitHub:  config.GitHubConfig{ClientID: "gh-id", ClientSecret: "gh-secret"},
		Providers: []config.ProviderConfig{{
			Name: "corp", Type: config.ProviderTypeOIDC,
			IssuerURL: "https://login.example.com", ClientID: "corp-client",
		}},
		Access: &config.AccessConfig{Rules: rules},
	}
	svc, err := NewAuthService(cfg, WithRoles(testRoles...))
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}
	t.Cleanup(func() { svc.Close() })
	return svc
}

func TestEvaluateRules(t *testing.T) {
	svc := newRulesService(t,
		config.AccessRule{Provider: "github", Users: []string{"Spirilis"}, Roles: []string{"admin"}},
		config.AccessRule{Provider: "github", Orgs: []string{"Acme"}, Teams: []string{"acme/Devs"}, Roles: []string{"writer"}},
		config.AccessRule{Provider: "corp", Subjects: []string{"sub-123"}, Roles: []string{"admin", "writer"}},
		config.AccessRule{Provider: "corp", Emails: []string{"Boss@Example.org"}, Roles: []string{"admin"}},
		config.AccessRule{Provider: "corp", EmailDomains: []string{"@example.com"}, Roles: []string{"reader"}},
		config.AccessRule{Provider: "corp", Groups: []string{"mcp-admins"}, Roles: []string{"admin"}},
	)

	tests := []struct {
		name      string
		id        Identity
		wantOK    bool
		wantRoles []string
	}{
		{"github login, case-insensitive", Identity{Provider: "github", Username: "SPIRILIS"}, true, []string{"admin"}},
		{"github org alias, case-insensitive", Identity{Provider: "github", Username: "x", Groups: []string{"acme"}}, true, []string{"writer"}},
		{"github team alias", Identity{Provider: "github", Username: "x", Groups: []string{"ACME/devs"}}, true, []string{"writer"}},
		{"github roles are a sorted union", Identity{Provider: "github", Username: "spirilis", Groups: []string{"acme"}}, true, []string{"admin", "writer"}},
		{"github org's team is not the org", Identity{Provider: "github", Username: "x", Groups: []string{"acme/ops"}}, false, nil},
		{"github nobody", Identity{Provider: "github", Username: "stranger"}, false, nil},
		{"login rule does not cross providers", Identity{Provider: "corp", Username: "spirilis", Subject: "s"}, false, nil},
		{"oidc subject exact", Identity{Provider: "corp", Subject: "sub-123"}, true, []string{"admin", "writer"}},
		{"oidc subject is case-sensitive", Identity{Provider: "corp", Subject: "SUB-123"}, false, nil},
		{"verified email", Identity{Provider: "corp", Subject: "s", Email: "boss@example.ORG", EmailVerified: true}, true, []string{"admin"}},
		{"unverified email", Identity{Provider: "corp", Subject: "s", Email: "boss@example.org"}, false, nil},
		{"verified email domain", Identity{Provider: "corp", Subject: "s", Email: "a@Example.com", EmailVerified: true}, true, []string{"reader"}},
		{"unverified email domain", Identity{Provider: "corp", Subject: "s", Email: "a@example.com"}, false, nil},
		{"domain is not a suffix match", Identity{Provider: "corp", Subject: "s", Email: "a@evil-example.com", EmailVerified: true}, false, nil},
		{"subdomain is not the domain", Identity{Provider: "corp", Subject: "s", Email: "a@sub.example.com", EmailVerified: true}, false, nil},
		{"domain needs a local part", Identity{Provider: "corp", Subject: "s", Email: "@example.com", EmailVerified: true}, false, nil},
		{"oidc group exact", Identity{Provider: "corp", Subject: "s", Groups: []string{"staff", "mcp-admins"}}, true, []string{"admin"}},
		{"oidc group is case-sensitive", Identity{Provider: "corp", Subject: "s", Groups: []string{"MCP-Admins"}}, false, nil},
		{"several rules, deduplicated roles", Identity{Provider: "corp", Subject: "sub-123", Groups: []string{"mcp-admins"},
			Email: "a@example.com", EmailVerified: true}, true, []string{"admin", "reader", "writer"}},
		{"unknown provider", Identity{Provider: "gone", Subject: "sub-123"}, false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			roles, ok := svc.access.evaluate(tt.id)
			if ok != tt.wantOK || !reflect.DeepEqual(roles, tt.wantRoles) {
				t.Fatalf("evaluate = %v, %v; want %v, %v", roles, ok, tt.wantRoles, tt.wantOK)
			}
		})
	}
}

func TestEvaluateAnyoneAndAdmissionWithoutRoles(t *testing.T) {
	svc := newRulesService(t, config.AccessRule{Provider: "corp", Anyone: true})
	if roles, ok := svc.access.evaluate(Identity{Provider: "corp", Subject: "x"}); !ok || len(roles) != 0 {
		t.Errorf("anyone rule: %v, %v; want admitted with no roles", roles, ok)
	}
	// Fail closed: the github provider has no rule.
	if _, ok := svc.access.evaluate(Identity{Provider: "github", Username: "x"}); ok {
		t.Error("a provider with no rule admitted a user")
	}
}

func TestRequireVerifiedEmailOff(t *testing.T) {
	no := false
	cfg := &config.AuthConfig{
		Storage: config.StorageConfig{DBPath: filepath.Join(t.TempDir(), "auth.db")},
		Providers: []config.ProviderConfig{{Name: "corp", Type: "oidc", IssuerURL: "https://l.example.com",
			ClientID: "c", RequireVerifiedEmail: &no}},
		Access: &config.AccessConfig{Rules: []config.AccessRule{{Provider: "corp", EmailDomains: []string{"example.com"}}}},
	}
	svc, err := NewAuthService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if _, ok := svc.access.evaluate(Identity{Provider: "corp", Subject: "s", Email: "a@example.com"}); !ok {
		t.Error("requireVerifiedEmail: false should admit an unverified email")
	}
}

func TestNewAuthServiceRoleValidation(t *testing.T) {
	tests := []struct {
		name    string
		roles   []Role
		rule    config.AccessRule
		wantErr string
	}{
		{"unknown role", testRoles, config.AccessRule{Provider: "github", Users: []string{"a"}, Roles: []string{"amdin"}}, `unknown role "amdin"`},
		{"roles but app declared none", nil, config.AccessRule{Provider: "github", Users: []string{"a"}, Roles: []string{"admin"}}, "declares no roles"},
		{"duplicate declared role", []Role{{Name: "a"}, {Name: "a"}}, config.AccessRule{Provider: "github", Users: []string{"a"}}, "declared twice"},
		{"empty declared role", []Role{{Name: ""}}, config.AccessRule{Provider: "github", Users: []string{"a"}}, "must be non-empty"},
		{"config error surfaces", testRoles, config.AccessRule{Provider: "nope", Anyone: true}, "unknown provider"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "auth.db")
			cfg := &config.AuthConfig{
				Storage: config.StorageConfig{DBPath: dbPath},
				GitHub:  config.GitHubConfig{ClientID: "x"},
				Access:  &config.AccessConfig{Rules: []config.AccessRule{tt.rule}},
			}
			svc, err := NewAuthService(cfg, WithRoles(tt.roles...))
			if err == nil {
				svc.Close()
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}
			if _, statErr := os.Stat(dbPath); statErr == nil {
				t.Error("the database was created although the configuration was refused")
			}
		})
	}
}

// An unreadable mounted secret refuses to start, before the database is opened. It used to be
// ignored, leaving an empty client secret that failed only at a user's login.
func TestNewAuthServiceUnreadableSecretFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "auth.db")
	cfg := &config.AuthConfig{
		Storage: config.StorageConfig{DBPath: dbPath},
		GitHub:  config.GitHubConfig{ClientID: "x", ClientSecretFile: filepath.Join(t.TempDir(), "missing")},
	}
	svc, err := NewAuthService(cfg)
	if err == nil {
		svc.Close()
		t.Fatal("NewAuthService started with an unreadable clientSecretFile")
	}
	if !strings.Contains(err.Error(), "auth.github.clientSecretFile") {
		t.Errorf("err = %v, want it to name auth.github.clientSecretFile", err)
	}
	if _, statErr := os.Stat(dbPath); statErr == nil {
		t.Error("the database was created although the configuration was refused")
	}
}

func TestRolesAccessors(t *testing.T) {
	svc := newRulesService(t, config.AccessRule{Provider: "github", Anyone: true})
	got := svc.Roles()
	if !reflect.DeepEqual(got, testRoles) {
		t.Errorf("Roles() = %v", got)
	}
	got[0].Name = "mutated"
	if svc.Roles()[0].Name != "admin" {
		t.Error("Roles() exposes the service's slice")
	}

	p := &Principal{Roles: []string{"admin", "writer"}}
	ctx := context.WithValue(context.Background(), ContextKeyPrincipal, p)
	if !HasRole(ctx, "writer") || HasRole(ctx, "reader") || !p.HasRole("admin") {
		t.Error("HasRole")
	}
	if !reflect.DeepEqual(svc.RolesFromContext(ctx), []string{"admin", "writer"}) {
		t.Errorf("RolesFromContext = %v", svc.RolesFromContext(ctx))
	}
	if HasRole(context.Background(), "admin") || svc.RolesFromContext(context.Background()) != nil {
		t.Error("no principal must hold no roles")
	}
	if _, ok := PrincipalFromContext(context.Background()); ok {
		t.Error("PrincipalFromContext without a principal")
	}
	var nilP *Principal
	if nilP.HasRole("admin") {
		t.Error("nil principal holds a role")
	}
}

// A user record as v0.11 stored it: no provider, no snapshot.
const legacyUserJSON = `{"id":"u-legacy","github_login":"OctoCat","github_id":583231,"email":"octo@example.com","name":"The Octocat"}`

func storeLegacyUser(t *testing.T, svc *AuthService) *User {
	t.Helper()
	var u User
	if err := json.Unmarshal([]byte(legacyUserJSON), &u); err != nil {
		t.Fatal(err)
	}
	if err := svc.storage.StoreUser(context.Background(), &u); err != nil {
		t.Fatal(err)
	}
	return &u
}

func TestIdentityFromLegacyUser(t *testing.T) {
	var u User
	if err := json.Unmarshal([]byte(legacyUserJSON), &u); err != nil {
		t.Fatal(err)
	}
	got := identityFromUser(&u)
	// Every pre-v0.12 record is a github.com user.
	want := Identity{Provider: "github", Issuer: "https://github.com", Subject: "583231", Username: "OctoCat", Email: "octo@example.com", Name: "The Octocat"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("identity = %+v, want %+v", got, want)
	}

	// New fields are additive: the legacy keys keep their names.
	setIdentitySnapshot(&u, Identity{Provider: "github", Issuer: "https://github.com", Subject: "583231", Username: "OctoCat",
		Groups: []string{"acme"}}, time.Unix(1700000000, 0))
	data, _ := json.Marshal(&u)
	for _, key := range []string{`"github_login":"OctoCat"`, `"github_id":583231`, `"provider":"github"`,
		`"groups":["acme"]`, `"last_login_at":"2023-11-14T22:13:20Z"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("stored user %s lacks %s", data, key)
		}
	}
	if strings.Contains(legacyUserJSON, "last_login_at") {
		t.Fatal("fixture is not legacy")
	}
	var fresh User
	_ = json.Unmarshal([]byte(legacyUserJSON), &fresh)
	if out, _ := json.Marshal(&fresh); strings.Contains(string(out), "last_login_at") {
		t.Errorf("a record with no snapshot gained last_login_at: %s", out)
	}
}

// serveAuthed runs Middleware with a bearer token, recording the principal the handler saw.
func serveAuthed(t *testing.T, svc *AuthService, token string) (*httptest.ResponseRecorder, *Principal) {
	t.Helper()
	var seen *Principal
	h := svc.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = PrincipalFromContext(r.Context())
		if GetUserFromContext(r.Context()) == nil {
			t.Error("the *User is missing from the context")
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, seen
}

func tokenFor(t *testing.T, svc *AuthService, userID string) string {
	t.Helper()
	at, err := svc.tokenService.GenerateAccessToken(userID, "client-1", "mcp", "")
	if err != nil {
		t.Fatal(err)
	}
	return at.Token
}

func TestMiddlewarePrincipal(t *testing.T) {
	svc := newRulesService(t,
		config.AccessRule{Provider: "github", Teams: []string{"acme/devs"}, Roles: []string{"writer"}},
		config.AccessRule{Provider: "corp", Groups: []string{"mcp-admins"}, Roles: []string{"admin", "writer"}},
	)
	now := time.Now()
	gh := &User{ID: "u-gh", GitHubLogin: "OctoCat", GitHubID: 1}
	setIdentitySnapshot(gh, Identity{Provider: "github", Issuer: "https://github.com", Subject: "1", Username: "OctoCat", Groups: []string{"acme", "acme/devs"}}, now)
	oidc := &User{ID: "u-corp"}
	setIdentitySnapshot(oidc, Identity{Provider: "corp", Issuer: "https://login.example.com", Subject: "0b5c-sub", Username: "jdoe", Email: "j@example.com",
		EmailVerified: true, Name: "J Doe", Groups: []string{"mcp-admins"}}, now)

	tests := []struct {
		name  string
		user  *User
		want  Principal
		login string
	}{
		{"github", gh, Principal{ID: "github:octocat", UserID: "u-gh", Provider: "github", Subject: "1", Username: "OctoCat",
			Groups: []string{"acme", "acme/devs"}, Roles: []string{"writer"}}, "OctoCat"},
		{"oidc keyed by sub", oidc, Principal{ID: "corp:0b5c-sub", UserID: "u-corp", Provider: "corp", Subject: "0b5c-sub",
			Username: "jdoe", Email: "j@example.com", EmailVerified: true, Name: "J Doe",
			Groups: []string{"mcp-admins"}, Roles: []string{"admin", "writer"}}, "jdoe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := svc.storage.StoreUser(context.Background(), tt.user); err != nil {
				t.Fatal(err)
			}
			rec, p := serveAuthed(t, svc, tokenFor(t, svc, tt.user.ID))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d %s", rec.Code, rec.Body)
			}
			if p == nil || !reflect.DeepEqual(*p, tt.want) {
				t.Fatalf("principal = %+v, want %+v", p, tt.want)
			}
			ctx := context.WithValue(context.Background(), ContextKeyUser, tt.user)
			if id, login, ok := svc.UserFromContext(ctx); !ok || id != tt.user.ID || login != tt.login {
				t.Errorf("UserFromContext = %q, %q, %v", id, login, ok)
			}
		})
	}
}

// Rules are evaluated per request: the same token is refused once its rule is gone.
func TestMiddlewareAppliesRuleChangesToIssuedTokens(t *testing.T) {
	svc := newRulesService(t, config.AccessRule{Provider: "github", Users: []string{"octocat"}})
	u := &User{ID: "u-1", GitHubLogin: "octocat", GitHubID: 1}
	setIdentitySnapshot(u, Identity{Provider: "github", Issuer: "https://github.com", Subject: "1", Username: "octocat"}, time.Now())
	if err := svc.storage.StoreUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	token := tokenFor(t, svc, u.ID)
	if rec, _ := serveAuthed(t, svc, token); rec.Code != http.StatusOK {
		t.Fatalf("before: status = %d %s", rec.Code, rec.Body)
	}

	other := newRulesService(t, config.AccessRule{Provider: "github", Users: []string{"someone-else"}})
	svc.access = other.access
	rec, p := serveAuthed(t, svc, token)
	if rec.Code != http.StatusForbidden || p != nil {
		t.Fatalf("after: status = %d, principal %v; want 403 and no handler call", rec.Code, p)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "insufficient_scope" {
		t.Errorf("body = %v, want insufficient_scope", body)
	}
}

// Records stored before v0.12.0 carry no groups. One that no rule matches without them gets a
// 401 when a group rule for its provider could match after a sign-in (which takes a snapshot),
// rather than a 403 it cannot recover from. With no such rule a sign-in cannot change the
// outcome, so it gets the 403 v0.11 gave. One that matches on its login is served as before.
func TestMiddlewareLegacyUserRecord(t *testing.T) {
	tests := []struct {
		name       string
		rules      []config.AccessRule
		wantStatus int
		wantID     string
	}{
		{"matches on login", []config.AccessRule{{Provider: "github", Users: []string{"octocat"}}}, http.StatusOK, "github:octocat"},
		{"matches on subject", []config.AccessRule{{Provider: "github", Subjects: []string{"583231"}}}, http.StatusOK, "github:octocat"},
		{"org rule needs a snapshot", []config.AccessRule{{Provider: "github", Orgs: []string{"acme"}}}, http.StatusUnauthorized, ""},
		{"team rule needs a snapshot", []config.AccessRule{{Provider: "github", Teams: []string{"acme/devs"}}}, http.StatusUnauthorized, ""},
		{"removed from a users rule", []config.AccessRule{{Provider: "github", Users: []string{"someone-else"}}}, http.StatusForbidden, ""},
		// A group rule for another provider cannot admit a GitHub user.
		{"group rule only for another provider", []config.AccessRule{
			{Provider: "github", Users: []string{"someone-else"}},
			{Provider: "corp", Groups: []string{"staff"}},
		}, http.StatusForbidden, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newRulesService(t, tt.rules...)
			u := storeLegacyUser(t, svc)
			rec, p := serveAuthed(t, svc, tokenFor(t, svc, u.ID))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body, tt.wantStatus)
			}
			if tt.wantID != "" && (p == nil || p.ID != tt.wantID) {
				t.Errorf("principal = %+v, want ID %s", p, tt.wantID)
			}
			if tt.wantStatus == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 without WWW-Authenticate")
			}
		})
	}
}

// The legacy allowlist form keeps v0.11's answer for a user taken off it: 403, not a sign-in
// loop. (genome-vcf-mcp's end-to-end test pins this.)
func TestMiddlewareLegacyAllowlistRemovedUser(t *testing.T) {
	svc := newConsentTestService(t, func(c *config.AuthConfig) {
		c.GitHub = config.GitHubConfig{ClientID: "gh-id", ClientSecret: "gh-secret"}
		c.Allowlist = config.AllowlistConfig{Users: []string{"someone"}}
	})
	u := storeLegacyUser(t, svc)
	if rec, _ := serveAuthed(t, svc, tokenFor(t, svc, u.ID)); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d %s, want 403", rec.Code, rec.Body)
	}
}

// A stored record is only judged by its provider's rules while it is from the issuer that
// provider has now: an OIDC issuerURL that changed (another realm), or the provider named
// github pointed at an Enterprise Server, is another user space where the same sub or login
// need not be the same person. Such a record gets a 401 (a sign-in finds or makes the user
// under the current issuer) and its refresh tokens stop working. NewAuthService refuses to start
// over such records (TestNewAuthServiceRefusesIssuerChangeUnderOneName); these are stored after
// it started, which is what this second line covers.
func TestStoredIssuerMustMatchProvider(t *testing.T) {
	newGHE := func(t *testing.T) *AuthService {
		return newConsentTestService(t, func(c *config.AuthConfig) {
			c.Providers = []config.ProviderConfig{{Name: "github", Type: config.ProviderTypeGitHub,
				ClientID: "id", ClientSecret: "s", BaseURL: "https://ghe.example.com"}}
			c.Access = &config.AccessConfig{Rules: []config.AccessRule{{Provider: "github", Users: []string{"octocat"}}}}
		})
	}
	corpRule := config.AccessRule{Provider: "corp", Subjects: []string{"sub-1"}}
	tests := []struct {
		name       string
		svc        func(*testing.T) *AuthService
		user       func() *User
		wantStatus int
	}{
		{"oidc, same issuer", func(t *testing.T) *AuthService { return newRulesService(t, corpRule) }, func() *User {
			u := &User{ID: "u"}
			setIdentitySnapshot(u, Identity{Provider: "corp", Issuer: "https://login.example.com", Subject: "sub-1"}, time.Now())
			return u
		}, http.StatusOK},
		{"oidc, former issuer", func(t *testing.T) *AuthService { return newRulesService(t, corpRule) }, func() *User {
			u := &User{ID: "u"}
			setIdentitySnapshot(u, Identity{Provider: "corp", Issuer: "https://old.example.com", Subject: "sub-1"}, time.Now())
			return u
		}, http.StatusUnauthorized},
		{"github.com record, github is now an Enterprise Server", newGHE, func() *User {
			u := &User{ID: "u", GitHubLogin: "octocat", GitHubID: 1}
			setIdentitySnapshot(u, Identity{Provider: "github", Issuer: "https://github.com", Subject: "1", Username: "octocat"}, time.Now())
			return u
		}, http.StatusUnauthorized},
		{"pre-v0.12 record, github is now an Enterprise Server", newGHE, func() *User {
			return &User{ID: "u", GitHubLogin: "octocat", GitHubID: 1}
		}, http.StatusUnauthorized},
		// Nobody can sign in with a provider that is gone, so a sign-in cannot help: 403.
		{"provider no longer configured", func(t *testing.T) *AuthService { return newRulesService(t, corpRule) }, func() *User {
			u := &User{ID: "u"}
			setIdentitySnapshot(u, Identity{Provider: "gone", Issuer: "https://gone.example.com", Subject: "sub-1"}, time.Now())
			return u
		}, http.StatusForbidden},
		{"pre-v0.12 record, github.com", func(t *testing.T) *AuthService {
			return newRulesService(t, config.AccessRule{Provider: "github", Users: []string{"octocat"}})
		}, func() *User { return &User{ID: "u", GitHubLogin: "octocat", GitHubID: 1} }, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := tt.svc(t)
			if err := svc.storage.StoreUser(context.Background(), tt.user()); err != nil {
				t.Fatal(err)
			}
			if rec, _ := serveAuthed(t, svc, tokenFor(t, svc, "u")); rec.Code != tt.wantStatus {
				t.Fatalf("Middleware: status = %d %s, want %d", rec.Code, rec.Body, tt.wantStatus)
			}
			client := storeTestClient(t, svc)
			rt, err := svc.tokenService.GenerateRefreshToken("u", client.ClientID, "", "")
			if err != nil {
				t.Fatal(err)
			}
			rec := tokenPOST(t, svc, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt.Token}, "client_id": {client.ClientID}})
			if wantOK := tt.wantStatus == http.StatusOK; (rec.Code == http.StatusOK) != wantOK {
				t.Errorf("refresh grant: status = %d %s, want success %v", rec.Code, rec.Body, wantOK)
			}
		})
	}
}

// HasRole does not depend on Roles being sorted: embedders build Principals themselves.
func TestPrincipalHasRole(t *testing.T) {
	tests := []struct {
		name string
		p    *Principal
		role string
		want bool
	}{
		{"unsorted roles", &Principal{Roles: []string{"writer", "admin"}}, "admin", true},
		{"absent", &Principal{Roles: []string{"writer"}}, "admin", false},
		{"no roles", &Principal{}, "admin", false},
		{"nil principal", nil, "admin", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.HasRole(tt.role); got != tt.want {
				t.Errorf("HasRole(%q) = %v, want %v", tt.role, got, tt.want)
			}
		})
	}
}

// The legacy form with an empty allowlist still admits every GitHub user, old records included.
func TestMiddlewareLegacyOpenConfig(t *testing.T) {
	svc, _ := newTestAuthService(t)
	u := storeLegacyUser(t, svc)
	rec, p := serveAuthed(t, svc, tokenFor(t, svc, u.ID))
	if rec.Code != http.StatusOK || p == nil || p.ID != "github:octocat" || len(p.Roles) != 0 {
		t.Fatalf("status = %d, principal %+v", rec.Code, p)
	}
}

// The refresh grant re-checks the rules, so a user no rule matches cannot mint new tokens.
func TestRefreshGrantRechecksRules(t *testing.T) {
	svc := newRulesService(t, config.AccessRule{Provider: "github", Users: []string{"octocat"}})
	// CreatedAt, or the startup sweep may remove it as an unused registration.
	client := &RegisteredClient{ClientID: "client-1", RedirectURIs: []string{"https://app.example.com/cb"}, CreatedAt: time.Now()}
	if err := svc.storage.StoreClient(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	refresh := func(userID string) *httptest.ResponseRecorder {
		rt, err := svc.tokenService.GenerateRefreshToken(userID, client.ClientID, "mcp", "")
		if err != nil {
			t.Fatal(err)
		}
		form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt.Token}, "client_id": {client.ClientID}}
		req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		svc.handleToken(rec, req)
		return rec
	}

	for _, u := range []*User{{ID: "ok", GitHubLogin: "octocat", GitHubID: 1}, {ID: "denied", GitHubLogin: "mallory", GitHubID: 2}} {
		setIdentitySnapshot(u, Identity{Provider: "github", Issuer: "https://github.com", Username: u.GitHubLogin}, time.Now())
		if err := svc.storage.StoreUser(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	if rec := refresh("ok"); rec.Code != http.StatusOK {
		t.Errorf("admitted user: %d %s", rec.Code, rec.Body)
	}
	for _, id := range []string{"denied", "no-such-user"} {
		if rec := refresh(id); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_grant") {
			t.Errorf("%s: %d %s, want invalid_grant", id, rec.Code, rec.Body)
		}
	}
}

// fakeGitHub answers GitHub's token and API endpoints in-process.
type fakeGitHub struct {
	id    int64 // default 42
	login string
	orgs  []string
	teams []string // "org/slug"
}

func (f *fakeGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	var body any
	switch {
	case r.URL.Path == "/login/oauth/access_token":
		body = map[string]string{"access_token": "gh-token", "token_type": "bearer"}
	case r.URL.Path == "/user":
		id := f.id
		if id == 0 {
			id = 42
		}
		body = map[string]any{"id": id, "login": f.login, "name": "N", "email": "e@example.com"}
	case r.URL.Path == "/user/orgs":
		orgs := []map[string]any{}
		for _, o := range f.orgs {
			orgs = append(orgs, map[string]any{"login": o})
		}
		body = orgs
	case r.URL.Path == "/user/teams":
		teams := []map[string]any{}
		for _, t := range f.teams {
			org, slug, _ := strings.Cut(t, "/")
			teams = append(teams, map[string]any{"slug": slug, "organization": map[string]any{"login": org}})
		}
		body = teams
	default:
		return nil, http.ErrNotSupported
	}
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rec).Encode(body)
	return rec.Result(), nil
}

// setGitHubTransport sends the "github" provider's requests to rt.
func setGitHubTransport(svc *AuthService, rt http.RoundTripper) {
	svc.providerByName["github"].(*githubProvider).client.httpClient = &http.Client{Transport: rt}
}

// The GitHub login still works end to end, with the legacy allowlist decided by the rules
// engine, and the login records the identity snapshot later requests are judged on.
func TestGitHubCallbackUsesRules(t *testing.T) {
	tests := []struct {
		name      string
		allowlist config.AllowlistConfig
		gh        fakeGitHub
		wantCode  bool
	}{
		{"allowlisted user", config.AllowlistConfig{Users: []string{"OctoCat"}}, fakeGitHub{login: "octocat"}, true},
		{"allowlisted org", config.AllowlistConfig{Orgs: []string{"acme"}}, fakeGitHub{login: "x", orgs: []string{"Acme"}}, true},
		{"allowlisted team", config.AllowlistConfig{Teams: []config.OrgTeam{{Org: "acme", Team: "devs"}}},
			fakeGitHub{login: "x", orgs: []string{"acme"}, teams: []string{"acme/devs"}}, true},
		{"wrong team", config.AllowlistConfig{Teams: []config.OrgTeam{{Org: "acme", Team: "devs"}}},
			fakeGitHub{login: "x", orgs: []string{"acme"}, teams: []string{"acme/ops"}}, false},
		{"not allowlisted", config.AllowlistConfig{Users: []string{"someone"}}, fakeGitHub{login: "x"}, false},
		{"empty allowlist admits all", config.AllowlistConfig{}, fakeGitHub{login: "anybody"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.AuthConfig{
				Enabled:   true,
				Issuer:    "https://mcp.example.com",
				Storage:   config.StorageConfig{DBPath: filepath.Join(t.TempDir(), "auth.db")},
				GitHub:    config.GitHubConfig{ClientID: "gh-id", ClientSecret: "gh-secret"},
				Allowlist: tt.allowlist,
				Clients: []config.StaticClient{{ClientID: "trusted", ClientSecret: "s",
					RedirectURIs: []string{"https://app.example.com/cb"}}},
			}
			svc, err := NewAuthService(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			gh := tt.gh
			setGitHubTransport(svc, &gh)

			if err := svc.storage.StoreAuthRequest(context.Background(), &PendingAuthRequest{
				ID: "req-1", ClientID: "trusted", RedirectURI: "https://app.example.com/cb", State: "st",
				CodeChallenge: strings.Repeat("a", 43), CodeChallengeMethod: "S256",
				CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
			}); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			svc.handleCallback(rec, httptest.NewRequest(http.MethodGet, "/callback?code=c&state=req-1", nil))

			loc := rec.Header().Get("Location")
			if got := strings.Contains(loc, "code="); got != tt.wantCode {
				t.Fatalf("Location = %q, want code issued = %v", loc, tt.wantCode)
			}
			if !tt.wantCode {
				if !strings.Contains(loc, "error=access_denied") {
					t.Errorf("Location = %q, want access_denied", loc)
				}
				return
			}
			u, err := svc.storage.GetUserByGitHubLogin(context.Background(), gh.login)
			if err != nil {
				t.Fatal(err)
			}
			want := append(append([]string{}, gh.orgs...), gh.teams...)
			if u.Provider != "github" || u.Subject != "42" || u.Username != gh.login || u.LastLoginAt.IsZero() ||
				len(u.Groups) != len(want) || (len(want) > 0 && !reflect.DeepEqual(u.Groups, want)) {
				t.Errorf("stored user = %+v, want a snapshot with groups %v", u, want)
			}
			if p, ok := svc.authorizeUser(u); !ok || p.ID != "github:"+strings.ToLower(gh.login) {
				t.Errorf("stored user not admitted on later requests: %+v", p)
			}
		})
	}
}

// Upgrading: a user stored by v0.11 (no snapshot) who was admitted through an org is refused
// with a 401 until they sign in again; the login keeps their user id and writes the snapshot,
// after which the same kind of token is served.
func TestLegacyUserUpgradePath(t *testing.T) {
	cfg := &config.AuthConfig{
		Enabled:   true,
		Issuer:    "https://mcp.example.com",
		Storage:   config.StorageConfig{DBPath: filepath.Join(t.TempDir(), "auth.db")},
		GitHub:    config.GitHubConfig{ClientID: "gh-id", ClientSecret: "gh-secret"},
		Allowlist: config.AllowlistConfig{Orgs: []string{"acme"}},
		Clients: []config.StaticClient{{ClientID: "trusted", ClientSecret: "s",
			RedirectURIs: []string{"https://app.example.com/cb"}}},
	}
	svc, err := NewAuthService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	setGitHubTransport(svc, &fakeGitHub{id: 583231, login: "OctoCat", orgs: []string{"acme"}})
	legacy := storeLegacyUser(t, svc)

	if rec, _ := serveAuthed(t, svc, tokenFor(t, svc, legacy.ID)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("before re-login: status = %d, want 401", rec.Code)
	}

	if err := svc.storage.StoreAuthRequest(context.Background(), &PendingAuthRequest{
		ID: "req-1", ClientID: "trusted", RedirectURI: "https://app.example.com/cb",
		CodeChallenge: strings.Repeat("a", 43), CodeChallengeMethod: "S256",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	svc.handleCallback(rec, httptest.NewRequest(http.MethodGet, "/callback?code=c&state=req-1", nil))
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "code=") {
		t.Fatalf("callback Location = %q, want a code", loc)
	}

	u, err := svc.storage.GetUser(context.Background(), legacy.ID)
	if err != nil {
		t.Fatalf("the re-login did not keep the user id: %v", err)
	}
	if u.LastLoginAt.IsZero() || !reflect.DeepEqual(u.Groups, []string{"acme"}) || u.GitHubID != 583231 {
		t.Errorf("user after re-login = %+v", u)
	}
	rec2, p := serveAuthed(t, svc, tokenFor(t, svc, legacy.ID))
	if rec2.Code != http.StatusOK || p == nil || p.ID != "github:octocat" {
		t.Fatalf("after re-login: status = %d, principal %+v", rec2.Code, p)
	}
}

// An OIDC provider whose IdP is unreachable does not stop the service starting; /authorize
// answers 503 for it, before it stores anything.
func TestAuthorizeProviderUnreachable(t *testing.T) {
	cfg := &config.AuthConfig{
		Issuer:  "https://mcp.example.com",
		Storage: config.StorageConfig{DBPath: filepath.Join(t.TempDir(), "auth.db")},
		// Port 1 on loopback refuses connections at once: no DNS, no wait.
		Providers: []config.ProviderConfig{{Name: "corp", Type: "oidc", IssuerURL: "https://127.0.0.1:1", ClientID: "c"}},
		Access:    &config.AccessConfig{Rules: []config.AccessRule{{Provider: "corp", Anyone: true}}},
	}
	svc, err := NewAuthService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	rec := startAuthorize(t, svc, storeTestClient(t, svc))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d %s, want 503", rec.Code, rec.Body)
	}
	if n := countBucket(t, svc, BucketAuthRequests); n != 0 {
		t.Errorf("%d pending requests stored for a provider that is down", n)
	}
}

// Principal.ID has no issuer in it, so a provider name whose issuer changed while the store
// holds its earlier users would give the new issuer's users the old ones' ids. NewAuthService
// refuses that, and tells the operator to use a new provider name.
func TestNewAuthServiceRefusesIssuerChangeUnderOneName(t *testing.T) {
	corpAt := func(name, issuer string) config.ProviderConfig {
		return config.ProviderConfig{Name: name, Type: config.ProviderTypeOIDC, IssuerURL: issuer, ClientID: "c"}
	}
	corpUser := func() *User {
		u := &User{ID: "u-corp"}
		setIdentitySnapshot(u, Identity{Provider: "corp", Issuer: "https://a.example.com", Subject: "1234"}, time.Now())
		return u
	}
	legacyUser := func() *User {
		var u User
		if err := json.Unmarshal([]byte(legacyUserJSON), &u); err != nil {
			t.Fatal(err)
		}
		return &u
	}
	newForm := func(ps ...config.ProviderConfig) func(*config.AuthConfig) {
		return func(c *config.AuthConfig) {
			c.Providers = ps
			c.Access = &config.AccessConfig{}
		}
	}
	tests := []struct {
		name    string
		stored  func() *User
		mutate  func(*config.AuthConfig)
		wantErr bool
	}{
		{"oidc, same issuer", corpUser, newForm(corpAt("corp", "https://a.example.com")), false},
		{"oidc, issuer changed under the same name", corpUser, newForm(corpAt("corp", "https://b.example.com")), true},
		{"oidc, new issuer under a new name", corpUser, newForm(corpAt("corp-b", "https://b.example.com")), false},
		{"oidc provider removed", corpUser, newForm(corpAt("other", "https://b.example.com")), false},
		{"pre-v0.12 record, legacy form", legacyUser, func(c *config.AuthConfig) {
			c.GitHub = config.GitHubConfig{ClientID: "id", ClientSecret: "s"}
		}, false},
		{"pre-v0.12 record, github is now an Enterprise Server", legacyUser, newForm(config.ProviderConfig{
			Name: "github", Type: config.ProviderTypeGitHub, ClientID: "id", ClientSecret: "s", BaseURL: "https://ghe.example.com"}), true},
		{"pre-v0.12 record, github is now an OIDC provider", legacyUser, newForm(corpAt("github", "https://a.example.com")), true},
		{"pre-v0.12 record, Enterprise Server under a new name", legacyUser, newForm(config.ProviderConfig{
			Name: "ghe", Type: config.ProviderTypeGitHub, ClientID: "id", ClientSecret: "s", BaseURL: "https://ghe.example.com"}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "auth.db")
			store, err := NewBoltStorage(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.StoreUser(context.Background(), tt.stored()); err != nil {
				t.Fatal(err)
			}
			store.Close()

			cfg := &config.AuthConfig{Enabled: true, Issuer: testIssuer, Storage: config.StorageConfig{DBPath: dbPath}}
			tt.mutate(cfg)
			svc, err := NewAuthService(cfg)
			if err == nil {
				svc.Close()
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewAuthService: err = %v, want error = %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "new name") {
				t.Errorf("error = %v, want it to tell the operator to use a new provider name", err)
			}
			// A refused start must not hold the database open.
			if err != nil {
				reopened, rerr := NewBoltStorage(dbPath)
				if rerr != nil {
					t.Fatalf("database still held after a refused start: %v", rerr)
				}
				reopened.Close()
			}
		})
	}
}
