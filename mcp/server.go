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

	// AdvertisedVersions is the protocol version list reported in three places that must
	// never disagree: server/discover's supportedVersions, the legacy "initialize"
	// diagnostic's data.supported, and -32022 (UnsupportedProtocolVersion)'s
	// data.supported. Defaults to SupportedVersions (just "2026-07-28") if unset.
	//
	// This is purely advisory — what a request declaring a version this server doesn't
	// actually run gets told is available as an alternative. It does NOT widen what
	// ParseRequestMeta accepts: a request's params._meta protocol version is still
	// validated against the modern-only SupportedVersions regardless of this field,
	// because a request naming e.g. "2025-11-25" inside the modern _meta envelope is
	// nonsense and must not be routed through the modern dispatch.
	//
	// A legacy-compatibility layer sets this to SupportedVersions plus every legacy
	// revision it serves, so a client told "unsupported" always sees one consistent,
	// truthful story about what the server (overlay included) actually speaks.
	AdvertisedVersions []string

	// Skills enables the io.modelcontextprotocol/skills extension when non-nil: the
	// capability is declared in server/discover and skills/list and skills/get start
	// answering. Leaving it nil is not a degraded mode — those methods genuinely do not
	// exist, no capability key is emitted, and nothing else changes.
	//
	// EXPERIMENTAL: tracks SEP-2640, an Extensions Track proposal that is open and not
	// merged. Wire shapes may change. See mcp/skills.go.
	Skills *SkillRegistry

	// SkillsDirectoryRead opts into resources/directory/read and is reported as the skills
	// extension's directoryRead setting. Ignored when Skills is nil.
	//
	// EXPERIMENTAL: tracks SEP-2640, which is not merged.
	SkillsDirectoryRead bool
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
		cfg.AdvertisedVersions = config.AdvertisedVersions
		cfg.Skills = config.Skills
		cfg.SkillsDirectoryRead = config.SkillsDirectoryRead
	}
	if len(cfg.AdvertisedVersions) == 0 {
		cfg.AdvertisedVersions = SupportedVersions
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

// advertisedVersions is the version list this server reports as available whenever it
// tells a client "here's what I speak" — server/discover, the legacy initialize
// diagnostic, and -32022's alternatives. See ServerConfig.AdvertisedVersions.
func (s *Server) advertisedVersions() []string {
	return s.config.AdvertisedVersions
}

// capabilities reports which of tools/resources/prompts this server actually serves: a
// registry with no entries declares no capability for it, rather than an always-present
// empty object.
func (s *Server) capabilities() ServerCapabilities {
	var caps ServerCapabilities
	if s.registry.HasTools() {
		caps.Tools = &ToolsCapability{ListChanged: true}
	}
	// Templates count: there is no separate "templates supported" capability in this
	// revision, so a registry holding only templates and no concrete resources must still
	// declare resources — otherwise a client (or the legacy compat overlay's handshake,
	// which derives its capabilities from server/discover) would conclude this server
	// serves no resources at all and never probe resources/templates/list.
	if s.resourceRegistry.HasResources() || s.resourceRegistry.HasTemplates() {
		caps.Resources = &ResourcesCapability{ListChanged: true}
	}
	if s.resourceRegistry.hasTemplateCompleters() {
		caps.Completions = &CompletionsCapability{}
	}
	if s.skillsEnabled() {
		// Declared whenever a SkillRegistry is configured, even with nothing registered:
		// unlike tools and resources, an empty skills/list is legitimate (a
		// resolver-backed catalog is unenumerable by design) and the SEP says hosts MUST
		// NOT read one as proof that a server has no skills.
		//
		// SEP-2640's own text says this goes in the "initialize response" because it was
		// drafted before 2026-07-28 removed the handshake. server/discover is where
		// capabilities live in this revision — do not "fix" this back.
		settings, _ := json.Marshal(skillsExtensionSettings{DirectoryRead: s.config.SkillsDirectoryRead})
		caps.Extensions = map[string]json.RawMessage{SkillsExtensionID: settings}
	}
	return caps
}

// skillsEnabled reports whether this server serves the io.modelcontextprotocol/skills
// extension. It gates both the capability and the router, the same way
// hasTemplateCompleters gates completions.
func (s *Server) skillsEnabled() bool {
	return s.config.Skills != nil
}

// directoryReadEnabled reports whether resources/directory/read is served. It is a setting
// of the skills extension, so it means nothing without the extension itself.
func (s *Server) directoryReadEnabled() bool {
	return s.skillsEnabled() && s.config.SkillsDirectoryRead
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
		w.WriteMessage(transport.NewErrorResponse(req.ID, s.legacyInitializeError()))
		return
	}

	meta, rerr := ParseRequestMeta(req.Params)
	if rerr != nil {
		if rerr.Code == transport.UnsupportedProtocolVersion && meta != nil {
			// ParseRequestMeta validates strictly against the modern-only
			// SupportedVersions; widen the alternatives offered here to whatever this
			// server actually advertises (SupportedVersions, plus any legacy revisions a
			// compatibility layer serves), so this error tells the same story as
			// server/discover and the initialize diagnostic.
			rerr = s.unsupportedProtocolVersionErrFor(meta.ProtocolVersion)
		}
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
	case "resources/templates/list":
		result, rerr = s.handleResourcesTemplatesList(ctx, req.Params)
	case "resources/read":
		result, rerr = s.handleResourcesRead(ctx, req.Params)
	case "completion/complete":
		// Optional and opt-in: with no CompletionProvider attached to any template, the
		// method genuinely does not exist here, and -32601 is the answer clients probe for.
		if !s.resourceRegistry.hasTemplateCompleters() {
			writeMethodNotFound(w, req.ID, req.Method, "no completion provider registered")
			return
		}
		result, rerr = s.handleCompletionComplete(ctx, req.Params)
	case "skills/list":
		// Same opt-in shape as completion/complete: without a SkillRegistry this server
		// does not serve the skills extension, and -32601 is the honest answer.
		if !s.skillsEnabled() {
			writeMethodNotFound(w, req.ID, req.Method, "no skill registry configured")
			return
		}
		result, rerr = s.handleSkillsList(ctx, req.Params)
	case "skills/get":
		if !s.skillsEnabled() {
			writeMethodNotFound(w, req.ID, req.Method, "no skill registry configured")
			return
		}
		result, rerr = s.handleSkillsGet(ctx, req.Params)
	case "resources/directory/read":
		// Gated a second time by the extension's directoryRead setting: a server that
		// declares directoryRead:false must not answer this method.
		if !s.directoryReadEnabled() {
			writeMethodNotFound(w, req.ID, req.Method, "directory read not enabled")
			return
		}
		result, rerr = s.handleResourcesDirectoryRead(ctx, req.Params)
	default:
		writeMethodNotFound(w, req.ID, req.Method, "")
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

// writeMethodNotFound answers -32601. reason, when non-empty, is logged but never sent: an
// opt-in method that is switched off must be indistinguishable from one this server never
// implemented, which is exactly what clients probe for.
func writeMethodNotFound(w transport.ResponseWriter, id json.RawMessage, method, reason string) {
	if reason != "" {
		logging.Debug("JSON-RPC method not found", "method", method, "reason", reason)
	} else {
		logging.Debug("JSON-RPC method not found", "method", method)
	}
	w.WriteMessage(transport.NewErrorResponse(id, &transport.RPCError{Code: transport.MethodNotFound, Message: "Method not found"}))
}
