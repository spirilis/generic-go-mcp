package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/spirilis/generic-go-mcp/config"
)

// AuthService is the main entry point for all auth operations
type AuthService struct {
	config       *config.AuthConfig
	storage      Storage
	tokenService *TokenService

	// providers are the identity providers users sign in with, in config order (the order the
	// chooser lists them); providerByName indexes them.
	providers      []Provider
	providerByName map[string]Provider

	// bg runs the providers' background fetches; Close stops them.
	bg *background

	// access is the compiled rule set every login and every request is checked against.
	access *accessPolicy

	// roles are the roles the application declared (WithRoles).
	roles []Role

	// adminRole is the role the admin API requires (WithAdminRole). adminForAll serves it to
	// every admitted user instead, as before v0.12.0; only the pure legacy form, with no admin
	// role named, gets that. With neither the admin API is not served.
	adminRole   string
	adminForAll bool

	// identityMaxAge is config.AccessConfig.IdentityMaxAge (0: the refresh token lifetime).
	identityMaxAge time.Duration

	// trustedClients are the client IDs pre-registered in config. The operator vetted their
	// redirect URIs, so they skip the consent screen. Membership is deliberately not
	// RegisteredClient.IsStatic: /admin/clients sets that too, for clients no operator reviewed.
	trustedClients map[string]bool

	// allowedRedirectHosts is config.RegistrationConfig.AllowedRedirectHosts, normalized. Empty
	// means /register accepts any redirect host.
	allowedRedirectHosts map[string]bool

	// stopSweep ends the background sweep of expired records; sweepDone closes when it has.
	stopSweep, sweepDone chan struct{}
}

// NewAuthService creates a new authentication service. It refuses to start on any
// configuration error: an unknown provider or role in a rule, an unreadable secret file, and
// the rest that config.AuthConfig.Resolve and the role checks reject.
func NewAuthService(cfg *config.AuthConfig, opts ...Option) (*AuthService, error) {
	if cfg == nil {
		return nil, fmt.Errorf("auth config is required")
	}
	var o serviceOptions
	for _, opt := range opts {
		opt(&o)
	}

	// Validate before opening storage, so a bad entry fails startup without leaving the database
	// open.
	allowedHosts, err := normalizeRedirectHosts(cfg.Registration.AllowedRedirectHosts)
	if err != nil {
		return nil, err
	}
	resolved, err := cfg.Resolve()
	if err != nil {
		return nil, err
	}
	access, err := compileAccess(resolved, o.roles, o.adminRole)
	if err != nil {
		return nil, err
	}

	// Providers do no network I/O here: an unreachable IdP must not stop the server starting.
	bg := newBackground()
	providers := make([]Provider, 0, len(resolved.Providers))
	byName := make(map[string]Provider, len(resolved.Providers))
	for _, p := range resolved.Providers {
		prov, err := newProvider(p, bg)
		if err != nil {
			bg.Close()
			return nil, err
		}
		providers = append(providers, prov)
		byName[p.Name] = prov
	}

	storage, err := NewBoltStorage(cfg.Storage.DBPath)
	if err != nil {
		bg.Close()
		return nil, fmt.Errorf("failed to create storage: %w", err)
	}
	if err := checkStoredIssuers(storage, resolved.Providers); err != nil {
		bg.Close()
		storage.Close()
		return nil, err
	}

	svc := &AuthService{
		config:               cfg,
		storage:              storage,
		providers:            providers,
		providerByName:       byName,
		bg:                   bg,
		tokenService:         NewTokenService(cfg.Issuer, storage),
		access:               access,
		roles:                slices.Clone(o.roles),
		adminRole:            o.adminRole,
		adminForAll:          o.adminRole == "" && resolved.LegacyForm,
		identityMaxAge:       resolved.IdentityMaxAge,
		trustedClients:       make(map[string]bool, len(cfg.Clients)),
		allowedRedirectHosts: allowedHosts,
	}

	// Rules that admit everyone (including the legacy empty allowlist) and providers no rule
	// admits from are legitimate, but dangerous or surprising to hit by accident on a deployed
	// server, so make them loud rather than silent.
	access.warn(resolved)

	// Initialize static clients from config
	for _, client := range cfg.Clients {
		svc.trustedClients[client.ClientID] = true
		err := svc.storage.StoreClient(context.Background(), &RegisteredClient{
			ClientID:                client.ClientID,
			ClientSecret:            hashSecret(client.ClientSecret),
			ClientName:              client.Name,
			RedirectURIs:            client.RedirectURIs,
			Scopes:                  client.Scopes,
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "client_secret_post",
			IsStatic:                true,
			CreatedAt:               time.Now(),
		})
		if err != nil {
			svc.bg.Close()
			svc.storage.Close()
			return nil, fmt.Errorf("failed to store static client %s: %w", client.ClientID, err)
		}
	}

	svc.startSweeper()
	return svc, nil
}

