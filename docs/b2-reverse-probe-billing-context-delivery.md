# Reverse probe original billing context — local delivery

Base `9a617bf16534c5dd6c6806bb789969f47ca419cd`. This gateway-only slice makes the existing DB-only billing-context reader recognize an explicitly owned retired-target probe. It does not create a probe, call a provider, consume/settle/refund money, change a quote, write a billing record, or open a reverse mutation endpoint.

## Binding and wire

`ReadMigrationProbeBillingContext` still takes only `{billingOperationId, taskId, groupId}`. The stored run's fully hashed authorization chooses the resource kind; request input cannot select a different resource. The new private helper keeps the forward shadow lifecycle and projection checks, and for reverse verifies:

- the original v1 retired-target resource request/checkpoint/command, epoch/storage, revision and lifecycle;
- its exact reciprocal Prepared linkage, or absence of operation effects before Prepare; cancelled intent cannot have seal effects;
- the specifically recorded retirement and historical committed request, including swapped physical identities and credential IDs;
- the original frozen run/spec, task, use, native billing operation, quote, endpoint, credential scope and upstream request through the existing common checks.

It deliberately does not require the retirement to remain the latest head, nor current pins/projection, to recognize a historical reverse bill. The original Begin verified the then-current target and froze its spec; reading it does not invent a missing historical projection. The source must still exist and match in current complete storage: missing/corrupt history is conflict/unknown/unavailable, never a synthetic proof or regeneration grant.

`context.targetResourceKind` and `context.revertOf` are emitted together only for reverse. Both are `omitempty` and appended after existing fields, so forward output preserves its original field order/bytes and stable receipt shape. Input and monetary evidence schemas, version/source, decimal handling and original amount checks are unchanged. Cancellation permits reading the original paid task but never permits Finalize/Prepare to resume it.

## App compatibility and limits

Exact app `58116ba9f1ca4db78b4af52ad49c2249a5c10d78` has a strict shadow-only V3 journal and immutable `billingObservations`. It rejects the new reverse context fields until a separate explicit reverse journal/intent variant is implemented. There is no persisted `probeIntent` field at that exact SHA; that field is being developed separately. This delivery does not modify the app or fabricate a reverse shadow record. No existing observation is rewritten or moved between operations.

The real-local native engine and HTTP test uses synthetic supplier, archive/wallet and billing evidence. It proves the actual call path, not real fal billing or live control-plane readiness. Reverse HTTP/runtime orchestration, production issuer/fleet proof, independent recovery watermark and use closure remain outside this slice. No new schema, SQL, TTL, physical channel or deployment is introduced.

## Validation evidence

- Parent failure reproduction: `/private/tmp/reverse-billing-parent-counterexample.jsonl`. The old reader rejects a valid private reverse Begin/Reserve original task as conflict.
- New model cases: `/private/tmp/reverse-billing-model2.jsonl` (25 pass). Covers original/cancelled no-DML reads, multiple later legal reverse cycles, current projection/config drift with original frozen bill retained, wrong group/billing/quote/task/use/spec/kind/revert/credential, missing/partial/wrong-version resource, retirement/history corruption, duplicate resource, missing run/task, pending upstream, and forward omission of reverse wire fields. Real ALTER DROP asserts the column is absent before reading.
- Actual controller path: `/private/tmp/reverse-billing-actual-http.jsonl` (4 cases + parent = 5 pass). Local private reverse native Submit/Poll → authenticated context HTTP → existing Prepare/Cutover; unknown acceptance, wrong billing and cancellation controls. Original supplier call count remains one. No secret/input/quote payload leaks in context output.
- Actual PostgreSQL16: `/private/tmp/reverse-billing-pg.jsonl` (5 leaf cases + parent = 6 pass). Control-row lock wait with live/cancellation/original upstream binding change/missing run/retirement corruption. Coherent old snapshot or serialization unavailable is followed by a coherent reread. DML-denying triggers cover control, both resources, Prepared, retirement, probe, task and use. SQLite `query_only` covers the same reader separately. Launcher `/private/tmp/b2-reverse-billing-pg.py`; `/private/tmp/reverse-billing-pg-launcher.log` confirms temporary container/network/credentials cleanup.
- Final related model/controller/balance race: **116 pass / 1 opt-in skip** (`TestMigrationProbeBillingInteropServer`), `/private/tmp/reverse-billing-final-race.jsonl`. Core build passed: `/private/tmp/reverse-billing-build.log`.
- Initial implementation incorrectly equated the internal route key (`model::ability`) with the public API model ID; the existing compiled spec/task validator already handles that distinction. That extra, invalid equality was removed, not an existing safeguard. Two intermediate negative test expectations were corrected to existing conflict rather than unavailable semantics. Those failed runs are not counted as passes.

## Pre-commit and release

The security-scan script and manual diff review cover changed Go paths, tests and this record. No new public mutation/auth bypass, credential output, input-built SQL or monetary write is introduced. Worktree and staged scans pass for 5 files: `/private/tmp/reverse-billing-security-worktree.log` and `/private/tmp/reverse-billing-security-staged.log`.

| Severity | Location | Finding | Action |
| --- | --- | --- | --- |
| clean | All 5 changed files | No findings | Local commit only |

Local commit only, no push/deploy or paid supplier request. Release remains independent branch → local checks → PR review → exact-SHA staging acceptance → GitHub main merge/version verification → fresh explicit user confirmation before production.
