# Usage attribution v1 — review before application

Key usage P2 (application plan `docs/superpowers/plans/2026-10-12-key-usage-p2.md`
§3.1). Adds, all nullable and without default:

| Table | Column | Holds |
|---|---|---|
| `logs` | `billing_operation_id varchar(200)` | the call's billing operation; every money movement of the call carries the same id. A native op is `native:` plus a 128-character request id (135 characters), so 128 would be too short. |
| `async_usage_infos` | `usage_model_id varchar(256)` | the call's canonical model id (model/capability) |
| `async_usage_infos` | `usage_source varchar(16)` | `api`, `playground` or `admin_demo` |
| `async_usage_infos` | `usage_at_ms bigint` | the request start in Unix milliseconds. `request_at` stays the pricing instant. |

and three `logs` indexes: `idx_logs_group_reqat (group_id, request_at DESC, id DESC)`,
`idx_logs_group_token_reqat (group_id, token_id, request_at DESC, id DESC)` and the
partial `idx_logs_group_billing_op (group_id, billing_operation_id) WHERE billing_operation_id IS NOT NULL`
(MySQL has no partial index and builds it over every row).

## Target

The gateway **LogDB** only: `LOG_SQL_DSN`, or the main gateway database when
that is unset. Never the application database. Production `app-postgres` and
`aiproxy-postgres` look alike; the Postgres guard refuses a database that has
`wallet_ledger` and one without `logs` and `async_usage_infos`.

## Order

1. Apply this SQL **before** deploying the gateway binary that declares the
   columns, at low traffic. This is a hard gate: no gateway P2 deploy or merge,
   staging included, before the SQL is applied and verified.
2. It is not needed by the running P1 binary, but it is **not invisible** to
   it: see "Expected errors right after step 1" under Apply.
3. The gateway P2 binary is deployed only after the application P2 is verified
   (its admit schema is strict and must accept the new fields first).

**Enforced at startup, in two layers.** Production runs AutoMigrate
(`DISABLE_AUTO_MIGRATE_DB` is not set on aiproxy), so both matter.

1. *Migration refusal.* Before LogDB AutoMigrate, a P2 binary checks an
   established Postgres or MySQL LogDB (the `logs` table exists) for the four
   columns. When one is missing it refuses to migrate
   (`model.ErrUsageAttributionSQLMissing`) instead of running `ALTER TABLE logs
   ADD COLUMN` without `lock_timeout` on the live table: with the LogDB on the
   main database startup fails; a split LogDB (`LOG_SQL_DSN`) retries every
   minute. A fresh database (no table to lock) and a development SQLite LogDB
   migrate as before.
2. *Health gate.* A P2 binary answers `GET /api/status` with 503
   `usage_attribution_storage_missing` until all four columns exist
   (`model.UsageAttributionStorageReady`, an empty projection that reads no
   data and migrates nothing). This also covers `DISABLE_AUTO_MIGRATE_DB=true`.

Railway health-checks `/api/status` for up to 300 s, so a deploy without this
SQL never alters `logs`, never receives traffic, and the previous binary keeps
serving. Without the gate every insert into `logs` and `async_usage_infos`
would fail, and those failures are only logged: video and sync measured image
calls would be delivered and never charged.

The indexes are not checked by the guard. On Postgres a P2 binary only warns
at startup (`missing or invalid logs indexes`) when one is missing or invalid;
it never builds them, because a plain `CREATE INDEX` blocks inserts on a large
table. A development SQLite LogDB gets them from startup as well.

## Apply

1. Confirm the target database without disclosing secrets (`current_database()`,
   `current_schema()`, the presence of `public.logs` and `public.async_usage_infos`
   and the absence of `public.wallet_ledger`). Read the running gateway's
   `SQL_MAX_LIFETIME` by name: it must be > 0 (unset means 60 s; see below).
   Back the database up. Choose a low-traffic moment.
2. Postgres: `psql -v ON_ERROR_STOP=1 -f postgres.sql`. **Never** with
   `-1`/`--single-transaction`: step 2 uses `CREATE INDEX CONCURRENTLY`, which
   cannot run inside a transaction.
   - Step 1 is one short transaction with `lock_timeout = 3s`. On a lock
     timeout, wait 30 s and re-run the file (every statement is idempotent);
     stop after 10 tries and look for the blocking session.
   - Step 2 builds the indexes without blocking inserts. A long-running
     transaction stalls `CONCURRENTLY`: check `pg_stat_activity` first.
3. SQLite (local development only): read `PRAGMA table_info(logs)` and
   `PRAGMA table_info(async_usage_infos)` first and skip every `ALTER` whose
   column exists (`ADD COLUMN` fails if it does), then run the rest of
   `sqlite.sql`.
4. MySQL: DDL commits implicitly and there is no `ADD COLUMN IF NOT EXISTS`.
   Run both `SELECT`s of `mysql.sql` first and execute only the statements
   whose column or index is missing; record what was run.

### Expected errors right after step 1

The gateway opens Postgres with GORM `PrepareStmt: true`: it caches named
server-side prepared statements per connection, and GORM reads with `SELECT *`
whenever a query has no `Select` (the outbox poll `GetPendingAsyncUsagesDue`,
the completed-info lookup by upstream id, log list reads). After `ADD COLUMN`
on `logs` or `async_usage_infos`, each such statement prepared before the
`ALTER` fails on its pooled connection with SQLSTATE 0A000 `cached plan must
not change result type`. GORM evicts a cached statement only on
`ErrBadConn`, so the error lasts until database/sql recycles the connection
(`SQL_MAX_LIFETIME`, default 60 s; with 0, until a restart).

- For up to `SQL_MAX_LIFETIME` seconds after step 1, expect a burst of failed
  outbox polls (they are retried), failed customer log and video-status reads,
  and `asyncUsageSettlement` alerts. This is normal.
- If 0A000 errors continue past that time, **restart** the running gateway
  service (a Railway restart; never a redeploy or rebuild, which builds from
  `main`) so it drops its cached statements.
- The application is not affected: drizzle always lists columns, and the app
  has no `SELECT *` or `RETURNING *` on `wallet_ledger`.
- The same applies to any future gateway `ADD COLUMN` on a hot table.

## Verify

Run the read-only queries at the end of `postgres.sql`: all three indexes with
`indisvalid = true`, and the four columns with lengths 200, 256 and 16 (and
`usage_at_ms` as `bigint`). An invalid index: `DROP INDEX CONCURRENTLY IF EXISTS
public.<name>;` then re-run step 2. Then deploy the binary and check that
`/api/status` answers 200 and that its log shows no AutoMigrate `ALTER` and no
missing-index warning.

## Rollback

Redeploy the previous binary and **keep** the columns and indexes. Older
binaries ignore them, and AutoMigrate never drops columns. Do not drop them
while any P2 binary may run.
