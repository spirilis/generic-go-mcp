package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/spirilis/generic-go-mcp/config"
)

// Provider is an identity provider users sign in with. The library drives the browser flow
// (/authorize, the chooser, /callback); a provider only builds its authorization URL and turns
// the code it sends back into an Identity.
type Provider interface {
	// Name is the provider's name from config: the prefix of principal ids and what rules name.
	Name() string
	// DisplayName is what the sign-in chooser shows.
	DisplayName() string
	// Type is config.ProviderTypeGitHub or config.ProviderTypeOIDC.
	Type() string

	// AuthCodeURL returns where to send the browser. state is echoed back to /callback; nonce
	// and pkceChallenge (S256) are bound into the provider's code where it supports them. ctx
	// bounds only how long the call may wait for a provider that is still starting up: it
	// answers ErrProviderUnavailable rather than wait longer.
	AuthCodeURL(ctx context.Context, callback, state, nonce, pkceChallenge string) (string, error)

	// Exchange redeems the code /callback received and returns the user's identity. verifier
	// and nonce are the ones AuthCodeURL was given.
	Exchange(ctx context.Context, code, callback, verifier, nonce string) (Identity, error)
}

// ErrProviderUnavailable means the provider cannot sign anyone in right now: its discovery
// document or keys have not been fetched (yet). /authorize answers 503 for it.
var ErrProviderUnavailable = errors.New("identity provider is not available")

// newProvider builds the provider a resolved config entry describes. It does no network I/O:
// OIDC discovery starts on first use, in bg (the service's lifetime).
func newProvider(p config.ProviderConfig, bg *background) (Provider, error) {
	switch p.Type {
	case config.ProviderTypeGitHub:
		return newGitHubProvider(p), nil
	case config.ProviderTypeOIDC:
		return newOIDCProvider(p, bg)
	}
	return nil, fmt.Errorf("auth provider %q: unknown type %q", p.Name, p.Type)
}

// providerIssuer is the issuer a provider's users are keyed under: the OIDC issuer URL, or
// the GitHub web URL (github.com and a GitHub Enterprise Server are different user spaces).
func providerIssuer(p config.ProviderConfig) string {
	if p.Type == config.ProviderTypeGitHub {
		return p.BaseURL
	}
	return p.IssuerURL
}

// maxProviderResponseBytes caps every response body read from an identity provider. Discovery
// documents, key sets, tokens and profiles are a few KiB; anything near this is hostile or
// broken, and must not be buffered whole.
const maxProviderResponseBytes = 1 << 20

// readCapped reads a provider response body, failing past maxProviderResponseBytes.
func readCapped(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxProviderResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxProviderResponseBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxProviderResponseBytes)
	}
	return data, nil
}

// noRedirects makes an http.Client return a redirect as the response instead of following it.
// A provider endpoint that redirects would otherwise take the request (and a client secret or
// a token) wherever it points, past the host checks made on the configured URLs.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// background runs the service's long-lived goroutines (OIDC discovery and key refresh) on a
// context that ends with the service, never on a request's: a request that gives up must not
// abort a fetch other requests are waiting on.
type background struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func newBackground() *background {
	ctx, cancel := context.WithCancel(context.Background())
	return &background{ctx: ctx, cancel: cancel}
}

// Go runs fn in a goroutine unless the service is closing. fn must return once ctx is done.
func (b *background) Go(fn func(ctx context.Context)) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		fn(b.ctx)
	}()
	return true
}

// Close stops the goroutines and waits for them.
func (b *background) Close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cancel()
	b.wg.Wait()
}
