package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Provider types.
const (
	ProviderTypeGitHub = "github"
	ProviderTypeOIDC   = "oidc"
)

// LegacyGitHubProvider is the name the legacy auth.github block is given as a provider. It is
// also the prefix of GitHub principal ids ("github:<login>"), which embedders already store.
const LegacyGitHubProvider = "github"

// ProviderConfig is one identity provider users can sign in with.
type ProviderConfig struct {
	Name        string `yaml:"name"`                  // [a-z][a-z0-9-]{0,31}; the prefix of principal ids
	Type        string `yaml:"type"`                  // "github" or "oidc"
	DisplayName string `yaml:"displayName,omitempty"` // shown on the sign-in chooser

	ClientID         string `yaml:"clientId,omitempty"`
	ClientSecret     string `yaml:"clientSecret,omitempty"`
	ClientIDFile     string `yaml:"clientIdFile,omitempty"`     // mounted secret; wins over ClientID
	ClientSecretFile string `yaml:"clientSecretFile,omitempty"` // mounted secret; wins over ClientSecret

	// GitHub only. Set both for GitHub Enterprise Server.
	BaseURL string `yaml:"baseURL,omitempty"` // default https://github.com
	APIURL  string `yaml:"apiURL,omitempty"`  // default https://api.github.com, or <baseURL>/api/v3

	// OIDC only. Discovery reads <IssuerURL>/.well-known/openid-configuration. https, or http on
	// a loopback host for a local IdP in development (only then may its endpoints be http).
	IssuerURL               string            `yaml:"issuerURL,omitempty"`
	TokenEndpointAuthMethod string            `yaml:"tokenEndpointAuthMethod,omitempty"` // client_secret_basic|client_secret_post|none
	Scopes                  []string          `yaml:"scopes,omitempty"`                  // default openid, profile, email; openid is forced
	Claims                  ClaimsConfig      `yaml:"claims,omitempty"`
	UserInfo                bool              `yaml:"userInfo,omitempty"`   // also merge the userinfo endpoint's claims
	AuthParams              map[string]string `yaml:"authParams,omitempty"` // extra /authorize parameters
	AllowedAlgorithms       []string          `yaml:"allowedAlgorithms,omitempty"`
	CAFile                  string            `yaml:"caFile,omitempty"`   // PEM bundle for a private CA
	Audience                string            `yaml:"audience,omitempty"` // accepted aud besides the client id

	// AllowedJWKSHosts are hosts other than the issuer's that jwks_uri, and userinfo_endpoint
	// when UserInfo is on, may point at. Some IdPs serve them from a separate host; anything
	// else is refused so a tampered discovery document cannot redirect key fetching or supply
	// the claims that rules admit on.
	AllowedJWKSHosts []string `yaml:"allowedJWKSHosts,omitempty"`

	// RequireVerifiedEmail (OIDC only; default true) makes emails and emailDomains rules match
	// only an email the IdP marks email_verified. It is a provider setting because whether that claim
	// can be trusted, or is sent at all, is a property of the IdP. Turn it off only for an IdP
	// that never sends the claim and only ever issues addresses it owns.
	RequireVerifiedEmail *bool `yaml:"requireVerifiedEmail,omitempty"`
}

// ClaimsConfig names where an OIDC identity's attributes are found: dotted paths into the
// merged id_token (and userinfo) claims, such as "realm_access.roles".
type ClaimsConfig struct {
	// Subject is always "sub" and is accepted only so that writing it is not silently ignored.
	// Users are keyed by (provider, issuer, sub); a configurable key would let a mutable claim
	// such as email take over another user's record.
	Subject  string `yaml:"subject,omitempty"`
	Username string `yaml:"username,omitempty"` // default preferred_username
	Name     string `yaml:"name,omitempty"`     // default name
	Groups   string `yaml:"groups,omitempty"`   // default groups; a string or an array of strings

	// Email defaults to email. The standard email_verified claim describes only the standard
	// email claim, so with any other path the email counts as unverified, and emails and
	// emailDomains rules match it only with requireVerifiedEmail: false.
	Email string `yaml:"email,omitempty"`

	// UsernameIsUnique lets users rules match this provider's username claim. Leave it off
	// unless the IdP guarantees the claim is unique and that users cannot choose or change it:
	// OIDC does not (preferred_username may be any string), and a users rule would then admit
	// whoever takes a listed name.
	UsernameIsUnique bool `yaml:"usernameIsUnique,omitempty"`
}

