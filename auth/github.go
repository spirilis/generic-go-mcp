package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spirilis/generic-go-mcp/config"
	"github.com/spirilis/generic-go-mcp/logging"
)

// The github.com endpoints. A GitHub Enterprise Server provider uses its own (baseURL and
// apiURL in config).
const (
	GitHubAuthorizeURL = "https://github.com/login/oauth/authorize"
	GitHubTokenURL     = "https://github.com/login/oauth/access_token"
	GitHubAPIURL       = "https://api.github.com"
)

// githubMaxPages bounds how many pages of organizations or teams a login reads (100 each).
const githubMaxPages = 10

// GitHubClient handles GitHub OAuth and API interactions
type GitHubClient struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client

	authorizeURL, tokenURL, apiURL string
}

// NewGitHubClient creates a new GitHub client for github.com.
func NewGitHubClient(cfg config.GitHubConfig) *GitHubClient {
	clientID := cfg.ClientID
	clientSecret := cfg.ClientSecret

	// Load from files if specified (for mounted secrets). NewAuthService reads them through
	// config.AuthConfig.Resolve, which fails startup on an unreadable file; this constructor
	// cannot return an error, so a direct caller at least gets the failure logged.
	if cfg.ClientIDFile != "" {
		id, err := config.ReadSecretFile(cfg.ClientIDFile)
		if err != nil {
			logging.Error("GitHub clientIdFile could not be read", "error", err)
		}
		clientID = id
	}
	if cfg.ClientSecretFile != "" {
		secret, err := config.ReadSecretFile(cfg.ClientSecretFile)
		if err != nil {
			logging.Error("GitHub clientSecretFile could not be read", "error", err)
		}
		clientSecret = secret
	}

	return &GitHubClient{
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirects},
		authorizeURL: GitHubAuthorizeURL,
		tokenURL:     GitHubTokenURL,
		apiURL:       GitHubAPIURL,
	}
}

// GetAuthorizationURL returns the GitHub OAuth authorization URL
func (gc *GitHubClient) GetAuthorizationURL(callbackURL, state string) string {
	params := url.Values{
		"client_id":    {gc.clientID},
		"redirect_uri": {callbackURL},
		"scope":        {"read:user read:org"},
		"state":        {state},
	}
	return gc.authorizeURL + "?" + params.Encode()
}

// GitHubTokenResponse represents GitHub's token response
type GitHubTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	Error       string `json:"error,omitempty"`
	ErrorDesc   string `json:"error_description,omitempty"`
}

// ExchangeCode exchanges an authorization code for a GitHub access token
func (gc *GitHubClient) ExchangeCode(ctx context.Context, code, callbackURL string) (string, error) {
	data := url.Values{
		"client_id":     {gc.clientID},
		"client_secret": {gc.clientSecret},
		"code":          {code},
		"redirect_uri":  {callbackURL},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gc.tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := gc.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to exchange code: %w", err)
	}
	defer resp.Body.Close()

	body, err := readCapped(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read token response: %w", err)
	}
	var tokenResp GitHubTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("failed to parse token response (HTTP %d): %w", resp.StatusCode, err)
	}
	if tokenResp.Error != "" {
		return "", fmt.Errorf("GitHub error: %s - %s", tokenResp.Error, tokenResp.ErrorDesc)
	}
	if resp.StatusCode != http.StatusOK || tokenResp.AccessToken == "" {
		return "", fmt.Errorf("GitHub token endpoint answered HTTP %d without a token", resp.StatusCode)
	}
	return tokenResp.AccessToken, nil
}

// GitHubUser represents a GitHub user
type GitHubUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// apiGet GETs an API URL and returns the body of a 200 response and the next page's URL, if
// any. Any other status is an error: a 401 or 403 body must not be decoded as an empty list.
func (gc *GitHubClient) apiGet(ctx context.Context, token, rawURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := gc.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := readCapped(resp.Body)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		if len(body) > 200 {
			body = body[:200]
		}
		return nil, "", fmt.Errorf("GitHub API %s: HTTP %d: %s", req.URL.Path, resp.StatusCode, body)
	}
	return body, gc.nextPage(resp.Header.Get("Link")), nil
}

// nextPage returns the rel="next" URL of a Link header, if it points back into this API (the
// token goes with it).
func (gc *GitHubClient) nextPage(link string) string {
	for _, part := range strings.Split(link, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok || !strings.Contains(params, `rel="next"`) {
			continue
		}
		next := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(target), "<"), ">")
		if strings.HasPrefix(next, gc.apiURL+"/") {
			return next
		}
	}
	return ""
}

// GetUser fetches the authenticated user's info from GitHub
func (gc *GitHubClient) GetUser(ctx context.Context, token string) (*GitHubUser, error) {
	body, _, err := gc.apiGet(ctx, token, gc.apiURL+"/user")
	if err != nil {
		return nil, err
	}
	var user GitHubUser
	if err := json.Unmarshal(body, &user); err != nil {
		return nil, err
	}
	if user.Login == "" || user.ID == 0 {
		return nil, fmt.Errorf("GitHub API /user: no login or id in the response")
	}
	return &user, nil
}

