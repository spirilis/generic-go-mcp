# generic-go-mcp

A reusable Go framework for building [Model Context Protocol](https://spec.modelcontextprotocol.io/) (MCP) servers over stdio, UNIX domain sockets, or Streamable HTTP.

## Overview

**generic-go-mcp** is a library that abstracts away the complexity of implementing MCP servers. It handles JSON-RPC 2.0 message parsing, transport layer management, authentication, and configuration—allowing you to focus on building powerful tools for Claude and other MCP clients.

### What is MCP?

The Model Context Protocol enables AI assistants like Claude to interact with external tools and data sources. This library makes it easy to create custom MCP servers that expose your own functionality to AI models.

## ⚠️ By default, the client MUST implement MCP protocol version 2026-07-28

`mcp.Server` implements MCP protocol version **2026-07-28 and nothing else**. This is a hard
cutover, not a preference: `mcp.SupportedVersions` is a single-element list, and `mcp.Server` on its
own has no dual-stack mode, no compatibility shim, and no negotiation down to an earlier revision. A
client that speaks 2025-11-25 or earlier **cannot talk to a bare `mcp.Server` at all** — not in
degraded form, not for `tools/list`, not for anything.

If every client you serve can move to 2026-07-28 together, this is the constraint you accept, and
you should stop reading this section. If you have a mixed or client-you-don't-control population
still on 2025-11-25 or earlier, wrap your server in the optional `compat` package instead of reaching
for a separate library or a translating proxy — see
[LEGACY-COMPAT.md](LEGACY-COMPAT.md). It is off by default (a plain `mcp.Server` is unaffected until
you opt in) and additive: with the switch off, the server underneath is byte-for-byte what this
section describes.

### What a conforming client must do

**1. Never send `initialize`.** The 2026-07-28 revision removed the handshake. An `initialize`
request is answered with JSON-RPC `-32601` (Method not found) carrying a diagnostic that names the
supported versions — it never yields a session:

```json
{"jsonrpc":"2.0","id":1,"error":{
  "code":-32601,
  "message":"Method not found: \"initialize\" is not implemented. This server speaks MCP protocol version 2026-07-28, which has no initialize handshake — every request carries its own protocol version and capabilities.",
  "data":{"supported":["2026-07-28"]}}}
```

That message exists purely so a legacy-only client has something intelligible to show its user.

**2. Put `_meta` on every single request.** There is no session to remember who you are, so each
request re-states it. Two fields are **mandatory on every request** (`server/discover`,
`tools/list`, `tools/call`, `resources/list`, `resources/templates/list`, `resources/read`,
`completion/complete`, `subscriptions/listen`):

| `params._meta` key | Required | Notes |
|---|---|---|
| `io.modelcontextprotocol/protocolVersion` | **yes** | Must be exactly `"2026-07-28"` |
| `io.modelcontextprotocol/clientCapabilities` | **yes** | May be `{}`, but the key must be present |
| `io.modelcontextprotocol/clientInfo` | no | `{name, version}`; display/logging only |
| `io.modelcontextprotocol/logLevel` | no | Per-request log level hint |
| `progressToken` | no | Enables progress notifications for this call |

Omitting either mandatory field is a malformed request: `-32602`, `missing required _meta field
"io.modelcontextprotocol/protocolVersion"`. This is the single most common first-contact failure.

```json
{
  "jsonrpc": "2.0", "id": 1, "method": "tools/call",
  "params": {
    "name": "current_time",
    "arguments": {},
    "_meta": {
      "io.modelcontextprotocol/protocolVersion": "2026-07-28",
      "io.modelcontextprotocol/clientCapabilities": {}
    }
  }
}
```

**3. Declare capabilities per request, honestly.** The server may only ask for what *this* request
declared. A tool that needs mid-call user input returns `-32021` (missing required client
capability) if `clientCapabilities.elicitation` was absent from the request that triggered it.

**4. Discover via `server/discover`, not a handshake.** It returns `supportedVersions`,
`capabilities`, optional `instructions`, and — like every result from this server — the server's
identity at `result._meta["io.modelcontextprotocol/serverInfo"]`. That is everything `initialize`
used to carry, and `server/discover` is itself an ordinary request that requires `_meta`.

**5. Handle Multi Round-Trip Requests.** Servers can no longer originate JSON-RPC requests, so
elicitation/sampling-style interactions come back as a result with `resultType: "input_required"`,
an `inputRequests` map, and an opaque signed `requestState`. The client answers by re-issuing the
same call (new JSON-RPC `id`, same `name`/`arguments`) with `inputResponses` and the
`requestState` echoed back **byte-for-byte**. See `examples/tools/confirm.go`.

**6. Over Streamable HTTP, additionally:**
- `POST /mcp` only. `GET` and `DELETE` (the old SSE stream and session teardown) return **405**.
- No `Mcp-Session-Id` — sessions do not exist.
- Send `MCP-Protocol-Version` and `Mcp-Method` headers on every request, plus `Mcp-Name` for
  `tools/call`, `resources/read`, and `prompts/get`. Each is validated against the JSON body;
  a missing or mismatched header is **HTTP 400** with JSON-RPC `-32020` (header mismatch).
- Browser-issued requests must carry an allowed `Origin` (defaults to localhost only; see
  `HTTPConfig.AllowedOrigins`), or the request is rejected with **403**.
- A response is a plain JSON object unless the handler emits a notification, at which point the
  same response upgrades to `text/event-stream` for that request only.

### Version-specific error codes

| Code | Meaning |
|---|---|
| `-32020` | Streamable HTTP header missing or disagreeing with the body |
| `-32021` | Request needs a client capability that was not declared |
| `-32022` | `protocolVersion` is not one this server implements (`data.supported` lists what is) |
| `-32601` on `initialize` | Legacy handshake attempted against a stateless server |

### Not implemented in this revision

Roots, Sampling, and MCP's own Logging utility are deprecated upstream in 2026-07-28 and are not
implemented here. Resource templates and `completion/complete` **are** implemented (see
[Resource Templates](#resource-templates)), but completion is only offered for resource template
variables — `ref/prompt` is rejected, since prompts are not implemented yet (the HTTP layer already reserves `prompts/get`
for the `Mcp-Name` header rule, and a `subscriptions/listen` filter may set `promptsListChanged`,
but nothing will ever fire it).

The `subscriptions/listen` **graceful-closure response** is also unimplemented. The spec SHOULDs
that a server-initiated teardown reply to the still-open listen request with an empty
`{"resultType":"complete"}` before closing; this library always treats termination as the
abrupt-disconnect case and writes no final response. A client must not wait for one.

See [GOLANG-MCP-CONVERT-TO-2026-07-28.md](GOLANG-MCP-CONVERT-TO-2026-07-28.md) for the full design
rationale, the hard-cutover decisions, and a worked wire-to-Go-types example.

## Features

- **Three Transports** - stdio (desktop integration), UNIX domain socket (local IPC), and
  Streamable HTTP (web services), behind one `Transport` interface
- **OAuth Authentication** - Built-in GitHub OAuth 2.0 support with PKCE for HTTP mode
- **YAML Configuration** - File-based config with defaults; OAuth credentials may instead be read
  from mounted secret files (Docker/Kubernetes)
- **Structured Logging** - Multi-level logging (trace/debug/info/warn/error) with JSON and text formats
- **Simple Tool API** - Register tools with JSON schema definitions and type-safe handlers
- **Live Change Notifications** - Runtime-mutable tool and resource catalogs; clients subscribe once
  via `subscriptions/listen` and receive `list_changed` and per-resource `updated` notifications
- **Parameterized Resources** - RFC 6570 resource templates (`resources/templates/list`) for
  unbounded keyspaces no `resources/list` could enumerate, with optional `completion/complete`
  to help clients narrow a template variable
- **Skills (experimental)** - Opt-in support for the `io.modelcontextprotocol/skills` extension
  (SEP-2640): `skills/list`, `skills/get`, and `resources/directory/read`, with a loader that turns
  an `embed.FS` of skill directories into a digest-correct catalog. Off unless you configure it
- **Production Ready** - BoltDB token storage, HMAC-signed MRTR request state, graceful shutdown
- **Small Dependency Surface** - `transport` imports nothing beyond the Go standard library; `mcp`
  adds only `gopkg.in/yaml.v3` (for parsing skill frontmatter). `auth` (BoltDB) is opt-in, pulled in
  only if you import it

## Using this library from another project

```bash
go get github.com/spirilis/generic-go-mcp@v0.2.0
```

If you're using an AI coding agent to build on top of this library, point it at
[`.claude/skills/generic-go-mcp-consumption/SKILL.md`](.claude/skills/generic-go-mcp-consumption/SKILL.md)
in this repo — it covers tool/resource authoring, transport and auth setup, and the Multi
Round-Trip Requests pattern, with every example checked against the real API.

## Quick Start

```go
package main

import (
    "context"
    "encoding/json"
    "os"
    "time"

    "github.com/spirilis/generic-go-mcp/config"
    "github.com/spirilis/generic-go-mcp/logging"
    "github.com/spirilis/generic-go-mcp/mcp"
    "github.com/spirilis/generic-go-mcp/transport"
)

func main() {
    // Load configuration
    cfg, _ := config.Load("config.yaml")
    logging.Initialize(cfg.Logging.Level, cfg.Logging.Format, os.Stderr)

    // Create a tool registry
    registry := mcp.NewToolRegistry()

    // Define a simple tool
    timeTool := mcp.Tool{
        Name:        "current_time",
        Description: "Returns the current UTC time",
        InputSchema: json.RawMessage(`{"type": "object", "additionalProperties": false}`),
    }

    // Register the tool with its implementation. ToolFunction takes a context (for
    // cancellation) and a *ToolRequest (arguments, parsed _meta, and — for tools that
    // need mid-call user input — Multi Round-Trip Requests helpers).
    registry.Register(timeTool, func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
        return &mcp.ToolCallResult{
            Content: []mcp.Content{mcp.Text(time.Now().UTC().String())},
        }, nil
    })

    // A resource registry is required even if you register no resources.
    resources := mcp.NewResourceRegistry()

    // Create the MCP server
    server := mcp.NewServer(registry, resources, &mcp.ServerConfig{
        Name:    "my-mcp-server",
        Version: "1.0.0",
    })

    // Start the stdio transport, then wait for the client to close stdin
    trans := transport.NewStdioTransport()
    if err := trans.Start(server); err != nil {
        logging.Error("starting transport", "error", err)
        os.Exit(1)
    }
    <-trans.Done()
    trans.Stop()
}
```

Then talk to it — note the mandatory `_meta`, without which every request is rejected:

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{
  "io.modelcontextprotocol/protocolVersion":"2026-07-28",
  "io.modelcontextprotocol/clientCapabilities":{}}}}' | ./my-mcp-server
```

## Project Structure

```
generic-go-mcp/
├── config/               # Configuration loading (YAML, env vars, secrets)
├── logging/              # Structured logging with multiple levels
├── auth/                 # OAuth 2.0 authentication (GitHub)
├── transport/            # Transport abstractions (stdio, UNIX socket, Streamable HTTP)
├── mcp/                  # MCP protocol implementation (JSON-RPC 2.0)
├── compat/               # Optional legacy (2025-11-25 and earlier) compatibility overlay
├── examples/
│   ├── go-mcp/           # Complete example server application
│   │   └── skills/       # Embedded demonstration skill (SEP-2640, UNIX-socket mode)
│   └── tools/            # Reference tools (date, fortune, confirm_delete/MRTR)
├── CLAUDE-new-project-harness.md  # Comprehensive getting started guide
├── CLAUDE.md             # Architecture and design patterns
├── GOLANG-MCP-CONVERT-TO-2026-07-28.md  # Protocol revision design & rationale
├── HTTP-TRANSPORT.md     # HTTP transport documentation
├── LEGACY-COMPAT.md      # Legacy protocol compatibility overlay documentation
└── LOGGING.md            # Logging system documentation
```

All packages are public and designed to be imported by your projects.

## Getting Started

For a comprehensive guide on building your own MCP server, see **[CLAUDE-new-project-harness.md](CLAUDE-new-project-harness.md)**.

This guide covers:
- Setting up a new project with the library
- Creating tools with and without arguments
- Main application template with stdio/HTTP mode switching
- Configuration examples (stdio, HTTP, auth-enabled)
- Build commands including cross-compilation and Docker
- Testing with Claude Code and HTTP clients
- Debugging tips and common issues

## Configuration

`config.Load(path)` reads a YAML file and applies defaults (`mode: stdio`, HTTP `0.0.0.0:8080`,
UNIX file mode `0660`, logging `info`/`text`). The example application layers CLI flags on top of
the loaded file; the `config` package itself does not read environment variables.

### Example Configuration (stdio mode)

```yaml
server:
  mode: "stdio"

logging:
  level: "info"
  format: "text"
```

### Example Configuration (UNIX socket mode)

```yaml
server:
  mode: "unix"
  unix:
    socket_path: /tmp/go-mcp.sock
    name: go-mcp-example-endpoint
    file_mode: 0660
```

### Example Configuration (HTTP mode with auth)

```yaml
server:
  mode: "http"
  http:
    host: "0.0.0.0"
    port: 8080
    # Origin allow-list for DNS-rebinding protection.
    # Omit for localhost-only; use ["*"] behind a trusted reverse proxy.
    allowed_origins: ["https://app.example.com"]

auth:
  enabled: true
  issuer: "https://mcp.example.com"   # must match the server's public URL
  github:
    clientId: "your-github-oauth-app-id"
    clientSecret: "your-github-oauth-secret"
    # Or read from mounted secret files instead:
    # clientIdFile: "/run/secrets/github_client_id"
    # clientSecretFile: "/run/secrets/github_client_secret"
  storage:
    dbPath: "/var/lib/go-mcp/oauth.db"
  allowlist:
    users: ["my-github-user"]
    orgs: ["my-company"]
    teams:
      - org: "my-company"
        team: "platform-team"   # team slug, not display name
  # Optional: only these hosts may appear in a self-registered client's redirect_uris.
  # registration:
  #   allowedRedirectHosts: ["claude.ai", "localhost", "127.0.0.1"]

logging:
  level: "info"
  format: "json"
```

Note the key names: they are camelCase under `auth:` and snake_case under `server:`, matching the
struct tags in `config/config.go`. There is no `redirect_url` key: the callback URL you register
with GitHub is always `<issuer>/callback`. See
[config-oauth-example.yaml](config-oauth-example.yaml) for the fully annotated version, including
pre-registered static clients.

#### The consent screen

Dynamic client registration at `<issuer>/register` is open, as RFC 7591 intends, and GitHub skips its
own approval prompt for an OAuth app the user has already authorized. Together those would let
anyone register a client whose `redirect_uri` is their own host and harvest a working authorization
code from an allowlisted user who merely follows a link — the confused-deputy problem the MCP
security guidance describes. So after GitHub sign-in and the allowlist check, the first time a user
authorizes a client the operator did not list under `auth.clients`, the server shows a consent screen
on the issuer's own origin. It names the client, the signed-in GitHub login, and the host the
browser will be sent to. The approval is remembered per user and client, so it costs one click per
new client, not one per sign-in.

The screen cannot be framed, and it is bound to the browser that started the sign-in by a one-shot
`SameSite=Strict` cookie (`__Host-mcp_consent` on an https issuer). Clients listed in `auth.clients`
skip it, since the operator vetted their redirect URIs. Clients created through `/admin/clients` do
not skip it.

For a deployment whose clients are known in advance, `auth.registration.allowedRedirectHosts`
additionally limits the hosts a self-registered client may name. Entries are bare hostnames,
compared case-insensitively with any port and path accepted, and a malformed entry fails startup.
Regardless of that setting, `/register` always rejects non-absolute URIs, fragments, and the
`javascript:`, `data:`, `vbscript:`, `file:`, `blob:` and `about:` schemes. Native-app custom schemes
are allowed (RFC 8252).

## Documentation

- **[CLAUDE-new-project-harness.md](CLAUDE-new-project-harness.md)** - Complete guide to building MCP servers with this library
- **[CLAUDE.md](CLAUDE.md)** - Architecture overview and design patterns
- **[HTTP-TRANSPORT.md](HTTP-TRANSPORT.md)** - HTTP transport details
- **[LOGGING.md](LOGGING.md)** - Logging system documentation
- **[MCP Specification](https://spec.modelcontextprotocol.io/)** - Official Model Context Protocol specification

## Examples

The [examples/](examples/) directory contains:

- **go-mcp/** - A complete MCP server demonstrating stdio/HTTP/UNIX-socket mode, auth integration, and graceful shutdown
- **tools/date.go** - Example tool with arguments, an `outputSchema`, and `structuredContent`
- **tools/fortune.go** - Example tool without arguments (executes fortune command)
- **tools/confirm.go** - Reference implementation of Multi Round-Trip Requests (elicitation)

Sample configs live at the repo root: [config.yaml](config.yaml) (stdio),
[config-unix.yaml](config-unix.yaml), [config-http.yaml](config-http.yaml), and
[config-oauth-example.yaml](config-oauth-example.yaml).

To build and run the example:

```bash
go build -o go-mcp ./examples/go-mcp
./go-mcp -config config.yaml
```

## Key Concepts

### Transport Layer
Abstracts communication mechanisms behind a common interface:
- **StdioTransport** - Reads from stdin, writes to stdout (for Claude Code, desktop apps).
  Serves arbitrary streams via `NewStdioTransportWithStreams(in, out)` — useful for tests. Alone
  among the transports its run has a natural end, so it also offers `Done()` (closed when the input
  reaches EOF) and `Err()` (nil for a clean EOF, non-nil if the read failed)
- **UnixTransport** - Newline-delimited JSON-RPC over a UNIX domain socket (local IPC)
- **HTTPTransport** - POST-only `/mcp` Streamable HTTP endpoint (web services, remote access)

Because the protocol is stateless, a transport may have several requests in flight concurrently on
one connection, so `HandleMessage` takes a `context.Context` and a `ResponseWriter` rather than
returning a single buffered response.

`HTTPTransport` owns its own `http.ServeMux` and `http.Server`, so `/mcp` cannot be mounted onto a
router you control. `HTTPTransportConfig.ExtraRoutes func(*http.ServeMux)` is the seam for endpoints
that need the same listener anyway — health and readiness probes, metrics. It runs before `/mcp` and
the auth routes, so a colliding pattern panics at startup rather than shadowing the protocol
endpoint, and the routes it adds sit outside the auth middleware. Leave it nil and routing is
exactly what it was before the field existed. See [HTTP-TRANSPORT.md](HTTP-TRANSPORT.md).

`transport` does not import `auth`: `HTTPTransportConfig.AuthService` is the small
`transport.AuthProvider` interface, so a server using only `mcp` + `transport` never pulls in
BoltDB. (`transport` itself is pure stdlib; `mcp` adds `gopkg.in/yaml.v3`, which the skills loader
needs to parse `SKILL.md` frontmatter.) Assign it only when auth is actually enabled — an unconditionally-assigned nil `*auth.AuthService`
is a typed nil that reads as non-nil through the interface.

### Tool and Resource Registries
Simple API for registering and listing tools (invocation is handled internally by the server, which
also enforces `_meta` validation, `x-mcp-header` checks, and Multi Round-Trip Request state
verification before your function runs):

```go
registry := mcp.NewToolRegistry()
registry.Register(toolDefinition, toolFunction)
registry.Unregister("my_tool") // bool: whether a tool by that name was there to remove
registry.List()                // All registered tools, in registration order (stable across calls)
registry.Get("my_tool")        // (Tool, bool) lookup by name
registry.HasTools()            // Whether any tool is registered
```

`mcp.NewResourceRegistry()` mirrors this for resources (`Register`, `Unregister` keyed by URI,
`List`, `Get`, `Read`, `HasResources`) and is a required argument to `mcp.NewServer` even when you
register no resources. It additionally carries the resource *template* API described in
[Resource Templates](#resource-templates). It adds one method with no tool equivalent:

```go
resources.NotifyUpdated("config://server-name") // announce a content change; bool = URI is registered
```

See [Change Notifications](#change-notifications) below for what that delivers and why it can't be
automatic.

Both registries are safe for concurrent use and can be mutated at runtime after the server has
started. Every mutation that actually changes the catalog emits a
`notifications/tools/list_changed` (or `resources/list_changed`) to any client with an open
`subscriptions/listen` stream; `Unregister` on a name or URI that isn't present returns `false`
and notifies nobody.

Three behaviors worth knowing:

- **Re-registering an existing name/URI replaces the entry and moves it to the end of the list**
  rather than adding a second one, so the list can never disagree with the function map.
- **Re-registering a resource URI also fires `resources/updated`** for it, on top of
  `list_changed`. A replacement swaps both the metadata and the `ResourceFunction`, and Go cannot
  compare func values, so the registry can't tell a description edit from a content swap —
  announcing it is the safe side of that. Registering a *new* URI fires `list_changed` only.
- **Removing the last tool or resource withdraws that capability** from `server/discover`, since
  capabilities are derived from what is actually registered.

Because list cursors are opaque offsets, unregistering between a client's page fetches can shift
later entries and cause one to be skipped — that is what `list_changed` is for; a client that sees
it should restart pagination.

### Resource Templates

Some resource families cannot be listed. Every pod in a cluster, every row in a table, every
environment variable — the keyspace is unbounded, so `resources/list` is the wrong shape for it.
The protocol's answer is an **RFC 6570 URI template**: the server advertises the *shape* through
`resources/templates/list`, the client expands it locally, and the read arrives on the ordinary
`resources/read` method carrying a concrete URI.

```go
resources.RegisterTemplate(mcp.ResourceTemplate{
    URITemplate: "mcp+kubectl://{context}/pod/{namespace}/{name}",
    Name:        "Pod",
    Description: "One pod, as JSON",
    MimeType:    "application/json",
}, func(ctx context.Context, req *mcp.ResourceReadRequest) (mcp.ResourceContentResult, error) {
    pod, err := lookupPod(ctx, req.Vars["context"], req.Vars["namespace"], req.Vars["name"])
    if errors.Is(err, errNoSuchPod) {
        // -32602 "Unknown resource", not -32603: absent is not a server fault.
        return mcp.ResourceContentResult{}, fmt.Errorf("pod %q: %w", req.Vars["name"], mcp.ErrResourceNotFound)
    }
    if err != nil {
        return mcp.ResourceContentResult{}, err
    }
    return mcp.ResourceContentResult{Text: pod}, nil
})
```

`RegisterTemplate` returns an `error` — unlike `Register`, which cannot fail — because the template
is compiled into a matcher up front, so a malformed one is caught at startup instead of silently
never matching. The registry API mirrors the concrete one: `UnregisterTemplate`, `ListTemplates`,
`HasTemplates`, `MatchTemplate`.

**Supported template syntax.** Reverse matching (URI → variable bindings) is not part of RFC 6570 —
the RFC only defines expansion — so this library implements it, and deliberately implements a small,
predictable subset. Anything else is a registration error naming the offending expression:

| Form | Matches | Notes |
|---|---|---|
| `{var}` | one path segment (never `/`) | RFC 6570 Level 1, which is all many clients implement |
| `{+var}` | one or more characters, `/` included | Reserved expansion; allowed only as the **final** expression |

Rejected: `{#frag}`, `{?query}`, `{&param}`, `{/path}`, `{.ext}`, `{;param}`, multi-variable `{a,b}`,
and the `{var:3}` / `{var*}` modifiers.

Things worth knowing:

- **A concrete resource always beats a template.** An exactly-registered URI is a deliberate claim
  about that one URI; a template that also matches it is the more general one.
- **Templates match in registration order, first match wins.** Register the more specific template
  first, or `scheme://x/{a}` will shadow `scheme://x/{a}/detail`.
- **Variables arrive percent-decoded** in `req.Vars`. `contents[0].uri` in the response is always the
  concrete URI the client asked for, never the template.
- **A template family counts as resources** for capability purposes: a registry holding only
  templates still advertises `resources` in `server/discover`, and `RegisterTemplate` /
  `UnregisterTemplate` fire `resources/list_changed` like any other catalog change. There is no
  separate template-changed notification.
- **`NotifyUpdated` works on template URIs.** A URI matching a registered template is announceable
  even though it was never registered individually — which is the point when you are watching an
  upstream that adds and removes members on its own.

There is deliberately no "templates supported" capability flag; the spec has none. Clients probe
`resources/templates/list` and treat `-32601` as unsupported. This server always answers it,
returning an empty array when nothing is registered.

#### Completion (optional)

A template describes a shape, not its contents, so `completion/complete` is the only protocol
mechanism for exploring a large variable domain. It is entirely opt-in: attach a provider and the
server declares the `completions` capability; attach none and `completion/complete` returns `-32601`,
which is exactly the probe clients use.

```go
resources.SetTemplateCompleter("mcp+kubectl://{context}/pod/{namespace}/{name}",
    mcp.CompletionFunc(func(ctx context.Context, req *mcp.CompletionRequest) (mcp.CompletionResult, error) {
        // req.Argument is the variable being completed, req.Value the partial input, and
        // req.Context the variables the client already resolved — use it to narrow the query.
        names, total := searchPodNames(ctx, req.Context["namespace"], req.Value)
        return mcp.CompletionResult{Values: names, Total: &total}, nil
    }))
```

Back it with a prefix index or a paged upstream query; never materialize the keyspace, since not
fitting in memory is why the template exists. The server truncates `Values` to the spec's 100-value
ceiling and sets `hasMore` if it had to.

### Skills (experimental)

**Status: opt-in and unstable.** This implements [SEP-2640, the Skills
Extension](https://github.com/modelcontextprotocol/modelcontextprotocol/pull/2640) — an
Extensions Track proposal that is **open and not merged**, written here against head `641d1eb`
(2026-08-20). Nothing about skills appears in the ratified 2026-07-28 specification, and **no
public client consumes it yet**: Claude Code's skills are local-only (`~/.claude/skills`, project
directories, plugins, claude.ai sync) with no `skill://` support, and SEP-2640 says Anthropic's
own support is prototyped internally. Wire shapes may change. Adopt it to be ready, not to be
consumed today.

A *skill* is a directory holding a `SKILL.md` with YAML frontmatter, plus any supporting files.
Serving one over MCP lets a host load a procedure written against **your** tools rather than
generic advice. The library's job is the catalog: publishing each skill's frontmatter verbatim
along with a sha256 digest per file, and serving those files as ordinary resources.

```go
//go:embed all:skills
var skillFS embed.FS

resources := mcp.NewResourceRegistry()
skills := mcp.NewSkillRegistry(resources)

content, _ := fs.Sub(skillFS, "skills") // keep the embed directory out of the URIs
if err := skills.LoadFS(content, "skill://"); err != nil {
    log.Fatal(err)
}

server := mcp.NewServer(tools, resources, &mcp.ServerConfig{
    Skills:              skills, // nil (the default) disables the whole extension
    SkillsDirectoryRead: true,   // opt into resources/directory/read
})
```

`LoadFS` walks the filesystem, parses and validates frontmatter, computes digests over raw bytes,
and registers every file as a readable resource. That single call is the point of the API: a
hand-maintained digest rots silently, and this one cannot — `SkillRegistry` derives digests from
the same bytes `resources/read` returns and offers no way to supply one.

Registering a skill (or a whole `LoadFS`) fires the ordinary
`notifications/resources/list_changed`, once per skill rather than once per file. SEP-2640 defines
no skills-specific notification, so none is invented here.

**Validation**, from the [Agent Skills specification](https://agentskills.io/specification), applied
at registration:

| Field | Rule |
|---|---|
| `name` | required, 1–64 chars, `[a-z0-9]` in single-hyphen-separated groups, and equal to the skill directory's name |
| `description` | required, non-empty, ≤ 1024 chars |
| `compatibility` | optional, ≤ 500 chars |
| everything else | passed through verbatim and uninterpreted |

`allowed-tools` is deliberately in that last row. It is a host-side consent matter that SEP-2640
says hosts MUST ignore for MCP-origin skills absent explicit per-skill approval, so this library
must not act on it.

Things worth knowing:

- **The `skill://` scheme is not privileged.** A server MAY serve skills under any scheme native to
  its domain (`github://owner/repo/skills/refunds/SKILL.md`), and everything works identically.
- **Skill-ness is established only by `skills/list` and `skills/get`.** Registering a `skill://`
  resource directly on the `ResourceRegistry` does *not* make it a skill, and this library never
  scans for a URI shape to invent one.
- **The last path segment must equal the frontmatter `name`.** `skill://acme/billing/refunds/SKILL.md`
  needs `name: refunds`; the segments before it are a free organizational prefix.
- **Nested skills publish flat.** A skill directory may contain further skills; the nested skill's
  files also appear in the enclosing skill's `resources` list, and the nested skill is listed as an
  ordinary top-level entry. Unregistering one leaves files the other still publishes readable.
- **`skills/get` answers for URIs `skills/list` never returned.** Attach a `SkillResolver` for a
  catalog too large or too dynamic to enumerate; it is consulted only when a registered lookup
  misses, and pair it with a `ResourceTemplate` so the files stay readable.
- **`skills/get`'s cache envelope is this library's call, not the spec's.** SEP-2640 defines `ttlMs`
  and `cacheScope` for `skills/list` and leaves `skills/get` open; a single entry gets the same
  hints with the same list TTL.

`resources/directory/read` is a separate opt-in (`SkillsDirectoryRead`) reported as the extension's
`directoryRead` setting. It lists the direct children of a directory in the resource namespace,
derived lexically from registered resource URIs — nothing to keep in step, and it is
scheme-agnostic rather than skill-exclusive. A directory with no registered descendants is
indistinguishable from one that never existed, and both answer `-32602`.

With `ServerConfig.Skills` left nil there is no `extensions` key in `server/discover` and all three
methods answer `-32601` — the same probe clients use to detect an unimplemented extension.

### Change Notifications

One long-lived request, `subscriptions/listen`, is the **entire** server→client notification
channel. The 2026-07-28 revision deleted `resources/subscribe`/`resources/unsubscribe` along with
the server's ability to originate JSON-RPC requests at all, and folded everything they did into this
one call. A client opens it once and leaves it open.

(Not to be confused with `notifications/progress`, which rides a tool call's *own* request stream
while that call runs. The subscription stream carries only catalog and resource-content changes.)

A client subscribes by naming what it wants in the `notifications` filter:

```json
{
  "jsonrpc": "2.0", "id": 4, "method": "subscriptions/listen",
  "params": {
    "notifications": {
      "toolsListChanged": true,
      "resourcesListChanged": true,
      "resourceSubscriptions": ["config://server-name", "file:///var/db/status.json"]
    },
    "_meta": {
      "io.modelcontextprotocol/protocolVersion": "2026-07-28",
      "io.modelcontextprotocol/clientCapabilities": {}
    }
  }
}
```

Every field is optional; omitting one simply means "don't send me those." The filters are
independent — a client that sets only `resourcesListChanged` hears nothing about resource *content*,
and vice versa.

| Filter field | Delivers | Triggered by |
|---|---|---|
| `toolsListChanged` | `notifications/tools/list_changed` | automatic, on any `ToolRegistry` mutation |
| `resourcesListChanged` | `notifications/resources/list_changed` | automatic, on any `ResourceRegistry` mutation |
| `resourceSubscriptions: [uri…]` | `notifications/resources/updated`, params `{"uri": …}` | **your code**, via `resources.NotifyUpdated(uri)` |
| `promptsListChanged` | — | accepted, never fires: prompts are not implemented |

That split is the thing to understand. The two `list_changed` flags need no code from you —
`mcp.NewServer` wires each registry's internal change hook to the server's broker, so ordinary
`Register`/`Unregister` calls emit them. `resourceSubscriptions` **cannot** work that way: a
`ResourceFunction` is called on demand and returns whatever it likes, so the library never sees the
underlying data and has no way to notice it changed. Only your code knows.

```go
// Watch something and announce it. NotifyUpdated returns false for a URI that
// isn't registered — a no-op that notifies nobody, so typos surface instead of
// silently broadcasting.
go func() {
    for range fileChanged {
        resources.NotifyUpdated("file:///var/db/status.json")
    }
}()
```

URIs are matched **exactly**. The spec permits a server to announce a sub-resource of a URI the
client subscribed to; a `ResourceRegistry` is a flat map with no URI hierarchy to derive one from,
so a client hears about exactly the URIs it named.

#### What the stream looks like

The first message is always an acknowledgment echoing the filter the server actually registered:

```json
{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{
  "notifications":{"toolsListChanged":true,"resourcesListChanged":true,
                   "resourceSubscriptions":["config://server-name","file:///var/db/status.json"]},
  "_meta":{"io.modelcontextprotocol/subscriptionId":4}}}
```

Then notifications, until the request ends:

```json
{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{
  "uri":"config://server-name",
  "_meta":{"io.modelcontextprotocol/subscriptionId":4}}}
```

Every message on the stream carries `_meta["io.modelcontextprotocol/subscriptionId"]`, set to the
listen request's own JSON-RPC `id`. That is mandatory, not decorative: on stdio and UNIX sockets all
requests share one channel, so a client running several concurrent subscriptions needs it to tell
them apart.

#### Delivery guarantees

**Notifications are hints, not an event log.** Each subscriber gets a 16-deep buffer, and once it is
full the broker drops rather than blocks — a registry mutation, or a `NotifyUpdated` call on a hot
path, must never stall behind a slow client. A burst can therefore reach a client as fewer
notifications than you sent, or as none at all.

Design both sides around that:

- Treat every notification as *"something may have changed, go re-read"* — never as a count of
  changes, and never as a stream you can replay state from.
- There is no `Last-Event-ID` resumability in this revision. A client that reconnects gets no
  backfill; it should re-`list` and re-`read` what it cares about.
- Because `ReadTTLMs` defaults to `0` (always refetch — see below), `resources/updated` is not
  load-bearing for cache invalidation. It is a *push* signal that lets a client re-read promptly
  instead of polling.

#### Lifecycle and cancellation

The request stays open until the client ends it. How depends on the transport:

| Transport | Client ends the subscription by | Notes |
|---|---|---|
| Streamable HTTP | closing the response stream | A listen request always becomes `text/event-stream`, since its acknowledgment is itself a notification. A `:` keep-alive comment every 15s holds it open through idle proxies. |
| stdio / UNIX socket | sending `notifications/cancelled` with `params.requestId` set to the listen request's `id` | Matched on the raw JSON id, so `4` and `"4"` are different requests. The connection multiplexes, so the subscription never blocks other in-flight requests. |

Either way the server writes **no final JSON-RPC response** for the listen request — see
"[Not implemented in this revision](#not-implemented-in-this-revision)" above. See
[HTTP-TRANSPORT.md](HTTP-TRANSPORT.md) for the SSE framing details.

### Result Caching Hints
Every result carries a `resultType`, and list/read results carry `ttlMs` and `cacheScope` hints.
`ListTTLMs` covers `server/discover`, `tools/list`, `resources/list`, and
`resources/templates/list`; `ReadTTLMs` covers `resources/read`. Defaults are 5 minutes for
catalogs and 0 (always refetch) for resource reads; override via `ServerConfig.ListTTLMs`,
`ReadTTLMs`, and `DefaultCacheScope` (set `"private"` for per-user catalogs behind auth).

`completion/complete` deliberately carries neither: the spec does not list it among the cacheable
operations, and a completion list is the last thing a client should be holding on to.

### JSON-RPC 2.0 Protocol
All MCP communication follows JSON-RPC 2.0 specification with automatic message parsing, validation, and error handling.

## Building

### Standard Build
```bash
go build -o my-mcp-server
```

### Cross-Compilation
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o my-mcp-server
```

### Docker Multi-Platform Build
```bash
docker buildx build --platform linux/amd64,linux/arm64 -t my-mcp-server:latest .
```

See [CLAUDE-new-project-harness.md](CLAUDE-new-project-harness.md) for complete build instructions and Dockerfile examples.

## License

MIT License - see [LICENSE](LICENSE) file for details.

## Contributing

Contributions are welcome! This library is designed to be general-purpose and not specialized to any particular use case. When contributing:

1. Keep the core library transport-agnostic
2. Follow JSON-RPC 2.0 and MCP specification conventions
3. Add tests for both stdio and HTTP transports
4. Document new features in the appropriate .md files

## Acknowledgments

Built following the [Model Context Protocol specification](https://spec.modelcontextprotocol.io/) by Anthropic.
