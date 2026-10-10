# Billing operation ID migration (review required)

`2026-09-24-billing-operation-id.sql` is for PostgreSQL's `public` schema in the gateway LogDB. `AsyncUsageInfo` and `ConsumeError` are both persisted through LogDB. Verify the actual target database/schema before execution. This file has NOT been executed in production.

The migration adds a nullable `varchar(128)` field to each table, with no default, index or data backfill. Existing tasks retain NULL and the new worker falls back to their old wallet request identity. Never generate new operation IDs for legacy rows: that could charge an already-debited task twice.

The transaction uses short lock/statement timeouts. Lock timeout aborts the transaction; investigate/wait and retry rather than removing the limit under load. Re-running is allowed, but incompatible existing columns cause the verification block to abort. Tested twice against a local PGlite PostgreSQL engine with legacy rows; NULL values preserved. Production DDL remains subject to review.

Rollout after explicit review approval: compatible wallet → this SQL → gateway → validation → coordinated rotation of gateway admin and wallet internal keys. Do not change CONFIG_ENCRYPTION_KEY. Existing gateway startup AutoMigrate knows these models; do not launch the new binary against production before the reviewed migration step.

Rollback the gateway binary only; retain the additive columns and compatible wallet. Dropping columns while old/new tasks or retries are outstanding is not part of rollback.

The twelve production Go files form SEC-2. `core/task/sec_wallet_integration_test.go` is the companion-wallet integration suite, and `core/testdata/sec_rollback_integration_test.go` is copied into an export of the old gateway's `core/common/balance/` to verify old-source compatibility. No real provider traffic is needed. See the companion application's `docs/security/2026-09-24-security-patch.md` for commands, deployment baseline constraints and verification limits.
