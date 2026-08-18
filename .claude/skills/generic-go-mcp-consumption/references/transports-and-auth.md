# Transports and auth

All three transports implement the same `transport.Transport` interface:

```go
type Transport interface {
	Start(handler MessageHandler) error // non-blocking: launches its own goroutine(s) and returns
	Stop() error                        // initiates shutdown; see each transport for what it waits on
}
```

`*mcp.Server` implements `transport.MessageHandler`, so every transport is started the same way:
`trans.Start(server)`. Pick the transport based on how the server will be reached; nothing about
tool/resource code changes between them.

A transport whose run can end on its own additionally implements `transport.DoneNotifier`:

```go
type DoneNotifier interface {
	Done() <-chan struct{} // closed when the transport's run has ended
}
```

Only `StdioTransport` does — stdin reaching EOF is a real ending. HTTP and UNIX transports end only
when you call `Stop()`, so they deliberately don't implement it.

## stdio — `transport.NewStdioTransport()`

```go
trans := transport.NewStdioTransport()
if err := trans.Start(server); err != nil {
	// ...
}

<-trans.Done()   // the client closed stdin; we're done
trans.Stop()     // cancel anything still in flight
if err := trans.Err(); err != nil {
	os.Exit(1)   // the read failed; this was not a clean disconnect
}
```

Newline-delimited JSON-RPC over stdin/stdout. This is what a desktop MCP client (Claude Desktop,
etc.) launches as a subprocess. No config struct — the only thing you can vary is which streams it
serves, via `NewStdioTransportWithStreams(in io.Reader, out io.Writer)`. Pass `nil` for either to
get the process default; pass a pair of `io.Pipe`s to drive the whole server from a test.

**Shutdown:** the portable shutdown signal for a stdio server is the client closing stdin, and
`Done()` is how you observe it — select on it alongside your `signal.Notify` channel so the process
exits when *either* fires:

```go
select {
case <-sigCh:
case <-trans.Done():
}
```

`Stop()` cancels in-flight request contexts and **returns immediately**; it does not wait for the
read loop, which is parked in a blocking read that nothing portable can interrupt. If you want to
wait for the loop to unwind, write `trans.Stop(); <-trans.Done()`, and if you want a hard deadline,
select `Done()` against your own `time.After`. `Err()` is valid once `Done()` is closed and tells
you whether the run ended on a clean EOF (nil) or a read failure (non-nil) — that's your exit code.

Note this changed in **v0.6.0**: `Stop()` used to block until the read loop exited, which in
practice meant it hung forever whenever it was called while stdin was still open.

## UNIX socket — `transport.NewUnixTransport(config)`

```go
trans := transport.NewUnixTransport(transport.UnixTransportConfig{
	SocketPath: "/var/run/my-mcp.sock",
	FileMode:   0660,
})
```

