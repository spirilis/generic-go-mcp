package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// authFromYAML parses an auth: block the way Load does.
func authFromYAML(t *testing.T, yaml string) *AuthConfig {
	t.Helper()
	cfg, err := LoadFromString(yaml)
	if err != nil {
		t.Fatalf("LoadFromString: %v", err)
	}
	if cfg.Auth == nil {
		t.Fatal("no auth block")
	}
	return cfg.Auth
}

// The legacy YAML that v0.10–v0.11 consumers ship must resolve exactly as the same settings
// written as a struct literal (which is how embedders and tests build it).
func TestResolveLegacyYAMLMatchesStructLiteral(t *testing.T) {
	fromYAML := authFromYAML(t, `
auth:
  enabled: true
  issuer: https://mcp.example.com
  github: {clientId: id1, clientSecret: sec1}
  allowlist:
    users: [Spirilis]
    orgs: [acme]
    teams: [{org: acme, team: devs}]
`)
	literal := &AuthConfig{
		Enabled: true,
		Issuer:  "https://mcp.example.com",
		GitHub:  GitHubConfig{ClientID: "id1", ClientSecret: "sec1"},
		Allowlist: AllowlistConfig{
			Users: []string{"Spirilis"},
			Orgs:  []string{"acme"},
			Teams: []OrgTeam{{Org: "acme", Team: "devs"}},
		},
	}
	a, err := fromYAML.Resolve()
	if err != nil {
		t.Fatalf("YAML: %v", err)
	}
	b, err := literal.Resolve()
	if err != nil {
		t.Fatalf("literal: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("YAML and literal resolve differently:\n%+v\n%+v", a, b)
	}

	want := ResolvedRule{
		AccessRule: AccessRule{Provider: "github", Users: []string{"Spirilis"}, Orgs: []string{"acme"},
			Teams: []string{"acme/devs"}},
		Where: "auth.allowlist",
	}
	if len(a.Rules) != 1 || !reflect.DeepEqual(a.Rules[0], want) {
		t.Errorf("rules = %+v, want [%+v]", a.Rules, want)
	}
	if a.LegacyOpen {
		t.Error("LegacyOpen set although the allowlist is not empty")
	}
	gh, ok := a.Provider("github")
	if !ok || gh.ClientID != "id1" || gh.ClientSecret != "sec1" || gh.Type != ProviderTypeGitHub ||
		gh.BaseURL != "https://github.com" || gh.APIURL != "https://api.github.com" {
		t.Errorf("github provider = %+v", gh)
	}
}

// Which forms keep the legacy "empty allowlist admits everyone" behaviour.
func TestResolveLegacyOpenOnlyForPureLegacyForm(t *testing.T) {
	oidc := ProviderConfig{Name: "corp", Type: ProviderTypeOIDC, IssuerURL: "https://login.example.com", ClientID: "c"}
	tests := []struct {
		name      string
		cfg       AuthConfig
		wantOpen  bool
		wantRules int
		wantForm  bool // LegacyForm
	}{
		{"nothing configured (existing test fixtures)", AuthConfig{}, true, 1, true},
		{"legacy github, empty allowlist", AuthConfig{GitHub: GitHubConfig{ClientID: "x"}}, true, 1, true},
		{"legacy github, users allowlist", AuthConfig{GitHub: GitHubConfig{ClientID: "x"}, Allowlist: AllowlistConfig{Users: []string{"a"}}}, false, 1, true},
		{"legacy github, empty access block", AuthConfig{GitHub: GitHubConfig{ClientID: "x"}, Access: &AccessConfig{}}, false, 0, false},
		{"legacy github plus providers", AuthConfig{GitHub: GitHubConfig{ClientID: "x"}, Providers: []ProviderConfig{oidc}}, false, 0, false},
		{"providers only", AuthConfig{Providers: []ProviderConfig{oidc}}, false, 0, false},
		// Malformed legacy entries never matched and still don't; the allowlist is not empty.
		{"legacy empty user entry", AuthConfig{Allowlist: AllowlistConfig{Users: []string{""}}}, false, 0, true},
		{"legacy team without slug", AuthConfig{Allowlist: AllowlistConfig{Teams: []OrgTeam{{Org: "acme"}}}}, false, 0, true},
		{"legacy org/team under orgs", AuthConfig{Allowlist: AllowlistConfig{Orgs: []string{"acme/devs"}}}, false, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := tt.cfg.Resolve()
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if res.LegacyOpen != tt.wantOpen || len(res.Rules) != tt.wantRules {
				t.Fatalf("LegacyOpen = %v, rules = %+v; want %v and %d rules", res.LegacyOpen, res.Rules, tt.wantOpen, tt.wantRules)
			}
			if res.LegacyForm != tt.wantForm {
				t.Errorf("LegacyForm = %v, want %v", res.LegacyForm, tt.wantForm)
			}
			if tt.wantOpen && (!res.Rules[0].Anyone || res.Rules[0].Provider != "github") {
				t.Errorf("legacy open rule = %+v, want anyone on github", res.Rules[0])
			}
		})
	}
}

