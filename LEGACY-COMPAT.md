# Legacy MCP Compatibility Overlay

## Overview

`mcp.Server` implements MCP protocol version **2026-07-28 and nothing else** — that revision made MCP
stateless, removing the `initialize` handshake and `Mcp-Session-Id` sessions every earlier revision
relied on. Upstream gave the connection-scoped revisions (2025-11-25 and earlier) a roughly 12-month
migration window. Until that closes, a client stuck on one of those revisions cannot get past its
*first message* against a bare `mcp.Server`: `initialize` is a method that no longer exists, so there
is no degraded mode, just a client that cannot connect at all.

The `compat` package is the answer for a server with a mixed or client-you-don't-control population:
it wraps an `*mcp.Server` (or any `transport.MessageHandler`) and serves both eras at once, selected
per request, behind a switch you turn off once your clients have all moved.

**The property this design protects deliberately:** with the overlay simply not wired in (the
default), the server underneath is byte-for-byte the modern-only server described in
[GOLANG-MCP-CONVERT-TO-2026-07-28.md](GOLANG-MCP-CONVERT-TO-2026-07-28.md) and
[README.md](README.md). One tool registry, one set of handlers, one auth path — two protocol
envelopes. Nothing below the overlay is aware there are two eras.

## Enabling it

### As a library

```go
inner := mcp.NewServer(registry, resourceRegistry, &mcp.ServerConfig{
    Name:    "my-server",
    Version: "1.0.0",
    // Feed server/discover, the legacy initialize diagnostic, and -32022 from one list,
    // so a client is never told three different stories about what this server speaks.
    AdvertisedVersions: append([]string{mcp.ProtocolVersion}, compat.LegacyVersions...),
})

overlay := compat.New(inner, compat.Config{
    SessionTTL: 30 * time.Minute, // zero uses this same 30-minute default
    ServerInfo: mcp.Implementation{Name: "my-server", Version: "1.0.0"},
})

trans := transport.NewHTTPTransport(transport.HTTPTransportConfig{
    // ... Host, Port, AllowedOrigins as usual ...
    LegacySessions: overlay.LegacySessions(), // nil (the default) would mean compat off
})
trans.Start(overlay) // pass the overlay, not inner, as the MessageHandler
```

`overlay.LegacySessions()` returns a genuinely nil `transport.LegacySessions` if `overlay` itself is
nil — the typed-nil-in-an-interface trap `NewHTTPTransport` already guards against defensively, same
as `HTTPTransportConfig.AuthService`. Only build an `Overlay` (and only assign `LegacySessions`) when
compatibility is actually meant to be on; don't construct one unconditionally and rely on that guard.

On stdio/UNIX, just pass `overlay` to `trans.Start(overlay)` — there's no `LegacySessions` wiring
needed, since those transports have no GET/DELETE surface to gate.

### Via `examples/go-mcp`

```yaml
server:
  mode: "http"
  legacy_compat:
    enabled: true      # default: false
    session_ttl: "30m" # default: 30m
```

```bash
./go-mcp -config config.yaml -legacy-compat        # force on regardless of the file
./go-mcp -config config.yaml -legacy-compat=false   # force off regardless of the file
```

