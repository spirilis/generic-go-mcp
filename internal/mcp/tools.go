package mcp

import (
	"encoding/json"
	"fmt"
)

// Tool represents an MCP tool definition
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolFunction is the actual implementation of a tool
type ToolFunction func(arguments json.RawMessage) (interface{}, error)

// ToolRegistry manages available tools
type ToolRegistry struct {
	tools     []Tool
	functions map[string]ToolFunction
}

// NewToolRegistry creates a new tool registry
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools:     make([]Tool, 0),
		functions: make(map[string]ToolFunction),
	}
}

// Register adds a tool to the registry
func (r *ToolRegistry) Register(tool Tool, fn ToolFunction) {
	r.tools = append(r.tools, tool)
	r.functions[tool.Name] = fn
}

// List returns all registered tools
func (r *ToolRegistry) List() []Tool {
	return r.tools
}

// Call executes a tool by name
func (r *ToolRegistry) Call(name string, arguments json.RawMessage) (interface{}, error) {
	fn, exists := r.functions[name]
	if !exists {
		return nil, fmt.Errorf("tool not found: %s", name)
	}
	return fn(arguments)
}

// ToolContent represents the content of a tool response
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolCallResult represents the result of a tool call
type ToolCallResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}
