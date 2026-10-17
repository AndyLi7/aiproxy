# BACKGROUND-JOIN-1: local cancellation and shutdown ownership

Base: `6c58a12ed460a6324cd6e061d6aed3c7509d465b`.
Branch: `codex/b2-background-join`.

## Behavior

- `main` registers `AsyncUsagePollTask` and `NativeTaskRecoveryTask` with its existing shutdown wait group before any `Wait`. Normal shutdown waits before closing the databases.
- Async polling joins its image-prepayment and upstream-billing child loops after cancellation. Native recovery joins all four started workers.
- Tick selection explicitly rechecks cancellation. A simultaneously ready tick cannot launch a new pass with an already-cancelled context. Full-batch immediate retries also stop.
- Async scans/stale-submit recovery and claims now carry the caller context into GORM; the old no-context APIs remain wrappers for existing callers. Owner, status, CAS conditions and original three-minute async processing lease are unchanged.
- Native recovery checks cancellation before each claim and uses `DB.WithContext(ctx)` for its claim query/update. Cancellation during SQL row-lock waiting aborts the query rather than installing a later owner when the lock is released.
- Async per-item wait groups, renewal stop/join, original claim deadlines and bounded detached owned cleanup are retained. A claim that won immediately before cancellation remains protected by its existing lease; cancellation is not grounds for an unowned release. Prepayment's existing five-second exit transaction runs after normal Sync return, even if the parent was cancelled.

## Verification

```sh
go test -race -json ./core/task ./core/common/nativetask ./core/model ./core \
  -run 'Test(Background|Native|AsyncUsage|Image|MainRecovery|MainRegisters)' -count=1
go build ./core/...
```

Final relevant race run: 156 roots / 700 events = 695 passed, 5 external native fixtures skipped, zero failures. The five skips are corpus execution, cross-service archive recovery, real-wallet settlement/replay, rejected submission refund, and lost begin-attempt reply; this batch does not claim those integrations were rerun. Final targeted new tests: 10 roots / 12 events, all passed. Build and the 14-file security scan passed.

Additional actual PostgreSQL 16 tests on a fresh loopback-only disposable database:

```sh
B2_G3_TEST_POSTGRES_DSN=... go test -race -json ./core/task \
  -run '^TestPostgres(BackgroundCancelledClaimLockWait|AsyncUsageRenewalStopJoinsSQLWait)$' -count=1
```

Evidence: `/private/tmp/b2-background-final/`. `reproduce-pg.py` creates a new cached PostgreSQL 16 container with tmpfs storage, a random password held only in process memory, and removes the container in `finally`. No DSN file is created. PostgreSQL coverage is two roots / six events: async/native cancelled claim lock waits plus the two existing renewal stop/join scenarios.

New tests cover a blocked poll and both child cleanups, all four native workers, cancellation with ready ticks, immediate full-batch cancellation, cancellation before and during DB queries, cancellation between native items, unchanged async lease semantics, actual HTTP billing request cancellation, and actual `main` registration of the two jobs with its shutdown wait group.

## Limits

This is local goroutine ownership only. It does not install a persistent executor handoff, close use records, authorize safe-disable/migration, or prove all replicas or remote application billing workers exited. The HTTP test deliberately leaves its remote handler blocked after local cancellation and return. Existing unknown/active evidence is not cleared by joining.

No new timeout is introduced. Existing 600-second HTTP server shutdown timeout does not bound the wait group; a worker that ignores cancellation must still actually return. A forced process kill remains an unknown outcome. Other background task families are outside this batch.

No schema, TTL, pricing, migration SQL, provider calls, production writes, push or deployment. Release remains local validation → reviewed PR → exact staging acceptance → GitHub main → fresh user production approval.
