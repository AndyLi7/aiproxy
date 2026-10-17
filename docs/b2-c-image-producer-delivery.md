# IMAGE-SUBMIT-EXEC-1 — frontend start, receipt and return

Base: `1e83db024dd9e15f8fdced688129750b34a7cca2`. Local branch `codex/b2-image-producer-executors`.

## Scope and invariants

The actual image HTTP handler now records a root producer in the existing reservation/admission transaction, before its first wallet admit. Each real new attempt use receives a linked attempt producer in the same Prepare transaction. Only the original newly-created reservation carries the private nonce/handle; reading task or use rows cannot reconstruct it. A covered root without the original handle rejects later Prepare, receipt and release rather than silently adopting a different source. Repeated HTTP requests still read the existing task and never generate again.

The accepted 69ba360 private receipt actor remains authoritative. Its queued/synchronous result/failed/unknown receipt atomically stores a signed producer receipt with the original task/usage/log transaction. Control, task, usage, producer rows and final log follow one lock order; network calls stay outside DB locks. Original operation, quote, price, metadata and timeout policy are unchanged. No new TTL exists.

The outer HTTP wrapper records producer return only after the inner handler, provider Body.Close and synchronous `finishReservedImageCall` cleanup have returned normally. Cleanup now reports its write error instead of silently permitting returned evidence. Panic/Goexit, cleanup failure and return-write failure retain active rows. A provider-accepted result whose local receipt failed may produce error_returned after normal stack/cleanup return, but its empty receipt and unproven continuation preserve the unknown acceptance window. This does not grant replay or closure.

The only use-close exception is the existing definite wallet refusal before any provider attempt: release atomically writes the original private producer's reservation_released receipt and deletes the original reservation. Root use stays open until successful frontend return commits both returned and the existing exact imageReleaseHash closure. If cleanup/return fails, active/open conservatively blocks same-request reuse; this availability tradeoff is intentional. Other roots and all attempts remain open even when returned. A DB-read copy cannot bypass this rule through the old release API.

## Optional storage

`core/migrations/channel-image-producer-v1/{sqlite,postgres}.sql` are review-only idempotent drafts. New protocol 8/version 1 was checked unused in the starting combined tree; it depends on the existing execution protocol 1. One new table stores root and attempt rows. There is no runtime migration or historical backfill.

Absent extension keeps the old uncovered behavior; late installation never adds a producer to an existing invocation. Partial/missing-column/version/storage corruption fails closed. MySQL retains extension-absent compatibility only; this batch does not prove or enable its producer extension. No real database was migrated.

## Verification

- `/private/tmp/producer-controller-task-race.jsonl`: complete controller/task race, 904 pass / 20 skip. Skips are four existing PG cancellation leaves and 16 external application-wallet/native-delivery/ledger fixtures; no prior evidence is represented as a new run.
- `/private/tmp/producer-model-race-final.jsonl`: targeted producer, original receipt and image admission race, 189 pass. This run preceded the final additional core-hash comparison in producer receipt; the changed receipt paths were rerun below.
- `/private/tmp/producer-final-focused.jsonl`: final producer model + real HTTP + original receipt/failover race, 88 pass. Includes actual provider Body.Close normal/panic ordering; >=400 cleanup error/panic/Goexit; known refusal deletion/closure; return-write failure; accepted/receipt-failure no paid resubmission; late install/partial storage; private-source rejection; start insertion rollback; original-price failover and unknown acceptance.
- `/private/tmp/image-producer-pg.jsonl`: final actual PostgreSQL 16 race, 12 leaves / 13 including parent pass. Receipt final-log and journal waits, return journal waits and original release journal waits each cover live, changed fact and parent cancellation; canceled operations leave task/usage/journal unchanged. Harness `/private/tmp/b2-image-producer-pg.py`; disposable container/network/password/DSN removed.
- `go -C core build ./...`: `/private/tmp/image-producer-build.log`, exit 0.
- Security script and manual diff review: `/private/tmp/image-producer-security-worktree.log` and `/private/tmp/image-producer-security-staged.log`. No secrets, new external endpoint, client-derived authority or unsafe SQL interpolation added.

An initial run of the older receipt fixture hit two Prepare failures before assertions, and an early cleanup test incorrectly used a wallet-claim error that returns HTTP 202 (where cleanup is intentionally no-op). The latter fixture was corrected to trigger actual post-provider receipt failure/HTTP 503; final targeted and full runs pass. No production guard/timeout/assertion was relaxed to make fixtures pass.

## Not certified

Returned means only this frontend stack returned. Provider jobs, unknown acceptance, poll, wallet recovery, background goroutines and global safe-disable are not certified finished. Receipt/outbox association is not a newly accepted poll7 handoff: poll7/no-task measured schemas or consumers were not changed. All continuation states remain unproven. Provider/wallet tests use local synthetic HTTP, no paid generation or live application wallet.

Local only: no push, deployment, production access or migration. Required publication sequence remains independent branch/local checks → PR review → exact commit staging acceptance → GitHub main merge/version check → new explicit user confirmation before production. The original A conversation remains paused.
