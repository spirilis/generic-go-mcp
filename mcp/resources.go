package mcp

import (
	"context"
	"encoding/json"
	"fmt"
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
type ResourceContentResult struct {
	Text     string
	Blob     string
	MimeType string
}

// ResourceFunction produces the content of a resource when read.
type ResourceFunction func(ctx context.Context) (ResourceContentResult, error)

// ResourceRegistry manages available resources, safe for concurrent registration, lookup,
// and reads.
type ResourceRegistry struct {
	mu        sync.RWMutex
	resources []Resource
	functions map[string]ResourceFunction // keyed by URI
	onChange  func()
}

// NewResourceRegistry creates a new resource registry
func NewResourceRegistry() *ResourceRegistry {
	return &ResourceRegistry{
		resources: []Resource{},
		functions: make(map[string]ResourceFunction),
	}
}

// Register adds a resource and its function to the registry. If the registry is already
// attached to a running Server, this fires a notifications/resources/list_changed to any
// subscribed clients.
func (r *ResourceRegistry) Register(res Resource, fn ResourceFunction) {
	r.mu.Lock()
	r.resources = append(r.resources, res)
	r.functions[res.URI] = fn
	notify := r.onChange
	r.mu.Unlock()
	if notify != nil {
		notify()
	}
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

	res, ok := s.resourceRegistry.Get(p.URI)
	if !ok {
		return nil, invalidParamsErr("Unknown resource: %s", p.URI)
	}

	content, err := s.resourceRegistry.Read(ctx, p.URI)
	if err != nil {
		return nil, internalErr(err)
	}

	mimeType := res.MimeType
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
				URI:      p.URI,
				Name:     res.Name,
				Title:    res.Title,
				MimeType: mimeType,
				Text:     content.Text,
				Blob:     content.Blob,
			},
		},
	}, nil
}
