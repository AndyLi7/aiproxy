# PREPAID-OWNED-SYNC-1 — local delivery

Base: `74e844c6dc7d8e010d5515011db88d9bbfcad0b3`. Branch: `codex/b2-prepaid-owned-sync`.

## Behavior and authority

The actual image recovery loop, immediate completed-image synchronization, and poll-winning terminal refunds now share `imageprepayment.SyncOwned`. It requires a process-private one-start permit. The optional existing protocol 5 sidecar supplies the original random owner and two-minute deadline; protocol 6 supplies Prepare/Accept and normal-return release + execution return + handoff exit in one transaction. Normal error returns record `error_returned`; panic/Goexit and failed release retain active evidence. No TTL, schema, quote, operation or amount policy is introduced.

A completed-image poll may acquire the ordinary due recovery claim only while its original persisted ProcessingToken/status/deadline is still valid. A future recovery schedule defers this attempt. A failed/timeout poll transaction may acquire terminal-priority ownership in the **same transaction** as its durable terminal decision: only if the sidecar is absent/empty. Any nonempty owner, including an expired one, defers to normal recovery. This avoids an empty-owner ABA between the terminal decision and a later claim. Handles are attached to in-memory objects only after commit. Lock order is control → task → usage → sidecar → log; network I/O is outside these transactions. Both the captured original poll deadline and the newly claimed sidecar deadline are checked with fresh DB time after writes.

Every owned wallet action and every actual application HTTP retry rechecks the original sidecar. Prepayment HTTP redirects remain prohibited by the existing client. There is no nil-handle or new-owner fallback. Missing extensions retain existing unproven compatibility; partial/corrupt installed storage rejects. No historical coverage is invented.

Controller failover accepted/failed paths and the unconfirmed-submission sweep have no original poll or private terminal eligibility. Their bare immediate Sync is removed; they persist the existing task/attempt/failed/unsettled state and defer accept/refund synchronization to the existing minute recovery scan and existing billing schedule. This can delay those immediate refunds until recovery is due. Failed tasks still refund the entire original hold under the same operation, and later upstream bills remain platform loss. Tests exercise the actual controller and sweep, then the claimed recovery through the final refund, not merely zero immediate calls.

## Durable settled receipt completion

Review identified a recovery-first starvation case: a completed task whose billing was already settled must not attempt another wallet claim forever. `CompleteClaimedPrepaidImageUsage` now locks task → usage → log and completes both usage and log from the **persisted** receipt/data, clearing only the original valid poll token. It validates exact task/usage/log operation and ownership, full `ParseImagePrepaymentQuote`, original prepaid amount equality, terminal receipt status/currency/amount invariants, and the final original deadline. Actual-cost settlement may exceed the original hold (refund zero); the legacy receipt policy remains unchanged. Invalid/missing receipts or logs remain recoverable rather than fabricating completion. It performs no wallet call and does not change any sidecar, executor, handoff or use evidence.

The same path handles Sync-success followed by poll loss: the old poll cannot write logs, a subsequent legitimate poll resumes from the durable receipt with zero extra wallet actions. Frozen quote, operation, attribution and pricing version remain unchanged. As before, prepaid usage records zero legacy price and the wallet's actual charged amount, avoiding ordinary post-consumption.

## Reproduction and verification

- Before implementation: `/private/tmp/prepaid-owned-before-1f1qi176/before.jsonl`, archived base `74e844c`; four actual worker cases fail (valid terminal handoff, future-empty terminal priority, active-owner exclusion, late wallet owner). This is synthetic local provider/wallet evidence.
- Final relevant race package logs: `/private/tmp/prepaid-owned-final-v4-packages.jsonl`.
- Model claim/handoff regressions: `/private/tmp/prepaid-owned-final-v2-model.jsonl`; no unrelated `TestCallStatusSQLMatchesGo` matrix run.
- Final PostgreSQL: `/private/tmp/b2-prepaid-owned-pg-owd7x6o_/tests.jsonl`, runner `/private/tmp/run-b2-prepaid-owned-pg.py`. Disposable cached PostgreSQL 16, separate holder/test connections, 22 leaf cases: terminal/timeout/success/receipt paths, task/sidecar/final-log waits, live/expired leases and owner changes. No MySQL instance claim.
- Actual current application wallet: `/private/tmp/b2-prepaid-owned-wallet-hi8hpjqu/gateway.jsonl`, runner `/private/tmp/run-b2-prepaid-owned-wallet.py`. Exact app `3e93cbe7883ea83d696b3feac642c67ec0438b38`, tracked `prepayment-wallet-server.ts`, disposable SQLite, loopback only. Real refund commits, original sidecar is then made expired before response delivery; old worker stops before get/receipt. Recovery replays the same operation and ledger contains one original debit plus one full refund. Old active handoff remains active; new owner's normal handoff exits. No real supplier or bucket is contacted.
- Build: `/private/tmp/prepaid-owned-final-v4-build.log` (`go -C core build ./...`).
- Test fixtures that previously assumed immediate controller Sync now verify deferred actual owned recovery. The deadline fixture's missing billing operation is corrected to match real admission. No guard is weakened to accommodate old artificial claims.

Verification totals: four complete relevant packages **926 pass / 18 skip** under race; targeted model suite **138 pass / 22 skip**. The 22 model skips are this batch's PostgreSQL leaves, all separately run and passed (**22 leaves / 23 including parent**). Of the 18 package skips, this batch's real-wallet test was separately run and passed; the remaining 17 are two prior stop/join PG cases and 15 external-fixture cases (prior accounting wallet, existing wallet lifecycle, real-wallet admission, native application/corpus/trial, SEC wallet, attribution). They were not silently counted as passes. Build exited 0. PostgreSQL container/credentials and wallet process/disposable SQLite were cleaned.

Security skill: deterministic scan plus manual full diff review passed, 18 changed files. Evidence `/private/tmp/prepaid-owned-security-final.log`. SQL uses fixed identifiers and bound values; private handles are not JSON/DB/request fields; no environment file, credential, database or generated artifact is committed.

| Level | Location | Finding | Action |
| --- | --- | --- | --- |
| clean | this delivery's 18 files | no new actionable security findings | local commit permitted |

## Remaining boundaries

This is not complete image/global executor coverage or a safe-disable/drain proof. Admission/begin/reject during controller submission retain their prior generation protection; unconfirmed terminal persistence, global wallet reconciliation, other background executors, all-replica issuer/fences and independent recovery watermark remain separate work. All uses/continuations stay open/unproven. A sidecar timeout or durable receipt is not proof of remote cancellation or old executor exit. An HTTP already accepted before owner loss cannot be undone; recovery uses the original operation/quote, not a recalculated charge.

No push, PR, deployment, production change, migration, paid call or real-data mutation was performed. Release remains independent branch → local checks → PR review → exact staging acceptance → GitHub main → merged revision verification → fresh explicit user approval before production. The original A conversation remains paused.
