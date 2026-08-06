# MRTR (confirm-before-act tools) and resources

## Multi Round-Trip Requests (MRTR)

2026-07-28 removed server-initiated JSON-RPC requests entirely — a server can no longer push a
`elicitation/create` (or `sampling/createMessage`, or `roots/list`) request down to the client
mid-call the way earlier revisions did. **MRTR replaces that**: a tool that needs more information
returns a special `input_required` result instead of completing, and the client is expected to
retry the *same* call (new JSON-RPC `id`, same tool name/arguments) with the answer attached.

This is the pattern to reach for any time you'd have used elicitation, sampling, or roots in an
older MCP server — most commonly, confirming a destructive action before it happens.

### The full round trip, worked example

`examples/tools/confirm.go` is the reference implementation — a `confirm_delete(count)` tool that
asks "Delete N records?" before doing anything. Read it in full; here's the shape:

```go
func ConfirmTool(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
	var args ConfirmArguments
	if err := req.BindArguments(&args); err != nil {
		return mcp.ErrorResultf("invalid arguments: %v", err), nil
	}

	// A tool that may send an inputRequest MUST check the client declared the
	// capability first — sending elicitation/create to a client that never
	// declared elicitation support is a protocol violation.
	if !req.ClientCapabilities.HasElicitation() {
		return mcp.ErrorResultf("this tool requires elicitation support, which the client did not declare"), nil
	}

	// On a retry, the client's answer arrives here, keyed by whatever name
	// you chose below ("confirm").
	answer, asked := req.ElicitResponse("confirm")
	if !asked {
		// First call: ask, and return input_required instead of completing.
		return req.NeedInput(mcp.InputRequests{
			"confirm": mcp.NewElicitRequest("form", fmt.Sprintf("Delete %d record(s)?", args.Count), confirmSchema),
		})
	}
	if !answer.Accepted() {
		return mcp.ErrorResultf("cancelled by user"), nil
	}

	// Second call, confirmed: actually do the thing.
	return &mcp.ToolCallResult{
		Content: []mcp.Content{mcp.Text(fmt.Sprintf("Deleted %d record(s)", args.Count))},
	}, nil
}
```

**On the wire**, the first call to `confirm_delete` with `{"count": 12}` gets back:

```json
{
  "resultType": "input_required",
  "inputRequests": {
    "confirm": {
      "method": "elicitation/create",
      "params": {"mode": "form", "message": "Delete 12 record(s)?", "requestedSchema": {"...": "..."}}
    }
  },
  "requestState": "<opaque HMAC-signed blob>"
}
```

The client resolves that with the user, then sends a **new JSON-RPC request** (new `id`) — same
`name` and `arguments` as the original call, plus the answer and the echoed `requestState`:

```json
{
  "method": "tools/call",
  "params": {
    "name": "confirm_delete",
    "arguments": {"count": 12},
    "inputResponses": {"confirm": {"action": "accept", "content": {"confirm": true}}},
    "requestState": "<the exact blob from the previous response>"
  }
}
```

That completes and returns the normal `ToolCallResult`.

### What `requestState` protects against

The blob is HMAC-signed (`mcp/mrtr.go`) over: the calling principal (via
`ServerConfig.PrincipalFromContext`), an expiry (5 minutes, `requestStateTTL`), and a digest of the
exact tool name + arguments it was issued for. `req.NeedInput`/the router reject a retry whose
`requestState` is tampered with, expired, issued for a different principal, or issued for a
different tool call — it's not just an opaque token you can round-trip blindly; it's actually
verified server-side on the retry (`s.verifyRequestState` in `mcp/mrtr.go`, called from
`handleToolsCall` in `mcp/tools.go`).

If you never set `ServerConfig.PrincipalFromContext`, every caller is treated as the same empty
principal — fine for an unauthenticated server, but means an MRTR confirmation from user A's
session could technically be replayed by user B if `requestState` leaked between them. Wire it to
`auth.GetUserFromContext` if you're running with OAuth (see `transports-and-auth.md`).

### Capability checks are mandatory, not optional

`req.NeedInput` itself returns a `*mcp.MissingCapabilityError` (translated to the `-32021` JSON-RPC
error) if any `InputRequests` entry names a method (`elicitation/create`, `sampling/createMessage`,
`roots/list`) the caller's declared `ClientCapabilities` doesn't cover. The explicit
`req.ClientCapabilities.HasElicitation()` check in the example above is a courtesy early-exit with
a friendlier message — the framework enforces the rule regardless.

## Resources

Simpler than tools — no MRTR involved. A `Resource` is metadata (URI, name, description, MIME
type); a `ResourceFunction` produces its content on read.

