package mcp

import (
	"fmt"
	"sync"
)

// Resource represents an MCP resource definition
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ResourceFunction returns the content of a resource
type ResourceFunction func() (string, error)

// ResourceRegistry manages available resources
type ResourceRegistry struct {
	mu        sync.RWMutex
	resources []Resource
	functions map[string]ResourceFunction // keyed by URI
}

// NewResourceRegistry creates a new resource registry
func NewResourceRegistry() *ResourceRegistry {
	return &ResourceRegistry{
		resources: []Resource{},
		functions: make(map[string]ResourceFunction),
	}
}

// Register adds a resource and its function to the registry
func (r *ResourceRegistry) Register(res Resource, fn ResourceFunction) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resources = append(r.resources, res)
	r.functions[res.URI] = fn
}

// List returns all registered resources
func (r *ResourceRegistry) List() []Resource {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Return a copy to prevent external modification
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

// Read executes the function for the given resource URI and returns its content
func (r *ResourceRegistry) Read(uri string) (string, error) {
	r.mu.RLock()
	fn, exists := r.functions[uri]
	r.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("resource not found: %s", uri)
	}

	return fn()
}

// HasResources returns true if the registry has any resources
func (r *ResourceRegistry) HasResources() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.resources) > 0
}
