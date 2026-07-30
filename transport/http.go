package transport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/spirilis/generic-go-mcp/auth"
	"github.com/spirilis/generic-go-mcp/logging"
)

// responseRecorder wraps http.ResponseWriter to capture response details for logging.
type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	size       int
	body       *bytes.Buffer
}

func newResponseRecorder(w http.ResponseWriter) *responseRecorder {
	return &responseRecorder{
		ResponseWriter: w,
		statusCode:     http.StatusOK, // Default status
		body:           &bytes.Buffer{},
	}
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	// Capture body if trace logging is enabled
	if logging.IsTraceEnabled() {
		r.body.Write(b)
	}
	n, err := r.ResponseWriter.Write(b)
	r.size += n
	return n, err
}

// Flush forwards to the underlying ResponseWriter's Flusher, if any. Without this,
// *responseRecorder would not itself satisfy http.Flusher even when the wrapped
// ResponseWriter does (embedding the http.ResponseWriter interface does not promote
// Flush, which belongs to the separate http.Flusher interface) — silently breaking SSE
// flushing for any handler that only sees the recorder.
func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// HTTPTransportConfig holds configuration for HTTP transport
type HTTPTransportConfig struct {
	Host        string
	Port        int
	AuthService *auth.AuthService // Optional auth service

	// AllowedOrigins is the allow-list checked against a request's Origin header, per the
	// Streamable HTTP requirement to validate Origin and prevent DNS rebinding. A request
	// with no Origin header (i.e. not sent from a browser) is always allowed. If empty,
	// the default policy allows only http(s)://localhost and http(s)://127.0.0.1 (any
	// port) — appropriate for a server bound to loopback. Set to []string{"*"} to allow
	// any origin (e.g. behind a trusted reverse proxy that already restricts access).
	AllowedOrigins []string
}

// HTTPTransport implements Transport using the stateless Streamable HTTP binding
// (2026-07-28): a single POST-only /mcp endpoint, no protocol-level sessions, no GET/DELETE.
type HTTPTransport struct {
	config      HTTPTransportConfig
	handler     MessageHandler
	server      *http.Server
	stopCh      chan struct{}
	wg          sync.WaitGroup
	authService *auth.AuthService
}

// NewHTTPTransport creates a new HTTP transport
func NewHTTPTransport(config HTTPTransportConfig) *HTTPTransport {
	// Set defaults
	if config.Host == "" {
		config.Host = "0.0.0.0"
	}
	if config.Port == 0 {
		config.Port = 8080
	}

	return &HTTPTransport{
		config:      config,
		stopCh:      make(chan struct{}),
		authService: config.AuthService,
	}
}

// Start begins the HTTP server
func (t *HTTPTransport) Start(handler MessageHandler) error {
	t.handler = handler

	mux := http.NewServeMux()

	// Register auth endpoints if auth is enabled
	if t.authService != nil {
		t.authService.RegisterRoutes(mux)
		t.authService.RegisterAdminRoutes(mux)

		// Wrap /mcp with auth middleware
		mux.Handle("/mcp", t.authService.Middleware(http.HandlerFunc(t.handleMCP)))
		logging.Info("OAuth authentication enabled")
	} else {
		// No auth - direct handler
		mux.HandleFunc("/mcp", t.handleMCP)
	}

	t.server = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", t.config.Host, t.config.Port),
		Handler: mux,
	}

	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		logging.Info("HTTP server listening", "addr", t.server.Addr, "transport", "Streamable HTTP")
		if err := t.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logging.Error("HTTP server error", "error", err)
		}
	}()

	return nil
}

// Stop gracefully stops the HTTP server
func (t *HTTPTransport) Stop() error {
	close(t.stopCh)

	if t.server != nil {
		if err := t.server.Close(); err != nil {
			return err
		}
	}

	t.wg.Wait()
	return nil
}

