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

The blob is HMAC-signed with `ServerConfig.RequestStateKey` (`mcp/mrtr.go`) over: the calling
principal (via `ServerConfig.PrincipalFromContext`), an expiry (5 minutes, `requestStateTTL`), and a
digest of the exact tool name + arguments it was issued for. `req.NeedInput`/the router reject a
retry whose `requestState` is tampered with, expired, issued for a different principal, or issued
for a different tool call — it's not just an opaque token you can round-trip blindly; it's actually
verified server-side on the retry (`s.verifyRequestState` in `mcp/mrtr.go`, called from
`handleToolsCall` in `mcp/tools.go`).

If you never set `ServerConfig.PrincipalFromContext`, every caller is treated as the same empty
principal — fine for an unauthenticated server, but means an MRTR confirmation from user A's
session could technically be replayed by user B if `requestState` leaked between them. Wire it to
`auth.GetUserFromContext` if you're running with OAuth (see `transports-and-auth.md`).

`RequestStateKey` matters just as much and is easier to overlook: leave it empty and `NewServer`
generates a random key at startup, so **every retry issued before a restart fails verification after
one**, and behind a load balancer a retry that lands on a different replica fails immediately. Set it
explicitly to a stable shared secret for any multi-replica or restart-tolerant HTTP deployment — see
`transports-and-auth.md`.

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

Everything else about mutating a live catalog — `Unregister`, what happens when you re-register an
existing URI, announcing that a resource's *content* changed (`NotifyUpdated`), and how a client
subscribes to any of it via `subscriptions/listen` — is in
`notifications-and-registries.md`.

### Saying "no such resource"

Return `mcp.ErrResourceNotFound` (or wrap it with `%w`) and the client gets `-32602
"Unknown resource: <uri>"` — the same answer an unregistered URI gets. Any other error is `-32603`,
which tells the client the *server* is broken, so don't use it for something merely absent:

```go
return mcp.ResourceContentResult{}, fmt.Errorf("pod %q: %w", name, mcp.ErrResourceNotFound)
```

Never return empty content to mean "not found" — it is indistinguishable from a resource that
legitimately has none.

## Resource templates (parameterized resources)

Use a template when the keyspace can't be enumerated: every pod in a cluster, every row in a table,
every environment variable. `resources/list` is the wrong shape for those, so the server advertises
the *shape* via `resources/templates/list`, the client expands it locally, and the read arrives on
ordinary `resources/read` carrying a concrete URI.

```go
err := resources.RegisterTemplate(
	mcp.ResourceTemplate{
		URITemplate: "mcp+kubectl://{context}/pod/{namespace}/{name}",
		Name:        "Pod",
		Description: "One pod, as JSON",
		MimeType:    "application/json",
	},
	func(ctx context.Context, req *mcp.ResourceReadRequest) (mcp.ResourceContentResult, error) {
		// req.URI is the concrete URI as sent; req.Vars holds the percent-decoded bindings.
		pod, err := lookupPod(ctx, req.Vars["context"], req.Vars["namespace"], req.Vars["name"])
		if errors.Is(err, errNoSuchPod) {
			return mcp.ResourceContentResult{}, fmt.Errorf("pod %q: %w", req.Vars["name"], mcp.ErrResourceNotFound)
		}
		if err != nil {
			return mcp.ResourceContentResult{}, err
		}
		return mcp.ResourceContentResult{Text: pod}, nil
	},
)
if err != nil {
	log.Fatalf("bad resource template: %v", err) // compiled at registration, so this is a startup bug
}
```

`RegisterTemplate` returns an `error` where `Register` cannot fail: the template is compiled into a
matcher up front, so a malformed one is caught at startup rather than silently never matching.

**Only two RFC 6570 forms are supported.** Reverse matching (URI → bindings) isn't in the RFC at all,
so this library implements a deliberately small subset:

