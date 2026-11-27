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
- **OAuth Authentication** - OAuth 2.1 authorization server for HTTP mode (PKCE, dynamic client
  registration). Users sign in with GitHub or any OpenID Connect provider; access rules admit them
  and grant app-defined roles, which can hide tools from callers who lack them
- **YAML Configuration** - File-based config with defaults; OAuth credentials may instead be read
  from mounted secret files (Docker/Kubernetes)
- **Structured Logging** - Multi-level logging (trace/debug/info/warn/error) with JSON and text formats
- **Simple Tool API** - Register tools with JSON schema definitions and type-safe handlers
- **Live Change Notifications** - Runtime-mutable tool and resource catalogs; clients subscribe once
  via `subscriptions/listen` and receive `list_changed` and per-resource `updated` notifications
- **Parameterized Resources** - RFC 6570 resource templates (`resources/templates/list`) for
  unbounded keyspaces no `resources/list` could enumerate, with optional `completion/complete`
  to help clients narrow a template variable
- **Skills (beta)** - Opt-in support for the `io.modelcontextprotocol/skills` extension
  (SEP-2640), which Claude and Claude Code load skills through: `skills/list`, `skills/get`, and
  `resources/directory/read`, with a loader that turns an `embed.FS` of skill directories into a
  catalog whose digests, sizes and frontmatter cannot disagree with the files. Off unless you
  configure it
- **Production Ready** - BoltDB token storage, HMAC-signed MRTR request state, graceful shutdown
- **Small Dependency Surface** - `transport` imports nothing beyond the Go standard library; `mcp`
  adds only `gopkg.in/yaml.v3` (for parsing skill frontmatter). `auth` (BoltDB) is opt-in, pulled in
  only if you import it

## Using this library from another project

```bash
go get github.com/spirilis/generic-go-mcp@v0.12.0
```

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
├── auth/                 # OAuth 2.1 authorization server: GitHub and OIDC sign-in, access rules, roles
├── transport/            # Transport abstractions (stdio, UNIX socket, Streamable HTTP)
├── mcp/                  # MCP protocol implementation (JSON-RPC 2.0)
├── compat/               # Optional legacy (2025-11-25 and earlier) compatibility overlay
├── examples/
│   ├── go-mcp/           # Complete example server application
│   │   └── skills/       # Embedded demonstration skill (Skills extension, every mode)
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
  storage:
    dbPath: "/var/lib/go-mcp/oauth.db"
  providers:
    - name: github
      type: github
      clientId: "your-github-oauth-app-id"
      clientSecretFile: "/run/secrets/github/client-secret"
    - name: corp
      type: oidc
      displayName: Corp SSO
      issuerURL: "https://login.example.com/realms/corp"
      clientId: "mcp-server"
      clientSecretFile: "/run/secrets/corp/client-secret"
  access:
    rules:
      - provider: github
        users: ["my-github-user"]
        roles: [admin]
      - provider: corp
        groups: ["mcp-users"]
        roles: [reader]
    identityMaxAge: 168h
  # Optional: only these hosts may appear in a self-registered client's redirect_uris.
  # registration:
  #   allowedRedirectHosts: ["claude.ai", "localhost", "127.0.0.1"]

logging:
  level: "info"
  format: "json"
```

Note the key names: they are camelCase under `auth:` and snake_case under `server:`, matching the
struct tags in `config/config.go` and `config/auth_providers.go`. See
[config-oauth-example.yaml](config-oauth-example.yaml) for the fully annotated version, including
the legacy form and pre-registered static clients.

### Authentication

With `auth.enabled`, the server is an OAuth 2.1 authorization server for its MCP clients (`/register`,
`/authorize`, `/token`, PKCE required), and `/mcp` takes only its bearer tokens. Users prove who they
are by signing in, in the browser, with an upstream **provider**: GitHub, or any OpenID Connect
identity provider. **Access rules** then decide who is admitted and which of the application's
**roles** they hold.

#### The redirect URI

Every provider sends the browser back to one URL on this server: **`<issuer>/callback`** (for the
config above, `https://mcp.example.com/callback`). Register exactly that URL at each provider: in the
GitHub OAuth App's "Authorization callback URL", or as a valid redirect URI of the OIDC client (in
Keycloak, the client's "Valid redirect URIs"; in Entra ID, a "Web" platform redirect URI). There is
no per-provider path and no `redirect_url` key. The pending sign-in remembers which provider it went
to.

