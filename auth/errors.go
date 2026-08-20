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

// authError delivers an authorization-endpoint error to the client by redirecting back to
// redirectURI with error/error_description/state query parameters (OAuth 2.1 §4.1.2.1).
//
// A non-empty redirectURI MUST already have been validated against the client's registered
// set before it reaches here: an unregistered or unparseable redirect target is never
// redirected to (that would be an open redirect and could leak the error to an
// attacker-controlled URL) — such cases fall back to authErrorDirect. A missing redirectURI
// does too.
//
// The redirect is written by setting Location directly rather than calling http.Redirect:
// this helper has no *http.Request to give it, and http.Redirect dereferences one
// unconditionally (r.Method, and r.URL for a relative target) — passing nil panics.
func (svc *AuthService) authError(w http.ResponseWriter, redirectURI, errorCode, description, state string) {
	if redirectURI == "" {
		svc.authErrorDirect(w, errorCode, description)
		return
	}

	u, err := url.Parse(redirectURI)
	if err != nil || u == nil {
		svc.authErrorDirect(w, errorCode, description)
		return
	}

	q := u.Query()
	q.Set("error", errorCode)
	if description != "" {
		q.Set("error_description", description)
	}
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()

	w.Header().Set("Location", u.String())
	w.WriteHeader(http.StatusFound)
}

// authErrorDirect renders an authorization error straight to the response, for the cases
// where there is no trustworthy redirect target to send it to (no redirect_uri, or one that
// failed client validation).
func (svc *AuthService) authErrorDirect(w http.ResponseWriter, errorCode, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(OAuthError{
		Error:            errorCode,
		ErrorDescription: description,
	})
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
