// Package compat is an optional legacy-compatibility overlay: it lets a generic-go-mcp
// server built for MCP 2026-07-28 also serve clients speaking a connection-scoped
// revision (2025-11-25 and earlier), behind a switch an embedder turns on explicitly.
//
// The overlay is additive. With it absent (or simply not wired into a transport), the
// server underneath is byte-for-byte the modern-only server this library implements
// everywhere else — see mcp.Server and GOLANG-MCP-CONVERT-TO-2026-07-28.md. Overlay wraps
// a transport.MessageHandler (typically an *mcp.Server) and implements
// transport.MessageHandler itself, so it drops into exactly the seam a Transport already
// expects:
//
//	inner := mcp.NewServer(registry, resourceRegistry, cfg)
//	overlay := compat.New(inner, compat.Config{})
//	trans.Start(overlay)
//
// One tool registry, one set of handlers, one auth path — two protocol envelopes. Nothing
// below the overlay is aware there are two eras: a legacy request is translated into the
// modern wire shape, forwarded to inner, and its response is stripped back down to the
// legacy shape on the way out.
package compat

import (
	"encoding/json"

	"github.com/spirilis/generic-go-mcp/transport"
)

// LegacyVersions lists the connection-scoped MCP protocol revisions this overlay can
// negotiate at a legacy "initialize" handshake, newest first. These revisions differ from
// one another in ways most servers never exercise (resources, prompts, audio content, tool
// output schemas); what varies between them here is only the version string echoed back.
var LegacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// NegotiateVersion echoes requested if this overlay recognizes it, otherwise falls back to
// the newest revision it speaks — the same "answer with the newest you support and let the
// client decide whether it can proceed" rule a real negotiation would apply to an unknown
// version.
func NegotiateVersion(requested string) string {
	for _, v := range LegacyVersions {
		if v == requested {
			return v
		}
	}
	return LegacyVersions[0]
}

// DeclaresModernProtocol reports whether params carries the 2026-07-28 protocol-version
// _meta key — the only unambiguous marker of the modern wire format. This re-exports
// transport.DeclaresModernProtocol rather than reimplementing it: the Streamable HTTP
// binding needs the exact same verdict before the protocol layer is even entered (to
// decide whether to enforce modern headers and validate a legacy session id), and two
// independent implementations of "is this legacy?" that can disagree is a bug factory.
func DeclaresModernProtocol(params json.RawMessage) bool {
	return transport.DeclaresModernProtocol(params)
}

// Claims reports whether the overlay should handle a request itself (translating it into
// the modern shape and forwarding) rather than passing it straight through to the inner
// modern handler unchanged. A request claims legacy handling if it is the legacy handshake
// method itself, or if it lacks the modern protocol-version _meta key that every genuine
// 2026-07-28 request carries. Do not try to infer the era from the method name (beyond
// "initialize"), the params shape, or HTTP headers instead of this.
func Claims(method string, params json.RawMessage) bool {
	if method == "initialize" {
		return true // only a connection-scoped client sends it
	}
	return !DeclaresModernProtocol(params)
}
