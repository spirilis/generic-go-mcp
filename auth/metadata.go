package auth

import (
	"encoding/json"
	"net/http"
)

// AuthorizationServerMetadata per RFC 8414
type AuthorizationServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint,omitempty"`
	ScopesSupported                   []string `json:"scopes_supported,omitempty"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	RequirePKCE                       bool     `json:"require_pkce,omitempty"` // OAuth 2.1
}

// ProtectedResourceMetadata per RFC 9728
type ProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported,omitempty"`
	BearerMethodsSupported []string `json:"bearer_methods_supported,omitempty"`
}

// handleAuthServerMetadata handles GET /.well-known/oauth-authorization-server
func (svc *AuthService) handleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	metadata := AuthorizationServerMetadata{
		Issuer:                            svc.config.Issuer,
		AuthorizationEndpoint:             svc.config.Issuer + "/authorize",
		TokenEndpoint:                     svc.config.Issuer + "/token",
		RegistrationEndpoint:              svc.config.Issuer + "/register",
		ScopesSupported:                   []string{"mcp:tools", "mcp:resources", "mcp:prompts"},
		ResponseTypesSupported:            []string{"code"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported: []string{"client_secret_post", "none"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		RequirePKCE:                       true, // OAuth 2.1 mandatory
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(metadata)
}

// handleProtectedResourceMetadata handles GET /.well-known/oauth-protected-resource
func (svc *AuthService) handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	metadata := ProtectedResourceMetadata{
		Resource:               svc.config.Issuer + "/mcp",
		AuthorizationServers:   []string{svc.config.Issuer},
		ScopesSupported:        []string{"mcp:tools", "mcp:resources", "mcp:prompts"},
		BearerMethodsSupported: []string{"header"},
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(metadata)
}
