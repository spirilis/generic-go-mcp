package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/spirilis/generic-go-mcp/transport"
)

// Resource describes one readable resource.
type Resource struct {
	URI         string                 `json:"uri"`
	Name        string                 `json:"name"`
	Title       string                 `json:"title,omitempty"`
	Description string                 `json:"description,omitempty"`
	MimeType    string                 `json:"mimeType,omitempty"`
	Size        *int64                 `json:"size,omitempty"`
	Annotations *Annotations           `json:"annotations,omitempty"`
	Icons       []Icon                 `json:"icons,omitempty"`
	Meta        map[string]interface{} `json:"_meta,omitempty"`
}

// ResourceContentResult is what a ResourceFunction returns: exactly one of Text or Blob
// (base64-encoded binary) should be set. MimeType, if non-empty, overrides the mime type
// registered on the Resource for this particular read.
//
// Meta and Annotations are optional and reach the wire as the contents[0]._meta and
// contents[0].annotations fields of the resources/read result. They are per-read: catalog
// metadata belongs on the Resource (or ResourceTemplate) instead. Keys under a reserved
// reverse-domain prefix are the specification's business, not this library's — see
// SkillsMetaPrefix for one such use.
type ResourceContentResult struct {
	Text        string
	Blob        string
	MimeType    string
	Meta        map[string]interface{}
	Annotations *Annotations
}

// ResourceFunction produces the content of a resource when read.
type ResourceFunction func(ctx context.Context) (ResourceContentResult, error)

// ResourceRegistry manages available resources, safe for concurrent registration, lookup,
// and reads.
type ResourceRegistry struct {
	mu        sync.RWMutex
	resources []Resource
	functions map[string]ResourceFunction // keyed by URI
	templates []*compiledTemplate         // registration order; first match wins
	onChange  func()
	onUpdate  func(string)
}

// NewResourceRegistry creates a new resource registry
func NewResourceRegistry() *ResourceRegistry {
	return &ResourceRegistry{
		resources: []Resource{},
		functions: make(map[string]ResourceFunction),
	}
}

// Register adds a resource and its function to the registry. Registering a URI that is
// already present replaces it and moves it to the end of the list, rather than adding a
// second entry — the list and the URI-keyed function map must not be allowed to disagree.
// If the registry is already attached to a running Server, this fires a
// notifications/resources/list_changed to any subscribed clients.
//
// A replacement additionally fires notifications/resources/updated for that URI, while a
// first-time registration does not: replacing an entry swaps both its metadata and its
// ResourceFunction, and since Go cannot compare func values the registry has no way to tell
// a metadata edit from a content swap. Announcing the update is the safe side of that
// ambiguity. A brand-new URI is a catalog addition, which list_changed already covers.
func (r *ResourceRegistry) Register(res Resource, fn ResourceFunction) {
	r.mu.Lock()
	replaced := r.removeLocked(res.URI)
	r.resources = append(r.resources, res)
	r.functions[res.URI] = fn
	notify, update := r.onChange, r.onUpdate
	r.mu.Unlock()
	if notify != nil {
		notify()
	}
	if replaced && update != nil {
		update(res.URI)
	}
}

// Unregister removes the resource registered under uri, reporting whether one was found.
// If the registry is attached to a running Server and something was actually removed, this
// fires a notifications/resources/list_changed to any subscribed clients; unregistering an
// absent URI is a no-op and notifies nobody.
//
// Note for paginating clients: resources/list cursors are opaque offsets, so a removal
// between two page fetches shifts later entries and can cause one to be skipped. That is
// what list_changed is for — a client that sees it should restart pagination.
func (r *ResourceRegistry) Unregister(uri string) bool {
	r.mu.Lock()
	removed := r.removeLocked(uri)
	notify := r.onChange
	r.mu.Unlock()
	if removed && notify != nil {
		notify()
	}
	return removed
}

