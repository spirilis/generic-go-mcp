package mcp

// ResultType values (2026-07-28). Every result carries one; a client talking to an
// earlier-revision server that omits the field treats it as "complete".
const (
	ResultTypeComplete      = "complete"
	ResultTypeInputRequired = "input_required"
)

// CacheScope values for CacheableResult.
const (
	CacheScopePublic  = "public"
	CacheScopePrivate = "private"
)

// Result is implemented by every result type this server returns. It lets the router
// stamp resultType and _meta.serverInfo in one place (Server.HandleMessage) so no
// individual handler can forget them.
type Result interface {
	setResultType(string)
	setServerInfo(*Implementation)
}

// BaseResult is embedded by every result type. ResultType and Meta are exported so they
// serialize onto the wire, but are only ever assigned through the Result interface
// methods below (called by the router), never set directly by handler code — with one
// exception: InputRequiredResult's constructor sets ResultType directly to
// "input_required", which is why setResultType only fills an *empty* field rather than
// overwriting unconditionally.
type BaseResult struct {
	ResultType string                 `json:"resultType"`
	Meta       map[string]interface{} `json:"_meta,omitempty"`
}

func (b *BaseResult) setResultType(t string) {
	if b.ResultType == "" {
		b.ResultType = t
	}
}

func (b *BaseResult) setServerInfo(impl *Implementation) {
	if impl == nil {
		return
	}
	if b.Meta == nil {
		b.Meta = make(map[string]interface{})
	}
	b.Meta[metaKeyServerInfo] = impl
}

// CacheableResult is embedded by every result type for an operation the spec requires to
// carry caching hints: server/discover, tools/list, prompts/list, resources/list,
// resources/templates/list, and resources/read.
type CacheableResult struct {
	BaseResult
	TTLMs      int64  `json:"ttlMs"`
	CacheScope string `json:"cacheScope"`
}

// NewCacheableResult builds a CacheableResult with a valid (>= 0) ttlMs.
func NewCacheableResult(ttlMs int64, scope string) CacheableResult {
	if ttlMs < 0 {
		ttlMs = 0
	}
	return CacheableResult{TTLMs: ttlMs, CacheScope: scope}
}
