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
	Tools map[string]interface{} `json:"tools"`
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

	return InitializeResult{
		ProtocolVersion: "2024-11-05",
		Capabilities: Capabilities{
			Tools: map[string]interface{}{},
		},
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
