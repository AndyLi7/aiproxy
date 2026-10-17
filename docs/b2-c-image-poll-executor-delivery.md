# POLL-EXEC-1: actual image poll invocation evidence

Base: `9181812c1111d2617e31e7b27c3e40d3c982d63a`. Branch: `codex/b2-image-poll-executors`. Local-only delivery, pending coordinator review. No push, deployment, real database/provider/bucket access or paid generation.

## Behavior and boundaries

The actual image outbox claim now atomically creates an optional protocol 7/version 1 offer when its frozen root/attempt/proof lineage is available. The private original ProcessingToken capability is attached only after commit. The worker accepts it once before starting business work or renewal, stops and joins renewal after normal body return, then records durable exit. Panic, Goexit, expired authority, canceled parent or failed receipt leave the old invocation active; a valid new claim may coexist. A returned invocation does not mean the task succeeded or all descendants returned.

The existing three-minute claim and strict renewal are unchanged. The claim re-reads the stored timestamp (including DB precision). Renewal retains the old deadline for its final DB-clock test. Task/usage/log clear-token writes and a nonce-authenticated release receipt commit together. The receipt captures the actual original deadline, never the next-poll schedule or another owner's lease. Only a real timeout terminal winner can use it for bounded existing cancel cleanup; every fal Cancel request, including the 405 fallback, checks the original authority. Existing no-redirect policy remains. Wallet protocol 5/6 is independent: its lease cannot extend poll cleanup or make poll/sibling execution returned.

Lock order is control → task → usage → poll journal, followed by existing business sidecar/log writes. Wallet claim creation uses the already-held control authority; it never starts an inner control transaction. Network calls stay outside DB locks. All touched no-context receipt APIs use the immutable parent context saved only by successful single Accept; duplicate Accept cannot overwrite it. Explicit action/renew contexts remain explicit. Return uses the parent, not the canceled renewal context and not WithoutCancel.

## Affected entry points

- `TryClaimAsyncUsageInfoContext` and the original wrapper: image claim plus offer; nonimage and absent extension keep the existing CAS.
- `processOneAsyncUsage`: Accept → renewal/body → stop/join → RecordImagePollReturn. MeasuredImage priority remains inside this boundary.
- `RenewImagePollClaim`: scoped wrapper around the existing renewal transaction helper.
- `writeClaimedAsyncUsageInfo`: generic retry/touch/fail/complete receipts.
- `withImageAccountingClaim`: measured parking/evidence/ordinary completion, and existing settled prepaid receipt completion.
- `withImagePollClaim`: result/archive/oversized failure receipts; existing data CAS and metadata preserved.
- `imageWalletPollTransaction` / `ExpireImageGeneration`: genuine terminal winner wallet claim and poll release in the same outer transaction.
- `fal.Client.Cancel`: actionguard before both actual request attempts; native/unscoped behavior unchanged.

The original quote, operation, amounts, pricing rules, negative-balance/over-prepaid behavior, three-minute poll lease and two-minute wallet lease are unchanged. No new timing policy.

## Optional SQL

`core/migrations/channel-image-poll-handoff-v1/{sqlite,postgres}.sql` are review-only drafts. They add the repeated invocation journal and marker 7/version 1 and require protocol 6. No runtime automatic migration or historic backfill. Both drafts were replayed twice against isolated fixtures. Missing extension remains legacy/unproven; partial table/marker/version/columns reject. Late-installed or DB-loaded invocations without private handles cannot fabricate coverage. MySQL without the extension stays legacy; installing this unsupported extension rejects. No MySQL instance verification is claimed.

Root and historical attempt uses/proofs are validated separately from the current contract. Durable matching executor history supports temporary proof-source outage; changed epoch requires valid fresh proof. Missing/corrupt historical roots or attempt facts and revoked membership reject. First-time missing evidence cannot produce coverage.

## Verification

All paths below are local evidence files, not production results. Test counts include parent test nodes unless marked leaf.

- Five package race regression: `/private/tmp/poll-executor-final-packages.jsonl`: 439 pass / 8 skip; task, imageprepayment, balance, fal and actionguard. External fixtures and PG tests explicitly skip when their isolated servers are absent.
- Targeted model regression: `/private/tmp/poll-executor-final-model.jsonl`: 493 pass / 88 PG skips. This broad run preceded the final context propagation addition; final impacted model/worker tests are in `/private/tmp/poll-executor-final-supplement.jsonl`: 415 pass / 2 real-wallet skips (337 model + 78 task pass).
- Real PostgreSQL 16 independent connections: `/private/tmp/poll-executor-pg.jsonl`: 57 pass, 49 leaves (18 new poll journal/protocol/log locking cases; 29 ordinary accounting lock cases; 2 SQL renewal stop/join cases). Live/expiry/corrupt outcomes include last-log lock rollback. Harness `/private/tmp/b2-poll-executor-pg.py`.
- Final parent cancellation: `/private/tmp/poll-executor-pg-cancel.jsonl`: 2 pass. No-handle Accept and accepted Touch block on protocol 7, then cancel while holder remains locked; no new coverage, no usage/log/journal changes. Duplicate Accept does not replace original parent. Harness `/private/tmp/b2-poll-executor-pg-cancel.py`.
- Actual archive loop with local synthetic object store: `/private/tmp/poll-executor-archive.jsonl`: 4 pass including parent (valid/expired/replaced owner). Losing the claim on the first action prevents its receipt and the next action. No actual object store accessed. Initial fixture incorrectly began in result_processing with no data; corrected to actual in_progress→result_processing transition, without changing production behavior.
- Real application wallet fixture: exact app `3e93cbe7883ea83d696b3feac642c67ec0438b38`, disposable SQLite, actual wallet service, no real env/database. `/private/tmp/b2-poll-executor-wallet-ew__vm9s/gateway.jsonl`: 1 pass for actual worker refund accepted remotely then lost sidecar authority, recovery same operation with exactly one debit and full refund. The wallet sibling remains active when its original invocation cannot exit; the poll may independently exit under its valid receipt. Cleanup recorded in `cleanup.json`; harness `/private/tmp/run-b2-poll-executor-wallet.py`.
- `go -C core build ./...`: `/private/tmp/poll-executor-build.log`.
- Temporary PG containers, networks and process-only credentials removed after each run. Wallet stopped and disposable fixture cleaned. Provider/status/cancel HTTP is synthetic local HTTP, never a paid fal request.