// handleMCP handles the /mcp endpoint for Streamable HTTP transport. Only POST is a
// defined operation in this protocol revision; GET and DELETE (session lifecycle from
// earlier revisions) are rejected with 405, per the 2026-07-28 backward-compatibility
// guidance for a server that supports only this revision.
func (t *HTTPTransport) handleMCP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Wrap response writer to capture details
	recorder := newResponseRecorder(w)

	origin := r.Header.Get("Origin")
	if !t.originAllowed(origin) {
		logging.Warn("HTTP request rejected: origin not allowed", "origin", origin, "remote_addr", r.RemoteAddr)
		recorder.Header().Set("Content-Type", "application/json")
		recorder.WriteHeader(http.StatusForbidden)
		recorder.Write(NewErrorResponse(nil, &RPCError{Code: InvalidRequest, Message: "Forbidden: origin not allowed"}))
		return
	}

	t.setCORSHeaders(recorder, r, origin)

	if r.Method == http.MethodOptions {
		recorder.WriteHeader(http.StatusOK)
		return
	}

	// Trace: Log request details
	if logging.IsTraceEnabled() {
		sanitizedHeaders := logging.SanitizeHeaders(r.Header)
		logging.Trace("HTTP request received",
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
			"headers", sanitizedHeaders)
	}

	switch r.Method {
	case http.MethodPost:
		t.handlePost(recorder, r)
	default:
		// GET, DELETE, and anything else: no such operation in this revision (no
		// sessions, no standalone SSE stream, no session teardown).
		recorder.Header().Set("Content-Type", "application/json")
		recorder.WriteHeader(http.StatusMethodNotAllowed)
		recorder.Write(NewErrorResponse(nil, &RPCError{Code: MethodNotFound, Message: "Method not allowed: only POST is supported"}))
	}

	// Log request completion
	duration := time.Since(start)

	if logging.IsDebugEnabled() {
		logArgs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.statusCode,
			"size", recorder.size,
			"duration_ms", duration.Milliseconds(),
			"remote_addr", r.RemoteAddr,
		}

		// Add user info if available
		if user := auth.GetUserFromContext(r.Context()); user != nil {
			logArgs = append(logArgs, "user_id", user.ID, "github_login", user.GitHubLogin)
		}

		logging.Debug("HTTP request completed", logArgs...)
	}

	// Trace: Log response body
	if logging.IsTraceEnabled() && recorder.body.Len() > 0 {
		logging.Trace("HTTP response body", "body", recorder.body.String())
	}
}

// originAllowed implements the Origin validation the Streamable HTTP binding requires to
// prevent DNS rebinding. A request without an Origin header (i.e. not issued by a
// browser) is always allowed — there is nothing to rebind.
func (t *HTTPTransport) originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	if len(t.config.AllowedOrigins) == 0 {
		return isDefaultLocalOrigin(origin)
	}
	for _, allowed := range t.config.AllowedOrigins {
		if allowed == "*" || strings.EqualFold(allowed, origin) {
			return true
		}
	}
	return false
}

func isDefaultLocalOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

func (t *HTTPTransport) setCORSHeaders(w http.ResponseWriter, r *http.Request, origin string) {
	if origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	} else if len(t.config.AllowedOrigins) == 1 && t.config.AllowedOrigins[0] == "*" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
		w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
	} else {
		w.Header().Set("Access-Control-Allow-Headers",
			"Content-Type, Accept, Authorization, "+ProtocolVersionHeader+", "+MethodHeader+", "+NameHeader)
	}
}

// reqParamsPeek extracts just enough of a request's params to validate headers against
// the body, without the transport package needing to know about MCP-level types (which
// live in the mcp package, itself a consumer of this one).
type reqParamsPeek struct {
	Name string                     `json:"name"`
	URI  string                     `json:"uri"`
	Meta map[string]json.RawMessage `json:"_meta"`
}

