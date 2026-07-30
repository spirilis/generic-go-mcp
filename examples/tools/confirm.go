package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spirilis/generic-go-mcp/mcp"
)

// ConfirmArguments represents the arguments for the confirm_delete tool.
type ConfirmArguments struct {
	Count int `json:"count"`
}

var confirmSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"confirm": {"type": "boolean"}
	},
	"required": ["confirm"]
}`)

// ConfirmTool is the reference Multi Round-Trip Requests (MRTR) tool: it asks the client
// to confirm a destructive-looking action via elicitation before "completing" it.
//
// Call it once with just {"count": N}: it returns an input_required result carrying an
// elicitation/create request and a signed requestState. Retry the identical call (new
// JSON-RPC id) with inputResponses.confirm = {"action":"accept","content":{"confirm":true}}
// and requestState echoed back verbatim to see it complete.
func ConfirmTool(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
	var args ConfirmArguments
	if err := req.BindArguments(&args); err != nil {
		return mcp.ErrorResultf("invalid arguments: %v", err), nil
	}

	if !req.ClientCapabilities.HasElicitation() {
		return mcp.ErrorResultf("this tool requires elicitation support, which the client did not declare"), nil
	}

	answer, asked := req.ElicitResponse("confirm")
	if !asked {
		return req.NeedInput(mcp.InputRequests{
			"confirm": mcp.NewElicitRequest("form", fmt.Sprintf("Delete %d record(s)?", args.Count), confirmSchema),
		})
	}
	if !answer.Accepted() {
		return mcp.ErrorResultf("cancelled by user"), nil
	}

	return &mcp.ToolCallResult{
		Content: []mcp.Content{mcp.Text(fmt.Sprintf("Deleted %d record(s)", args.Count))},
	}, nil
}

// GetConfirmToolDefinition returns the MCP tool definition for confirm_delete.
func GetConfirmToolDefinition() mcp.Tool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"count": {
				"type": "integer",
				"description": "Number of records to (pretend to) delete"
			}
		},
		"required": ["count"]
	}`)

	destructive := true
	return mcp.Tool{
		Name:        "confirm_delete",
		Title:       "Confirm Delete (MRTR reference)",
		Description: "Deletes N records, asking the user to confirm first via elicitation. Reference implementation of the Multi Round-Trip Requests (MRTR) pattern.",
		InputSchema: schema,
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: &destructive,
		},
	}
}
