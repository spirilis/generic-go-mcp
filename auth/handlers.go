package auth

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// RegisterRoutes adds all OAuth endpoints to the mux
func (svc *AuthService) RegisterRoutes(mux *http.ServeMux) {
	// RFC 8414 - Authorization Server Metadata
	mux.HandleFunc("/.well-known/oauth-authorization-server", svc.handleAuthServerMetadata)

	// RFC 9728 - Protected Resource Metadata
	mux.HandleFunc("/.well-known/oauth-protected-resource", svc.handleProtectedResourceMetadata)

	// RFC 7591 - Dynamic Client Registration
	mux.HandleFunc("/register", svc.handleClientRegistration)

	// OAuth 2.1 Authorization Endpoint
	mux.HandleFunc("/authorize", svc.handleAuthorize)

	// OAuth 2.1 Token Endpoint
	mux.HandleFunc("/token", svc.handleToken)

	// GitHub OAuth callback
	mux.HandleFunc("/callback", svc.handleGitHubCallback)
}

// handleAuthorize handles the authorization endpoint (GET and POST)
func (svc *AuthService) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse parameters (from query string for GET, form for POST)
	var params url.Values
	if r.Method == http.MethodGet {
		params = r.URL.Query()
	} else {
		r.ParseForm()
		params = r.Form
	}

	clientID := params.Get("client_id")
	redirectURI := params.Get("redirect_uri")
	responseType := params.Get("response_type")
	scope := params.Get("scope")
	state := params.Get("state")
	codeChallenge := params.Get("code_challenge")
	codeChallengeMethod := params.Get("code_challenge_method")
	resource := params.Get("resource") // RFC 8707

	// Validate required parameters
	if responseType != "code" {
		svc.authError(w, redirectURI, "unsupported_response_type",
			"Only 'code' response_type is supported", state)
		return
	}

	// Validate PKCE (MANDATORY in OAuth 2.1)
	if err := ValidateCodeChallenge(codeChallenge, codeChallengeMethod); err != nil {
		svc.authError(w, redirectURI, "invalid_request", err.Error(), state)
		return
	}

	// Validate client
	client, err := svc.storage.GetClient(r.Context(), clientID)
	if err != nil || client == nil {
		svc.authError(w, redirectURI, "invalid_client", "Unknown client_id", state)
		return
	}

	// Validate redirect_uri
	if !svc.validateRedirectURI(client, redirectURI) {
		svc.authError(w, redirectURI, "invalid_request", "Invalid redirect_uri", state)
		return
	}

	// Store authorization request in pending storage and redirect to GitHub
	authRequestID := generateSecureToken(16)
	svc.storage.StoreAuthRequest(r.Context(), &PendingAuthRequest{
		ID:                  authRequestID,
		ClientID:            clientID,
		RedirectURI:         redirectURI,
		Scope:               scope,
		State:               state,
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
		Resource:            resource,
		CreatedAt:           time.Now(),
		ExpiresAt:           time.Now().Add(10 * time.Minute),
	})

	// Redirect to GitHub OAuth
	githubCallbackURL := svc.config.Issuer + "/callback"
	githubAuthURL := svc.githubClient.GetAuthorizationURL(githubCallbackURL, authRequestID)
	http.Redirect(w, r, githubAuthURL, http.StatusFound)
}

