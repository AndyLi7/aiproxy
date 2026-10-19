# Forward Cutover HTTP (local implementation)

Development baseline: accepted `72fb672` plus accepted `daf5b7d`, combined only in
this independent branch by merge `23263c3`. No reverse-author changes, production
operations, migrations, provider requests, or deployments are included.

## Contract

`POST /api/channel-migrations/operations/:id/cutover` is in the existing strict
header-only AdminAuth / no-store group. Body is exactly:

```json
{
  "version": 1,
  "authorizationHash": "original probe observation digest",
  "requestHash": "original prepared receipt digest",
  "probeFactHash": "original prepared receipt digest",
  "storageGeneration": "original prepared receipt storage generation"
}
```

The body limit is 4 KiB. Unknown/case-aliased/duplicate fields, extra query
parameters, malformed operation IDs, arbitrary command/proof/config payloads and
replacement deadlines are rejected. Digests must be canonical full hashes, not
these explanatory sample strings. The application must not reconstruct Go hashes.

The server loads the original frozen command and produced immutable evidence.
The actual runtime store still requires its server-configured issuer/executor.
Missing runtime authority fails closed; no test producer or environment bypass
has been installed. This is native probe-backed forward migration only.

The private guard follows control → resource → run → task → use → prepared,
then the existing command's channel/config/family/proof locks. It checks the
original actual task, use, evidence and exact prepared receipt in the same
transaction before and after `CutoverPrepared`. The existing atomic command
checks source/checkpoint/full pins/current head and issuer/probe revocation.
After the final guard it rechecks the actual database clock against both proofs.
No network or billing refresh occurs under these locks. The old public model
method retains its existing behavior; only this narrow HTTP path requires the
additional probe binding.

Only an original `prepared` operation or exact still-authorized `committed`
replay is accepted. Historical separate `sealed` operations, cancellation,
superseded heads and missing/changed task/evidence are rejected. There are no
separate Seal or Commit HTTP endpoints.

Success returns the existing complete `gateway-migration-read-v1` result. A
failed write never becomes a successful response based on an unrelated read.
Readback failure following a commit remains conflict/unavailable. After lost
response, GET of the exact operation can verify original committed historical
facts; it does not renew authorization, imply cache acknowledgement, or authorize
another write. Natural proof expiry blocks POST replay while unchanged complete
GET facts remain readable. Revocation or changed head still blocks authoritative
readback as required by the existing reader.

## Verification and reproduction

Evidence directory: `/private/tmp/b2-cutover-http-final/`.

- `race.jsonl`: 289 passing test/subtest events, 103 opt-in skips, zero failures,
  with `go test -race -json ./core/controller ./core/model -run
  'Test(Migration.*|AtomicCutover.*|PreparedMigration.*|Postgres.*Migration.*|Postgres.*Cutover.*)$'
  -count=1`. This preceded the final test-only historical-seal case and fixture
  snapshot route addition; the final HTTP suite is recorded separately below.
- `http-final-retry.jsonl`: 15 passing test/subtest events, zero skips/failures; final HTTP race suite command
  `go test -race -json ./core/controller -run '^TestMigrationCutover' -count=1`.
  Covers actual Gin probe/Prepare/cutover, discarded response then exact GET and
  replay, strict input/auth, changed bindings/task, revoked probe, cancellation,
  missing issuer, expired write versus historical read, superseded head and
  rejection of a genuine historical separate SealPrepared.
- `pg-final.jsonl`: five passing test/subtest events (one root, four scenarios),
  actual disposable PostgreSQL 16 and actual Gin HTTP. Second connection changes
  task or immutable evidence while commands wait; both roll back. A trigger
  blocks the final pin UPDATE on an advisory lock; live completes, natural
  deployment/probe deadline expiry rolls back barrier, pins and enable together.
  Another connection sees no intermediate sealed state. Original task/submit
  count remains one. `reproduce-pg.py <precise-source> <output.jsonl>` is reusable.
- `build-directories.json`: `go build ./...` passed separately in the actual
  `core`, `mcp-servers`, and `openapi-mcp` modules (there is no root go.mod).
- Security scanner plus manual auth, input, original-fact binding, transaction,
  credential/log/response and SQL review: no findings. No new schema or dependency.

Earlier failed fixture attempts are retained, not counted as successful evidence:
`pg-first.jsonl` waited for an UPDATE while the owned-source guard correctly held
an earlier model-config SELECT lock. The corrected fixture installs an isolated
BEFORE UPDATE advisory wait and proves the final-write window. `http-final.jsonl`
used a 1.8-second synthetic grant that expired before first cutover during the
local run; the final test gives setup ten seconds, then waits for actual natural
expiry. No production lifetime or business timeout was changed.

The PostgreSQL container is removed; the cleanup file records this. Credentials
existed only in the harness process environment and no DSN file was written.

## App-driven integration fixture

`B2_PROBE_APP_DRIVEN=1 B2_PROBE_BILLING_HTTP_FIXTURE_DIR=<fresh absolute directory>`
with `TestMigrationProbeBillingInteropServer` starts the real source/resource and
publishes private synthetic metadata. The actual app must adopt the resource,
write `app-prepared`, read the actual snapshot, and explicitly POST its first
probe. This mode never POSTs a probe itself; it observes GET until the task is
pending, then drives synthetic native Poll and publishes completed task context.
`app.json` points to the actual loopback app billing route. Write `stop` to clean
up metadata/credentials. This optional cross-language fixture is not claimed as
run by this gateway batch; the app author runs the app-first vertical slice.

## Remaining gates

This does not establish a real production issuer, continuous fleet fencing,
independent recovery watermark or cache-switch acknowledgement. Those remain
required before enabling real migrations. It does not expose reverse, recover an
old sealed operation, close use/executor evidence, disable old channels, revoke
keys, or claim production readiness. No new paid generation is authorized.

## Fixture-only follow-up

The app-first loopback run found the actual first task state can be `queued`,
while the fixture previously waited only for `pending`. The fixture now permits
both legal pre-poll states, still requiring the original nonempty use binding;
production code is unchanged. An external loopback client verified GET remained
absent before its explicit first POST, then real Submit/Poll completed and the
fixture confirmed exactly one provider submission/task/use. Evidence:
`/private/tmp/b2-cutover-http-final/queued-fixture-final.jsonl`. Metadata/app
credential files were removed on exit. No new provider request was sent outside
the synthetic loopback server; this is not the app author's final UI evidence.