// An access: key that is present but null or empty is the new form: it must not fall back to
// the legacy empty-allowlist-admits-everyone rule.
func TestResolveAccessKeyPresence(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		wantOpen bool
	}{
		{"absent", "auth:\n  github: {clientId: x}\n", true},
		{"bare key", "auth:\n  github: {clientId: x}\n  access:\n  # rules: commented out\n", false},
		{"null", "auth:\n  github: {clientId: x}\n  access: null\n", false},
		{"empty map", "auth:\n  github: {clientId: x}\n  access: {}\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := authFromYAML(t, tt.yaml).Resolve()
			if err != nil {
				t.Fatal(err)
			}
			if res.LegacyOpen != tt.wantOpen {
				t.Fatalf("LegacyOpen = %v, want %v (rules %+v)", res.LegacyOpen, tt.wantOpen, res.Rules)
			}
		})
	}
}

func TestResolveNewFormYAML(t *testing.T) {
	a := authFromYAML(t, `
auth:
  enabled: true
  github: {clientId: ghid, clientSecret: ghsec}
  providers:
    - name: corp
      type: oidc
      displayName: Corp SSO
      issuerURL: https://login.example.com/realms/corp
      clientId: corp-client
      clientSecret: corp-secret
      scopes: [profile, email, groups]
      claims: {groups: realm_access.roles}
      userInfo: true
      authParams: {prompt: select_account}
      allowedJWKSHosts: [Keys.Example.com]
    - name: ghe
      type: github
      baseURL: https://ghe.example.com/
      apiURL: https://ghe.example.com/api/v3
  access:
    identityMaxAge: 168h
    rules:
      - provider: github
        users: [spirilis]
        roles: [admin]
      - provider: corp
        emailDomains: [example.com]
        groups: [mcp-admins]
        roles: [admin, writer]
      - provider: ghe
        teams: [acme/devs]
`)
	res, err := a.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.IdentityMaxAge != 168*time.Hour {
		t.Errorf("IdentityMaxAge = %v", res.IdentityMaxAge)
	}
	if names := []string{res.Providers[0].Name, res.Providers[1].Name, res.Providers[2].Name}; !reflect.DeepEqual(names, []string{"github", "corp", "ghe"}) {
		t.Errorf("providers = %v", names)
	}
	corp, _ := res.Provider("corp")
	if !reflect.DeepEqual(corp.Scopes, []string{"openid", "profile", "email", "groups"}) {
		t.Errorf("scopes = %v, want openid forced in front", corp.Scopes)
	}
	if corp.TokenEndpointAuthMethod != "client_secret_basic" {
		t.Errorf("auth method = %q, want client_secret_basic for a client with a secret", corp.TokenEndpointAuthMethod)
	}
	wantClaims := ClaimsConfig{Subject: "sub", Username: "preferred_username", Email: "email", Name: "name", Groups: "realm_access.roles"}
	if corp.Claims != wantClaims {
		t.Errorf("claims = %+v, want %+v", corp.Claims, wantClaims)
	}
	if !reflect.DeepEqual(corp.AllowedAlgorithms, []string{"RS256", "ES256"}) {
		t.Errorf("algorithms = %v", corp.AllowedAlgorithms)
	}
	if corp.RequireVerifiedEmail == nil || !*corp.RequireVerifiedEmail {
		t.Error("requireVerifiedEmail should default to true")
	}
	if !reflect.DeepEqual(corp.AllowedJWKSHosts, []string{"keys.example.com"}) {
		t.Errorf("jwks hosts = %v", corp.AllowedJWKSHosts)
	}
	ghe, _ := res.Provider("ghe")
	// GitHub Enterprise Server's API lives under /api/v3 of its own host.
	if ghe.BaseURL != "https://ghe.example.com" || ghe.APIURL != "https://ghe.example.com/api/v3" || ghe.DisplayName != "GitHub" {
		t.Errorf("ghe = %+v", ghe)
	}
	if len(res.Rules) != 3 || res.Rules[1].Where != "auth.access.rules[1]" {
		t.Errorf("rules = %+v", res.Rules)
	}
}

