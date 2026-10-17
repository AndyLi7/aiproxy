# Restricted migration probe and Prepare — local batch

Base: `8968cab`. This batch is local only: no upstream paid call, real database,
production/staging deployment, or automatic migration. The installed runtime still
has no proven deployment issuer or billing provenance reader, so this is **not a
claim that full migration is available**.

## Actual path and immutable authorization

`POST /api/channel-migrations/operations/:id/probe` freezes an authenticated admin
request against the operation-owned disabled shadow, original source/checkpoint,
epoch/storage/family, full raw config pins, internal billing group/enabled token,
model, public native input and maximum initial hold. Only the stored publisher
configuration supplies native contract, endpoint, credential scope/key digest,
original quote and delivery origin. The engine uses the original published maximum
hold for this administrative probe; no new price, task TTL or actual-cost ceiling is
introduced. Final billing/negative-balance behavior remains the existing engine's.

The operation owns at most one run. Its task ID is a full hash of storage,
operation and checkpoint, **only an idempotency locator**. An unexported handle
contains a random nonce; its owner hash is bound with the use and native task in
one transaction. Existing `nativetask.Engine.Submit`, execution guards, wallet
claims, provider Submit, Poll, artifact delivery, settlement and recovery remain
responsible for actual execution. Only the reserve entry branches to the private
probe reserve function; ordinary public admission still rejects the disabled
channel. No task/use or authorizing history is synthesized after an unknown result.

Control serializes source/resource/run checks. Owner/token, source and proof are
checked under locks, use/task/run binding commits atomically, and proof time is
checked again after writes. A bound run never reserves again, including if its task
was refunded/deleted; a missing run with known task/use remains unknown. Cancelling
the resource preserves accepted work/history; it grants no refund, exit or drain.

Supported execution in this batch is **fal native request/bounded_request** using
the existing frozen public task contract. Legacy image-quantity routes and other
providers are explicitly unsupported here. This is not a new schema/parameter
mapping layer and is not proof that every existing image/TTS route can probe.

## HTTP contract

All three routes use existing header-only AdminAuth, reject query credentials and
set no-store. There is no Seal or Commit HTTP route in this batch.

- POST `.../:id/probe`: version 1, `command` (existing migration-read request shape,
  decimal-string revision), `resourceRequestHash`, `checkpointHash`, internal
  `model`, public native `input`, `groupId`, `tokenId`, decimal-string
  `maxPrepaidMicros`. Strict unknown-field/duplicate/depth/body-size validation.
  No provider, key, quote, result, `passed`, proof lifetime or caller task ID.
- GET `.../:id/probe`: current observation from one no-DML consistent snapshot.
  Version 1/source `gateway-migration-probe-v1`, state/reason/operationId,
  epoch/storageGeneration/revision and nullable observation. Observation contains
  authorization/command/resource/checkpoint/spec hashes, taskId/taskStatus/useId
  and decimal creation/bind times; never input, provider task ID, key or quote.
  `absent` means current complete storage absence only, never a historical
  recovery watermark. Missing/partial/corrupt storage is not absence.
- POST `.../:id/prepare`: `{version:1, authorizationHash}`. Loads the original
  durable authorization and server-produced fact, then calls existing atomic
  Prepare (resource consume + Prepared reverse binding). Success returns existing
  complete Prepared readback. Response loss is recovered through exact operation
  readback; an unavailable post-commit read is not converted to failure/absence.

Known replay with a bound task only observes; it never starts another generation.
A consumed Prepare replay uses its original immutable evidence, not a renewed
lifetime. Task final-fact hash and persistent probe revocation are rechecked.

## Billing evidence is a separate prerequisite

Wallet `settled` only establishes the wallet state. It cannot establish that the
provider bill belongs to this credential/account. The new internal callback is
`MigrationProbeBillingReader`; its input is exact billing operation, platform task,
provider upstream task, group, provider/endpoint, migration operation/original key digest, target credential scope/key digest and
original quote hash. Its result must bind that exact tuple plus immutable evidence
ID, fetch provenance hash, accountMembershipEvidenceHash and versioned kind
`fal-workspace-key-membership-v1`, currency, actual cost, customer due,
charged amount and settlement time. The membership evidence proves the operation-time
relationship of original/target keys and billing source; it is not a permanent
provider account UUID. Amounts must be safe integers; charged/due must
match the actual receipt. Charge may exceed the initial hold. Any future HTTP wire
for this internal callback must encode integers as bounded decimal strings.

The reader is invoked outside DB locks. Task/output/owned-artifact receipts and
settlement facts are validated again under locks afterward. Final facts and bill
provenance are hashed into an immutable probe fact; its expiry is the earliest
actual deployment grant/revocation-freshness expiry. Prepare does not extend it.
The task final-fact hash is stored alongside it so later task changes cannot reuse
old proof. Persistent revocation and existing Prepare checks remain mandatory.

**Default production reader is nil and returns `missing_provenance`.** Existing
application recovery uses a global fal billing admin key and does not persist
account/key membership provenance. Existing matched or matched→late bill status
cannot fill that gap; current credential lookup cannot backfill old evidence.
The follow-up application public reader must read hold/accepted route/original
quote/reconciliation/evidence and fetch-time provenance in one snapshot, with a
trusted durable task identity binding when old wallet quote lacks key digest.
Missing historical provenance remains unavailable. Test readers are local fixtures
only, not installable manifests, browser fields or runtime configuration flags.

