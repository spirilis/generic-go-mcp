package auth

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/spirilis/generic-go-mcp/config"
	"github.com/spirilis/generic-go-mcp/logging"
)

// The browser leg of an authorization: /authorize sends the user to their identity provider,
// directly when only one is configured and through a chooser page otherwise; /callback
// (handlers.go) takes them back.

// callbackURL is the redirect URI registered at every provider.
func (svc *AuthService) callbackURL() string { return svc.config.Issuer + "/callback" }

// startSignIn continues a validated /authorize request: straight to the only provider, or to
// the chooser.
func (svc *AuthService) startSignIn(w http.ResponseWriter, r *http.Request, pending *PendingAuthRequest) {
	switch len(svc.providers) {
	case 0:
		http.Error(w, "No sign-in provider is configured", http.StatusServiceUnavailable)
		return
	case 1:
		svc.redirectToProvider(w, r, pending, svc.providers[0])
		return
	}

	if err := svc.storage.StoreAuthRequest(r.Context(), pending); err != nil {
		http.Error(w, "Could not start authorization; try again later", http.StatusServiceUnavailable)
		return
	}
	view := chooserView{Issuer: svc.config.Issuer, Action: svc.config.Issuer + "/authorize/provider", RequestID: pending.ID}
	for _, p := range svc.providers {
		view.Providers = append(view.Providers, chooserProvider{Name: p.Name(), DisplayName: p.DisplayName()})
	}
	setPageHeaders(w)
	w.WriteHeader(http.StatusOK)
	_ = chooserTemplate.Execute(w, view)
}

// redirectToProvider binds pending to prov (with a fresh nonce and PKCE verifier), stores it,
// and sends the browser to the provider. A provider that is not ready answers 503 before
// anything is stored, so the user can simply try again.
func (svc *AuthService) redirectToProvider(w http.ResponseWriter, r *http.Request, pending *PendingAuthRequest, prov Provider) {
	pending.Provider = prov.Name()
	pending.Nonce = generateSecureToken(32)
	pending.UpstreamVerifier = generateSecureToken(32) // 43 base64url characters: a valid PKCE verifier

	target, err := prov.AuthCodeURL(r.Context(), svc.callbackURL(), pending.ID, pending.Nonce, s256(pending.UpstreamVerifier))
	if err != nil {
		logging.Warn("Sign-in provider unavailable", "provider", prov.Name(), "error", err)
		w.Header().Set("Retry-After", "5")
		http.Error(w, "Sign-in with "+prov.DisplayName()+" is unavailable right now; try again shortly",
			http.StatusServiceUnavailable)
		return
	}
	if err := svc.storage.StoreAuthRequest(r.Context(), pending); err != nil {
		// Sending the user to the IdP now would only fail at the callback, with a misleading error.
		http.Error(w, "Could not start authorization; try again later", http.StatusServiceUnavailable)
		return
	}

	// After the chooser's POST, 303 makes the browser follow with a GET.
	status := http.StatusFound
	if r.Method == http.MethodPost {
		status = http.StatusSeeOther
	}
	http.Redirect(w, r, target, status)
}

// handleChooseProvider receives the chooser's choice.
//
// The chooser grants nothing: it only picks where the browser signs in for a request /authorize
// already validated. Choosing again (the back button) re-binds the request to a fresh nonce
// and verifier, which voids the earlier redirect.
func (svc *AuthService) handleChooseProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}
	prov, ok := svc.providerByName[r.PostForm.Get("provider")]
	if !ok {
		http.Error(w, "Unknown sign-in provider", http.StatusBadRequest)
		return
	}
	id := r.PostForm.Get("request")
	if id == "" || len(id) > 64 {
		http.Error(w, "This sign-in request is invalid or has expired. Start the sign-in again.", http.StatusBadRequest)
		return
	}
	pending, err := svc.storage.GetAuthRequest(r.Context(), id)
	if err != nil || pending == nil || time.Now().After(pending.ExpiresAt) {
		http.Error(w, "This sign-in request is invalid or has expired. Start the sign-in again.", http.StatusBadRequest)
		return
	}
	svc.redirectToProvider(w, r, pending, prov)
}