func TestResolvePublicOIDCClientDefaults(t *testing.T) {
	cfg := AuthConfig{Providers: []ProviderConfig{{Name: "corp", Type: "oidc", IssuerURL: "http://localhost:8081", ClientID: "c"}}}
	res, err := cfg.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	p := res.Providers[0]
	if p.TokenEndpointAuthMethod != "none" || p.DisplayName != "corp" ||
		!reflect.DeepEqual(p.Scopes, []string{"openid", "profile", "email"}) {
		t.Errorf("provider = %+v", p)
	}
	if cfg.Providers[0].Scopes != nil || cfg.Providers[0].TokenEndpointAuthMethod != "" {
		t.Error("Resolve modified the caller's config")
	}
}

func TestResolveRejects(t *testing.T) {
	oidc := func(mut func(*ProviderConfig)) []ProviderConfig {
		p := ProviderConfig{Name: "corp", Type: ProviderTypeOIDC, IssuerURL: "https://login.example.com", ClientID: "c"}
		if mut != nil {
			mut(&p)
		}
		return []ProviderConfig{p}
	}
	rules := func(r ...AccessRule) *AccessConfig { return &AccessConfig{Rules: r} }
	no := false

	tests := []struct {
		name    string
		cfg     AuthConfig
		wantErr string
	}{
		{"unknown provider in a rule", AuthConfig{Providers: oidc(nil), Access: rules(AccessRule{Provider: "nope", Anyone: true})}, `unknown provider "nope"`},
		{"empty rule", AuthConfig{Providers: oidc(nil), Access: rules(AccessRule{Provider: "corp", Roles: []string{"admin"}})}, "matches nobody"},
		{"empty matcher entry", AuthConfig{Providers: oidc(nil), Access: rules(AccessRule{Provider: "corp", Users: []string{" "}})}, "users: empty entry"},
		{"anyone with matchers", AuthConfig{Providers: oidc(nil), Access: rules(AccessRule{Provider: "corp", Anyone: true, Users: []string{"a"}})}, "cannot be combined"},
		{"orgs on an oidc rule", AuthConfig{Providers: oidc(nil), Access: rules(AccessRule{Provider: "corp", Orgs: []string{"acme"}})}, "orgs and teams are for GitHub"},
		{"teams on an oidc rule", AuthConfig{Providers: oidc(nil), Access: rules(AccessRule{Provider: "corp", Teams: []string{"acme/devs"}})}, "orgs and teams are for GitHub"},
		{"team without slug", AuthConfig{GitHub: GitHubConfig{ClientID: "x"}, Access: rules(AccessRule{Provider: "github", Teams: []string{"acme"}})}, "org/team-slug"},
		{"email without @", AuthConfig{Providers: oidc(nil), Access: rules(AccessRule{Provider: "corp", Emails: []string{"bob"}})}, "not an email"},
		{"duplicate provider name", AuthConfig{Providers: append(oidc(nil), oidc(nil)...)}, "duplicate provider name"},
		{"legacy github and providers github", AuthConfig{GitHub: GitHubConfig{ClientID: "x"},
			Providers: []ProviderConfig{{Name: "github", Type: "github"}}}, "auth.github already configures"},
		{"bad name", AuthConfig{Providers: oidc(func(p *ProviderConfig) { p.Name = "Corp" })}, "must match"},
		{"name too long", AuthConfig{Providers: oidc(func(p *ProviderConfig) { p.Name = "a" + strings.Repeat("b", 32) })}, "must match"},
		{"unknown type", AuthConfig{Providers: oidc(func(p *ProviderConfig) { p.Type = "saml" })}, `type "saml"`},
		{"oidc without issuerURL", AuthConfig{Providers: oidc(func(p *ProviderConfig) { p.IssuerURL = "" })}, "issuerURL is required"},
		{"oidc without clientId", AuthConfig{Providers: oidc(func(p *ProviderConfig) { p.ClientID = "" })}, "clientId (or clientIdFile) is required"},
		{"http issuer", AuthConfig{Providers: oidc(func(p *ProviderConfig) { p.IssuerURL = "http://login.example.com" })}, "must use https"},
		{"issuer with query", AuthConfig{Providers: oidc(func(p *ProviderConfig) { p.IssuerURL = "https://login.example.com?x=1" })}, "no query"},
		{"http GitHub Enterprise", AuthConfig{Providers: []ProviderConfig{{Name: "ghe", Type: "github", BaseURL: "http://ghe.example.com"}}}, "must use https"},
		{"issuerURL on github", AuthConfig{Providers: []ProviderConfig{{Name: "ghe", Type: "github", IssuerURL: "https://x"}}}, "issuerURL is for type oidc"},
		{"reserved authParam nonce", AuthConfig{Providers: oidc(func(p *ProviderConfig) { p.AuthParams = map[string]string{"nonce": "x"} })}, `"nonce" is set by the library`},
		{"reserved authParam code_challenge*", AuthConfig{Providers: oidc(func(p *ProviderConfig) { p.AuthParams = map[string]string{"code_challenge_method": "plain"} })}, "set by the library"},
		{"reserved authParam redirect_uri", AuthConfig{Providers: oidc(func(p *ProviderConfig) { p.AuthParams = map[string]string{"Redirect_URI": "x"} })}, "set by the library"},
		{"alg none", AuthConfig{Providers: oidc(func(p *ProviderConfig) { p.AllowedAlgorithms = []string{"none"} })}, "never accepted"},
		{"alg HS256", AuthConfig{Providers: oidc(func(p *ProviderConfig) { p.AllowedAlgorithms = []string{"HS256"} })}, "never accepted"},
		{"basic auth without secret", AuthConfig{Providers: oidc(func(p *ProviderConfig) { p.TokenEndpointAuthMethod = "client_secret_basic" })}, "needs clientSecret"},
		{"none with secret", AuthConfig{Providers: oidc(func(p *ProviderConfig) {
			p.TokenEndpointAuthMethod = "none"
			p.ClientSecret = "s"
		})}, "public client"},
		{"bad auth method", AuthConfig{Providers: oidc(func(p *ProviderConfig) { p.TokenEndpointAuthMethod = "private_key_jwt" })}, "must be client_secret_basic"},
		{"configurable subject", AuthConfig{Providers: oidc(func(p *ProviderConfig) { p.Claims.Subject = "email" })}, "always keyed by sub"},
		{"jwks host with scheme", AuthConfig{Providers: oidc(func(p *ProviderConfig) { p.AllowedJWKSHosts = []string{"https://keys"} })}, "bare hostname"},
		{"negative identityMaxAge", AuthConfig{Providers: oidc(nil), Access: &AccessConfig{IdentityMaxAge: -time.Second}}, "must not be negative"},
		{"allowlist without a github provider", AuthConfig{Providers: oidc(nil), Allowlist: AllowlistConfig{Users: []string{"a"}}}, "auth.allowlist applies to the provider"},
		// An OIDC username claim may be chosen by the user, so users rules need an opt-in.
		{"users on an oidc rule", AuthConfig{Providers: oidc(nil), Access: rules(AccessRule{Provider: "corp", Users: []string{"alice"}})}, "claims.usernameIsUnique"},
		// GitHub does not say whether its profile email is verified.
		{"emails on a github rule", AuthConfig{GitHub: GitHubConfig{ClientID: "x"}, Access: rules(AccessRule{Provider: "github", Emails: []string{"a@example.com"}})}, "GitHub providers do not supply"},
		{"emailDomains on a github rule", AuthConfig{GitHub: GitHubConfig{ClientID: "x"}, Access: rules(AccessRule{Provider: "github", EmailDomains: []string{"example.com"}})}, "GitHub providers do not supply"},
		{"requireVerifiedEmail on github", AuthConfig{Providers: []ProviderConfig{{Name: "ghe", Type: "github", RequireVerifiedEmail: &no}}}, "requireVerifiedEmail is for type oidc"},
		{"claims on github", AuthConfig{Providers: []ProviderConfig{{Name: "ghe", Type: "github", Claims: ClaimsConfig{Email: "x"}}}}, "claims is for type oidc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.cfg.Resolve()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// users rules on an OIDC provider work once the operator vouches for the username claim.
func TestResolveOIDCUsersWithUniqueUsername(t *testing.T) {
	cfg := AuthConfig{
		Providers: []ProviderConfig{{Name: "corp", Type: ProviderTypeOIDC, IssuerURL: "https://login.example.com", ClientID: "c",
			Claims: ClaimsConfig{Username: "upn", UsernameIsUnique: true}}},
		Access: &AccessConfig{Rules: []AccessRule{{Provider: "corp", Users: []string{"alice@example.com"}}}},
	}
	if _, err := cfg.Resolve(); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

// An embedder that decodes AuthConfig into its own struct with KnownFields(true) still has
// typos under auth: refused; a misspelt allowlist would otherwise leave the legacy form with an
// empty allowlist, which admits every GitHub user.
func TestAuthConfigKeepsStrictDecoding(t *testing.T) {
	type embedder struct {
		OAuth AuthConfig `yaml:"oauth"`
	}
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"typo under auth", "oauth: {enabled: true, allowlst: {users: [x]}}\n", "field allowlst not found"},
		{"typo under access", "oauth: {access: {rulez: []}}\n", "field rulez not found"},
		{"valid", "oauth: {enabled: true, allowlist: {users: [x]}, access: {rules: []}}\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := yaml.NewDecoder(strings.NewReader(tt.yaml))
			dec.KnownFields(true)
			var e embedder
			err := dec.Decode(&e)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Decode: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadEmptyDocument(t *testing.T) {
	for _, in := range []string{"", "# only a comment\n"} {
		cfg, err := LoadFromString(in)
		if err != nil || cfg.Server.Mode != "stdio" || cfg.Auth != nil {
			t.Errorf("LoadFromString(%q) = %+v, %v", in, cfg, err)
		}
	}
}

func TestResolveSecretFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	idFile := write("id", "  file-id\n")
	secretFile := write("secret", "file-secret\n")
	emptyFile := write("empty", "\n")
	missing := filepath.Join(dir, "missing")

	tests := []struct {
		name       string
		gh         GitHubConfig
		wantID     string
		wantSecret string
		wantErr    string
	}{
		{"files win over inline values", GitHubConfig{ClientID: "inline", ClientSecret: "inline", ClientIDFile: idFile, ClientSecretFile: secretFile}, "file-id", "file-secret", ""},
		{"missing id file", GitHubConfig{ClientIDFile: missing}, "", "", "auth.github.clientIdFile"},
		{"missing secret file", GitHubConfig{ClientID: "x", ClientSecretFile: missing}, "", "", "auth.github.clientSecretFile"},
		{"empty secret file", GitHubConfig{ClientID: "x", ClientSecretFile: emptyFile}, "", "", "is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := (&AuthConfig{GitHub: tt.gh}).Resolve()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			gh, _ := res.Provider("github")
			if gh.ClientID != tt.wantID || gh.ClientSecret != tt.wantSecret {
				t.Errorf("id, secret = %q, %q", gh.ClientID, gh.ClientSecret)
			}
		})
	}

	// The same check applies to providers entries.
	cfg := AuthConfig{Providers: []ProviderConfig{{Name: "corp", Type: "oidc", IssuerURL: "https://l.example.com",
		ClientID: "c", ClientSecretFile: missing}}}
	if _, err := cfg.Resolve(); err == nil || !strings.Contains(err.Error(), "auth.providers[0] (corp).clientSecretFile") {
		t.Errorf("err = %v", err)
	}
}

// apiURL defaults to api.github.com for github.com, and to <baseURL>/api/v3 for an Enterprise
// Server, whose API is not on api.github.com.
func TestResolveGitHubAPIURLDefault(t *testing.T) {
	tests := []struct{ baseURL, apiURL, want string }{
		{"", "", "https://api.github.com"},
		{"https://github.com/", "", "https://api.github.com"},
		{"https://ghe.example.com/", "", "https://ghe.example.com/api/v3"},
		{"https://ghe.example.com", "https://api.ghe.example.com/", "https://api.ghe.example.com"},
	}
	for _, tt := range tests {
		a := AuthConfig{Providers: []ProviderConfig{{Name: "gh", Type: "github", BaseURL: tt.baseURL, APIURL: tt.apiURL}},
			Access: &AccessConfig{Rules: []AccessRule{{Provider: "gh", Anyone: true}}}}
		res, err := a.Resolve()
		if err != nil {
			t.Fatalf("%q: %v", tt.baseURL, err)
		}
		if got := res.Providers[0].APIURL; got != tt.want {
			t.Errorf("baseURL %q apiURL %q: resolved apiURL %q, want %q", tt.baseURL, tt.apiURL, got, tt.want)
		}
	}
}