// NotifyUpdated announces that the content behind uri changed, delivering a
// notifications/resources/updated to every subscriptions/listen stream watching that URI.
// It reports whether the URI is registered; announcing an unregistered URI is a no-op that
// notifies nobody, mirroring Unregister's contract.
//
// Unlike list_changed, the library cannot detect this on its own: a ResourceFunction is
// called on demand and its output is opaque to the registry, so only the consumer knows when
// the underlying thing changed. This is that explicit signal.
//
// A URI that matches no concrete resource but does match a registered resource template
// counts as known: template members are readable, so they are announceable, and a server
// watching an upstream (pods appearing and disappearing) needs to say so for URIs it never
// registered individually. A URI matching neither is still a no-op returning false, which is
// what catches a typo.
func (r *ResourceRegistry) NotifyUpdated(uri string) bool {
	r.mu.Lock()
	_, known := r.functions[uri]
	if !known {
		matched, _ := r.matchTemplateLocked(uri)
		known = matched != nil
	}
	update := r.onUpdate
	r.mu.Unlock()
	if known && update != nil {
		update(uri)
	}
	return known
}

// removeLocked drops any entry for uri, reporting whether one existed. Callers must hold
// r.mu. It deliberately does not fire onChange: notifications are sent by the exported
// caller after releasing the lock, since Broker.broadcast takes a lock of its own.
func (r *ResourceRegistry) removeLocked(uri string) bool {
	if _, ok := r.functions[uri]; !ok {
		return false
	}
	delete(r.functions, uri)
	for i, res := range r.resources {
		if res.URI == uri {
			r.resources = append(r.resources[:i], r.resources[i+1:]...)
			break
		}
	}
	return true
}

// mutateBatch removes every URI in remove and registers every resource in add (paired
// positionally with fns) under a single lock, firing notifications/resources/list_changed
// at most once for the whole batch instead of once per entry.
//
// This exists for SkillRegistry, where one skill is many resources: a client should be told
// the catalog changed once, not six times for one registration. Replacement semantics match
// Register — a URI already present is swapped and moved to the end, and fires
// notifications/resources/updated as well.
func (r *ResourceRegistry) mutateBatch(remove []string, add []Resource, fns []ResourceFunction) bool {
	r.mu.Lock()
	changed := false
	for _, uri := range remove {
		if r.removeLocked(uri) {
			changed = true
		}
	}
	var replaced []string
	for i, res := range add {
		if r.removeLocked(res.URI) {
			replaced = append(replaced, res.URI)
		}
		r.resources = append(r.resources, res)
		r.functions[res.URI] = fns[i]
		changed = true
	}
	notify, update := r.onChange, r.onUpdate
	r.mu.Unlock()

	if changed && notify != nil {
		notify()
	}
	if update != nil {
		for _, uri := range replaced {
			update(uri)
		}
	}
	return changed
}

// List returns all registered resources
func (r *ResourceRegistry) List() []Resource {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Resource, len(r.resources))
	copy(result, r.resources)
	return result
}

// Get returns the Resource metadata for the given URI
func (r *ResourceRegistry) Get(uri string) (Resource, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, res := range r.resources {
		if res.URI == uri {
			return res, true
		}
	}
	return Resource{}, false
}

// Read executes the function for the given resource URI and returns its content.
func (r *ResourceRegistry) Read(ctx context.Context, uri string) (ResourceContentResult, error) {
	r.mu.RLock()
	fn, exists := r.functions[uri]
	r.mu.RUnlock()

	if !exists {
		return ResourceContentResult{}, fmt.Errorf("resource not found: %s", uri)
	}
	return fn(ctx)
}

// HasResources returns true if the registry has any resources
func (r *ResourceRegistry) HasResources() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.resources) > 0
}

