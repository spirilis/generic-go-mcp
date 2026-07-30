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
