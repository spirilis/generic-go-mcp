package tools

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spirilis/generic-go-mcp/internal/mcp"
)

// DateArguments represents the arguments for the date tool
type DateArguments struct {
	Timezone string `json:"timezone"`
}

// DateTool returns the current date/time in the specified timezone
func DateTool(arguments json.RawMessage) (interface{}, error) {
	var args DateArguments
	if err := json.Unmarshal(arguments, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	// Default to UTC if no timezone specified
	timezone := args.Timezone
	if timezone == "" {
		timezone = "UTC"
	}

	// Load the timezone
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone: %w", err)
	}

	// Get current time in the specified timezone
	now := time.Now().In(loc)
	formatted := now.Format("2006-01-02 15:04:05 MST")

	return mcp.ToolCallResult{
		Content: []mcp.ToolContent{
			{
				Type: "text",
				Text: formatted,
			},
		},
	}, nil
}

// GetDateToolDefinition returns the MCP tool definition for date
func GetDateToolDefinition() mcp.Tool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"timezone": {
				"type": "string",
				"description": "IANA timezone name (e.g., 'America/New_York', 'Europe/London', 'Asia/Tokyo')"
			}
		},
		"required": ["timezone"]
	}`)

	return mcp.Tool{
		Name:        "date",
		Description: "Returns the current date and time in the specified timezone",
		InputSchema: schema,
	}
}
