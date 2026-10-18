package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeHandler is a minimal MessageHandler for exercising HTTPTransport's request
// validation and response-shape logic without a real MCP server behind it.
type fakeHandler struct {
	fn func(ctx context.Context, data []byte, w ResponseWriter)
}

func (h *fakeHandler) HandleMessage(ctx context.Context, data []byte, w ResponseWriter) {
	if h.fn != nil {
		h.fn(ctx, data, w)
	}
}

func newTestTransport(handler MessageHandler) *HTTPTransport {
	tr := NewHTTPTransport(HTTPTransportConfig{})
	tr.handler = handler
	return tr
}

func doRequest(tr *HTTPTransport, req *http.Request) *http.Response {
	rec := httptest.NewRecorder()
	tr.handleMCP(rec, req)
	return rec.Result()
}

func decodeError(t *testing.T, resp *http.Response) *RPCError {
	t.Helper()
	var env struct {
		Error *RPCError `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	return env.Error
}

const validMetaJSON = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

func TestMissingMcpMethodHeaderRejected(t *testing.T) {
	called := false
	tr := newTestTransport(&fakeHandler{fn: func(ctx context.Context, data []byte, w ResponseWriter) {
		called = true
		w.WriteMessage(NewSuccessResponse(json.RawMessage(`1`), map[string]string{"ok": "true"}))
	}})

	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{%s}}`, validMetaJSON)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set(ProtocolVersionHeader, "2026-07-28")
	// Mcp-Method intentionally omitted.

	resp := doRequest(tr, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	if rerr := decodeError(t, resp); rerr == nil || rerr.Code != HeaderMismatch {
		t.Fatalf("error = %+v, want code %d", rerr, HeaderMismatch)
	}
	if called {
		t.Error("handler must not be invoked when required headers are missing")
	}
}

func TestMcpNameMismatchRejected(t *testing.T) {
	tr := newTestTransport(&fakeHandler{fn: func(ctx context.Context, data []byte, w ResponseWriter) {
		w.WriteMessage(NewSuccessResponse(json.RawMessage(`1`), map[string]string{"ok": "true"}))
	}})

	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"real_tool","arguments":{},%s}}`, validMetaJSON)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set(ProtocolVersionHeader, "2026-07-28")
	req.Header.Set(MethodHeader, "tools/call")
	req.Header.Set(NameHeader, "wrong_tool")

	resp := doRequest(tr, req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	if rerr := decodeError(t, resp); rerr == nil || rerr.Code != HeaderMismatch {
		t.Fatalf("error = %+v, want code %d", rerr, HeaderMismatch)
	}
}

func TestMcpNameBase64SentinelDecodesAndMatches(t *testing.T) {
	called := false
	tr := newTestTransport(&fakeHandler{fn: func(ctx context.Context, data []byte, w ResponseWriter) {
		called = true
		w.WriteMessage(NewSuccessResponse(json.RawMessage(`1`), map[string]string{"ok": "true"}))
	}})

	name := "tööl" // non-ASCII forces base64 sentinel encoding
	nameJSON, _ := json.Marshal(name)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%s,"arguments":{},%s}}`, nameJSON, validMetaJSON)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set(ProtocolVersionHeader, "2026-07-28")
	req.Header.Set(MethodHeader, "tools/call")
	req.Header.Set(NameHeader, EncodeHeaderValue(name))

	resp := doRequest(tr, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d (error: %+v)", resp.StatusCode, http.StatusOK, decodeError(t, resp))
	}
	if !called {
		t.Fatal("handler should have been invoked once headers were validated")
	}
}

func TestOriginRejected(t *testing.T) {
	tr := newTestTransport(&fakeHandler{})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://evil.example.com")

	resp := doRequest(tr, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestOriginAllowedForLocalhostByDefault(t *testing.T) {
	tr := newTestTransport(&fakeHandler{fn: func(ctx context.Context, data []byte, w ResponseWriter) {
		w.WriteMessage(NewSuccessResponse(json.RawMessage(`1`), map[string]string{"ok": "true"}))
	}})
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{%s}}`, validMetaJSON)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set(ProtocolVersionHeader, "2026-07-28")
	req.Header.Set(MethodHeader, "tools/list")

	resp := doRequest(tr, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestGetAndDeleteNotAllowed(t *testing.T) {
	tr := newTestTransport(&fakeHandler{})
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req := httptest.NewRequest(method, "/mcp", nil)
		resp := doRequest(tr, req)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want %d", method, resp.StatusCode, http.StatusMethodNotAllowed)
		}
	}
}

func TestNotificationReturns202Accepted(t *testing.T) {
	called := false
	tr := newTestTransport(&fakeHandler{fn: func(ctx context.Context, data []byte, w ResponseWriter) {
		called = true
		// A notification handler must never call WriteMessage.
	}})

	body := `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"1"}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set(ProtocolVersionHeader, "2026-07-28")
	req.Header.Set(MethodHeader, "notifications/cancelled")

	resp := doRequest(tr, req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}
	if !called {
		t.Fatal("handler should be invoked for a notification")
	}
}

