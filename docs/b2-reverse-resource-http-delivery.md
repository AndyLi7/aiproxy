# Reverse retired-resource HTTP slice (local work pending Git restoration)

Source baseline: primary-reviewed temporary combination
`/private/tmp/b2-gateway-read-combined-lqu6d3qw/source`, manifest in adjacent
`combination.json`: exact `07f63e81de1f74afb3b0616397f188dd921b3a80` plus the five-file
`195431a9db8d4f746e7400d07ed4268bf0eba9b5` reverse billing-context change.
Development is isolated in `/private/tmp/b2-reverse-resource-http-work`.
No final commit SHA is claimed until this diff is transferred into an independent
real worktree on the primary's matching combination and reviewed/scanned there.

## Scope

The existing header-only admin migration group now exposes:

- `POST /api/channel-migrations/operations/:id/retired-target`: strict v1 request
  `{version, command, sourceSnapshotHash}`, using the existing reverse resource
  API. Command is the canonical decimal-revision wire, with explicit
  `targetResourceKind: reuse_retired_target` and original `seal.revertOf`.
- `POST .../retired-target/read`: strict `{version:1, request:<complete original
  create request>}`. The server computes the original canonical request hash and
  checks it with lifecycle/original retirement/Prepared facts in one read snapshot.
  This recovers a lost create response before the caller knows the checkpoint;
  it cannot select a resource by guessing an operation ID or create a replacement.
- `POST .../retired-target/cancel`: strict v1 original resourceRequestHash and
  checkpointHash; cancellation uses the existing atomic terminal intent, and
  readback uses a separate consistent snapshot. Failed readback stays unavailable.

Bodies are bounded to 4096 bytes and reject unknown/duplicate/null/missing fields,
noncanonical revisions, unpaired kind/revert, and unsupported JS-range channel IDs.
All three reject query arguments, inherit header-only auth/no-store, and emit only
an explicit `gateway-migration-retired-target-v1` DTO. No raw row, key, deployment
proof, task payload or quote is exposed. Existing forward discovery/DTO bytes are
unchanged; the old forwardOnly metadata is not a reverse execution grant.

Read validates original resource request/checkpoint/storage/epoch/lifecycle,
recorded retirement and historical committed original operation. It rejects an
ambiguous shadow, damaged/missing sources, incomplete optional schemas and orphan
operation effects. It does not demand that a historical retirement remain the
current head or fabricate an original full projection. A source-absent observation
is limited to the current complete storage snapshot and never permits resubmission.
Read needs no online issuer and performs no DML.

Actual `/probe` accepts only the existing forward pair or explicit reverse pair;
Begin still validates the original resource and current authorization. Actual
`/prepare` verifies the original authorizationHash then chooses the private forward
or reverse Prepare from the persisted authorization. It rechecks the same original
authorization after billing observation; the caller cannot inject a command/kind.
Actual `/cutover` binds the original authorization/request/probe-fact/storage to the
kind-specific consumed resource, task/use/evidence and Prepared in the existing
atomic transaction. Existing final proof/DB-clock checks remain. No network is
added under DB locks. Historical sealed states cannot be adopted.

The reverse target is the already-enabled physical channel. Both channels remain
enabled; no new physical row, status/key mutation, task/quote/amount rewrite, schema,
SQL or TTL is introduced. Cancellation preserves paid-task historical billing reads
but denies new execution. Unknown acceptance preserves the original task and does
not dispatch a new supplier request. No use closure or safe-disable is implemented.

## Verification (temporary source, not final SHA evidence)

- Focused controller/model race: 102 pass, 1 skip (`TestMigrationProbeBillingInteropServer`,
  external opt-in interop fixture not started). `/private/tmp/reverse-http-final-race.jsonl`.
  Covers existing forward and private reverse compatibility, actual reverse
  resource/read/probe/Prepare/cutover/replay, lost create response, one supplier
  submission, unknown nonresubmission, cancellation with original bill readable,
  exact cutover hash errors, fake-forward reverse rejection, no-DML/no-issuer reads,
  actual ALTER DROP plus column-absence assertion, source/effect integrity and auth.
- Fresh disposable PostgreSQL16: 5 leaves plus parent = 6 pass,
  `/private/tmp/reverse-http-pg.jsonl`. Final control write (create) and final pin
  write (cutover) each tested live and natural authorization expiry under actual
  lock waits; expiry rolls back the complete revision/resource/barrier/pin changes.
  Also all-table DML-denying trigger read with original-request mismatch. Launcher
  `/private/tmp/reverse-http-pg.py <source> <log>` uses cached image, loopback-only
  ephemeral database and environment-only credentials. Cleanup confirmed in
  `/private/tmp/reverse-http-pg.jsonl.cleanup`.
- Core, mcp-servers and openapi-mcp each `go build ./...` passed; logs
  `/private/tmp/reverse-http-build-{core,mcp-servers,openapi-mcp}.log`.
- Provider/wallet/billing in these tests are local synthetic fixtures. No actual
  supplier generation, historical monetary mutation or production access occurred.
- Manual review: fixed route/auth group, bound SQL values, no body logging, explicit
  safe DTO, original-resource/hash ownership, immutable amounts and lock/clock
  checks. Final worktree/staged security-scan remains required before commit.

## Remaining integration

App57524f5 remains forward-only and rejects reverse context/journal variants.
Application reverse resource/journal/UI waits for the real switch-confirmation and
logical-pointer contract; awaiting_switch_confirmation is not success. Runtime
bootstrap/issuer/fleet/cache/recovery coverage and existing optional retired-target
schema installation are separately reviewed prerequisites. This HTTP slice does
not make them proven, enable deployment, restore missing history or authorize a
paid probe automatically. No production migration/deployment is authorized here.

Release remains isolated branch → local checks → PR review → exact-SHA staging →
GitHub main/version verification → fresh explicit user confirmation for production.
