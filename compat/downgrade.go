package compat

import (
	"encoding/json"
	"fmt"

	"github.com/spirilis/generic-go-mcp/mcp"
	"github.com/spirilis/generic-go-mcp/transport"
)

// legacyMRTRRefusalText is shown to a legacy client instead of an input_required (MRTR)
// result it has no way to answer and retry: this revision predates Multi Round-Trip
// Requests entirely, and there is no way to emulate it — that would require a
// server-initiated request down a bidirectional channel this transport no longer has.
const legacyMRTRRefusalText = "This tool needs additional input before it can run, which " +
	"requires MCP protocol revision %s (Multi Round-Trip Requests). The negotiated " +
	"revision has no equivalent."

// legacyTextContent is the wire shape of one tools/call content entry, in both eras.
type legacyTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// legacyToolCallResult is the wire shape of a legacy tools/call result: content and
// isError, and nothing else — no resultType, no structuredContent (2026-07-28 additions).
type legacyToolCallResult struct {
	Content []legacyTextContent `json:"content"`
	IsError bool                `json:"isError"`
}

// mrtrRefusalResult marshals the visible tool-call error a legacy client sees in place of
// an input_required result, naming the modern revision and leaking no inputRequests or
// requestState.
func mrtrRefusalResult() json.RawMessage {
	text := fmt.Sprintf(legacyMRTRRefusalText, mcp.ProtocolVersion)
	out, _ := json.Marshal(legacyToolCallResult{
		Content: []legacyTextContent{{Type: "text", Text: text}},
		IsError: true,
	})
	return out
}

// downgradeResult strips the modern (2026-07-28) result envelope from raw — a successful
// result captured from the inner server — leaving the legacy shape underneath: tools/list
// becomes {tools, nextCursor?}, tools/call becomes {content, isError?}, resources/read
// becomes {contents}, and so on. resultType, ttlMs, cacheScope, and _meta.serverInfo are
// all inventions of the modern envelope with no legacy equivalent, so they are removed
// rather than translated. TTLMs and ResultType are declared without `omitempty` in mcp/,
// so leaving either in place would leak a spurious "ttlMs":0 or "resultType":"complete"
// into a response a legacy client has no field for.
//
// A result whose resultType is "input_required" (Multi Round-Trip Requests) cannot be
// downgraded at all — see mrtrRefusalResult — so it is replaced outright rather than
// stripped.
func downgradeResult(raw json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		// Not a JSON object — nothing this library's Result implementations ever
		// produce, but pass it through unchanged rather than guess at a transformation.
		return raw
	}

	if rt, ok := fields["resultType"]; ok {
		var resultType string
		if err := json.Unmarshal(rt, &resultType); err == nil && resultType == mcp.ResultTypeInputRequired {
			return mrtrRefusalResult()
		}
	}

	delete(fields, "resultType")
	delete(fields, "ttlMs")
	delete(fields, "cacheScope")
	delete(fields, "_meta")

	out, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return out
}

// downgradeResponse translates data — a raw JSON-RPC response captured from the inner
// modern server — into its legacy equivalent, ready to write to a legacy client. Error
// responses pass through unchanged: a JSON-RPC error object (code/message/data) carries no
// modern-only envelope fields to strip, so there is nothing to downgrade. id, if non-nil,
// overrides the id in data — used when the overlay forwards a translated request under a
// synthetic id and must report the result under the legacy client's original one.
func downgradeResponse(data []byte, id json.RawMessage) []byte {
	var env struct {
		ID     json.RawMessage     `json:"id"`
		Result json.RawMessage     `json:"result,omitempty"`
		Error  *transport.RPCError `json:"error,omitempty"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return data
	}
	if id == nil {
		id = env.ID
	}

	if env.Error != nil {
		return transport.NewErrorResponse(id, env.Error)
	}
	if len(env.Result) == 0 {
		return data
	}
	return transport.NewSuccessResponse(id, downgradeResult(env.Result))
}
