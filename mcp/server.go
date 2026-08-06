package mcp

import (
	"context"
	"crypto/rand"
	"encoding/json"

	"github.com/spirilis/generic-go-mcp/logging"
	"github.com/spirilis/generic-go-mcp/transport"
)

// Default caching hints. See ServerConfig.ListTTLMs / ReadTTLMs to override.
const (
	defaultListTTLMs = int64(300_000) // 5 minutes: tool/resource catalogs change rarely
	defaultReadTTLMs = int64(0)       // resource content is often dynamic; refetch by default
)

// ServerConfig configures a Server.
type ServerConfig struct {
	Name         string // Server name (default: "generic-go-mcp")
	Version      string // Server version (default: "0.1.0")
	Instructions string // Optional natural-language guidance for LLMs, returned from server/discover

	// RequestStateKey signs the opaque requestState blob used by Multi Round-Trip
	// Requests (MRTR). If empty, a random key is generated at startup — fine for a
	// single server process, but a requestState signed before a restart (or by a
	// different process behind a load balancer using a different key) will fail
	// verification on retry.
	RequestStateKey []byte

	// PrincipalFromContext extracts an opaque identifier for the caller (e.g. an
	// authenticated user ID) from a request's context, bound into signed requestState so
	// a retry cannot be replayed by a different principal. Defaults to a constant empty
	// principal when unset (fine for unauthenticated servers).
	PrincipalFromContext func(ctx context.Context) string

	// DefaultCacheScope is used for every CacheableResult this server produces. Defaults
	// to "public"; set to "private" if served tools/resources vary by caller (e.g. an
	// authenticated, per-user catalog).
	DefaultCacheScope string

	// ListTTLMs is the ttlMs hint on server/discover, tools/list, and resources/list
	// results. Defaults to 300000 (5 minutes) if nil.
	ListTTLMs *int64

	// ReadTTLMs is the ttlMs hint on resources/read results. Defaults to 0 (always
	// stale) if nil.
	ReadTTLMs *int64
}

// Server implements the MCP protocol (2026-07-28): a stateless request router over a
// ToolRegistry and a ResourceRegistry.
type Server struct {
	registry         *ToolRegistry
	resourceRegistry *ResourceRegistry
	config           ServerConfig
	broker           *Broker
	listTTLMs        int64
	readTTLMs        int64
}

// NewServer creates a new MCP server with the given registry, resource registry, and
// configuration. If config is nil, default values are used.
func NewServer(registry *ToolRegistry, resourceRegistry *ResourceRegistry, config *ServerConfig) *Server {
	cfg := ServerConfig{
		Name:    "generic-go-mcp",
		Version: "0.1.0",
	}
	if config != nil {
		if config.Name != "" {
			cfg.Name = config.Name
		}
		if config.Version != "" {
			cfg.Version = config.Version
		}
		if config.Instructions != "" {
			cfg.Instructions = config.Instructions
		}
		cfg.RequestStateKey = config.RequestStateKey
		cfg.PrincipalFromContext = config.PrincipalFromContext
		cfg.DefaultCacheScope = config.DefaultCacheScope
		cfg.ListTTLMs = config.ListTTLMs
		cfg.ReadTTLMs = config.ReadTTLMs
	}
	if len(cfg.RequestStateKey) == 0 {
		key := make([]byte, 32)
		_, _ = rand.Read(key)
		cfg.RequestStateKey = key
	}
	if cfg.PrincipalFromContext == nil {
		cfg.PrincipalFromContext = func(context.Context) string { return "" }
	}
	if cfg.DefaultCacheScope == "" {
		cfg.DefaultCacheScope = CacheScopePublic
	}

	listTTL := defaultListTTLMs
	if cfg.ListTTLMs != nil {
		listTTL = *cfg.ListTTLMs
	}
	readTTL := defaultReadTTLMs
	if cfg.ReadTTLMs != nil {
		readTTL = *cfg.ReadTTLMs
	}

	broker := newBroker()
	registry.onChange = broker.notifyToolsListChanged
	resourceRegistry.onChange = broker.notifyResourcesListChanged
	resourceRegistry.onUpdate = broker.notifyResourceUpdated

	return &Server{
		registry:         registry,
		resourceRegistry: resourceRegistry,
		config:           cfg,
		broker:           broker,
		listTTLMs:        listTTL,
		readTTLMs:        readTTL,
	}
}

