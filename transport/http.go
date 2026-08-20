package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/spirilis/generic-go-mcp/logging"
)

// defaultMaxBodyBytes bounds a POST /mcp body when HTTPTransportConfig.MaxBodyBytes is
// unset, matching the stdio framing's per-message ceiling (transport/stream.go).
const defaultMaxBodyBytes = 16 << 20 // 16 MiB

// AuthProvider is the subset of an authentication service that HTTPTransport needs to
// register OAuth routes and gate /mcp behind a token check. Declaring it here — rather
// than importing the auth package — keeps transport dependency-free (pure stdlib) for
// consumers who only need stdio or unauthenticated HTTP. auth.AuthService satisfies it;
// see auth/middleware.go's compile-time assertion.
type AuthProvider interface {
	RegisterRoutes(mux *http.ServeMux)
	RegisterAdminRoutes(mux *http.ServeMux)
	Middleware(next http.Handler) http.Handler
	// UserFromContext returns the authenticated user's id and login for the given
	// request context, and ok=false if the context carries no authenticated user.
	UserFromContext(ctx context.Context) (id, login string, ok bool)
}

// isNilAuthProvider reports whether v is a typed nil (e.g. a nil *auth.AuthService
// stored in the AuthProvider interface). A plain `v != nil` check is not enough here:
// an interface holding a nil pointer is itself non-nil, which would otherwise cause
// HTTPTransport to treat auth as enabled and panic when it calls through to the nil
// receiver. This mirrors the mistake a struct literal like
// HTTPTransportConfig{AuthService: authService} makes when authService is a nil
// *auth.AuthService assigned unconditionally.
func isNilAuthProvider(v AuthProvider) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

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
	AuthService AuthProvider // Optional auth provider (e.g. *auth.AuthService)

	// AllowedOrigins is the allow-list checked against a request's Origin header, per the
	// Streamable HTTP requirement to validate Origin and prevent DNS rebinding. A request
	// with no Origin header (i.e. not sent from a browser) is always allowed. If empty,
	// the default policy allows only http(s)://localhost and http(s)://127.0.0.1 (any
	// port) — appropriate for a server bound to loopback. Set to []string{"*"} to allow
	// any origin (e.g. behind a trusted reverse proxy that already restricts access).
	AllowedOrigins []string

	// LegacySessions, if non-nil, enables the legacy (2025-11-25 and earlier) Streamable
	// HTTP surface: GET /mcp opens a standalone SSE stream for the named session, DELETE
	// /mcp tears one down, and unknown-session handling and header-validation exemptions
	// apply to legacy requests. Nil (the default) means compatibility is off — GET and
	// DELETE both answer 405, exactly as this transport did before this field existed.
	// Typically supplied by a compatibility layer wrapping the MessageHandler passed to
	// Start (e.g. compat.Overlay.LegacySessions()).
	LegacySessions LegacySessions

	// MaxBodyBytes caps a single POST /mcp request body, so one oversized request cannot
	// exhaust memory in io.ReadAll. Zero selects defaultMaxBodyBytes (16 MiB); a negative
	// value disables the cap entirely (unbounded — not recommended on an exposed listener).
	MaxBodyBytes int64
}

// HTTPTransport implements Transport using the stateless Streamable HTTP binding
// (2026-07-28): a single POST-only /mcp endpoint, no protocol-level sessions, no GET/DELETE
// — unless HTTPTransportConfig.LegacySessions is set, which restores GET/DELETE for the
// legacy requests a compatibility layer claims.
type HTTPTransport struct {
	config         HTTPTransportConfig
	handler        MessageHandler
	server         *http.Server
	stopCh         chan struct{}
	wg             sync.WaitGroup
	authService    AuthProvider
	legacySessions LegacySessions
}

