package transport

import (
	"context"
	"encoding/base64"
	"strings"
)

// Standard Streamable HTTP request header names (2026-07-28).
const (
	ProtocolVersionHeader = "MCP-Protocol-Version"
	MethodHeader          = "Mcp-Method"
	NameHeader            = "Mcp-Name"
	// ParamHeaderPrefix is prepended to a tool's x-mcp-header name to form the header that
	// carries that parameter's value, e.g. x-mcp-header "Region" -> "Mcp-Param-Region".
	ParamHeaderPrefix = "Mcp-Param-"
)

const (
	base64SentinelPrefix = "=?base64?"
	base64SentinelSuffix = "?="
)

// EncodeHeaderValue applies the base64 sentinel encoding
// (streamable-http#value-encoding) to s if it cannot be safely represented as a plain
// ASCII header value, or if it happens to already match the sentinel pattern (to avoid
// ambiguity). Otherwise it returns s unchanged.
func EncodeHeaderValue(s string) string {
	if needsBase64Encoding(s) {
		return base64SentinelPrefix + base64.StdEncoding.EncodeToString([]byte(s)) + base64SentinelSuffix
	}
	return s
}

// DecodeHeaderValue reverses EncodeHeaderValue. If s does not carry the sentinel prefix
// and suffix, it is returned unchanged (it was never encoded).
func DecodeHeaderValue(s string) (string, error) {
	if strings.HasPrefix(s, base64SentinelPrefix) && strings.HasSuffix(s, base64SentinelSuffix) {
		inner := strings.TrimSuffix(strings.TrimPrefix(s, base64SentinelPrefix), base64SentinelSuffix)
		b, err := base64.StdEncoding.DecodeString(inner)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return s, nil
}

func needsBase64Encoding(s string) bool {
	// A plain value that happens to match the sentinel pattern must still be encoded, to
	// avoid being misread as an encoded value on the receiving end.
	if strings.HasPrefix(s, base64SentinelPrefix) && strings.HasSuffix(s, base64SentinelSuffix) {
		return true
	}
	if s == "" {
		return false
	}
	// Leading/trailing space or tab can't round-trip through a trimmed header value.
	if s[0] == ' ' || s[0] == '\t' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t' {
		return true
	}
	for _, r := range s {
		// RFC 9110 field-value: visible ASCII (0x21-0x7E), space (0x20), horizontal tab (0x09).
		if r == '\t' || r == ' ' {
			continue
		}
		if r < 0x21 || r > 0x7E {
			return true
		}
	}
	return false
}

// requestHeadersKey is the context key under which RequestHeaders is stored.
type requestHeadersKey struct{}

// RequestHeaders carries the subset of a Streamable HTTP request's headers relevant to
// MCP-level validation (e.g. x-mcp-header parameter mirroring), so that the mcp package
// can validate Mcp-Param-* headers against a tool's inputSchema without the transport
// layer needing to know anything about tool schemas. Header names are stored
// case-normalized (canonical MIME header form); values are the raw (possibly
// base64-sentinel-encoded) header value.
type RequestHeaders struct {
	Values map[string]string
}

// Get returns the raw (possibly encoded) value for a header name, decoding it via
// DecodeHeaderValue is the caller's responsibility.
func (h RequestHeaders) Get(name string) (string, bool) {
	if h.Values == nil {
		return "", false
	}
	v, ok := h.Values[name]
	return v, ok
}

// WithRequestHeaders attaches h to ctx.
func WithRequestHeaders(ctx context.Context, h RequestHeaders) context.Context {
	return context.WithValue(ctx, requestHeadersKey{}, h)
}

// HeadersFromContext retrieves RequestHeaders previously attached with
// WithRequestHeaders. ok is false when no headers were attached (e.g. on stdio, which has
// no header layer at all).
func HeadersFromContext(ctx context.Context) (h RequestHeaders, ok bool) {
	h, ok = ctx.Value(requestHeadersKey{}).(RequestHeaders)
	return h, ok
}