PG skips in broad model output are not all retested: only the explicitly listed 47 model leaves were run in the separate PG batch. In five-package output the two PG SQL-join leaves and the new wallet fixture were separately run; other wallet/ledger/SEC fixture skips remain skips. Earlier-batch PG evidence is not represented as new testing here.

## Nine acceptance groups

1. Claim/offer atomic failure, death before Accept leaves offered; no invented return.
2. Absent/late/partial extension, actual worker legacy versus covered behavior.
3. Same-token legitimate extension past initial bound, natural expiry then valid new owner, old active row preserved.
4. Stop/join waits for renewal cleanup; journal and final log lock expiry rolls back release or preserves active.
5. Actual worker panic/Goexit keeps active; ordinary failed completion records error_returned only after normal cleanup.
6. Release followed by new owner: old return uses authenticated old deadline; corrupt release denied.
7. Real timeout worker plus local fal HTTP: valid 405 fallback sends two requests; delayed expired first response sends no fallback; refund delay past original deadline sends no cancel; 307/308 do not follow.
8. Settled prepaid receipt completes usage/log without wallet calls and preserves active wallet sibling; successful Sync then lost poll receipt recovers with a new poll and no duplicate wallet action.
9. Historical attempts/failover and legal epoch work; corrupt/missing source or revoked proof fails; continuation/use remain unproven/open.

## Combination and remaining work

Coordinator head `160c649` has the later background Context APIs. This branch deliberately remains based on 9181812 and does not merge another author's worktree. It introduces the identical `TryClaimAsyncUsageInfoContext(ctx, info, token, leaseUntil, now)` wrapper; coordinator should combine a single implementation preserving `.WithContext(ctx)` and its actual batch scan caller. The existing original wrapper continues to work.

This covers only an actual poll invocation with an ImageTaskID and provable source. It does not prove frontend Submit/unknown upstream acceptance, no-task measured producers, global billing/reconciliation, all replicas, independent recovery watermark, or safe-disable/use closure. Those remain separate work. Unknown historic gaps, expired active poll and wallet siblings remain durable. SQL production installation requires user review.

Release order remains independent branch → local tests → PR review → exact SHA staging acceptance → merge GitHub main → verify merge version → new explicit user confirmation before production. This delivery performs local development only and does not resume original A conversation.

## Pre-commit security review

Security-scan script and manual diff review passed: SQL uses bound values or fixed dialect expressions; no new public capability input or private nonce/token serialization, no secret/config file, no new dependency or network destination, no price/amount change. Terminal cleanup uses the original nonce-bound deadline, recovery uses its existing independent owner, and no DB lease is held across HTTP. The draft SQL only appears in test fixtures and reviewed migration files.

## POLL-EXEC-R1 follow-up (parent d5f4c0f)

Coordinator review held this batch for two fixes. `ParkMeasuredImageUsage` and `PrepareClaimedImageTaskMeasurement` also enter the new journal; their no-context wrappers now use the accepted invocation's immutable parent context. A complete `rg` call-site audit of the four transaction wrappers is saved at `/private/tmp/poll-executor-context-audit.txt`; explicit-context paths remain explicit, all no-context scoped paths use `imagePollParentContext`.

The initial implementation incorrectly included `MeasuredImage` in stable poll identity, although actual `completeImageTaskUsage` legitimately freezes it from false to true. Removed only this mutable state from that hash. Existing accounting transaction comparisons of persisted mode, Price, amount, usage, operation and task binding remain unchanged. There is no deployed protocol-7 history to migrate; this optional draft remains uninstalled.

Exact parent d5f4c0f archive with only the new actual-worker test injected: `/private/tmp/poll-r1-parent-proof-n7xfrs1p/measurement-before.jsonl`. Both ordinary first-measurement paths fail: park stays pending and completion makes no charge. The three negative controls pass. Fixed exact same test: `/private/tmp/poll-executor-measurement-after.jsonl`, six pass including parent: park, complete, changed price, wrong mode, prepaid conflict. Completion uses a local in-process balance stub, not a real charge.

Final affected model race: 352 pass in `/private/tmp/poll-executor-r1-race.jsonl`. That combined run also exposed the timeout fixture's 450ms setup allowance being exhausted under race load before the intended first cancel; its worker results are superseded by `/private/tmp/poll-executor-r1-task-race.jsonl`: 88 pass / 2 external-wallet skips. Only that test setup allowance became two seconds; the actual local HTTP handler still waits past the captured original deadline. No production TTL changed. The fixed measurement controls are included in this final run.

Real temporary PG `/private/tmp/poll-executor-pg-cancel.jsonl`: five pass including parent, four leaves (no-handle Accept; Touch; measured Park; first-measurement freeze). Each waits behind protocol-7 row lock, cancels parent while holder stays locked, returns promptly and leaves usage/log/journal unchanged. Container/network/credentials removed. `go -C core build ./...` passed at `/private/tmp/poll-executor-r1-build.log`. This is the new repair verification; the broad 49-leaf PG and real application wallet checks above remain parent-batch evidence, not repeated repair results.
