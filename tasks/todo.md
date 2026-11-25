# v0.11.1: compact the OAuth store (2026-11-25) — DONE

From the mcp-kube-baker security audit: anonymous `/authorize` and `/register` traffic can grow the bbolt
file, which sweeping never shrinks, and a store failure made the refresh grant burn the token and panic.

- [x] `BoltStorage.Compact(ctx)`: copy live data to `<path>.compact` (`bolt.Compact`), close, atomic rename,
      reopen. Writes are gated for the whole run (`writeGate`), reads only for the swap (`dbMu`). Every
      storage method now goes through `view`/`update`.
- [x] `AuthService.CompactStore(ctx)`: sweep, then compact; returns `CompactResult`. Not scheduled by the
      library: the embedder runs it hourly or on SIGUSR1 (README "Keeping the token store small").
- [x] Refresh grant: mint the new tokens before deleting the old one; a store failure answers `server_error`.
- [x] Tests: flood + sweep + compact shrinks the file and keeps live data; write waits and read proceeds
      between copy and swap; no acknowledged write lost under concurrent load (-race); leftover temp file;
      cancelled run; refresh survives a failing store (panics without the fix).

Not done: no cap or rate limit on pending `/authorize` records (a cap lets an attacker block sign-ins for
the 10-minute TTL instead); `/register` slots can still be pinned; a POST form to `/authorize` is bounded
by the 64 KiB body limit, not the embedder's query limit.

# Skills extension: August draft -> released Beta spec (2026-11-25) — DONE

Target: `specification/beta/skills.mdx` in modelcontextprotocol/ext-skills (SEP-2640 Final,
extension stage Beta per SEP-3392). Claude Code >= 2.1.331 and claude.ai connectors consume it
(public beta, 2026-11-24). Acceptance: `@modelcontextprotocol/conformance@alpha` server skills
scenarios against the example over HTTP.

## Wire and API
- [x] `SkillResource.size` (required): byte length of the same bytes the digest covers.
- [x] `Skill.resources` is always present: an array, or the string `"dynamic"` (`Skill.Dynamic`).
- [x] `Skill._meta` (optional), from `SkillDef.Meta`.
- [x] `SkillDef.Dynamic`: list a generated skill without files.
- [x] Resolver output is validated before it is sent (shape, name, digest format, size).
- [x] Frontmatter is derived from the SKILL.md bytes; a supplied map that differs is rejected.
- [x] YAML -> JSON as a YAML 1.2 host parser reads it (unquoted dates stay strings).
- [x] Per-skill limits: 512 files, 16 MiB; `AllowOversizedSkills` opts out.
- [x] Declaring the extension always declares `resources`.

## Around it
- [x] Example serves skills in every mode (conformance needs a URL).
- [x] Legacy overlay: tests for `initialize` (extension + resources) and `size` on skills/list.
- [x] Comments, README, CLAUDE.md, consumption skill: Beta, clients exist, Claude's host limits.
- [x] `go test -race ./...`, `go vet`, `gofmt`; conformance run on 2026-07-28 and 2025-11-25.

## Review
- Conformance (`@modelcontextprotocol/conformance@alpha`, skills scenarios) against the example:
  45/45 as a 2026-07-28 client; 43/43 as a 2025-11-25 client through the overlay, the two
  cache-attribute checks skipped as not applicable before 2026-07-28. The v0.10.2 code fails
  `sep-2640-resources-size-required`, so the suite does discriminate.
- Same result against a scratch server with a nested skill, a `Dynamic` skill, a resolver-only
  skill and frontmatter holding an unquoted date, `014` and `yes`.
- MCP Inspector `--cli --method skills/list --verify`: every digest, size and frontmatter
  verified; the dynamic skill reported `unverifiable`, as it should be.
- Not tested: loading a skill in Claude or Claude Code. MCP skills are not available in the
  session type this work ran in.
- The legacy overlay still strips `ttlMs`/`cacheScope` from skills results. That is deliberate:
  the fields are undefined before 2026-07-28 and the conformance suite skips them there.

## Not doing
- No tag: a module tag is permanent and no Claude client has loaded these skills yet.

# Audit remediation — Skills (SEP-2640) & general codebase — DONE

Source: security/correctness audit (plan file take-a-look-at-encapsulated-creek.md).

## Fixes
- [x] G1 (HIGH): `auth/errors.go` — removed the `http.Redirect(w, nil, …)` nil-request panic
      (set Location directly); handle url.Parse error; reordered `handleAuthorize` so client +
      redirect_uri are validated FIRST and reported directly (never redirect an error to an
      unvalidated URI). Added `authErrorDirect`. Regression tests in auth/handlers_test.go.
