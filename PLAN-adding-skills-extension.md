# PLAN: SEP-2640 Skills Extension (`io.modelcontextprotocol/skills`)

## Context

A downstream consumer (`~/src/mcp-kube-baker`, a Kubernetes MCP server) wants to
ship Kubernetes-troubleshooting *skills* — procedures written against its own
tool names (`kubectl_get_pods` → the pod resource → `kubectl_get_pod_logs` with
`previous=true` → `kubectl_get_events`) rather than generic `kubectl` advice a
read-only server cannot execute. The content is static, embedded, and immutable
per build: the easy case, and a good first consumer.

Four things made this un-implementable from outside package `mcp`:

- `(*Server).HandleMessage`'s dispatch switch (`mcp/server.go:234`) is closed —
  no registration hook, no fallback — so `skills/list`, `skills/get`, and
  `resources/directory/read` all fell to `default:` → `-32601`.
- `ServerCapabilities.Extensions` exists (`mcp/capabilities.go:34`) but
  `capabilities()` never populated it and `ServerConfig` had no field reaching it.
- `ResourceContentResult` (`mcp/resources.go:28`) carried only `Text`/`Blob`/
  `MimeType`, while the wire type `ResourceContent` declares `_meta` and
  `annotations` — unreachable by composition, since the builder is private.

Wrapping the server from outside (as `compat.Overlay` does) was prototyped and
rejected: it means unmarshalling `server/discover` results to splice in a
capability, duplicating pagination and `CacheableResult` construction, and
re-implementing it in every consumer.

**Goal:** implement the skills extension end-to-end as an explicitly
experimental, **opt-in** surface, with legacy (`compat`) pass-through. Target
release: **v0.8.0**.

