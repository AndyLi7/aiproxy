# B2 minimal reverse cutover — local delivery

Base: `72fb672daae139ca28923e394df549717d6094d3`. This slice reuses an enabled retired physical target via an explicit reverse resource. It does not create a replacement channel, revoke keys, change task contracts/quotes/bills, or close admission uses.

## APIs and state

- `PrepareRetiredTargetResource`: binds the exact latest retirement and its historical committed request to the new operation, epoch/storage, current complete source inventory, family, raw pin hash, target projection, identities and credential IDs. The optional v1 resource changes control revision by one; replay returns only the exact durable resource.
- `BeginMigrationProbe` / existing private native reservation: the explicit `reuse_retired_target` kind permits a new restricted probe against the retired, still-enabled target. A public admission request or string operation ID is not a probe capability. Unknown provider acceptance remains unknown and does not re-submit.
- `PrepareReverseMigrationFromProbe` / `PrepareReverseMigration`: consume the created resource and bind its checkpoint and the probe request/evidence/fact hashes atomically. The same private verifier/proof rules apply as forward prepare.
- `CutoverReversePrepared`: one control transaction invokes the existing private seal and commit helpers. Both physical channels stay enabled. Family and complete raw pin CAS change atomically with the two logical revisions; deployment freshness/probe deadline are checked after the final write. Exact committed replay and the actual prepared readback work at exhausted revision, while first cutover requires two revision slots.
- `CancelRetiredTargetResource`: only created or unsealed prepared intent may become cancelled/abandoned. It does not mutate channel status or keys. Committed and historical sealed operations cannot be adopted as new reverse work.
- `ReadRetiredTargetResource` returns durable intent only. `ReadPreparedMigrationFacts` separately verifies current head/postconditions and includes the explicit resource kind. A historical committed receipt alone grants no current write permission.

Lock order remains control → owned target resource → prepared/probe facts → channels/configuration. Source rows and raw pins are protected using the existing helpers; there is no network call under a database transaction and no new TTL. Pending created resources also guard ordinary channel/config writes before prepared exists.

Empty forward kind serializes byte-for-byte as before. The existing forward APIs keep disabled-shadow behavior and reject reverse requests explicitly. Existing HTTP probe decoding rejects the new kind; this delivery does not enable a reverse HTTP/runtime endpoint.

## Historical evidence boundary

The historical forward record proves physical identity/key and the exact latest retirement chain. It did not retain a complete historical projection of its old source, so this implementation neither invents nor claims that projection. A new reverse resource freezes the current reviewed inventory/projection/pins, and a new restricted probe checks that current target. Old accepted tasks retain their original `FrozenContract`, quote, billing receipt and operation. Same-key reverse does not revoke a shared key. A→B→C cannot jump C→A; legal C→B then B→A can use immutable historical receipts independently from current head.

## Schema and integration limits

[Optional SQL draft](sql/b2-retired-target-resource-draft.sql) adds only `channel_migration_retired_target_resources` and two nonunique channel indexes. It is not registered in runtime auto migration. The draft was replayed into isolated SQLite and PostgreSQL fixtures. No production/test environment SQL was executed. Missing/partial/wrong-version reverse storage fails closed; missing optional storage retains forward behavior. MySQL was not instance-tested.

Parent-reviewed `daf5b7d` DB-only probe-billing-context reader is not in this branch. Its forward-only resource/lifecycle lookup still needs strict reverse resource-kind adaptation on the combined baseline. The local end-to-end test uses a real loopback HTTP supplier and native engine path but synthetic wallet/archive/billing provenance, not production billing proof. No real supplier or paid call occurred.

Real issuer/fleet/cache/recovery-watermark evidence, reverse HTTP/application orchestration, broader executor/use closure and safe-disable remain outside this slice. Historical sealed records remain preserved and cannot be adopted; this is not a claim of comprehensive historical recovery.

## Verification

- Scoped model/controller race: **274 pass, 3 skip**, `/private/tmp/reverse-final-race.jsonl`. The skips are opt-in forward HTTP interop fixture servers (`TestMigrationReadHTTPInteropServer`, `TestMigrationShadowLifecycleHTTPInteropServer`, `TestMigrationShadowLifecycleHTTPNativeBuilderServer`), not reverse failures.
- After two final identity-bound hardening checks (historical seal epoch and resource replay lifecycle revision), rerun all reverse tests: **31 pass, zero skip/fail**, `/private/tmp/reverse-hardening-race.jsonl`.
- Actual PostgreSQL16: **7 leaf scenarios / 8 including parent pass**, `/private/tmp/reverse-pg.jsonl`. Six final-pin-write wait cases cover live success, deployment expiration, revocation freshness expiration, probe expiration, competing raw pin write and context cancellation. Two simultaneous reverse resource contenders yield one winner. The temporary container/network/credentials were cleaned; launcher `/private/tmp/b2-reverse-pg.py`, cleanup log `/private/tmp/reverse-pg-launcher.log`. This PG run precedes the two final validation-only hardening checks; it is not represented as a rerun of those checks.
- Core build passed: `/private/tmp/reverse-build.log`; final build after hardening also passed: `/private/tmp/reverse-final-build.log`.
- Coverage also includes exact replay/readback/cancel; repeated reverse cycles without new physical IDs; same-key behavior; unknown raw large numeric/price field preservation; old FrozenContract/quote/bill retention; genuine ALTER DROP missing-column rejection; pending ordinary mutations; explicit forward hash compatibility; real local native success, unknown acceptance/no resubmit and wrong-billing rejection.
- Intermediate failures were fixture-only: duplicate synthetic probe evidence insertion and an incorrect expected JSON literal capitalization/omitted field. Final tests correct those fixtures; no failing run is counted as pass.

## Pre-commit review

Security scanner and manual diff review cover all 17 changed files. No live credential, new external request, input-built SQL, public authorization bypass or task-price rewrite was found. Scanner logs: `/private/tmp/reverse-security-worktree.log`, `/private/tmp/reverse-security-staged.log`.

| Severity | Location | Finding | Action |
| --- | --- | --- | --- |
| clean | Complete local diff | No findings | Commit locally only |

## Release boundary

Local branch only. No push or deployment. Required release sequence: independent branch → local checks → PR review → exact-SHA staging acceptance → merge GitHub main → verify merged version → new explicit user confirmation before any production release.
