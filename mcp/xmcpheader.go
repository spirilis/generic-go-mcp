package mcp

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/spirilis/generic-go-mcp/transport"
)

// xmcpHeaderBinding pairs a path (a chain of object property names) into a tool's
// arguments with the HTTP header name that path's value must be mirrored into.
type xmcpHeaderBinding struct {
	path   []string
	header string
}

// extractXMCPHeaderBindings walks a tool's inputSchema, collecting every property
// annotated with x-mcp-header that is reachable via a chain of "properties" keys only —
// the "statically reachable" restriction the spec places on where x-mcp-header may
// appear (not through items, oneOf/anyOf/allOf, if/then/else, or $ref).
func extractXMCPHeaderBindings(schema json.RawMessage) ([]xmcpHeaderBinding, *transport.RPCError) {
	if len(schema) == 0 {
		return nil, nil
	}
	var root struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &root); err != nil || len(root.Properties) == 0 {
		return nil, nil
	}
	var bindings []xmcpHeaderBinding
	if err := walkXMCPProperties(root.Properties, nil, &bindings); err != nil {
		return nil, err
	}
	return bindings, nil
}

func walkXMCPProperties(props map[string]json.RawMessage, prefix []string, out *[]xmcpHeaderBinding) *transport.RPCError {
	for name, raw := range props {
		var prop struct {
			XMCPHeader string                     `json:"x-mcp-header"`
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(raw, &prop); err != nil {
			continue
		}
		path := append(append([]string{}, prefix...), name)
		if prop.XMCPHeader != "" {
			if prop.Type == "number" {
				return headerMismatchErr("x-mcp-header on %q: type \"number\" is not permitted", strings.Join(path, "."))
			}
			*out = append(*out, xmcpHeaderBinding{path: path, header: transport.ParamHeaderPrefix + prop.XMCPHeader})
		}
		if len(prop.Properties) > 0 {
			if err := walkXMCPProperties(prop.Properties, path, out); err != nil {
				return err
			}
		}
	}
	return nil
}

func lookupArgPath(args map[string]json.RawMessage, path []string) (json.RawMessage, bool) {
	if len(path) == 0 || args == nil {
		return nil, false
	}
	raw, ok := args[path[0]]
	if !ok {
		return nil, false
	}
	if len(path) == 1 {
		return raw, true
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil {
		return nil, false
	}
	return lookupArgPath(nested, path[1:])
}

// argValueMatchesHeader compares an argument's raw JSON value against a decoded header
// value, per the type-conversion rules in the spec (string as-is, integer as decimal,
// boolean as lowercase true/false) — comparing integers numerically rather than as
// strings, so "42" and "42.0" are treated as equal.
func argValueMatchesHeader(argVal json.RawMessage, headerVal string) bool {
	var s string
	if err := json.Unmarshal(argVal, &s); err == nil {
		return s == headerVal
	}
	var b bool
	if err := json.Unmarshal(argVal, &b); err == nil {
		return (b && headerVal == "true") || (!b && headerVal == "false")
	}
	var n json.Number
	if err := json.Unmarshal(argVal, &n); err == nil {
		hv, err := strconv.ParseFloat(headerVal, 64)
		if err != nil {
			return false
		}
		nv, err := n.Float64()
		if err != nil {
			return false
		}
		return hv == nv
	}
	return false
}

// validateToolHeaders checks any Mcp-Param-* headers attached to ctx (by the Streamable
// HTTP transport) against tool's x-mcp-header-annotated arguments, per
// streamable-http#custom-headers-from-tool-parameters. It is a no-op when the transport
// attached no headers at all (stdio/UNIX, which have no header layer) or the tool
// declares no x-mcp-header annotations.
func validateToolHeaders(ctx context.Context, tool Tool, arguments json.RawMessage) *transport.RPCError {
	headers, ok := transport.HeadersFromContext(ctx)
	if !ok {
		return nil
	}
	bindings, err := extractXMCPHeaderBindings(tool.InputSchema)
	if err != nil {
		return err
	}
	if len(bindings) == 0 {
		return nil
	}

	var args map[string]json.RawMessage
	if len(arguments) > 0 {
		_ = json.Unmarshal(arguments, &args)
	}

	for _, b := range bindings {
		headerVal, headerPresent := headers.Get(b.header)
		argVal, argPresent := lookupArgPath(args, b.path)

		if !argPresent {
			if headerPresent {
				return headerMismatchErr("header %s is present but argument %s is absent", b.header, strings.Join(b.path, "."))
			}
			continue
		}
		if !headerPresent {
			return headerMismatchErr("missing required header %s for argument %s", b.header, strings.Join(b.path, "."))
		}
		decoded, decodeErr := transport.DecodeHeaderValue(headerVal)
		if decodeErr != nil {
			return headerMismatchErr("invalid encoding for header %s", b.header)
		}
		if !argValueMatchesHeader(argVal, decoded) {
			return headerMismatchErr("header %s does not match argument %s", b.header, strings.Join(b.path, "."))
		}
	}
	return nil
}