// handleGitHubCallback handles the GitHub OAuth callback
func (svc *AuthService) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state") // This is our authRequestID

	if code == "" {
		http.Error(w, "Missing authorization code", http.StatusBadRequest)
		return
	}

	// Retrieve pending auth request
	authReq, err := svc.storage.GetAuthRequest(r.Context(), state)
	if err != nil || authReq == nil {
		http.Error(w, "Invalid or expired authorization request", http.StatusBadRequest)
		return
	}

	// Delete the pending request (one-time use)
	svc.storage.DeleteAuthRequest(r.Context(), state)

	// Check if expired
	if time.Now().After(authReq.ExpiresAt) {
		svc.authError(w, authReq.RedirectURI, "access_denied",
			"Authorization request expired", authReq.State)
		return
	}

	// Exchange code for GitHub token
	githubCallbackURL := svc.config.Issuer + "/callback"
	githubToken, err := svc.githubClient.ExchangeCode(r.Context(), code, githubCallbackURL)
	if err != nil {
		svc.authError(w, authReq.RedirectURI, "server_error",
			"Failed to authenticate with GitHub", authReq.State)
		return
	}

	// Get user info from GitHub
	githubUser, err := svc.githubClient.GetUser(r.Context(), githubToken)
	if err != nil {
		svc.authError(w, authReq.RedirectURI, "server_error",
			"Failed to get user info", authReq.State)
		return
	}

	// Check authorization (allowlist)
	if !svc.isUserAuthorized(r.Context(), githubUser, githubToken) {
		svc.authError(w, authReq.RedirectURI, "access_denied",
			"User not authorized", authReq.State)
		return
	}

	// Check if user already exists
	existingUser, _ := svc.storage.GetUserByGitHubLogin(r.Context(), githubUser.Login)
	var user *User
	if existingUser != nil {
		// Update existing user
		user = existingUser
		user.Email = githubUser.Email
		user.Name = githubUser.Name
		user.AvatarURL = githubUser.AvatarURL
	} else {
		// Create new user
		user = &User{
			ID:          generateSecureToken(16),
			GitHubLogin: githubUser.Login,
			GitHubID:    githubUser.ID,
			Email:       githubUser.Email,
			Name:        githubUser.Name,
			AvatarURL:   githubUser.AvatarURL,
		}
	}
	svc.storage.StoreUser(r.Context(), user)

	// Generate authorization code
	authCode, err := svc.tokenService.GenerateAuthorizationCode(
		authReq.ClientID,
		authReq.RedirectURI,
		authReq.Scope,
		authReq.CodeChallenge,
		authReq.CodeChallengeMethod,
		authReq.Resource,
		user.ID,
	)
	if err != nil {
		svc.authError(w, authReq.RedirectURI, "server_error",
			"Failed to generate authorization code", authReq.State)
		return
	}

	// Redirect back to client with authorization code
	redirectURL, _ := url.Parse(authReq.RedirectURI)
	q := redirectURL.Query()
	q.Set("code", authCode.Code)
	if authReq.State != "" {
		q.Set("state", authReq.State)
	}
	redirectURL.RawQuery = q.Encode()

	http.Redirect(w, r, redirectURL.String(), http.StatusFound)
}

// handleToken handles the token endpoint
func (svc *AuthService) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.ParseForm()
	grantType := r.FormValue("grant_type")

	switch grantType {
	case "authorization_code":
		svc.handleAuthCodeGrant(w, r)
	case "refresh_token":
		svc.handleRefreshTokenGrant(w, r)
	default:
		svc.tokenError(w, "unsupported_grant_type",
			"Only authorization_code and refresh_token grants are supported")
	}
}