With more than one provider, `/authorize` shows a small chooser page (`POST /authorize/provider`);
with one, it redirects straight to it.

#### GitHub

GitHub is the two-value setup: an [OAuth App](https://github.com/settings/developers) with the
callback URL above, and its client ID and secret.

```yaml
providers:
  - name: github          # principal ids are "github:<login>", as before v0.12
    type: github
    clientId: "Iv1.xxxxxxxxxxxxxxxxxx"
    clientSecret: "xxxxxxxx"
    # GitHub Enterprise Server: baseURL (apiURL defaults to <baseURL>/api/v3)
    # baseURL: "https://github.example.com"
```

At each sign-in the library reads the user's organizations and team memberships (scope `read:org`)
and stores them as groups `org` and `org/team-slug`, so a rule can match them. A failed lookup is
logged and those groups are left out; the sign-in still succeeds.

The legacy form, `auth.github` plus `auth.allowlist`, still loads and behaves exactly as before. It
stands for one provider named `github` and one rule on it.

#### OpenID Connect providers

Any provider that publishes `<issuerURL>/.well-known/openid-configuration` works: Keycloak, Entra
ID, Google, Okta, Authentik, Dex, Cognito. The library uses the authorization code flow with PKCE
(S256) and a nonce, and verifies the id_token itself: RS256 or ES256 only, signature against the
provider's JWKS, `iss`, `aud`/`azp`, `exp`/`iat`/`nbf` with 60 seconds of leeway, and the nonce.

| Key | Default | Meaning |
| --- | --- | --- |
| `name` | (required) | `[a-z][a-z0-9-]{0,31}`, unique. Rules refer to it, and it prefixes principal ids (`corp:<sub>`). |
| `type` | (required) | `oidc` (or `github`). |
| `displayName` | the name | Shown on the chooser and consent pages. |
| `issuerURL` | (required) | Must be https and equal the `issuer` the discovery document reports, exactly. http is accepted only on loopback (`localhost`, `127.0.0.1`), for a local IdP in development; only then may the discovery document's endpoints be http (on loopback) too. |
| `clientId` / `clientIdFile` | (required) | The client registered at the IdP. |
| `clientSecret` / `clientSecretFile` | none | Omit for a public client. |
| `tokenEndpointAuthMethod` | `client_secret_basic` with a secret, else `none` | Or `client_secret_post`. Must agree with whether a secret is set. |
| `scopes` | `[openid, profile, email]` | `openid` is always sent. |
| `claims.username` | `preferred_username` | Dotted paths reach nested claims. A key that itself contains dots or colons (`https://x/groups`, `cognito:groups`) is tried whole first. |
| `claims.email` | `email` | `email_verified` speaks only for the standard `email` claim, so with any other path (`upn`, `mail`) the email counts as unverified: email rules then match it only with `requireVerifiedEmail: false`. |
| `claims.usernameIsUnique` | `false` | Allows `users` rules for this provider. Set it only when the IdP guarantees the username claim is unique and that users cannot choose or change it; OIDC promises neither for `preferred_username`. |
| `claims.name` | `name` | |
| `claims.groups` | `groups` | A string or an array of strings, e.g. `realm_access.roles` (Keycloak realm roles), `cognito:groups`. |
| `claims.subject` | `sub` | Not configurable: users are always keyed by `(provider, issuer, sub)`, never by email or username. |
| `userInfo` | `false` | Also fetch the userinfo endpoint and merge its claims (its `sub` must match). Some IdPs put groups only there. |
| `authParams` | none | Extra `/authorize` parameters: `prompt`, `hd`, `domain_hint`, `acr_values`... The ones the library sets (`client_id`, `redirect_uri`, `state`, `nonce`, `code_challenge*`, `response_type`, `scope`) are refused. |
| `allowedAlgorithms` | `[RS256, ES256]` | A subset of those two. `none` and `HS*` are never accepted. |
| `caFile` | system roots | PEM bundle added to the system roots, for an IdP behind a private CA. |
| `audience` | none | An `aud` value accepted besides the client id. |
| `allowedJWKSHosts` | none | Hosts besides the issuer's that `jwks_uri`, and `userinfo_endpoint` when `userInfo` is on, may use (the userinfo claims are unsigned and decide admission as much as the keys do). Some IdPs (Google, for one) serve these from hosts other than the issuer's; list the hosts their discovery document names. |
| `requireVerifiedEmail` | `true` | `emails` and `emailDomains` rules match only an email the IdP marks `email_verified`. OIDC only. |

Discovery is lazy: startup makes no network calls, and an IdP that is down at startup does not stop
the server. The first sign-in fetches discovery and keys in the background; until they arrive, that
provider's `/authorize` answers 503 with `Retry-After`. Keys are refreshed hourly, and on an unknown
`kid` at most once a minute. A discovery document whose `issuer` differs from `issuerURL`, or whose
`jwks_uri` (or, with `userInfo`, `userinfo_endpoint`) is not https on an allowed host, disables the
provider until restart.

#### Access rules

```yaml
access:
  rules:
    - provider: github
      users: ["spirilis"]              # pass/fail: admitted, no role
    - provider: github
      teams: ["acme/platform"]         # orgs: ["acme"] for a whole org
      roles: [admin]
    - provider: corp
      groups: ["mcp-admins"]
      roles: [admin, reader]
    - provider: corp
      emailDomains: ["example.com"]    # verified email only
      roles: [reader]
  identityMaxAge: 168h
```

- A user is admitted when **at least one rule** matches, and holds the **union** of the roles of
  every matching rule. A rule matches when **any one** of its matchers does.
- Matchers: `users` (GitHub login, case-insensitive; for OIDC only with `claims.usernameIsUnique`),
  `subjects` (exact `sub`; for GitHub the numeric user id), `emails` (case-insensitive) and
  `emailDomains` (the exact domain after the last `@`, no suffix match), both OIDC only and verified
  emails only, `groups` (exact for OIDC, case-insensitive for GitHub), and, for GitHub only, `orgs`
  and `teams` (`org/team-slug`). `anyone: true` admits every user the provider authenticates; it
  stands alone in its rule and is logged as a warning.
- **Fail closed:** in this form, a provider no rule names admits nobody (a startup warning says so).
  Only the legacy form with an empty `allowlist` still admits every GitHub user.
- Startup refuses a rule naming an unknown provider, an empty rule, `orgs`/`teams` on an OIDC rule,
  `emails`/`emailDomains` on a GitHub rule (GitHub does not say whether the profile email is
  verified), `users` on an OIDC rule without `claims.usernameIsUnique`, an empty or malformed entry,
  and a role the application did not declare.
- Rules are evaluated **on every request**, against an identity snapshot (username, email, groups)
  taken at the user's last browser sign-in. A config change applies at once; a user no rule matches
  any more gets `403 insufficient_scope`.
- **`identityMaxAge`** bounds how stale that snapshot can get. A client can refresh its tokens for
  this long after the user last signed in through the browser; then the refresh grant answers
  `invalid_grant` and the client sends the user through `/authorize` again, which takes a new
  snapshot. `0` (the default) means the refresh token lifetime, 30 days. The refresh grant also
  re-checks the rules.

#### Roles and the Go API

Roles are an enumerated list your application declares. Config can grant only those, so a typo in a
rule fails at startup instead of granting nothing.

```go
authService, err := auth.NewAuthService(cfg.Auth,
    auth.WithRoles(
        auth.Role{Name: "admin", Description: "may use destructive tools"},
        auth.Role{Name: "reader", Description: "may use the read-only tools"},
    ))

// Gate a tool: callers holding none of the roles never see it.
registry.RegisterWithOptions(deleteTool, deleteFn, mcp.RequireRoles("admin"))

server := mcp.NewServer(registry, resources, &mcp.ServerConfig{
    RolesFromContext: authService.RolesFromContext,
    PrincipalFromContext: func(ctx context.Context) string {
        p, _ := auth.PrincipalFromContext(ctx)
        if p == nil {
            return ""
        }
        return p.ID
    },
})
```

Inside a tool, or anywhere downstream of the middleware:

```go
p, ok := auth.PrincipalFromContext(ctx) // *auth.Principal
p.ID            // "<provider>:<sub>" for OIDC; "github:<login>" (login lowercased) for GitHub
p.Provider, p.Subject // never reassigned: key data by these where a renamed and reclaimed
                      // GitHub login must not inherit it (GitHub's Subject is the numeric id)
p.Provider      // "github", "corp"
p.Username, p.Email, p.EmailVerified, p.Name, p.Groups
p.Roles         // sorted, from the current rules
p.HasRole("admin")
auth.HasRole(ctx, "admin")
authService.Roles() // what the application declared, for docs or a UI
```

`mcp.RequireRoles(roles...)` is any-of, passed to `RegisterWithOptions` (`Register` keeps its
two-argument signature, which embedders' own `Registrar` interfaces rely on). With `ServerConfig.RolesFromContext` set, a gated tool is left
out of `tools/list` for a caller without one of its roles, and `tools/call` on it answers exactly
the `-32602` an unknown tool name gets, so a probe cannot tell the two apart. The same holds for
legacy clients through the `compat` overlay. While any tool is gated, `tools/list` results carry
`cacheScope: "private"`. `RequireRoles()` with no roles hides the tool from everyone. With
`RolesFromContext` nil (stdio, UNIX sockets, HTTP without auth) nothing is gated. Two things stay
caller-independent: `server/discover` declares the tools capability whenever any tool is registered,
and `notifications/tools/list_changed` goes to every listener (it names no tool).

The admin API (`POST`/`GET /admin/clients`, which mints and lists static clients) is served only to
holders of the role you name with `auth.WithAdminRole("admin")` (it must be a declared role); others
get 403. Without `WithAdminRole`, a config in the new form (`auth.providers` or `auth.access`) does
not serve it at all, and only the pure legacy form keeps the v0.11 behaviour of serving it to every
admitted user. Legacy rules grant no roles, so `WithAdminRole` with a legacy config closes the
admin API to everyone.

`transport.AuthProvider.UserFromContext` still returns the store's user id and the username, and
`auth.GetUserFromContext` the stored `*auth.User`. Do not build principal ids from
`User.GitHubLogin`: it is empty for OIDC users and for GitHub providers not named `github`, so all of
them would share one id. Use `auth.PrincipalFromContext`.

#### Secrets from files (Helm, Kubernetes)

Every `clientId` and `clientSecret` has a `clientIdFile` / `clientSecretFile` twin that reads the
value from a file and wins over the inline one, so a chart can mount a Secret and keep credentials
out of its ConfigMap:

```yaml
providers:
  - name: github
    type: github
    clientIdFile: /run/secrets/github/client-id
    clientSecretFile: /run/secrets/github/client-secret
```

Surrounding whitespace is trimmed. A file that cannot be read, or is empty, refuses to start, with
an error naming the key and the file. An `access:` key left present but empty (rules commented out,
or Helm rendering `null`) counts as the new form when the config is read with `config.Load` (or
`LoadFromBytes`): it admits nobody rather than falling back to the legacy admit-everyone. An
embedder that decodes `config.AuthConfig` into its own struct gets a nil `Access` for a null
`access:` (yaml.v3 leaves a null pointer nil), which is the legacy form; write `access: {}` or
`access: {rules: []}` there, or set `Access` to `&config.AccessConfig{}` in code.

#### Upgrading to v0.12.0

Existing configs and code keep working: `auth.github` + `auth.allowlist` load and admit the same
users, `/callback` is unchanged (no OAuth App needs editing), stored users and their consents carry
over, and `NewAuthService(cfg)`, `ToolRegistry.Register(tool, fn)` and `transport.AuthProvider` keep
their exact signatures (so interfaces you declared around them still hold; gating uses the new
`RegisterWithOptions`). A few behaviors did change:

- **MRTR answers need their `requestState`.** A `tools/call` carrying `inputResponses` without a
  `requestState` is refused with -32602 before the tool runs. Earlier versions passed such answers
  to the tool unverified, so a client could pre-confirm a confirm-before-act tool. Real clients echo
  the `requestState` they were given and are unaffected.
- **Refresh is no longer forever.** Each user must sign in through the browser again at least every
  `identityMaxAge` (30 days unless set). Clients handle this as an ordinary `invalid_grant`.
- **Refresh is bounded for users stored before v0.12 too.** Their age counts from their first
  token refresh after the upgrade, so they sign in again within `identityMaxAge` of it.
- **Stored users from before v0.12 have no groups on record.** When no rule matches such a user but
  some rule for their provider matches on `orgs`, `teams` or `groups`, `/mcp` answers 401 ("Sign in
  again"), not 403, so a user admitted through an org or team signs in once and that sign-in records
  the groups. With no such rule (a `users`-only allowlist, say) a sign-in could not change the
  outcome, so a removed user gets 403, as in v0.11. Users admitted by login are unaffected.
- **A provider name is bound to its issuer.** `Principal.ID` is the provider name plus the sub or
  login, without the issuer, so the server refuses to start when the store holds users of a
  provider from another issuer than the one it has now: pointing the provider named `github` at an
  Enterprise Server (`baseURL`), or changing an OIDC `issuerURL`, would otherwise give the new
  issuer's users the ids (and your data keyed by them) of the old issuer's. Give the new issuer a
  new provider name; the old name's users are no longer admitted once it is removed. The check
  reads the auth store, so it does not survive replacing the database file while your own data
  keyed by principal ids stays.
- **The admin API needs a role in the new form.** `/admin/clients` is served to every admitted user
  only for the pure legacy form; with `auth.providers` or `auth.access`, name the role that may use
  it with `auth.WithAdminRole`, or it is not served (see "Roles and the Go API").
- **A `*File` secret that is unreadable or empty now refuses to start** instead of being ignored.
- **GitHub API errors are checked.** A failed orgs or teams lookup at sign-in drops those groups (and
  any roles they grant) until the next successful sign-in.
- **`auth.Storage` gained `GetUserByIdentity`.** A custom `Storage` implementation (the library's
  own is `BoltStorage`) must add it.
- Before enabling an OIDC provider, or a GitHub provider not named `github`, move any principal ids
  built from `User.GitHubLogin` to `auth.PrincipalFromContext(ctx).ID`. `Principal.ID` lowercases
  the login (`github:spirilis`), so if your stored ids kept the login's case (`"github:" +
  User.GitHubLogin`), lowercase them, or compare case-insensitively, when you switch.
- The debug access log names the signed-in user's login under `login` (it was `github_login`).

What a new config gains: `auth.providers` (several, GitHub Enterprise, any OIDC IdP), `auth.access`
rules with roles, and `identityMaxAge`. New-form configs fail closed: a provider with no rule admits
nobody.

#### The consent screen

Dynamic client registration at `<issuer>/register` is open, as RFC 7591 intends, and GitHub (like
most IdPs) skips its own approval prompt for an app the user has already authorized. Together those
would let anyone register a client whose `redirect_uri` is their own host and harvest a working
authorization code from an admitted user who merely follows a link — the confused-deputy problem the
MCP security guidance describes. So after sign-in and the access rules, the first time a user
authorizes a client the operator did not list under `auth.clients`, the server shows a consent screen
on the issuer's own origin. It names the client, the provider and the signed-in user, and the host
the browser will be sent to. The approval is remembered per user and client, so it costs one click per
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

`/register` reads RFC 7591 metadata as clients send it: `scope` is one space-separated string (an array
is accepted too). A client asking for `token_endpoint_auth_method: "none"` is registered as a public
client, with no secret, relying on PKCE, which `/authorize` requires of every client. Any other request
gets `client_secret_post` and a secret, returned once with `client_secret_expires_at: 0`.

#### Keeping the token store small

The store is one bbolt file. The service sweeps expired records every ten minutes, but bbolt only
reuses the pages that frees: the file never shrinks. Since `/register` and `/authorize` answer
anyone, a burst of requests can leave the file large for good. `AuthService.CompactStore` gives
the space back: it sweeps, copies the live data into a new file beside the store, and renames it
over the old one.

- Writes (sign-ins, token refreshes) wait for the whole run; reads, which is every authenticated
  `/mcp` request, wait only for the final close, rename and reopen.
- The rename is atomic: a crash leaves the complete old file or the complete new one.
- The copy needs free space for the live data on the same volume. With none it fails and the
  store is left as it was.

The service never runs it on its own. Call it on a timer, on a signal, or both:

```go
go func() {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1) // kill -USR1 <pid> compacts now
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-usr1:
		}
		res, err := authService.CompactStore(ctx)
		if err != nil {
			logging.Error("auth store compaction failed", "error", err)
			continue
		}
		logging.Info("auth store compacted", "swept", res.Swept,
			"bytes_before", res.BytesBefore, "bytes_after", res.BytesAfter, "took", res.Duration)
	}
}()
```


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

`HTTPTransport.Start` owns its own `http.ServeMux` and `http.Server`.
`HTTPTransportConfig.ExtraRoutes func(*http.ServeMux)` is the seam for endpoints that need that
listener too — health and readiness probes, metrics. It runs before `/mcp` and the auth routes, so a
colliding pattern panics at startup rather than shadowing the protocol endpoint, and the routes it
adds sit outside the auth middleware. Leave it nil and routing is exactly what it was before the
field existed. An application that runs its own server instead mounts
`HTTPTransport.Handler(handler)` at `/mcp` on its own mux (no `Start`; it registers the auth routes
itself). See [HTTP-TRANSPORT.md](HTTP-TRANSPORT.md#mounting-mcp-on-your-own-server).

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
registry.RegisterWithOptions(adminTool, adminFn, mcp.RequireRoles("admin")) // see "Roles and the Go API"
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

### Skills (beta)

**Status: opt-in, Beta.** This implements the [Skills
extension](https://modelcontextprotocol.io/extensions/skills/overview)
(`io.modelcontextprotocol/skills`), following
[`specification/beta/skills.mdx`](https://github.com/modelcontextprotocol/ext-skills/blob/main/specification/beta/skills.mdx)
as read on 2026-11-25. SEP-2640 is Final; the extension itself is at stage Beta, which means real
use is welcome and a breaking change is possible but expected to be rare. It is an extension, not
part of the 2026-07-28 core specification.

Hosts consume it now. Claude Code (v2.1.331 or later) and claude.ai connectors load skills from MCP
servers in public beta, and VS Code does too. The example server passes the skills scenarios of the
official [conformance suite](https://github.com/modelcontextprotocol/conformance)
(`@modelcontextprotocol/conformance@alpha`) as a 2026-07-28 client and as a 2025-11-25 client
through the legacy overlay, and the MCP Inspector's `--verify` accepts its digests, sizes and
frontmatter. **Nobody has loaded a skill from this library into Claude itself yet**: treat that
path as untested until you have done it.

A *skill* is a directory holding a `SKILL.md` with YAML frontmatter, plus any supporting files.
Serving one over MCP lets a host load a procedure written against **your** tools rather than
generic advice. The library's job is the catalog: publishing each skill's frontmatter verbatim
along with a sha256 digest and byte size per file, and serving those files as ordinary resources.

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

`LoadFS` walks the filesystem, parses and validates frontmatter, computes digests and sizes over
raw bytes, and registers every file as a readable resource. That single call is the point of the
API. A host refuses a skill whose file does not match its manifest entry, or whose `SKILL.md`
frontmatter differs from the entry's, so a hand-maintained copy of either is a skill that silently
stops loading. `SkillRegistry` derives all three from the same bytes `resources/read` returns and
offers no way to supply a digest or a size.

Each entry on the wire:

```json
{
  "uri": "skill://pdf-processing/SKILL.md",
  "frontmatter": { "name": "pdf-processing", "description": "Extract, fill, and assemble PDF documents" },
  "resources": [
    { "uri": "skill://pdf-processing/SKILL.md", "digest": "sha256:99b7…b19e", "size": 151 },
    { "uri": "skill://pdf-processing/templates/invoice.md", "digest": "sha256:61f4…d9ee", "size": 29 }
  ]
}
```

`resources` is always present: the complete manifest, or the string `"dynamic"` for a skill whose
content is generated (below).

Registering a skill (or a whole `LoadFS`) fires the ordinary
`notifications/resources/list_changed`, once per skill rather than once per file. The extension
defines no skills-specific notification, so none is invented here.

**Validation**, applied at registration:

| What | Rule |
|---|---|
| `SKILL.md` | begins with a `---` line, YAML frontmatter, and a closing `---` line |
| `name` | required, 1–64 chars, `[a-z0-9]` in single-hyphen-separated groups, and equal to the skill directory's name |
| `description` | required, non-empty, ≤ 1024 chars |
| `compatibility` | optional, ≤ 500 chars |
| every other frontmatter key | passed through verbatim and uninterpreted |
| size | at most 512 files and 16 MiB per skill (`MaxSkillResources`, `MaxSkillBytes`); `AllowOversizedSkills(true)` lifts it |

`allowed-tools` is deliberately in the pass-through row. It is a host-side consent matter that
hosts MUST ignore for MCP-origin skills absent explicit per-skill approval, so this library must
not act on it.

The two size limits are what every conforming host must accept; a larger skill may load nowhere,
which is why registration refuses it by default.

**Frontmatter is converted the way a YAML 1.2 parser reads it**, because the host parses the same
file and compares. An unquoted `2026-11-25` stays the string `"2026-11-25"`, `014` is 14, `yes` is
a string, and `<<` is an ordinary key. Hosts may still differ on unusual scalars, so quote any
value that is not plainly a string, a number or `true`/`false`. `SkillDef.Frontmatter` is optional
when the skill has files; if you set it, it must equal the file's frontmatter or `Register` fails.

Things worth knowing:

- **The `skill://` scheme is not privileged.** A server MAY serve skills under any scheme native to
  its domain (`github://owner/repo/skills/refunds/SKILL.md`), and everything works identically.