// callbackProvider is the provider a pending request was sent to. A request stored before
// v0.12.0 names none; it went to GitHub, the only provider there was, so it is accepted only
// while there is still exactly one. With several, an unnamed request is one that never left
// the chooser, and no code can belong to it.
func (svc *AuthService) callbackProvider(pending *PendingAuthRequest) Provider {
	if pending.Provider != "" {
		return svc.providerByName[pending.Provider]
	}
	if len(svc.providers) == 1 {
		return svc.providers[0]
	}
	return nil
}

// userForIdentity returns the stored user a login's identity belongs to, updated with the new
// snapshot, or a new user. It does not store it.
//
// Users are found by (provider, issuer, subject) only: never by email or username, which an IdP
// may reassign. The one exception is the users stored before v0.12.0, who are indexed by GitHub
// login alone: a GitHub login falls back to that index, and takes the record only if it is a
// GitHub record of the same provider and the same numeric GitHub id. A login can be renamed and
// then claimed by someone else, who must not inherit the old record and its consents.
func (svc *AuthService) userForIdentity(ctx context.Context, id Identity) (*User, error) {
	user, err := svc.storage.GetUserByIdentity(ctx, id.Provider, id.Issuer, id.Subject)
	if err != nil && !errors.Is(err, ErrUserNotFound) {
		return nil, err
	}
	github := svc.access.providers[id.Provider].Type == config.ProviderTypeGitHub
	if user == nil && github && id.Provider == config.LegacyGitHubProvider {
		legacy, err := svc.storage.GetUserByGitHubLogin(ctx, id.Username)
		if err != nil && !errors.Is(err, ErrUserNotFound) {
			return nil, err
		}
		if legacy != nil {
			prev := identityFromUser(legacy)
			if prev.Provider == id.Provider && prev.Issuer == id.Issuer &&
				(legacy.GitHubID == 0 || strconv.FormatInt(legacy.GitHubID, 10) == id.Subject) {
				user = legacy
			}
		}
	}
	if user == nil {
		user = &User{ID: generateSecureToken(16)}
	}

	if github {
		user.GitHubID, _ = strconv.ParseInt(id.Subject, 10, 64)
		// The login index (and the GitHubLogin field embedders read) belongs to the provider
		// named "github", as it always has; another GitHub provider (an Enterprise Server) is
		// a separate user space whose logins may collide with github.com's.
		if id.Provider == config.LegacyGitHubProvider {
			user.GitHubLogin = id.Username
		}
	}
	setIdentitySnapshot(user, id, time.Now())
	return user, nil
}

// maxIdentityAge is how long after a browser login the refresh grant keeps working.
func (svc *AuthService) maxIdentityAge() time.Duration {
	if svc.identityMaxAge > 0 {
		return svc.identityMaxAge
	}
	return RefreshTokenTTL
}

// setPageHeaders sets the headers of the library's HTML pages (the chooser and the consent
// screen); see renderConsent for why each one is there.
func setPageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
}

type chooserProvider struct{ Name, DisplayName string }

type chooserView struct {
	Issuer    string
	Action    string
	RequestID string
	Providers []chooserProvider
}

var chooserTemplate = template.Must(template.New("chooser").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in</title>
<style>
  body { font-family: system-ui, sans-serif; background: #f6f7f9; color: #1c1e21; margin: 0; }
  main { max-width: 32rem; margin: 3rem auto; background: #fff; padding: 2rem; border-radius: 8px;
         box-shadow: 0 1px 3px rgba(0,0,0,.12); }
  h1 { font-size: 1.3rem; margin-top: 0; }
  .providers { display: flex; flex-direction: column; gap: .75rem; margin-top: 1.5rem; }
  button { font-size: 1rem; padding: .7rem 1.4rem; border-radius: 6px; border: 1px solid #ccd0d5;
           background: #fff; cursor: pointer; text-align: left; }
  button:hover { border-color: #1a7f37; }
  @media (prefers-color-scheme: dark) {
    body { background: #18191a; color: #e4e6eb; }
    main { background: #242526; box-shadow: none; }
    button { background: #3a3b3c; color: #e4e6eb; border-color: #4e4f50; }
  }
</style>
</head>
<body>
<main>
  <h1>Sign in to {{.Issuer}}</h1>
  <p>Choose how to sign in.</p>
  <form method="post" action="{{.Action}}">
    <input type="hidden" name="request" value="{{.RequestID}}">
    <div class="providers">
      {{range .Providers}}<button type="submit" name="provider" value="{{.Name}}">Continue with {{.DisplayName}}</button>
      {{end}}
    </div>
  </form>
</main>
</body>
</html>
`))
