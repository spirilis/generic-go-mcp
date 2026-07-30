package tools

import (
	"context"
	"encoding/json"
	"time"

	"github.com/spirilis/generic-go-mcp/mcp"
)

// DateArguments represents the arguments for the date tool
type DateArguments struct {
	Timezone string `json:"timezone"`
}

// DateTool returns the current date/time in the specified timezone, as both a
// human-readable text block and structured content — the reference example for a tool
// that returns structuredContent alongside an outputSchema.
func DateTool(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
	var args DateArguments
	if err := req.BindArguments(&args); err != nil {
		return mcp.ErrorResultf("invalid arguments: %v", err), nil
	}

	// Default to UTC if no timezone specified
	timezone := args.Timezone
	if timezone == "" {
		timezone = "UTC"
	}

	// Load the timezone
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return mcp.ErrorResultf("invalid timezone: %v", err), nil
	}

	// Get current time in the specified timezone
	now := time.Now().In(loc)
	formatted := now.Format("2006-01-02 15:04:05 MST")

	return &mcp.ToolCallResult{
		Content: []mcp.Content{mcp.Text(formatted)},
		StructuredContent: map[string]interface{}{
			"iso8601":  now.Format(time.RFC3339),
			"timezone": loc.String(),
		},
	}, nil
}

// GetDateToolDefinition returns the MCP tool definition for date
func GetDateToolDefinition() mcp.Tool {
	inputSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"timezone": {
				"type": "string",
				"description": "IANA timezone name (e.g., 'America/New_York', 'Europe/London', 'Asia/Tokyo')"
			}
		},
		"required": ["timezone"]
	}`)

	outputSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"iso8601": {
				"type": "string",
				"description": "Current time in RFC 3339 / ISO 8601 format"
			},
			"timezone": {
				"type": "string",
				"description": "Resolved IANA timezone name"
			}
		},
		"required": ["iso8601", "timezone"]
	}`)

	return mcp.Tool{
		Name:         "date",
		Title:        "Current Date/Time",
		Description:  "Returns the current date and time in the specified timezone",
		InputSchema:  inputSchema,
		OutputSchema: outputSchema,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: true,
		},
	}
}