- **Skill-ness is established only by `skills/list` and `skills/get`.** Registering a `skill://`
  resource directly on the `ResourceRegistry` does *not* make it a skill, and this library never
  scans for a URI shape to invent one. A server that used to publish skills as plain `skill://`
  resources, or through a `skill://index.json`, is invisible to a host until it registers them here.
- **The last path segment must equal the frontmatter `name`.** `skill://acme/billing/refunds/SKILL.md`
  needs `name: refunds`; the segments before it are a free organizational prefix.
- **Declaring the extension declares `resources`.** A configured `SkillRegistry`, even an empty
  one, puts both in `server/discover`, as the specification requires.
- **Nested skills publish flat.** A skill directory may contain further skills; the nested skill's
  files also appear in the enclosing skill's `resources` list, and the nested skill is listed as an
  ordinary top-level entry. Unregistering one leaves files the other still publishes readable.
- **Generated skills are `Dynamic`.** `Register(SkillDef{URI, Frontmatter, Dynamic: true})` lists an
  entry with `"resources": "dynamic"` and registers no files: you serve the `SKILL.md` and the rest
  yourself, usually with a `ResourceTemplate`. Such a skill has no content integrity and hosts may
  decline it, so prefer a manifest whenever the bytes are stable.
- **`skills/get` answers for URIs `skills/list` never returned.** Attach a `SkillResolver` for a
  catalog too large to enumerate; it is consulted only when a registered lookup misses. It returns
  the entry itself, with a full `Resources` manifest (digest and size per file) or `Dynamic`, and
  the entry is checked before it is sent: an invalid one is logged and answered `-32603`.
