package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/spirilis/generic-go-mcp/transport"
)

// ToolAnnotations describes tool behavior hints. These are untrusted unless the tool
// definition comes from a trusted server — a client MUST NOT rely on them for security
// decisions.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  bool   `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// Icon is a visual identifier attachable to a Tool, Resource, Prompt, or Implementation.
type Icon struct {
	Src      string   `json:"src"`
	MimeType string   `json:"mimeType,omitempty"`
	Sizes    []string `json:"sizes,omitempty"`
	Theme    string   `json:"theme,omitempty"`
}

// Tool describes one callable tool: its name, human-facing metadata, and the JSON
// Schema (2020-12 by default) its arguments and, optionally, its structured output must
// conform to.
type Tool struct {
	Name         string                 `json:"name"`
	Title        string                 `json:"title,omitempty"`
	Description  string                 `json:"description,omitempty"`
	InputSchema  json.RawMessage        `json:"inputSchema"`
	OutputSchema json.RawMessage        `json:"outputSchema,omitempty"`
	Annotations  *ToolAnnotations       `json:"annotations,omitempty"`
	Icons        []Icon                 `json:"icons,omitempty"`
	Meta         map[string]interface{} `json:"_meta,omitempty"`
}

// ToolRequest carries everything a ToolFunction needs about the call in progress.
type ToolRequest struct {
	Name               string
	Arguments          json.RawMessage
	Meta               *RequestMeta
	ClientCapabilities *ClientCapabilities
	InputResponses     InputResponses
	RequestState       string

	ctx    context.Context
	server *Server
}

// BindArguments unmarshals the call's raw arguments into v.
func (r *ToolRequest) BindArguments(v interface{}) error {
	if len(r.Arguments) == 0 {
		return nil
	}
	return json.Unmarshal(r.Arguments, v)
}

// ElicitResponse returns the client's answer to a previously requested elicitation named
// key (as set via NeedInput on an earlier attempt at this same call), if present.
func (r *ToolRequest) ElicitResponse(key string) (*ElicitResult, bool) {
	if r.InputResponses == nil {
		return nil, false
	}
	raw, ok := r.InputResponses[key]
	if !ok {
		return nil, false
	}
	var res ElicitResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, false
	}
	return &res, true
}

// NeedInput returns an InputRequiredResult (Multi Round-Trip Requests) asking the client
// to fulfill reqs. The framework signs a requestState the client must echo back
// unmodified on its retry of this same call; ToolRequest.RequestState/ElicitResponse read
// it back on that retry. Returns a *MissingCapabilityError if reqs includes a method the
// caller's declared ClientCapabilities does not cover — servers MUST NOT send an
// inputRequests entry the client hasn't declared support for.
func (r *ToolRequest) NeedInput(reqs InputRequests) (Result, error) {
	if missing := missingCapabilitiesFor(reqs, r.ClientCapabilities); len(missing) > 0 {
		return nil, &MissingCapabilityError{Capabilities: missing}
	}
	principal := r.server.config.PrincipalFromContext(r.ctx)
	state := r.server.signRequestState(principal, r.Name, r.Arguments)
	return newInputRequiredResult(reqs, state), nil
}

// ToolFunction is the implementation of a tool. A non-nil error is a protocol-level
// failure (mapped to JSON-RPC -32603): tool execution errors that the model should see
// and can potentially recover from belong in a returned ToolCallResult with IsError set
// (see ErrorResultf), not in the error return.
type ToolFunction func(ctx context.Context, req *ToolRequest) (Result, error)

// ToolCallResult is the result of a completed tools/call.
type ToolCallResult struct {
	BaseResult
	Content           []Content   `json:"content,omitempty"`
	StructuredContent interface{} `json:"structuredContent,omitempty"`
	IsError           bool        `json:"isError,omitempty"`
}

// ErrorResultf builds a tool execution error result: reported to the model as
// isError:true content, per the spec's distinction between protocol errors (JSON-RPC
// errors) and tool execution errors (actionable feedback the model can act on).
func ErrorResultf(format string, args ...interface{}) *ToolCallResult {
	return &ToolCallResult{Content: []Content{Text(fmt.Sprintf(format, args...))}, IsError: true}
}

// ToolRegistry manages available tools, safe for concurrent registration and lookup —
// notably including registration happening at runtime after the server has started,
// which is now an observable event via subscriptions/listen.
type ToolRegistry struct {
	mu        sync.RWMutex
	tools     []Tool
	functions map[string]ToolFunction
	gates     map[string][]string // tool name -> roles of which the caller needs one; absent = ungated
	onChange  func()
}

// ToolOption configures one tool at registration (RegisterWithOptions). See RequireRoles.
type ToolOption func(*toolOptions)

type toolOptions struct {
	gated bool
	roles []string
}

