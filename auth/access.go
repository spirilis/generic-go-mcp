package auth

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spirilis/generic-go-mcp/config"
	"github.com/spirilis/generic-go-mcp/logging"
)

// Role is an application-defined role that access rules can grant. The application declares
// its roles with WithRoles; a rule naming any other role refuses to start, so a typo in config
// cannot silently grant nothing (or, worse, be mistaken for a working grant).
type Role struct {
	Name        string
	Description string // for documentation and UIs; not interpreted
}

// Option configures NewAuthService.
type Option func(*serviceOptions)

type serviceOptions struct {
	roles     []Role
	adminRole string
}

// WithRoles declares the roles this application understands.
func WithRoles(roles ...Role) Option {
	return func(o *serviceOptions) { o.roles = append(o.roles, roles...) }
}

// WithAdminRole makes the admin API (/admin/clients) available to principals holding role,
// which must be one of the declared roles. Without it the admin API is served only for the
// pure legacy config form, to every admitted user as before v0.12.0. A config in the new form
// can admit far more people (a whole email domain, anyone at an IdP) than may mint or list
// static clients, so it gets no admin API unless the application names who may use it. The
// legacy form's rules grant no roles, so with it this closes the admin API to everyone.
func WithAdminRole(role string) Option {
	return func(o *serviceOptions) { o.adminRole = role }
}

// Identity is what a provider says about a signed-in user, normalized across providers. It is
// snapshotted onto the User record at each browser login, and access rules are evaluated
// against that snapshot on every request.
type Identity struct {
	Provider      string // provider name from config, e.g. "github" or "corp"
	Issuer        string // the OIDC issuer URL, or the GitHub web URL (e.g. https://github.com)
	Subject       string // GitHub numeric user id as a string, or the OIDC sub
	Username      string // GitHub login, or the OIDC username claim
	Email         string
	EmailVerified bool
	Name          string
	Groups        []string // OIDC groups claim; GitHub: "org" and "org/team-slug"
	AvatarURL     string   // GitHub avatar, or the OIDC picture claim
}

// Principal is the signed-in user as an application sees it, with the roles the current rules
// grant. It is in the request context of every request that passed Middleware.
type Principal struct {
	// ID is "<provider>:<sub>" for OIDC, and "<provider>:<login>" for a GitHub provider, the
	// login lowercased: the "github:<login>" form embedders already key data by, though the
	// case may differ from ids they built from User.GitHubLogin. A GitHub login can be renamed
	// and then claimed by another account, which then has the same ID; key data by Provider
	// and Subject (GitHub's numeric user id) where that matters. Never an email or an OIDC
	// username.
	ID       string
	UserID   string // the auth store's user id
	Provider string
	Subject  string
	Username string
	Email    string

	EmailVerified bool
	Name          string
	Groups        []string
	Roles         []string // sorted when Middleware builds it
}

// HasRole reports whether the principal holds role. A nil principal holds none. It does not
// rely on Roles being sorted: embedders build Principals in their own tests.
func (p *Principal) HasRole(role string) bool {
	return p != nil && slices.Contains(p.Roles, role)
}

// PrincipalFromContext returns the principal Middleware put in ctx.
func PrincipalFromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(ContextKeyPrincipal).(*Principal)
	return p, ok && p != nil
}

// HasRole reports whether the request's principal holds role.
func HasRole(ctx context.Context, role string) bool {
	p, _ := PrincipalFromContext(ctx)
	return p.HasRole(role)
}

// Roles returns the roles the application declared, in declaration order.
func (svc *AuthService) Roles() []Role {
	return slices.Clone(svc.roles)
}

// RolesFromContext returns the roles of the request's principal (nil without one). Its shape
// fits mcp.ServerConfig.RolesFromContext.
func (svc *AuthService) RolesFromContext(ctx context.Context) []string {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil
	}
	return slices.Clone(p.Roles)
}

