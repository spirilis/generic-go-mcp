package transport

import (
	"context"
	"encoding/json"
	"net/http"
)

// Transport defines the interface for different communication mechanisms.
//
// Start is non-blocking: it launches the transport's own goroutine(s) and returns. Stop
// initiates shutdown; whether it waits for anything is the individual transport's business
// (StdioTransport's, notably, returns immediately — see its documentation). A transport
// whose run can end on its own additionally implements DoneNotifier.
type Transport interface {
	Start(handler MessageHandler) error
	Stop() error
}

// DoneNotifier is implemented by a Transport whose run has a natural end, so that
// transport-agnostic startup code can wait for it without knowing which transport it holds:
//
//	var transDone <-chan struct{}
//	if d, ok := trans.(transport.DoneNotifier); ok {
//		transDone = d.Done()
//	}
//	select {
//	case <-sigCh:
//	case <-transDone: // nil for a transport that never ends on its own; blocks forever
//	}
//
// Currently only StdioTransport: its run ends when its input reaches EOF, which is the
// portable shutdown signal for a stdio server. It is deliberately NOT implemented by
// HTTPTransport or UnixTransport — those end only when Stop is called, so a Done there would
// report "somebody stopped me", not "my peer went away", and inviting a caller to exit on it
// would be inviting a bug.
type DoneNotifier interface {
	// Done returns a channel closed once the transport's run has ended. It is valid to call
	// at any point in the transport's life, including before Start.
	Done() <-chan struct{}
}

// MessageHandler processes incoming JSON-RPC messages. Implementations MUST be safe for
// concurrent use: a transport may invoke HandleMessage for multiple in-flight requests at
// once (e.g. one call parked in subscriptions/listen while another is a quick tools/call).
type MessageHandler interface {
	// HandleMessage processes a single JSON-RPC request or notification carried in data.
	//
	// For a notification (no "id" field in the message), the handler MUST NOT call
	// w.WriteMessage.
	//
	// For a request, the handler MUST eventually call w.WriteMessage exactly once with the
	// final JSON-RPC response, optionally preceded by any number of w.WriteNotification
	// calls (progress, or notifications delivered on a subscriptions/listen stream).
	// HandleMessage returns once the exchange for this message is complete; for a
	// long-lived request such as subscriptions/listen, that may not happen until ctx is
	// cancelled.
	HandleMessage(ctx context.Context, data []byte, w ResponseWriter)
}

// ResponseWriter emits server-to-client messages belonging to one in-flight client request.
type ResponseWriter interface {
	// WriteNotification emits a request-scoped JSON-RPC notification (e.g.
	// notifications/progress, or a notification delivered on a subscriptions/listen
	// stream). On the Streamable HTTP transport, the first call upgrades the response to
	// text/event-stream.
	WriteNotification(method string, params interface{}) error
	// WriteMessage emits the final JSON-RPC response for the request and signals that no
	// further notifications will follow for it. Calling it more than once per request is a
	// caller bug.
	WriteMessage(data []byte) error
}

// HeaderSetter is implemented by a ResponseWriter over a transport that has response
// headers (currently: Streamable HTTP, and the in-memory BufferedResponseWriter test
// double). A legacy-compatibility layer uses it to mint Mcp-Session-Id at handshake time.
// A header set after the first byte of the response body is written is silently dropped.
//
// It is deliberately NOT implemented on the stdio/UNIX stream ResponseWriter: a byte
// stream has no headers, and that absence is itself the signal a compatibility layer uses
// to decide whether to mint a per-connection session or fall back to stdio's one implicit
// session.
type HeaderSetter interface {
	SetResponseHeader(name, value string)
}

// requestInfoKey is the context key under which RequestInfo is stored.
type requestInfoKey struct{}

// RequestInfo carries the subset of transport-level request state a legacy-compatibility
// layer needs but the request body doesn't itself carry: the session id and protocol
// version a legacy client sent as headers (Streamable HTTP) rather than in params._meta.
type RequestInfo struct {
	// SessionID is the Mcp-Session-Id request header. Legacy Streamable HTTP requests
	// only; empty on stdio/UNIX and on modern requests.
	SessionID string
	// ProtocolVersion is the MCP-Protocol-Version request header, exactly as sent.
	ProtocolVersion string
}

// WithRequestInfo attaches info to ctx.
func WithRequestInfo(ctx context.Context, info *RequestInfo) context.Context {
	return context.WithValue(ctx, requestInfoKey{}, info)
}

// RequestInfoFromContext retrieves RequestInfo previously attached with
// WithRequestInfo. Returns nil when no RequestInfo was attached — notably, always nil on
// stdio/UNIX, which has no header layer to populate it from.
func RequestInfoFromContext(ctx context.Context) *RequestInfo {
	info, _ := ctx.Value(requestInfoKey{}).(*RequestInfo)
	return info
}