// DirectoryChildren returns the direct children of dirURI, reporting whether dirURI names
// a directory at all. It backs resources/directory/read.
//
// The answer is derived lexically from the URIs of registered concrete resources rather
// than from any directory bookkeeping: a resource whose URI begins with dirURI+"/" is a
// descendant, its next path segment names the child, and a descendant further down
// collapses into a synthesized "inode/directory" entry for that segment. Nothing has to be
// kept in step with anything, and the method works for any scheme — the specification is
// explicit that directory reads are not skill-exclusive.
//
// File children are the registered Resource with Name replaced by the child's own segment,
// which is what a directory listing wants; resources/list keeps the fuller, disambiguating
// name. Resource template members are not consulted: a template describes an unbounded
// keyspace, so its members are not enumerable and a directory of them does not exist.
//
// One consequence of deriving rather than bookkeeping: a directory with no registered
// descendants is indistinguishable from one that was never there, and both report false —
// which resources/directory/read answers with -32602.
func (r *ResourceRegistry) DirectoryChildren(dirURI string) ([]Resource, bool) {
	dirURI = strings.TrimSuffix(dirURI, "/")
	if dirURI == "" {
		return nil, false
	}
	prefix := dirURI + "/"

	r.mu.RLock()
	defer r.mu.RUnlock()

	children := make([]Resource, 0, 8)
	seen := make(map[string]struct{})
	for _, res := range r.resources {
		if !strings.HasPrefix(res.URI, prefix) {
			continue
		}
		rest := res.URI[len(prefix):]
		if rest == "" {
			continue
		}
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			seg := rest[:i]
			if seg == "" {
				// A leading "/" in the remainder (e.g. a degenerate prefix like "scheme:/"
				// against "scheme://host/…") would otherwise synthesize a nameless child.
				continue
			}
			uri := prefix + seg
			if _, dup := seen[uri]; dup {
				continue
			}
			seen[uri] = struct{}{}
			children = append(children, Resource{URI: uri, Name: displaySegment(seg), MimeType: mimeTypeDirectory})
			continue
		}
		if _, dup := seen[res.URI]; dup {
			continue
		}
		seen[res.URI] = struct{}{}
		child := res
		child.Name = displaySegment(rest)
		children = append(children, child)
	}
	if len(children) == 0 {
		return nil, false
	}
	return children, true
}

// RegisterTemplate adds a resource template and the function that reads one concrete member
// of it. Unlike Register, this can fail: the uriTemplate is compiled into a matcher up
// front, and a malformed or unsupported template is reported here rather than silently
// never matching anything. See compileURITemplate for the supported forms ({var}, and a
// terminal {+var}).
//
// Registering a uriTemplate that is already present replaces it and moves it to the end of
// the list, mirroring Register. Any completion provider previously attached with
// SetTemplateCompleter is carried over — re-registering to change a description should not
// silently drop completion support.
//
// Ordering matters: MatchTemplate is first-match-wins in registration order, so a general
// template registered first will shadow a more specific one registered later. Register
// "scheme://x/{a}/detail" before "scheme://x/{+rest}", not after.
//
// If the registry is already attached to a running Server, this fires a
// notifications/resources/list_changed — the specification has no template-specific
// notification, and list_changed covers the resource catalog as a whole.
func (r *ResourceRegistry) RegisterTemplate(tmpl ResourceTemplate, fn ResourceTemplateFunction) error {
	if fn == nil {
		return fmt.Errorf("uri template %q: nil ResourceTemplateFunction", tmpl.URITemplate)
	}
	re, vars, err := compileURITemplate(tmpl.URITemplate)
	if err != nil {
		return err
	}

	r.mu.Lock()
	compiled := &compiledTemplate{tmpl: tmpl, fn: fn, re: re, vars: vars}
	if existing, i := r.findTemplateLocked(tmpl.URITemplate); existing != nil {
		compiled.comp = existing.comp
		r.templates = append(r.templates[:i], r.templates[i+1:]...)
	}
	r.templates = append(r.templates, compiled)
	notify := r.onChange
	r.mu.Unlock()

	if notify != nil {
		notify()
	}
	return nil
}