// accessRule is a config.AccessRule compiled for matching: lowercased sets, with GitHub orgs
// and teams folded into groups.
type accessRule struct {
	where    string
	provider string
	anyone   bool

	users, subjects, emails, domains, groups map[string]bool

	// foldGroups compares groups case-insensitively. GitHub org and team names are; an IdP's
	// group names may not be, so OIDC groups match exactly.
	foldGroups bool

	// requireVerified makes emails and domains match only a verified email.
	requireVerified bool

	roles []string
}

// accessPolicy is the compiled rule set. It is immutable after NewAuthService.
type accessPolicy struct {
	rules     []accessRule
	providers map[string]config.ProviderConfig

	// groupRules names the providers with a rule that matches on groups (GitHub orgs and
	// teams included): only for those can a fresh login, which snapshots the groups, admit a
	// user the stored record does not.
	groupRules map[string]bool
}

// compileAccess validates the rules' roles and the admin role against the declared ones and
// compiles the rules.
func compileAccess(res *config.ResolvedAuth, declared []Role, adminRole string) (*accessPolicy, error) {
	known := make(map[string]bool, len(declared))
	for _, r := range declared {
		if r.Name == "" || strings.ContainsAny(r.Name, " \t\r\n") {
			return nil, fmt.Errorf("auth.WithRoles: role name %q must be non-empty with no whitespace", r.Name)
		}
		if known[r.Name] {
			return nil, fmt.Errorf("auth.WithRoles: role %q declared twice", r.Name)
		}
		known[r.Name] = true
	}
	if adminRole != "" && !known[adminRole] {
		return nil, fmt.Errorf("auth.WithAdminRole: unknown role %q (declared: %s)", adminRole, roleNames(declared))
	}

	ap := &accessPolicy{
		providers:  make(map[string]config.ProviderConfig, len(res.Providers)),
		groupRules: make(map[string]bool),
	}
	for _, p := range res.Providers {
		ap.providers[p.Name] = p
	}
	for _, r := range res.Rules {
		p := ap.providers[r.Provider] // config.Resolve checked it exists
		github := p.Type == config.ProviderTypeGitHub
		c := accessRule{
			where:           r.Where,
			provider:        r.Provider,
			anyone:          r.Anyone,
			users:           lowerSet(r.Users),
			subjects:        make(map[string]bool, len(r.Subjects)),
			emails:          lowerSet(r.Emails),
			domains:         make(map[string]bool, len(r.EmailDomains)),
			groups:          make(map[string]bool, len(r.Groups)+len(r.Orgs)+len(r.Teams)),
			foldGroups:      github,
			requireVerified: p.RequireVerifiedEmail == nil || *p.RequireVerifiedEmail,
		}
		for _, s := range r.Subjects {
			c.subjects[strings.TrimSpace(s)] = true
		}
		for _, d := range r.EmailDomains {
			c.domains[strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@"))] = true
		}
		for _, g := range slices.Concat(r.Groups, r.Orgs, r.Teams) {
			g = strings.TrimSpace(g)
			if github {
				g = strings.ToLower(g)
			}
			c.groups[g] = true
			ap.groupRules[r.Provider] = true
		}
		for _, role := range r.Roles {
			role = strings.TrimSpace(role)
			if len(declared) == 0 {
				return nil, fmt.Errorf("%s.roles: the application declares no roles, so rules cannot grant %q", r.Where, role)
			}
			if !known[role] {
				return nil, fmt.Errorf("%s.roles: unknown role %q (declared: %s)", r.Where, role, roleNames(declared))
			}
			c.roles = append(c.roles, role)
		}
		ap.rules = append(ap.rules, c)
	}
	return ap, nil
}

// warn logs the startup warnings for configurations that are valid but easy to get wrong.
func (ap *accessPolicy) warn(res *config.ResolvedAuth) {
	ruled := make(map[string]bool)
	for _, r := range ap.rules {
		ruled[r.provider] = true
		switch {
		case r.anyone && res.LegacyOpen:
			logging.Warn("auth enabled with an empty allowlist: every authenticated GitHub user will be authorized; " +
				"set auth.allowlist.users/orgs/teams to restrict access")
		case r.anyone:
			logging.Warn("auth access rule admits every user the provider authenticates",
				"rule", r.where, "provider", r.provider)
		}
	}
	for name := range ap.providers {
		if !ruled[name] {
			logging.Warn("auth provider has no access rule, so it admits nobody", "provider", name)
		}
	}
}

// evaluate returns whether id is admitted and, if so, its roles: sorted, without duplicates.
func (ap *accessPolicy) evaluate(id Identity) ([]string, bool) {
	var roles []string
	admitted := false
	for i := range ap.rules {
		if ap.rules[i].matches(id) {
			admitted = true
			roles = append(roles, ap.rules[i].roles...)
		}
	}
	if !admitted {
		return nil, false
	}
	slices.Sort(roles)
	return slices.Compact(roles), true
}

func (r *accessRule) matches(id Identity) bool {
	if id.Provider != r.provider {
		return false
	}
	if r.anyone {
		return true
	}
	if id.Username != "" && r.users[strings.ToLower(id.Username)] {
		return true
	}
	if id.Subject != "" && r.subjects[id.Subject] {
		return true
	}
	if id.EmailVerified || !r.requireVerified {
		email := strings.ToLower(id.Email)
		if at := strings.LastIndex(email, "@"); at > 0 {
			if r.emails[email] || r.domains[email[at+1:]] {
				return true
			}
		}
	}
	for _, g := range id.Groups {
		if r.foldGroups {
			g = strings.ToLower(g)
		}
		if g != "" && r.groups[g] {
			return true
		}
	}
	return false
}

// legacyIssuer is the issuer of every record written before v0.12.0: they are all github.com
// users, since the old GitHub configuration had no baseURL.
const legacyIssuer = "https://github.com"

// identityFromUser reads the identity snapshot off a stored user. Records written before
// v0.12.0 have no provider and are github.com users; their username and subject come from the
// GitHub fields.
func identityFromUser(u *User) Identity {
	id := Identity{
		Provider:      u.Provider,
		Issuer:        u.Issuer,
		Subject:       u.Subject,
		Username:      u.Username,
		Email:         u.Email,
		EmailVerified: u.EmailVerified,
		Name:          u.Name,
		Groups:        u.Groups,
		AvatarURL:     u.AvatarURL,
	}
	if id.Provider == "" {
		id.Provider = config.LegacyGitHubProvider
		// Stated rather than left empty, so the record stops matching once the provider
		// named github is pointed at an Enterprise Server or redefined as OIDC.
		id.Issuer = legacyIssuer
	}
	if id.Username == "" {
		id.Username = u.GitHubLogin
	}
	if id.Subject == "" && u.GitHubID != 0 {
		id.Subject = strconv.FormatInt(u.GitHubID, 10)
	}
	if u.LastLoginAt.IsZero() && u.Provider == "" {
		id.Groups = slices.Concat(u.Organizations, u.Teams)
	}
	return id
}

// setIdentitySnapshot records what the provider said at this login on the user record.
func setIdentitySnapshot(u *User, id Identity, now time.Time) {
	u.Provider = id.Provider
	u.Issuer = id.Issuer
	u.Subject = id.Subject
	u.Username = id.Username
	u.Email = id.Email
	u.EmailVerified = id.EmailVerified
	u.Name = id.Name
	u.Groups = slices.Clone(id.Groups)
	u.AvatarURL = id.AvatarURL
	u.LastLoginAt = now.UTC()
}

// authorizeUser evaluates the current rules against a stored user. The principal is returned
// only when the user is admitted.
func (svc *AuthService) authorizeUser(u *User) (*Principal, bool) {
	id := identityFromUser(u)
	if !svc.sameIssuer(id) {
		return nil, false
	}
	roles, ok := svc.access.evaluate(id)
	if !ok {
		return nil, false
	}
	p := &Principal{
		UserID:        u.ID,
		Provider:      id.Provider,
		Subject:       id.Subject,
		Username:      id.Username,
		Email:         id.Email,
		EmailVerified: id.EmailVerified,
		Name:          id.Name,
		Groups:        slices.Clone(id.Groups),
		Roles:         roles,
	}
	// A login is GitHub's stable public handle and what embedders already key data by; an
	// OIDC username is not stable, so OIDC principals are keyed by sub.
	if svc.access.providers[id.Provider].Type == config.ProviderTypeGitHub {
		p.ID = id.Provider + ":" + strings.ToLower(id.Username)
	} else {
		p.ID = id.Provider + ":" + id.Subject
	}
	return p, true
}

// sameIssuer reports whether id is from the issuer its provider has now. A record from another
// issuer (the provider's issuerURL or baseURL has since changed, or the provider named github
// is no longer github.com) is a different user space: the same sub there need not be the same
// person. checkStoredIssuers refuses to start with such records, so this is a second line for
// a store written to after that check.
func (svc *AuthService) sameIssuer(id Identity) bool {
	p, ok := svc.access.providers[id.Provider]
	return ok && id.Issuer == providerIssuer(p)
}

// checkStoredIssuers refuses to start when the store holds users of a configured provider
// name from an issuer other than the one that name has now. Principal.ID is built from the
// provider name and the sub or login, not the issuer, so a new issuer under an old name would
// hand its users the ids of the old issuer's users, and with them whatever the application
// keyed by those ids. Pre-v0.12 records count as github.com users of the provider named github.
// Records of providers no longer configured are left alone: nobody can sign in as them.
//
// The guard is only as durable as the store: with a new database file the old users, and the
// check, are gone, while application data keyed by their ids may not be.
func checkStoredIssuers(s *BoltStorage, providers []config.ProviderConfig) error {
	current := make(map[string]string, len(providers))
	for _, p := range providers {
		current[p.Name] = providerIssuer(p)
	}
	return s.forEachUser(func(u *User) error {
		id := identityFromUser(u)
		want, ok := current[id.Provider]
		if !ok || id.Issuer == want {
			return nil
		}
		return fmt.Errorf("auth.providers (%s): the provider's issuer is now %s, but the auth store holds its users "+
			"from %s. Principal ids are %q plus the user's sub or login, without the issuer, so the users of both "+
			"would share ids: give the provider for %s a new name instead (the old name's users then stop being "+
			"admitted once it is gone)", id.Provider, want, id.Issuer, id.Provider+":", want)
	})
}

// reloginMayAdmit reports whether a browser login could admit a user that authorizeUser
// refuses, which decides between 401 (sign in again) and 403. It can when the record is from
// another issuer of a provider still configured (the login finds or creates the user under
// the current one), or when it has no identity snapshot (stored before v0.12.0, which recorded
// no groups) and some rule for its provider matches on groups. Otherwise the login would read
// the same identity and be refused too, or could not happen at all (the provider is gone), so
// a 401 would only send the client round a pointless browser sign-in.
func (svc *AuthService) reloginMayAdmit(u *User) bool {
	id := identityFromUser(u)
	p, ok := svc.access.providers[id.Provider]
	if !ok {
		return false
	}
	if id.Issuer != providerIssuer(p) {
		return true
	}
	return u.LastLoginAt.IsZero() && svc.access.groupRules[id.Provider]
}

func lowerSet(vals []string) map[string]bool {
	set := make(map[string]bool, len(vals))
	for _, v := range vals {
		set[strings.ToLower(strings.TrimSpace(v))] = true
	}
	return set
}

func roleNames(roles []Role) string {
	names := make([]string, len(roles))
	for i, r := range roles {
		names[i] = r.Name
	}
	return strings.Join(names, ", ")
}