func TestPlainResultIsApplicationJSON(t *testing.T) {
	tr := newTestTransport(&fakeHandler{fn: func(ctx context.Context, data []byte, w ResponseWriter) {
		w.WriteMessage(NewSuccessResponse(json.RawMessage(`1`), map[string]string{"ok": "true"}))
	}})

	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t","arguments":{},%s}}`, validMetaJSON)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set(ProtocolVersionHeader, "2026-07-28")
	req.Header.Set(MethodHeader, "tools/call")
	req.Header.Set(NameHeader, "t")

	resp := doRequest(tr, req)
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

// TestErrorResponseMapsToCorrectHTTPStatus guards against a regression where
// httpResponseWriter.WriteMessage always wrote 200 OK regardless of the JSON-RPC error
// code inside the body: the Streamable HTTP binding requires specific error codes (e.g.
// MethodNotFound, UnsupportedProtocolVersion) to surface as specific non-200 statuses.
func TestErrorResponseMapsToCorrectHTTPStatus(t *testing.T) {
	cases := []struct {
		name       string
		code       int
		wantStatus int
	}{
		{"MethodNotFound", MethodNotFound, http.StatusNotFound},
		{"UnsupportedProtocolVersion", UnsupportedProtocolVersion, http.StatusBadRequest},
		{"MissingRequiredClientCapability", MissingRequiredClientCapability, http.StatusBadRequest},
		{"InvalidParams", InvalidParams, http.StatusBadRequest},
		{"InternalError", InternalError, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTestTransport(&fakeHandler{fn: func(ctx context.Context, data []byte, w ResponseWriter) {
				w.WriteMessage(NewErrorResponse(json.RawMessage(`1`), &RPCError{Code: tc.code, Message: "test"}))
			}})
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{%s}}`, validMetaJSON)
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
			req.Header.Set(ProtocolVersionHeader, "2026-07-28")
			req.Header.Set(MethodHeader, "tools/list")

			resp := doRequest(tr, req)
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

func TestNotificationBeforeResultUpgradesToSSE(t *testing.T) {
	tr := newTestTransport(&fakeHandler{fn: func(ctx context.Context, data []byte, w ResponseWriter) {
		w.WriteNotification("notifications/progress", map[string]interface{}{"progress": 1})
		w.WriteMessage(NewSuccessResponse(json.RawMessage(`1`), map[string]string{"ok": "true"}))
	}})

	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t","arguments":{},%s}}`, validMetaJSON)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set(ProtocolVersionHeader, "2026-07-28")
	req.Header.Set(MethodHeader, "tools/call")
	req.Header.Set(NameHeader, "t")

	resp := doRequest(tr, req)
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
}

// G4: a POST body larger than MaxBodyBytes is rejected with 413 rather than read unbounded.
func TestOversizedBodyRejected(t *testing.T) {
	called := false
	tr := NewHTTPTransport(HTTPTransportConfig{MaxBodyBytes: 1024})
	tr.handler = &fakeHandler{fn: func(ctx context.Context, data []byte, w ResponseWriter) {
		called = true
		w.WriteMessage(NewSuccessResponse(json.RawMessage(`1`), struct{}{}))
	}}

	big := strings.Repeat("a", 4096)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"%s",%s}}`, big, validMetaJSON)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set(ProtocolVersionHeader, "2026-07-28")
	req.Header.Set(MethodHeader, "tools/list")

	resp := doRequest(tr, req)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if called {
		t.Error("handler was invoked for an oversized body; it should have been rejected first")
	}
}

// A body within the cap still flows through to the handler.
func TestBodyWithinCapIsAccepted(t *testing.T) {
	called := false
	tr := NewHTTPTransport(HTTPTransportConfig{MaxBodyBytes: 1 << 20})
	tr.handler = &fakeHandler{fn: func(ctx context.Context, data []byte, w ResponseWriter) {
		called = true
		w.WriteMessage(NewSuccessResponse(json.RawMessage(`1`), struct{}{}))
	}}

	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{%s}}`, validMetaJSON)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set(ProtocolVersionHeader, "2026-07-28")
	req.Header.Set(MethodHeader, "tools/list")

	resp := doRequest(tr, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !called {
		t.Error("handler was not invoked for a body within the cap")
	}
}

