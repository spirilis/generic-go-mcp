package auth

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ConsentTTL bounds how long a consent screen stays answerable.
const ConsentTTL = 5 * time.Minute

// Why there is a consent screen at all.
//
// Registration at /register is open (RFC 7591), and GitHub skips its own approval prompt for an
// OAuth app the user has already authorized. Without a screen here, an attacker could register a
// client whose redirect_uri is their own host, send an allowlisted user to /authorize, and have
// that user's browser deliver a working authorization code to the attacker — with no click beyond
// following a link. The MCP security guidance calls this the confused-deputy problem, and
// requires per-client user consent from a server that fronts a third-party identity provider
// under one static client ID, which is exactly what this package does with GitHub.
//
// So before a client that the operator did not pre-register first receives a code for a user,
// the user is shown who is asking and where the code will go, and must approve it. The approval
// is remembered per (user, client). Dynamically registered clients get fresh IDs, so an attacker
// cannot inherit consent that was granted to a legitimate client.

// grantOrAskConsent finishes an authorization that has passed GitHub login and the allowlist:
// straight to the client for a config-registered client or one the user already approved,
// otherwise via the consent screen.
func (svc *AuthService) grantOrAskConsent(w http.ResponseWriter, r *http.Request, authReq *PendingAuthRequest, user *User) {
	if svc.trustedClients[authReq.ClientID] {
		svc.completeAuthorization(w, r, authReq, user.ID)
		return
	}

	granted, err := svc.storage.HasConsent(r.Context(), user.ID, authReq.ClientID)
	if err != nil {
		svc.authError(w, authReq.RedirectURI, "server_error", "Failed to read consent", authReq.State)
		return
	}
	if granted {
		svc.completeAuthorization(w, r, authReq, user.ID)
		return
	}

	client, err := svc.storage.GetClient(r.Context(), authReq.ClientID)
	if err != nil || client == nil {
		// Deleted between /authorize and now.
		svc.authError(w, authReq.RedirectURI, "invalid_client", "Unknown client_id", authReq.State)
		return
	}

	binding := generateSecureToken(32)
	pc := &PendingConsent{
		ID:          generateSecureToken(32),
		BindingHash: hashSecret(binding),
		UserID:      user.ID,
		Request:     *authReq,
		ExpiresAt:   time.Now().Add(ConsentTTL),
	}
	if err := svc.storage.StorePendingConsent(r.Context(), pc); err != nil {
		svc.authError(w, authReq.RedirectURI, "server_error", "Failed to start consent", authReq.State)
		return
	}

	name, secure := svc.consentCookie()
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    binding,
		Path:     "/",
		MaxAge:   int(ConsentTTL.Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})

	clientName := client.ClientName
	if clientName == "" {
		clientName = "An unnamed application"
	}
	svc.renderConsent(w, consentView{
		ClientName:     clientName,
		SelfRegistered: !client.IsStatic,
		Destination:    redirectDestination(authReq.RedirectURI),
		Login:          user.GitHubLogin,
		Issuer:         svc.config.Issuer,
		Action:         svc.config.Issuer + "/consent",
		ConsentID:      pc.ID,
	})
}

// handleConsent receives the consent screen's Approve or Deny.
func (svc *AuthService) handleConsent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	name, secure := svc.consentCookie()
	cookie, cookieErr := r.Cookie(name)
	// One-shot: clear it whatever the outcome.
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode,
	})
	if cookieErr != nil || cookie.Value == "" {
		http.Error(w, "This approval must come from the browser that started the sign-in. "+
			"Start the sign-in again.", http.StatusForbidden)
		return
	}

	pc, err := svc.storage.TakePendingConsent(r.Context(), r.PostForm.Get("consent_id"), hashSecret(cookie.Value))
	switch {
	case errors.Is(err, ErrConsentBindingMismatch):
		http.Error(w, "This approval must come from the browser that started the sign-in. "+
			"Start the sign-in again.", http.StatusForbidden)
		return
	case err != nil:
		http.Error(w, "This consent request is invalid or has expired. Start the sign-in again.",
			http.StatusBadRequest)
		return
	}

	authReq := &pc.Request
	if r.PostForm.Get("decision") != "approve" {
		svc.authError(w, authReq.RedirectURI, "access_denied", "The user denied the request", authReq.State)
		return
	}

	if err := svc.storage.StoreConsent(r.Context(), pc.UserID, authReq.ClientID); err != nil {
		svc.authError(w, authReq.RedirectURI, "server_error", "Failed to record consent", authReq.State)
		return
	}
	svc.completeAuthorization(w, r, authReq, pc.UserID)
}

