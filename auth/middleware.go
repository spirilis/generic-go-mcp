package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/spirilis/generic-go-mcp/logging"
)

// Context keys
type contextKey string

const (
	ContextKeyUser        contextKey = "auth_user"
	ContextKeyAccessToken contextKey = "auth_access_token"
	ContextKeySession     contextKey = "auth_session"
)

// Middleware creates an HTTP middleware for token validation
func (svc *AuthService) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract Bearer token from Authorization header
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			logging.Debug("Auth failed: missing Authorization header", "remote_addr", r.RemoteAddr)
			svc.unauthorized(w, "Missing Authorization header")
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			logging.Debug("Auth failed: invalid Authorization header format", "remote_addr", r.RemoteAddr)
			svc.unauthorized(w, "Invalid Authorization header format")
			return
		}

		tokenStr := parts[1]

		// Validate token
		accessToken, err := svc.tokenService.ValidateAccessToken(tokenStr)
		if err != nil {
			if err == ErrTokenExpired {
				logging.Debug("Auth failed: token expired", "remote_addr", r.RemoteAddr)
				svc.unauthorized(w, "Access token expired")
			} else {
				logging.Debug("Auth failed: invalid token", "remote_addr", r.RemoteAddr, "error", err)
				svc.unauthorized(w, "Invalid access token")
			}
			return
		}

		// Get user
		user, err := svc.storage.GetUser(r.Context(), accessToken.UserID)
		if err != nil || user == nil {
			logging.Debug("Auth failed: user not found", "user_id", accessToken.UserID, "remote_addr", r.RemoteAddr)
			svc.unauthorized(w, "User not found")
			return
		}

		logging.Debug("Auth successful",
			"user_id", user.ID,
			"github_login", user.GitHubLogin,
			"client_id", accessToken.ClientID,
			"remote_addr", r.RemoteAddr)

		// Add to context
		ctx := r.Context()
		ctx = context.WithValue(ctx, ContextKeyUser, user)
		ctx = context.WithValue(ctx, ContextKeyAccessToken, accessToken)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// unauthorized sends a 401 response with WWW-Authenticate header per RFC 9728
func (svc *AuthService) unauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="MCP Server", `+
		`resource_metadata="`+svc.config.Issuer+`/.well-known/oauth-protected-resource"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	json.NewEncoder(w).Encode(map[string]string{
		"error":             "invalid_token",
		"error_description": message,
	})
}

// forbidden sends a 403 response for insufficient scope
func (svc *AuthService) forbidden(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	json.NewEncoder(w).Encode(map[string]string{
		"error":             "insufficient_scope",
		"error_description": message,
	})
}

// GetUserFromContext retrieves the authenticated user from the request context
func GetUserFromContext(ctx context.Context) *User {
	user, ok := ctx.Value(ContextKeyUser).(*User)
	if !ok {
		return nil
	}
	return user
}

// GetAccessTokenFromContext retrieves the access token from the request context
func GetAccessTokenFromContext(ctx context.Context) *AccessToken {
	token, ok := ctx.Value(ContextKeyAccessToken).(*AccessToken)
	if !ok {
		return nil
	}
	return token
}
