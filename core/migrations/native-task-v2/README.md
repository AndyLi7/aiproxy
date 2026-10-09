# Native task storage v2 — review before application

Adds one nullable column, `native_tasks.public_error`, to a table created by
[native-task-v1](../native-task-v1/README.md) before 2026-10-09. It holds JSON
`{"issues":[{"field":"voice","rule":"unsupported_value"}]}` for tasks that
failed with `invalid_parameters`: field paths and rule codes only, never
provider text or the customer's values.

**Apply it before deploying the gateway binary that has the column.**
`NativeTaskStorageReady` checks every `NativeTask` field, so a binary ahead of
the schema disables all native execution: submissions, task reads and the
recovery worker that settles refunds answer `native_execution_unavailable`.
Older binaries ignore the extra nullable column, so the order is safe.

The column belongs in the **gateway log database** (`LOG_SQL_DSN`, or the main
gateway database when that is unset), not the application database.

1. Confirm the target database without disclosing secrets. Back it up and read
   the current `native_tasks` columns. If `public_error` already exists (a
   table created from the current v1 script), there is nothing to apply.
2. Execute the dialect script once:
   - `sqlite.sql`: plain `ADD COLUMN`; it fails, and changes nothing, if the
     column exists.
   - `postgres.sql`: one transaction with `lock_timeout`, a schema guard and
     `ADD COLUMN IF NOT EXISTS`; a nullable column without default is a
     metadata-only change.
   - `mysql.sql`: DDL commits implicitly and has no `IF NOT EXISTS`; inspect
     first and record the statement.
3. Verify the column, then deploy the binary.

**Deploy the application first** (or together): the change that accepts the
optional `errorIssues` of a native trial and the admin native task detail.
Earlier applications parse trial answers strictly, so every admin trial that
fails with `invalid_parameters` would fail to read. Order: application, this
ALTER, gateway binary.

Rollback means deploying the previous binary and keeping the column. Do not
drop it while failed tasks may still be read. Earlier binaries do not know
the `invalid_parameters` code: they never refund or settle such a task, so
recovery would retry it forever and the customer's hold would stay. Before
rolling back, let recovery settle them; this must return 0:

```sql
SELECT count(*) FROM native_tasks
WHERE error_code = 'invalid_parameters' AND billing_settled = false;
```

Otherwise roll back only to a binary that knows `invalid_parameters`. Settled
tasks stay readable; earlier binaries show them with the generic failure
message, without issues.

A new database is created from the v1 scripts, which are generated from the
current model and already include the column; do not apply v2 to it.