// AccessConfig says who is admitted and with which roles.
type AccessConfig struct {
	// Rules: a user is admitted when at least one rule matches, and holds the union of the
	// matching rules' roles.
	Rules []AccessRule `yaml:"rules,omitempty"`

	// IdentityMaxAge bounds how long after their last browser login a user may keep
	// refreshing tokens, so group changes at the IdP take effect. 0 means the refresh token
	// lifetime.
	IdentityMaxAge time.Duration `yaml:"identityMaxAge,omitempty"`
}

// AccessRule matches users of one provider. It matches when the user matches any one of its
// matchers (the legacy allowlist's users, orgs and teams were alternatives too).
type AccessRule struct {
	Provider string `yaml:"provider"`

	Users        []string `yaml:"users,omitempty"`        // GitHub login (OIDC: only with claims.usernameIsUnique), case-insensitive
	Orgs         []string `yaml:"orgs,omitempty"`         // GitHub only: the group "org"
	Teams        []string `yaml:"teams,omitempty"`        // GitHub only: the group "org/team-slug"
	Subjects     []string `yaml:"subjects,omitempty"`     // exact sub (GitHub: numeric user id)
	Emails       []string `yaml:"emails,omitempty"`       // case-insensitive; verified emails only
	EmailDomains []string `yaml:"emailDomains,omitempty"` // exact domain after the @; verified emails only
	Groups       []string `yaml:"groups,omitempty"`       // the groups claim (GitHub: orgs and org/team)

	// Anyone matches every user the provider authenticates. It cannot be combined with
	// other matchers, and is logged as a warning at startup.
	Anyone bool `yaml:"anyone,omitempty"`

	Roles []string `yaml:"roles,omitempty"` // must be roles the application declared
}

// ResolvedAuth is the provider and rule set an AuthConfig stands for, legacy form included.
type ResolvedAuth struct {
	// Providers have their defaults applied and their *File secrets read.
	Providers []ProviderConfig

	// Rules in order: the legacy allowlist's rule (if any) first, then auth.access.rules.
	Rules []ResolvedRule

	IdentityMaxAge time.Duration

	// LegacyOpen is set for the pure legacy form (auth.github and auth.allowlist only) with an
	// empty allowlist, which admits every GitHub user, as it always has. Rules then holds one
	// Anyone rule for the github provider.
	LegacyOpen bool

	// LegacyForm is set for the pure legacy form (auth.github and auth.allowlist only, no
	// providers and no access block). Behaviour that v0.11 tied to "admitted at all", such as
	// the admin API, stays that way only for it: the new form has roles to separate the two.
	LegacyForm bool
}

// ResolvedRule is an AccessRule with where it came from, for error and warning messages.
type ResolvedRule struct {
	AccessRule
	Where string // e.g. "auth.access.rules[2]" or "auth.allowlist"
}

// Provider returns the resolved provider with the given name.
func (r *ResolvedAuth) Provider(name string) (ProviderConfig, bool) {
	for _, p := range r.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return ProviderConfig{}, false
}

// markEmptyAccess records an `access:` key that is present but empty or null (everything under
// it commented out, or a Helm template rendering null) as an empty AccessConfig. yaml.v3 leaves
// the pointer nil, which reads as the pure legacy form, and with an empty allowlist that admits
// every GitHub user: a config meant to tighten access would open it instead.
//
// It works on the parsed document rather than as an AuthConfig.UnmarshalYAML hook, because a
// hook has to decode the rest of the block with a fresh decoder, which drops the caller's
// KnownFields(true): embedders that decode AuthConfig strictly would stop catching typos under
// auth:, such as a misspelt allowlist, which would then admit everyone.
func markEmptyAccess(doc *yaml.Node, cfg *Config) {
	if cfg.Auth == nil || cfg.Auth.Access != nil {
		return
	}
	auth := mappingValue(doc, "auth")
	if auth != nil && mappingValue(auth, "access") != nil {
		cfg.Auth.Access = &AccessConfig{}
	}
}

