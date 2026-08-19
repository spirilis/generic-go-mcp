# PLAN: Parameterized Resources Engine (Resource Templates)

## Context

A downstream consumer (`~/src/mcp-kube-baker`, a Kubernetes MCP server) needs
parameterized resources like `mcp+kubectl://{context}/pod/{namespace}/{name}` —
an unbounded keyspace that can never be enumerated by `resources/list`. The MCP
2026-07-28 spec covers this with **resource templates**: the server advertises
RFC 6570 URI templates via `resources/templates/list`, the client expands them
locally, and calls plain `resources/read` with a concrete URI.

This library today has no template support: `ResourceRegistry` (`mcp/resources.go`)
is exact-match concrete URIs only, `handleResourcesRead` rejects any unregistered
URI, and the method router (`mcp/server.go`, `route()` switch) has no
`resources/templates/list` case. The only mention of templates is a comment in
`mcp/result.go:53`.

**Goal:** implement resource templates end-to-end, spec-compliant for 2026-07-28,
with legacy (`compat`) pass-through. Target release: **v0.7.0**.

Spec references:
- <https://modelcontextprotocol.io/specification/2026-07-28/server/resources>
- RFC 6570 — <https://datatracker.ietf.org/doc/html/rfc6570>

## Spec requirements (compliance checklist)

- [ ] `resources/templates/list` returns `resourceTemplates: []` entries of
      `{uriTemplate, name, title?, description?, mimeType?}` with pagination
      (`cursor`/`nextCursor`), plus the standard 2026-07-28 result envelope:
      `resultType: "complete"`, `ttlMs`, `cacheScope` (`CacheableResult`).
- [ ] `resources/read` is unchanged on the wire — concrete URI only. The server
      matches it against registered templates when no exact resource matches.
- [ ] No-match → JSON-RPC error `-32602` ("Unknown resource"), **never** an empty
      `contents: []`.
- [ ] There is **no** "templates supported" capability flag —
      `capabilities.resources` has only `listChanged`/`subscribe`. Clients probe
      `resources/templates/list` and treat `-32601` as unsupported. So: the
      `resources` capability must be advertised when templates exist even with
      zero concrete resources, and `resources/templates/list` must answer
      (with an empty array) whenever the capability is advertised.
- [ ] Template set MUST NOT vary per-connection or as a side effect of another
      request (registry mutation by the embedder is fine — that's what
      `list_changed` is for; it MAY vary by request authorization).
- [ ] `completion/complete` for narrowing template variables is a separate
      capability (`completions`) — **out of scope** for this release; see Future
      work.

## Design

### 1. New types (new file `mcp/templates.go`)

```go
// ResourceTemplate mirrors the spec's resourceTemplates entry.
type ResourceTemplate struct {
	URITemplate string       `json:"uriTemplate"`
	Name        string       `json:"name"`
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	MimeType    string       `json:"mimeType,omitempty"`
	Annotations *Annotations `json:"annotations,omitempty"`
}

// ResourceReadRequest is what a template handler receives on resources/read.
type ResourceReadRequest struct {
	URI  string            // the concrete URI the client sent
	Vars map[string]string // template variables extracted from URI, percent-decoded
}

type ResourceTemplateFunction func(ctx context.Context, req *ResourceReadRequest) (ResourceContentResult, error)
```

Existing concrete-resource API (`Resource`/`ResourceFunction`) is untouched.

### 2. Registry API (extend `ResourceRegistry` in `mcp/resources.go`)

```go
// RegisterTemplate compiles and registers a template. Returns an error for a
// malformed or unsupported template (see matcher below) — unlike Register,
// which cannot fail. Re-registering an existing uriTemplate replaces it and
// moves it to the end of the list (same semantics as tools/resources).
func (r *ResourceRegistry) RegisterTemplate(tmpl ResourceTemplate, fn ResourceTemplateFunction) error

// UnregisterTemplate removes by exact uriTemplate string; reports whether it
// was present.
func (r *ResourceRegistry) UnregisterTemplate(uriTemplate string) bool

func (r *ResourceRegistry) ListTemplates() []ResourceTemplate
func (r *ResourceRegistry) HasTemplates() bool
```

- Same `sync.RWMutex` discipline as existing methods; safe to mutate after the
  server has started.
- Mutations that change the catalog fire `notifications/resources/list_changed`
  through the same hook `Register`/`Unregister` use today (the spec has no
  separate template-changed notification; `list_changed` covers both). Follow
  the existing "no actual change → no notification" rule.

### 3. URI template matcher (the crux)

RFC 6570 defines **expansion only** (template → URI). Reverse matching
(URI → variable bindings) is not in the RFC and must be implemented here. The
`mcp` + `transport` packages are deliberately **pure standard library** — keep
that guarantee: hand-roll the matcher, do not import `yosida95/uritemplate`.

Supported forms (reject everything else at `RegisterTemplate` time with a
descriptive error):

| Form | Semantics for matching |
|---|---|
| `{var}` (Level 1) | matches one path segment: one or more chars, **never** `/`; percent-decode the matched text into `Vars` |
| `{+var}` (reserved expansion) | may span `/`; only allowed as the **final** expression in the template |

Unsupported (registration error): `{#var}`, `{?a,b}`, `{&a}`, `{/seg*}`,
`{a,b}` multi-variable expressions, prefix/explode modifiers (`{var:3}`,
`{var*}`).