// Close closes the auth service and releases resources. Background work (the sweep, OIDC
// discovery and key refresh) has stopped by the time it returns.
func (svc *AuthService) Close() error {
	svc.bg.Close()
	svc.stopSweeper()
	if svc.storage != nil {
		return svc.storage.Close()
	}
	return nil
}

// User represents an authenticated user
type User struct {
	ID            string   `json:"id"`
	GitHubLogin   string   `json:"github_login"`
	GitHubID      int64    `json:"github_id"`
	Email         string   `json:"email,omitempty"`
	Name          string   `json:"name,omitempty"`
	AvatarURL     string   `json:"avatar_url,omitempty"`
	Organizations []string `json:"organizations,omitempty"`
	Teams         []string `json:"teams,omitempty"` // Format: "org/team"

	// The identity snapshot, rewritten at each browser login and evaluated against the access
	// rules on every request. A record from before v0.12.0 has none of these: it is a GitHub
	// user (Provider "" reads as "github") whose groups were never recorded, which
	// LastLoginAt.IsZero() identifies.
	//
	// (Provider, Issuer, Subject) is the record's identity key: an OIDC user is found by it at
	// login, never by email or username. Issuer is the OIDC issuer, or the GitHub web URL.
	Provider      string    `json:"provider,omitempty"`
	Issuer        string    `json:"issuer,omitempty"`
	Subject       string    `json:"subject,omitempty"`
	Username      string    `json:"username,omitempty"`
	EmailVerified bool      `json:"email_verified,omitempty"`
	Groups        []string  `json:"groups,omitempty"`
	LastLoginAt   time.Time `json:"last_login_at,omitzero"`
}

// AuthorizationCode represents a pending authorization code
type AuthorizationCode struct {
	Code                string    `json:"code"`
	ClientID            string    `json:"client_id"`
	RedirectURI         string    `json:"redirect_uri"`
	Scope               string    `json:"scope"`
	CodeChallenge       string    `json:"code_challenge"`
	CodeChallengeMethod string    `json:"code_challenge_method"`
	Resource            string    `json:"resource,omitempty"` // RFC 8707
	UserID              string    `json:"user_id"`
	ExpiresAt           time.Time `json:"expires_at"`
	CreatedAt           time.Time `json:"created_at"`
}

// AccessToken represents an issued access token
type AccessToken struct {
	Token     string    `json:"token"`
	TokenType string    `json:"token_type"` // "Bearer"
	ClientID  string    `json:"client_id"`
	UserID    string    `json:"user_id"`
	Scope     string    `json:"scope"`
	Resource  string    `json:"resource,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

// RefreshToken represents a refresh token
type RefreshToken struct {
	Token     string    `json:"token"`
	ClientID  string    `json:"client_id"`
	UserID    string    `json:"user_id"`
	Scope     string    `json:"scope"`
	Resource  string    `json:"resource,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`

	// LegacyAgeFrom is when the refresh chain of a user with no recorded login (a record from
	// before v0.12.0) first refreshed after the upgrade. It is carried across rotations and
	// stands in for User.LastLoginAt in the identityMaxAge check, so such a user too must sign
	// in again within identityMaxAge of the upgrade. It lives on the token, not the user, so
	// the refresh grant never rewrites a user record a concurrent login may be updating.
	LegacyAgeFrom time.Time `json:"legacy_age_from,omitzero"`
}

// RegisteredClient represents an OAuth client (dynamic or static)
type RegisteredClient struct {
	ClientID                string    `json:"client_id"`
	ClientSecret            string    `json:"client_secret,omitempty"` // Hashed
	ClientName              string    `json:"client_name"`
	ClientURI               string    `json:"client_uri,omitempty"`
	LogoURI                 string    `json:"logo_uri,omitempty"`
	RedirectURIs            []string  `json:"redirect_uris"`
	Scopes                  []string  `json:"scope,omitempty"`
	GrantTypes              []string  `json:"grant_types,omitempty"`
	ResponseTypes           []string  `json:"response_types,omitempty"`
	TokenEndpointAuthMethod string    `json:"token_endpoint_auth_method,omitempty"`
	IsStatic                bool      `json:"is_static"` // True if from config or admin endpoint
	CreatedAt               time.Time `json:"created_at"`
}

