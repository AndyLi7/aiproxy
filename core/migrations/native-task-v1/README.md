# Native task storage v1 — review before application

These additive scripts create `native_tasks` and its recovery/owner indexes in the
**gateway log database**, which may differ from the gateway configuration database.
They are not registered in startup auto-migration and have not been applied to any
live database. Production application requires review and authorization.

Generate review copies without a database connection or environment credentials:

```sh
go run ./cmd/native-task-migration -dialect sqlite
go run ./cmd/native-task-migration -dialect postgres
go run ./cmd/native-task-migration -dialect mysql
```

These scripts are regenerated from the current model, so they create a new
table with every current column. A table created from an earlier copy is
upgraded by the reviewed additive scripts in [native-task-v2](../native-task-v2/README.md)
(`public_error`, 2026-10-09), applied before the matching binary is deployed.

The command prints DDL only. It has no apply option. PostgreSQL/MySQL dialect
planning disables connection probes; SQLite uses an ephemeral in-memory database.
MySQL payload fields use LONGTEXT so bounded native JSON larger than 64 KiB fits.
SQLite execution is covered in temporary-database tests. PostgreSQL/MySQL DDL is
planned from their Gorm dialects but still needs execution rehearsal on those
engines before production use.

Application procedure for an authorized operator:

1. Confirm the selected LOG_SQL_DSN/SQLite log database without disclosing secrets.
   Back up that database and inspect whether `native_tasks` already exists.
2. If absent, review the dialect script against the deployed model version, then
   execute the script. Use a transaction for SQLite/PostgreSQL. MySQL DDL commits
   implicitly, so record each statement and inspect any partial failure.
3. If present, do not drop, recreate or blindly re-run this script. Compare its
   fields, primary key and indexes and prepare a separate reviewed repair.
4. Verify all fields, the ID primary key and four indexes. Native runtime checks
   every expected field and refuses incomplete storage. Wallet configuration and
   owned-artifact configuration are separate gates; a created table alone does
   not prove request execution or publication readiness.
5. Rehearse submission, owner isolation, restart/recovery and durable delivery in
   an isolated environment before enabling real channels.

A repeat apply deliberately fails at CREATE TABLE rather than silently blessing
an incompatible existing table. Rollback means disabling native admission and
retaining the table while outstanding requests, billing and artifact delivery
finish. Do not delete task rows or drop the table as an automatic rollback.
