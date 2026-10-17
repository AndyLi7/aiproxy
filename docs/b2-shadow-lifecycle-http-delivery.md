# Owned shadow lifecycle HTTP v2

Local command/read boundary only, based on `fdf131ecac4d4c45aacba57eaecb18fa070f571e`. No schema change, online migration, probe/issuer installation, supplier request, push or deployment.

## API

Existing creation v1 GET/POST and its immutable receipt are unchanged. Added:

- `GET /api/channel-migrations/operations/:id/shadow/lifecycle`
- `POST /api/channel-migrations/operations/:id/shadow/cancel`, exact body `{ "version": 2, "resourceRequestHash": "<64 lowercase hex>", "checkpointHash": "<64 lowercase hex>" }`.

Both use the existing header-only AdminAuth group, reject query credentials (all query parameters), set no-store/no-cache on success and rejection. GET rejects a body. POST is capped at 4096 bytes; duplicate, unknown, case-aliased, missing/null fields, trailing data and unsupported versions are rejected. Operation IDs retain the existing alphanumeric-first bounded rule. There are no new seal, commit, probe or execution endpoints.

The fixed v2 envelope has `version`, `source= gateway-migration-shadow-lifecycle-v2`, `state`, `reason`, `operationId`, `epoch`, `storageGeneration`, `currentRevision`, `observation`. State is ready/absent/unsupported/conflict/unavailable. Error envelopes discard all partial observations. Revision/time fields are decimal strings; consumers must enforce their own safe numeric range before converting. Physical channel IDs use the existing number convention and are checked against the JavaScript safe-integer bound.

A ready observation contains:

- `resourceRequest`: the original, fully verified version 1 request, including original source and target projection; **no plaintext channel key**. Existing typed review rules, raw configs JSON string, bounded decoding, request digest and unique resource linkage are preserved. Current known channel keys are checked again before returning the request. This does not claim to detect arbitrary disguised secrets: permitted config structures are the existing explicitly reviewed inventory structures, not arbitrary plugin/config payloads.
- `creation`: the complete original immutable `MigrationShadowReceipt`, unchanged, with the original source hash, identities, checkpoint, creation time/revision.
- `lifecycle`: state created/consumed/cancelled and revision/preparedAtMs/cancelledAtMs strings (unperformed times are `"0"`).
- `preparedBinding`: null, or exact operationId/requestHash/probeHash/probeFactHash/shadowCheckpointHash/storageGeneration/fromChannelId/toChannelId/state from the matching Prepared row. A consumed resource requires the original reverse binding. Previously consumed cancellation requires Prepared abandoned.

`creation.requestHash` identifies the shadow creation request. `preparedBinding.requestHash` identifies the later migration command. They are not interchangeable. First-create response loss can be recovered by comparing `resourceRequest` to the application's already frozen complete intent before adopting the opaque receipt; the application must not guess Go hashes or adopt a receipt by name alone. Unrelated source drift does not alter this original request and does not prevent precise cancellation.

## Atomicity and limitations

GET uses one existing no-DML transaction: PostgreSQL repeatable-read plus control SHARE, SQLite consistent snapshot. It does **not** claim PostgreSQL `READ ONLY` transaction mode, which is incompatible with SHARE locking. Original creation, lifecycle, target identity/projection and Prepared reverse binding are read together. Damaged rows, partial schemas, unsupported versions, changed/deleted target, missing Prepared or resource with remaining known effects are never absent. Absence means only no resource or known operation effects in the current complete storage/epoch snapshot, not proof that history never existed or that a backup was not restored.

POST invokes existing cancellation and constructs this same complete observation in the **same write transaction**. A response-construction failure rolls back the cancellation. The controller does not stitch post-write queries. A lost response is recovered by exact GET and, when appropriate, identical cancellation replay. Created becomes cancelled; consumed + prepared becomes cancelled + abandoned. Sealed/committed cannot cancel. Target, credentials, original resource, probe/use/billing evidence are retained. Source/family drift is not used to rewrite old checkpoints or silently reacquire authorization.

Ready is a lifecycle observation only. Prepared state is not a current-head grant. The read deliberately does not invoke an online issuer, refresh a probe, or prove current probe validity, full pin membership, fleet fencing, executor exit, drain or deletion safety. Execution recovery still requires the separate complete Prepared/current-head read and valid proofs. The application must retain its inventory-only execution gate until the remaining protocol is accepted. Cancelled resources remain protected from ordinary management writes; deletion/credential revocation and a general cleanup lifecycle remain out of scope. The old parent empty-key same-status regression is not changed here; main integration retains the separately accepted fix.

## Verification and reproduction

Evidence directory: `/private/tmp/b2-shadow-lifecycle-http-final/`.

- Related race: `go test -race -json ./core/model ./core/controller ./core/router -run '^(TestMigration|TestPrepared|TestChannelMigration|TestPostgresMigration)' -count=1`. Final exact-tree run (`race-delivery.jsonl`): 217 pass, 49 environment/opt-in skips, no failures; skipped PG tests were separately run below. Two opt-in lifecycle/native-builder fixtures run through separate real HTTP servers for application interoperability.
- Real PostgreSQL: `python3 /private/tmp/b2-shadow-lifecycle-http-final/reproduce-pg.py <exact source> <output.jsonl>`: 49 passes. Includes control-lock waiting, read/cancel atomic observation, whole cancellation rollback on after-write reverse-binding corruption, and triggers rejecting all DML on the GET path. Fixtures are isolated PostgreSQL 16 containers, synthetic credentials in process memory, automatic removal; no DSN file.
- Actual Go → application integration passed 2/2: lifecycle states/exact-original-request rejection/cancel recovery, and real publisher inventory → V3 builder → Go create/read → SQLite shadow_ready → unrelated app source drift → Go cancel → persistent abandoned, preserving original configs. App evidence: `/private/tmp/b2-resource-commands-interop.log`, test `tests/model-ops-resource-gateway-interop.test.ts`; app changes are independently owned/reviewed. Both loopback servers were stopped and their connection metadata removed.
- SQLite GET is tested under `PRAGMA query_only=ON`. Actual Gin middleware covers auth and strict input. Transport-response loss is exercised using a real loopback HTTP server. Original v1 create/read and R1 no-DML regressions remain covered.
- `go build ./core/...` passes. An initial `go build ./...` at the workspace root was an invalid go.work scope and failed before building; that log is retained separately.
- `reproduce-http.py <exact source> <new /private/tmp dir>` starts the real Gin router with created/consumed/cancelled synthetic operations. `reproduce-native-http.py` starts an unused source containing the accepted real publisher export `testdata/migration-inventory/publisher-builder.json`, physical channel 11, for actual app builder → create → lifecycle → cancel verification. No provider call is made. Connection metadata is mode 0600; writing `stop` shuts down and deletes it, with cleanup recorded separately.

The first HTTP test exposed reuse of a disabled-only write helper when reading committed targets. Fixed only in the new read producer by separately checking the actual expected status before comparing a local projection copy; existing write guards were not relaxed. Security scanning (8 files) and manual auth/input/transaction/data-exposure review passed before commit. Only synthetic fixtures are stored in tests; credentials and local evidence remain outside Git.