// completeAuthorization issues an authorization code and sends the browser back to the client.
// Every path that reaches it has already validated authReq.RedirectURI against the client's
// registered set at /authorize.
func (svc *AuthService) completeAuthorization(w http.ResponseWriter, r *http.Request, authReq *PendingAuthRequest, userID string) {
	redirectURL, err := url.Parse(authReq.RedirectURI)
	if err != nil {
		svc.authErrorDirect(w, "invalid_request", "The client's registered redirect_uri is not a valid URL")
		return
	}

	authCode, err := svc.tokenService.GenerateAuthorizationCode(
		authReq.ClientID,
		authReq.RedirectURI,
		authReq.Scope,
		authReq.CodeChallenge,
		authReq.CodeChallengeMethod,
		authReq.Resource,
		userID,
	)
	if err != nil {
		svc.authError(w, authReq.RedirectURI, "server_error",
			"Failed to generate authorization code", authReq.State)
		return
	}

	q := redirectURL.Query()
	q.Set("code", authCode.Code)
	if authReq.State != "" {
		q.Set("state", authReq.State)
	}
	redirectURL.RawQuery = q.Encode()

	// After the consent POST, 303 makes the browser follow with a GET, rather than leaving that
	// to how it happens to treat a 302 (RFC 9700 §4.12).
	status := http.StatusFound
	if r.Method == http.MethodPost {
		status = http.StatusSeeOther
	}
	http.Redirect(w, r, redirectURL.String(), status)
}

// consentCookie names the browser-binding cookie. With an https issuer the __Host- prefix makes
// the browser refuse any version of it that is not Secure, host-only and Path=/, so a sibling
// subdomain cannot plant one. An http issuer (local development) cannot use the prefix.
func (svc *AuthService) consentCookie() (name string, secure bool) {
	if strings.HasPrefix(strings.ToLower(svc.config.Issuer), "https://") {
		return "__Host-mcp_consent", true
	}
	return "mcp_consent", false
}

// redirectDestination is what the consent screen names as the code's destination: scheme and
// host, which is what a user can actually judge. A path on a trusted host tells them nothing more,
// and a long one would push the host out of view.
func redirectDestination(redirectURI string) string {
	u, err := url.Parse(redirectURI)
	if err != nil || u.Scheme == "" {
		return redirectURI
	}
	if u.Host == "" {
		return u.Scheme + ":" // a native app's custom scheme, e.g. "myapp:"
	}
	return u.Scheme + "://" + u.Host
}

type consentView struct {
	ClientName     string
	SelfRegistered bool
	Destination    string
	Login          string
	Issuer         string
	Action         string
	ConsentID      string
}

// renderConsent writes the consent screen.
//
// The headers matter as much as the page:
//   - frame-ancestors / X-Frame-Options: a page that grants access must not be framable, or it
//     can be clickjacked.
//   - There is deliberately no CSP form-action. Chrome enforces form-action on the redirects that
//     follow a form submission, and the approval ends in a 303 to the client's redirect_uri, which
//     form-action 'self' would block. default-src does not cover form-action, so leaving it out
//     is not the same as allowing everything else.
//   - no-store and no-referrer keep the consent ID out of caches and out of Referer headers.
func (svc *AuthService) renderConsent(w http.ResponseWriter, v consentView) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	_ = consentTemplate.Execute(w, v)
}

var consentTemplate = template.Must(template.New("consent").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Authorize access</title>
<style>
  body { font-family: system-ui, sans-serif; background: #f6f7f9; color: #1c1e21; margin: 0; }
  main { max-width: 32rem; margin: 3rem auto; background: #fff; padding: 2rem; border-radius: 8px;
         box-shadow: 0 1px 3px rgba(0,0,0,.12); }
  h1 { font-size: 1.3rem; margin-top: 0; }
  .dest { font-family: ui-monospace, monospace; font-size: 1.1rem; background: #f0f2f5;
          padding: .6rem .8rem; border-radius: 4px; word-break: break-all; }
  .warn { background: #fff4e5; border-left: 4px solid #f0a020; padding: .6rem .8rem; }
  .actions { display: flex; gap: .75rem; margin-top: 1.5rem; }
  button { font-size: 1rem; padding: .6rem 1.4rem; border-radius: 6px; border: 1px solid #ccd0d5;
           background: #fff; cursor: pointer; }
  button.approve { background: #1a7f37; border-color: #1a7f37; color: #fff; }
  @media (prefers-color-scheme: dark) {
    body { background: #18191a; color: #e4e6eb; }
    main { background: #242526; box-shadow: none; }
    .dest { background: #3a3b3c; }
    .warn { background: #3d2e12; }
    button { background: #3a3b3c; color: #e4e6eb; border-color: #4e4f50; }
  }
</style>
</head>
<body>
<main>
  <h1>Authorize {{.ClientName}}?</h1>
  <p>You are signed in to GitHub as <strong>{{.Login}}</strong>.</p>
  <p><strong>{{.ClientName}}</strong> is asking for access to <strong>{{.Issuer}}</strong> on your behalf.</p>
  {{if .SelfRegistered}}<p class="warn">This application registered itself, so its name is not verified. Decide by where it will send you, below.</p>{{end}}
  <p>If you approve, your browser will be sent to:</p>
  <p class="dest">{{.Destination}}</p>
  <p>Approve only if you started this sign-in yourself, from an application you trust, and you recognize that address.</p>
  <form method="post" action="{{.Action}}">
    <input type="hidden" name="consent_id" value="{{.ConsentID}}">
    <div class="actions">
      <button type="submit" name="decision" value="approve" class="approve">Approve</button>
      <button type="submit" name="decision" value="deny">Deny</button>
    </div>
  </form>
</main>
</body>
</html>
`))
