# Stable ordinary channel routing (local review)

This slice lets ordinary publication add model routing or change priority without
creating another physical channel. It does not activate migration runtime,
release historical channels, close uses, change quotes, or change old bills.
Ordinary publisher names containing `[shadow v...]` have no authority meaning.
Actual migration-owned resources retain the existing mutation guards, including
committed resources awaiting a separately reviewed migration cleanup policy.

## HTTP contract

All three routes require the existing header-only admin authentication, reject
query credentials/query parameters, and return `Cache-Control: no-store`:

- `GET /api/channels/:id/routing`
- `PATCH /api/channels/:id/routing`
- `GET /api/channels/:id/routing/requests/:requestId`

The ordinary gateway `success/data` envelope contains fixed data fields:
`version:1`, `source:"gateway-channel-routing-v1"`, `state`, `reason`,
`observedAtMs`, `snapshot`, `receipt`. States are ready/absent/conflict/unavailable/
unsupported. Nonready errors have null facts. `absent` is only an exact-request
observation; it is not a missing-channel or generic HTTP error.

Snapshot contains `identity:{channelId,keyDigest,identityHash}`, `status`,
`revision`, `routingHash`, and `routing:{name,models,modelMapping,sets,priority,
backupOnly,providerBindings}`. Hashes are opaque server values. Arrays retain
order. Provider bindings use the existing four fields provider/id/revision/
contractHash, with the existing `sha256:` hash format. Empty bindings are `{}`;
unknown existing binding fields refuse a complete snapshot instead of being
silently deleted. Empty sets are valid. Explicit empty models remain refused for legacy channels. A channel with an
access-configuration creation receipt may return to an empty route set, with empty
model mapping and provider bindings, while preserving its physical ID and identity. Models must already exist unless the existing
DisableModelConfig setting disables that old validation.

PATCH requires exactly version/requestId/expectedIdentity/expectedRevision/
expectedRoutingHash/notAfterMs/routing. Unknown, duplicate, null mandatory fields,
oversized bodies (512 KiB), malformed canonical decimals, and nonrouting fields
are rejected. Status/key/type/baseURL/proxy/TLS/identity configs cannot be written.
Only the provider-bindings value is replaced inside the raw configs string;
other bytes, unknown fields and large integers are retained exactly.

Receipt contains requestId/requestHash/full original request/before/after/
committedAtMs. Request IDs are globally unique, bound to the entire immutable
request including its original deadline. Exact replay returns the old receipt
without another write, even after its deadline; the separate snapshot always
means the current channel, not the old after state. Soft-deleted channels have no
current snapshot; a valid retained receipt can still be read. Compensation uses
a new request ID and the old receipt's after revision as CAS; external edits stop
it. No name-based channel adoption or PUT/shadow fallback exists in this service.

## Concurrency and recovery

All supported channel mutations serialize on the physical channel row and bump
its persistent revision in the same transaction, even identical-value old PUT or
status writes. This detects value ABA. Both revision and the complete identity /
routing hash must match. Revisions and wire times are canonical decimal strings
bounded by JavaScript MAX_SAFE_INTEGER; revision overflow refuses the write.

DB-clock deadline checks run after locks and after channel/receipt writes, so
expiry rolls back all effects. notAfterMs is only the caller's original lease
upper bound, NOT proof of current application owner or a cross-database fence.
The app must freeze it and wait for uncertain old work rather than renew it on
replay. GET exact receipt takes a PG channel SHARE lock; SQLite reserves its
writer lock *before any read establishes a WAL snapshot*. Thus absent plus
observedAtMs >= original notAfterMs means a matching late write can no longer
commit. Earlier absence or any unavailable result remains unknown. Do not use
the app host clock for this inference.

For full legacy identity validation the app brackets its existing full-channel
GET with two equal routing snapshots, then writes with that same revision.
Do not reconstruct Go identity hashes in TypeScript. All supported legacy writes
advance revisions, so an intervening identity change and return is not invisible.
Raw SQL/older binaries do not honor this protocol and must not remain writers.

## Explicit installation boundary

The two adjacent SQL files are **review drafts**, not registered migrations and
not executed by startup. Stop all old writers, retain a verified backup, review
and execute the selected SQL only with environment-specific authorization, and
deploy all writers together. Initial revision 1 covers all existing physical
rows, including soft-deleted rows; it does not fabricate historical receipts.
Use a fail-fast runner: PostgreSQL psql must use ON_ERROR_STOP; SQLite must stop on the first error and explicitly ROLLBACK. Never continue a failed draft to COMMIT.
No request backfills rows. New channel creation inserts its initial state in the
same transaction. Completely absent tables preserve old CRUD but new CAS is
unavailable. Any partial installation/missing state refuses protected old CRUD
and migration writes. SQLite and PostgreSQL have executable concurrency tests;
MySQL CAS is unsupported (legacy absent-table CRUD still supported).

Covered writes: BatchInsertChannels (add/copy/import), UpdateChannel's central
persist path, status changes (admin/background), soft deletion, actual migration
shadow creation, and common forward/reverse commit. Financial usage counters,
balance/test timestamps and model config rows do not change channel identity or
routing; they are outside this per-channel revision. Existing admission/control/
resource guards are retained. No new issuer or migration activation is needed.

## Local verification and reproduction

`go test -race ./core/model ./core/controller -run
'^TestRoutingCAS|^TestRoutingHTTPAuth' -count=1` uses isolated SQLite and actual Gin.
`TestPostgresRoutingCASBoundaries` requires a fresh local PostgreSQL database named
g3_queue_test through B2_G3_TEST_POSTGRES_DSN; it creates/drops its own schema and
covers competing CAS, old PUT contention, lock expiry/cancellation, write-after-
expiry rollback and receipt reads behind a writer. Never point it at real data.

For actual app interop run `TestRoutingHTTPInteropServer` with the explicit
B2_ROUTING_HTTP_FIXTURE_DIR absolute private temp directory. connection.json is
0600 and contains only synthetic admin/channel credentials. It exposes the real
routing and ordinary channel/model-config handlers against isolated SQLite. The
client writes `stop`; the server removes connection.json and closes/deletes its
owned database. No provider endpoint is called. Set GOCACHE to the task cache.

The parent separately validates the actual app publisher against this service;
standalone DTO tests are not represented as complete publication acceptance.

## Author verification record

Final related race regression: 298 passed, 5 existing opt-in skips. Actual local
PostgreSQL: 6 boundary cases plus parent, 7 passed, no skips. All three Go modules
built. Independent reviewer ran 13 SQLite/HTTP cases and replayed both exact SQL
drafts on isolated SQLite/PostgreSQL, including retained soft-deleted rows,
repeated install rejection and rollback after partial installation. Test-only
credentials remained in private temporary fixture metadata and are removed when
the fixture stops. These are local author/reviewer results; parent acceptance and
application end-to-end acceptance remain separate.
