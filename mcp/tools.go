package mcp

import (
	"context"
	"encoding/json"
	"fmt"
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
	onChange  func()
}

// NewToolRegistry creates a new tool registry
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools:     make([]Tool, 0),
		functions: make(map[string]ToolFunction),
	}
}

// Register adds a tool to the registry. If the registry is already attached to a running
// Server, this fires a notifications/tools/list_changed to any subscribed clients.
func (r *ToolRegistry) Register(tool Tool, fn ToolFunction) {
	r.mu.Lock()
	r.tools = append(r.tools, tool)
	r.functions[tool.Name] = fn
	notify := r.onChange
	r.mu.Unlock()
	if notify != nil {
		notify()
	}
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

func (r *ToolRegistry) call(ctx context.Context, req *ToolRequest) (Result, error) {
	r.mu.RLock()
	fn := r.functions[req.Name]
	r.mu.RUnlock()
	return fn(ctx, req)
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

	all := s.registry.List()
	page, next, err := paginate(all, p.Cursor, defaultPageSize)
	if err != nil {
		return nil, invalidParamsErr("invalid cursor")
	}

	return &ToolsListResult{
		CacheableResult: NewCacheableResult(s.listTTLMs, s.cacheScope()),
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

	tool, ok := s.registry.Get(p.Name)
	if !ok {
		return nil, invalidParamsErr("Unknown tool: %s", p.Name)
	}

	if rerr := validateToolHeaders(ctx, tool, p.Arguments); rerr != nil {
		return nil, rerr
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

	result, err := s.registry.call(ctx, req)
	if err != nil {
		if mc, ok := err.(*MissingCapabilityError); ok {
			return nil, missingClientCapabilityErr(mc.Capabilities...)
		}
		return nil, internalErr(err)
	}
	return result, nil
}
