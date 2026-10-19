# Migration inventory v1 (local, read only)

Base: `1b0b992b61773c131b2f853ed88a4755ed48f3db` (C2 commands R1).

`GET /api/channel-migrations/inventory` uses the existing admin bearer-key
middleware, with a preceding header-only guard. Any query string is rejected;
all responses are `Cache-Control: no-store`. No body/query parameter selects a
subset or authorizes a migration. Response is the existing `{success,data}`
envelope. The `data` contract is defined in `MigrationInventory` and contains:

- `version:1`, `source:"gateway-persistent-inventory-v1"`, `state` (`ready`,
  `legacy`, `unsupported`), `complete` and optional non-sensitive `reasonCode`.
- Ready only: epoch, revision **decimal string**, storageGeneration, sorted
  features advertised by this binary, full sorted configModels, opaque pinHash,
  sourceSnapshotHash, channels, configs. App must reject a revision outside its
  current journal's safe integer range. Features are not fleet evidence.
- Each channel: server-generated `identity` (channelId/keyDigest/identityHash),
  `routing` (id/name/type/status/models/modelMapping/sets/priority/backupOnly/
  enabledNoPermissionBan/warnErrorRate/maxErrorRate/deletedAt),
  `identityProjection` (baseUrl/proxyUrl/skipTlsVerify/configsJson), projectionHash.
  `deletedAt` is RFC3339Nano string or null. Deleted physical rows remain included
  for history/shared-key review. Routing nil collections normalize to []/{};
  configsJson is the original DB text or null, never a float64 reserialization.
- Each config: model + rawJson containing **all original DB columns**, preserving
  config/plugin JSON strings, unknown fields, large integers and prices. pinHash
  is the exact raw table hash used by the command layer, not a hash of sanitized
  projection. sourceSnapshotHash binds the complete safe wire + control + pinHash.

The endpoint reads one consistent complete snapshot. PostgreSQL uses repeatable
read and a shared control row lock. SQLite uses its read transaction. It does not
reuse command transactions (which issue an UPDATE). Concurrent control commit
while waiting produces unsupported on serialization conflict, requiring a new
read, never a mixed revision. No shadow, journal, probe, use or control write.

Limits: at most 4096 physical channels and 4096 configs, 16 MiB encoded inventory,
32 nested review levels. Any overflow rejects the whole response; no truncation
or partial-page completeness claim. Database/schema failures, unsupported
storage/dialect, split DB/LogDB, incomplete migration tables, unknown protocol or
corrupt prepared state return unsupported. Absent admission storage or valid
never-activated state returns legacy/complete:false, not fabricated authorization.

## Credential boundary and supported review surface

No plaintext channel key, remark, balance or test-error fields are projected.
The actual typed Go ChannelIdentityOf remains authoritative. Raw channel JSON is
inspected **before** GORM serializer decoding. An iterative token parser rejects
nesting above 32, duplicate keys and multiple roots before recursive decoding.

Plugin review is a field/type allowlist taken from the actual cache, cachefollow,
think-split and stream-fake Config structs. Unknown plugin names/fields reject,
including harmless-looking strings or numbers. Web-search and patch currently
reject entirely: web-search Google/Bing specs contain independent API keys and
patch can contain arbitrary headers. The producer never assumes a key-name
blacklist proves an unknown plugin safe.

Channel configs admit the explicit outbound-policy and provider-binding fields;
model configs admit public model/capability identifiers, supported format/voice
identifier arrays, explicit quote/native-binding metadata and input-meter metadata.
The native binding contract is the already-public frozen TaskContract, validated
with the existing CompileTaskContract implementation: original public schema,
description, default and example text is preserved. Fixed parameters are separately
restricted to the actual native-platform-controls policy set (safety, sync, output
limiters, source-enum-verified URL delivery switches), never arbitrary credentials.
Unknown extensions
may contain numeric/bool/null values and their containers; unknown text (including
customCredential/authentication_token/apiKeyValue aliases) rejects. Arbitrary
private config/plugin payloads with text are deliberately not supported in this
batch; they need a separately reviewed typed projection. Public TaskContract is
the explicit exception, not an exemption for arbitrary provider config. Original unknown numeric
fields remain byte-for-byte in configsJson/rawJson. Unknown DB columns with text
also reject. This restriction can block otherwise legitimate existing models and
is an explicit coverage limit, not permission to strip fields or fabricate reuse.

After typed validation, defense in depth checks credential fields, recognizable
credential values, nested encoded JSON and values matching actual channel keys.
Credential-bearing URLs and any URL query/fragment reject rather than redact.
Errors never echo values; source queries use a discarded GORM logger. The app
must independently validate before persisting review data. No inventory payload
should be logged. Supporting a new opaque credential/config format requires
reviewing its schema first, not expanding a generic blacklist.

## Verification and remaining work

Local focused model + actual Gin router tests cover complete sets, deleted rows,
exact raw numbers/prices, hash drift, no writes, absent/partial/wrong/corrupt
storage, large revision, secret plugins/nested strings/URLs/copied keys, limits,
header auth and no-cache. Real disposable PostgreSQL 16 tests cover DML-denial
triggers and control lock wait rollback/commit/cancel with fresh snapshot recovery.
Real app b36c59b4 publisher outputs for ordinary/native vector, measured video,
FLUX schnell image and metered Eleven v4 Turbo TTS all round-trip through the
producer with exact config text and raw pinHash. A fifth actual publisher fixture
exports the Go wire to the application builder integration test. See testdata
provenance; these fixtures are local runtime executions, not live provider tests.
Race regression and build/security results are recorded in the delivery report.

This does not install or validate a real issuer, all-replica fencing, recovery
watermark, probe updater, or app prepare ownership/history classification. Ready
means a safe complete inventory, not migration permission. Commands remain
server-only; actual HTTP write adapter, safe reverse, post-seal re-evidence
recovery and full C acceptance remain separate. No schema additions/migrations,
provider calls, real bucket access, push or deployment are performed by this batch.

Final local evidence (2026-10-17): `/private/tmp/b2-inventory-final/`.

- Race: 19 root tests / 79 passing cases, no failure or skip:
  `go test -race -json ./core/model ./core/controller ./core/router -run '^(TestMigrationInventory|TestPreparedMigration|TestGetStatus|TestNativeValidationRegisteredRouteAndAuth|TestChannelRouting)' -count=1`
- PostgreSQL 16: 2 root tests / 5 passing cases, no failure or skip:
  `go test -json ./core/model -run '^TestPostgresMigrationInventory' -count=1`
  using the existing loopback-only `B2_G3_TEST_POSTGRES_DSN` disposable harness.
- `go build ./core/...` passed. A preliminary root `go build ./...` was the wrong
  go.work package pattern; the correct complete core-package build passed.
- `git diff --check` passed; security-scan script plus manual auth/data-flow/diff
  review passed across 16 changed files. No HIGH/MEDIUM findings.
- PostgreSQL container `b2-inventory-pg`, tmpfs data and temporary `dsn` credential
  file removed and absence verified; `cleanup.json` records cleanup. No dedicated
  Docker network was created.

The builder integration fixture stores the original publisher configuration under
`test/native::text-to-video`, the existing internal route key. The native contract
retains its public model `test/native/text-to-video`; the production mapping is not
reimplemented or altered by inventory.
