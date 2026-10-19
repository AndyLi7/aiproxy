# A local gateway integration after relocation

2026-10-18. Local branch `codex/b2-local-integration`. Repositories now live at `/Users/andyli7/code/token` and `/Users/andyli7/code/aiproxy-src`; the original Documents directories and original uncommitted takeover worktree remain untouched backups.

## Source and behavior

Base `07f63e81de1f74afb3b0616397f188dd921b3a80` contains reviewed retention diagnostics. Merge `1f724b0c` adds reviewed reverse original-billing context `195431a9db8d4f746e7400d07ed4268bf0eba9b5`. Verified frozen cache, reverse HTTP, and controlled-runtime overlays were then applied by file hashes. Only router/api.go and the shared probe HTTP test fixture needed a three-way merge; both preserve the cache GET and three explicit retired-target routes.

- Cache observation reports this authenticated process's actual loaded snapshot. Its boot comes from the single ProcessBootIncarnation source, shared with controlled runtime. It is not fleet ACK, drain or close authority.
- Reverse resource create/read/cancel and the shared probe/prepare/cutover HTTP path retain explicit resource kind, original retirement/request/checkpoint/task/use/credential/quote bindings. Lost creation responses read the full original request. Original billing remains readable without granting execution permission.
- The supervised runtime starts real fixed child processes through private inherited pipes, authenticates artifact and boot, stages and activates bounded proof, and stops/joins the owned set on loss or expiry. Sticky known refusal survives failed persistent revoke and subsequent EOF; it does not become ordinary source outage.
- Controlled runtime remains fresh private local SQLite only. Common Prepare/Seal/Commit/forward and reverse Cutover remain disabled until independent intent/ACK/checkpoint installation. Normal noncontrolled paths retain existing behavior. No business lease, amount, price or database schema changes are introduced.

## Verification on this combination

Evidence is private local data at `/Users/andyli7/code/.token-relocation-20261018/integration-review/`; do not commit or publish those artifacts.

- Final union of all three previously reviewed test selections: **206 roots, 507 pass, 122 opt-in/helper skips, zero failures**, race enabled. selection.json records the exact union; combined-race-final.jsonl is final evidence. Skips remain explicit, not passing checks.
- New isolated PostgreSQL16 combined checks: **22 pass, no skips**, covering cache two-process/read-only/snapshot, retention consistent/current-head/no-DML, reverse HTTP final-write/read-only, original reverse billing and controlled exact revocation/final authorization. pg-cleanup.json confirms temporary container removal; generated credentials were held only in process environments. This is not a PostgreSQL controlled-runtime bootstrap or live-environment test.
- Actual current app `ddd966a7a7f865ce6362fd616738e6a12409c7d0` to combined Go native HTTP fixture: **one full app scenario and Go fixture pass**. Prepare original resource, single probe, app billing callback, reviewed Prepared, atomic Cutover/readback and retention diagnostics retain original task/quote/wallet/bill invariants. SQLite memory app wallet and supplier/billing provenance are synthetic; no paid call or real bucket was used. Both fixture processes exited and synthetic credential metadata was removed.
- Rebuilt actual gateway main and coordinator from this tree. The mcp-servers and openapi-mcp workspace modules also build successfully. Compiled binaries are test artifacts outside Git, not release artifacts.

The first broad combination had 505 pass/122 skip/two failure events for one leaf and its parent: the older reverse engine fixture still expected a valid reverse HTTP Cutover to conflict. The reviewed explicit reverse HTTP implementation correctly returned committed. Test-only correction preserves wrong-authorization conflict plus unchanged Prepared, then checks the full original typed committed HTTP readback, model replay, original quote/bill/contract, one supplier call and use=open. Three consecutive targeted runs pass (15 events); the final full union above reruns the original selection. No production code was weakened to satisfy the fixture. Earlier failure logs remain preserved separately.

## Security and remaining limits

Full worktree and staged scans use the project security-scan script, plus root and independent agent review of all changed paths. HTTP auth/body bounds/no-store, SQL parameterization, private pipe/file boundaries, original monetary facts and controlled common authorization were checked.

| Severity | Scope | Finding | Action |
| --- | --- | --- | --- |
| clean | 35 code/test/delivery paths plus this record | No remaining findings | Local commit only after script and final diff checks |

Independent intent/exact commit ACK/restore checkpoint, actual deployed fleet coverage, cache confirmation, application reverse UI and joint use closure/safe-disable remain unfinished. A is not complete and migration is not enabled for a real environment. Next local implementation preserves the original migration epoch/storage generation during an exact checkpoint resume, with new boot/session and a larger proof sequence; old history must not be rewritten.

Release order remains local implementation and checks, reviewed exact-SHA staging acceptance, GitHub PR/main version verification, then fresh user confirmation before production. This integration did not push, deploy, execute environment SQL, access real databases, regenerate paid examples or resume the original paused engineer A chat.