## Optional schema and compatibility

Two isolated SQL drafts accompany this batch: probe-runtime protocol v1 and a
run table. The run stores immutable authorization/spec JSON, full hashes, task/use
binding, private owner hash, timestamps and immutable evidence/final-task hash.
No plaintext channel credential is added. Raw input remains private server task/run
material and is never copied to HTTP observations or application review journals.

SQL is not in AutoMigrate or a production installer. Missing/partial/wrong-version
new extension blocks these new routes; original ordinary admission remains usable.
SQLite uses qualified column checks to avoid treating absent quoted columns as
string constants. New probe command storage requires the already-supported same
SQLite/PostgreSQL DB and LogDB; no cross-store atomicity or MySQL support is claimed.

## Verification and remaining execution gates

Local actual HTTP tests use a synthetic HTTP supplier and wallet with the existing
native Submit/Poll/delivery/settle engine, plus the real publisher native-video
fixture. They cover admin boundaries, input limits, one submit across replays,
open durable use/task before every wallet action, settled-without-provenance refusal,
wrong upstream/key/account/amount, late task changes, immutable Prepare/replay and
revocation. No real bucket/provider billing is exercised.

PostgreSQL tests use an isolated loopback PostgreSQL 16 container: real control-row
lock waits with live/config-change/owner-disable/proof-revoke/cancel/rollback,
two independent Go test processes racing one exact operation, missing-history
restart refusal, and draft SQL replay. They do not claim a real deployment issuer,
provider, or cross-service PostgreSQL wallet integration. SQLite draft/partial/
wrong-version and ordinary-admission compatibility are also covered.

Before full C can be exposed, independently implement and verify:
1. Real issuer identity and revocation source, continuous all-replica membership/
   admission fencing and cache ACKs, and independent recovery watermark.
2. Provider-account/key membership and immutable bill-fetch provenance producer,
   plus the consistent internal application reader described above.
3. Post-seal proof expiry/revocation recovery or safe exact reverse; forward-only
   commands must not expose Seal until this is operationally recoverable.
4. Application execution orchestration and supported-lane expansion with actual
   staging acceptance. Local joined work or completed probe never closes use or
   proves all background/accounting executors exited.

The original disabled shadow, prepared history, open use and unknown continuations
are retained. No ordinary status/save shortcut, shared-key revocation, or automatic
proof refresh is introduced.

### Local reproduction and audit

Evidence directory: `/private/tmp/b2-probe-runtime-final/`.

```sh
go test -race -json ./core/model ./core/common/nativetask ./core/controller ./core/router \
  -run 'Test(Migration|PreparedMigration|Native|PostgresMigrationProbe|EnablingChannelWithEmptyKey)' -count=1
python3 /private/tmp/b2-probe-runtime-final/reproduce-pg.py "$SOURCE" "$NEW_LOG"
go build ./core/... ./mcp-servers/... ./openapi-mcp/...
```

The PG runner accepts an exact archive source directory, starts only cached
`postgres:16-alpine` on loopback/tmpfs, places the random password in process
environment only, and removes the container in `finally`. No credential/DSN file
is retained. Every run has a `.cleanup` receipt. The independent-process test
writes only synthetic authorization/proof into a mode-0600 temporary fixture and
removes it; children receive the disposable PG DSN only through environment.

Earlier diagnostic runs remain in that directory: numeric versus string duration
in the native fixture, combined trigger DDL rejected by the PG driver, draft-runner
splitting a comment semicolon, and GORM fixture DropColumn not actually removing a
handwritten-DDL column. The final partial test uses actual ALTER DROP and asserts
the column is absent. These corrected fixture/harness issues are not described as
production vulnerabilities. The initial root `go build ./...` was invalid for this
multi-module go.work; the full three-module command above is the valid check.

Security review: strict existing admin authentication, safe fixed error output,
bounded/duplicate-rejecting JSON, parameterized SQL, same-DB atomic use/task binding,
original quote/owner/identity checks, no client proof or key, no new provider client
or unsafe redirect path, no runtime test issuer/provenance fallback. The new model
writes suppress ORM payload logging; observations contain bounded IDs/hashes only.
No dependency or environment variable is added.

| Severity | Location | Result | Action |
| --- | --- | --- | --- |
| Clean | 12 changed files, scanner and manual review | No unresolved high/medium finding | Local commit only |

Final local results: `race-delivery.jsonl` has 416 passing test events (159 roots),
28 skips (14 roots), zero failures. Skips include externally supplied corpus/wallet
and application/HTTP fixtures plus opt-in PG tests. `pg-delivery.jsonl` separately
passes 9 events (3 roots: six lock scenarios, actual two-process/restart, draft
replay); its standalone child entry skips once because it is started by the parent
test with its private fixture. This is not a missing multiprocess execution.
`build-delivery.log` passes all three modules. Do not sum old PG evidence into this
batch or describe external fixture skips as passed integration coverage.