| Form | Matches | Notes |
|---|---|---|
| `{var}` | one path segment, never `/` | Level 1 — all many clients implement |
| `{+var}` | one or more chars, `/` included | Reserved expansion; **final expression only** |

`{#frag}`, `{?query}`, `{&param}`, `{/path}`, `{.ext}`, `{;param}`, `{a,b}`, `{var:3}`, `{var*}` are
all registration errors naming the offending expression.

Gotchas that will bite you:

- **Concrete resources beat templates**, always.
- **Templates match in registration order, first match wins** — register the specific one first, or
  `scheme://x/{a}` shadows `scheme://x/{a}/detail`.
- **`{var}` won't cross a `/`.** For multi-segment values use `{+var}`; `file:///{path}` only appears
  to work because clients are lax about encoding.
- Registering/unregistering a template fires `notifications/resources/list_changed` — there is no
  separate template-changed notification.
- A registry holding **only** templates still advertises the `resources` capability.
- `NotifyUpdated` works for a URI that matches a template, even though it was never registered
  individually.

Sibling API: `UnregisterTemplate`, `ListTemplates`, `HasTemplates`, `MatchTemplate`.

### Optional: completion for template variables

A template describes shape, not contents, so `completion/complete` is the only protocol mechanism
for exploring a large variable domain. It's opt-in — attach a provider and the server declares the
`completions` capability; attach none and the method returns `-32601`, which is exactly the probe
clients use to detect support.

```go
resources.SetTemplateCompleter("mcp+kubectl://{context}/pod/{namespace}/{name}",
	mcp.CompletionFunc(func(ctx context.Context, req *mcp.CompletionRequest) (mcp.CompletionResult, error) {
		// req.Argument = variable being completed, req.Value = partial input,
		// req.Context = variables the client already resolved (use it to narrow the query).
		names, total := searchPodNames(ctx, req.Context["namespace"], req.Value)
		return mcp.CompletionResult{Values: names, Total: &total}, nil
	}))
```

Back it with a prefix index or a paged upstream query — never materialize the keyspace, since not
fitting in memory is the whole reason the template exists. The server truncates `Values` to the
spec's 100-value ceiling and sets `hasMore` if it had to. `SetTemplateCompleter` errors if no
template is registered under that exact `uriTemplate` string.

Note that most hosts today will not exercise templates: resources are *application*-driven (the host
decides how to surface them), while tools are *model*-driven. If the model needs to look something
up mid-reasoning, give it a **tool** — implement templates for the linkable, citable surface, not as
a replacement for a lookup tool.

## Skills over MCP (experimental, SEP-2640)

