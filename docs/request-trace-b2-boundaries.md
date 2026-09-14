# B2 implementation boundaries

Baseline: `021713122c1d463c255ea6f013ac7f1749fb4992`.
Status: gateway B2 implementation ready for PR review. Platform C and end-to-end acceptance remain outstanding; this is not deployment readiness.
Parent design: platform `docs/superpowers/specs/2026-09-08-request-trace-design.md`.

## Verified integration points

- `core/middleware/request_trace.go` starts an unowned buffered Session before authentication.
- `core/middleware/auth.go` calls `BindRequestTraceGroup` after resolving the real group.
- `core/common/requesttrace/session.go` flushes buffered snapshots when ownership binds. After flushing, persisted identity must not be rewritten.
- `core/common/requesttrace/recorder.go` only permits locally owned parents. External parent support needs an explicit trusted path, not removal of this restriction.
- `core/controller/relay-controller.go:saveAsyncUsageInfo` creates the persisted async usage record with group, channel and upstream task ID.
- `core/model/async_usage.go` uses the log database. Correlation must use that same database, including split primary/log configurations.
- `core/trace/runtime.go` is opt-in. Missing tracing or failed correlation must leave existing business responses and settlement unchanged.

## Sequence

1. Define a versioned signed envelope and normal-path signer/verifier unit tests. Bind version, key ID, trace ID, parent span ID, group, request ID, source, HTTP method/path, issued time and random nonce. Use a dedicated service secret, never customer API keys. Do not sign request bodies or persist headers.
2. Consume nonce atomically in shared log storage after cryptographic verification and authenticated-group comparison. A process-local cache alone is insufficient for multiple gateway instances. Bound lookup time and retention; failure falls back to a new local trace.
3. Bind verified context before Session flush. Preserve the real authentication timestamps and all buffered events. No rewriting already-persisted identities. Ordinary API traffic continues to generate local contexts.
4. Persist task association separately from billing data, after successful task persistence. Associate immutable async record identity, authenticated group, channel and task identifier with the verified trace. No request-ID-only lookup, billing schema mutation, or financial side effect.
5. Resolve subsequent status/content operations only after existing task ownership checks. Preserve a distinct HTTP root per request. Background polling uses its persisted task record; never derives ownership from inbound headers. Missing/ambiguous association remains uncorrelated.
6. Enforce the existing per-trace limit across resumed sessions in storage. Record bounded aggregate observation counts if detailed polling reaches its cap; do not infer vendor completion timestamps from local observations.

## Required acceptance before claiming B2 complete

- Normal signed platform request preserves trace/root linkage and real account ownership.
- Direct API requests remain independent even when request IDs are reused.
- Task submission and later authorized polling resolve through the persisted task identity across process restarts.
- Correlation unavailable/disabled preserves response, upstream call count, retry and settlement behavior.
- SQLite and isolated PostgreSQL coverage, including split log database; concurrency and race verification for binding and nonce consumption.
- No signatures, credentials, bodies, media URLs or raw errors in stored/queryable projections.
- No deployment, trace enablement, paid upstream calls or merge as part of local implementation.

## Scope boundaries

B2 introduces the gateway contract for platform signing. Platform signing integration and platform spans must be coordinated with C; a gateway-only implementation is not end-to-end completion. C owns aggregation and diagnostics UI. A/B1 are already implemented and must not be repeated.

## Protocol decisions for the first slice