// mappingValue returns the value of key in a mapping node (or a document holding one).
func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

var providerNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// reservedAuthParams are the /authorize parameters the library sets itself. Letting config
// override them would break, or quietly weaken, the flow (a fixed state or nonce, say).
var reservedAuthParams = map[string]bool{
	"client_id": true, "redirect_uri": true, "state": true, "nonce": true,
	"code_challenge": true, "code_challenge_method": true, "response_type": true, "scope": true,
}

// supportedAlgorithms are the id_token signature algorithms the library verifies.
var supportedAlgorithms = map[string]bool{"RS256": true, "ES256": true}

// Resolve validates the auth configuration and returns the providers and rules it stands for,
// mapping the legacy auth.github and auth.allowlist blocks onto a provider named "github" and
// one rule. It reads *File secrets, and fails when one is configured but cannot be read: a
// server that starts with an empty client secret only fails later, at a user's login, with an
// error that names neither the file nor the cause. It does not modify the receiver.
//
// Role names are not checked here; only the application knows them (auth.WithRoles).
func (a *AuthConfig) Resolve() (*ResolvedAuth, error) {
	legacyGitHub := a.GitHub != (GitHubConfig{})
	pureLegacy := len(a.Providers) == 0 && a.Access == nil
	res := &ResolvedAuth{LegacyForm: pureLegacy}

	// Pure legacy keeps a github provider even with no credentials: that always started, and
	// tests and stdio-only embedders rely on it.
	if legacyGitHub || pureLegacy {
		p := ProviderConfig{
			Name:             LegacyGitHubProvider,
			Type:             ProviderTypeGitHub,
			ClientID:         a.GitHub.ClientID,
			ClientSecret:     a.GitHub.ClientSecret,
			ClientIDFile:     a.GitHub.ClientIDFile,
			ClientSecretFile: a.GitHub.ClientSecretFile,
		}
		if err := resolveProvider(&p, "auth.github"); err != nil {
			return nil, err
		}
		res.Providers = append(res.Providers, p)
	}
	for i, p := range a.Providers {
		where := fmt.Sprintf("auth.providers[%d]", i)
		if p.Name != "" {
			where += " (" + p.Name + ")"
		}
		p.Scopes = append([]string(nil), p.Scopes...) // resolveProvider may rewrite them
		if err := resolveProvider(&p, where); err != nil {
			return nil, err
		}
		if _, dup := res.Provider(p.Name); dup {
			if p.Name == LegacyGitHubProvider && legacyGitHub {
				return nil, fmt.Errorf("%s: auth.github already configures the provider %q; "+
					"move it into auth.providers or rename this one", where, p.Name)
			}
			return nil, fmt.Errorf("%s: duplicate provider name %q", where, p.Name)
		}
		res.Providers = append(res.Providers, p)
	}

	al := a.Allowlist
	switch {
	case len(al.Users) > 0 || len(al.Orgs) > 0 || len(al.Teams) > 0:
		// Malformed legacy entries (an empty user, a team missing its org or slug, easy to get
		// from a templated chart) always started and never matched. They still never match,
		// rather than now refusing to start; and they are dropped rather than passed on, since
		// orgs: ["acme/devs"] would otherwise start matching the team. Open versus closed is
		// still decided on the entries as written.
		rule := AccessRule{Provider: LegacyGitHubProvider}
		for _, u := range al.Users {
			if strings.TrimSpace(u) != "" {
				rule.Users = append(rule.Users, u)
			}
		}
		for _, o := range al.Orgs {
			if strings.TrimSpace(o) != "" && !strings.Contains(o, "/") {
				rule.Orgs = append(rule.Orgs, o)
			}
		}
		for _, t := range al.Teams {
			if team := t.Org + "/" + t.Team; checkMatcher("teams", team) == nil {
				rule.Teams = append(rule.Teams, team)
			}
		}
		if len(rule.Users)+len(rule.Orgs)+len(rule.Teams) > 0 {
			res.Rules = append(res.Rules, ResolvedRule{AccessRule: rule, Where: "auth.allowlist"})
		}
	case pureLegacy:
		res.LegacyOpen = true
		res.Rules = append(res.Rules, ResolvedRule{
			AccessRule: AccessRule{Provider: LegacyGitHubProvider, Anyone: true},
			Where:      "auth.allowlist",
		})
	}
	if a.Access != nil {
		if a.Access.IdentityMaxAge < 0 {
			return nil, fmt.Errorf("auth.access.identityMaxAge: must not be negative")
		}
		res.IdentityMaxAge = a.Access.IdentityMaxAge
		for i, r := range a.Access.Rules {
			res.Rules = append(res.Rules, ResolvedRule{AccessRule: r, Where: fmt.Sprintf("auth.access.rules[%d]", i)})
		}
	}
	for _, r := range res.Rules {
		if err := res.validateRule(r); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// resolveProvider validates p in place, applies its defaults and reads its secret files.
func resolveProvider(p *ProviderConfig, where string) error {
	if !providerNameRE.MatchString(p.Name) {
		return fmt.Errorf("%s: name %q must match [a-z][a-z0-9-]{0,31}", where, p.Name)
	}
	var err error
	if p.ClientIDFile != "" {
		if p.ClientID, err = ReadSecretFile(p.ClientIDFile); err != nil {
			return fmt.Errorf("%s.clientIdFile: %w", where, err)
		}
	}
	if p.ClientSecretFile != "" {
		if p.ClientSecret, err = ReadSecretFile(p.ClientSecretFile); err != nil {
			return fmt.Errorf("%s.clientSecretFile: %w", where, err)
		}
	}

	switch p.Type {
	case ProviderTypeGitHub:
		if p.IssuerURL != "" {
			return fmt.Errorf("%s: issuerURL is for type oidc; GitHub Enterprise uses baseURL and apiURL", where)
		}
		if p.RequireVerifiedEmail != nil {
			// GitHub's profile email is whatever the user chose to show; with this off, email
			// rules would admit on it.
			return fmt.Errorf("%s: requireVerifiedEmail is for type oidc; GitHub users never match email rules", where)
		}
		if p.Claims != (ClaimsConfig{}) {
			return fmt.Errorf("%s: claims is for type oidc", where)
		}
		if p.DisplayName == "" {
			p.DisplayName = "GitHub"
		}
		if p.BaseURL == "" {
			p.BaseURL = "https://github.com"
		}
		p.BaseURL = strings.TrimSuffix(p.BaseURL, "/")
		if p.APIURL == "" {
			// GitHub Enterprise Server serves its REST API under /api/v3 of its own host; only
			// github.com has a separate API host.
			p.APIURL = "https://api.github.com"
			if p.BaseURL != "https://github.com" {
				p.APIURL = p.BaseURL + "/api/v3"
			}
		}
		if err := checkHTTPS(p.BaseURL); err != nil {
			return fmt.Errorf("%s.baseURL: %w", where, err)
		}
		if err := checkHTTPS(p.APIURL); err != nil {
			return fmt.Errorf("%s.apiURL: %w", where, err)
		}
		p.BaseURL = strings.TrimSuffix(p.BaseURL, "/")
		p.APIURL = strings.TrimSuffix(p.APIURL, "/")
		return nil
	case ProviderTypeOIDC:
		return resolveOIDC(p, where)
	default:
		return fmt.Errorf("%s: type %q must be %q or %q", where, p.Type, ProviderTypeGitHub, ProviderTypeOIDC)
	}
}

func resolveOIDC(p *ProviderConfig, where string) error {
	if p.BaseURL != "" || p.APIURL != "" {
		return fmt.Errorf("%s: baseURL and apiURL are for type github; OIDC uses issuerURL", where)
	}
	if p.IssuerURL == "" {
		return fmt.Errorf("%s: issuerURL is required for type oidc", where)
	}
	// Not normalized: the id_token's iss must equal it exactly, trailing slash included.
	if err := checkHTTPS(p.IssuerURL); err != nil {
		return fmt.Errorf("%s.issuerURL: %w", where, err)
	}
	if p.ClientID == "" {
		return fmt.Errorf("%s: clientId (or clientIdFile) is required for type oidc", where)
	}
	if p.DisplayName == "" {
		p.DisplayName = p.Name
	}

	switch p.TokenEndpointAuthMethod {
	case "":
		p.TokenEndpointAuthMethod = "none"
		if p.ClientSecret != "" {
			p.TokenEndpointAuthMethod = "client_secret_basic"
		}
	case "client_secret_basic", "client_secret_post":
		if p.ClientSecret == "" {
			return fmt.Errorf("%s: tokenEndpointAuthMethod %s needs clientSecret (or clientSecretFile)",
				where, p.TokenEndpointAuthMethod)
		}
	case "none":
		if p.ClientSecret != "" {
			return fmt.Errorf("%s: tokenEndpointAuthMethod none is a public client; remove clientSecret", where)
		}
	default:
		return fmt.Errorf("%s: tokenEndpointAuthMethod %q must be client_secret_basic, client_secret_post or none",
			where, p.TokenEndpointAuthMethod)
	}

	if len(p.Scopes) == 0 {
		p.Scopes = []string{"openid", "profile", "email"}
	}
	hasOpenID := false
	for _, s := range p.Scopes {
		if s == "" || strings.ContainsAny(s, " \t") {
			return fmt.Errorf("%s.scopes: %q is not a single scope", where, s)
		}
		hasOpenID = hasOpenID || s == "openid"
	}
	if !hasOpenID {
		p.Scopes = append([]string{"openid"}, p.Scopes...)
	}

	if p.Claims.Subject != "" && p.Claims.Subject != "sub" {
		return fmt.Errorf("%s.claims.subject: users are always keyed by sub; %q cannot be used", where, p.Claims.Subject)
	}
	p.Claims.Subject = "sub"
	if p.Claims.Username == "" {
		p.Claims.Username = "preferred_username"
	}
	if p.Claims.Email == "" {
		p.Claims.Email = "email"
	}
	if p.Claims.Name == "" {
		p.Claims.Name = "name"
	}
	if p.Claims.Groups == "" {
		p.Claims.Groups = "groups"
	}

	for k := range p.AuthParams {
		if reservedAuthParams[strings.ToLower(k)] || strings.HasPrefix(strings.ToLower(k), "code_challenge") {
			return fmt.Errorf("%s.authParams: %q is set by the library and cannot be overridden", where, k)
		}
	}

	if len(p.AllowedAlgorithms) == 0 {
		p.AllowedAlgorithms = []string{"RS256", "ES256"}
	}
	for _, alg := range p.AllowedAlgorithms {
		if !supportedAlgorithms[alg] {
			return fmt.Errorf("%s.allowedAlgorithms: %q is not supported (RS256 and ES256 are; "+
				"none and the HS* algorithms are never accepted)", where, alg)
		}
	}

	hosts := make([]string, 0, len(p.AllowedJWKSHosts))
	for _, h := range p.AllowedJWKSHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || strings.ContainsAny(h, "/:?#@") {
			return fmt.Errorf("%s.allowedJWKSHosts: %q must be a bare hostname", where, h)
		}
		hosts = append(hosts, h)
	}
	p.AllowedJWKSHosts = hosts
	if p.RequireVerifiedEmail == nil {
		yes := true
		p.RequireVerifiedEmail = &yes
	}
	return nil
}

// checkHTTPS accepts an absolute https URL, or http for a loopback host (local development and
// tests). Anything else would send client secrets and codes in the clear.
func checkHTTPS(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q must be an absolute URL with no query or fragment", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if h == "localhost" || (net.ParseIP(h) != nil && net.ParseIP(h).IsLoopback()) {
			return nil
		}
	}
	return fmt.Errorf("%q must use https (http is accepted only for localhost)", raw)
}

