# Atomic prepared cutover (local model API)

Base: `a73678525321e613df204113b6b8856c68055fa0`. Independent `codex/b2-atomic-cutover` worktree. No HTTP endpoint, runtime activation, schema/TTL change, push, deployment or real service calls.

`CutoverPrepared(ctx, ChannelMigrationRequest)` takes the original exact prepared request. It loads the existing trusted deployment/probe authorization outside DB locks, then uses one control transaction with resource-before-prepared ordering. The original seal and commit transaction bodies are extracted unchanged into internal helpers; public Seal/SealPrepared/CommitPreparedPins keep their existing transaction boundaries and semantics. It does not call transaction-opening public methods from inside another transaction.

A first cutover accepts only `prepared`, forward requests. Seal writes the original barrier/family and first logical revision, then the caller rereads control in the same transaction. Commit preserves full raw-config CAS, immutable quote/unknown JSON, target enable, receipt and second revision. Every original end-of-write deployment/probe/fresh-DB-clock check remains. Failure rolls back both stages; no intermediate sealed state is externally committed. Cache rebuild happens only after successful transaction completion and is not a cache ACK.

An exact current committed replay checks original request/resource/probe identity, current head and all committed postconditions; it writes no new revision. A changed request, superseded head, historical `sealed`, abandoned operation or reverse request is refused. Response loss is resolved through the original exact `ReadPreparedMigration` / `ReadPreparedMigrationFacts` path, never by status alone. Expired authority can refuse command replay; readback remains the fact source under its existing conditions.

The first prepared operation checks two-revision capacity. From `MaxInt64-2`, successful cutover can reach `MaxInt64` and still support exact replay and readback. A private control-transaction option permits this only for CutoverPrepared and the existing readback method; all other public mutations retain their exhausted-counter refusal. Actual HTTP-facing `ReadPreparedMigrationFacts` already allowed MaxInt64; it is tested, not changed.

## Local verification

- `/private/tmp/cutover-unit-final.jsonl`: **18 pass**, race. Same/different key, atomic result/readback/replay, later head rejection, unknown large JSON/frozen quote preservation, real shadow binding, changed reciprocal binding, pin/enable/receipt failure rollback, final proof/probe revocation rollback, historical sealed/reverse refusal, exact overflow boundaries and MaxInt64 readback.
- `/private/tmp/cutover-regression-race.jsonl`: **188 pass / 3 skip**, related migration/read/shadow/old API race suite. This preceded final test-only additions; implementation unchanged. Skips are optional HTTP interoperability fixture servers (`TestMigrationReadHTTPInteropServer`, `TestMigrationShadowLifecycleHTTPInteropServer`, `TestMigrationShadowLifecycleHTTPNativeBuilderServer`), not failed tests.
- `/private/tmp/cutover-pg.jsonl`: **29 leaves / 31 including parents pass**, actual temporary PostgreSQL 16 double-connection race tests. Five new cases hold the config row until the **commit-phase UPDATE after seal** blocks: live, deployment expiry, probe expiry, external pin change, cancellation. Second connection sees original prepared/disabled/config/no-family state while waiting. Rejected attempts preserve the original operation and control state. The other 24 leaves rerun existing public prepare/seal/commit channel-lock behavior.
- `/private/tmp/b2-cutover-pg.py` / `cutover-pg-harness.log`: local cached image only, ephemeral tmpfs DB/network/password, cleaned in finally. The first cancelled-case fixture used the worker context for the holder too, causing auto-rollback/ErrTxDone; corrected to independent holder context. No business guard was relaxed. Final-proof callback fixtures use a separate GORM Statement on the same transaction to avoid inherited prepared-row conditions.
- `go -C core build ./...`: passed, `/private/tmp/cutover-build.log`.
- Security skill script plus manual review: `/private/tmp/cutover-security-{worktree,staged}.log`. No dynamic SQL identifiers from requests, no new secrets/HTTP paths, unchanged complete transaction bodies, no ordinary status bypass. Diff whitespace check passed.

## Not delivered in this slice

Historical sealed completion with refreshed authorization, protected reverse/rollback, real issuer/runtime installation, external recovery checkpoint, all-replica cache ACK and joint use closure/safe-disable remain separate. A forward atomic model command is not full migration readiness. Old identities/keys/tasks remain available according to the original prepared contract; no old use is cleared.

Release stays independent branch → local checks → PR review → exact commit staging acceptance → main → verify merged revision → new explicit user confirmation before production.
