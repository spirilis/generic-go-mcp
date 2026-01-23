package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
)

// Sentinel errors
var (
	ErrTokenNotFound      = errors.New("token not found")
	ErrTokenExpired       = errors.New("token expired")
	ErrClientNotFound     = errors.New("client not found")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrUserNotFound       = errors.New("user not found")
	ErrSessionNotFound    = errors.New("session not found")
)

// OAuthError represents an OAuth error response per RFC 6749
type OAuthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
	ErrorURI         string `json:"error_uri,omitempty"`
}

// authError redirects with error (for authorization endpoint)
func (svc *AuthService) authError(w http.ResponseWriter, redirectURI, errorCode, description, state string) {
	if redirectURI == "" {
		// Can't redirect, return error directly
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(OAuthError{
			Error:            errorCode,
			ErrorDescription: description,
		})
		return
	}

	u, _ := url.Parse(redirectURI)
	q := u.Query()
	q.Set("error", errorCode)
	if description != "" {
		q.Set("error_description", description)
	}
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()

	http.Redirect(w, nil, u.String(), http.StatusFound)
}

// tokenError returns error for token endpoint
func (svc *AuthService) tokenError(w http.ResponseWriter, errorCode, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(OAuthError{
		Error:            errorCode,
		ErrorDescription: description,
	})
}

// registrationError returns error for client registration
func (svc *AuthService) registrationError(w http.ResponseWriter, errorCode, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(OAuthError{
		Error:            errorCode,
		ErrorDescription: description,
	})
}
