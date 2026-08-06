# Authoring tools

A tool is two things registered together: an `mcp.Tool` (the JSON-Schema-described definition the
client sees from `tools/list`) and an `mcp.ToolFunction` (the Go handler that runs the call). Look
at `examples/tools/date.go`, `examples/tools/fortune.go`, and `examples/tools/confirm.go` in this
repo for three real, working reference tools spanning the common shapes below.

## The handler signature

```go
type ToolFunction func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error)
```

- Return a **non-nil `error`** only for a protocol-level failure (mapped to JSON-RPC `-32603
  Internal error`) — something the model can't act on, like a downstream service being
  unreachable in a way that isn't the caller's fault.
- For anything the model *should* see and could potentially recover from — bad input, a
  business-rule rejection, a resource not found — return `nil` error and a result with `IsError:
  true`. Use the `mcp.ErrorResultf` helper:

  ```go
  return mcp.ErrorResultf("invalid timezone: %v", err), nil
  ```

  This is the same pattern `examples/tools/date.go` uses for bad input and
  `examples/tools/fortune.go` uses when the `fortune` binary itself fails.

## Reading arguments

`ToolRequest.BindArguments` unmarshals the call's raw JSON arguments into a struct you define:

```go
type DateArguments struct {
	Timezone string `json:"timezone"`
}

func DateTool(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
	var args DateArguments
	if err := req.BindArguments(&args); err != nil {
		return mcp.ErrorResultf("invalid arguments: %v", err), nil
	}
	...
}
```

## Building the result

`mcp.ToolCallResult` is what a normal (non-MRTR) tool call returns:

```go
type ToolCallResult struct {
	Content           []Content   // required — what the model sees
	StructuredContent interface{} // optional — machine-readable companion, validated against OutputSchema
	IsError           bool        // set true for tool execution errors (see above)
}
```

`Content` is a union type; build entries with constructors rather than struct literals:

| Constructor | Produces |
|---|---|
| `mcp.Text(s string)` | a text block |
| `mcp.Image(base64Data, mimeType string)` | an image block |
| `mcp.Audio(base64Data, mimeType string)` | an audio block |
| `mcp.ResourceLinkContent(uri, name, description, mimeType string)` | a pointer to a resource the client can separately `resources/read` |
| `mcp.EmbeddedResourceContent(mcp.EmbeddedResourceContents{...})` | a resource's content embedded inline |

Returning both `Content` (human-readable) and `StructuredContent` (machine-readable, validated
against the tool's `OutputSchema` if set) is the recommended pattern for a tool whose output another
program might consume — see `examples/tools/date.go`, which returns a formatted string in `Content`
and `{iso8601, timezone}` in `StructuredContent`.

## Defining the schema

```go
func GetDateToolDefinition() mcp.Tool {
	return mcp.Tool{
		Name:        "date",
		Title:       "Current Date/Time",       // optional, human-facing
		Description: "Returns the current date and time in the specified timezone",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"timezone": {"type": "string", "description": "IANA timezone name"}
			},
			"required": ["timezone"]
		}`),
		OutputSchema: json.RawMessage(`{...}`), // optional; validates StructuredContent
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: true, // hints only — untrusted, never a security boundary
		},
	}
}
```

`InputSchema`/`OutputSchema` are `json.RawMessage`, i.e. hand-written JSON Schema (2020-12 by
default) — there's no Go-struct-to-schema generator in this library. Keep the raw-string literal
pattern shown above; it's what every reference tool does.

`ToolAnnotations` (`Title`, `ReadOnlyHint`, `DestructiveHint *bool`, `IdempotentHint`,
`OpenWorldHint *bool`) are **hints**, not enforced — a client MUST NOT rely on them for security
decisions. `examples/tools/confirm.go` sets `DestructiveHint: &destructive` on a delete-like tool
purely as a UI signal; the actual safety mechanism for a destructive action is MRTR confirmation
(see `mrtr-and-resources.md`), not the annotation.

## Registering

```go
registry := mcp.NewToolRegistry()
registry.Register(GetDateToolDefinition(), DateTool)
registry.Unregister("date") // bool: whether a tool by that name was there to remove
```

`ToolRegistry` is safe for concurrent registration, mutation, and lookup, including *after* the
server has started. Registering the first tool is what makes the server advertise a `tools`
capability from `server/discover`; unregistering the last one withdraws it.

Three things to know before you mutate a live catalog:

- **Registering an existing name replaces that tool** and moves it to the end of the list rather
  than adding a second entry, so `tools/list` can never disagree with what `tools/call` will run.
- **A tool can vanish mid-call.** If `Unregister` lands between a caller's `tools/list` and its
  `tools/call` — or between the router's lookup and the call itself — the caller gets the same
  "Unknown tool" error a never-registered name produces, not a crash.
- **Any mutation that actually changes the catalog fires
  `notifications/tools/list_changed`** to clients listening via `subscriptions/listen`;
  `Unregister` on an absent name returns `false` and notifies nobody.

See `notifications-and-registries.md` for the full picture, including the delivery contract for
those notifications (they can be dropped — don't treat them as an event log).

## x-mcp-header (advanced, HTTP-only)

If an `InputSchema` property sets `"x-mcp-header": "SomeName"`, the Streamable HTTP transport
requires the caller to mirror that argument's value into an `Mcp-Param-SomeName` header and will
reject the call with `-32020 HeaderMismatch` if the header is absent or doesn't match the argument.
This only applies to properties statically reachable via nested `properties` (not `items`,
`oneOf`/`anyOf`/`allOf`, or `$ref`), and never applies on stdio/UNIX (no header layer exists there).
Most tools never need this — it exists for parameters an HTTP proxy or gateway needs to see without
parsing the JSON-RPC body (e.g. routing/rate-limiting by tenant). See `mcp/xmcpheader.go` if you
need it.
