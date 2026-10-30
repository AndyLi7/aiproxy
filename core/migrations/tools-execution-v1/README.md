# Tools storage checkpoint

These are explicit additive migrations for the internal Tools execution ledger.
They are **not** an instruction to apply a deployment. The 2026-10-30 checkpoint
is source integration only; all existing services and data must remain intact.

- `postgres.sql`: three tables and two indexes, in a transaction.
- `sqlite.sql`: the same three tables and indexes for isolated local fixtures.
- No historical task, API token, wallet or channel rows are copied or rewritten.
- No runtime HTTP path calls AutoMigrate. Missing storage fails closed.
- Before a future reviewed application, inspect for partial/pre-existing tables;
  do not replace them or hide schema mismatches using `IF NOT EXISTS`.

For PostgreSQL, a separate migration role needs CREATE on the target schema.
The runtime role needs SELECT/INSERT/UPDATE on `tools_executions`,
SELECT/INSERT/UPDATE on `tools_model_bindings`, and SELECT/INSERT/DELETE on
`tools_request_nonces`. No new sequences, ownership changes or broad grants are
needed. Existing channel/model read permissions are still required. Grants must
use the actual reviewed database/schema/role rather than a copied fixture role.

Keep `TOOLS_INTERNAL_KEY` unset until an explicit later activation review; the
internal routes return 503 without a suitable key. A key must be server-only,
shared with the authorized application, and at least 32 bytes. Admin management
routes retain the gateway AdminAuth boundary. Binding changes do not submit jobs.

A future code rollback should close Agent/Tools admission and restore the prior
accepted application/gateway pair while retaining these additive tables, keys,
receipts and operation IDs. Do not drop tables to roll back code, resubmit an
unconfirmed operation, rotate keys, restore an older database over new data, or
reset wallet/billing state. Reconcile uncertain submissions separately.

Local validation: fresh SQLite normal-main fixture; isolated PostgreSQL 17
schema with sentinel preservation and fixture row writes. PostgreSQL migration
permissions and long-running production behavior remain deployment acceptance
work; nothing was applied to staging or production during this checkpoint.