// LegacySessions is the session-management surface a legacy-compatibility layer exposes to
// the Streamable HTTP binding, without the transport package needing to import the
// protocol package that implements it (that would be a cycle: transport is a dependency of
// both mcp and any compatibility layer built on it).
//
// Passed via HTTPTransportConfig.LegacySessions. A nil value means compatibility is off,
// and drives the entire legacy HTTP surface (GET/DELETE on /mcp, Mcp-Session-Id handling)
// being disabled — callers implementing this MUST return a genuinely nil interface value
// when there is no legacy stack, not a non-nil interface holding a nil pointer.
type LegacySessions interface {
	// Known reports whether id names a live (not expired, not terminated) session.
	Known(id string) bool
	// Terminate ends the session named id, reporting whether it existed. Idempotent: a
	// second call for the same id returns false without error.
	Terminate(id string) bool
	// Done returns a channel closed when the session named id ends (expiry, explicit
	// Terminate, or replacement by a re-handshake on the same key), for a standalone GET
	// stream to select on alongside the request's own context. ok is false if id does not
	// name a live session.
	Done(id string) (<-chan struct{}, bool)
	// StreamNotifications relays change notifications for the session named id to w, in
	// the legacy notification shape, until ctx is done or the session ends. Used to drive
	// the standalone GET /mcp SSE stream.
	StreamNotifications(ctx context.Context, id string, w ResponseWriter) error
}

// JSONRPCRequest represents a JSON-RPC 2.0 request or notification.
//
// ID is kept as json.RawMessage rather than a decoded interface{} so that request IDs
// round-trip byte-for-byte (an incoming `"id": 0` must not be dropped by `omitempty` on a
// zero Go int, and a string id must not be coerced to a number or vice versa). A
// notification is any message with no "id" key at all, i.e. len(ID) == 0.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether the request carries no "id" and is therefore a
// notification: the receiver MUST NOT send a response.
func (r *JSONRPCRequest) IsNotification() bool {
	return len(r.ID) == 0
}

// ModernProtocolMetaKey is the params._meta key whose presence marks a request as
// speaking the modern (2026-07-28) protocol envelope. See DeclaresModernProtocol.
const ModernProtocolMetaKey = "io.modelcontextprotocol/protocolVersion"

// DeclaresModernProtocol reports whether params (a JSON-RPC request's params) carries
// ModernProtocolMetaKey inside _meta — the only unambiguous marker of the 2026-07-28 wire
// format. The connection-scoped MCP revisions (2025-11-25 and earlier) negotiate a
// protocol version once, at a now-removed initialize handshake, and never name one in
// _meta on any later request — so its presence means modern, and its absence means
// legacy. Do not infer the era from the method name, the params shape, or HTTP headers.
//
// This lives here, rather than in a legacy-compatibility layer built on top of transport,
// because this transport's own Streamable HTTP binding needs the same verdict before the
// protocol layer is even entered (to decide whether to enforce modern headers and
// validate a session id) — and two independent implementations of "is this legacy?" that
// can disagree is a bug factory. A compatibility layer should call this function (or
// re-export it) rather than reimplementing the check.
func DeclaresModernProtocol(params json.RawMessage) bool {
	if len(params) == 0 {
		return false
	}
	var env struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(params, &env); err != nil {
		return false
	}
	_, ok := env.Meta[ModernProtocolMetaKey]
	return ok
}

// JSONRPCResponse represents a JSON-RPC 2.0 response (result or error).
type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError represents a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return e.Message
}

// Standard JSON-RPC 2.0 error codes.
const (
	ParseError     = -32700
	InvalidRequest = -32600
	MethodNotFound = -32601
	InvalidParams  = -32602
	InternalError  = -32603
)

// MCP-defined error codes (2026-07-28), allocated from the -32020..-32099 sub-range the
// MCP specification reserves for itself. Earlier revisions' -32002 (resource not found)
// and -32042 (URL elicitation required) are retired and MUST NOT be emitted by this
// revision.
const (
	// HeaderMismatch indicates the Streamable HTTP request's headers (MCP-Protocol-Version,
	// Mcp-Method, Mcp-Name, Mcp-Param-*) do not match the corresponding values in the
	// request body, or a required header is missing/malformed.
	HeaderMismatch = -32020
	// MissingRequiredClientCapability indicates the server needed a capability
	// (e.g. elicitation) that the client did not declare in its request's
	// io.modelcontextprotocol/clientCapabilities.
	MissingRequiredClientCapability = -32021
	// UnsupportedProtocolVersion indicates the server does not implement the protocol
	// version the request declared.
	UnsupportedProtocolVersion = -32022
)

// HTTPStatusForRPCError maps a JSON-RPC error code to the HTTP status the Streamable HTTP
// binding requires for it. Errors outside this list are valid JSON-RPC errors delivered
// inside a normal 200 OK response body (per base JSON-RPC semantics); only the
// transport-validation failures below get a non-2xx status.
func HTTPStatusForRPCError(code int) int {
	switch code {
	case InvalidParams, HeaderMismatch, MissingRequiredClientCapability, UnsupportedProtocolVersion:
		return http.StatusBadRequest
	case MethodNotFound:
		return http.StatusNotFound
	default:
		return http.StatusOK
	}
}

// NewSuccessResponse marshals a successful JSON-RPC response for id.
func NewSuccessResponse(id json.RawMessage, result interface{}) []byte {
	data, _ := json.Marshal(JSONRPCResponse{JSONRPC: "2.0", ID: nullIfEmpty(id), Result: result})
	return data
}

// NewErrorResponse marshals a JSON-RPC error response for id.
func NewErrorResponse(id json.RawMessage, err *RPCError) []byte {
	data, _ := json.Marshal(JSONRPCResponse{JSONRPC: "2.0", ID: nullIfEmpty(id), Error: err})
	return data
}

func nullIfEmpty(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}
