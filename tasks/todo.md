# v0.12.0: any OIDC provider, access rules, app-defined roles (2026-11-26) — DONE (uncommitted, untagged)

Ask (user, 2026-11-26): OAuth beyond GitHub. GitHub stays the privileged, two-value setup (easy in a Helm
chart); any OIDC provider can be added ad hoc with every OIDC parameter exposed. Access is either
pass/fail entry or app-specific roles, granted by OIDC identity or by group claims. The roles are an
enumerated list the embedding application passes in. Not committed, tagged or pushed: the user releases.

## Hard constraints
- Stdlib + bbolt only. No new module dependencies (JWT/JWKS verification by hand: RS256, ES256).
- Backward compatible for the six consumers (v0.10.1–v0.11.1): `auth.github` + `auth.allowlist` YAML load
  unchanged and behave as before (including the empty-allowlist-admits-all warning, legacy form only);
  `/callback` stays the redirect path registered in live GitHub OAuth Apps; `User` JSON tags in bbolt are
  never renamed (a stored user with no provider is GitHub); `transport.AuthProvider` signature unchanged;
  `NewAuthService(cfg)` still compiles (options are variadic); `ToolRegistry.Register(tool, fn)` still
  compiles.
- Verify consumers with a throwaway go.work in the scratchpad (GOWORK=…), never touching their go.mod.