// validateHeaders enforces the Streamable HTTP requirement that MCP-Protocol-Version,
// Mcp-Method, and (for tools/call, resources/read, prompts/get) Mcp-Name are present and
// match the request body. Callers MUST NOT invoke this for "initialize" requests: legacy
// clients don't send these headers at all, and the caller is expected to let those
// through to the handler's own diagnostic instead of rejecting them here.
func (t *HTTPTransport) validateHeaders(r *http.Request, req JSONRPCRequest) error {
	pv := r.Header.Get(ProtocolVersionHeader)
	if pv == "" {
		return fmt.Errorf("missing required header %s", ProtocolVersionHeader)
	}

	var pp reqParamsPeek
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &pp)
	}

	if bodyPV, ok := stringFromRaw(pp.Meta["io.modelcontextprotocol/protocolVersion"]); ok && bodyPV != pv {
		return fmt.Errorf("%s header %q does not match body value %q", ProtocolVersionHeader, pv, bodyPV)
	}

	mcpMethod := r.Header.Get(MethodHeader)
	if mcpMethod == "" {
		return fmt.Errorf("missing required header %s", MethodHeader)
	}
	if mcpMethod != req.Method {
		return fmt.Errorf("%s header %q does not match body method %q", MethodHeader, mcpMethod, req.Method)
	}

	if requiresNameHeader(req.Method) {
		headerName := r.Header.Get(NameHeader)
		if headerName == "" {
			return fmt.Errorf("missing required header %s", NameHeader)
		}
		decoded, err := DecodeHeaderValue(headerName)
		if err != nil {
			return fmt.Errorf("invalid %s header encoding: %w", NameHeader, err)
		}
		bodyName := pp.Name
		if bodyName == "" {
			bodyName = pp.URI
		}
		if decoded != bodyName {
			return fmt.Errorf("%s header %q does not match body value %q", NameHeader, decoded, bodyName)
		}
	}

	return nil
}

func requiresNameHeader(method string) bool {
	switch method {
	case "tools/call", "resources/read", "prompts/get":
		return true
	}
	return false
}

func stringFromRaw(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// collectHeaders gathers the headers relevant to MCP-level (as opposed to
// transport-level) validation — chiefly Mcp-Param-* headers mirroring
// x-mcp-header-annotated tool parameters, which only the mcp package (holder of tool
// schemas) can validate.
func collectHeaders(r *http.Request) RequestHeaders {
	values := make(map[string]string)
	for name, vals := range r.Header {
		if len(vals) == 0 {
			continue
		}
		if strings.HasPrefix(name, ParamHeaderPrefix) ||
			strings.EqualFold(name, NameHeader) ||
			strings.EqualFold(name, MethodHeader) ||
			strings.EqualFold(name, ProtocolVersionHeader) {
			values[name] = vals[0]
		}
	}
	return RequestHeaders{Values: values}
}

func writeHTTPError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(NewErrorResponse(id, &RPCError{Code: code, Message: message}))
}

// discardResponseWriter is used for notification POSTs: any protocol-level notification
// handling happens for its side effects only. This revision defines no client-to-server
// notification delivered over Streamable HTTP (notifications/cancelled is stdio/UNIX
// only — on HTTP, closing the response stream is itself the cancellation signal), so in
// practice this exists to accept whatever a client sends without crashing on it.
type discardResponseWriter struct{}

func (discardResponseWriter) WriteNotification(string, interface{}) error { return nil }
func (discardResponseWriter) WriteMessage([]byte) error                   { return nil }

// handlePost handles POST requests, the only operation this transport defines.
func (t *HTTPTransport) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, nil, ParseError, "Failed to read request body")
		return
	}

	if logging.IsTraceEnabled() {
		logging.Trace("HTTP POST request body", "body", string(body))
	}

	var req JSONRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, nil, ParseError, "Invalid JSON")
		return
	}

	// "initialize" gets no header enforcement: a legacy client sending it has no idea
	// these headers exist. Let it reach the handler, which returns a diagnostic
	// UnsupportedProtocolVersion/MethodNotFound naming the versions we do support; that
	// error's code maps to the correct HTTP status via HTTPStatusForRPCError below.
	if req.Method != "initialize" {
		if verr := t.validateHeaders(r, req); verr != nil {
			logging.Debug("HTTP header validation failed", "error", verr, "remote_addr", r.RemoteAddr)
			writeHTTPError(w, http.StatusBadRequest, req.ID, HeaderMismatch, verr.Error())
			return
		}
	}

	ctx := WithRequestHeaders(r.Context(), collectHeaders(r))

	if req.IsNotification() {
		t.handler.HandleMessage(ctx, body, discardResponseWriter{})
		w.WriteHeader(http.StatusAccepted)
		return
	}

	rw := newHTTPResponseWriter(w)
	defer rw.closeDone()
	t.handler.HandleMessage(ctx, body, rw)
}