Implementation: at registration, translate the template into an anchored
`*regexp.Regexp` — literal spans are `regexp.QuoteMeta`'d, `{var}` becomes
`([^/]+)`, `{+var}` becomes `(.+)` — plus an ordered list of variable names.
Validate: balanced braces, valid varnames (`[A-Za-z0-9_]+` per RFC varchar,
`.` optional — keep it strict), no duplicate variable names, `{+var}` terminal
only. Store compiled form alongside the `ResourceTemplate`.

Matching at read time: percent-decode each captured group with
`url.PathUnescape`; a group that fails to decode → no match (fall through).

### 4. Read dispatch (`handleResourcesRead`, `mcp/resources.go:222`)

Order:
1. Exact-match concrete resource (existing `Get`/`Read` path) — concrete always
   wins over templates.
2. Templates in **registration order, first-match-wins**. Document the
   shadowing gotcha in the doc comment: register more-specific templates first
   (`scheme://x/{a}/detail` before `scheme://x/{a}` if both exist).
3. No match → existing `invalidParamsErr("Unknown resource: %s", uri)` (`-32602`).

For a template hit, build the `ResourcesReadResult` the same way the concrete
path does: `contents[0].uri` = the **concrete** request URI, `name`/`title`
from the template, `mimeType` precedence = per-read `ResourceContentResult.MimeType`
override → template `MimeType` → `"text/plain"`. Handler `error` → `internalErr`
(`-32603`), same as concrete resources.

Add `ResourceRegistry.MatchTemplate(uri string) (ResourceTemplate, ResourceTemplateFunction, map[string]string, bool)`
(or fold into a `ReadAny`) so the handler stays thin.

### 5. New handler: `resources/templates/list`

- New case in the `route()` switch (`mcp/server.go`, next to `resources/list`).
- Mirror `handleResourcesList` (`mcp/resources.go:195`): optional `cursor`
  param, `paginate(...)` with `defaultPageSize`, result:

```go
type ResourcesTemplatesListResult struct {
	CacheableResult
	ResourceTemplates []ResourceTemplate `json:"resourceTemplates"`
	NextCursor        string             `json:"nextCursor,omitempty"`
}
```

- `CacheableResult` from `NewCacheableResult(s.listTTLMs, s.cacheScope())`;
  `resultType`/`serverInfo` are stamped by the router like every other result.

### 6. Capability advertisement

`Server.capabilities()` (`mcp/server.go:160`) currently gates the `resources`
capability on `s.resourceRegistry.HasResources()`. Change to
`HasResources() || HasTemplates()`. Verify the same predicate isn't duplicated
elsewhere (subscriptions gating, `server/discover`).

### 7. Legacy compat (`compat/`)

Resource templates also exist in 2025-11-25 and earlier, so the overlay should
serve them to legacy clients:

- Route legacy `resources/templates/list` through to the modern handler (check
  how `compat/overlay.go` routes `resources/list` and do the same).
- `downgradeResult` (`compat/downgrade.go:56`) must strip
  `resultType`/`ttlMs`/`cacheScope` from the new result type — verify it's
  generic (key-stripping on raw JSON) or extend it.
- Error-code note: legacy resource-not-found is `-32002`, modern is `-32602`.
  Check whether the overlay already translates this for `resources/read`; if it
  doesn't translate today, don't add translation just for this feature — keep
  behavior consistent with existing reads and note it in LEGACY-COMPAT.md.
- Add a compat test: legacy-era request → templates listed, envelope fields
  stripped.

### 8. Out of scope / future work (note in README)

- `completion/complete` + the `completions` capability for narrowing template
  variables (the spec's mechanism for exploring large keyspaces).
- Query-string operators (`{?var}`) and other RFC 6570 levels.
- MRTR (`InputRequiredResult`) from `resources/read` for missing variables.

## Tests

- **Matcher** (`mcp/templates_test.go`): compile/match table tests —
  percent-encoding round-trips, `{var}` refuses `/`, `{+var}` spans segments,
  `{+var}` non-terminal rejected at registration, unsupported operators
  rejected, duplicate varnames rejected, registration-order shadowing,
  concrete-resource-beats-template.
- **Handlers**: `resources/templates/list` empty vs populated vs paginated
  (reuse `paginate` test patterns); `resources/read` dispatch precedence;
  no-match → `-32602`; handler error → `-32603`; template registered after
  start fires `list_changed` (reuse subscriptions test harness in
  `mcp/conformance_test.go` / `mcp/subscriptions.go` tests).
- **Capability**: templates-only registry advertises `resources` in
  `server/discover`.
- **Compat**: legacy `resources/templates/list` works and is downgraded.

## Docs to update (same commit series)

- `README.md` — templates section.
- `.claude/skills/generic-go-mcp-consumption/references/mrtr-and-resources.md`
  — add a "Resource templates" section with a compiling example; per the
  skill's accuracy note, verify every snippet against the real source and
  refresh the accuracy note's commit hash.
- `examples/go-mcp/main.go` — register one template (e.g.
  `mcp+unix:///env/{name}` returning an environment variable) so the reference
  server exercises the feature.
- `LEGACY-COMPAT.md` — templates pass-through + error-code note.

## Release

Tag **v0.7.0** when done. Downstream `mcp-kube-baker` is developing against a
`replace => ../generic-go-mcp` directive and will pick this up immediately;
it drops the replace once the tag exists.