## Config (auth.*)
```yaml
auth:
  enabled: true
  issuer: https://mcp.example.com          # this server, as OAuth AS (unchanged)
  storage: {dbPath: /data/oauth.db}
  github: {clientId, clientSecret, clientIdFile, clientSecretFile}   # legacy == providers[{name: github, type: github}]
  allowlist: {users, orgs, teams}                                     # legacy == one rule on provider github
  providers:                       # new; may be combined with legacy github (then no github entry here)
    - name: github                 # [a-z][a-z0-9-]{0,31}; prefix of principal ids; unique
      type: github
      displayName: GitHub
      clientId/clientSecret/clientIdFile/clientSecretFile
      baseURL: https://github.com          # optional, GitHub Enterprise Server
      apiURL: https://api.github.com       # optional
    - name: corp
      type: oidc
      displayName: Corp SSO
      issuerURL: https://login.example.com/realms/corp   # discovery: <issuerURL>/.well-known/openid-configuration
      clientId/clientSecret/clientIdFile/clientSecretFile  # secret optional (public client + PKCE)
      tokenEndpointAuthMethod: client_secret_basic|client_secret_post|none   # default basic if secret
      scopes: [openid, profile, email]     # default; "openid" forced
      claims:                              # dotted paths into the merged id_token(+userinfo) claims
        subject: sub                       # fixed; documented, not configurable
        username: preferred_username
        email: email                       # email_verified applies only when this is "email"
        name: name
        groups: groups                     # e.g. "realm_access.roles", "cognito:groups"; string or array
        usernameIsUnique: false            # (fix round 1) required for users: rules on this provider
      userInfo: false                      # also fetch userinfo_endpoint and merge (needed by some IdPs for groups)
      authParams: {prompt: select_account} # extra /authorize params (hd, domain_hint, acr_values…); cannot override reserved ones
      allowedAlgorithms: [RS256, ES256]    # default; never "none", never HS*
      caFile: ""                           # PEM for a private CA; else system roots
      audience: ""                         # extra accepted aud besides clientId (rare)
      allowedJWKSHosts: []                 # hosts besides the issuer's that jwks_uri (and userinfo_endpoint
                                           # when userInfo) may use
      requireVerifiedEmail: true           # emails/emailDomains rules need email_verified (IdP property); oidc only
  access:
    rules:                         # admitted iff >=1 rule matches; roles = union of matching rules' roles
      - provider: github
        users: [spirilis]          # github login (case-insensitive); oidc only with claims.usernameIsUnique
        roles: [admin]
      - provider: github
        orgs: [acme]               # github only: alias for groups "acme"
        teams: [acme/devs]         # github only: alias for groups "acme/devs"
      - provider: corp
        subjects: ["0b5c…"]        # exact sub
        emails: [a@x.com]          # requires email_verified=true (provider's requireVerifiedEmail, default true)
        emailDomains: [example.com]
        groups: [mcp-admins]       # from the groups claim (github: orgs and org/team)
        roles: [admin, writer]
      - provider: corp
        anyone: true               # explicit "every user this IdP authenticates"; warned at startup
    identityMaxAge: 168h           # refresh grant refused after this since the last browser login, so group
                                   # changes at the IdP take effect; 0 = refresh-token TTL (30d)
```
- Validation at NewAuthService (refuse to start): unknown provider in a rule; empty rule (no matcher); duplicate
  provider name; bad name; oidc without issuerURL/clientId; issuerURL not https (http only on loopback, which
  also lets that IdP's endpoints be http on loopback);
  `orgs`/`teams` on a non-github rule; a role not in the app's enumerated list; roles in config but app declared
  none; reserved authParams (client_id, redirect_uri, state, nonce, code_challenge*, response_type, scope).
- Also refused: `anyone: true` combined with other matchers; empty matcher entries (`users: [""]`); a team not
  `org/slug`; an email without `@`; `claims.subject` other than `sub`; algorithms other than RS256/ES256;
  tokenEndpointAuthMethod inconsistent with the secret; a `*File` secret that is unreadable or empty.
- Also refused (fix round 1, deviations): `users` on an OIDC rule unless the provider sets
  `claims.usernameIsUnique: true` (preferred_username may be chosen or changed by the user, OIDC Core 5.7);
  `emails`/`emailDomains` on a GitHub rule (they could never match); `requireVerifiedEmail` or `claims` on a
  GitHub provider (OIDC-only settings; `requireVerifiedEmail: false` would have let email rules match
  GitHub's unverified profile email).
- Matching: a rule matches when ANY of its matchers matches (the legacy allowlist's users/orgs/teams were
  alternatives). users and emails case-insensitive; subjects exact; emailDomains exact domain after the last
  `@` (no suffix match), verified email only; groups case-insensitive for GitHub providers, exact for OIDC.
  GitHub identities have EmailVerified=false, so email rules never match GitHub users. `email_verified` is
  applied only when `claims.email` is the standard `email` claim; any other path is unverified.
- Legacy `auth.allowlist` is exempt from the entry checks: malformed entries (empty user, team missing org or
  slug, `org/team` under orgs) are dropped and never match, as before, instead of refusing to start.
- An `access:` key that is present but null/empty (commented-out rules, Helm `null`) counts as the new form,
  so it never falls back to legacy admit-all. Deviation (fix round 1): detected in `config.LoadFromBytes` by
  walking the parsed yaml.Node, not by an `AuthConfig.UnmarshalYAML` hook, which re-decoded with a fresh
  decoder and so dropped an embedder's `KnownFields(true)` (a misspelt `allowlist` was then silently ignored
  and admitted everyone). Embedders decoding AuthConfig themselves get nil for a null `access:` (documented).
- Fail closed: new-form config with no rule for a provider admits nobody from it (warn at startup).
  Legacy form (`github`+`allowlist`, no `providers`/`access`) keeps today's semantics, warning included.

## Go API
```go
svc, err := auth.NewAuthService(cfg.Auth,
    auth.WithRoles(
        auth.Role{Name: "admin",  Description: "manage everything"},
        auth.Role{Name: "writer", Description: "create and edit"},
    ))
p, ok := auth.PrincipalFromContext(ctx) // *Principal
type Principal struct {
    ID       string   // stable: "github:<login>" legacy-compatible? -> see below
    UserID   string   // internal store id (unchanged meaning)
    Provider string   // "github", "corp"
    Subject  string   // github numeric id as string / oidc sub
    Username string   // github login / username claim
    Email    string
    EmailVerified bool
    Name     string
    Groups   []string
    Roles    []string // sorted, from current rules (evaluated per request)
}
func (p *Principal) HasRole(role string) bool
auth.HasRole(ctx, "admin") bool
svc.Roles() []auth.Role                      // what the app declared (for docs/UI)
svc.RolesFromContext(ctx) []string           // plugs into mcp.ServerConfig.RolesFromContext
```
- Principal.ID: github → `github:<login>` (lowercased login; bin-inventory's ids are lowercased, but
  mcp-kube-baker's and genome-vcf-mcp's keep the login's case, so README tells them to lowercase stored ids
  when switching); oidc → `<provider>:<sub>`. A GitHub login can be renamed and reclaimed, so the ID is not
  reassignment-safe; Provider+Subject is (documented). User records keyed for OIDC by (provider, issuer,
  sub), never email/username.
- Roles/admission evaluated in Middleware on every request from the stored identity snapshot + current rules:
  config edits apply immediately; a user no rule matches any more gets 403 insufficient_scope.
- Group snapshot is taken at browser login; refresh grant re-checks rules and refuses past identityMaxAge
  (invalid_grant → client re-runs /authorize). Documented residual staleness ≤ identityMaxAge.
- `UserFromContext` (transport.AuthProvider) still returns (UserID, login) — login = Username.
- Principal.ID prefix is the provider *name*; the provider type decides login (github) vs sub (oidc).
- Pre-v0.12 user records (no `last_login_at`) carry no groups, since the old callback never stored orgs/teams.
  If no rule matches such a record and some rule for its provider matches on groups (orgs/teams included),
  Middleware answers 401 (not 403) so the client signs in again and the login takes the snapshot; a 403 would
  strand org/team-admitted users after the upgrade. With no such rule a login cannot change the outcome, so
  it answers 403 as v0.11 did (fix round 1; genome-vcf-mcp's removed-user test pins it).
- Pre-v0.12 records read as issuer `https://github.com` (the only one v0.11 could produce), and a record is
  judged only while its issuer is its provider's current one (authorizeUser and the legacy login fallback),
  so pointing `github` at a GHES or redefining it as OIDC does not hand old records to the new user space. An
  issuer mismatch answers 401 (a login makes the user under the new issuer), not 403.
  Deviation (fix round 2): Principal.ID has no issuer, so the per-request check alone still let the new
  issuer's users take the old ones' ids. NewAuthService now refuses to start when the store holds users of a
  configured provider name from another issuer (pre-v0.12 records = github / https://github.com), and the error
  says to use a new provider name. Records of removed providers are ignored. The per-request check (401) stays
  as a second line for records written after startup. The guard lasts only as long as the store.
- Refresh grant: re-evaluates the rules (invalid_grant if the user is no longer admitted), and refuses once
  `now - last_login_at > identityMaxAge` (0 = RefreshTokenTTL, 30d). A pre-v0.12 record has no `last_login_at`;
  deviation (fix round 1): its age counts from the refresh chain's first refresh after the upgrade,
  `RefreshToken.LegacyAgeFrom` (`legacy_age_from`), carried across rotations. It is on the token so the grant
  never rewrites a user record a concurrent login may be updating. Upgrade consequence: before v0.12 a client
  could refresh forever; now every user re-runs the browser login at least every identityMaxAge (30 days by
  default), pre-v0.12 users within identityMaxAge of their chain's first refresh after the upgrade (so at
  most RefreshTokenTTL + identityMaxAge after it).
- GitHub login always fetches orgs and teams (per_page=100, following `Link: rel="next"` up to 10 pages, only
  within apiURL) so a rule added later finds them in the snapshot. Non-200 answers are errors now; a failed
  orgs or teams lookup is logged and leaves those groups out (login still succeeds), as the allowlist did.

## Flow
- `/authorize` (unchanged validation) → one provider: straight redirect; several: a chooser page (same CSP/style
  as consent.go) → `/authorize/provider` POST {request id, provider} → redirect. PendingAuthRequest gains
  Provider, Nonce, UpstreamVerifier (PKCE S256 toward the IdP, always for oidc).
- `/callback` for all providers; provider read from the pending request (state = request id, unchanged).
- OIDC callback: exchange code (with verifier) → id_token verify: alg allowlist, kid in cached JWKS, iss ==
  discovered issuer == configured issuerURL, aud contains clientId (+audience), azp == clientId when multiple
  aud, exp/iat/nbf with 60s leeway, nonce matches; optional userinfo merge (sub must match).
- Discovery/JWKS: lazy, background, single-flight on the service context with timeout + backoff; never on a
  request context; startup never fails because an IdP is down (that provider answers 503 until ready);
  jwks_uri must be https on the issuer's host unless `jwksURLHostCheck: false`... (keep: same host or
  explicitly allowed via `allowedJWKSHosts`); response size caps (1 MiB); JWKS refreshed on a timer (1h) and
  on an unknown kid at most once per minute; issuer mismatch in the discovery doc is fatal for that provider.
- GitHub becomes a provider implementation (orgs/teams fetched at login → Groups "org", "org/team").

### As built (step 2)
- `Provider` interface: `Name()`, `DisplayName()`, `Type()`,
  `AuthCodeURL(ctx, callback, state, nonce, pkceChallenge) (string, error)`,
  `Exchange(ctx, code, callback, verifier, nonce) (Identity, error)`. AuthCodeURL takes a ctx and returns an
  error (deviation from the sketch): an OIDC provider that is still fetching discovery answers
  `ErrProviderUnavailable` after a bounded wait (5s or the request ending), and /authorize answers 503 with
  Retry-After before storing anything.
- Discovery is lazy: no network I/O in NewAuthService; the first sign-in (or callback) starts one goroutine per
  provider on the service's context (`background`, stopped and waited for by Close). It retries with backoff
  1s→5m, then refreshes the JWKS hourly and on an unknown kid at most once a minute (a failed refresh keeps the
  old keys). Requests only wait on it. Fatal (no retry until restart): discovery issuer != issuerURL; jwks_uri,
  and userinfo_endpoint when `userInfo` is on (fix round 1: its unsigned claims decide admission), not https
  or not on the issuer's hostname / `allowedJWKSHosts` (hostnames compared without port). Other failures
  (unreachable, non-200, missing endpoints, authorization/token endpoints not https) are retried. The
  authorization and token endpoints may be on any host (some IdPs serve them elsewhere; what comes back is
  trusted only once the id_token verifies). http is accepted for any endpoint only on loopback and only when
  issuerURL itself is http on loopback (a local development IdP), so a document cannot aim the client secret
  or a bearer token at local services from an https issuer.
- Unknown-kid refresh waits for a fetch that *began* after it asked (fetch generation counter): one already
  in flight may hold the pre-rotation key set.
- HTTP to IdPs: redirects are not followed; bodies capped at 1 MiB; `caFile` is added to the system roots and
  an unreadable/empty caFile refuses to start.
- id_token: alg in allowlist checked first (none/HS* never reach verification); `crit` refused; RSA ≥ 2048 bits;
  EC P-256 point validated; ES256 raw r||s; kid matched (no kid: any key of the alg's type); iss exact; sub
  required; aud must contain clientId or the configured `audience` (either one suffices); azp must equal
  clientId when present and is required when aud has several values; exp and iat required, nbf optional, 60s
  leeway; nonce must match.
- Userinfo (when `userInfo: true`): JSON only (application/jwt refused); sub must equal the id_token's; merged
  over the id_token claims except iss/sub/aud/exp/iat/nbf/nonce/azp/auth_time/at_hash/c_hash/jti.
- Claims: a configured path is tried as a literal key first, then split on dots (longest key first), so
  `https://x/groups` and `cognito:groups` work; groups may be a string or an array of strings; `email_verified`
  may be a bool or "true"; `picture` → AvatarURL.
- GitHub provider: no PKCE or nonce sent to GitHub (no id_token; older GHES lacks PKCE; the client secret binds
  the code). `apiURL` defaults to `<baseURL>/api/v3` when baseURL is not github.com.
- Users: every login is looked up by the new `users_by_identity` index (provider NUL issuer NUL subject; issuer
  = OIDC issuerURL or GitHub baseURL), GitHub included, so github.com and a GHES with the same login are two
  users. Fallback for pre-v0.12 records: only for the provider named `github`, by login, and only if the record
  is a GitHub record of that provider, from the same issuer (pre-v0.12 records are github.com), with the same
  numeric id (or none): a renamed-then-reclaimed login does not inherit the old record and its consents. `GitHubLogin` (and its index) is set only for the provider named
  `github`. `User.Issuer` added (`issuer,omitempty`). `Storage` gained `GetUserByIdentity` (an exported
  interface; no consumer implements it). authorizeUser refuses a record whose stored issuer differs from the
  provider's current one (401 while the provider is configured, 403 once it is gone).
- /callback: dispatches on the pending request's provider; a request with none is accepted only when exactly one
  provider is configured (in flight across the upgrade), else 400. An IdP `error=` is relayed as access_denied
  without the IdP's description. Exchange failures → server_error (temporarily_unavailable when the provider is
  not ready). Consent screen names the provider's display name.
- Admin API (fix round 2, not in the original spec): `/admin/clients` was served to every admitted user. New
  `auth.WithAdminRole(role)` (validated against the declared roles in compileAccess) serves it only to holders
  of that role (403 otherwise). Without it, only the pure legacy form (`ResolvedAuth.LegacyForm`: no providers,
  no access block) keeps the v0.11 behaviour; a new-form config does not register the route. The route is the
  exact path `/admin/clients`, so the by-id GET and DELETE branches were never reachable through the mux;
  left as is. The example passes `WithAdminRole("admin")`.
- Chooser: same headers as consent (shared `setPageHeaders`), POST /authorize/provider {request, provider}, 8 KiB
  body, unknown provider / unknown or expired request → 400; choosing again re-binds nonce and verifier.

## mcp package (role-gated tools)
- `ServerConfig.RolesFromContext func(ctx) []string` (nil = no gating).
- `ToolRegistry.RegisterWithOptions(tool, fn, opts ...ToolOption)` (see As built: not a variadic on Register);
  `mcp.RequireRoles("admin", "writer")` (any-of).
- tools/list hides tools the caller lacks; tools/call on one → same -32602 as an unknown tool (no oracle).
  Applies to the compat (legacy) path too. When any tool is gated, list results get cacheScope "private".

### As built (step 3)
- Deviation: options go on a new `RegisterWithOptions(tool, fn, opts ...ToolOption)`; `Register(tool, fn)` keeps
  its exact signature. A variadic Register still compiles for direct calls, but onenote-mcp and youtube-mcp
  declare `Registrar` interfaces with `Register(mcp.Tool, mcp.ToolFunction)`, which a variadic method no longer
  satisfies (their builds failed against the tree with it).
- Gate = any-of over `RolesFromContext(ctx)`, read once per request. `RequireRoles()` with no roles hides the tool
  from everyone (fail closed); a set `RolesFromContext` returning nil hides every gated tool. Only a nil func
  turns gating off (stdio, UNIX, HTTP without auth).
- tools/list filters before paginating (cursors are offsets into the caller's own list). tools/call gates before
  `x-mcp-header` validation and requestState verification, whose errors would otherwise confirm the tool; the gate
  is checked again in `registry.call` under the lock that fetches the function, so a concurrent re-Register behind
  a gate cannot run for an unauthorized caller. Re-registering without `RequireRoles` ungates.
- cacheScope "private" applies to tools/list only, while `RolesFromContext != nil` and some tool is gated.
- Caller-independent, by design: server/discover declares `tools` whenever any tool is registered (gated or not);
  `notifications/tools/list_changed` goes to every listener and carries no params beyond `_meta`, so it names no
  tool (registering a gated tool is visible to role-less listeners only as "something changed").
- compat needs no code: `forward` and `StreamNotifications` call the inner server with the request ctx. Tests in
  `compat/gating_test.go` prove list, call (whole-response equality with an unknown tool) and listen.
- Example: roles admin/reader; `confirm_delete` gated to admin; `RolesFromContext` and `PrincipalFromContext`
  (`Principal.ID`) wired only in http mode with auth on (elsewhere no request carries a principal, so gating would
  hide the tool from everyone). Auth service now built before `mcp.NewServer`. `examples/go-mcp/main_test.go`
  loads config-oauth-example.yaml and starts NewAuthService with and without the declared roles.
- Docs deviation: the "Upgrading to v0.12.0" note says config- and API-compatible, not "fully backward
  compatible", and lists the behaviour changes from steps 1-2 (identityMaxAge ends refresh-forever, one 401 for
  org/team-admitted pre-v0.12 users, unreadable *File secrets refuse to start, GitHub lookup failures drop groups,
  GitHubLogin-based principal ids).

## Steps
- [x] config: Providers, Access, validation (+ legacy mapping) with table tests (`config.AuthConfig.Resolve`)
- [x] auth: Provider interface; GitHub provider (refactor, enterprise URLs, groups); OIDC provider (discovery,
      JWKS, id_token verify, userinfo, PKCE+nonce) with an httptest fake IdP
- [x] auth: rules engine + roles (WithRoles, validation, evaluation), Principal, context accessors; Middleware
      evaluates per request (403 insufficient_scope); GitHub callback decides through the engine
- [x] auth: chooser page; /callback dispatch; refresh-grant re-check + identityMaxAge; user store keyed by
      provider identity, legacy users read as github
- [x] mcp: RolesFromContext + RequireRoles gating (modern + compat), cacheScope private
- [x] example app: declares roles, one gated tool; config-oauth-example.yaml with github + oidc + rules
- [x] docs: README auth section (+ Upgrading to v0.12.0, consumption-skill pointer removed), HTTP-TRANSPORT.md,
      LEGACY-COMPAT.md note on gating
- [x] delete .claude/skills/generic-go-mcp-consumption (user, 2026-11-26): ~/src/skills/golang-mcp-2026-07-28 becomes
      the one build-or-migrate skill; its content is folded in there first
- [x] go vet, go test -race ./...; consumers build against the tree via go.work
      (step 2 run, 2026-11-26: bin-inventory, mcp-kube-baker, onenote-mcp, youtube-mcp pass; genome-vcf-mcp
      `TestOAuthOnlyEndToEnd/user_removed_from_the_allowlist_is_refused_per_request` fails: 401 "Sign in again",
      wants 403. Cause: step 1's Middleware branch answering 401 for records with no snapshot. Proposed narrow
      fix: 401 only when some rule for that provider has a groups/orgs/teams matcher (only then can a re-login
      change the outcome), else 403 as v0.11. Not applied.)
      (step 3 run, 2026-11-26: same five build, vet and test clean except the same genome-vcf-mcp case; a variadic
      `Register` broke onenote-mcp and youtube-mcp `Registrar` interfaces, hence `RegisterWithOptions`.)
      (fix round 1, 2026-11-26: the 401 narrowed to "a group rule exists for the provider" (or an issuer
      mismatch); all six consumers build, vet and test clean, genome-vcf-mcp's removed-user case included.)
      Migration note for docs (done in README "Upgrading to v0.12.0"): mcp-kube-baker, genome-vcf-mcp, bin-inventory and onenote-mcp build their principal
      as "github:"+User.GitHubLogin. GitHubLogin is empty for OIDC users and for GitHub providers not named
      `github`, so a consumer must move to auth.PrincipalFromContext before enabling such a provider, or all
      those users collapse into the principal "github:".
- [x] adversarial review (security, compat, correctness) and fixes

## Review
- Two review rounds (OIDC security, authz logic, compat, test quality): 19 findings, all fixed with tests that fail
  without the fix (see the deviations recorded above). Verified: gofmt, vet, go test -race, -count=20 on auth; all six
  consumers build, vet and test against the tree through a scratch go.work; example server end to end against a fake
  OIDC IdP (chooser, PKCE+nonce, roles, gated tool hidden and refused as unknown, admin API by role).
- Found while rebuilding the skill: tools/call accepted inputResponses with no requestState, skipping the signed
  round trip (a client could pre-confirm a confirm-before-act tool). Now -32602 before the tool runs
  (mcp/tools.go); TestMRTRRoundTrip covers it and fails with the check removed.
- README's `go get` line said @v0.2.0; now @v0.12.0.
- The repo-local skill is deleted; ~/src/skills/golang-mcp-2026-07-28 is the one build-or-migrate skill.

---

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
