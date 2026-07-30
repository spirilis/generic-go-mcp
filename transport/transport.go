package transport

import (
	"context"
	"encoding/json"
	"net/http"
)

// Transport defines the interface for different communication mechanisms
type Transport interface {
	Start(handler MessageHandler) error
	Stop() error
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
