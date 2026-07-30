package mcp

import (
	"encoding/json"

	"github.com/spirilis/generic-go-mcp/transport"
)

// Reserved io.modelcontextprotocol/* _meta keys (2026-07-28).
const (
	metaKeyProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	metaKeyClientInfo         = "io.modelcontextprotocol/clientInfo"
	metaKeyClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	metaKeyLogLevel           = "io.modelcontextprotocol/logLevel"
	metaKeyServerInfo         = "io.modelcontextprotocol/serverInfo"
	metaKeySubscriptionID     = "io.modelcontextprotocol/subscriptionId"
	metaKeyProgressToken      = "progressToken"
)

// Implementation identifies a client or server by name and version. It is self-reported
// and MUST NOT be used for security decisions — display, logging, and debugging only.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// RootsCapability, SamplingCapability-shaped maps, etc. are represented loosely below:
// Roots/Sampling are deprecated in this revision and this server never requires them, so
// ClientCapabilities only needs to detect *presence* (a client offering the capability),
// not parse its internal shape.

// ClientCapabilities is the client capabilities relevant to a single request, as declared
// in io.modelcontextprotocol/clientCapabilities. Every request is required to include
// this field (possibly as an empty object): the server must not assume any capability the
// client hasn't declared on this specific request.
type ClientCapabilities struct {
	Roots       map[string]interface{}     `json:"roots,omitempty"`
	Sampling    map[string]interface{}     `json:"sampling,omitempty"`
	Elicitation map[string]interface{}     `json:"elicitation,omitempty"`
	Extensions  map[string]json.RawMessage `json:"extensions,omitempty"`
}

// HasElicitation reports whether the client declared support for elicitation on this
// request — required before a tool may send an elicitation/create MRTR inputRequest.
func (c *ClientCapabilities) HasElicitation() bool {
	return c != nil && c.Elicitation != nil
}

// HasSampling reports whether the client declared support for sampling on this request.
func (c *ClientCapabilities) HasSampling() bool {
	return c != nil && c.Sampling != nil
}

// HasRoots reports whether the client declared support for roots on this request.
func (c *ClientCapabilities) HasRoots() bool {
	return c != nil && c.Roots != nil
}

// RequestMeta is the parsed, validated form of a request's params._meta. Every request
// this server routes (other than the legacy "initialize" diagnostic) goes through
// ParseRequestMeta before dispatch.
type RequestMeta struct {
	ProtocolVersion    string
	ClientInfo         *Implementation
	ClientCapabilities *ClientCapabilities
	LogLevel           string
	ProgressToken      json.RawMessage
}

// paramsMetaEnvelope peeks at params._meta without requiring the caller to know the rest
// of a method's param shape.
type paramsMetaEnvelope struct {
	Meta map[string]json.RawMessage `json:"_meta"`
}

// ParseRequestMeta extracts and validates the per-request protocol fields every MCP
// request (other than the legacy "initialize" diagnostic) is required to carry. A
// missing required field is a malformed request (-32602); an unsupported protocol
// version is reported via UnsupportedProtocolVersionError (-32022), but the parsed
// RequestMeta is still returned in that case so callers needing to log or inspect it can.
func ParseRequestMeta(params json.RawMessage) (*RequestMeta, *transport.RPCError) {
	var env paramsMetaEnvelope
	if len(params) > 0 {
		if err := json.Unmarshal(params, &env); err != nil {
			return nil, invalidParamsErr("invalid params: %v", err)
		}
	}
	meta := env.Meta

	pv, ok := stringFromRaw(meta[metaKeyProtocolVersion])
	if !ok || pv == "" {
		return nil, invalidParamsErr("missing required _meta field %q", metaKeyProtocolVersion)
	}

	rawCaps, present := meta[metaKeyClientCapabilities]
	if !present {
		return nil, invalidParamsErr("missing required _meta field %q", metaKeyClientCapabilities)
	}
	caps := &ClientCapabilities{}
	if err := json.Unmarshal(rawCaps, caps); err != nil {
		return nil, invalidParamsErr("invalid _meta field %q: %v", metaKeyClientCapabilities, err)
	}

	var clientInfo *Implementation
	if raw, present := meta[metaKeyClientInfo]; present {
		clientInfo = &Implementation{}
		if err := json.Unmarshal(raw, clientInfo); err != nil {
			return nil, invalidParamsErr("invalid _meta field %q: %v", metaKeyClientInfo, err)
		}
	}

	logLevel, _ := stringFromRaw(meta[metaKeyLogLevel])
	progressToken := meta[metaKeyProgressToken]

	rm := &RequestMeta{
		ProtocolVersion:    pv,
		ClientInfo:         clientInfo,
		ClientCapabilities: caps,
		LogLevel:           logLevel,
		ProgressToken:      progressToken,
	}

	if !isSupportedVersion(pv) {
		return rm, unsupportedProtocolVersionErr(pv)
	}

	return rm, nil
}
