# Transports and auth

All three transports implement the same `transport.Transport` interface:

```go
type Transport interface {
	Start(handler MessageHandler) error // non-blocking: launches its own goroutine(s) and returns
	Stop() error                        // blocks until shutdown is complete
}
```

`*mcp.Server` implements `transport.MessageHandler`, so every transport is started the same way:
`trans.Start(server)`. Pick the transport based on how the server will be reached; nothing about
tool/resource code changes between them.

## stdio — `transport.NewStdioTransport()`

```go
trans := transport.NewStdioTransport()
trans.Start(server)
```

Newline-delimited JSON-RPC over stdin/stdout. This is what a desktop MCP client (Claude Desktop,
etc.) launches as a subprocess. No config struct — nothing to set.

**Shutdown caveat:** `Stop()` cancels in-flight request contexts but can't interrupt a blocked
stdin read. The portable shutdown signal for a stdio server is the client closing stdin; if you
need a hard deadline, race `Stop()` against your own timeout.

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
