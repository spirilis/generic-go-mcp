package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLegacySessions is a minimal, deterministic LegacySessions test double. It lives here
// rather than reusing the real compat.Overlay because transport cannot import compat
// (compat imports transport — that would be a real cycle, not just an inconvenient one).
type fakeLegacySessions struct {
	mu     sync.Mutex
	known  map[string]chan struct{}
	stream func(ctx context.Context, id string, w ResponseWriter) error
}

func newFakeLegacySessions() *fakeLegacySessions {
	return &fakeLegacySessions{known: make(map[string]chan struct{})}
}

// add registers a live session named id, returning its Done channel.
func (f *fakeLegacySessions) add(id string) <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	done := make(chan struct{})
	f.known[id] = done
	return done
}

func (f *fakeLegacySessions) Known(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.known[id]
	return ok
}

func (f *fakeLegacySessions) Terminate(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	done, ok := f.known[id]
	if !ok {
		return false
	}
	close(done)
	delete(f.known, id)
	return true
}

func (f *fakeLegacySessions) Done(id string) (<-chan struct{}, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	done, ok := f.known[id]
	return done, ok
}

func (f *fakeLegacySessions) StreamNotifications(ctx context.Context, id string, w ResponseWriter) error {
	if f.stream != nil {
		return f.stream(ctx, id, w)
	}
	<-ctx.Done()
	return nil
}

var _ LegacySessions = (*fakeLegacySessions)(nil)

func newTestTransportWithLegacySessions(handler MessageHandler, ls LegacySessions) *HTTPTransport {
	tr := NewHTTPTransport(HTTPTransportConfig{LegacySessions: ls})
	tr.handler = handler
	return tr
}

// TestLegacySessionsNilByDefault guards the "nil means compatibility off" contract at its
// source: a caller that never sets HTTPTransportConfig.LegacySessions must get a genuinely
// nil interface back, not a typed nil.
func TestLegacySessionsNilByDefault(t *testing.T) {
	tr := NewHTTPTransport(HTTPTransportConfig{})
	if tr.legacySessions != nil {
		t.Error("expected legacySessions to be nil when HTTPTransportConfig.LegacySessions is unset")
	}
}

func TestLegacyPOSTServedWithoutModernHeaders(t *testing.T) {
	fake := newFakeLegacySessions()
	fake.add("sess-1")
	called := false
	tr := newTestTransportWithLegacySessions(&fakeHandler{fn: func(ctx context.Context, data []byte, w ResponseWriter) {
		called = true
		w.WriteMessage(NewSuccessResponse(json.RawMessage(`1`), map[string]string{"ok": "true"}))
	}}, fake)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set(SessionIDHeader, "sess-1")
	// Deliberately no MCP-Protocol-Version / Mcp-Method headers — a real 2025-11-25
	// client has no idea they exist.

	resp := doRequest(tr, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d (error: %+v)", resp.StatusCode, http.StatusOK, decodeError(t, resp))
	}
	if !called {
		t.Fatal("handler should have been invoked for a legacy request without modern headers")
	}
}

func TestModernRequestsStillHeaderValidatedWithCompatOn(t *testing.T) {
	fake := newFakeLegacySessions()
	called := false
	tr := newTestTransportWithLegacySessions(&fakeHandler{fn: func(ctx context.Context, data []byte, w ResponseWriter) {
		called = true
	}}, fake)

	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{%s}}`, validMetaJSON)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set(ProtocolVersionHeader, "2026-07-28")
	// Mcp-Method intentionally omitted: a modern request must still be rejected exactly
	// as it would be with compatibility off.

	resp := doRequest(tr, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	if rerr := decodeError(t, resp); rerr == nil || rerr.Code != HeaderMismatch {
		t.Fatalf("error = %+v, want code %d", rerr, HeaderMismatch)
	}
	if called {
		t.Error("handler must not be invoked when required headers are missing, even with compat on")
	}
}

func TestUnknownSessionHandling(t *testing.T) {
	fake := newFakeLegacySessions()
	tr := newTestTransportWithLegacySessions(&fakeHandler{fn: func(ctx context.Context, data []byte, w ResponseWriter) {
		w.WriteMessage(NewSuccessResponse(json.RawMessage(`1`), map[string]string{"ok": "true"}))
	}}, fake)

	t.Run("legacy request naming an unknown session -> 404", func(t *testing.T) {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set(SessionIDHeader, "nonexistent")
		resp := doRequest(tr, req)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
		}
	})

	t.Run("modern request with a stray unknown session header -> 200, ignored", func(t *testing.T) {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{%s}}`, validMetaJSON)
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set(ProtocolVersionHeader, "2026-07-28")
		req.Header.Set(MethodHeader, "tools/list")
		req.Header.Set(SessionIDHeader, "nonexistent")
		resp := doRequest(tr, req)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want %d (error: %+v)", resp.StatusCode, http.StatusOK, decodeError(t, resp))
		}
		if got := resp.Header.Get(SessionIDHeader); got != "" {
			t.Errorf("session header echoed back as %q, want it left alone", got)
		}
	})

	t.Run("initialize with a stale session -> 200", func(t *testing.T) {
		body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set(SessionIDHeader, "nonexistent")
		resp := doRequest(tr, req)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want %d (error: %+v)", resp.StatusCode, http.StatusOK, decodeError(t, resp))
		}
	})
}