// isNilLegacySessions reports whether v is a typed nil, the same class of bug
// isNilAuthProvider guards against: a caller assigning a nil concrete pointer to this
// interface field unconditionally must not be treated as "compatibility enabled."
func isNilLegacySessions(v LegacySessions) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
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
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = defaultMaxBodyBytes
	}

	authService := config.AuthService
	if isNilAuthProvider(authService) {
		// Guard against a typed-nil AuthProvider (e.g. a caller assigning a nil
		// *auth.AuthService unconditionally) — treat it the same as auth disabled.
		authService = nil
	}

	legacySessions := config.LegacySessions
	if isNilLegacySessions(legacySessions) {
		legacySessions = nil
	}

	return &HTTPTransport{
		config:         config,
		stopCh:         make(chan struct{}),
		authService:    authService,
		legacySessions: legacySessions,
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
		// Bound how long a client may take to send its request, so a slow-loris client
		// cannot tie up a connection indefinitely. WriteTimeout is deliberately left unset:
		// a subscriptions/listen response is a long-lived SSE stream, and a write deadline
		// would sever it. Body size is bounded separately (MaxBodyBytes), and the stream's
		// own keep-alive guards idle intermediaries.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
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
// guidance for a server that supports only this revision — unless legacySessions is set,
// in which case GET opens a standalone SSE stream and DELETE tears a legacy session down,
// for the legacy clients the compatibility layer serves.
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
	case http.MethodGet:
		if t.legacySessions == nil {
			t.methodNotAllowed(recorder)
		} else {
			t.handleGet(recorder, r)
		}
	case http.MethodDelete:
		if t.legacySessions == nil {
			t.methodNotAllowed(recorder)
		} else {
			t.handleDelete(recorder, r)
		}
	default:
		t.methodNotAllowed(recorder)
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
		if t.authService != nil {
			if id, login, ok := t.authService.UserFromContext(r.Context()); ok {
				logArgs = append(logArgs, "user_id", id, "github_login", login)
			}
		}

		logging.Debug("HTTP request completed", logArgs...)
	}

	// Trace: Log response body
	if logging.IsTraceEnabled() && recorder.body.Len() > 0 {
		logging.Trace("HTTP response body", "body", recorder.body.String())
	}
}

// methodNotAllowed answers a request for an HTTP method this transport does not define an
// operation for on /mcp — always true for anything but POST/GET/DELETE, and true for
// GET/DELETE too when legacySessions is nil (compatibility off).
func (t *HTTPTransport) methodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusMethodNotAllowed)
	w.Write(NewErrorResponse(nil, &RPCError{Code: MethodNotFound, Message: "Method not allowed"}))
}