func (svc *AuthService) handleAuthCodeGrant(w http.ResponseWriter, r *http.Request) {
	code := r.FormValue("code")
	redirectURI := r.FormValue("redirect_uri")
	clientID := r.FormValue("client_id")
	codeVerifier := r.FormValue("code_verifier")
	resource := r.FormValue("resource") // RFC 8707

	// Get authorization code
	authCode, err := svc.storage.GetAuthCode(r.Context(), code)
	if err != nil || authCode == nil {
		svc.tokenError(w, "invalid_grant", "Invalid authorization code")
		return
	}

	// Delete code (one-time use)
	svc.storage.DeleteAuthCode(r.Context(), code)

	// Validate code hasn't expired
	if time.Now().After(authCode.ExpiresAt) {
		svc.tokenError(w, "invalid_grant", "Authorization code expired")
		return
	}

	// Validate client_id matches
	if authCode.ClientID != clientID {
		svc.tokenError(w, "invalid_grant", "Client ID mismatch")
		return
	}

	// Validate redirect_uri matches
	if authCode.RedirectURI != redirectURI {
		svc.tokenError(w, "invalid_grant", "Redirect URI mismatch")
		return
	}

	// Validate PKCE (MANDATORY)
	if err := ValidatePKCE(codeVerifier, authCode.CodeChallenge, authCode.CodeChallengeMethod); err != nil {
		svc.tokenError(w, "invalid_grant", err.Error())
		return
	}

	// Validate resource if provided (RFC 8707)
	if resource != "" && authCode.Resource != "" && resource != authCode.Resource {
		svc.tokenError(w, "invalid_target", "Resource mismatch")
		return
	}

	// Generate tokens
	accessToken, err := svc.tokenService.GenerateAccessToken(
		authCode.UserID, clientID, authCode.Scope, authCode.Resource)
	if err != nil {
		svc.tokenError(w, "server_error", "Failed to generate access token")
		return
	}

	refreshToken, err := svc.tokenService.GenerateRefreshToken(
		authCode.UserID, clientID, authCode.Scope, authCode.Resource)
	if err != nil {
		svc.tokenError(w, "server_error", "Failed to generate refresh token")
		return
	}

	// Return token response
	resp := TokenResponse{
		AccessToken:  accessToken.Token,
		TokenType:    "Bearer",
		ExpiresIn:    int(time.Until(accessToken.ExpiresAt).Seconds()),
		RefreshToken: refreshToken.Token,
		Scope:        accessToken.Scope,
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
}

func (svc *AuthService) handleRefreshTokenGrant(w http.ResponseWriter, r *http.Request) {
	refreshTokenStr := r.FormValue("refresh_token")
	clientID := r.FormValue("client_id")

	// Get refresh token
	refreshToken, err := svc.storage.GetRefreshToken(r.Context(), refreshTokenStr)
	if err != nil || refreshToken == nil {
		svc.tokenError(w, "invalid_grant", "Invalid refresh token")
		return
	}

	// Validate token hasn't expired
	if time.Now().After(refreshToken.ExpiresAt) {
		svc.storage.DeleteRefreshToken(r.Context(), refreshTokenStr)
		svc.tokenError(w, "invalid_grant", "Refresh token expired")
		return
	}

	// Validate client_id matches
	if refreshToken.ClientID != clientID {
		svc.tokenError(w, "invalid_grant", "Client ID mismatch")
		return
	}

	// Delete old refresh token (rotation)
	svc.storage.DeleteRefreshToken(r.Context(), refreshTokenStr)

	// Generate new tokens
	accessToken, _ := svc.tokenService.GenerateAccessToken(
		refreshToken.UserID, clientID, refreshToken.Scope, refreshToken.Resource)
	newRefreshToken, _ := svc.tokenService.GenerateRefreshToken(
		refreshToken.UserID, clientID, refreshToken.Scope, refreshToken.Resource)

	resp := TokenResponse{
		AccessToken:  accessToken.Token,
		TokenType:    "Bearer",
		ExpiresIn:    int(time.Until(accessToken.ExpiresAt).Seconds()),
		RefreshToken: newRefreshToken.Token,
		Scope:        accessToken.Scope,
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
}

// handleClientRegistration implements RFC 7591 Dynamic Client Registration
func (svc *AuthService) handleClientRegistration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ClientRegistrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		svc.registrationError(w, "invalid_client_metadata", "Invalid JSON")
		return
	}

	// Validate required fields
	if len(req.RedirectURIs) == 0 {
		svc.registrationError(w, "invalid_redirect_uri", "redirect_uris is required")
		return
	}

	// Generate client credentials
	clientID, clientSecret := GenerateClientCredentials()

	// Create client record
	client := &RegisteredClient{
		ClientID:                clientID,
		ClientSecret:            hashSecret(clientSecret),
		ClientName:              req.ClientName,
		ClientURI:               req.ClientURI,
		LogoURI:                 req.LogoURI,
		RedirectURIs:            req.RedirectURIs,
		Scopes:                  req.Scope,
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "client_secret_post",
		IsStatic:                false,
		CreatedAt:               time.Now(),
	}

	if err := svc.storage.StoreClient(r.Context(), client); err != nil {
		svc.registrationError(w, "server_error", "Failed to register client")
		return
	}

	// Return registration response with plain secret (only time it's returned)
	resp := ClientRegistrationResponse{
		ClientID:                clientID,
		ClientSecret:            clientSecret, // Plain text, only returned once
		ClientName:              client.ClientName,
		ClientURI:               client.ClientURI,
		LogoURI:                 client.LogoURI,
		RedirectURIs:            client.RedirectURIs,
		GrantTypes:              client.GrantTypes,
		ResponseTypes:           client.ResponseTypes,
		TokenEndpointAuthMethod: client.TokenEndpointAuthMethod,
		ClientIDIssuedAt:        client.CreatedAt.Unix(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

// TokenResponse is the OAuth token endpoint response
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// ClientRegistrationRequest per RFC 7591
type ClientRegistrationRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
	LogoURI                 string   `json:"logo_uri,omitempty"`
	Scope                   []string `json:"scope,omitempty"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
}

// ClientRegistrationResponse per RFC 7591
type ClientRegistrationResponse struct {
	ClientID                string   `json:"client_id"`
	ClientSecret            string   `json:"client_secret,omitempty"`
	ClientName              string   `json:"client_name,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
	LogoURI                 string   `json:"logo_uri,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at,omitempty"`
}