func TestDeleteTerminatesAndSubsequentReuse404s(t *testing.T) {
	fake := newFakeLegacySessions()
	fake.add("sess-1")
	tr := newTestTransportWithLegacySessions(&fakeHandler{}, fake)

	delReq := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	delReq.Header.Set(SessionIDHeader, "sess-1")
	resp := doRequest(tr, delReq)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	postReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	postReq.Header.Set(SessionIDHeader, "sess-1")
	resp2 := doRequest(tr, postReq)
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("POST reusing a terminated session: status = %d, want %d", resp2.StatusCode, http.StatusNotFound)
	}
}

func TestDeleteUnknownSession404s(t *testing.T) {
	fake := newFakeLegacySessions()
	tr := newTestTransportWithLegacySessions(&fakeHandler{}, fake)

	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set(SessionIDHeader, "nonexistent")
	resp := doRequest(tr, req)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

// TestGetOpensSSEStreamAndExitsOnClientDisconnect proves the standalone GET stream opens
// text/event-stream immediately and honors the request's own context (the client closing
// its connection) as one of its two exit conditions.
func TestGetOpensSSEStreamAndExitsOnClientDisconnect(t *testing.T) {
	fake := newFakeLegacySessions()
	fake.add("sess-1")
	tr := newTestTransportWithLegacySessions(&fakeHandler{}, fake)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil).WithContext(ctx)
	req.Header.Set(SessionIDHeader, "sess-1")
	rec := httptest.NewRecorder()

	handlerDone := make(chan struct{})
	go func() {
		tr.handleMCP(rec, req)
		close(handlerDone)
	}()

	// Give the handler time to start and upgrade the response to SSE before asserting
	// it's still open. This check only touches the handlerDone channel, not the
	// recorder itself — the recorder's Header map is written by the handler goroutine,
	// so reading it here (unsynchronized with that write) would be a data race even
	// though the sleep makes it reliable in practice.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-handlerDone:
		t.Fatal("handler returned before the client disconnected")
	default:
	}

	cancel() // simulate the client closing its connection

	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after the client disconnected")
	}

	// Safe to inspect the recorder now: receiving handlerDone's close synchronizes with
	// everything the handler goroutine did before returning, including its header write.
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
}

// TestGetExitsWhenSessionTerminated proves the standalone GET stream's other exit
// condition: the session itself ending (TTL expiry or an explicit DELETE), independent of
// the client's own connection.
func TestGetExitsWhenSessionTerminated(t *testing.T) {
	fake := newFakeLegacySessions()
	fake.add("sess-1")
	tr := newTestTransportWithLegacySessions(&fakeHandler{}, fake)

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(SessionIDHeader, "sess-1")
	rec := httptest.NewRecorder()

	handlerDone := make(chan struct{})
	go func() {
		tr.handleMCP(rec, req)
		close(handlerDone)
	}()

	time.Sleep(50 * time.Millisecond)
	fake.Terminate("sess-1")

	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after the session was terminated")
	}
}

func TestGetUnknownSession404s(t *testing.T) {
	fake := newFakeLegacySessions()
	tr := newTestTransportWithLegacySessions(&fakeHandler{}, fake)

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set(SessionIDHeader, "nonexistent")
	resp := doRequest(tr, req)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestCORSExposesSessionIDHeaderWhenCompatOn(t *testing.T) {
	fake := newFakeLegacySessions()
	tr := newTestTransportWithLegacySessions(&fakeHandler{}, fake)

	req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	resp := doRequest(tr, req)

	if got := resp.Header.Get("Access-Control-Expose-Headers"); !strings.Contains(got, SessionIDHeader) {
		t.Errorf("Access-Control-Expose-Headers = %q, want it to contain %q", got, SessionIDHeader)
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "DELETE") {
		t.Errorf("Access-Control-Allow-Methods = %q, want it to contain DELETE", got)
	}
}

// TestCORSUnchangedWhenCompatOff guards the "byte-for-byte unchanged with the switch off"
// property at the CORS headers specifically, since those are easy to leave leaking a
// legacy-only header even when nothing else regresses.
func TestCORSUnchangedWhenCompatOff(t *testing.T) {
	tr := newTestTransport(&fakeHandler{}) // no LegacySessions -> compat off

	req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	resp := doRequest(tr, req)

	if got := resp.Header.Get("Access-Control-Expose-Headers"); got != "" {
		t.Errorf("Access-Control-Expose-Headers = %q, want empty when compat is off", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); strings.Contains(got, "DELETE") {
		t.Errorf("Access-Control-Allow-Methods = %q, must not contain DELETE when compat is off", got)
	}
}