- **`ttlMs` and `cacheScope` are on all three results**, as the specification requires. `skills/get`
  uses the list TTL (`ListTTLMs`). Neither is an integrity property.

**What Claude does with a skill** (from [Claude Code's
documentation](https://code.claude.com/docs/en/mcp#use-skills-from-mcp-servers), read 2026-11-25;
check it before relying on any of this):

- The skill appears as `<server>:<skill>`, for example `/docs:pdf`; a connector's server name gets a
  prefix (`claude_ai_Docs:pdf`).
- At most 100 skills per server are shown.
- These frontmatter fields are ignored for an MCP skill: `allowed-tools`, `disallowed-tools`,
  `hooks`, `model`, `effort`, `context`, `agent`, `background`, `shell`, `paths`, `metadata`.
  `!` commands do not run and `@` references are not attached.
- Skills from connectors that Anthropic scans must fit 100 KB per text file and 400 KB per skill,
  and a skill with an archive, or a binary file such as WebP or OTF, does not load. PNG, JPEG, GIF,
  SVG, PDF and several font and media formats are neither scanned nor counted.
- Not available in `claude -p`, the Agent SDK, cloud sessions or GitHub Actions.

`resources/directory/read` is a separate opt-in (`SkillsDirectoryRead`) reported as the extension's
`directoryRead` setting. It lists the direct children of a directory in the resource namespace,
derived lexically from registered resource URIs — nothing to keep in step, and it is
scheme-agnostic rather than skill-exclusive. A directory with no registered descendants is
indistinguishable from one that never existed, and both answer `-32602`. Template-backed files are
not enumerable, so do not turn it on for a namespace you serve only through a `ResourceTemplate`.

With `ServerConfig.Skills` left nil there is no `extensions` key in `server/discover` and all three
methods answer `-32601` — the same probe clients use to detect an unimplemented extension.

**Upgrading from v0.10.x** (which followed an August 2026 draft):

- Every manifest entry gains `size`, and `resources` is never omitted. No code change is needed for
  skills registered through `LoadFS` or `Register`.
- A `SkillResolver` must now return a complete `Resources` manifest with `Size` set, or
  `Dynamic: true`. An entry with neither used to go out without `resources`; it is now `-32603`.
- `Register` parses the frontmatter from the `SKILL.md` bytes. A `SKILL.md` without frontmatter, or
  a `SkillDef.Frontmatter` that differs from the file's, is now an error.
- A skill over 512 files or 16 MiB is refused unless `AllowOversizedSkills(true)`.
- Frontmatter values that YAML 1.1 would have typed (an unquoted date, `014`) now reach the wire as
  a YAML 1.2 parser reads them.

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
`tools/list` switches to `"private"` on its own while `RolesFromContext` is set and any tool is
gated, since its contents then depend on the caller.

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