// UnregisterTemplate removes the template registered under the exact uriTemplate string,
// reporting whether one was found. As with Unregister, removing something actually present
// fires notifications/resources/list_changed; removing an absent template is a no-op that
// notifies nobody.
func (r *ResourceRegistry) UnregisterTemplate(uriTemplate string) bool {
	r.mu.Lock()
	existing, i := r.findTemplateLocked(uriTemplate)
	removed := existing != nil
	if removed {
		r.templates = append(r.templates[:i], r.templates[i+1:]...)
	}
	notify := r.onChange
	r.mu.Unlock()

	if removed && notify != nil {
		notify()
	}
	return removed
}

// ListTemplates returns all registered resource templates, in registration order.
func (r *ResourceRegistry) ListTemplates() []ResourceTemplate {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Deliberately make() rather than a nil slice: this feeds resources/templates/list,
	// where an empty catalog must serialize as [] and not null.
	result := make([]ResourceTemplate, len(r.templates))
	for i, t := range r.templates {
		result[i] = t.tmpl
	}
	return result
}

// HasTemplates returns true if the registry has any resource templates.
func (r *ResourceRegistry) HasTemplates() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.templates) > 0
}

// MatchTemplate finds the first registered template matching a concrete URI, in registration
// order, and returns it along with its read function and the percent-decoded variable
// bindings extracted from the URI.
func (r *ResourceRegistry) MatchTemplate(uri string) (ResourceTemplate, ResourceTemplateFunction, map[string]string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	compiled, vars := r.matchTemplateLocked(uri)
	if compiled == nil {
		return ResourceTemplate{}, nil, nil, false
	}
	return compiled.tmpl, compiled.fn, vars, true
}

// SetTemplateCompleter attaches a completion provider to an already-registered template,
// enabling completion/complete for its variables. Passing nil removes it.
//
// This is what turns the completions capability on: a server with no completers anywhere
// does not advertise it and answers completion/complete with -32601, which is precisely the
// probe clients use to detect support.
func (r *ResourceRegistry) SetTemplateCompleter(uriTemplate string, p CompletionProvider) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, _ := r.findTemplateLocked(uriTemplate)
	if existing == nil {
		return fmt.Errorf("no resource template registered for %q", uriTemplate)
	}
	existing.comp = p
	return nil
}

// templateCompletion is an immutable snapshot of what completion/complete needs about one
// template, taken under the registry lock so a concurrent SetTemplateCompleter cannot race
// the handler.
type templateCompletion struct {
	provider CompletionProvider
	vars     []string // immutable after compilation; safe to share
}

func (tc templateCompletion) hasVar(name string) bool {
	for _, v := range tc.vars {
		if v == name {
			return true
		}
	}
	return false
}

// templateCompletion looks up a template by its exact uriTemplate string — which is what a
// completion/complete request's ref/resource "uri" carries.
func (r *ResourceRegistry) templateCompletion(uriTemplate string) (templateCompletion, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	existing, _ := r.findTemplateLocked(uriTemplate)
	if existing == nil {
		return templateCompletion{}, false
	}
	return templateCompletion{provider: existing.comp, vars: existing.vars}, true
}

// hasTemplateCompleters reports whether any registered template can answer
// completion/complete. It gates both the completions capability and the router.
func (r *ResourceRegistry) hasTemplateCompleters() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range r.templates {
		if t.comp != nil {
			return true
		}
	}
	return false
}

// findTemplateLocked returns the entry registered under the exact uriTemplate string and its
// index, or (nil, -1). Callers must hold r.mu (either mode).
func (r *ResourceRegistry) findTemplateLocked(uriTemplate string) (*compiledTemplate, int) {
	for i, t := range r.templates {
		if t.tmpl.URITemplate == uriTemplate {
			return t, i
		}
	}
	return nil, -1
}

