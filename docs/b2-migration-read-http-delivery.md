# MIGRATION-READ-HTTP-R1

Base: `0ebb52a689dd20b1bdbdd25558c6df6c5bfb4e97`.
Branch: `codex/b2-migration-read-http`. Local read capability only; no migration write endpoint, shadow/probe producer, issuer installation or execution adapter.

## Wire contract

All paths use existing header-only AdminAuth and `Cache-Control: no-store` / `Pragma: no-cache`, including denials. Any query string is refused. Authentication remains server-to-server. Existing `success/data` envelope contains these DTOs from `core/model/channel_migration_read.go`:

- Common: `{version:1,source:"gateway-migration-read-v1",state,reason}`. State is `ready | absent | unsupported | conflict | unavailable`; reason is always present and contains only fixed non-sensitive codes.
- `GET /api/channel-migrations/protocol`: ready adds `protocol:{version:1,atomicPins:true,currentHeadRead:true,forwardOnly:true,storageGeneration}`, `executionReady:false`, and explicit missing-producer/recovery/drain blockers. This is command schema/read capability, never permission to migrate.
- `POST /api/channel-migrations/snapshot/read`: body exactly `{from:{channelId,keyDigest,identityHash},to:{channelId,keyDigest,identityHash}}`. Strict case-sensitive keys, duplicate/unknown keys, arrays, missing keys, excessive size/depth and trailing JSON are rejected. Body limit 4096 bytes. Both physical IDs must differ. Ready adds `snapshot:{epoch,revision,storageGeneration,configModels,pinHash,projectionHash,familyHash}`.
- `GET /api/channel-migrations/operations/:id`: ready adds `operationId,epoch,revision,storageGeneration,configModels,readback`. The readback contains original request and receipt, exact physical from/to, enable states, full pin/projection/current-family hashes and this operation's seal `currentHead` (null before its own seal).
- `absent` is exclusive to the operation reader and includes exact operationId/current epoch/revision/storageGeneration. It requires complete installed source schemas, validated control and no prepared row, barrier or family authorization for this operation. Missing dependencies/orphaned effects are not absence.

Every int64 revision/timestamp is a decimal string: snapshot/top-level revision, request.seal.expectedRevision, receipt.sealRevision/commitRevision/committedAtMs, and currentHead.revision/recordedAtMs. The consumer must check its safe-integer boundary; Go never truncates to a JS number. Hashes remain gateway authoritative. Request `revertOf` is always present, empty for forward-only v1. Credential IDs are from the persisted request, not separately invented by a controller.

Operation IDs use `[A-Za-z0-9][A-Za-z0-9._:-]{0,127}`, matching the app reader and preventing path normalization ambiguity. Older command APIs historically accepted other strings; this HTTP path explicitly rejects such IDs (400/conflict), never misreports them as absent. A separately reviewed unambiguous historical query format would be needed if such records exist.

`currentHead:null` means this operation has no seal yet, not that its families have no history. A real consecutive migration fixture proves a second prepared operation remains readable with the first operation's prior family heads intact.

## Consistency and safety

New HTTP producers do not call `ChannelAdmissionStore.transaction`, whose revision self-update is a write. They use one SQL transaction: PostgreSQL repeatable-read with control `FOR SHARE`, or SQLite's consistent transaction snapshot. Other rows use snapshot SELECTs. Existing command/old-reader paths retain their UPDATE locks. PostgreSQL's `READ ONLY` transaction flag cannot be combined with `FOR SHARE`; no such flag is claimed. Actual database triggers rejecting all DML validate the no-DML API property, and a query hook checks no command `FOR UPDATE` leaks into the new reader. SQLite is independently tested with `PRAGMA query_only=ON`.

The same transaction validates original request/receipt/hash/storage/epoch, identity and enable state, complete configuration set, family authorization/current head, and persisted probe revocation/fact hash, then produces the DTO. No controller joins multiple observations. A known revision rollback, missing commit receipt with enabled target, new/deleted/changed config member, revoked probe, changed epoch/storage, stale head or abandoned operation does not produce a ready readback. PostgreSQL concurrent changes during control-lock waiting yield serialization refusal or an entirely consistent permitted snapshot, never mixed facts.

The full source is bounded by existing inventory limits (4096 rows per channel/config table, 16 MiB aggregate text, JSON depth 32). Exceeding a limit refuses the whole read; no truncated set authorizes recovery. Responses contain no raw configs, plugins, channel key or arbitrary internal error text. Probe expiry observation is not renewed proof: these APIs neither extend nor refresh the original fact and do not authorize a write.

## Reproduction

```sh
go test -race -json ./core/model ./core/controller ./core/router \
  -run 'Test(Admission|PreparedMigration|MigrationRead|MigrationInventory)' -count=1
go build ./core/...
```

Final verification: Go race 110 roots / 462 events = 461 passed, one opt-in HTTP fixture skipped in the ordinary run. That fixture was then run separately against the final source: the real app reader at `80643d54ff3b022edaa7823afde12df959d852de` passed 48/48 including actual Gin protocol/snapshot/committed/absent requests. Actual PostgreSQL: 5 roots / 42 events passed (32 existing command lock/rollback cases plus 10 new read-only cases). Gateway build, diff check and 9-file security scan passed.

Actual disposable PostgreSQL harness: `/private/tmp/b2-migration-read-final/reproduce-pg.py WORKTREE RESULT_JSONL`. It uses cached PostgreSQL 16 on loopback/tmpfs, keeps its random password in process memory and removes the container in finally. No DSN file or production database is used.

Cross-language producer: set `B2_MIGRATION_READ_HTTP_FIXTURE_DIR=/private/tmp/<unique-dir>` and run `go test ./core/model -run '^TestMigrationReadHTTPInteropServer$' -count=1`. It starts the actual Gin router with synthetic SQLite prepared→sealed→committed facts; `connection.json` contains synthetic loopback connection details for the app reader. Write `<dir>/stop` after testing; server cleanup deletes the connection file and temporary SQLite. The fixture is skipped unless explicitly requested. `/private/tmp/b2-migration-read-final/reproduce-http.py GATEWAY_ARCHIVE NEW_FIXTURE_DIR [APP_ARCHIVE]` automates startup/readiness; with APP_ARCHIVE it runs the five app reader/regression files, then stops the Go fixture and removes connection metadata. Without APP_ARCHIVE it waits for the caller to write the stop file. Evidence and cleanup records live under `/private/tmp/b2-migration-read-final/`.

This batch cannot enable complete C: trusted issuer/full-replica fencing/independent recovery watermark, restricted probe production, safe post-seal recovery and joint drain evidence remain separate requirements. No paid generation, provider call, migration SQL, push or deployment is performed.