// handleGet implements the legacy standalone SSE stream: GET /mcp, scoped to the session
// named by the Mcp-Session-Id header. Only reachable when t.legacySessions is non-nil.
// The stream stays open — emitting change notifications as they occur, plus the
// httpResponseWriter's periodic keep-alive comment — until the client disconnects or the
// session ends (TTL expiry, explicit DELETE, or a re-handshake replacing it); both exits
// are honored by selecting on the request's own context and the session's Done channel.
func (t *HTTPTransport) handleGet(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get(SessionIDHeader)
	if sessionID == "" || !t.legacySessions.Known(sessionID) {
		writeHTTPError(w, http.StatusNotFound, nil, InvalidRequest, "Session not found")
		return
	}

	done, ok := t.legacySessions.Done(sessionID)
	if !ok {
		// Ended between the Known check above and here — treat as never found.
		writeHTTPError(w, http.StatusNotFound, nil, InvalidRequest, "Session not found")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()

	rw := newHTTPResponseWriter(w)
	defer rw.closeDone()
	// A standalone GET stream opens text/event-stream immediately — there is no final
	// JSON-RPC message to wait for, unlike a request-scoped notification stream that only
	// upgrades lazily on its first WriteNotification.
	rw.startSSE()

	if err := t.legacySessions.StreamNotifications(ctx, sessionID, rw); err != nil {
		logging.Debug("legacy SSE stream ended", "session", sessionID, "error", err)
	}
}

// handleDelete implements legacy session teardown: DELETE /mcp, scoped to the session
// named by the Mcp-Session-Id header. Only reachable when t.legacySessions is non-nil.
func (t *HTTPTransport) handleDelete(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get(SessionIDHeader)
	if sessionID == "" || !t.legacySessions.Terminate(sessionID) {
		writeHTTPError(w, http.StatusNotFound, nil, InvalidRequest, "Session not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	if t.legacySessions != nil {
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, DELETE, OPTIONS")
		// A browser client cannot read the Mcp-Session-Id header the legacy handshake
		// mints unless it is explicitly exposed — merely allowing it in the request
		// direction is not enough, and every later request would then fail.
		w.Header().Set("Access-Control-Expose-Headers", SessionIDHeader)
	} else {
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	}
	if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
		w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
	} else {
		w.Header().Set("Access-Control-Allow-Headers",
			"Content-Type, Accept, Authorization, "+ProtocolVersionHeader+", "+MethodHeader+", "+NameHeader+", "+SessionIDHeader)
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
// match the request body. Callers MUST NOT invoke this for "initialize" requests, nor
// (when a compatibility layer is enabled) for any other request a legacy client sent:
// those revisions predate these headers entirely, and the caller is expected to let such
// requests through to the handler's own diagnostic (or the compatibility layer) instead
// of rejecting them here.
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

// isLegacyRequest reports whether req should be routed to a legacy-compatibility layer
// rather than validated as a modern (2026-07-28) request: either it's the legacy
// handshake method itself, or it lacks the modern protocol-version _meta key that every
// genuine 2026-07-28 request carries. See DeclaresModernProtocol for why this predicate,
// and no other, is the right one.
func isLegacyRequest(req JSONRPCRequest) bool {
	return req.Method == "initialize" || !DeclaresModernProtocol(req.Params)
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
	if t.config.MaxBodyBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, t.config.MaxBodyBytes)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeHTTPError(w, http.StatusRequestEntityTooLarge, nil, InvalidRequest, "Request body too large")
			return
		}
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

	// legacy is only ever true when a compatibility layer is actually enabled — with
	// t.legacySessions nil, every request is treated as modern except "initialize"
	// itself, unchanged from this transport's original modern-only behavior.
	legacy := t.legacySessions != nil && isLegacyRequest(req)
	sessionID := r.Header.Get(SessionIDHeader)

	// Rule 1: an unknown session is a 404, so the client knows to re-handshake — but only
	// for a legacy request that isn't "initialize". A modern request carrying a leftover
	// session header must have it ignored, per the stateless binding; "initialize" is
	// exempt because establishing a new session is exactly what it's for, and the client
	// will still be holding the dead id when it retries.
	if legacy && req.Method != "initialize" && sessionID != "" && !t.legacySessions.Known(sessionID) {
		writeHTTPError(w, http.StatusNotFound, req.ID, InvalidRequest, "Session not found")
		return
	}

	// Rule 2: the 2026-07-28 header requirements (MCP-Protocol-Version, Mcp-Method,
	// Mcp-Name) bind modern requests only. "initialize" is always exempt (a legacy client
	// sending it has no idea these headers exist, and the modern-only diagnostic needs to
	// see it unvalidated); every other legacy request is exempt only once a compatibility
	// layer is actually claiming it, since demanding these headers of a client speaking a
	// revision where they don't exist would reject every legacy call before the
	// compatibility layer ever saw it.
	if req.Method != "initialize" && !legacy {
		if verr := t.validateHeaders(r, req); verr != nil {
			logging.Debug("HTTP header validation failed", "error", verr, "remote_addr", r.RemoteAddr)
			writeHTTPError(w, http.StatusBadRequest, req.ID, HeaderMismatch, verr.Error())
			return
		}
	}

	ctx := WithRequestHeaders(r.Context(), collectHeaders(r))
	ctx = WithRequestInfo(ctx, &RequestInfo{SessionID: sessionID, ProtocolVersion: r.Header.Get(ProtocolVersionHeader)})

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

// startSSE forces the immediate upgrade to text/event-stream, for a standalone GET stream
// that has no final JSON-RPC response to wait for and so cannot rely on the lazy
// first-WriteNotification upgrade every other response on this writer uses.
func (rw *httpResponseWriter) startSSE() {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if !rw.started {
		rw.upgradeToSSELocked()
	}
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

// SetResponseHeader implements HeaderSetter: a legacy-compatibility layer uses it to mint
// Mcp-Session-Id on a successful "initialize". Per the interface contract, a header set
// after the first byte of the response is written is silently dropped — the handshake
// must set it before returning its result.
func (rw *httpResponseWriter) SetResponseHeader(name, value string) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.started {
		return
	}
	rw.w.Header().Set(name, value)
}

var _ HeaderSetter = (*httpResponseWriter)(nil)

func (rw *httpResponseWriter) writeSSEEventLocked(data []byte) error {
	if _, err := fmt.Fprintf(rw.w, "data: %s\n\n", data); err != nil {
		return err
	}
	if rw.flusher != nil {
		rw.flusher.Flush()
	}
	return nil
}
