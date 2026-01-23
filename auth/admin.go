package auth

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// RegisterAdminRoutes adds admin endpoints to the mux (protected by auth middleware)
func (svc *AuthService) RegisterAdminRoutes(mux *http.ServeMux) {
	// Wrap admin endpoints with auth middleware
	mux.Handle("/admin/clients", svc.Middleware(http.HandlerFunc(svc.handleAdminClients)))
}

// handleAdminClients handles admin client management endpoints
func (svc *AuthService) handleAdminClients(w http.ResponseWriter, r *http.Request) {
	// Extract client ID from path if present
	path := r.URL.Path
	clientID := ""
	if strings.HasPrefix(path, "/admin/clients/") {
		clientID = strings.TrimPrefix(path, "/admin/clients/")
	}

	switch r.Method {
	case http.MethodPost:
		svc.handleCreateStaticClient(w, r)
	case http.MethodGet:
		if clientID != "" {
			svc.handleGetStaticClient(w, r, clientID)
		} else {
			svc.handleListStaticClients(w, r)
		}
	case http.MethodDelete:
		if clientID != "" {
			svc.handleDeleteStaticClient(w, r, clientID)
		} else {
			http.Error(w, "Client ID required", http.StatusBadRequest)
		}
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// CreateStaticClientRequest is the request body for creating a static client
type CreateStaticClientRequest struct {
	ClientName   string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
	Scopes       []string `json:"scopes,omitempty"`
}

// StaticClientResponse is the response for static client operations
type StaticClientResponse struct {
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret,omitempty"` // Only included on creation
	ClientName   string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
	Scopes       []string `json:"scopes,omitempty"`
	CreatedAt    string   `json:"created_at"`
}

// handleCreateStaticClient creates a new static client
func (svc *AuthService) handleCreateStaticClient(w http.ResponseWriter, r *http.Request) {
	var req CreateStaticClientRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Validate required fields
	if req.ClientName == "" {
		http.Error(w, "client_name is required", http.StatusBadRequest)
		return
	}
	if len(req.RedirectURIs) == 0 {
		http.Error(w, "redirect_uris is required", http.StatusBadRequest)
		return
	}

	// Generate client credentials
	clientID, clientSecret := GenerateClientCredentials()

	// Create client record
	client := &RegisteredClient{
		ClientID:                clientID,
		ClientSecret:            hashSecret(clientSecret),
		ClientName:              req.ClientName,
		RedirectURIs:            req.RedirectURIs,
		Scopes:                  req.Scopes,
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "client_secret_post",
		IsStatic:                true,
		CreatedAt:               time.Now(),
	}

	if err := svc.storage.StoreClient(r.Context(), client); err != nil {
		http.Error(w, "Failed to create client", http.StatusInternalServerError)
		return
	}

	// Return response with plain secret (only time it's returned)
	resp := StaticClientResponse{
		ClientID:     clientID,
		ClientSecret: clientSecret, // Plain text, only returned once
		ClientName:   client.ClientName,
		RedirectURIs: client.RedirectURIs,
		Scopes:       client.Scopes,
		CreatedAt:    client.CreatedAt.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

// handleListStaticClients lists all static clients
func (svc *AuthService) handleListStaticClients(w http.ResponseWriter, r *http.Request) {
	clients, err := svc.storage.ListClients(r.Context())
	if err != nil {
		http.Error(w, "Failed to list clients", http.StatusInternalServerError)
		return
	}

	// Filter to only static clients and hide secrets
	var staticClients []StaticClientResponse
	for _, client := range clients {
		if client.IsStatic {
			staticClients = append(staticClients, StaticClientResponse{
				ClientID:     client.ClientID,
				ClientName:   client.ClientName,
				RedirectURIs: client.RedirectURIs,
				Scopes:       client.Scopes,
				CreatedAt:    client.CreatedAt.Format(time.RFC3339),
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(staticClients)
}

// handleGetStaticClient gets a specific static client
func (svc *AuthService) handleGetStaticClient(w http.ResponseWriter, r *http.Request, clientID string) {
	client, err := svc.storage.GetClient(r.Context(), clientID)
	if err != nil {
		http.Error(w, "Client not found", http.StatusNotFound)
		return
	}

	if !client.IsStatic {
		http.Error(w, "Not a static client", http.StatusForbidden)
		return
	}

	resp := StaticClientResponse{
		ClientID:     client.ClientID,
		ClientName:   client.ClientName,
		RedirectURIs: client.RedirectURIs,
		Scopes:       client.Scopes,
		CreatedAt:    client.CreatedAt.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleDeleteStaticClient deletes a static client
func (svc *AuthService) handleDeleteStaticClient(w http.ResponseWriter, r *http.Request, clientID string) {
	client, err := svc.storage.GetClient(r.Context(), clientID)
	if err != nil {
		http.Error(w, "Client not found", http.StatusNotFound)
		return
	}

	if !client.IsStatic {
		http.Error(w, "Cannot delete non-static client", http.StatusForbidden)
		return
	}

	if err := svc.storage.DeleteClient(r.Context(), clientID); err != nil {
		http.Error(w, "Failed to delete client", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