- Wire value: unpadded URL-safe base64 of canonical JSON, a dot, then unpadded URL-safe base64 HMAC-SHA256. JSON field order is the `TrustedClaims` declaration order. Sign the UTF-8 string `aiproxy-request-trace-v1.` followed by the encoded payload. This is a platform protocol, not an upstream vendor standard.
- Maximum envelope 2048 bytes. Key material must be at least 32 random bytes and independent of customer keys. Key ID selects from the server-only startup key map.
- Acceptance window: issued time at most 120 seconds old and at most 30 seconds ahead. Nonce retention ends at issued time +121 seconds. Verification uses a 100ms storage context deadline.
- Shared storage consumes a SHA256 digest of key ID, colon and nonce with a primary-key insert; duplicate insertion does not update retention. Runtime schedules hourly cleanup, bounded at 500 rows per table per tick. This can accumulate an expired-row backlog under sustained traffic; improve cleanup capacity before production enablement.
- Tests cover normal signed round-trip, unavailable nonce storage, shared-store single claim, bounded expiry cleanup, buffered/future Session identity, normal middleware persistence/header stripping and submit/poll association. Startup accepts `REQUEST_TRACE_SERVICE_KEYS`: a server-only JSON map of key ID to standard-base64 key material (32–64 bytes, at most eight keys). Empty/malformed configuration leaves local tracing; keys are never printed. Existing hourly bounded cleanup also removes expired nonce/task-association records.
- No existing service was enabled or migrated. The nonce table joins the already opt-in Trace migration when a future runtime starts with tracing enabled.

## Binding checkpoint

`Runtime.BindRequest` verifies against the authenticated group and actual method/path/request ID, then uses `Session.BindVerifiedContext`. Unavailable context falls back to ordinary `BindGroup`. Session projects buffered and future snapshots before persistence without changing recorder-local parent validation or timestamps. The service header is removed at trace middleware entry, including disabled/unsupported paths, so later adaptors cannot forward it.

For async continuation, ownership is not established solely by authentication: `getStoredVideoRequestModel` subsequently checks `CacheGetStore(group, tokenID, VideoGenerationStoreID(videoID))`. Task correlation must happen after that lookup, while provisional span identity is still buffered. Do not retrofit a new trace ID onto already-flushed spans. Preserve authenticated ownership on early route failure even when no task association can be resolved.

This ordering is now implemented for supported GET/DELETE video routes: defer owner flush until the existing lookup, and fall back to the authenticated owner at middleware completion on early errors. POST submission persists a separate association only after `CreateAsyncUsageInfo` succeeds. Association lookup matches group, token, channel and task; ambiguous matches remain local. Financial tables are not updated by correlation or its cleanup.

## Remaining B2 acceptance

- Background automatic usage polling now uses its persisted async record to resume association immediately around `FetchAsyncUsage`; verifies the association's async record ID and records only local poll/terminal-observation stages. It does not instrument settlement-only retries as upstream polls.
- Health projection now includes a correlation-failure count for unavailable signed contexts, failed association writes/lookups and background association misses. No raw error is projected.
- Real isolated PostgreSQL concurrent nonce acceptance has been verified (16 competing stores, one successful claim); existing PostgreSQL trace-store integration tests passed. This exercises shared-database coordination, not a deployed multi-process system.
- Normal-path Node JSON/HMAC signing is accepted by Go. Client implementations must follow the documented canonical encoding; no platform production signer is deployed.
- Background poll summaries retain a saturating count and first/last local observation time independently of the 256-span cap, with sliding association retention. The summary is persisted for C to expose; no claim that the current UI already displays it.
- Full `go test ./...`, `go build ./...`, race tests for `common/requesttrace` and `trace`, and 50 repetitions of the Session lifecycle concurrency test passed. One legacy concurrency test was corrected to identify the root by Session root ID instead of assuming emitter order.
- Platform signing/aggregation remains paired work with C. No deployment or paid calls have been made.

## Review handoff

- Canonical JSON is Go `encoding/json.Marshal` output: no whitespace, declaration-order fields, HTML escaping for `<`, `>` and `&`, and escaped U+2028/U+2029. The Node interoperability fixture covers ordinary ASCII fields; C must implement the same escaping, not assume arbitrary `JSON.stringify` output is identical.
- Signed POST contexts bind before persistence. Supported GET/DELETE task operations intentionally use the owner-checked stored association instead of accepting a fresh signed parent.
- Security review covered the changed authentication binding, parameterized storage queries, secret/header handling and financial-path isolation. The deterministic scanner found no HIGH findings. Four framework-specific ignore checks (`.output`, `.nitro`, Paraglide and TanStack generated files) do not apply to this Go gateway and are false positives, not accepted unresolved security findings.
- Trace remains opt-in and disabled by default. No service keys were configured. Do not merge, deploy or enable as part of this handoff.
