# IMAGE-ACCEPT-R1 — original frontend receipt qualification

Base: `8d9b69e0a5d5044c3c9d83c37eb42b324296ef6c`. Local branch `codex/b2-image-submit-receipts`.

## Problem and change

A real PostgreSQL two-connection counterexample showed `AcceptImageTask` reading a submitting task, then the existing `ExpireUnconfirmedImageSubmission` committing failed task/usage/log, then the old primary-key Accept overwriting task/usage back to queued/pending. The failed log and timeout fields remained. An existing failed-result writer reproduced the same ordering. Serialized terminal-first and accept-first controls establish the intended ordering.

Accept now locks the task before checking state, uses a state/usage CAS and checks exactly one task, usage and log row. Synchronous completion also locks the original task. Actual frontend dispatch calls use a private qualification captured by successful Prepare and SaveAttempt, not request IDs or public generation strings. It binds original actor/attempt/generation, task and usage billing facts, operation, route, quote, original price and log identity. A persisted read cannot recreate that qualification. Active admission additionally checks the original use and channel identity. Missing/late extension never manufactures historical coverage; partial storage fails closed.

The receipt transaction locks control (when available), task, usage and log, then validates and writes in the same transaction. A terminal winner rejects the late receipt. SaveAttempt failure invalidates qualification. Failover retains original frozen usage facts and private qualification while restoring only the candidate route fields. Existing unknown acceptance remains unknown and repeated HTTP submission reads the existing task without another upstream submission. Metadata, exact large seeds, auxiliary images and original price remain intact.

This adds no schema, protocol, TTL, producer execution journal or wallet action. The private object is unexported and absent from JSON/DB. Existing id-only internal helpers remain for compatibility, with the terminal race repaired; real frontend paths use the owned wrappers.

## Verification

All listed tests use isolated SQLite or a disposable local PostgreSQL 16 container; provider/wallet HTTP in controller tests is synthetic local HTTP, not a paid generation or real wallet fixture.

- Pre-fix evidence: `/private/tmp/image-submit-proof-kjgw3w1v/proof.jsonl` (actual controller 7 pass including parent), `pg-terminal-proof.jsonl` (4 PG leaves, 5 including parent). A first failed barrier harness is separately retained and is not product evidence.
- Final model targeted race: `/private/tmp/image-submit-receipt-model-final.jsonl`: 75 pass, 15 PG leaves skipped without DSN.
- Final actual PG race: `/private/tmp/image-submit-receipt-pg.jsonl`: all 15 leaves / 17 including parents pass. Covers terminal-first, receipt-first, read-before-terminal lock wait for old and owned Accept/synchronous complete; final log-row wait valid, changed operation, and context cancellation. Harness `/private/tmp/b2-image-submit-receipt-pg.py`; container/network/temporary credentials removed.
- Final complete controller/task race: `/private/tmp/image-submit-receipt-packages-confirm.jsonl`: 891 pass, 20 skip, no failures. Skips: 4 older PG leaves not rerun in this batch; 16 application-wallet/native delivery/ledger external fixtures not configured. Do not present old PG or wallet evidence as new runs.
- `go -C core build ./...`: `/private/tmp/image-submit-receipt-build-final.log`, exit 0.
- Three historical direct-call test fixtures now supply their actual original operation/group/token and Prepare/Save sequence rather than bypassing the new private qualification.
- Earlier whole-package runs exposed the two incomplete prepaid fixtures, corrected above. A subsequent run had one transient `TestImageRecoveryHandoffActualFailoversAcrossEpochs` failure (expected wallet calls 4, actual 2, SyncOwned unavailable). Isolated rerun and 10 consecutive repetitions passed, then final whole-package run passed. No timeout, lease, assertion or production recovery change was made for it; root cause was not established. Evidence retained in `image-submit-receipt-packages-final.jsonl`, `image-submit-receipt-handoff-recheck.jsonl`, `image-submit-receipt-handoff-repeat.jsonl`.

## Boundaries and release

This closes the observed receipt/terminal race, not the producer's durable start/return or unknown upstream acceptance window. All uses remain open/unproven. Existing wallet refunds, accounting and producer cleanup are not newly claimed to be jointly closed; no safe-disable qualification is added. No request can adopt a newer owner through the new API.

Only local development/verification is performed. Required release order: independent branch and local checks, PR review, exact-commit staging acceptance, merge GitHub main, verify merged version, then new explicit user confirmation before production. No push, deployment, production database access, migration, paid call or production action is authorized by this delivery.
