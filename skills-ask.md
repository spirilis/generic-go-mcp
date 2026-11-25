● Here's the ask, written against what v0.7.0 actually has:

  ---

  Proposal: SEP-2640 Skills Extension support in generic-go-mcp

  Status of the target spec: SEP-2640 "Skills Extension" — Extensions Track, Draft, open and under active review (31 review comments; last touched 2026-08-20). Extension identifier io.modelcontextprotocol/skills. Owned by the Skills over MCP WG. The predecessor SEP-2076, which
  made skills a first-class primitive, was closed as superseded.

  Nothing about skills appears in the ratified 2026-07-28 spec pages or changelog — the top-level spec overview links skills only as an extension. This proposal is therefore for an explicitly experimental, opt-in feature whose wire shapes may change before the SEP merges. That
  framing should be load-bearing in the API design (see §7).

  Skill format is out of scope for both the SEP and this proposal: SEP-2640 delegates it to the Agent Skills specification and states it "does not redefine, constrain, or extend the skill format."

  1. Why the library, not the consumer

  I hit this building a skills surface into mcp-kube-baker. Four things make it un-implementable from outside the mcp package:

  ┌────────────────────────────────┬───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
  │              Need              │                                                                                                               Blocker in v0.7.0                                                                                                               │
  ├────────────────────────────────┼───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
  │ Route skills/list / skills/get │ (*Server).HandleMessage's dispatch switch is closed. No registration hook, no fallback handler. Both methods fall to default: → -32601.                                                                                                       │
  ├────────────────────────────────┼───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
  │ Declare the extension          │ ServerCapabilities.Extensions map[string]json.RawMessage exists (mcp/capabilities.go:34) but is never populated, and ServerConfig has no field that reaches it. (*Server).capabilities() sets Tools, Resources, Completions and nothing else. │
  ├────────────────────────────────┼───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
  │ resources/directory/read       │ Same closed dispatch.                                                                                                                                                                                                                         │
  ├────────────────────────────────┼───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
  │ _meta on a resources/read      │ ResourceContent has Meta and Annotations on the wire (mcp/resources.go:366-375), but ResourceContentResult — the type a ResourceFunction returns — carries only Text, Blob, MimeType, and (*Server).resourceReadResult never populates either │
  │ payload                        │  field. Unreachable by composition; the builder is a private method.                                                                                                                                                                          │
  └────────────────────────────────┴───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┘
  
  A consumer can wrap the server: transport.MessageHandler is a one-method interface (transport/transport.go:46) and compat.Overlay is the precedent for wrapping it and rewriting result JSON. I prototyped that route and rejected it — it means an out-of-tree overlay unmarshalling
  server/discover results to splice in a capability, duplicating pagination and CacheableResult envelope construction, and re-implementing it in every consumer. This belongs in the library.

  What already works and needs no change: arbitrary _meta on Resource/ResourceTemplate catalog entries (Meta map[string]interface{} on both), text/markdown content, Blob for binary assets, per-read MIME override, resources/list_changed push, and the compat overlay's
  forwarding/downgrade path.

  2. Wire shapes to implement

  skills/list — MUST, once the extension is declared. Paginated; MAY return partial or empty results for generated/unenumerable catalogs.

  {
    "skills": [
      {
        "uri": "skill://acme/billing/refunds/SKILL.md",
        "frontmatter": { "name": "refunds", "description": "..." },
        "resources": [
          { "uri": "skill://acme/billing/refunds/SKILL.md", "digest": "sha256:<64 hex>" },
          { "uri": "skill://acme/billing/refunds/references/GUIDE.md", "digest": "sha256:<64 hex>" }
        ]
      }
    ],
    "nextCursor": "opaque",
    "resultType": "complete",
    "ttlMs": 300000,
    "cacheScope": "public"
  }

  frontmatter is the SKILL.md YAML verbatim — not a normalized struct. That matters: unknown keys must survive, because the format spec allows metadata, license, compatibility, allowed-tools, and Claude Code ships a superset of its own.

  skills/get — MUST. Params {"uri": "<SKILL.md uri>"}, returns one entry in the same shape as a skills/list element. MUST resolve URIs absent from the listing (that's the point — it's how unenumerable catalogs work). Non-skill URI → -32602, which invalidParamsErr already
  produces.

  resources/directory/read — optional, gated behind the capability sub-setting. Non-recursive direct children of a resource whose mimeType is inode/directory, paginated. Exists because resources/list enumerates the whole server namespace with no way to scope a subtree.

  Capability declaration:

  { "capabilities": { "extensions": { "io.modelcontextprotocol/skills": { "directoryRead": false } } } }

  directoryRead defaults false and is currently the only defined setting. Under 2026-07-28 this goes in the server/discover result; the SEP's own text says "initialize response" because it was drafted pre-2026-07-28 — worth noting in a code comment so nobody "fixes" it later.

  Two rules the implementation must not violate:

  1. skill:// is a SHOULD, not a MUST. "A server MAY serve skills under another scheme native to its domain (e.g. github://owner/repo/skills/refunds/SKILL.md). No scheme is privileged." So the registry must accept any URI, and must never auto-classify a resource as a skill
     because of its scheme.
  2. "A host MUST NOT conclude that a resource is a skill merely because its URI carries a particular scheme." Skill-ness is established only by skills/list/skills/get. The library must not, for example, synthesize skill entries by scanning the resource registry for skill://
     prefixes.

  The final segment of a skill's path MUST equal the frontmatter name. Relative references inside SKILL.md (references/GUIDE.md) resolve against the skill root like a filesystem, not as absolute URIs — the client does that resolution, so the server only needs sibling URIs to be
  readable.

  3. Proposed API

  Additive throughout — NewServer's positional signature stays intact, which matters because consumers and the in-tree example call it positionally.

  // skills.go

  // Skill is one entry in skills/list. Frontmatter is the SKILL.md YAML verbatim:
  // unknown keys are preserved, because the Agent Skills format permits fields this
  // library has no opinion about, and hosts ship supersets of their own.
  type Skill struct {
      URI         string                 `json:"uri"`
      Frontmatter map[string]interface{} `json:"frontmatter"`
      Resources   []SkillResource        `json:"resources"`
  }

  // SkillResource names one file belonging to a skill. Digest is "sha256:" + 64 lowercase
  // hex of the file's RAW BYTES, and MUST match what resources/read returns for URI.
  type SkillResource struct {
      URI    string `json:"uri"`
      Digest string `json:"digest"`
  }

  type SkillRegistry struct { /* mu, entries, order, resolver, onChange */ }

  // NewSkillRegistry takes the resource registry because a skill's files ARE resources:
  // registering a skill registers its files so resources/read can serve them, and the
  // two can never drift out of step.
  func NewSkillRegistry(rr *ResourceRegistry) *SkillRegistry

  // Register adds one skill and every file resource it names. content is keyed by the
  // URIs in s.Resources; every one MUST be present. Errors if a digest is malformed, if
  // s.URI is not among s.Resources, or if the last path segment of the skill path does
  // not equal frontmatter["name"].
  func (r *SkillRegistry) Register(s Skill, content map[string]SkillContent) error

  func (r *SkillRegistry) Unregister(uri string) bool          // also unregisters its files
  func (r *SkillRegistry) List() []Skill                       // registration order, stable
  func (r *SkillRegistry) Get(uri string) (Skill, bool)

  // SetResolver backs skills/get for catalogs too large or too dynamic to enumerate.
  // Consulted only after Get misses. This is what makes "MUST resolve URIs absent from
  // the listing" implementable without materializing the keyspace.
  func (r *SkillRegistry) SetResolver(f SkillResolver)

  type SkillResolver func(ctx context.Context, uri string) (Skill, map[string]SkillContent, bool, error)

  // SkillContent is one file's bytes. Size and Digest are derived, not asked for.
  type SkillContent struct {
      Bytes    []byte
      MimeType string // defaults to text/markdown for *.md, else sniffed, else text/plain
  }

  Wiring, one new ServerConfig field:

  type ServerConfig struct {
      // ...existing fields...

      // Skills enables the io.modelcontextprotocol/skills extension when non-nil.
      // EXPERIMENTAL: tracks SEP-2640, which is a Draft. Wire shapes may change.
      Skills *SkillRegistry

      // SkillsDirectoryRead opts into resources/directory/read and is reported as the
      // extension's directoryRead setting. Ignored when Skills is nil.
      SkillsDirectoryRead bool
  }

  Then in (*Server).capabilities():

  if s.skills != nil {
      caps.Extensions = map[string]json.RawMessage{
          "io.modelcontextprotocol/skills": json.RawMessage(
              fmt.Sprintf(`{"directoryRead":%t}`, s.skillsDirectoryRead)),
      }
  }

  and three cases in HandleMessage's switch, returning -32601 when s.skills == nil so an undeclared extension stays honestly unimplemented.

  3a. The loader is the part consumers actually need

  The audit turned up no static-content helper anywhere in the library — the example demonstrates the hand-rolled closure pattern. For skills that's not just inconvenient, it's a correctness trap: digests must match file bytes exactly, and hand-maintained digests rot silently.

  // LoadFS walks fsys for SKILL.md files, parses and validates frontmatter, computes
  // sha256 digests over raw bytes, and registers each skill with every sibling file
  // under its directory. uriPrefix is typically "skill://".
  //
  // This is the whole reason a consumer reaches for this API: one call turns an
  // embed.FS of skill directories into a compliant skills/list plus readable resources,
  // with no digest bookkeeping.
  func (r *SkillRegistry) LoadFS(fsys fs.FS, uriPrefix string) error

  Consumer side, which is where I want to end up:

  //go:embed all:content
  var skillFS embed.FS

  skills := mcp.NewSkillRegistry(resourceRegistry)
  if err := skills.LoadFS(skillFS, "skill://"); err != nil { /* fatal */ }

  fs.FS rather than embed.FS keeps os.DirFS and test fstest.MapFS working for free.

  Validation LoadFS should enforce, straight from the Agent Skills spec — these are cheap and catch real authoring errors:

  - name: required, 1–64 chars, [a-z0-9-] only, no leading/trailing hyphen, no --, must equal the parent directory name (and therefore the skill path's final segment, satisfying the SEP's rule)
  - description: required, non-empty, ≤1024 chars
  - compatibility: optional, ≤500 chars
  - license, metadata (string→string), allowed-tools: passed through verbatim, uninterpreted — allowed-tools in particular is a host-side consent matter the SEP says hosts MUST ignore for MCP-origin skills absent explicit per-skill approval, so the library must not act on it

  Resource.Size should be populated while the loader has the bytes in hand — it's already an optional field on Resource and skill files have a known size.

  Dependency question for you: frontmatter parsing needs YAML. If gopkg.in/yaml.v3 is already a direct dep for the config schema, LoadFS can live in mcp. If you'd rather keep mcp dependency-free, split it: Skill/SkillRegistry/handlers in mcp, LoadFS in a mcp/skillfs subpackage
  that owns the YAML dep. Register taking Frontmatter map[string]interface{} means a consumer can always parse it themselves.

  4. Notifications

  SEP-2640 defines no skills-specific notification. Since a skill's files are ordinary resources, registering or unregistering one already fires resources/list_changed through the existing ResourceRegistry.onChange → Broker path. My recommendation: rely on that, and do not
  invent notifications/skills/list_changed.

  There's a wrinkle worth being deliberate about: SubscriptionFilter already has a PromptsListChanged field that is accepted and never fires (mcp/subscriptions.go:15-19, documented as such in the README). If skills follow the same "accepted, never fires" pattern, say so in the
  same place, so the two dead-ish fields read as one consistent policy rather than an oversight. Otherwise this is a good question to put to the WG — a client that has cached skills/list has no way to learn a skill's frontmatter changed while its file set stayed identical.

  5. Cache envelope, pagination, legacy compat

  - skills/list embeds CacheableResult with s.listTTLMs / s.cacheScope(), exactly like handleResourcesList. resultType comes free from the central result.setResultType(ResultTypeComplete) in HandleMessage.
  - skills/get's envelope is unresolved in the SEP itself — it says skills/list carries ttlMs/cacheScope per SEP-2549 "in protocol versions 2026-07-28 and later" but leaves skills/get open. I'd embed CacheableResult there too for consistency and note the open question in a
    comment; a single-entry result is as cacheable as a list.
  - Pagination should reuse whatever tools/list and resources/list already do for cursor/nextCursor rather than growing a second scheme.
  - compat/overlay.go: add "skills/list", "skills/get", and "resources/directory/read" to the forward switch, or they'll -32601 for legacy clients while working for modern ones. downgradeResult strips only top-level resultType/ttlMs/cacheScope/_meta and doesn't descend into
    arrays, so per-skill frontmatter and per-file digest survive the downgrade untouched — no extra work there.
  - One genuine decision: should legacy clients see the extension at all? The extensions mechanism (SEP-2133) is 2026-07-28-era, but the overlay already rebuilds capabilities for its synthetic initialize response, so passing capabilities.extensions through is a two-line change.
    I'd forward it — a legacy client that understands the extension can use it, and one that doesn't ignores an unknown capability key.

  6. Independent, smaller ask: _meta on read results

  Worth doing regardless of skills, and the SEP gives it a concrete use — servers MAY expose extra frontmatter on a skill's resource _meta, with keys under the reserved io.modelcontextprotocol.skills/ reverse-domain prefix.

  type ResourceContentResult struct {
      Text     string
      Blob     string
      MimeType string
      Meta        map[string]interface{} // NEW -> ResourceContent._meta
      Annotations *Annotations           // NEW -> ResourceContent.annotations
  }

  Purely additive — existing consumers recompile unchanged — and it makes two wire fields reachable that the JSON structs already declare but no code can populate. Catalog-level _meta on Resource already works, so this is only about parity on the read path.

  7. Keeping an experimental feature honest

  The single most important design property: cfg.Skills == nil must mean nothing changes. No capability key, no new methods answering anything but -32601, no behavioural delta for the 99% of servers that don't serve skills. That makes the whole surface opt-in, so a breaking SEP
  revision is a breaking change to an experimental feature nobody was forced to adopt.

  Concretely I'd suggest:

  - Doc-comment every exported skills symbol with EXPERIMENTAL: and the SEP revision it tracks (date or PR commit), so drift is visible in go doc
  - A SkillsExtensionID = "io.modelcontextprotocol/skills" constant rather than the literal scattered across handler and capability code
  - One paragraph in LEGACY-COMPAT.md and the README stating plainly that no public client consumes this yet (Claude Code's skills are local-only — ~/.claude/skills, project dirs, plugins, claude.ai sync — with no skill:// support in its docs or changelog; SEP-2640 says
    Anthropic's support is "prototyped internally, not yet public")
  - Don't chase FastMCP. Its shipping SkillsProvider also uses skill:// but discovers via a per-skill skill://<name>/_manifest resource instead of a central method, and diverges on URI structure and metadata mapping. SEP-2640 calls this out by name as needing migration.
    Implementing both conventions would double the surface for a spec that hasn't settled.

  8. Acceptance tests

  In conformance_test.go style:

  - skills/list with no skills registered → empty skills array, not null; resultType/ttlMs/cacheScope present
  - skills/list → resources[].digest for every file matches sha256 of what resources/read returns for that same URI (the invariant most likely to rot)
  - skills/get on a registered URI → same entry as its skills/list element
  - skills/get on a URI known only to the resolver → resolves; resolver is not consulted when Get hits
  - skills/get on a plain non-skill resource URI → -32602
  - cfg.Skills == nil → skills/list, skills/get, resources/directory/read all -32601, and server/discover has no capabilities.extensions key
  - cfg.Skills != nil → capabilities.extensions["io.modelcontextprotocol/skills"] == {"directoryRead":false}; true only with SkillsDirectoryRead
  - LoadFS over an fstest.MapFS: rejects name mismatching its directory, name with -- or uppercase, >64-char name, >1024-char description, missing description, absent SKILL.md; preserves unknown frontmatter keys verbatim
  - Register → resources/read of every skill file returns its bytes with text/markdown; Unregister → those URIs -32602 and the skill leaves skills/list
  - Registering/unregistering a skill fires resources/list_changed exactly once
  - Legacy compat: skills/list over the overlay returns the same skills array with ttlMs/cacheScope/resultType stripped and frontmatter/digest intact
  - A skill registered under a non-skill:// scheme works identically (the scheme is not privileged)
  - A skill://-scheme resource registered directly on the ResourceRegistry, never through SkillRegistry, does not appear in skills/list

  9. What this unblocks downstream

  mcp-kube-baker wants to ship Kubernetes-troubleshooting skills — procedures written against its own tool names (kubectl_get_pods → the pod resource → kubectl_get_pod_logs with previous=true → kubectl_get_events) rather than generic kubectl advice a read-only MCP server can't
  execute. Content is static, embedded, immutable per build: the easy case, and a good first consumer. With the API above that's NewSkillRegistry + LoadFS + one ServerConfig field, and the digest/frontmatter/validation burden stays in one tested place instead of being re-derived
  per server.

  Until the library has this, the honest fallback for a consumer is a tool whose description carries the skill catalog — model-driven, works today, and no client has to understand any extension. That's what I'm building in the meantime, and it's complementary rather than
  redundant: SEP-2640 gives a host the ability to load a skill deliberately, while a tool gives the model a way to pull one mid-reasoning.

  ---

  Two things I'd flag before you start: the SEP moved as recently as today, so it's worth re-reading the current diff for renamed fields before committing to frontmatter/resources/digest — and skills/get's cache envelope is genuinely unresolved upstream, so whatever you pick
  there, pick it with a comment saying it's your call and not the spec's.
