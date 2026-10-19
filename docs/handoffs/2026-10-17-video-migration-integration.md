# Native video delivery and migration integration — local review candidate

## Scope and bases

- Isolated branch `codex/b2-video-migration-integration`, based on `8dde1d4` (A protected renewal precision fix).
- Integrated B patches `4fa1be5`, `e4c1cb2`, `4ef2407`, `8cb66c9` without changing the original A, B, or coordinator worktrees.
- A ancestors `f43ace3`, `9132589`, `205c9bd`, `cc40a6e`, `d72940a` remain in history. Protected `ExtendNativeRecovery` is retained, rather than B's older implementation.
- No database schema change, deployment, push, real bucket access or paid generation. Protocol extensions remain optional and all continuation/use closure limits remain unchanged.

## Integration findings and fixes

1. **Worker label is not claim authority.** B originally passed `owner` (the worker label) into long delivery. A returns a new random `RecoveryOwner` for each claim, including same-worker same-second reacquisition. `recoverClaim` now passes that exact claim token and preserves A's original-until context and outer Prepare/Accept/release/exit transaction. Both imported B tests now assert that the worker label cannot extend a claim.
2. **Context-cloned GORM handles are not the admission storage identity.** B replaced `Engine.DB` with `DB.WithContext`. A intentionally rejects a different DB/LogDB handle for activated admission. The first combined regression run reproduced unavailable admission throughout poll/billing. The fix keeps the original Engine.DB and only attaches recovery context to specific task transitions and receipt writes; it does not loosen A's storage-identity checks. `TestVideoMigrationActualRecoveryLongDelivery` exercises activated admission plus all invocation/handoff extensions and would fail with the cloned handle.
3. **Receipt and task writes need the current lease after locks and after writes.** `WithNativeRecoveryGuard` locks the task, checks the exact owner against fresh database time, executes the synchronous transition, then rechecks before commit. It covers archive checkpoint, prepared/completed delivery, accepted result/poll failure/timeout, and the pair of wallet receipt and terminal/log writes. Wallet actions check authority before each call. Replacement owners and late replies cannot save into the new claim; failed validation rolls the whole local transaction back. Ownerless legacy calls retain their behavior.
4. **Preserve the actual delivery budgets.** RecoverOnce retains its normal 90-second poll child, but Deliver uses the original recovery parent for its existing ten-minute archive context after protected extension to eleven minutes. Generation, five-minute waiting, two-minute dispatch, frozen contract and monetary rules are not changed or added together. Owned callbacks and their synchronous cleanup return before A's outer handoff release/exit; no new background worker is introduced.

## New actual call-chain coverage

`core/common/nativetask/video_migration_integration_test.go` runs real Engine RecoverOnce → Poll → Deliver → SyncBilling → handoff code, with synthetic provider output, artifact receipt and wallet only:

- live protected extension, actual ten-minute archive context / eleven-minute lease; wait naturally past the original claim deadline, finish delivery and observe all invocation returns and outer exit;
- replacement before extension, replacement while archiving, cancellation from archive's deferred cleanup, and a late settled wallet receipt after owner replacement;
- real PostgreSQL second-connection receipt row locks: live, natural expiration, owner replacement;
- real PostgreSQL second-connection lock at the actual Deliver → ExtendNativeRecovery call: live continuation or original deadline naturally expiring during the wait. Expired work never calls archive and retains accepted/active handoff evidence.

The short original-claim expiry is obtained by passing a historical scheduler timestamp to the real ClaimNativeRecovery caller, without changing production clocks or budgets. This validates the long-delivery wiring; it is not a claim that this run spent ten real minutes downloading a video.

`core/model/native_recovery_fence_mysql_internal_test.go` verifies real GORM-generated MySQL SQL uses UNIX_TIMESTAMP and never SQLite/PostgreSQL clock syntax. This is SQL recording only, not a MySQL server-lock test.

## Evidence and limits

Local logs are under `/private/tmp/b2-video-migration-*.log`.

- Four runtime/controller packages: final combined race result 474 roots / 1034 test events = 1012 pass, 22 opt-in fixture skips. The two PG opt-in roots are separately executed with real PostgreSQL below.
- Related model native/admission/async-renewal race run: 138 roots / 513 test events = 454 pass, 59 opt-in fixture skips, zero failures (`b2-video-migration-model-final.log`).
- PostgreSQL race: 6 roots / 32 passing test events, including 13 actual call-chain events and 19 retained extension/handoff regression events. The tightened cancellation assertion also passed its five-event PG rerun.
- `go build ./core/...` passed. (Repository uses go.work, so top-level `go build ./...` is not a valid module pattern.)
- Full model-package exploratory run was stopped during the unrelated large CallStatusSQLMatchesGo SQLite matrix; it is not reported as a full-suite pass. The targeted model run covers the changed recovery/admission paths.
- External wallet/Nitro/256 MiB app listener fixtures remain skipped in this run. B's previously accepted evidence is preserved but is not described as a new cross-service rerun here. The ordinary streaming/range/hash/MIME/auth unit regressions ran.
- Local PostgreSQL uses a fresh loopback-only container and test-created schemas. Container, its proven-owned anonymous volume, and temporary credentials were removed before handoff.
- Security script plus manual changed-file review: no findings. Review covered frozen-origin URL validation, no provider credential forwarding, byte/type/hash/range bounds, authenticated group/token lookup, exact owner checks and transaction rollback.

All admission uses remain open; continuation_state remains unproven. This does not prove full C, safe-disable, real issuer, all replicas fenced, independent recovery watermarks, or production readiness. No production SQL was executed.
