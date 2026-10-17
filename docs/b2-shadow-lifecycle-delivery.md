# Operation-owned shadow lifecycle (local commands only)

Base: `86e294f8e9275566dfa8310fcb1e1e17ad72e90b`. This isolated batch adds no execution HTTP route, app adapter, issuer, probe runner, paid call, deployment or runtime migration.

## Durable contract

Shadow storage protocol is explicitly v2. The resource's original Version=1 creation payload, RequestJSON/RequestHash, SourceSnapshotHash/SourceFamilyHash, CheckpointHash and existing HTTP creation receipt are unchanged. Added lifecycle fields are `LifecycleState`, `LifecycleRevision`, `PreparedRequestHash`, `PreparedProbeHash`, `PreparedProbeFactHash`, `PreparedAtMs`, `CancelledAtMs`. New creation atomically starts `created`; old v1 rows upgraded by the isolated draft remain `unknown`, never inferred from current absence.

Prepared has a server-written `ShadowCheckpointHash` reverse binding. A linked Prepared cannot silently become an unowned legacy operation when its resource disappears. Owned legacy Prepared rows lacking that binding are not repaired or adopted. No-shadow installations retain old Prepared v1 reads/creates/commit saves even without this optional column; installing any shadow extension requires complete v2 storage and the reverse column. Ordinary no-extension paths remain compatible. v1/partial/corrupt owned storage fails closed.

* `PrepareMigration`: under control, verify the original full source inventory checkpoint, exact operation/resource/from/to/target credential, current full pin set/rewrite and trusted probe/deployment facts. Insert Prepared and bind `created → consumed` in the same transaction; advance control once. Same-op exact replay returns the original Prepared. Different operation/request or missing/mismatching history fails. `MigrationFeatures` is a trusted server-side runtime projection, not HTTP input; it must match the original source inventory.
* `SealPrepared`: verifies both persisted directions of the link. While still prepared, also verifies the complete created source and stable family; legitimate unrelated admission/family revisions do not alter the original source hash. Once this operation seals, use the existing Prepared/current-head checks; do not rewrite SourceFamilyHash or the creation checkpoint. Commit retains atomic enable plus full pin CAS.
* `CancelMigrationShadow(ctx, operationId, resourceRequestHash, checkpointHash)`: stops this operation, retains the disabled channel/resource/probe/use/billing history. It does not require the whole old inventory or family to be unchanged. It checks exact resource structure/ownership/context, target identity/projection/disabled status and actual operation effects. `created → cancelled`; `consumed → cancelled` only by atomically applying the existing unsealed Prepared abandonment checks. Public `AbandonPrepared` follows the same atomic cancellation when owned. Exact replay is idempotent. Sealed/committed, unknown, mismatching or contradictory state is rejected.
* `ReadMigrationShadowLifecycle`: no-DML server-only diagnostic read, bound to operation/checkpoint and complete storage context. Returns created/consumed/cancelled and original preparation digest linkage. No absent-as-restart return. A consumed diagnostic is **not** current-head authorization; execution must still use the existing complete Prepared/current-head readback.

Normal save/status/delete continues to reject owned shadows even after cancellation. Cancellation is not resource disposal, executor stop/join, drain, refund or key revocation. It does not cancel a remote pending probe by inventing an acknowledgement. A future real probe producer must durably bind its run and check the lifecycle before a new side effect; current verifier is an evidence reader, not that producer.

## Transaction and recovery limits

All command writers acquire control first. New preparation/cancellation takes resource ownership before Prepared; source/channel/config locks are acquired before final comparison. Existing proof verifiers run outside the transaction, with the existing local evidence/revocation/deadline checks retained inside. Normal writers' shared control lock protects membership, including INSERT/DELETE. Read-only diagnostics use the existing snapshot/control SHARE path and perform no self-update.

Prepare resource linkage and Prepared either both commit or neither does. Cancel resource state and Prepared abandonment either both commit or neither does. Lost responses recover by exact immutable operation/request/checkpoint linkage. No name lookup, receipt repair, key replacement or ordinary status workaround is introduced. Current database absence never proves no past operation: deleting a resource, losing Prepared or whole-store rollback remains a conflict/unknown boundary; independent recovery watermark is still required.

Application integration remains gated. It needs durable creation receipt → probe run → consumed Prepared checkpoint, phase-aware source checks, and explicit retained-cancelled state. No new HTTP DTO has been published for the diagnostic. Original R2 creation GET intentionally rejects consumed/cancelled as a current creation checkpoint; a caller must not convert this conflict to null and recreate.

## SQL and compatibility

`docs/sql/b2-shadow-resources-*-draft.sql` describes fresh v2 resource storage. `docs/sql/b2-shadow-lifecycle-v2-upgrade-*-draft.sql` upgrades complete old v1 plus Prepared in a transaction, leaving old resource lifecycle unknown. Existing isolated Prepared fresh-install drafts include the optional reverse column. None is in runtime AutoMigrate. The upgrade is not an idempotent production rollout script; check exact schema and coordinate binary versions before any separately reviewed deployment. Production SQL is not authorized by this batch.

Ordinary legacy MySQL compatibility is covered only by the existing SQL/GORM recording tests, not a MySQL instance. Owned lifecycle commands remain SQLite/PostgreSQL only; no DB/LogDB cross-store atomicity is claimed.

## Verification and remaining gates

Evidence: `/private/tmp/b2-shadow-lifecycle-final/`. Final exact-tree verification: related race 132 roots / 550 test events = 549 pass + 1 existing opt-in R1 HTTP fixture skip; real PostgreSQL 13 roots / 42 pass; `go build ./core/...` passed. Final logs are `race-delivery.jsonl`, `postgres-delivery.jsonl`, `build-delivery.log`. Security scan and manual diff review cover 14 files. The first broad final attempt contained a native execution fixture failure during a wall-clock jump from 23:44:01 to 23:56:50 with Go elapsed 0.30s; its live one-minute proof expired. That log is retained, not counted as a passing run. Subsequent focused and final regressions are recorded separately.

Tests cover retained cancellation after config/family drift, exact replay, wrong-op/credential, missing either side of linkage, no-history unknown upgrade, trigger rollback, same-key old admission, ordinary mutation refusal, full prepare/seal/commit, unrelated revision growth, true SQLite query-only recovery, PG blocked source/target changes, cancel/prepare/seal races, statement-trigger no-DML reads, PG atomic rollback and actual draft replay. Fixtures are synthetic local proof producers; they are not real issuer or supplier evidence.

Real restricted-probe production, full-replica continuous fencing, independent recovery watermark, safe post-seal reverse/reauthorization and joint task/billing/executor closure remain separate gates. Immutable original probe expiry/revocation can still stop a sealed operation. This batch neither refreshes old proof nor opens complete C for deployment.

Next HTTP wire proposal (not implemented): `/private/tmp/b2-shadow-lifecycle-http-wire-proposal.md`. It requires a separately reviewed single-snapshot producer of original creation receipt + lifecycle + Prepared linkage; do not assemble it with separate reads.