// validateRule checks one rule against the resolved providers.
func (res *ResolvedAuth) validateRule(r ResolvedRule) error {
	p, ok := res.Provider(r.Provider)
	if !ok {
		if r.Where == "auth.allowlist" {
			return fmt.Errorf("auth.allowlist applies to the provider %q, which is not configured", r.Provider)
		}
		return fmt.Errorf("%s: unknown provider %q", r.Where, r.Provider)
	}
	if p.Type != ProviderTypeGitHub && (len(r.Orgs) > 0 || len(r.Teams) > 0) {
		return fmt.Errorf("%s: orgs and teams are for GitHub providers; use groups for %q", r.Where, r.Provider)
	}

	matchers := 0
	lists := []struct {
		field string
		vals  []string
	}{
		{"users", r.Users}, {"orgs", r.Orgs}, {"teams", r.Teams}, {"subjects", r.Subjects},
		{"emails", r.Emails}, {"emailDomains", r.EmailDomains}, {"groups", r.Groups}, {"roles", r.Roles},
	}
	for _, l := range lists {
		for _, v := range l.vals {
			// An empty entry would match an identity that lacks the attribute.
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("%s.%s: empty entry", r.Where, l.field)
			}
			if l.field != "roles" {
				matchers++
			}
			if err := checkMatcher(l.field, v); err != nil {
				return fmt.Errorf("%s.%s: %w", r.Where, l.field, err)
			}
		}
	}
	switch {
	case r.Anyone && matchers > 0:
		return fmt.Errorf("%s: anyone: true cannot be combined with other matchers", r.Where)
	case !r.Anyone && matchers == 0:
		return fmt.Errorf("%s: rule matches nobody; give users, subjects, emails, emailDomains, groups "+
			"(orgs, teams for GitHub) or anyone: true", r.Where)
	}
	if p.Type == ProviderTypeGitHub && (len(r.Emails) > 0 || len(r.EmailDomains) > 0) {
		// GitHub does not say whether the profile email is verified, so such a rule could
		// never match; refusing it beats a rule that silently admits nobody.
		return fmt.Errorf("%s: emails and emailDomains need a verified email, which GitHub providers do not supply; "+
			"use users, subjects, orgs or teams for %q", r.Where, r.Provider)
	}
	if p.Type == ProviderTypeOIDC && len(r.Users) > 0 && !p.Claims.UsernameIsUnique {
		// OIDC Core 5.7: preferred_username need not be unique, and many IdPs let users choose
		// or change it, so a users rule would admit whoever names themselves after a listed user.
		return fmt.Errorf("%s: users matches the username claim (%s), which %q may let users choose; use subjects, "+
			"emails or groups, or set claims.usernameIsUnique: true on the provider if the IdP guarantees it",
			r.Where, p.Claims.Username, r.Provider)
	}
	return nil
}

func checkMatcher(field, v string) error {
	switch field {
	case "teams":
		org, team, ok := strings.Cut(v, "/")
		if !ok || org == "" || team == "" || strings.Contains(team, "/") {
			return fmt.Errorf("%q must be org/team-slug", v)
		}
	case "orgs":
		if strings.Contains(v, "/") {
			return fmt.Errorf("%q is an org name; put org/team pairs under teams", v)
		}
	case "emails":
		if i := strings.LastIndex(v, "@"); i <= 0 || i == len(v)-1 {
			return fmt.Errorf("%q is not an email address", v)
		}
	case "emailDomains":
		if strings.Contains(strings.TrimPrefix(v, "@"), "@") || strings.TrimPrefix(v, "@") == "" {
			return fmt.Errorf("%q is not a domain", v)
		}
	}
	return nil
}

// ReadSecretFile reads a mounted secret, trimming surrounding whitespace. An empty file is an
// error: it is always a deployment mistake, and would otherwise surface as a confusing
// failure at the identity provider.
func ReadSecretFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}