// RequireRoles restricts a tool to callers holding at least one of roles, as reported by
// ServerConfig.RolesFromContext. To any other caller the tool does not exist: tools/list leaves
// it out and tools/call answers exactly as for an unknown name, so a probe learns nothing about
// what is registered. With no roles the tool is hidden from everyone (any-of over nothing),
// which fails closed rather than silently ungating a tool whose role list came out empty.
//
// The gate only applies when ServerConfig.RolesFromContext is set. A server without it (stdio,
// or HTTP without auth) has no notion of who is calling, and serves every tool.
func RequireRoles(roles ...string) ToolOption {
	return func(o *toolOptions) {
		o.gated = true
		o.roles = append(o.roles, roles...)
	}
}

// NewToolRegistry creates a new tool registry
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools:     make([]Tool, 0),
		functions: make(map[string]ToolFunction),
		gates:     make(map[string][]string),
	}
}

// Register adds a tool to the registry. Registering a name that is already present
// replaces it and moves it to the end of the list, rather than adding a second entry — the
// list and the name-keyed function map must not be allowed to disagree. If the registry is
// already attached to a running Server, this fires a notifications/tools/list_changed to
// any subscribed clients.
//
// Register is RegisterWithOptions with no options: it registers an ungated tool.
func (r *ToolRegistry) Register(tool Tool, fn ToolFunction) {
	r.RegisterWithOptions(tool, fn)
}

// RegisterWithOptions is Register with options, such as RequireRoles. It is a separate method
// rather than a variadic parameter on Register because embedders declare interfaces with
// Register's exact two-argument signature, which *ToolRegistry must keep satisfying.
// Re-registering a name replaces its options too, so registering it again without RequireRoles
// ungates it.
func (r *ToolRegistry) RegisterWithOptions(tool Tool, fn ToolFunction, opts ...ToolOption) {
	var o toolOptions
	for _, opt := range opts {
		opt(&o)
	}
	r.mu.Lock()
	r.removeLocked(tool.Name)
	r.tools = append(r.tools, tool)
	r.functions[tool.Name] = fn
	if o.gated {
		// A non-nil slice even for RequireRoles(): absence from the map is what means ungated.
		r.gates[tool.Name] = append([]string{}, o.roles...)
	}
	notify := r.onChange
	r.mu.Unlock()
	if notify != nil {
		notify()
	}
}

// Unregister removes the tool registered under name, reporting whether one was found. If
// the registry is attached to a running Server and something was actually removed, this
// fires a notifications/tools/list_changed to any subscribed clients; unregistering an
// absent name is a no-op and notifies nobody.
//
// Note for paginating clients: tools/list cursors are opaque offsets, so a removal between
// two page fetches shifts later entries and can cause one to be skipped. That is what
// list_changed is for — a client that sees it should restart pagination.
func (r *ToolRegistry) Unregister(name string) bool {
	r.mu.Lock()
	removed := r.removeLocked(name)
	notify := r.onChange
	r.mu.Unlock()
	if removed && notify != nil {
		notify()
	}
	return removed
}

// removeLocked drops any entry for name, reporting whether one existed. Callers must hold
// r.mu. It deliberately does not fire onChange: notifications are sent by the exported
// caller after releasing the lock, since Broker.broadcast takes a lock of its own.
func (r *ToolRegistry) removeLocked(name string) bool {
	if _, ok := r.functions[name]; !ok {
		return false
	}
	delete(r.functions, name)
	delete(r.gates, name)
	for i, t := range r.tools {
		if t.Name == name {
			r.tools = append(r.tools[:i], r.tools[i+1:]...)
			break
		}
	}
	return true
}

// List returns all registered tools, in registration order (a stable, deterministic
// order across calls when the underlying set hasn't changed, as recommended for
// client-side caching and LLM prompt cache hit rates).
func (r *ToolRegistry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, len(r.tools))
	copy(out, r.tools)
	return out
}

// Get returns the Tool definition for name.
func (r *ToolRegistry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// HasTools reports whether any tool is registered.
func (r *ToolRegistry) HasTools() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools) > 0
}

// HasGatedTools reports whether any registered tool was registered with RequireRoles.
func (r *ToolRegistry) HasGatedTools() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.gates) > 0
}

// listVisible returns the tools allowed admits, in registration order. allowed sees each tool's
// required roles (nil for an ungated tool) and is called under the read lock, so it must not
// touch the registry.
func (r *ToolRegistry) listVisible(allowed func(required []string, gated bool) bool) []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		required, gated := r.gates[t.Name]
		if allowed(required, gated) {
			out = append(out, t)
		}
	}
	return out
}

// getVisible is Get for one caller: a tool allowed does not admit is reported as absent.
func (r *ToolRegistry) getVisible(name string, allowed func(required []string, gated bool) bool) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.functions[name]; !ok {
		return Tool{}, false
	}
	required, gated := r.gates[name]
	if !allowed(required, gated) {
		return Tool{}, false
	}
	for _, t := range r.tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// errToolNotFound is returned by call when the tool disappeared between the handler's
// Get and the call itself — now possible, since Unregister can land in that window.
var errToolNotFound = errors.New("tool not found")

