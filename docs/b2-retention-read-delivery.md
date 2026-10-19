# P0 migration retention diagnostic

Base: `63f4e707d38d72e0a114e0b2e4ee384c62f95f72`. Local-only implementation;
no application/UI wiring, schema migration, execution gate, supplier request,
wallet mutation, deployment or use-closure endpoint.

## HTTP and interpretation

`GET /api/channel-migrations/operations/:id/retention` uses the existing strict
header-only administrator middleware, no query/body, bounded operation ID, and
`Cache-Control: no-store` even on auth/input failures. The normal gateway
`success/data` envelope contains `MigrationRetentionResult` version 1, source
`gateway-migration-retention-v1`.

- `state=ready` means this diagnostic snapshot is readable. It never means the
  channel is drained or may be disabled/revoked.
- `cutover=committed_current` requires the original complete operation reader:
  original request/receipt, both identities, complete pins, family/current head.
  The previous reader is factored into one shared transaction helper with its
  validation unchanged. A superseded/corrupt operation remains conflict.
- `retention=required` means known open uses, recorded active executions,
  unfinished handoffs or nonterminal native tasks remain. Otherwise `unproven`.
  Neither value authorizes a status change. The existing ordinary-management
  barrier/use guard, including closed historical reservations, remains intact.
- `historyCoverage=unproven`, `cacheSwitch=not_observed_here`, and
  `authority=not_evaluated_by_diagnostic` state the boundaries of this read.
  No online issuer is contacted; no runtime authority verdict is fabricated.
- `absent` means only no original operation/effects in the current validated
  storage snapshot. An owned shadow/run/retired resource without Prepared is
  conflict. Partial/unknown optional installation is unavailable, never absence.

All counts/revisions/timestamps are canonical decimal strings. Missing sources
have `counts:null` and state unavailable (unknown protocol => invalid_protocol).
Any schema query error, storage failure or source limit clears partial counts;
limit returns `retention_source_limit_exceeded`. Limits apply to a **combined
4096 rows** across uses, execution sources and native tasks, with limit+1 probes;
there is no silently truncated result. Counts describe different journal rows,
can overlap, and must not be summed as distinct user requests.

## Scope and completeness

Uses are selected by the original **physical** from-channel ID, across all epochs,
then split into exact original identity vs other historical identities. Same-key
work on another physical channel is excluded. Key revocation is separate.

Sources: relay execution, native execution, native dispatch/recovery handoffs,
image recovery/poll handoffs, image producers, measured producers/claims. The
existing schema and explicit protocol version are checked with pure SELECTs;
write-side storage helpers with UPDATE locks are not used. Rows are projected to
nonsecret binding/state/timestamp fields, not task payloads, raw prices, credentials,
executor proof JSON, wallet evidence or request text.

These are *recorded facts*, not reconstructed private executor capabilities.
Invalid bindings/states and old storage/epoch observations remain visible. A
returned invocation does not erase another active row; a returned call still has
unproven continuation. Offered/accepted scopes remain unfinished despite expired
leases. Empty owners or timer expiry never become proof of exit.

Native task observations include nonterminal, submitting/submission_unknown,
failed-without-upstream-ID plus an original open use, present/expired owners,
local billing state and unmatched native uses. `localBillingUnsettled` is only an
observation: it creates no physical-channel billing blocker and does not claim an
independent global billing-account request needs the old inference channel.
Historical refund/failed status does not prove upstream nonacceptance.

Orphan rows with no surviving use/parent cannot safely be attributed to every
physical channel. Missing historical producer coverage, unrepresented legacy
image/background tasks, independent app billing, all-replica coverage and external
recovery watermarks are **not certified** here. These gaps remain unproven even
when every observed counter is zero. No new table/TTL or broad workflow is added.

## Concurrency and application boundary

The complete original-operation validation and all counters share the existing
SQLite read transaction or PostgreSQL repeatable-read transaction with control
FOR SHARE. This is a no-DML interface, **not** PostgreSQL's READ ONLY transaction
flag (which cannot coexist with the required row lock). DB/LogDB must be the same
supported SQLite/PostgreSQL store; unsupported stores remain unsupported.

Tests use actual HTTP, SQLite WAL concurrent writer commits, and PostgreSQL
separate connections. They prove a snapshot does not mix old use state with new
execution state, and a control/head writer waits until the reader completes.
All-table INSERT/UPDATE/DELETE rejection triggers prove neither observation nor
shared helper extraction performs DML. These are local synthetic fixtures, not
production issuer/worker coverage.

App `confirmSwitch` must independently verify the exact original committed
receipt plus actual replica/cache-switch evidence. This read does not supply a
confirmation, start a drain timer, or replace the approved minimum wait and
continuation coverage. Application/UI consumption is a later batch.

## Validation

Evidence directory: `/private/tmp/b2-retention-final/` (temporary local evidence).

- `go test -race -json ./core/controller ./core/model -run
  'Test(MigrationRetention|Migration(Read|Cutover|Probe)|PreparedMigration|ChannelMigration)' -count=1`
  in repository root: 123 pass, 2 opt-in fixture skips. See `race.jsonl`.
- Final P0 HTTP/SQLite edge regressions: `go test -race -json ./core/controller
  -run '^TestMigrationRetention' -count=1`: 21 pass, zero skips; see `retention-final-race.jsonl`.
- Actual disposable PostgreSQL: `python3 reproduce-pg.py <exact-source-root>
  <output.jsonl>`; runner invokes `go test -race -json ./core/controller
  -run '^TestPostgresMigrationRetention' -count=1`: 3 pass, zero skips. See `pg-final.jsonl` and cleanup.
  Cached postgres:16-alpine, loopback random port/schema, no real DB; credentials
  only in process environment, no DSN file. Container removed in finally.
- Build separately from **core**, **mcp-servers**, **openapi-mcp** using
  `go build ./...`; root has go.work, not go.mod. Logs named `build-<module>.log`.
- Project security-scan script plus manual auth/SQL/data-exposure/state review;
  exact staged scan is required before commit. No persistence/schema/SQL changes.

Initial test corrections are retained honestly: first SQLite auth test expected
400 for an unknown query, while existing header-only middleware correctly returns
401 for any query. Only test expectation changed. The first PG run failed before
P0 in the reused initial probe fixture (nil observation). Adding the actual HTTP
response to that existing assertion did not change business behavior; the next
run and final run passed. The first failure has no proven root cause and is not
presented as a successful P0 run. Original logs: `pg.jsonl`, then `pg-debug.jsonl`.

### Security result

| Severity | Scope | Result |
| --- | --- | --- |
| Clean | 9 changed files | Script scan clean; manually reviewed header auth, no query credentials, no-store, bound operation IDs, constant SQL identifiers/bound values, no-DML transactions, bounded counts and no secret/raw payload output. |

No database containers, networks or local DSN credential files remain from these
runs. No production/staging resource or provider API was accessed.
