package compat

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/spirilis/generic-go-mcp/mcp"
	"github.com/spirilis/generic-go-mcp/transport"
)

// stdioSessionKey is the reserved session id for stdio's one implicit session. A byte
// stream has no headers to carry a session id, and it doesn't need one: on stdio the
// connection is the process, so there is exactly one session, keyed by the empty string.
const stdioSessionKey = ""

// defaultSessionTTL is used when Config.SessionTTL is zero.
const defaultSessionTTL = 30 * time.Minute

// maxLegacySessions caps the store so a client re-handshaking in a loop cannot grow it
// without bound. A few hundred is plenty for the compatibility window this overlay exists
// to cover.
const maxLegacySessions = 256

// legacySession is what a legacy "initialize" handshake establishes and every later
// request on that session recalls — the connection-scoped revisions declare client
// capabilities once, at the handshake, and every later request trusts what was agreed
// then.
//
// Everything below except lastSeen and terminated is set once at creation and never
// mutated afterward, so a reader (a tool, or this session forwarded into a translated
// request) can access it without holding sessionStore.mu. Only lastSeen and terminated are
// mutable, and only ever under that lock.
type legacySession struct {
	id              string
	protocolVersion string
	clientInfo      *mcp.Implementation
	capabilities    *mcp.ClientCapabilities

	createdAt  time.Time
	lastSeen   time.Time
	done       chan struct{} // closed on termination; a standalone GET stream watches it
	terminated bool
}

// newSessionID mints a cryptographically secure, visible-ASCII session id: hex over 16
// random bytes satisfies both. Following the precedent set by mcp.NewServer's random
// RequestStateKey generation, a crypto/rand.Read error (which does not happen on any
// platform this library supports) is not specially handled.
func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sessionStore holds legacy sessions in memory, per-process — deliberately. A restart
// invalidates every session; the next request against a stale id gets 404 and the client
// re-handshakes, which is the designed recovery path, not a bug. It's also why running a
// single replica matters unless this store is externalized.
//
// There is no background janitor goroutine: a ticker would leak in tests and burn a
// goroutine in every process that enables compatibility but never sees a legacy client.
// Expired sessions are swept opportunistically on create, the only path that grows the
// store.
type sessionStore struct {
	mu    sync.Mutex
	byID  map[string]*legacySession
	ttl   time.Duration
	inner transport.MessageHandler // used by StreamNotifications, see listen.go
}

func newSessionStore(inner transport.MessageHandler, ttl time.Duration) *sessionStore {
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	return &sessionStore{
		byID:  make(map[string]*legacySession),
		ttl:   ttl,
		inner: inner,
	}
}

// create stores sess under sess.id, replacing (and terminating) any existing session on
// that id first. Re-handshaking on the same id — stdioSessionKey for stdio, since that's
// the only case a "same key" collision can occur — is a legitimate reset, not an error.
// Also opportunistically sweeps expired sessions and, if the store is at capacity, evicts
// the least-recently-used entry to make room.
func (s *sessionStore) create(sess *legacySession) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sweepLocked()

	if old, ok := s.byID[sess.id]; ok {
		s.terminateLocked(old)
	}
	if len(s.byID) >= maxLegacySessions {
		s.evictLRULocked()
	}

	sess.lastSeen = time.Now()
	s.byID[sess.id] = sess
}

// get returns the session named id, touching its lastSeen (extending its idle TTL) if
// found. ok is false for an id naming no live session — including one that has aged past
// ttl, which get itself removes on the way out.
func (s *sessionStore) get(id string) (*legacySession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	if time.Since(sess.lastSeen) > s.ttl {
		s.terminateLocked(sess)
		delete(s.byID, id)
		return nil, false
	}
	sess.lastSeen = time.Now()
	return sess, true
}

// touch extends id's idle TTL without returning the session, for callers (e.g. a swallowed
// notifications/initialized) that only need the side effect.
func (s *sessionStore) touch(id string) {
	s.get(id)
}

// terminateLocked closes sess.done exactly once. Callers must hold s.mu.
func (s *sessionStore) terminateLocked(sess *legacySession) {
	if sess.terminated {
		return
	}
	sess.terminated = true
	close(sess.done)
}

// sweepLocked removes every session past its idle TTL. Callers must hold s.mu.
func (s *sessionStore) sweepLocked() {
	now := time.Now()
	for id, sess := range s.byID {
		if now.Sub(sess.lastSeen) > s.ttl {
			s.terminateLocked(sess)
			delete(s.byID, id)
		}
	}
}

// evictLRULocked terminates and removes the least-recently-used session, if any. Callers
// must hold s.mu.
func (s *sessionStore) evictLRULocked() {
	var oldestID string
	var oldest time.Time
	found := false
	for id, sess := range s.byID {
		if !found || sess.lastSeen.Before(oldest) {
			oldestID, oldest, found = id, sess.lastSeen, true
		}
	}
	if found {
		s.terminateLocked(s.byID[oldestID])
		delete(s.byID, oldestID)
	}
}

// Known implements transport.LegacySessions.
func (s *sessionStore) Known(id string) bool {
	_, ok := s.get(id)
	return ok
}

// Terminate implements transport.LegacySessions. Idempotent: terminating an id that is
// already gone (expired, or already terminated) reports false without error.
func (s *sessionStore) Terminate(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.byID[id]
	if !ok {
		return false
	}
	s.terminateLocked(sess)
	delete(s.byID, id)
	return true
}

// Done implements transport.LegacySessions.
func (s *sessionStore) Done(id string) (<-chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	return sess.done, true
}

// StreamNotifications is defined in listen.go, alongside the compile-time
// transport.LegacySessions assertion — it needs the subscription-bridging machinery
// defined there.
