package mcp

import (
	"encoding/json"

	"github.com/spirilis/generic-go-mcp/logging"
	"github.com/spirilis/generic-go-mcp/transport"
)

// ServerConfig holds configuration for the MCP server
type ServerConfig struct {
	Name    string // Server name (default: "generic-go-mcp")
	Version string // Server version (default: "0.1.0")
}

// Server implements the MCP protocol
type Server struct {
	registry    *ToolRegistry
	config      ServerConfig
	initialized bool
}

// NewServer creates a new MCP server with the given registry and configuration.
// If config is nil, default values are used.
func NewServer(registry *ToolRegistry, config *ServerConfig) *Server {
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
	}
	return &Server{
		registry:    registry,
		config:      cfg,
		initialized: false,
	}
}

// HandleMessage processes incoming JSON-RPC messages
func (s *Server) HandleMessage(data []byte) []byte {
	var req transport.JSONRPCRequest
	if err := json.Unmarshal(data, &req); err != nil {
		logging.Debug("JSON-RPC parse error", "error", err)
		return s.errorResponse(nil, transport.ParseError, "Parse error", err)
	}

	// Log JSON-RPC method call
	logging.Debug("JSON-RPC request", "method", req.Method, "id", req.ID)

	// Trace: Log full params
	if logging.IsTraceEnabled() && req.Params != nil {
		paramsJSON, _ := json.Marshal(req.Params)
		logging.Trace("JSON-RPC params", "method", req.Method, "params", string(paramsJSON))
	}

	// Handle notifications (no response)
	if req.ID == nil {
		s.handleNotification(req.Method, req.Params)
		return nil
	}

	// Route to appropriate handler
	var result interface{}
	var err error

	switch req.Method {
	case "initialize":
		result, err = s.handleInitialize(req.Params)
	case "tools/list":
		result, err = s.handleToolsList(req.Params)
	case "tools/call":
		result, err = s.handleToolsCall(req.Params)
	default:
		logging.Debug("JSON-RPC method not found", "method", req.Method)
		return s.errorResponse(req.ID, transport.MethodNotFound, "Method not found", nil)
	}

	if err != nil {
		logging.Debug("JSON-RPC error", "method", req.Method, "error", err)
		return s.errorResponse(req.ID, transport.InternalError, err.Error(), nil)
	}

	// Trace: Log response data
	if logging.IsTraceEnabled() && result != nil {
		resultJSON, _ := json.Marshal(result)
		logging.Trace("JSON-RPC response", "method", req.Method, "result", string(resultJSON))
	}

	return s.successResponse(req.ID, result)
}

// successResponse creates a successful JSON-RPC response
func (s *Server) successResponse(id interface{}, result interface{}) []byte {
	resp := transport.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	data, _ := json.Marshal(resp)
	return data
}

// errorResponse creates an error JSON-RPC response
func (s *Server) errorResponse(id interface{}, code int, message string, data interface{}) []byte {
	resp := transport.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &transport.RPCError{
			Code:    code,
			Message: message,
			Data:    data,
		},
	}
	respData, _ := json.Marshal(resp)
	return respData
}