// httpResponseWriter implements ResponseWriter over a single HTTP response: the first
// write decides whether the response is a single JSON object or an SSE stream scoped to
// this request, per the Streamable HTTP binding.
type httpResponseWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	started bool
	sse     bool
	once    sync.Once
	done    chan struct{}
}

func newHTTPResponseWriter(w http.ResponseWriter) *httpResponseWriter {
	fl, _ := w.(http.Flusher)
	return &httpResponseWriter{w: w, flusher: fl, done: make(chan struct{})}
}

func (rw *httpResponseWriter) closeDone() {
	rw.once.Do(func() { close(rw.done) })
}

func (rw *httpResponseWriter) upgradeToSSELocked() {
	rw.w.Header().Set("Content-Type", "text/event-stream")
	rw.w.Header().Set("Cache-Control", "no-cache")
	rw.w.Header().Set("Connection", "keep-alive")
	rw.w.Header().Set("X-Accel-Buffering", "no")
	rw.w.WriteHeader(http.StatusOK)
	rw.started = true
	rw.sse = true
	if rw.flusher != nil {
		rw.flusher.Flush()
	}
	go rw.keepAlive()
}

// keepAlive periodically emits an SSE comment line so intermediaries and client idle
// timeouts don't close a quiet long-lived stream (chiefly subscriptions/listen).
func (rw *httpResponseWriter) keepAlive() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-rw.done:
			return
		case <-ticker.C:
			rw.mu.Lock()
			fmt.Fprint(rw.w, ":\r\n")
			if rw.flusher != nil {
				rw.flusher.Flush()
			}
			rw.mu.Unlock()
		}
	}
}

func (rw *httpResponseWriter) WriteNotification(method string, params interface{}) error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if !rw.started {
		rw.upgradeToSSELocked()
	}
	data, err := json.Marshal(struct {
		JSONRPC string      `json:"jsonrpc"`
		Method  string      `json:"method"`
		Params  interface{} `json:"params,omitempty"`
	}{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	return rw.writeSSEEventLocked(data)
}

func (rw *httpResponseWriter) WriteMessage(data []byte) error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	defer rw.closeDone()

	if !rw.started {
		status := http.StatusOK
		if code, ok := rpcErrorCode(data); ok {
			status = HTTPStatusForRPCError(code)
		}
		rw.w.Header().Set("Content-Type", "application/json")
		rw.w.WriteHeader(status)
		rw.started = true
		_, err := rw.w.Write(data)
		return err
	}
	// Once the response has already switched to an SSE stream, the HTTP status is
	// committed (200 OK) — an error occurring afterward can only be reported inside the
	// stream's final JSON-RPC message, per base JSON-RPC error semantics.
	return rw.writeSSEEventLocked(data)
}

// rpcErrorCode extracts a JSON-RPC error's numeric code from a marshaled response, if
// any. Used to pick the HTTP status for the *first* write of a response, per the
// Streamable HTTP binding's requirement that specific error codes map to specific
// non-200 statuses (e.g. UnsupportedProtocolVersionError -> 400, MethodNotFound -> 404).
func rpcErrorCode(data []byte) (int, bool) {
	var env struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil || env.Error == nil {
		return 0, false
	}
	return env.Error.Code, true
}

func (rw *httpResponseWriter) writeSSEEventLocked(data []byte) error {
	if _, err := fmt.Fprintf(rw.w, "data: %s\n\n", data); err != nil {
		return err
	}
	if rw.flusher != nil {
		rw.flusher.Flush()
	}
	return nil
}
