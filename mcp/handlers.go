package mcp

import (
	"encoding/json"
	"fmt"
)

// InitializeParams represents the parameters for the initialize request
type InitializeParams struct {
	ProtocolVersion string                 `json:"protocolVersion"`
	Capabilities    map[string]interface{} `json:"capabilities"`
	ClientInfo      ClientInfo             `json:"clientInfo"`
}

// ClientInfo contains information about the client
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResult represents the result of the initialize request
type InitializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    Capabilities `json:"capabilities"`
	ServerInfo      ServerInfo   `json:"serverInfo"`
}

// Capabilities represents server capabilities
type Capabilities struct {
	Tools     map[string]interface{}  `json:"tools"`
	Resources *map[string]interface{} `json:"resources,omitempty"`
}

// ServerInfo contains information about the server
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// handleInitialize processes the initialize request
func (s *Server) handleInitialize(params json.RawMessage) (interface{}, error) {
	var initParams InitializeParams
	if err := json.Unmarshal(params, &initParams); err != nil {
		return nil, fmt.Errorf("invalid initialize params: %w", err)
	}

	capabilities := Capabilities{
		Tools: map[string]interface{}{},
	}

	// Include resources capability if we have any resources
	if s.resourceRegistry.HasResources() {
		emptyMap := make(map[string]interface{})
		capabilities.Resources = &emptyMap
	}

	return InitializeResult{
		ProtocolVersion: "2024-11-05",
		Capabilities:    capabilities,
		ServerInfo: ServerInfo{
			Name:    s.config.Name,
			Version: s.config.Version,
		},
	}, nil
}

// handleNotification processes notification messages
func (s *Server) handleNotification(method string, params json.RawMessage) error {
	if method == "notifications/initialized" {
		s.initialized = true
		return nil
	}
	return nil
}

// ToolsListResult represents the result of tools/list request
type ToolsListResult struct {
	Tools []Tool `json:"tools"`
}

// handleToolsList returns the list of available tools
func (s *Server) handleToolsList(params json.RawMessage) (interface{}, error) {
	return ToolsListResult{
		Tools: s.registry.List(),
	}, nil
}

// ToolsCallParams represents the parameters for tools/call request
type ToolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// handleToolsCall executes a tool and returns the result
func (s *Server) handleToolsCall(params json.RawMessage) (interface{}, error) {
	var callParams ToolsCallParams
	if err := json.Unmarshal(params, &callParams); err != nil {
		return nil, fmt.Errorf("invalid tools/call params: %w", err)
	}

	result, err := s.registry.Call(callParams.Name, callParams.Arguments)
	if err != nil {
		return nil, err
	}

	return result, nil
}

// ResourcesListResult represents the result of resources/list request
type ResourcesListResult struct {
	Resources []Resource `json:"resources"`
}

// handleResourcesList returns the list of available resources
func (s *Server) handleResourcesList(params json.RawMessage) (interface{}, error) {
	return ResourcesListResult{
		Resources: s.resourceRegistry.List(),
	}, nil
}

// ResourcesReadParams represents the parameters for resources/read request
type ResourcesReadParams struct {
	URI string `json:"uri"`
}

// ResourcesReadResult represents the result of resources/read request
type ResourcesReadResult struct {
	Contents []ResourceContent `json:"contents"`
}

// ResourceContent represents the content of a resource
type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
}

// handleResourcesRead reads a resource and returns its content
func (s *Server) handleResourcesRead(params json.RawMessage) (interface{}, error) {
	var readParams ResourcesReadParams
	if err := json.Unmarshal(params, &readParams); err != nil {
		return nil, fmt.Errorf("invalid resources/read params: %w", err)
	}

	content, err := s.resourceRegistry.Read(readParams.URI)
	if err != nil {
		return nil, err
	}

	// Get the resource metadata for MIME type
	mimeType := "text/plain" // default fallback
	if res, found := s.resourceRegistry.Get(readParams.URI); found && res.MimeType != "" {
		mimeType = res.MimeType
	}

	return ResourcesReadResult{
		Contents: []ResourceContent{
			{
				URI:      readParams.URI,
				MimeType: mimeType,
				Text:     content,
			},
		},
	}, nil
}
