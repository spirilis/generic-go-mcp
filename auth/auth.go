package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/spirilis/generic-go-mcp/config"
	"github.com/spirilis/generic-go-mcp/logging"
)

// AuthService is the main entry point for all auth operations
type AuthService struct {
	config       *config.AuthConfig
	storage      Storage
	githubClient *GitHubClient
	tokenService *TokenService
}

// NewAuthService creates a new authentication service
func NewAuthService(cfg *config.AuthConfig) (*AuthService, error) {
	if cfg == nil {
		return nil, fmt.Errorf("auth config is required")
	}

	storage, err := NewBoltStorage(cfg.Storage.DBPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage: %w", err)
	}

	githubClient := NewGitHubClient(cfg.GitHub)
	tokenService := NewTokenService(cfg.Issuer, storage)

	svc := &AuthService{
		config:       cfg,
		storage:      storage,
		githubClient: githubClient,
		tokenService: tokenService,
	}

	// A completely empty allowlist means isUserAuthorized admits every authenticated GitHub
	// user (see github.go). That is a legitimate configuration, but a dangerous default to
	// hit by accident on a deployed server, so make it loud rather than silent.
	if len(cfg.Allowlist.Users) == 0 && len(cfg.Allowlist.Orgs) == 0 && len(cfg.Allowlist.Teams) == 0 {
		logging.Warn("auth enabled with an empty allowlist: every authenticated GitHub user will be authorized; " +
			"set auth.allowlist.users/orgs/teams to restrict access")
	}

	// Initialize static clients from config
	for _, client := range cfg.Clients {
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
			return nil, fmt.Errorf("failed to store static client %s: %w", client.ClientID, err)
		}
	}

	return svc, nil
}

// Close closes the auth service and releases resources
func (svc *AuthService) Close() error {
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