**Read this first.** [SEP-2640](https://github.com/modelcontextprotocol/modelcontextprotocol/pull/2640)
is an Extensions Track proposal that is **open and not merged** — this library implements it against
head `641d1eb` (2026-08-20). Nothing about skills is in the ratified 2026-07-28 specification, and
no public client consumes it yet (Claude Code's skills are local-only; the SEP says Anthropic's
support is prototyped internally). Everything below may change with the SEP. Every exported symbol
carries an `EXPERIMENTAL:` doc comment naming the SEP and that commit.

If what you actually want today is "let the model pull in a procedure mid-reasoning", write a
**tool** whose description carries the catalog — that works with every client right now. The skills
extension is the complementary thing: it lets a *host* deliberately load a skill.

### What you serve

A skill is a directory with a `SKILL.md` at its root ([Agent Skills
specification](https://agentskills.io/specification)) plus supporting files. The library publishes
each skill's frontmatter verbatim along with a `sha256` digest per file, and serves the files as
ordinary resources.

```go
//go:embed all:skills
var skillFS embed.FS

resources := mcp.NewResourceRegistry()
skills := mcp.NewSkillRegistry(resources) // a skill's files ARE resources

content, err := fs.Sub(skillFS, "skills") // keep "skills/" out of every URI
if err != nil {
	log.Fatal(err)
}
if err := skills.LoadFS(content, "skill://"); err != nil {
	log.Fatal(err) // bad frontmatter, a name that doesn't match its directory, ...
}

server := mcp.NewServer(tools, resources, &mcp.ServerConfig{
	Skills:              skills, // nil (the default) means the extension does not exist
	SkillsDirectoryRead: true,   // opt into resources/directory/read
})
```

That is the whole integration. `LoadFS` walks the filesystem, parses and validates frontmatter,
computes digests over raw bytes, and registers every file as a readable resource.

**Never hand-maintain a digest**, and note that the API gives you no way to: `SkillRegistry` derives
every digest from the same bytes `resources/read` returns. `Register(mcp.SkillDef{...})` is the
non-filesystem path and takes `Files map[string]mcp.SkillContent` — bytes in, digests out.

### The rules that bite

- **The last path segment must equal the frontmatter `name`.** `skill://acme/billing/refunds/SKILL.md`
  needs `name: refunds`. Segments before it are a free organizational prefix.
- **`name` is `[a-z0-9]` in single-hyphen-separated groups, 1–64 chars**, and must match the
  directory name. `description` is required, non-empty, ≤ 1024 chars. `compatibility` ≤ 500.
  Everything else in the frontmatter is passed through **verbatim and uninterpreted** — including
  `allowed-tools`, which is a host consent matter the SEP says hosts MUST ignore for MCP-origin
  skills. Do not build server behavior on it.
- **The `skill://` scheme is not privileged.** `github://owner/repo/skills/refunds/SKILL.md` works
  identically. Correspondingly, registering a `skill://` resource straight on the `ResourceRegistry`
  does **not** make it a skill — skill-ness comes only from `skills/list` / `skills/get`.
- **Nested skills publish flat.** A skill directory may contain further skills; the nested skill's
  files appear in the enclosing skill's `resources` list *and* the nested skill is listed as an
  ordinary top-level entry. Unregistering one leaves files the other still publishes readable.
- **Registration fires the ordinary `notifications/resources/list_changed`** — once per skill, not
  once per file. SEP-2640 defines no skills-specific notification, so a client that cached
  `skills/list` has no way to learn a skill's frontmatter changed while its file set did not.

### Catalogs too big to list

`skills/get` must answer for skills `skills/list` never returned. Attach a resolver, consulted only
when a registered lookup misses:

```go
skills.SetResolver(func(ctx context.Context, uri string) (mcp.Skill, bool, error) {
	fm, files, ok := generateSkill(ctx, uri)
	if !ok {
		return mcp.Skill{}, false, nil // -> -32602, not a fabricated entry
	}
	return mcp.Skill{URI: uri, Frontmatter: fm, Resources: files}, true, nil
})
```

A resolver returns the wire shape directly, so **it owns digest correctness**, and nothing is
registered on the `ResourceRegistry` on its behalf — pair it with a `ResourceTemplate` over the same
URI namespace or the files will not be readable. Omitting `Resources` entirely is legal for a
dynamically generated skill (hosts MAY decline to load one).

### `resources/directory/read`

A second opt-in (`SkillsDirectoryRead`), reported as the extension's `directoryRead` setting. It
lists the *direct* children of a directory in the resource namespace, marking subdirectories
`inode/directory`. Children are derived lexically from registered resource URIs, so there is nothing
to keep in step and it is scheme-agnostic rather than skill-only. A directory with no registered
descendants is indistinguishable from one that never existed — both `-32602`. It paginates like
`resources/list`.

### Checking your work by hand

```bash
# the extension shows up only when ServerConfig.Skills is non-nil
... server/discover | jq '.result.capabilities.extensions'
# {"io.modelcontextprotocol/skills":{"directoryRead":true}}

# the invariant worth automating: published digest == sha256 of what resources/read returns
... skills/list | jq -r '.result.skills[].resources[] | .uri'
```

With `ServerConfig.Skills` left `nil`, `server/discover` has no `extensions` key and all three
methods answer `-32601` — the probe a client uses to detect an unimplemented extension.
