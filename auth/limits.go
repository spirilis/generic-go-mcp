package auth

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/spirilis/generic-go-mcp/logging"
)

// Limits on what the unauthenticated OAuth endpoints accept and keep. Without them anyone who
// can reach /register or /authorize can fill memory and the token store.
const (
	maxOAuthBodyBytes    = 64 << 10 // request bodies of /register, /authorize, /token
	maxRedirectURIs      = 16
	maxURILen            = 2048
	maxClientNameLen     = 256
	maxStateLen          = 4096
	maxScopeLen          = 2048
	maxChallengeLen      = 256
	maxDynamicClients    = 2000
	unusedClientMaxAge   = 24 * time.Hour
	sweepInterval        = 10 * time.Minute
	registrationScopeMax = 64
)

// registrationProblem returns why a registration request is too large, or "".
func registrationProblem(req *ClientRegistrationRequest) string {
	switch {
	case len(req.RedirectURIs) > maxRedirectURIs:
		return "too many redirect_uris"
	case len(req.ClientName) > maxClientNameLen:
		return "client_name is too long"
	case len(req.ClientURI) > maxURILen || len(req.LogoURI) > maxURILen:
		return "client_uri or logo_uri is too long"
	case len(req.Scope) > registrationScopeMax || len(strings.Join(req.Scope, " ")) > maxScopeLen:
		return "scope is too long"
	}
	for _, u := range req.RedirectURIs {
		if len(u) > maxURILen {
			return "a redirect_uri is too long"
		}
	}
	return ""
}

// authorizeProblem returns why an authorization request is too large, or "".
func authorizeProblem(state, scope, resource, challenge, method, redirectURI string) string {
	switch {
	case len(state) > maxStateLen:
		return "state is too long"
	case len(scope) > maxScopeLen:
		return "scope is too long"
	case len(resource) > maxURILen || len(redirectURI) > maxURILen:
		return "resource or redirect_uri is too long"
	case len(challenge) > maxChallengeLen || len(method) > 16:
		return "code_challenge is too long"
	}
	return ""
}

// dynamicClientCount counts self-registered clients.
func (svc *AuthService) dynamicClientCount(ctx context.Context) (int, error) {
	clients, err := svc.storage.ListClients(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range clients {
		if !c.IsStatic {
			n++
		}
	}
	return n, nil
}

// sweeper is implemented by stores that can drop expired records.
type sweeper interface {
	Sweep(ctx context.Context, now time.Time, unusedClientAge time.Duration) (int, error)
}

// startSweeper removes expired records now and then every sweepInterval, until stop is closed.
func (svc *AuthService) startSweeper() {
	sw, ok := svc.storage.(sweeper)
	if !ok {
		return
	}
	svc.stopSweep = make(chan struct{})
	svc.sweepDone = make(chan struct{})
	go func() {
		defer close(svc.sweepDone)
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		for {
			if n, err := sw.Sweep(context.Background(), time.Now(), unusedClientMaxAge); err != nil {
				logging.Warn("auth store sweep failed", "error", err)
			} else if n > 0 {
				logging.Debug("auth store sweep removed expired records", "count", n)
			}
			select {
			case <-svc.stopSweep:
				return
			case <-t.C:
			}
		}
	}()
}

// stopSweeper ends the background sweep and waits for it.
func (svc *AuthService) stopSweeper() {
	if svc.stopSweep != nil {
		close(svc.stopSweep)
		<-svc.sweepDone
		svc.stopSweep = nil
	}
}

// Sweep deletes expired authorization requests, pending consents, codes and tokens, and
// self-registered clients older than unusedClientAge that hold no consent, token, code or
// pending request. It returns how many records it removed.
//
// bbolt reuses the freed pages, so the file stops growing; it does not shrink.
func (s *BoltStorage) Sweep(ctx context.Context, now time.Time, unusedClientAge time.Duration) (int, error) {
	removed := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		inUse := map[string]bool{}
		for _, name := range []string{BucketAuthRequests, BucketPendingConsents, BucketAuthCodes, BucketAccessTokens, BucketRefreshTokens} {
			b := tx.Bucket([]byte(name))
			var dead [][]byte
			err := b.ForEach(func(k, v []byte) error {
				var rec struct {
					ExpiresAt time.Time `json:"expires_at"`
					ClientID  string    `json:"client_id"`
					Request   struct {
						ClientID string `json:"client_id"`
					} `json:"request"`
				}
				if json.Unmarshal(v, &rec) != nil {
					return nil // not ours to judge
				}
				if !rec.ExpiresAt.IsZero() && now.After(rec.ExpiresAt) {
					dead = append(dead, append([]byte(nil), k...))
					return nil
				}
				inUse[rec.ClientID] = true
				inUse[rec.Request.ClientID] = true
				return nil
			})
			if err != nil {
				return err
			}
			for _, k := range dead {
				if err := b.Delete(k); err != nil {
					return err
				}
			}
			removed += len(dead)
		}
		// A remembered consent keeps its client: the key is userID NUL clientID.
		if err := tx.Bucket([]byte(BucketConsents)).ForEach(func(k, _ []byte) error {
			if i := strings.IndexByte(string(k), 0); i >= 0 {
				inUse[string(k[i+1:])] = true
			}
			return nil
		}); err != nil {
			return err
		}
		clients := tx.Bucket([]byte(BucketClients))
		var dead [][]byte
		if err := clients.ForEach(func(k, v []byte) error {
			var c RegisteredClient
			if json.Unmarshal(v, &c) != nil || c.IsStatic || inUse[c.ClientID] {
				return nil
			}
			if now.Sub(c.CreatedAt) > unusedClientAge {
				dead = append(dead, append([]byte(nil), k...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, k := range dead {
			if err := clients.Delete(k); err != nil {
				return err
			}
		}
		removed += len(dead)
		return nil
	})
	return removed, err
}
