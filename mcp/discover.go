package mcp

import (
	"context"

	"github.com/spirilis/generic-go-mcp/transport"
)

// DiscoverResult is the response to server/discover: the versions this server speaks,
// its capabilities, and its identity — everything a client used to learn from
// initialize, available without any handshake.
type DiscoverResult struct {
	CacheableResult
	SupportedVersions []string           `json:"supportedVersions"`
	Capabilities      ServerCapabilities `json:"capabilities"`
	Instructions      string             `json:"instructions,omitempty"`
}

// handleDiscover implements server/discover, which every server MUST support so a client
// can learn supported versions/capabilities/identity without relying on a handshake.
func (s *Server) handleDiscover(ctx context.Context, meta *RequestMeta) (Result, *transport.RPCError) {
	return &DiscoverResult{
		CacheableResult:   NewCacheableResult(s.listTTLMs, s.cacheScope()),
		SupportedVersions: SupportedVersions,
		Capabilities:      s.capabilities(),
		Instructions:      s.config.Instructions,
	}, nil
}
