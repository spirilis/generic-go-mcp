package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"time"
)

const (
	AccessTokenLength  = 32 // 256 bits
	RefreshTokenLength = 32
	AuthCodeLength     = 32
	ClientIDLength     = 16
	ClientSecretLength = 32

	AccessTokenTTL  = 1 * time.Hour
	RefreshTokenTTL = 24 * time.Hour * 30 // 30 days
	AuthCodeTTL     = 10 * time.Minute
)

// TokenService handles token generation and validation
type TokenService struct {
	issuer  string
	storage Storage
}

// NewTokenService creates a new token service
func NewTokenService(issuer string, storage Storage) *TokenService {
	return &TokenService{
		issuer:  issuer,
		storage: storage,
	}
}

// GenerateAccessToken creates a new access token
func (ts *TokenService) GenerateAccessToken(userID, clientID, scope, resource string) (*AccessToken, error) {
	token := generateSecureToken(AccessTokenLength)

	accessToken := &AccessToken{
		Token:     token,
		TokenType: "Bearer",
		ClientID:  clientID,
		UserID:    userID,
		Scope:     scope,
		Resource:  resource,
		ExpiresAt: time.Now().Add(AccessTokenTTL),
		CreatedAt: time.Now(),
	}

	if err := ts.storage.StoreAccessToken(context.Background(), accessToken); err != nil {
		return nil, err
	}

	return accessToken, nil
}

// GenerateRefreshToken creates a new refresh token
func (ts *TokenService) GenerateRefreshToken(userID, clientID, scope, resource string) (*RefreshToken, error) {
	token := generateSecureToken(RefreshTokenLength)

	refreshToken := &RefreshToken{
		Token:     token,
		ClientID:  clientID,
		UserID:    userID,
		Scope:     scope,
		Resource:  resource,
		ExpiresAt: time.Now().Add(RefreshTokenTTL),
		CreatedAt: time.Now(),
	}

	if err := ts.storage.StoreRefreshToken(context.Background(), refreshToken); err != nil {
		return nil, err
	}

	return refreshToken, nil
}

// GenerateAuthorizationCode creates a new authorization code
func (ts *TokenService) GenerateAuthorizationCode(
	clientID, redirectURI, scope, codeChallenge, codeChallengeMethod, resource, userID string,
) (*AuthorizationCode, error) {
	code := generateSecureToken(AuthCodeLength)

	authCode := &AuthorizationCode{
		Code:                code,
		ClientID:            clientID,
		RedirectURI:         redirectURI,
		Scope:               scope,
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
		Resource:            resource,
		UserID:              userID,
		ExpiresAt:           time.Now().Add(AuthCodeTTL),
		CreatedAt:           time.Now(),
	}

	if err := ts.storage.StoreAuthCode(context.Background(), authCode); err != nil {
		return nil, err
	}

	return authCode, nil
}

// ValidateAccessToken checks if an access token is valid
func (ts *TokenService) ValidateAccessToken(token string) (*AccessToken, error) {
	accessToken, err := ts.storage.GetAccessToken(context.Background(), token)
	if err != nil {
		return nil, err
	}

	if accessToken == nil {
		return nil, ErrTokenNotFound
	}

	if time.Now().After(accessToken.ExpiresAt) {
		return nil, ErrTokenExpired
	}

	return accessToken, nil
}

// generateSecureToken generates a cryptographically secure random token
func generateSecureToken(length int) string {
	b := make([]byte, length)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// GenerateClientCredentials generates client_id and client_secret
func GenerateClientCredentials() (clientID, clientSecret string) {
	clientID = generateSecureToken(ClientIDLength)
	clientSecret = generateSecureToken(ClientSecretLength)
	return
}