func TestExtraRoutesAreServedAlongsideMCP(t *testing.T) {
	tr := NewHTTPTransport(HTTPTransportConfig{
		ExtraRoutes: func(mux *http.ServeMux) {
			mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"status":"healthy"}`))
			})
		},
	})
	tr.handler = &fakeHandler{fn: func(ctx context.Context, data []byte, w ResponseWriter) {
		w.WriteMessage(NewSuccessResponse(json.RawMessage(`1`), map[string]string{"ok": "true"}))
	}}
	mux := tr.buildMux()

	// The embedder's route is reachable.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/health status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != `{"status":"healthy"}` {
		t.Errorf("/health body = %q", got)
	}

	// ...and /mcp still works alongside it.
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{%s}}`, validMetaJSON)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set(ProtocolVersionHeader, "2026-07-28")
	req.Header.Set(MethodHeader, "tools/list")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/mcp status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestNilExtraRoutesLeavesRoutingUnchanged(t *testing.T) {
	tr := newTestTransport(&fakeHandler{})
	mux := tr.buildMux()

	// Nothing but /mcp is registered, so an unrelated path is a plain 404 — the same
	// behavior this transport had before ExtraRoutes existed.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/health status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// fakeAuthProvider gates requests on a fixed bearer token and records route registration,
// so a test can tell whether Handler registered anything on the embedder's behalf.
type fakeAuthProvider struct {
	registered bool
}

func (f *fakeAuthProvider) RegisterRoutes(*http.ServeMux)      { f.registered = true }
func (f *fakeAuthProvider) RegisterAdminRoutes(*http.ServeMux) { f.registered = true }
func (f *fakeAuthProvider) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (f *fakeAuthProvider) UserFromContext(context.Context) (string, string, bool) {
	return "", "", false
}

func modernToolsList(t *testing.T, url string) *http.Request {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{%s}}`, validMetaJSON)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(ProtocolVersionHeader, "2026-07-28")
	req.Header.Set(MethodHeader, "tools/list")
	return req
}

// TestHandlerMountsOnEmbedderMux drives the exported Handler through a real server built
// from the embedder's own mux: POST is served, GET is the endpoint's own 405 (compat off),
// a foreign Origin is refused, and the embedder's other routes are untouched.
func TestHandlerMountsOnEmbedderMux(t *testing.T) {
	tr := NewHTTPTransport(HTTPTransportConfig{})
	mux := http.NewServeMux()
	mux.Handle("/mcp", tr.Handler(&fakeHandler{fn: func(ctx context.Context, data []byte, w ResponseWriter) {
		w.WriteMessage(NewSuccessResponse(json.RawMessage(`1`), map[string]string{"ok": "true"}))
	}}))
	mux.HandleFunc("/own", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.DefaultClient.Do(modernToolsList(t, srv.URL+"/mcp"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("POST /mcp = %d %q, want 200 application/json", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	resp, err = http.Get(srv.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /mcp = %d, want 405", resp.StatusCode)
	}

	req := modernToolsList(t, srv.URL+"/mcp")
	req.Header.Set("Origin", "https://evil.example")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign Origin = %d, want 403", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/own")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("embedder route = %d, want 418", resp.StatusCode)
	}
}

// TestHandlerAppliesAuthMiddlewareOnly: with an AuthService, Handler puts its middleware in
// front of /mcp but registers no routes — the embedder owns its mux.
func TestHandlerAppliesAuthMiddlewareOnly(t *testing.T) {
	auth := &fakeAuthProvider{}
	tr := NewHTTPTransport(HTTPTransportConfig{AuthService: auth})
	h := tr.Handler(&fakeHandler{fn: func(ctx context.Context, data []byte, w ResponseWriter) {
		w.WriteMessage(NewSuccessResponse(json.RawMessage(`1`), map[string]string{"ok": "true"}))
	}})
	if auth.registered {
		t.Fatal("Handler must not register auth routes")
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.DefaultClient.Do(modernToolsList(t, srv.URL+"/mcp"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", resp.StatusCode)
	}
	req := modernToolsList(t, srv.URL+"/mcp")
	req.Header.Set("Authorization", "Bearer good")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("good token = %d, want 200", resp.StatusCode)
	}
}

// TestResponseRecorderUnwraps lets http.ResponseController reach the connection through
// the recorder handleMCP wraps every response in.
func TestResponseRecorderUnwraps(t *testing.T) {
	inner := httptest.NewRecorder()
	if got := newResponseRecorder(inner).Unwrap(); got != inner {
		t.Fatalf("Unwrap() = %v, want the wrapped writer", got)
	}
}