// call runs the tool named by req. The gate is checked again here, under the same lock that
// fetches the function: a concurrent Register can replace an ungated tool with a gated one
// between the handler's lookup and this call, and the function that runs must be one the
// caller is allowed. A refusal is errToolNotFound, so it reads as an unknown tool.
func (r *ToolRegistry) call(ctx context.Context, req *ToolRequest, allowed func(required []string, gated bool) bool) (Result, error) {
	r.mu.RLock()
	fn, ok := r.functions[req.Name]
	if ok {
		required, gated := r.gates[req.Name]
		ok = allowed(required, gated)
	}
	r.mu.RUnlock()
	if !ok {
		return nil, errToolNotFound
	}
	return fn(ctx, req)
}

// toolGate returns the visibility test for the caller of ctx. Without RolesFromContext every
// tool is visible. Otherwise the caller's roles are read once, and a gated tool is visible
// only when they include one of its required roles; a caller with no roles sees no gated tool.
func (s *Server) toolGate(ctx context.Context) func(required []string, gated bool) bool {
	if s.config.RolesFromContext == nil {
		return func([]string, bool) bool { return true }
	}
	held := s.config.RolesFromContext(ctx)
	return func(required []string, gated bool) bool {
		if !gated {
			return true
		}
		for _, role := range required {
			if slices.Contains(held, role) {
				return true
			}
		}
		return false
	}
}

// toolsListCacheScope is "private" whenever tools/list can differ between callers, so a shared
// cache never hands one caller's catalog to another. That is the case only when gating is on
// and some tool is gated; otherwise the server's default applies.
func (s *Server) toolsListCacheScope() string {
	if s.config.RolesFromContext != nil && s.registry.HasGatedTools() {
		return CacheScopePrivate
	}
	return s.cacheScope()
}

// ToolsListResult is the result of tools/list.
type ToolsListResult struct {
	CacheableResult
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

func (s *Server) handleToolsList(ctx context.Context, params json.RawMessage) (Result, *transport.RPCError) {
	var p struct {
		Cursor string `json:"cursor"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}

	// Filter before paginating: cursors are offsets, and offsets into the unfiltered list would
	// let short pages reveal how many hidden tools sit between them.
	all := s.registry.listVisible(s.toolGate(ctx))
	page, next, err := paginate(all, p.Cursor, defaultPageSize)
	if err != nil {
		return nil, invalidParamsErr("invalid cursor")
	}

	return &ToolsListResult{
		CacheableResult: NewCacheableResult(s.listTTLMs, s.toolsListCacheScope()),
		Tools:           page,
		NextCursor:      next,
	}, nil
}

// ToolsCallParams is the params of a tools/call request, including the optional MRTR
// retry fields (inputResponses, requestState).
type ToolsCallParams struct {
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments"`
	InputResponses InputResponses  `json:"inputResponses,omitempty"`
	RequestState   string          `json:"requestState,omitempty"`
}

func (s *Server) handleToolsCall(ctx context.Context, meta *RequestMeta, params json.RawMessage) (Result, *transport.RPCError) {
	var p ToolsCallParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalidParamsErr("invalid tools/call params: %v", err)
	}

	// The gate runs before anything that depends on the tool itself (header bindings,
	// requestState): those errors differ from the unknown-tool one and would confirm that a
	// hidden tool exists.
	allowed := s.toolGate(ctx)
	tool, ok := s.registry.getVisible(p.Name, allowed)
	if !ok {
		return nil, invalidParamsErr("Unknown tool: %s", p.Name)
	}

	if rerr := validateToolHeaders(ctx, tool, p.Arguments); rerr != nil {
		return nil, rerr
	}

	// NeedInput always signs a requestState, so a genuine retry carries one. inputResponses
	// without it answer a question this server never asked: accepting them would let a client
	// pre-answer a confirm-before-act elicitation and skip the round trip entirely.
	if len(p.InputResponses) > 0 && p.RequestState == "" {
		return nil, invalidParamsErr("invalid requestState: inputResponses require the requestState of the request that asked for them")
	}
	if p.RequestState != "" {
		principal := s.config.PrincipalFromContext(ctx)
		if err := s.verifyRequestState(p.RequestState, principal, p.Name, p.Arguments); err != nil {
			return nil, invalidParamsErr("invalid requestState: %v", err)
		}
	}

	req := &ToolRequest{
		Name:               p.Name,
		Arguments:          p.Arguments,
		Meta:               meta,
		ClientCapabilities: meta.ClientCapabilities,
		InputResponses:     p.InputResponses,
		RequestState:       p.RequestState,
		ctx:                ctx,
		server:             s,
	}

	result, err := s.registry.call(ctx, req, allowed)
	if err != nil {
		if mc, ok := err.(*MissingCapabilityError); ok {
			return nil, missingClientCapabilityErr(mc.Capabilities...)
		}
		if errors.Is(err, errToolNotFound) {
			// Unregistered (or re-registered behind a gate) concurrently, after the lookup
			// above succeeded. Report it the same way as a name that was never registered.
			return nil, invalidParamsErr("Unknown tool: %s", p.Name)
		}
		return nil, internalErr(err)
	}
	return result, nil
}
