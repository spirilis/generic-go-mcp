package tools

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/spirilis/generic-go-mcp/internal/mcp"
)

// FortuneTool executes the fortune command and returns output
func FortuneTool(arguments json.RawMessage) (interface{}, error) {
	// Execute fortune command
	cmd := exec.Command("fortune")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to execute fortune: %w", err)
	}

	return mcp.ToolCallResult{
		Content: []mcp.ToolContent{
			{
				Type: "text",
				Text: strings.TrimSpace(string(output)),
			},
		},
	}, nil
}

// GetFortuneToolDefinition returns the MCP tool definition for fortune
func GetFortuneToolDefinition() mcp.Tool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {}
	}`)

	return mcp.Tool{
		Name:        "fortune",
		Description: "Returns a random fortune from the fortune command",
		InputSchema: schema,
	}
}