// AuthSession ties MCP sessions to authenticated users
type AuthSession struct {
	SessionID   string    `json:"session_id"`
	UserID      string    `json:"user_id"`
	ClientID    string    `json:"client_id"`
	AccessToken string    `json:"access_token"`
	CreatedAt   time.Time `json:"created_at"`
	LastUsedAt  time.Time `json:"last_used_at"`
}

// PendingAuthRequest represents a pending OAuth authorization request
type PendingAuthRequest struct {
	ID                  string    `json:"id"`
	ClientID            string    `json:"client_id"`
	RedirectURI         string    `json:"redirect_uri"`
	Scope               string    `json:"scope"`
	State               string    `json:"state"`
	CodeChallenge       string    `json:"code_challenge"`
	CodeChallengeMethod string    `json:"code_challenge_method"`
	Resource            string    `json:"resource,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	ExpiresAt           time.Time `json:"expires_at"`

	// The sign-in at the identity provider. Provider is empty while the user is on the
	// chooser page (and on requests stored before v0.12.0, which all went to GitHub). Nonce
	// and UpstreamVerifier (PKCE toward the IdP) are fresh for each redirect to a provider.
	Provider         string `json:"provider,omitempty"`
	Nonce            string `json:"nonce,omitempty"`
	UpstreamVerifier string `json:"upstream_verifier,omitempty"`
}

// PendingConsent is an authorization that has passed the provider login and the access rules, parked
// until the user approves or denies the requesting client on the consent screen.
type PendingConsent struct {
	ID string `json:"id"`

	// BindingHash is the SHA-256 of the consent cookie set on the browser that was shown the
	// consent screen. The approval must come back from that same browser.
	BindingHash string `json:"binding_hash"`

	UserID    string             `json:"user_id"`
	Request   PendingAuthRequest `json:"request"`
	ExpiresAt time.Time          `json:"expires_at"`
}

// normalizeRedirectHosts validates config.RegistrationConfig.AllowedRedirectHosts and returns it
// as a lowercase set. Entries must be bare hostnames. A URL, a host:port or a path is rejected
// rather than silently never matching: an allowlist that matches nothing would lock out every
// client, and one that was meant to be narrower than it is would pass review unnoticed.
func normalizeRedirectHosts(hosts []string) (map[string]bool, error) {
	set := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		host := strings.ToLower(strings.TrimSpace(h))
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]") // "[::1]" -> "::1"
		switch {
		case host == "":
			return nil, fmt.Errorf("auth.registration.allowedRedirectHosts: empty entry")
		case strings.ContainsAny(host, "/?#@"),
			strings.Contains(host, ":") && net.ParseIP(host) == nil:
			return nil, fmt.Errorf("auth.registration.allowedRedirectHosts: %q must be a bare hostname "+
				"(e.g. \"claude.ai\" or \"localhost\"), with no scheme, port or path", h)
		}
		set[host] = true
	}
	return set, nil
}

// hashSecret hashes a secret using SHA-256
func hashSecret(secret string) string {
	hash := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(hash[:])
}

// verifySecret checks if a secret matches a hashed value, comparing in constant time so a
// timing side channel can't be used to probe the stored hash.
func verifySecret(secret, hashedSecret string) bool {
	return subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(hashedSecret)) == 1
}

// authenticateClient authenticates the client presenting itself at the token endpoint.
// It returns an empty string on success, or a human-readable error_description otherwise
// (mapped to an OAuth "invalid_client" error by the caller).
//
// A client registered with a stored secret is confidential (client_secret_post) and MUST
// present the matching secret. A client with no stored secret is public and authenticates
// by PKCE alone (verified separately, per grant), so an empty stored secret is not a
// bypass — it is the marker of a public client.
func (svc *AuthService) authenticateClient(ctx context.Context, clientID, clientSecret string) string {
	client, err := svc.storage.GetClient(ctx, clientID)
	if err != nil || client == nil {
		return "Unknown client"
	}
	if client.ClientSecret != "" && !verifySecret(clientSecret, client.ClientSecret) {
		return "Invalid client credentials"
	}
	return ""
}

// validateRedirectURI checks if a redirect URI matches one of the registered URIs
func (svc *AuthService) validateRedirectURI(client *RegisteredClient, redirectURI string) bool {
	for _, uri := range client.RedirectURIs {
		if uri == redirectURI {
			return true
		}
	}
	return false
}
