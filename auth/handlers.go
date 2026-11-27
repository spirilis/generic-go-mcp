package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spirilis/generic-go-mcp/logging"
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

	// Sign-in provider chooser (see chooser.go), and every provider's callback
	mux.HandleFunc("/authorize/provider", svc.handleChooseProvider)
	mux.HandleFunc("/callback", svc.handleCallback)

	// Consent screen submission (see consent.go)
	mux.HandleFunc("/consent", svc.handleConsent)
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
		r.Body = http.MaxBytesReader(w, r.Body, maxOAuthBodyBytes)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid or oversized request", http.StatusBadRequest)
			return
		}
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

	// The request is stored until the provider's callback, and anyone can send one.
	if problem := authorizeProblem(state, scope, resource, codeChallenge, codeChallengeMethod, redirectURI); problem != "" {
		http.Error(w, "Invalid request: "+problem, http.StatusBadRequest)
		return
	}

	// Validate the client and redirect_uri FIRST, and report their failures directly rather
	// than by redirecting. A redirect target is only trustworthy once it has been matched
	// against the client's registered set; redirecting an error to an unvalidated URI would
	// be an open redirect that leaks the error to an attacker-controlled destination.
	client, err := svc.storage.GetClient(r.Context(), clientID)
	if err != nil || client == nil {
		svc.authErrorDirect(w, "invalid_client", "Unknown client_id")
		return
	}
	if !svc.validateRedirectURI(client, redirectURI) {
		svc.authErrorDirect(w, "invalid_request", "Invalid redirect_uri")
		return
	}

	// redirect_uri is now trusted: subsequent errors are delivered to it per OAuth 2.1.
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

	svc.startSignIn(w, r, &PendingAuthRequest{
		ID:                  generateSecureToken(16),
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
}

// handleCallback receives every provider's redirect back (the path registered in live GitHub
// OAuth Apps, so it never changes). The pending request, found by state, says which provider
// the browser was sent to.
func (svc *AuthService) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := q.Get("code")
	state := q.Get("state") // This is our authRequestID
	upstreamErr := q.Get("error")

	if code == "" && upstreamErr == "" {
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

	prov := svc.callbackProvider(authReq)
	if prov == nil {
		// The browser never left the chooser (or the provider was removed from config): no
		// code from anywhere can belong to this request.
		http.Error(w, "This sign-in has no identity provider; start it again", http.StatusBadRequest)
		return
	}
	if upstreamErr != "" {
		// The user cancelled, or the IdP refused them. Its description is not relayed: it is
		// whatever the IdP (or whoever crafted this URL) put there.
		logging.Info("Sign-in refused at the identity provider", "provider", prov.Name(), "error", upstreamErr)
		svc.authError(w, authReq.RedirectURI, "access_denied",
			"Sign-in was cancelled or refused at "+prov.DisplayName(), authReq.State)
		return
	}

	identity, err := prov.Exchange(r.Context(), code, svc.callbackURL(), authReq.UpstreamVerifier, authReq.Nonce)
	if err != nil {
		logging.Warn("Sign-in failed at the identity provider", "provider", prov.Name(), "error", err)
		errCode := "server_error"
		if errors.Is(err, ErrProviderUnavailable) {
			errCode = "temporarily_unavailable"
		}
		svc.authError(w, authReq.RedirectURI, errCode,
			"Failed to authenticate with "+prov.DisplayName(), authReq.State)
		return
	}

	// Check authorization against the access rules (the legacy allowlist is one of them)
	if _, ok := svc.access.evaluate(identity); !ok {
		logging.Info("Sign-in refused: no access rule matches",
			"provider", identity.Provider, "username", identity.Username, "subject", identity.Subject)
		svc.authError(w, authReq.RedirectURI, "access_denied",
			"User not authorized", authReq.State)
		return
	}

	user, err := svc.userForIdentity(r.Context(), identity)
	if err == nil {
		err = svc.storage.StoreUser(r.Context(), user)
	}
	if err != nil {
		// Without the snapshot every request would be judged on stale identity.
		logging.Error("callback: storing the user failed", "error", err)
		svc.authError(w, authReq.RedirectURI, "server_error", "Failed to store user", authReq.State)
		return
	}

	// Straight back to the client if it is pre-registered or already approved by this user;
	// otherwise via the consent screen.
	svc.grantOrAskConsent(w, r, authReq, user)
}

// handleToken handles the token endpoint
func (svc *AuthService) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxOAuthBodyBytes)
	if err := r.ParseForm(); err != nil {
		svc.tokenError(w, "invalid_request", "Invalid or oversized request")
		return
	}
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
	clientSecret := r.FormValue("client_secret")
	codeVerifier := r.FormValue("code_verifier")
	resource := r.FormValue("resource") // RFC 8707

	// Authenticate the client. A client registered with a secret (confidential,
	// client_secret_post) MUST present the matching secret; a public client — one with no
	// stored secret — relies on PKCE alone.
	if rerr := svc.authenticateClient(r.Context(), clientID, clientSecret); rerr != "" {
		svc.tokenError(w, "invalid_client", rerr)
		return
	}

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
	clientSecret := r.FormValue("client_secret")

	// Authenticate the client before honoring the refresh (see handleAuthCodeGrant).
	if rerr := svc.authenticateClient(r.Context(), clientID, clientSecret); rerr != "" {
		svc.tokenError(w, "invalid_client", rerr)
		return
	}

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

	// The rules may have changed since the login, and Middleware would refuse the new access
	// token anyway; invalid_grant sends the client back to /authorize, where a fresh login
	// re-reads the user's identity.
	user, err := svc.storage.GetUser(r.Context(), refreshToken.UserID)
	if err != nil && !errors.Is(err, ErrUserNotFound) {
		logging.Error("refresh grant: reading the user failed", "error", err)
		svc.tokenError(w, "server_error", "Failed to issue tokens")
		return
	}
	if user == nil || err != nil {
		svc.tokenError(w, "invalid_grant", "Unknown user")
		return
	}
	if _, ok := svc.authorizeUser(user); !ok {
		svc.tokenError(w, "invalid_grant", "User is no longer authorized; sign in again")
		return
	}
	// Groups are snapshotted at the browser login, so a refresh is bounded by how old that
	// snapshot may get: past identityMaxAge the client must send the user through the IdP
	// again, which picks up group changes and accounts disabled there. A record from before
	// v0.12.0 has no login time; its age counts from the chain's first refresh after the
	// upgrade, or it could refresh forever and never take a snapshot.
	identityAt, legacyAgeFrom := user.LastLoginAt, time.Time{}
	if identityAt.IsZero() {
		legacyAgeFrom = refreshToken.LegacyAgeFrom
		if legacyAgeFrom.IsZero() {
			legacyAgeFrom = time.Now().UTC()
		}
		identityAt = legacyAgeFrom
	}
	if time.Since(identityAt) > svc.maxIdentityAge() {
		svc.tokenError(w, "invalid_grant", "The sign-in is too old to refresh; sign in again")
		return
	}

	// Generate the new tokens first: if the store cannot take them (a full disk, say), the
	// old refresh token must survive so the client can try again.
	accessToken, err := svc.tokenService.GenerateAccessToken(
		refreshToken.UserID, clientID, refreshToken.Scope, refreshToken.Resource)
	if err != nil {
		logging.Error("refresh grant: storing the access token failed", "error", err)
		svc.tokenError(w, "server_error", "Failed to issue tokens")
		return
	}
	newRefreshToken, err := svc.tokenService.generateRefreshToken(
		refreshToken.UserID, clientID, refreshToken.Scope, refreshToken.Resource, legacyAgeFrom)
	if err != nil {
		logging.Error("refresh grant: storing the refresh token failed", "error", err)
		svc.tokenError(w, "server_error", "Failed to issue tokens")
		return
	}

	// Delete old refresh token (rotation)
	svc.storage.DeleteRefreshToken(r.Context(), refreshTokenStr)

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

	// Anyone can reach this endpoint: bound what it reads and what it keeps.
	r.Body = http.MaxBytesReader(w, r.Body, maxOAuthBodyBytes)
	var req ClientRegistrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		svc.registrationError(w, "invalid_client_metadata", "Invalid client metadata: "+err.Error())
		return
	}
	if problem := registrationProblem(&req); problem != "" {
		svc.registrationError(w, "invalid_client_metadata", problem)
		return
	}
	if n, err := svc.dynamicClientCount(r.Context()); err != nil || n >= maxDynamicClients {
		svc.registrationError(w, "server_error", "Too many registered clients; try again later")
		return
	}

	// Validate required fields
	if len(req.RedirectURIs) == 0 {
		svc.registrationError(w, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, uri := range req.RedirectURIs {
		if problem := svc.checkRegisteredRedirectURI(uri); problem != "" {
			svc.registrationError(w, "invalid_redirect_uri", problem)
			return
		}
	}

	// Generate client credentials. A client asking for token_endpoint_auth_method "none" (which the
	// metadata advertises) is a public client: no secret, PKCE alone, as /authorize requires PKCE of
	// everyone and /token treats a client without a stored secret as public. Any other request gets
	// client_secret_post, the one secret-based method /token accepts.
	clientID, clientSecret := GenerateClientCredentials()
	authMethod, storedSecret := "client_secret_post", hashSecret(clientSecret)
	if req.TokenEndpointAuthMethod == "none" {
		authMethod, clientSecret, storedSecret = "none", "", ""
	}

	// Create client record
	client := &RegisteredClient{
		ClientID:                clientID,
		ClientSecret:            storedSecret,
		ClientName:              req.ClientName,
		ClientURI:               req.ClientURI,
		LogoURI:                 req.LogoURI,
		RedirectURIs:            req.RedirectURIs,
		Scopes:                  []string(req.Scope),
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: authMethod,
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
		Scope:                   ScopeList(client.Scopes),
		ClientIDIssuedAt:        client.CreatedAt.Unix(),
	}
	if clientSecret != "" {
		never := int64(0)
		resp.ClientSecretExpiresAt = &never
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

// checkRegisteredRedirectURI vets a redirect_uri offered at /register, returning a description
// of the problem or "" if it is acceptable.
//
// Always enforced: an absolute URI with no fragment (RFC 6749 §3.1.2), and none of the schemes
// that execute or read locally instead of navigating. Custom schemes stay allowed, since native
// apps use them (RFC 8252). The host allowlist applies only when configured.
func (svc *AuthService) checkRegisteredRedirectURI(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return fmt.Sprintf("redirect_uri %q is not an absolute URI", raw)
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return fmt.Sprintf("redirect_uri %q must not contain a fragment", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "javascript", "data", "vbscript", "file", "blob", "about":
		return fmt.Sprintf("redirect_uri scheme %q is not permitted", u.Scheme)
	}
	if len(svc.allowedRedirectHosts) > 0 && !svc.allowedRedirectHosts[strings.ToLower(u.Hostname())] {
		return fmt.Sprintf("redirect_uri host %q is not permitted by this server's registration policy",
			u.Hostname())
	}
	return ""
}

// TokenResponse is the OAuth token endpoint response
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// ScopeList is a client's registered scope. RFC 7591 §2 defines the "scope" metadata as one
// string of space-separated values ("mcp:tools mcp:resources"), which is what MCP clients such as
// Claude send; a JSON array is accepted too, for callers written against the earlier []string
// field. It is written back in the RFC form.
type ScopeList []string

// UnmarshalJSON implements json.Unmarshaler: a space-separated string or an array of strings.
func (s *ScopeList) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		*s = strings.Fields(str)
		return nil
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("scope must be a string of space-separated values")
	}
	*s = list
	return nil
}

// MarshalJSON implements json.Marshaler: one space-separated string (RFC 7591 §2).
func (s ScopeList) MarshalJSON() ([]byte, error) { return json.Marshal(strings.Join(s, " ")) }

// ClientRegistrationRequest per RFC 7591
type ClientRegistrationRequest struct {
	RedirectURIs            []string  `json:"redirect_uris"`
	ClientName              string    `json:"client_name,omitempty"`
	ClientURI               string    `json:"client_uri,omitempty"`
	LogoURI                 string    `json:"logo_uri,omitempty"`
	Scope                   ScopeList `json:"scope,omitempty"`
	GrantTypes              []string  `json:"grant_types,omitempty"`
	ResponseTypes           []string  `json:"response_types,omitempty"`
	TokenEndpointAuthMethod string    `json:"token_endpoint_auth_method,omitempty"`
}

// ClientRegistrationResponse per RFC 7591
type ClientRegistrationResponse struct {
	ClientID                string    `json:"client_id"`
	ClientSecret            string    `json:"client_secret,omitempty"`
	ClientName              string    `json:"client_name,omitempty"`
	ClientURI               string    `json:"client_uri,omitempty"`
	LogoURI                 string    `json:"logo_uri,omitempty"`
	RedirectURIs            []string  `json:"redirect_uris"`
	GrantTypes              []string  `json:"grant_types,omitempty"`
	ResponseTypes           []string  `json:"response_types,omitempty"`
	TokenEndpointAuthMethod string    `json:"token_endpoint_auth_method,omitempty"`
	Scope                   ScopeList `json:"scope,omitempty"`
	ClientIDIssuedAt        int64     `json:"client_id_issued_at,omitempty"`
	// ClientSecretExpiresAt is required whenever a secret is issued (RFC 7591 §3.2.1); 0 = never.
	ClientSecretExpiresAt *int64 `json:"client_secret_expires_at,omitempty"`
}
