package mcp

import (
	"fmt"
	"strings"

	"github.com/spirilis/generic-go-mcp/transport"
)

// invalidParamsErr builds a -32602 Invalid params error. This is the code used for
// malformed requests, unknown tool/resource names, and missing/invalid _meta fields.
func invalidParamsErr(format string, args ...interface{}) *transport.RPCError {
	return &transport.RPCError{Code: transport.InvalidParams, Message: fmt.Sprintf(format, args...)}
}

// internalErr wraps an unexpected Go error (as opposed to a tool execution error, which
// is reported as isError:true in a normal result, not a JSON-RPC error) as -32603.
func internalErr(err error) *transport.RPCError {
	return &transport.RPCError{Code: transport.InternalError, Message: err.Error()}
}

// unsupportedProtocolVersionErr builds the -32022 error a request gets when it declares a
// protocol version this server does not implement.
func unsupportedProtocolVersionErr(requested string) *transport.RPCError {
	return &transport.RPCError{
		Code:    transport.UnsupportedProtocolVersion,
		Message: "Unsupported protocol version",
		Data:    map[string]interface{}{"supported": SupportedVersions, "requested": requested},
	}
}

// missingClientCapabilityErr builds the -32021 error returned when a request needs a
// capability (e.g. elicitation, for MRTR) the client did not declare.
func missingClientCapabilityErr(caps ...string) *transport.RPCError {
	return &transport.RPCError{
		Code:    transport.MissingRequiredClientCapability,
		Message: "Missing required client capability: " + strings.Join(caps, ", "),
		Data:    map[string]interface{}{"requiredCapabilities": caps},
	}
}

// headerMismatchErr builds the -32020 error for a Streamable HTTP header/body mismatch
// detected at the MCP protocol layer (as opposed to the transport-level header presence
// checks in transport/http.go).
func headerMismatchErr(format string, args ...interface{}) *transport.RPCError {
	return &transport.RPCError{Code: transport.HeaderMismatch, Message: fmt.Sprintf(format, args...)}
}

// legacyInitializeError is what this server returns to an "initialize" request: this
// revision has no handshake, so the method simply doesn't exist, but the error names the
// versions this server does support since that message may be the only diagnostic a
// legacy-only client can surface to its user.
func legacyInitializeError() *transport.RPCError {
	return &transport.RPCError{
		Code: transport.MethodNotFound,
		Message: "Method not found: \"initialize\" is not implemented. This server speaks " +
			"MCP protocol version 2026-07-28, which has no initialize handshake — every " +
			"request carries its own protocol version and capabilities.",
		Data: map[string]interface{}{"supported": SupportedVersions},
	}
}

// MissingCapabilityError is returned by ToolRequest.NeedInput when the caller asked to
// send an inputRequests entry (e.g. elicitation/create) the client never declared support
// for. handleToolsCall translates it into missingClientCapabilityErr.
type MissingCapabilityError struct {
	Capabilities []string
}

func (e *MissingCapabilityError) Error() string {
	return "missing required client capability: " + strings.Join(e.Capabilities, ", ")
}