func (s *Server) serverInfo() *Implementation {
	return &Implementation{Name: s.config.Name, Version: s.config.Version}
}

func (s *Server) cacheScope() string {
	return s.config.DefaultCacheScope
}

// capabilities reports which of tools/resources/prompts this server actually serves: a
// registry with no entries declares no capability for it, rather than an always-present
// empty object.
func (s *Server) capabilities() ServerCapabilities {
	var caps ServerCapabilities
	if s.registry.HasTools() {
		caps.Tools = &ToolsCapability{ListChanged: true}
	}
	if s.resourceRegistry.HasResources() {
		caps.Resources = &ResourcesCapability{ListChanged: true}
	}
	return caps
}

// HandleMessage implements transport.MessageHandler: it parses one JSON-RPC request or
// notification, validates its per-request metadata, routes it, and writes the result (or
// error) through w.
func (s *Server) HandleMessage(ctx context.Context, data []byte, w transport.ResponseWriter) {
	var req transport.JSONRPCRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logging.Debug("JSON-RPC parse error", "error", err)
		w.WriteMessage(transport.NewErrorResponse(nil, &transport.RPCError{Code: transport.ParseError, Message: "Parse error"}))
		return
	}

	logging.Debug("JSON-RPC request", "method", req.Method, "id", string(req.ID))
	if logging.IsTraceEnabled() && req.Params != nil {
		logging.Trace("JSON-RPC params", "method", req.Method, "params", string(req.Params))
	}

	if req.IsNotification() {
		// notifications/cancelled is acted on by the transport binding before this is
		// ever reached (stdio/UNIX cancel the matching in-flight request context; on
		// Streamable HTTP, closing the response stream is itself the cancellation
		// signal and the notification is not defined at all). No other client-to-server
		// notification exists in this protocol revision, so there is nothing to do here.
		return
	}

	// "initialize" predates per-request _meta entirely (2025-11-25 and earlier); a
	// legacy client sending it has no idea _meta exists, so this must be checked before
	// attempting to parse it, and answered with a diagnostic naming the versions this
	// server does support instead.
	if req.Method == "initialize" {
		w.WriteMessage(transport.NewErrorResponse(req.ID, legacyInitializeError()))
		return
	}

	meta, rerr := ParseRequestMeta(req.Params)
	if rerr != nil {
		w.WriteMessage(transport.NewErrorResponse(req.ID, rerr))
		return
	}

	// subscriptions/listen is long-lived and streams notifications directly through w;
	// it doesn't fit the uniform single-Result dispatch below.
	if req.Method == "subscriptions/listen" {
		s.handleSubscriptionsListen(ctx, req.ID, req.Params, w)
		return
	}

	var result Result
	switch req.Method {
	case "server/discover":
		result, rerr = s.handleDiscover(ctx, meta)
	case "tools/list":
		result, rerr = s.handleToolsList(ctx, req.Params)
	case "tools/call":
		result, rerr = s.handleToolsCall(ctx, meta, req.Params)
	case "resources/list":
		result, rerr = s.handleResourcesList(ctx, req.Params)
	case "resources/read":
		result, rerr = s.handleResourcesRead(ctx, req.Params)
	default:
		logging.Debug("JSON-RPC method not found", "method", req.Method)
		w.WriteMessage(transport.NewErrorResponse(req.ID, &transport.RPCError{Code: transport.MethodNotFound, Message: "Method not found"}))
		return
	}

	if rerr != nil {
		logging.Debug("JSON-RPC error", "method", req.Method, "error", rerr.Message)
		w.WriteMessage(transport.NewErrorResponse(req.ID, rerr))
		return
	}

	result.setResultType(ResultTypeComplete)
	result.setServerInfo(s.serverInfo())

	if logging.IsTraceEnabled() {
		resultJSON, _ := json.Marshal(result)
		logging.Trace("JSON-RPC response", "method", req.Method, "result", string(resultJSON))
	}

	w.WriteMessage(transport.NewSuccessResponse(req.ID, result))
}
