# Native task storage v3 — own task queue — review before application

Adds the own task queue's columns and indexes to a `native_tasks` table
created by [native-task-v1](../native-task-v1/README.md) before 2026-10-15
(spec `docs/superpowers/specs/2026-10-15-own-task-queue.md` in the application
repository, §8.1):

| Column | Type | Default |
|---|---|---|
| `queue_scope` | text (`varchar(128)`) | `''` |
| `enqueued_at` | time, like `created_at` | NULL |
| `dispatched_at` | time | NULL |
| `dispatch_owner` | text (`varchar(64)`) | `''` |
| `dispatch_until` | time | NULL |
| `dispatch_after` | time | NULL |

Indexes:

- `idx_native_tasks_status_queue_scope (status, queue_scope)`: the dispatcher
  asks every second which scopes have waiting tasks.
- `idx_native_tasks_group_created (group_id, created_at)`: each queue decision
  estimates the waiting customers' active tasks over the last day only, never
  their whole history.

**Apply the columns before deploying the gateway binary that has them. This
order is mandatory.** `ReserveNativeTask` inserts the whole row, and the new
binary writes `queue_scope` on every native task. `NativeTaskStorageReady`
checks every `NativeTask` field, so a binary ahead of the schema disables all
native execution (submissions, reads and the recovery worker answer
`native_execution_unavailable`) instead of writing bad data. Older binaries
ignore the extra columns, so applying this first is safe and needs no
rollback script. The binary does not need the indexes; build them before
`NativeQueueEnabled` is turned on.

The columns belong in the **gateway log database** (`LOG_SQL_DSN`, or the
main gateway database when that is unset), not the application database.

## PostgreSQL (production)

1. Confirm the target database without disclosing secrets, back it up, and
   read the current `native_tasks` columns and indexes. If `queue_scope`
   already exists (a table created from the current v1 script), skip step 3.
2. Size check, to know how long the index builds take:

   ```sql
   SELECT count(*) AS rows, pg_size_pretty(pg_total_relation_size('public.native_tasks')) AS size
   FROM public.native_tasks;
   ```

3. `postgres.sql`: one transaction with `lock_timeout`, a schema guard and
   `ADD COLUMN IF NOT EXISTS`. Adding a nullable column, or one with a constant
   default, is metadata-only (no table rewrite). Locking: `ALTER TABLE` takes
   an ACCESS EXCLUSIVE lock and keeps it **until the transaction commits**;
   while it waits for running queries, and while it holds the lock, every read
   and write of `native_tasks` waits behind it. The script therefore holds
   nothing but the six metadata changes (milliseconds), and `lock_timeout`
   (5 s) abandons it rather than letting queued reads pile up behind a long
   query; rerun it at a quiet moment if that happens.
4. `postgres-indexes.sql`: `CREATE INDEX CONCURRENTLY IF NOT EXISTS`, which
   neither blocks reads nor writes, whatever the table size. It cannot run
   inside a transaction block: run each statement on its own (as `psql -f`
   does), never inside `BEGIN`/`COMMIT`. A failed build leaves an INVALID
   index: `DROP INDEX CONCURRENTLY` it and run that statement again. (Building
   the indexes inside the step-3 transaction would keep the ACCESS EXCLUSIVE
   lock, and so block reads, for the whole build; never do that on a large
   table.)
5. Verify the six columns and both indexes (`indisvalid` true), then deploy
   the binary.

## SQLite and MySQL

- `sqlite.sql`: plain `ADD COLUMN`; each fails, and changes nothing, if the
  column exists. Run the statements in one transaction.
- `mysql.sql`: DDL commits implicitly and has no `IF NOT EXISTS`; inspect the
  table first and record the statements.

## Order with the application

**Deploy the application's first part first** (wallet accepts `queue: true`,
`waiting` holds and settle reason `not_dispatched`). The gateway binary
settles a task that failed before it was sent (including the stop-loss's
`not_started`) with `not_dispatched` when the wallet holds no attempt, even
with `NativeQueueEnabled=false`. Order: application part 1, this migration,
gateway binary.

## Rollback

Rollback means deploying the previous binary and keeping the columns. First
set `NativeQueueEnabled=false`, then wait until both checks return 0.

Nothing is queued: earlier binaries do not know `waiting` or `dispatching`.

```sql
SELECT count(*) FROM native_tasks WHERE status IN ('waiting', 'dispatching');
```

Nothing failed with a queue code is still unrefunded: earlier binaries do not
know these codes and would never settle them, leaving the customer's hold in
place until the application's backstop.

```sql
SELECT count(*) FROM native_tasks
WHERE billing_settled = false
  AND error_code IN ('cancelled', 'queue_timeout', 'model_unavailable_while_queued', 'insufficient_balance');
```

A new database is created from the v1 scripts, which are generated from the
current model and already include the columns and indexes; do not apply v3
to it.
