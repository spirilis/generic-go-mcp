package mcp

// ProtocolVersion is the MCP protocol revision this server implements.
const ProtocolVersion = "2026-07-28"

// SupportedVersions lists every protocol version this server accepts in a request's
// io.modelcontextprotocol/protocolVersion field. Per the hard-cutover decision recorded
// in GOLANG-MCP-CONVERT-TO-2026-07-28.md, this is a single-element list: there is no
// dual-era support, and no fallback negotiation to an earlier revision.
var SupportedVersions = []string{ProtocolVersion}

func isSupportedVersion(v string) bool {
	for _, s := range SupportedVersions {
		if s == v {
			return true
		}
	}
	return false
}
