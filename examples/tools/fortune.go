package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"

	"github.com/spirilis/generic-go-mcp/mcp"
)

// FortuneTool executes the fortune command and returns output
func FortuneTool(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
	// Execute fortune command
	cmd := exec.CommandContext(ctx, "fortune")
	output, err := cmd.Output()
	if err != nil {
		return mcp.ErrorResultf("failed to execute fortune: %v", err), nil
	}

	return &mcp.ToolCallResult{
		Content: []mcp.Content{mcp.Text(strings.TrimSpace(string(output)))},
	}, nil
}

// GetFortuneToolDefinition returns the MCP tool definition for fortune
func GetFortuneToolDefinition() mcp.Tool {
	schema := json.RawMessage(`{
		"type": "object",
		"additionalProperties": false
	}`)

	return mcp.Tool{
		Name:        "fortune",
		Description: "Returns a random fortune from the fortune command",
		InputSchema: schema,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: true,
		},
	}
}