```go
resources := mcp.NewResourceRegistry()
resources.Register(
	mcp.Resource{
		URI:         "config://server-name",
		Name:        "Server Name",
		Description: "The configured name of this server",
		MimeType:    "text/plain",
	},
	func(ctx context.Context) (mcp.ResourceContentResult, error) {
		return mcp.ResourceContentResult{Text: "my-server"}, nil
	},
)
```

`ResourceContentResult{Text, Blob, MimeType}` — set `Text` for text content or `Blob` (base64) for
binary; `MimeType` here overrides the one registered on `Resource` for this particular read, if you
need per-read variance. If your resource URI has no natural scheme (e.g. it isn't `file://` or
`https://`), invent one — `examples/go-mcp/main.go`'s UNIX-socket mode registers `mcp+unix:///name`
and `mcp+unix:///pid` for exactly this reason, since a bare `/name` isn't a valid absolute URI.

Like tools, `ResourceRegistry.Register` after the server has started fires
`notifications/resources/list_changed` to subscribed clients automatically — you don't need to do
anything extra for dynamic resource sets.

### Removing and replacing entries

`Unregister` is the counterpart, keyed by URI on resources and by name on tools. It returns whether
there was anything to remove, and fires `list_changed` only when it actually removed something:

```go
resources.Unregister("config://server-name") // true
resources.Unregister("config://server-name") // false — already gone, notifies nobody
registry.Unregister("my_tool")               // same contract on ToolRegistry
```

Re-registering a URI or name that already exists **replaces** the entry and moves it to the end of
the list rather than adding a duplicate, so `resources/list` can never disagree with what
`resources/read` will actually run. Removing the last resource also withdraws the `resources`
capability from `server/discover`, since capabilities are derived from what is registered.

The protocol has no "unregister" method — this is purely server-side. Don't confuse it with the
`resources/subscribe`/`resources/unsubscribe` methods, which 2026-07-28 deleted outright; those
were about a *client* subscribing to updates on a resource, and their replacement is
`subscriptions/listen`.

### Announcing that a resource's content changed

`list_changed` covers the *catalog*: something was added, replaced, or removed. It says nothing
about the bytes behind a URI that has been registered all along. That is a separate notification,
`notifications/resources/updated`, and you have to fire it yourself:

```go
// Your code just rewrote whatever this resource reads from.
resources.NotifyUpdated("config://server-name") // true — the URI is registered
resources.NotifyUpdated("config://typo")        // false — no-op, notifies nobody
```

It cannot be automatic. A `ResourceFunction` is called on demand and returns whatever it likes; the
library never sees the underlying data and has no way to know it changed. Only your code does.

Two rules to keep in mind:

- **URIs are matched exactly.** The spec allows a server to announce a sub-resource of a URI the
  client subscribed to; this library does not, because a `ResourceRegistry` is a flat map with no
  URI hierarchy to derive one from. A client hears about the URIs it literally named.
- **Re-registering a URI fires `resources/updated` too**, on top of `list_changed`, because
  `Register` replaces the `ResourceFunction` as well as the metadata and Go can't compare func
  values — the registry can't tell a `Description` fix from a content swap, so it announces both.
  A first-time `Register` of a URI fires `list_changed` only.

## subscriptions/listen (change notifications)

A client that wants to hear about changes calls `subscriptions/listen` once and leaves the request
open. Its `notifications` param is a `NotificationFilter` naming what it wants:

```json
{
  "jsonrpc": "2.0", "id": 4, "method": "subscriptions/listen",
  "params": {
    "notifications": {
      "toolsListChanged": true,
      "resourcesListChanged": true,
      "resourceSubscriptions": ["config://server-name"]
    },
    "_meta": { "io.modelcontextprotocol/protocolVersion": "2026-07-28",
               "io.modelcontextprotocol/clientCapabilities": {} }
  }
}
```

The three list-changed flags need no code from you: `mcp.NewServer` wires each registry's internal
`onChange` hook to the server's broker, so ordinary `Register`/`Unregister` calls emit them.

`resourceSubscriptions` is the one that does need code. It opts into
`notifications/resources/updated` for the named URIs, and nothing will ever fire unless your server
calls `resources.NotifyUpdated(uri)` (see above). The two are independent filters — a client
subscribed only to `resourcesListChanged` hears nothing from `NotifyUpdated`, and vice versa.

Every message on the stream carries `_meta.io.modelcontextprotocol/subscriptionId` set to the
listen request's own JSON-RPC id, so a client running several subscriptions over one stdio or UNIX
connection can demultiplex them. The first message is always
`notifications/subscriptions/acknowledged`, echoing the filter the server actually registered.
