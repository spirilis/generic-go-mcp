package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spirilis/generic-go-mcp/config"
)

const (
	GitHubAuthorizeURL = "https://github.com/login/oauth/authorize"
	GitHubTokenURL     = "https://github.com/login/oauth/access_token"
	GitHubAPIURL       = "https://api.github.com"
)

// GitHubClient handles GitHub OAuth and API interactions
type GitHubClient struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client
}

// NewGitHubClient creates a new GitHub client
func NewGitHubClient(cfg config.GitHubConfig) *GitHubClient {
	clientID := cfg.ClientID
	clientSecret := cfg.ClientSecret

	// Load from files if specified (for mounted secrets)
	if cfg.ClientIDFile != "" {
		data, _ := os.ReadFile(cfg.ClientIDFile)
		clientID = strings.TrimSpace(string(data))
	}
	if cfg.ClientSecretFile != "" {
		data, _ := os.ReadFile(cfg.ClientSecretFile)
		clientSecret = strings.TrimSpace(string(data))
	}

	return &GitHubClient{
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
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
	return GitHubAuthorizeURL + "?" + params.Encode()
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

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, GitHubTokenURL,
		strings.NewReader(data.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := gc.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to exchange code: %w", err)
	}
	defer resp.Body.Close()

	var tokenResp GitHubTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("failed to parse token response: %w", err)
	}

	if tokenResp.Error != "" {
		return "", fmt.Errorf("GitHub error: %s - %s", tokenResp.Error, tokenResp.ErrorDesc)
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

// GetUser fetches the authenticated user's info from GitHub
func (gc *GitHubClient) GetUser(ctx context.Context, token string) (*GitHubUser, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, GitHubAPIURL+"/user", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := gc.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API error: %s", body)
	}

	var user GitHubUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, err
	}

	return &user, nil
}

// GitHubOrg represents a GitHub organization
type GitHubOrg struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// GetUserOrgs fetches the user's organization memberships
func (gc *GitHubClient) GetUserOrgs(ctx context.Context, token string) ([]GitHubOrg, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, GitHubAPIURL+"/user/orgs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := gc.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var orgs []GitHubOrg
	if err := json.NewDecoder(resp.Body).Decode(&orgs); err != nil {
		return nil, err
	}

	return orgs, nil
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
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, GitHubAPIURL+"/user/teams", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := gc.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var teams []GitHubTeam
	if err := json.NewDecoder(resp.Body).Decode(&teams); err != nil {
		return nil, err
	}

	return teams, nil
}

// isUserAuthorized checks if the user matches the allowlist
func (svc *AuthService) isUserAuthorized(ctx context.Context, ghUser *GitHubUser, token string) bool {
	allowlist := svc.config.Allowlist

	// If no allowlist configured, allow all authenticated users
	if len(allowlist.Users) == 0 && len(allowlist.Orgs) == 0 && len(allowlist.Teams) == 0 {
		return true
	}

	// Check username allowlist
	for _, user := range allowlist.Users {
		if strings.EqualFold(user, ghUser.Login) {
			return true
		}
	}

	// Check organization membership
	if len(allowlist.Orgs) > 0 || len(allowlist.Teams) > 0 {
		orgs, err := svc.githubClient.GetUserOrgs(ctx, token)
		if err == nil {
			for _, org := range orgs {
				for _, allowedOrg := range allowlist.Orgs {
					if strings.EqualFold(allowedOrg, org.Login) {
						return true
					}
				}
			}
		}

		// Check team membership
		if len(allowlist.Teams) > 0 {
			teams, err := svc.githubClient.GetUserTeams(ctx, token)
			if err == nil {
				for _, team := range teams {
					for _, allowedTeam := range allowlist.Teams {
						if strings.EqualFold(allowedTeam.Org, team.Organization.Login) &&
							strings.EqualFold(allowedTeam.Team, team.Slug) {
							return true
						}
					}
				}
			}
		}
	}

	return false
}
