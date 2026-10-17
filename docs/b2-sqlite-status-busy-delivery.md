# Ordinary SQLite channel status: bounded complete-transaction retry

Base: `4bb778b`. Independent local correction; no migration SQL, DSN/global connection changes, production operation or paid call.

## Existing failure and scope

Read-only comparison of exact parent `4bb778b`, exact cutover `72fb672`, and parent plus the staged cutover diff reproduced the same original `TestConcurrentCacheRebuildsAreSerialized` failure under `-race -count=20`: parent 20 failures, cutover 17 failures/3 passes, combination 18 failures/2 passes. The channel status, migration mutation and cache test files were identical. The fixture contains no migration tables, so the failing path is ordinary SQLite deferred transactions reading guards and then upgrading to a status write; it never reaches cutover's control transaction. Evidence: `/private/tmp/b2-cache-status-review/`.

## Change

- `UpdateChannelStatusByIDContext` adds request cancellation without breaking existing callers. The existing API delegates with Background; the admin HTTP handler passes its actual Request.Context.
- Only ordinary SQLite status transactions retry. The real `*glebarez/go-sqlite.Error` must have primary result code 5 (including extended BUSY codes). Text lookalikes, other dialects, business guards, missing rows and other failures never retry.
- There are at most 8 attempts, with cancel-aware exponential 2/4/8/16/32/64/128ms waits between them. The existing DB driver's per-operation wait remains unchanged; this is not a task/proof TTL or a global SQLite policy.
- A failed write callback must explicitly roll back successfully before another attempt is eligible. BEGIN/COMMIT failures and unconfirmed rollback do not replay. Context cancellation remains detectable even when the driver also returns BUSY/transaction-closed; successful commits still rebuild cache if cancellation arrives afterward.
- Each attempt rereads extension presence and reruns in-transaction migration/owned-shadow guards and current key checks. An extension installed during retry transfers to the existing control path, including partial-storage refusal. The previous redundant pre-transaction key read is removed; the authoritative protected read stays in the transaction.
- This is deliberately not a general retry callback API: the local closure only reads DB guards/key and writes status. Cache rebuild occurs once after final success; monitor error reset remains once in the HTTP handler after success. No external work is inside the retried closure.

## Verification

- Original concurrent cache test: 20 repeated race runs pass after the fix; original cache monotonicity/last-write/count assertions remain intact.
- Actual SQLite WAL snapshot invalidation yields driver `SQLITE_BUSY_SNAPSHOT` (517). A waiting status enable then observes a newly empty key and refuses, or sees newly installed partial migration protection and refuses; unrelated changes permit a fresh retry. Request cancellation leaves status/cache unchanged.
- A DB trigger records status effects. A genuine driver BUSY injected after actual SQL forces two complete rollbacks; the successful third attempt leaves exactly one trigger effect and one cache rebuild. This injection checks rollback behavior; it is distinct from the real concurrent/WAL conflict cases.
- Retry exhaustion, already-cancelled request, cancellation during retry, unconfirmed rollback, missing row, non-BUSY string lookalike and MySQL recording dialect refusal are covered. MySQL is recording/control-flow validation, not a server locking test.
- Existing prepared migration, owned-shadow/ordinary compatibility, original cache rebuild and legacy enabled-empty-key same-status tests are rerun. Real HTTP cancellation is verified.
- A first explicit-rollback test run exposed cancellation being obscured by simultaneous BUSY/ErrTxDone. The final implementation preserves both error causes with errors.Join; no business error is retried because of this.

Final targeted race result: 76 pass / 31 opt-in PostgreSQL skips; original concurrent test: 20/20 pass; additional actual owned-shadow guards: 2/2 pass. No new PostgreSQL server result is claimed for this SQLite-only correction. Commands and final counts are retained in `/private/tmp/b2-cache-status-review/status-final2.jsonl`, `fixed-final-count20.jsonl` and `owned-guards.jsonl` / `acceptance.json` alongside them. Actual build directories are `core`, `mcp-servers`, `openapi-mcp`, each `go build ./...`; see `build-final2.json`. Security scan and manual diff review are recorded in this evidence directory.

No branch push, deployment or database migration was performed. Existing migration-extension transactions retain their own control/proof semantics; this change does not broadly retry those operations.
