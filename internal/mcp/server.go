package mcp

import (
	"encoding/json"

	"github.com/spirilis/generic-go-mcp/internal/transport"
)

// Server implements the MCP protocol
type Server struct {
	registry    *ToolRegistry
	initialized bool
}

// NewServer creates a new MCP server
func NewServer(registry *ToolRegistry) *Server {
	return &Server{
		registry:    registry,
		initialized: false,
	}
}

// HandleMessage processes incoming JSON-RPC messages
func (s *Server) HandleMessage(data []byte) []byte {
	var req transport.JSONRPCRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return s.errorResponse(nil, transport.ParseError, "Parse error", err)
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
		return s.errorResponse(req.ID, transport.MethodNotFound, "Method not found", nil)
	}

	if err != nil {
		return s.errorResponse(req.ID, transport.InternalError, err.Error(), nil)
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