// matchTemplateLocked runs the first-match-wins scan. Callers must hold r.mu — NotifyUpdated
// holds it for writing and MatchTemplate for reading, which is why the lock is the caller's
// job rather than this function's.
func (r *ResourceRegistry) matchTemplateLocked(uri string) (*compiledTemplate, map[string]string) {
	for _, t := range r.templates {
		if vars, ok := t.match(uri); ok {
			return t, vars
		}
	}
	return nil, nil
}

// ResourceContent is one entry in a resources/read result's contents array.
type ResourceContent struct {
	URI         string                 `json:"uri"`
	Name        string                 `json:"name,omitempty"`
	Title       string                 `json:"title,omitempty"`
	MimeType    string                 `json:"mimeType,omitempty"`
	Text        string                 `json:"text,omitempty"`
	Blob        string                 `json:"blob,omitempty"`
	Annotations *Annotations           `json:"annotations,omitempty"`
	Meta        map[string]interface{} `json:"_meta,omitempty"`
}

// ResourcesListResult is the result of resources/list.
type ResourcesListResult struct {
	CacheableResult
	Resources  []Resource `json:"resources"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

func (s *Server) handleResourcesList(ctx context.Context, params json.RawMessage) (Result, *transport.RPCError) {
	var p struct {
		Cursor string `json:"cursor"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}

	all := s.resourceRegistry.List()
	page, next, err := paginate(all, p.Cursor, defaultPageSize)
	if err != nil {
		return nil, invalidParamsErr("invalid cursor")
	}

	return &ResourcesListResult{
		CacheableResult: NewCacheableResult(s.listTTLMs, s.cacheScope()),
		Resources:       page,
		NextCursor:      next,
	}, nil
}

// ResourcesReadResult is the result of resources/read.
type ResourcesReadResult struct {
	CacheableResult
	Contents []ResourceContent `json:"contents"`
}

func (s *Server) handleResourcesRead(ctx context.Context, params json.RawMessage) (Result, *transport.RPCError) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalidParamsErr("invalid resources/read params: %v", err)
	}

	// Concrete resources win over templates: an exactly-registered URI is a deliberate
	// statement about that one URI, and a template that happens to also match it is the
	// more general claim.
	if res, ok := s.resourceRegistry.Get(p.URI); ok {
		content, err := s.resourceRegistry.Read(ctx, p.URI)
		if err != nil {
			return nil, readErr(p.URI, err)
		}
		return s.resourceReadResult(p.URI, res.Name, res.Title, res.MimeType, content), nil
	}

	if tmpl, fn, vars, ok := s.resourceRegistry.MatchTemplate(p.URI); ok {
		content, err := fn(ctx, &ResourceReadRequest{URI: p.URI, Vars: vars})
		if err != nil {
			return nil, readErr(p.URI, err)
		}
		return s.resourceReadResult(p.URI, tmpl.Name, tmpl.Title, tmpl.MimeType, content), nil
	}

	// An error, never an empty contents array: "no such resource" and "a resource that is
	// legitimately empty" must not look the same on the wire.
	return nil, invalidParamsErr("Unknown resource: %s", p.URI)
}

// resourceReadResult builds the resources/read result shared by the concrete and template
// paths. uri is always the concrete URI the client asked for — for a template read that is
// the expansion, not the template. registeredMime is whatever the Resource or
// ResourceTemplate declared; a per-read MimeType on the content overrides it, and
// "text/plain" is the fallback when neither says anything.
func (s *Server) resourceReadResult(uri, name, title, registeredMime string, content ResourceContentResult) *ResourcesReadResult {
	mimeType := registeredMime
	if content.MimeType != "" {
		mimeType = content.MimeType
	}
	if mimeType == "" {
		mimeType = "text/plain"
	}

	return &ResourcesReadResult{
		CacheableResult: NewCacheableResult(s.readTTLMs, s.cacheScope()),
		Contents: []ResourceContent{
			{
				URI:         uri,
				Name:        name,
				Title:       title,
				MimeType:    mimeType,
				Text:        content.Text,
				Blob:        content.Blob,
				Annotations: content.Annotations,
				Meta:        content.Meta,
			},
		},
	}
}
