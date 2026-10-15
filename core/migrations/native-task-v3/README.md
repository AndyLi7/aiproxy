# Native task storage v3 — own task queue — review before application

Adds the own task queue's columns and index to a `native_tasks` table created
by [native-task-v1](../native-task-v1/README.md) before 2026-10-15 (spec
`docs/superpowers/specs/2026-10-15-own-task-queue.md` in the application
repository, §8.1):

| Column | Type | Default |
|---|---|---|
| `queue_scope` | text (`varchar(128)`) | `''` |
| `enqueued_at` | time, like `created_at` | NULL |
| `dispatched_at` | time | NULL |
| `dispatch_owner` | text (`varchar(64)`) | `''` |
| `dispatch_until` | time | NULL |
| `dispatch_after` | time | NULL |

Index `idx_native_tasks_status_queue_scope (status, queue_scope)`: the
dispatcher asks every second which scopes have waiting tasks.

**Apply it before deploying the gateway binary that has the columns. This
order is mandatory.** `ReserveNativeTask` inserts the whole row, and the new
binary writes `queue_scope` on every native task. `NativeTaskStorageReady`
checks every `NativeTask` field, so a binary ahead of the schema disables all
native execution (submissions, reads and the recovery worker answer
`native_execution_unavailable`) instead of writing bad data. Older binaries
ignore the extra columns, so applying this first is safe and needs no
rollback script.

The columns belong in the **gateway log database** (`LOG_SQL_DSN`, or the
main gateway database when that is unset), not the application database.

1. Confirm the target database without disclosing secrets. Back it up and read
   the current `native_tasks` columns and indexes. If `queue_scope` already
   exists (a table created from the current v1 script), there is nothing to
   apply.
2. Execute the dialect script once:
   - `postgres.sql`: one transaction with `lock_timeout`, a schema guard and
     `ADD COLUMN IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS`. The columns
     are metadata-only changes; the index build briefly blocks writes to the
     table (it is small: unsettled and recent tasks).
   - `sqlite.sql`: plain `ADD COLUMN`; it fails, and changes nothing, if a
     column exists.
   - `mysql.sql`: DDL commits implicitly and has no `IF NOT EXISTS`; inspect
     first and record the statements.
3. Verify the six columns and the index, then deploy the binary.

**Deploy the application's first part first** (wallet accepts `queue: true`,
`waiting` holds and settle reason `not_dispatched`). The gateway binary
settles a task that failed before it was sent (including the stop-loss's
`not_started`) with `not_dispatched` when the wallet holds no attempt, even
with `NativeQueueEnabled=false`. Order: application part 1, this migration,
gateway binary.

Rollback means deploying the previous binary and keeping the columns. First
set `NativeQueueEnabled=false` and wait until no task is queued; earlier
binaries do not know `waiting` or `dispatching`. This must return 0:

```sql
SELECT count(*) FROM native_tasks WHERE status IN ('waiting', 'dispatching');
```

A new database is created from the v1 scripts, which are generated from the
current model and already include the columns; do not apply v3 to it.
