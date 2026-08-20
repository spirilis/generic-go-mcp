package mcp

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spirilis/generic-go-mcp/logging"
	"github.com/spirilis/generic-go-mcp/transport"
)

// invalidParamsErr builds a -32602 Invalid params error. This is the code used for
// malformed requests, unknown tool/resource names, and missing/invalid _meta fields.
func invalidParamsErr(format string, args ...interface{}) *transport.RPCError {
	return &transport.RPCError{Code: transport.InvalidParams, Message: fmt.Sprintf(format, args...)}
}

// internalErr wraps an unexpected Go error (as opposed to a tool execution error, which
// is reported as isError:true in a normal result, not a JSON-RPC error) as -32603. The
// underlying detail is logged rather than put on the wire: an internal failure's message
// can carry implementation detail (paths, upstream addresses, query fragments) a client has
// no business seeing. Enable debug logging to see it.
func internalErr(err error) *transport.RPCError {
	logging.Debug("internal error", "error", err)
	return &transport.RPCError{Code: transport.InternalError, Message: "Internal error"}
}

// readErr classifies an error from a ResourceFunction or ResourceTemplateFunction. A
// resource the handler says is absent is -32602, the same answer an unregistered URI gets —
// "you asked about something that isn't here" is a client-side fact, not a server fault.
// Anything else is a genuine -32603. See ErrResourceNotFound.
func readErr(uri string, err error) *transport.RPCError {
	if errors.Is(err, ErrResourceNotFound) {
		return invalidParamsErr("Unknown resource: %s", uri)
	}
	return internalErr(err)
}

// buildUnsupportedProtocolVersionErr builds the -32022 error a request gets when it
// declares a protocol version this server does not implement, listing advertised as the
// alternatives a client can retry with.
func buildUnsupportedProtocolVersionErr(requested string, advertised []string) *transport.RPCError {
	return &transport.RPCError{
		Code:    transport.UnsupportedProtocolVersion,
		Message: "Unsupported protocol version",
		Data:    map[string]interface{}{"supported": advertised, "requested": requested},
	}
}

// unsupportedProtocolVersionErr is used by ParseRequestMeta, which has no access to a
// particular Server and so always validates strictly against the modern-only
// SupportedVersions — a request declaring a legacy version inside the modern _meta
// envelope is nonsense regardless of what any compatibility layer additionally serves.
func unsupportedProtocolVersionErr(requested string) *transport.RPCError {
	return buildUnsupportedProtocolVersionErr(requested, SupportedVersions)
}

// unsupportedProtocolVersionErrFor reports the wider ServerConfig.AdvertisedVersions list
// as alternatives, once ParseRequestMeta has already determined (against the strict,
// modern-only list) that the request's declared version is unsupported. Used by
// Server.HandleMessage so this error, server/discover, and the initialize diagnostic never
// tell a client three different stories about what the server speaks.
func (s *Server) unsupportedProtocolVersionErrFor(requested string) *transport.RPCError {
	return buildUnsupportedProtocolVersionErr(requested, s.advertisedVersions())
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
// versions this server does support (ServerConfig.AdvertisedVersions) since that message
// may be the only diagnostic a legacy-only client can surface to its user.
func (s *Server) legacyInitializeError() *transport.RPCError {
	return &transport.RPCError{
		Code: transport.MethodNotFound,
		Message: "Method not found: \"initialize\" is not implemented. This server speaks " +
			"MCP protocol version 2026-07-28, which has no initialize handshake — every " +
			"request carries its own protocol version and capabilities.",
		Data: map[string]interface{}{"supported": s.advertisedVersions()},
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