Same newline-delimited JSON-RPC framing as stdio, reused per the spec's guidance for custom
transports over a reliable byte stream. Only one connection is served at a time — a new connection
closes and replaces whatever the previous one was doing. `examples/go-mcp/main.go` uses this mode
to also demonstrate registering resources (a `/name` and `/pid` resource under a custom
`mcp+unix://` URI scheme, since a bare `/name` isn't a valid absolute URI).

## Streamable HTTP — `transport.NewHTTPTransport(config)`

```go
trans := transport.NewHTTPTransport(transport.HTTPTransportConfig{
	Host: "0.0.0.0",
	Port: 8080,
	// AllowedOrigins: []string{"*"}, // see below
})
```

A single POST-only `/mcp` endpoint. GET and DELETE return 405 — there's no session lifecycle to
manage in this revision. A response is a plain JSON body until a handler emits a notification, at
which point it upgrades to `text/event-stream` for the rest of that request. Today
`subscriptions/listen` is the only handler that does so, and it always does (its acknowledgment is
itself a notification); `notifications/progress` is parsed off incoming `_meta` but no tool-facing
API emits it yet, so a normal `tools/call` always answers with plain JSON.

**Every POST to `/mcp` must carry, in addition to `params._meta` (see the top-level SKILL.md):**

| Header | Required when | Must match |
|---|---|---|
| `MCP-Protocol-Version` | always | `params._meta["io.modelcontextprotocol/protocolVersion"]` in the body |
| `Mcp-Method` | always | the JSON-RPC `method` field |
| `Mcp-Name` | `tools/call`, `resources/read`, `prompts/get` | `params.name` (or `params.uri` for resources) |

A mismatch or missing header is `-32020 HeaderMismatch`, HTTP 400. This is transport-level
validation, separate from and in addition to the `-32602`/`-32022` body-level `_meta` validation
that applies on every transport.

**Origin allow-list (DNS rebinding protection):** a request with no `Origin` header (not from a
browser) is always allowed. With `AllowedOrigins` empty (the default), only
`http(s)://localhost` and `http(s)://127.0.0.1` (any port) are allowed — correct for a
loopback-bound server. Set `AllowedOrigins: []string{"*"}` to allow any origin, e.g. behind a
reverse proxy that already restricts access; anything else in the slice is an exact origin match.

## Optional: serving legacy (pre-2026-07-28) clients too — `compat` package

Everything above assumes a client that speaks 2026-07-28. If you need to serve clients still on
2025-11-25 or earlier (they can't get past `initialize` — that method doesn't exist on a bare
`*mcp.Server`), wrap the server in `compat.Overlay` instead of passing it to `trans.Start` directly.
Off unless you opt in; with it not wired in, behavior is unchanged from everything above.

```go
overlay := compat.New(server, compat.Config{
	ServerInfo: mcp.Implementation{Name: "my-mcp-server", Version: "1.0.0"},
})

httpCfg := transport.HTTPTransportConfig{
	Host: "0.0.0.0", Port: 8080,
	LegacySessions: overlay.LegacySessions(), // restores GET/DELETE + Mcp-Session-Id for legacy clients
}
trans := transport.NewHTTPTransport(httpCfg)
trans.Start(overlay) // pass overlay, not server
```

Also widen `mcp.ServerConfig.AdvertisedVersions` on the inner server so `server/discover`, the legacy
`initialize` diagnostic, and `-32022` all report the same version list:
`append([]string{mcp.ProtocolVersion}, compat.LegacyVersions...)`.

On stdio/UNIX, `trans.Start(overlay)` is all you need — no `LegacySessions` wiring, since those
transports have no GET/DELETE surface to gate. A tool's `req.ClientCapabilities`/`req.BindArguments`
work identically regardless of which era the caller is on; the overlay translates in both directions
around your tool code, which never needs to know. The one thing that doesn't translate: a tool that
calls `req.NeedInput` (MRTR) gets a visible `isError: true` refusal on the legacy side instead of the
`input_required` round-trip, since that revision has no way to answer one. See
[LEGACY-COMPAT.md](../../../../LEGACY-COMPAT.md) in the repo root for the full design (session
lifecycle, per-request era detection, result downgrading, known limitations).

## Optional GitHub OAuth (`auth` package, HTTP-only)

Auth is optional and only meaningful for the HTTP transport. Importing `auth` pulls in
`go.etcd.io/bbolt` for token storage — skip this package entirely for a stdio-only or
unauthenticated-HTTP server.

```go
authService, err := auth.NewAuthService(&config.AuthConfig{
	Issuer: "https://mcp.example.com",
	GitHub: config.GitHubConfig{ClientID: "...", ClientSecret: "..."},
	Storage: config.StorageConfig{DBPath: "/var/lib/my-mcp/oauth.db"},
	Allowlist: config.AllowlistConfig{Users: []string{"your-github-login"}},
})
if err != nil { /* handle */ }
defer authService.Close()

httpCfg := transport.HTTPTransportConfig{Host: "0.0.0.0", Port: 8080}
if authEnabled {
	httpCfg.AuthService = authService
}
trans := transport.NewHTTPTransport(httpCfg)
```

`transport.HTTPTransportConfig.AuthService` is typed as the small `transport.AuthProvider`
interface (`RegisterRoutes`, `RegisterAdminRoutes`, `Middleware`, `UserFromContext`) rather than
`*auth.AuthService` directly — this is what keeps `transport` itself free of the `auth`/`bbolt`
dependency chain; `*auth.AuthService` satisfies the interface, so passing it just works.

**The one trap:** only set `AuthService` when auth is actually enabled.

```go
// WRONG — authService may be a nil *auth.AuthService here; assigning it
// unconditionally stores a *typed* nil in the AuthProvider interface field,
// which is non-nil from the interface's point of view (Go's classic nil-interface
// gotcha) and the server will treat auth as enabled with a nil receiver.
httpCfg := transport.HTTPTransportConfig{AuthService: authService}

// RIGHT
httpCfg := transport.HTTPTransportConfig{}
if authService != nil {
	httpCfg.AuthService = authService
}
```

(As of v0.2.0, `NewHTTPTransport` also defends against this itself via a reflect-based typed-nil
check — but write the conditional assignment anyway; relying on the library's defense instead of
your own correct code is the wrong instinct even when it happens to work.)

When `AuthService` is set, `HTTPTransport` registers GitHub's OAuth routes (`RegisterRoutes`),
admin routes for managing static clients (`RegisterAdminRoutes`), and wraps `/mcp` in
`Middleware`, which validates the bearer token and attaches the authenticated user to the request
context. Inside a tool handler, the `ctx` passed to your `ToolFunction` carries that same context,
so `auth.GetUserFromContext(ctx)` returns `*auth.User{ID, GitHubLogin, Email, ...}` (nil if
unauthenticated). Separately, if you want a signed MRTR `requestState` (see
`mrtr-and-resources.md`) bound to the caller so one user's confirmation can't be replayed by
another, set `mcp.ServerConfig.PrincipalFromContext` to a `func(ctx context.Context) string` that
extracts an ID from the same context — e.g. `func(ctx context.Context) string { u :=
auth.GetUserFromContext(ctx); if u == nil { return "" }; return u.ID }`.

With auth on, also set `mcp.ServerConfig.DefaultCacheScope = "private"` if the catalog or resource
content varies per user. It defaults to `"public"`, which tells intermediaries one user's
`tools/list` is safe to reuse for another.

See `config-oauth-example.yaml` in this repo for a full YAML config including allowlists
(by user, GitHub org, or org/team) and static OAuth clients.

## Deploying HTTP: set `RequestStateKey`

`mcp.ServerConfig.RequestStateKey` signs the MRTR `requestState` blob. Leave it empty and
`NewServer` generates a random 32-byte key at startup — fine for stdio, where the process and the
client live and die together, and quietly broken anywhere else:

- **After a restart**, any `requestState` a client is holding fails verification. The user sees
  their confirmation rejected with no indication why.
- **Behind a load balancer with more than one replica**, an MRTR retry only succeeds if it happens
  to land on the pod that issued it — so confirm-before-act tools fail intermittently, which is the
  worst version of this bug to debug.

```go
server := mcp.NewServer(registry, resources, &mcp.ServerConfig{
	Name:            "my-mcp-server",
	Version:         "1.0.0",
	RequestStateKey: []byte(os.Getenv("MCP_REQUEST_STATE_KEY")), // stable across restarts and replicas
})
```

Any stable secret ≥32 bytes works; treat it like a signing key (mounted secret or env var, not
checked in, rotatable). If your server registers no MRTR tools, this doesn't apply.