- [x] G2 (MED, decision=ENFORCE): `authenticateClient` verifies client_secret for confidential
      (client_secret_post) clients in both grants; public clients (no stored secret) still pass
      on PKCE. `verifySecret` now constant-time and live. Tests cover wrong/missing/correct.
- [x] G3 (MED): `NewAuthService` logs a Warn when auth is enabled with an empty allowlist.
- [x] G4 (MED): `transport/http.go` — ReadHeader/Read/Idle timeouts (WriteTimeout omitted so
      it can't sever long-lived SSE), plus `http.MaxBytesReader` body cap (MaxBodyBytes,
      default 16 MiB) → 413. Tests: oversized→413, within-cap→200.
- [x] G5 (LOW): `internalErr` logs detail, returns generic "Internal error" (no raw Go text).
- [x] S1 (LOW): SkillRegistry `digests` map + pre-mutation guard rejects two skills publishing
      one file URI with different bytes (excludes a skill's own replacement). Tests added.
- [x] S2 (LOW): `Register`/`Unregister` hold the registry lock through `rr.mutateBatch`, so a
      skill is never listable before its files are readable (lock order is one-way, no deadlock).
- [x] S3 (VLOW): `DirectoryChildren` skips empty-segment children on degenerate prefixes.

## Verification
- [x] `gofmt -l .` clean, `go vet ./...` clean, `go test -race ./...` all green (fresh cache).
- [x] Example server builds.
- [x] New auth tests: denied authorize redirects (no panic); invalid redirect_uri / unknown
      client reported directly (no open redirect); token grant enforces client_secret.
- [x] New transport tests: oversized POST → 413 (handler not invoked); within-cap → 200.

## Review
Nine findings from the audit addressed. The only HIGH (G1) was a confirmed nil-request panic
that broke the entire OAuth error-redirect path and was unauthenticated-triggerable; the fix
also closes the latent open-redirect it masked by validating redirect_uri before redirecting.
G2 was resolved per the user's decision to enforce secrets. New tests were added for auth
(previously untested) and for the two mechanical protections (G4 body cap, S1 digest guard).
The skills code itself needed only low-severity hardening — the audit found it sound, and there
is no SQL/injection surface anywhere (BoltDB key/value).

Not done (out of scope / no user request): nothing committed; no new tag; config-file plumbing
for MaxBodyBytes was not added (the 16 MiB default protects every HTTP deployment, and the knob
is available on HTTPTransportConfig for embedders who need it).

# v0.10.0: mount /mcp on an embedder's own server (2026-10-17, for bin-inventory Phase 4)

- [x] `HTTPTransport.Handler(handler) http.Handler`: the /mcp endpoint (auth middleware included) for an
      embedder that owns its http.Server and mux; registers no routes; `buildMux` uses the same `mcpHandler`.
- [x] Legacy requests' JSON-RPC errors answer HTTP 200 (the 2026-07-28 status table is modern-only; a 404
      told 2025-11-25 clients their session was gone). Unknown-session 404 unchanged.
- [x] `responseRecorder.Unwrap` so http.ResponseController reaches the connection through it.
- [x] Docs: HTTP-TRANSPORT.md (mounting section, server timeout guidance, legacy status note), README,
      LEGACY-COMPAT.md, ExtraRoutes comment lists /consent.
- [x] Tests: Handler on an embedder mux (POST, own 405, Origin, embedder route), auth middleware only (no
      route registration), recorder Unwrap, legacy error → 200 vs modern → 404 (fails without the fix).

Known, not fixed here: `Start` sets `ReadTimeout: 30s`, and an expired read deadline cancels the request
context once net/http's background read starts, so SSE streams under `Start` likely end after ~30s.

# v0.10.1: dynamic client registration as MCP clients send it (2026-10-18)

Claude Desktop's custom connector failed at `POST /register` with 400 `invalid_client_metadata` "Invalid JSON":
`ClientRegistrationRequest.Scope` was `[]string`, but RFC 7591 §2 makes `scope` a space-separated string, and Claude
sends one (the metadata advertises `scopes_supported`).

- [x] `ScopeList`: accepts the string (and the old array form), written back as the RFC string; the response now
      carries `scope`. A decode failure names its cause instead of "Invalid JSON".
- [x] `token_endpoint_auth_method: "none"` (advertised in the metadata) registers a public client (no secret; PKCE is
      mandatory at /authorize; /token already treats a secret-less client as public). Others: client_secret_post.
- [x] `client_secret_expires_at: 0` whenever a secret is issued (RFC 7591 §3.2.1 requires it).
- [x] Tests: Claude's registration body (fails on v0.10.0 with the production error), array scope, numeric scope,
      public vs confidential registration and /token client authentication.
