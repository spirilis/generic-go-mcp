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