The CLI flag only overrides the config file when actually passed (tracked via `flag.Visit`, not the
flag's own zero value) — omit it and the config file's `legacy_compat.enabled` decides. The server
logs its posture and served revisions at startup:

```
level=INFO msg="MCP protocol support" modern=2026-07-28 legacy_compat=true advertised_versions="[2026-07-28 2025-11-25 2025-06-18 2025-03-26 2024-11-05]"
```

## How a request is classified

The only unambiguous marker of the modern wire format is `params._meta["io.modelcontextprotocol/protocolVersion"]`.
The connection-scoped revisions negotiate a version once, at the handshake, and never name one in
`_meta` — so its presence means modern, its absence means legacy (`compat.Claims` /
`transport.DeclaresModernProtocol`). Do not infer the era from the method name, the params shape, or
HTTP headers instead of this. `transport/http.go` uses the exact same predicate before the protocol
layer is even entered (to decide whether to enforce modern headers and validate a session id) — two
independent implementations of "is this legacy?" that could disagree would be a bug factory.

**The trade this accepts:** with compat on, a *modern* client that omits its mandatory `_meta` gets a
legacy-shaped response instead of `-32602`. That request is malformed under 2026-07-28 and is
genuinely indistinguishable from a well-formed legacy one — no predicate can separate them. Turning
compat off restores the strict answer.

## What the overlay implements

| Legacy method | Behavior |
| --- | --- |
| `initialize` | Negotiates a version (echoes a known one, else falls back to the newest — `2025-11-25`), mints a session, and derives capabilities/instructions/identity from the inner server's own `server/discover` rather than duplicating that logic. `resources.subscribe` is always reported `false`. |
| `initialize` naming `2026-07-28` | The same `-32601` diagnostic a bare `mcp.Server` gives a modern client calling `initialize` — pointing at `server/discover` — and mints **no** session. Never quietly downgrades a modern client into a legacy one. |
| `notifications/initialized` | Swallowed; extends the session's idle TTL. No response, as required for a notification. |
| `ping` | `{}`. Mandatory in these revisions, unlike the modern stack (which doesn't route it at all). |
| `tools/list`, `tools/call`, `resources/list`, `resources/read`, `resources/templates/list`, `completion/complete`, `skills/list`, `skills/get`, `resources/directory/read` | Translated to the modern shape using the calling session's declared capabilities, forwarded to the inner handler unchanged, and downgraded back. |
| `skills/list` / `skills/get` / `resources/directory/read` specifics | The skills extension (SEP-2640) defines an identical params object regardless of era, so forwarding is all that is needed. `capabilities.extensions` reaches the legacy handshake too, because `buildInitializeResult` rebuilds capabilities through the typed `mcp.ServerCapabilities`, which has a field for it — a legacy client that understands the extension can use it, one that does not ignores an unknown key. A server with no `SkillRegistry` answers all three with the inner stack's `-32601`. See known limitation 7 below. |
| `resources/templates/list` / `completion/complete` specifics | Both predate 2026-07-28 and share an identical params object across eras, so forwarding is all that is needed. A server with no `CompletionProvider` registered answers `completion/complete` with the inner stack's `-32601`, which passes straight through — the same "unsupported" signal a legacy client probes for. |
| everything else (including `resources/subscribe`/`unsubscribe`) | `-32601` — the correct answer for a capability the handshake never declared. |

A call with no live session — the handshake was skipped, or the session id was dropped or expired —
is still served, with no capabilities declared (the safe reading of "nothing was negotiated"), not
refused.

### Result downgrading

Legacy result shapes are the modern ones minus the envelope: `tools/list` becomes
`{tools, nextCursor?}`, `tools/call` becomes `{content, isError?}`, `resources/read` becomes
`{contents}`, `resources/templates/list` becomes `{resourceTemplates, nextCursor?}`, `skills/list`
becomes `{skills, nextCursor?}`, and `skills/get` becomes `{skill}`. `resultType`,
`ttlMs`, `cacheScope`, and `_meta` are stripped rather than translated — they're inventions of the
2026-07-28 envelope with no legacy equivalent.

Downgrading is generic key-stripping on the raw JSON result, not a per-method translation, which is
why a new result type needs no new downgrade code. It strips only the four **top-level** keys and
never descends, which is what lets a skill's per-entry `frontmatter` and per-file `digest` cross the
boundary untouched.

**MRTR (`input_required`) cannot be downgraded.** A client on a revision without it has no way to
answer and retry — that mechanism requires a bidirectional request channel these transports no longer
have, so it is never emulated. Instead the overlay substitutes a visible tool-call error naming the
modern revision:

```json
{"content":[{"type":"text","text":"This tool needs additional input before it can run, which requires MCP protocol revision 2026-07-28 (Multi Round-Trip Requests). The negotiated revision has no equivalent."}],"isError":true}
```

leaking no `inputRequests` or `requestState`. `examples/tools/confirm.go` (`confirm_delete`) is the
reference MRTR tool and exercises exactly this path when called by a legacy client.

## Sessions

The connection-scoped revisions declare client capabilities **once**, at `initialize`, and every
later request trusts what was agreed then — unlike the modern stack, which has no session at all. So
the overlay keeps an in-memory, per-process session store:

- **Sessions are in-memory and per-process.** A restart invalidates every session; the client's next
  request 404s and it re-handshakes. That's the designed recovery path, not a bug — but it's also why
  you should run a single replica unless you externalize the store.
- **No janitor goroutine.** Expired sessions are swept opportunistically on create, not by a
  background ticker — nothing leaks in a process that enables compat but never sees a legacy client.
- **Capped** at 256 sessions, evicting least-recently-used past that.
- **stdio/UNIX gets one implicit session**, under a reserved key, since the connection *is* the
  process there — no header layer to carry an id. Re-handshaking on either transport is a legitimate
  reset: the old session is terminated and replaced, not treated as an error.

## HTTP binding

See [HTTP-TRANSPORT.md](HTTP-TRANSPORT.md#legacy-compatibility-overlay) for the full GET/DELETE/
`Mcp-Session-Id`/CORS table. In short: `POST /mcp` serves both eras (selected per request); `GET /mcp`
opens a standalone SSE stream scoped to a live session (`404` if the session is unknown, closes when
the session ends *or* the client disconnects — whichever comes first); `DELETE /mcp` tears a session
down (`204`, then a `404` on reuse); and `Mcp-Session-Id` is exposed via
`Access-Control-Expose-Headers` so a browser client can actually read the id an `initialize` handshake
minted.

## Known limitations

1. **A modern client omitting its mandatory `_meta` gets a legacy-shaped response, not `-32602`**,
   whenever compat is on — see "How a request is classified" above. This is inherent, not a bug to
   fix; turning compat off restores the strict answer.
2. **The 2024-11-05 HTTP+SSE binding is not restored.** That revision's separate `GET /sse` endpoint,
   `event: endpoint` frame, and `Last-Event-ID` resumability are out of scope — only the
   Streamable-HTTP-era surface on `/mcp` comes back. A 2024-11-05 client works fine over stdio/UNIX,
   but not over HTTP.
3. **MRTR tools are unusable for legacy clients** — visibly, by design (see above).
4. **Modern content types** (`resource_link`, audio) **can reach a client on a revision that predates
   them.** Tool authors targeting a mixed client population should keep that in mind.
5. Sessions don't survive a restart (see "Sessions" above).
6. **Resource-not-found reaches a legacy client as `-32602`, not the legacy `-32002`.** Earlier
   revisions used `-32002` for this, but 2026-07-28 retired that code and this library never emits
   it anywhere (`transport/transport.go`). The overlay does not translate error codes at all — a
   JSON-RPC error object carries no modern-only envelope fields to strip — so the modern code passes
   through unchanged. Clients on those revisions are expected to accept `-32602` as well; this
   applies to reads of both concrete resources and resource-template members.

7. **The skills extension is forwarded, but it is experimental on both sides of the boundary.**
   SEP-2640 is an unmerged proposal, the `extensions` capability mechanism itself postdates every
   revision this overlay serves, and no public client consumes either. Forwarding it costs nothing —
   a legacy client that does not understand the key ignores it — but do not read "it works over the
   overlay" as "a 2025-11-25 client out there will use it". A server that never sets
   `ServerConfig.Skills` exposes none of this in either era.

**Nothing about the tools changes between eras.** Same registry, same schemas, same data, same auth.
Only the envelope differs — "legacy mode" is not a degraded mode.

## Sunsetting

Give operators a one-line way to turn this off (`-legacy-compat=false`, or flipping
`legacy_compat.enabled` in config) once your legacy clients have all moved, and confirm doing so
restores the modern-only posture exactly — that's what `go test ./...` with compat simply never wired
in already proves for every pre-existing test in this repository.

## Testing

- `compat/compat_test.go` — the protocol-level conformance suite: handshake negotiation, session
  lifecycle, capability propagation, result downgrading, MRTR refusal, advertised-version
  consistency, and the resource-template surface (`resources/templates/list` and template reads
  downgraded correctly, `completion/complete` with and without a provider, the `-32602` note above,
  and a templates-only registry still declaring `resources` at handshake), plus the skills surface
  (`skills/list`/`skills/get`/`resources/directory/read` forwarded and downgraded with frontmatter
  and digests intact, `capabilities.extensions` surviving the handshake, and all three answering
  `-32601` when the inner server has no skill registry).
- `transport/http_compat_test.go` — the HTTP binding: header-validation exemptions, unknown-session
  handling, the standalone GET stream's two exit conditions, DELETE teardown, and CORS.
- `mcp/conformance_test.go` and the pre-existing `transport/http_test.go` passing **unchanged** is
  the evidence that the modern-only posture is untouched when this overlay isn't wired in.