// getPaged reads every page (up to githubMaxPages) of a list endpoint.
func getPaged[T any](ctx context.Context, gc *GitHubClient, token, path string) ([]T, error) {
	var all []T
	next := gc.apiURL + path
	for page := 0; next != ""; page++ {
		if page == githubMaxPages {
			logging.Warn("GitHub list truncated", "path", path, "items", len(all))
			break
		}
		body, n, err := gc.apiGet(ctx, token, next)
		if err != nil {
			return nil, err
		}
		var items []T
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("GitHub API %s: %w", path, err)
		}
		all = append(all, items...)
		next = n
	}
	return all, nil
}

// GitHubOrg represents a GitHub organization
type GitHubOrg struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// GetUserOrgs fetches the user's organization memberships
func (gc *GitHubClient) GetUserOrgs(ctx context.Context, token string) ([]GitHubOrg, error) {
	return getPaged[GitHubOrg](ctx, gc, token, "/user/orgs?per_page=100")
}

// GitHubTeam represents a GitHub team
type GitHubTeam struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Slug         string    `json:"slug"`
	Organization GitHubOrg `json:"organization"`
}

// GetUserTeams fetches the user's team memberships
func (gc *GitHubClient) GetUserTeams(ctx context.Context, token string) ([]GitHubTeam, error) {
	return getPaged[GitHubTeam](ctx, gc, token, "/user/teams?per_page=100")
}

// githubProvider signs users in with a GitHub OAuth App, on github.com or a GitHub Enterprise
// Server.
//
// It sends no PKCE challenge or nonce: GitHub has no id_token to carry a nonce, and older
// Enterprise Server releases do not support PKCE. The code is bound to this server by the
// client secret, which never leaves it.
type githubProvider struct {
	name, displayName, issuer string
	client                    *GitHubClient
}

func newGitHubProvider(p config.ProviderConfig) *githubProvider {
	gc := NewGitHubClient(config.GitHubConfig{ClientID: p.ClientID, ClientSecret: p.ClientSecret})
	gc.authorizeURL = p.BaseURL + "/login/oauth/authorize"
	gc.tokenURL = p.BaseURL + "/login/oauth/access_token"
	gc.apiURL = p.APIURL
	return &githubProvider{name: p.Name, displayName: p.DisplayName, issuer: p.BaseURL, client: gc}
}

func (g *githubProvider) Name() string        { return g.name }
func (g *githubProvider) DisplayName() string { return g.displayName }
func (g *githubProvider) Type() string        { return config.ProviderTypeGitHub }

func (g *githubProvider) AuthCodeURL(_ context.Context, callback, state, _, _ string) (string, error) {
	return g.client.GetAuthorizationURL(callback, state), nil
}

// Exchange redeems the code and reads the user, their organizations and their teams.
//
// Organizations and teams are always fetched, not only when a rule names them: the snapshot
// is evaluated against the rules as they are on each later request, and a rule added
// afterwards must find them there. A failed lookup is logged and leaves those groups out, as
// the allowlist always did: an org or team rule then does not match this login, while a user
// admitted by login still gets in.
func (g *githubProvider) Exchange(ctx context.Context, code, callback, _, _ string) (Identity, error) {
	token, err := g.client.ExchangeCode(ctx, code, callback)
	if err != nil {
		return Identity{}, err
	}
	ghUser, err := g.client.GetUser(ctx, token)
	if err != nil {
		return Identity{}, err
	}
	id := Identity{
		Provider: g.name,
		Issuer:   g.issuer,
		Subject:  strconv.FormatInt(ghUser.ID, 10),
		Username: ghUser.Login,
		Email:    ghUser.Email,
		// The profile email is whatever the user chose to show, and GitHub does not say
		// whether it is verified here, so email rules never match a GitHub user.
		EmailVerified: false,
		Name:          ghUser.Name,
		AvatarURL:     ghUser.AvatarURL,
		Groups:        []string{},
	}
	orgs, err := g.client.GetUserOrgs(ctx, token)
	if err != nil {
		logging.Warn("GitHub organizations lookup failed; org rules will not match this login",
			"provider", g.name, "login", ghUser.Login, "error", err)
	}
	for _, org := range orgs {
		id.Groups = append(id.Groups, org.Login)
	}
	teams, err := g.client.GetUserTeams(ctx, token)
	if err != nil {
		logging.Warn("GitHub teams lookup failed; team rules will not match this login",
			"provider", g.name, "login", ghUser.Login, "error", err)
	}
	for _, team := range teams {
		id.Groups = append(id.Groups, team.Organization.Login+"/"+team.Slug)
	}
	return id, nil
}