**Status of the target spec.** [SEP-2640](https://github.com/modelcontextprotocol/modelcontextprotocol/pull/2640)
is Extensions Track, **open and not merged**; written against head `641d1eb`,
branch `sep/skills-extension`, `updated_at 2026-08-20T13:55:46Z`. Nothing about
skills appears in the ratified 2026-07-28 spec pages or changelog, and no public
client consumes it yet. That framing is load-bearing in the API design (see §8).

Skill *format* is out of scope for both the SEP and this work: SEP-2640 delegates
it to the [Agent Skills specification](https://agentskills.io/specification) and
states it "does not redefine, constrain, or extend the skill format."

Spec references:
- SEP-2640 source: `seps/2640-skills-extension.md` on the branch above
- WG scratch repo: <https://github.com/modelcontextprotocol/experimental-ext-skills>
- Agent Skills: <https://agentskills.io/specification>

## Spec requirements (compliance checklist)

- [x] Capability declared as
      `capabilities.extensions["io.modelcontextprotocol/skills"] = {"directoryRead": <bool>}`,
      `directoryRead` defaulting to `false`. The SEP says "initialize response"
      because it was drafted pre-2026-07-28; under this revision it belongs in
      `server/discover`.
- [x] `skills/list` — MUST, once the extension is declared. Entries of
      `{uri, frontmatter, resources[]}`, paginated (`cursor`/`nextCursor`), with
      the 2026-07-28 envelope (`resultType`, `ttlMs`, `cacheScope`). MAY return
      empty or partial listings; an entry is never split across pages.
- [x] `skills/get` — MUST. Params `{uri}`; result is the entry nested under a
      **`skill`** key. MUST answer for skills absent from the listing. A URI that
      is not a skill this server serves → `-32602`.
- [x] `resources/directory/read` — optional, gated behind `directoryRead`.
      Direct children only, subdirectories marked `mimeType: "inode/directory"`,
      paginated. Absent or non-directory URI → `-32602`. Scheme-agnostic.
- [x] `frontmatter` is the `SKILL.md` YAML **verbatim** as JSON — unknown keys
      survive (`license`, `metadata`, `compatibility`, `allowed-tools`, and host
      supersets).
- [x] `resources[].digest` is `sha256:` + 64 lowercase hex over the file's raw
      bytes and MUST match what `resources/read` returns for that URI.
      `resources` MAY be omitted only for a dynamically generated skill.
- [x] The final segment of the skill path MUST equal `frontmatter.name`.
- [x] `skill://` is a **SHOULD, not a MUST** — "a server MAY serve skills under
      another scheme native to its domain… No scheme is privileged."
- [x] "A host MUST NOT conclude that a resource is a skill merely because its URI
      carries a particular scheme." The library must never synthesize skill
      entries by scanning the resource registry.
- [x] Nested skills: a skill directory MAY contain descendant skills; their files
      also appear in the enclosing skill's `resources`, and publication is flat.
- [x] `SKILL.md` resource metadata SHOULD carry `mimeType: text/markdown` and
      take `name`/`description` from the frontmatter.
- [x] `allowed-tools` and other permission-widening frontmatter MUST NOT be acted
      on by the server.

## Design

### 1. New file `mcp/skills.go` — types and registry

`Skill` / `SkillResource` are the wire shapes, produced by the registry and never
assembled by a consumer. `SkillDef` (`URI`, `Frontmatter`, `Files map[string]SkillContent`)
is the input side.

**Digests are derived, never supplied.** This deviates from the original ask,
which had the caller populate `Skill.Resources[].Digest` and the library validate
its format. Deriving makes the invariant most likely to rot — published digest vs.
served bytes — structurally impossible to violate rather than merely detectable.

`SkillRegistry` holds the ordered skills plus `owns` (skill URI → file URIs) and
`refs` (file URI → owning-skill count). The refcount exists for nested skills,
where two skills legitimately publish the same file: unregistering one must not
orphan the other's content.

`NewSkillRegistry(rr *ResourceRegistry)` takes the resource registry because a
skill's files *are* resources; registering a skill registers its files, so
`skills/list` and `resources/read` can never disagree about what exists.

Validation at registration (stdlib `regexp`, per Agent Skills + the SEP's path
rule): `name` 1–64 chars matching `^[a-z0-9]+(-[a-z0-9]+)*$` (which forbids a
leading/trailing hyphen and `--` in one expression) and equal to the skill root's
final segment; `description` required, non-empty, ≤1024; `compatibility` ≤500;
the frontmatter must round-trip through `json.Marshal`. Everything else passes
through uninterpreted.

Content handling: a deliberately hand-rolled extension→mime table rather than
`mime.TypeByExtension`, whose answers come from the host's `mime.types` file — a
skill's catalog must not depend on which machine serves it. Bytes go out as
`Text` when valid NUL-free UTF-8, else as a base64 `Blob`; either way the client
digests exactly the bytes that produced the published digest.

### 2. `(*SkillRegistry).LoadFS(fsys fs.FS, uriPrefix string) error`

The reason a consumer reaches for this API: one call turns an `embed.FS` of skill
directories into a compliant `skills/list` plus readable resources, with no digest
bookkeeping. `fs.FS` rather than `embed.FS` keeps `os.DirFS` and `testing/fstest`
working for free.

Walk rules: a directory is a skill iff it directly contains `SKILL.md`; a skill's
files are every regular file at or below it (including a nested skill's, which is
also registered as its own flat entry); a directory with no `SKILL.md` outside any
skill is an organizational prefix, but a *file* there is an error since it would
belong to no skill; dot-prefixed entries are skipped; zero skills is an error.
`fs.WalkDir`'s lexical order makes output deterministic.

**Dependency decision:** frontmatter parsing needs YAML, so `mcp` now imports
`gopkg.in/yaml.v3`. It was already a direct module dependency (via `config`), so
`go.mod` is unchanged, but `mcp` is no longer stdlib-only and the claim to the
contrary in `CLAUDE.md`/`README.md` had to be corrected. The alternative — a
`mcp/skillfs` subpackage owning the dependency — was considered and not taken;
a hand-rolled YAML subset was rejected outright, since "verbatim as JSON" needs a
real parser.

### 3. Wiring (`mcp/server.go`)

Two additive `ServerConfig` fields, `Skills *SkillRegistry` and
`SkillsDirectoryRead bool`; `NewServer`'s positional signature is untouched. No
broker wiring is needed — a skill's files are ordinary resources, so registration
already flows through `resourceRegistry.onChange`.

`capabilities()` sets `Extensions` when `Skills != nil`, **even with zero skills
registered**: unlike tools and resources, an empty listing is legitimate and the
SEP says hosts MUST NOT read one as proof a server has no skills.

Three dispatch cases follow the `completion/complete` precedent — when the gate is
false the method genuinely does not exist, so `-32601` before any handler runs.
The two existing `-32601` sites were refactored onto a shared
`writeMethodNotFound` helper, which logs *why* but never says so on the wire: an
opt-in method that is switched off must be indistinguishable from one that was
never implemented.

### 4. Handlers

`handleSkillsList` is the `handleResourcesList` pattern verbatim (lenient cursor
decode → `List()` → `paginate` → `CacheableResult`). `handleSkillsGet` tries
`Get`, then the resolver only on a miss, then `-32602`.

`skills/get`'s cache envelope is **this library's call, not the spec's** —
SEP-2640 defines `ttlMs`/`cacheScope` for `skills/list` and leaves `skills/get`
open. A single entry is as cacheable as a listing of one, so it carries the same
hints with the same list TTL. Noted in the type's doc comment.

### 5. `resources/directory/read` — derived, not bookkept

New `ResourceRegistry.DirectoryChildren(dirURI) ([]Resource, bool)`: any registered
resource whose URI begins with `dirURI+"/"` is a descendant, its next path segment
names the child, and a deeper descendant collapses into a synthesized
`inode/directory` entry. No directory resources to register, nothing to keep in
step, and scheme-agnostic as the SEP requires. Reusing `Resource` yields exactly
`{uri, name, mimeType}` on the wire because every other field is `omitempty`;
`Name` is overridden with the child's own segment, which is what a directory
listing wants while `resources/list` keeps the disambiguating relative path.

Consequence worth naming: a directory with no registered descendants is
indistinguishable from one that never existed. Both `-32602`.

### 6. Batch registration (`mcp/resources.go`)

`ResourceRegistry.Register` fires `onChange` on every call, so a six-file skill
would emit six `list_changed` notifications. Unexported
`mutateBatch(remove, add, fns)` does removals and additions under one lock and
fires `onChange` once, plus `onUpdate` per replaced URI — matching `Register`'s
replacement semantics. Hooks are captured under the lock and called after
`Unlock`, as every existing mutator does, because `Broker.broadcast` takes its own
mutex.

### 7. `_meta` / `annotations` on read results

Independent of skills and worth doing regardless: `ResourceContentResult` gains
`Meta map[string]interface{}` and `Annotations *Annotations`, copied onto the
single `ResourceContent` by `resourceReadResult`. Purely additive — the type has
no JSON tags — and it makes two already-declared wire fields reachable for the
first time. SEP-2640 gives it a concrete use (extra frontmatter under the reserved
`io.modelcontextprotocol.skills/` prefix, exported as `mcp.SkillsMetaPrefix`),
but the loader does not populate it by default: it is a MAY, and it duplicates
what `skills/list` already carries verbatim.

### 8. Notifications — add nothing

SEP-2640 defines no skills notification, and a skill's files are ordinary
resources, so `resources/list_changed` already fires. Deliberately **not** adding a
`SkillsListChanged` field to `NotificationFilter`: `PromptsListChanged` is already
accepted-and-never-fired, and a second dead field would read as an oversight
rather than a policy.

Open question for the WG, documented rather than papered over: a client that
cached `skills/list` cannot learn a skill's frontmatter changed while its file set
stayed identical.

### 9. Legacy compat (`compat/`)

Three method names added to `overlay.go`'s forward switch. Nothing else:
`buildInitializeResult` rebuilds capabilities through the typed
`mcp.ServerCapabilities`, which already has an `Extensions` field, so the
capability propagates to legacy clients with no code change; and `downgradeResult`
strips only four top-level keys without descending, so per-skill `frontmatter` and
per-file `digest` survive intact.

Forwarding the extension to legacy clients is a deliberate choice: one that
understands it can use it, one that does not ignores an unknown capability key.

### 10. Keeping an experimental feature honest

`cfg.Skills == nil` means *nothing changes* — no capability key, no method
answering anything but `-32601`, no behavioural delta for the servers that do not
serve skills. A breaking SEP revision is then a breaking change to an experimental
feature nobody was forced to adopt.

- Every exported skills symbol carries an `EXPERIMENTAL:` doc comment naming
  SEP-2640 and the head commit, so drift is visible in `go doc`.
- `SkillsExtensionID` is a constant, not a literal scattered across handler and
  capability code.
- README and LEGACY-COMPAT.md state plainly that no public client consumes this.

### 11. Out of scope / future work

- **No `skill://index.json` or `_manifest` resource.** FastMCP's shipping
  `SkillsProvider` discovers via a per-skill `skill://<name>/_manifest` and
  diverges on URI structure and metadata mapping; SEP-2640 names it as needing
  migration. Implementing both conventions would double the surface for a spec
  that has not settled.
- **No host-side behaviour**: no digest *verification* (a client MUST), no
  `allowed-tools` interpretation, no skill-format opinions beyond the frontmatter
  validation above.
- **No skills-specific notification** (§8).

## Tests

- `mcp/skills_test.go` — its own `newSkillServer` fixture over `testing/fstest`
  rather than growing the 922-line `conformance_test.go`, reusing `call`,
  `validMeta`, `observeDuringMutationN`, `hasNotification`:
  - envelope and empty-array shape; unknown-cursor `-32602`
  - **the digest ↔ `resources/read` invariant**, on both the text and blob paths
  - `skills/get` nesting under `skill` and matching its `skills/list` element
  - resolver consulted only on a miss; a declining resolver still `-32602`
  - `Skills == nil` → three `-32601`s and no `extensions` key
  - capability settings on/off, and the router agreeing with them
  - `directory/read`: direct children only, `inode/directory`, trailing slash,
    non-directory `-32602`, non-`skill://` scheme, pagination
  - `LoadFS` rejection table (name/description/compatibility rules, missing
    `SKILL.md`, root `SKILL.md`, orphan file, malformed frontmatter)
  - nested skills publishing flat and sharing files across unregister
  - exactly one `list_changed` per skill registered/unregistered
  - a `skill://` resource registered directly is not a skill
  - `_meta`/`annotations` reaching `contents[0]`
- `compat/compat_test.go` — legacy handshake carrying `capabilities.extensions`;
  `skills/list`, `skills/get`, `resources/directory/read` forwarded and downgraded
  with frontmatter and digests intact; all three `-32601` without a registry.

## Docs to update (same commit series)

- `README.md` — Features bullet, a `### Skills (experimental)` section between
  completion and change notifications, the corrected dependency claim, and the
  project-structure tree.
- `CLAUDE.md` — the lede's "what is and isn't implemented" paragraph, a
  `### Skills (EXPERIMENTAL — SEP-2640)` subsection, the `compat/`
  responsibilities bullet, the corrected pure-stdlib paragraph, the tree.
- `LEGACY-COMPAT.md` — forwarded-method table rows, result-downgrading list,
  known limitation 7 (experimental on both sides), and the testing list.
- `.claude/skills/generic-go-mcp-consumption/` — `SKILL.md` frontmatter,
  `ServerConfig` table, "Where to go next", and the accuracy note; a new
  "Skills over MCP" section in `references/mrtr-and-resources.md`. Per the
  skill's own accuracy note, verify every snippet against the real source.
- `examples/go-mcp/skills/timezone-report/` — an embedded demonstration skill
  written against the example server's own `date` tool, loaded in UNIX-socket mode
  alongside the resource template and completion provider.

## Release

Tag **v0.8.0** when done. Downstream `mcp-kube-baker` can develop against a
`replace => ../generic-go-mcp` directive and drop it once the tag exists. Per the
consumption skill's accuracy note, re-verify from a downstream `go get` of the
tagged module through the proxy — not just this checkout — and update the
`@v0.7.0` pin in `SKILL.md` at that point.
