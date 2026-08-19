# Parameterized Resources Engine (Resource Templates) — v0.7.0

Plan: `/home/spirilis/.claude/plans/get-started-on-a-agile-hellman.md`

## Implementation

- [x] `mcp/templates.go` — ResourceTemplate / ResourceReadRequest / ResourceTemplateFunction types
- [x] `mcp/templates.go` — `compileURITemplate` matcher ({var} + terminal {+var}, stdlib only)
- [x] `mcp/resources.go` — registry: RegisterTemplate / UnregisterTemplate / ListTemplates / HasTemplates / MatchTemplate
- [x] `mcp/resources.go` — NotifyUpdated falls through to template matching
- [x] `mcp/resources.go` — handleResourcesRead template dispatch + shared resourceReadResult helper
- [x] `mcp/templates.go` — handleResourcesTemplatesList + ResourcesTemplatesListResult
- [x] `mcp/server.go` — dispatch case + capabilities() widened to HasResources() || HasTemplates()
- [x] `mcp/completion.go` — optional CompletionProvider, completion/complete handler, completions capability
- [x] `compat/overlay.go` — forward resources/templates/list and completion/complete

## Tests

- [x] `mcp/templates_test.go` — matcher compile/match unit tests
- [x] `mcp/conformance_test.go` — templates/list, read precedence, notifications, capability, completions
- [x] `compat/compat_test.go` — legacy templates/list downgraded, legacy read on a template URI

## Docs / examples

- [x] `examples/go-mcp/main.go` — mcp+unix:///env/{name} template + completer
- [x] `README.md`
- [x] `LEGACY-COMPAT.md`
- [x] `.claude/skills/generic-go-mcp-consumption/` (SKILL.md + references/mrtr-and-resources.md)
- [x] `CLAUDE.md`

## Verification

- [x] gofmt -l . clean, go vet ./..., go test ./...
- [x] examples/go-mcp builds and answers the three end-to-end requests
- [x] mcp-kube-baker builds against the working tree

## Review

All items complete. `gofmt -l .` clean, `go vet ./...` clean, `go test -race ./...` green
(92 passing tests across `mcp` + `compat`; 44 new test functions in `mcp/templates_test.go`,
7 new in `compat/compat_test.go`). No existing test was modified — the pre-existing suite passing
unchanged is the evidence the modern-only posture is untouched.

### What shipped

- `mcp/templates.go` — `ResourceTemplate`, `ResourceReadRequest`, `ResourceTemplateFunction`,
  `ErrResourceNotFound`, the hand-rolled RFC 6570 matcher, and `handleResourcesTemplatesList`.
- `mcp/completion.go` — opt-in `CompletionProvider` / `CompletionFunc` and `completion/complete`.
- `mcp/resources.go` — template registry API, `NotifyUpdated` template fallthrough, read dispatch.
- `mcp/server.go` / `mcp/capabilities.go` — two dispatch cases, widened `resources` capability,
  new `completions` capability.
- `compat/overlay.go` — two methods added to the forward list (no downgrade changes needed).
- Docs: `README.md`, `LEGACY-COMPAT.md`, `CLAUDE.md`, and three skill files.

### Deviations from the approved plan

1. **Added `mcp.ErrResourceNotFound`** (not in the plan). End-to-end testing showed a URI that
   *matches* a template but names an absent member returned `-32603` (server fault) instead of
   `-32602` (no such resource). A template claims a whole URI shape, so the handler is the only
   thing that knows the member is missing, and it previously had no way to say so. Applies to
   concrete `ResourceFunction`s too, for symmetry.
2. **`RegisterTemplate` rejects a variable-free template** with "register it as a concrete Resource
   instead" — it would otherwise be unreachable via `resources/list` and duplicate `Register`.
3. **Template tests live in `mcp/templates_test.go`**, not appended to the 922-line
   `mcp/conformance_test.go`. They use that file's existing package-level helpers (`call`,
   `validMeta`, `observeDuringMutation`, `hasNotification`, `notificationParams`) unchanged.
4. **`newTestServer` / `newTestOverlay` were left alone**; the new tests build their own fixtures,
   so no existing assertion shifted underneath.

### Known follow-ups (not blocking)

- **`v0.7.0` is not tagged yet.** `SKILL.md`'s `go get` pin was bumped to `v0.7.0` and its accuracy
  note now carries an explicit "pending re-verification against the tag" marker, per that note's own
  rule about checking a downstream `go get` of the tagged module rather than a checkout.
- **`mcp-kube-baker` does not currently build**, for a reason unrelated to this work: its `go.sum`
  has zero `gopkg.in/yaml.v3` entries. Verified instead with a standalone consumer program that
  exercises the whole new public API (`RegisterTemplate`, `SetTemplateCompleter`, `MatchTemplate`,
  `NotifyUpdated`, `ErrResourceNotFound`, `CompletionFunc`) on a pure `mcp` + `transport` import
  graph. A `go mod tidy` in that repo is theirs to run.
- Out of scope by decision: `{?query}` and other RFC 6570 levels, `ref/prompt` completion (prompts
  are unimplemented), and MRTR from `resources/read`.
