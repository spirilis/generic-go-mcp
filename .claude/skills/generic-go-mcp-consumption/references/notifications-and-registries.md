# Runtime catalog mutation and change notifications

Both registries are safe for concurrent use and can be mutated at any time, including long after the
server has started. Every mutation that actually changes the catalog announces itself to clients
listening on a `subscriptions/listen` stream — which is, in this revision, the *entire* server→client
notification channel.

## The registry API

| Method | `ToolRegistry` | `ResourceRegistry` | Notes |
|---|---|---|---|
| `Register(def, fn)` | by `Tool.Name` | by `Resource.URI` | Replaces an existing entry; see below |
| `Unregister(key) bool` | name | URI | `false` if nothing was there to remove |
| `List()` | `[]Tool` | `[]Resource` | Registration order, stable across calls |
| `Get(key)` | `(Tool, bool)` | `(Resource, bool)` | Metadata lookup |
| `HasTools()` / `HasResources()` | ✓ | ✓ | Drives the advertised capability |
| `Read(ctx, uri)` | — | `(ResourceContentResult, error)` | Runs the `ResourceFunction` |
| `NotifyUpdated(uri) bool` | — | ✓ | Announce a content change; see below |

Registering the *first* tool or resource is what makes the server advertise that capability from
`server/discover`, and unregistering the *last* one withdraws it — capabilities are derived from what
is actually registered, not declared up front (`mcp/server.go`, `Server.capabilities`).

## Removing and replacing entries

`Unregister` returns whether there was anything to remove, and fires `list_changed` only when it
actually removed something:

```go
resources.Unregister("config://server-name") // true
resources.Unregister("config://server-name") // false — already gone, notifies nobody
registry.Unregister("my_tool")               // same contract on ToolRegistry
```

Re-registering a URI or name that already exists **replaces** the entry and moves it to the end of
the list rather than adding a duplicate, so `resources/list` can never disagree with what
`resources/read` will actually run.

Two consequences of mutating a live catalog:

- **A tool can be unregistered between a caller's `tools/list` and its `tools/call`**, or even
  between the handler's internal lookup and the call itself. That is not a crash: `ToolRegistry.call`
  reports the same "Unknown tool" error a never-registered name produces (`mcp/tools.go`).
- **List cursors are opaque offsets**, so unregistering between a client's page fetches shifts later
  entries and can cause one to be skipped. That is what `list_changed` is for — a client that sees it
  should restart pagination.

The protocol has no "unregister" method — this is purely server-side. Don't confuse it with the
`resources/subscribe`/`resources/unsubscribe` methods, which 2026-07-28 deleted outright; those were
about a *client* subscribing to updates on a resource, and their replacement is
`subscriptions/listen`.

## Announcing that a resource's content changed

`list_changed` covers the *catalog*: something was added, replaced, or removed. It says nothing about
the bytes behind a URI that has been registered all along. That is a separate notification,
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
  client subscribed to; this library does not, because a `ResourceRegistry` is a flat map with no URI
  hierarchy to derive one from. A client hears about the URIs it literally named.
- **Re-registering a URI fires `resources/updated` too**, on top of `list_changed`, because
  `Register` replaces the `ResourceFunction` as well as the metadata and Go can't compare func
  values — the registry can't tell a `Description` fix from a content swap, so it announces both. A
  first-time `Register` of a URI fires `list_changed` only.

## subscriptions/listen

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

| Filter field | Delivers | Triggered by |
|---|---|---|
| `toolsListChanged` | `notifications/tools/list_changed` | automatic, on any `ToolRegistry` mutation |
| `resourcesListChanged` | `notifications/resources/list_changed` | automatic, on any `ResourceRegistry` mutation |
| `resourceSubscriptions: [uri…]` | `notifications/resources/updated`, params `{"uri": …}` | **your code**, via `resources.NotifyUpdated(uri)` |
| `promptsListChanged` | — | accepted, never fires: prompts are not implemented |

Every field is optional, and the filters are **independent** — a client subscribed only to
`resourcesListChanged` hears nothing from `NotifyUpdated`, and vice versa.

The two list-changed flags need no code from you: `mcp.NewServer` wires each registry's internal
`onChange` hook to the server's broker, so ordinary `Register`/`Unregister` calls emit them.
`resourceSubscriptions` is the one that needs code, for the reason above.

Every message on the stream carries `_meta.io.modelcontextprotocol/subscriptionId` set to the listen
request's own JSON-RPC id, so a client running several subscriptions over one stdio or UNIX
connection can demultiplex them. The first message is always
`notifications/subscriptions/acknowledged`, echoing the filter the server actually registered.

## Delivery guarantees — read this before designing around notifications

**Notifications are hints, not an event log.** Each subscriber gets a 16-deep buffer, and once it is
full the broker drops rather than blocks (`Broker.broadcast`, `mcp/subscriptions.go`): a registry
mutation, or a `NotifyUpdated` on a hot path, must never stall behind a slow client. A burst can
therefore reach a client as fewer notifications than you sent, or as none at all.

Design both sides around that:

- Treat every notification as *"something may have changed, go re-read"* — never as a count of
  changes, and never as a stream you can reconstruct state from.
- There is no `Last-Event-ID` resumability in this revision. A reconnecting client gets no backfill;
  it should re-`list` and re-`read` whatever it cares about.
- Because `ReadTTLMs` defaults to `0` (always refetch), `resources/updated` is not load-bearing for
  cache invalidation. It is a *push* signal that lets a client re-read promptly instead of polling.

If you need guaranteed delivery of every change, notifications are the wrong layer — put a sequence
number or a version field in the resource's own content and let the client reconcile.

## Lifecycle and cancellation

The listen request stays open until the client ends it. How depends on the transport:

| Transport | Client ends the subscription by | Notes |
|---|---|---|
| Streamable HTTP | closing the response stream | A listen request always becomes `text/event-stream`, since its acknowledgment is itself a notification. A `:` keep-alive comment every 15s holds it open through idle proxies. |
| stdio / UNIX socket | sending `notifications/cancelled` with `params.requestId` set to the listen request's `id` | Matched on the **raw JSON id**, so `4` and `"4"` are different requests. The connection multiplexes, so a subscription never blocks other in-flight requests. |

`notifications/cancelled` is handled by the stdio/UNIX framing layer (`transport/stream.go`), not by
`mcp` — it is not wired up on HTTP, where closing the stream is the cancellation signal.

Either way the server writes **no final JSON-RPC response** for the listen request. The spec's
"graceful closure" (an empty result before closing) is a SHOULD and is not implemented; every
termination is treated as the abrupt-disconnect case. A client must not block waiting for a response
to its listen request.
