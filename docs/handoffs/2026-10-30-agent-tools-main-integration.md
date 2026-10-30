# Agent Tools development checkpoint — 2026-10-30

This checkpoint preserves recovered Agent work on the **current gateway main**.
It is not a release acceptance, permission to deploy, or permission to call a
real provider. The project manager owns review/merge coordination. Existing
worktrees, services, database rescues, encryption keys and settings are retained.

## Source and boundaries

- Gateway base: `8d4010b37bd4fc0e56814747e52b6c4a398516ea` from
  `AndyLi7/aiproxy` main; its tree matches the previously accepted gateway base.
- Original Agent commits `51a180a3`, `496d32d3`, `44dcb016` were cherry-picked
  onto that base, followed by the preserved recovery overlay. Do not replace
  main with the divergent old `44dcb016` checkout.
- Companion App: `AndyLi7/token-platform` PR 150,
  `e03ba418691af082bbfe6fcce0174d237fccedfd`.
- Main's typed 404, capability diagnostics and public error redaction remain.
- Internal Tools use HMAC owner/session/method/path/body/revision binding,
  persistent replay nonces and task IDs. Submission intent is persisted before
  the single provider call; lost/uncertain acceptance never causes automatic
  resubmission or fallback. Frozen account/contract and leases protect reads.
- Tools bindings and execution records are separate from retail API admission
  and wallet settlement. Unknown provider cost is unreported, not zero.
- Fal image reference metadata is accepted through existing native schema
  validation; public API input checks are unchanged. No extra model-choice loop.
- Owned artifacts reuse bounded, integrity-checked storage and owner-scoped
  reads. System DNS remains the default. The pre-existing recovered opt-in
  Cloudflare DNS mode remains explicit; no host networking or trust store was
  changed. `DISABLE_UPSTREAM_BILLING_RECOVERY` defaults false and only skips
  that legacy worker when explicitly enabled, preserving image recovery.

## Reproducible local evidence

No real model, image generation, provider billing lookup or provider health
trial was performed. Test credentials and image pixels are synthetic.

1. Normal `core/main.go` builds for Darwin arm64 and Linux arm64 using Go 1.27,
   `CGO_ENABLED=0`, `-trimpath -buildvcs=false` (not a Go test harness binary).
2. Seven-package targeted regression selection: **35 top-level / 146 including
   subtests passed**, no skip/failure. Covers HMAC/replay, submit/cancel, frozen
   binding, Wan references, owned-image SSRF/integrity, preserved main routing,
   public error privacy, usage identity and API-wallet separation.
3. `scripts/test-tools-production-main.py`: **27 checks passed** against the
   actual main executable; macOS sandbox only permits a rejecting local proxy.
   Includes missing schema, auth, idempotency after failed CONNECT, cancellation,
   timeout, ownership, restart/receipt reads, and no token/wallet task writes.
   This failure-boundary fixture seeds artifact evidence; it is not a success
   generation claim.
4. `scripts/test-tools-production-main-success.mjs`: **14 checks passed** with
   the Linux main in a network-isolated container. A temporary CA is trusted by
   that child process only. The CDN address belongs to container loopback only.
   Fake Fal submit → poll → result → real bounded CDN fetch → archive → owned
   read → main restart/read all complete. No task/output rows are seeded.
   Exactly one provider submission, no replay duplicate; 1600×900 dimensions;
   unknown cost and cross-session denial verified.
5. Companion App's actual services connect to the normal main over a loopback
   Docker-stdio bridge. Five scenarios pass: inspection report, inspection
   failure, timeout, cancellation, provider failure. Each submits once including
   replay; owner checks, version parent, 1600×900 output, null cost, untouched
   wallet and database reopen are verified. The fixture adapter compares
   decoded RGB SHA256, since App intentionally strips metadata/re-encodes PNG;
   byte equality to the raw CDN PNG would incorrectly reject valid normalization.
6. Full browser/SDK pairing also passed on the same normal-main Linux binary.
   Synthetic management keys were aligned after an initial fixture mismatch;
   the failed first run stopped after one capability query without submitting.
   The successful second run used actual Mastra/SDK HTTP/SSE and performed
   `image_capabilities → image_gen → inspect_images → final`; the inspector
   made its own scripted model call. Exactly one fake provider submission,
   three tool calls, persisted inspection with unknown cost, zero wallet ledger
   writes. Browser reload retained the session/result; rendered image natural
   dimensions were 1600×900. Session `6ee75ad1-30de-4497-96ff-2c91a46177a3`,
   run `85380d12-7033-4c14-bc3d-5f625fd8b5f7`. All LLM decisions/reports were
   scripted, not evidence of autonomous real-model reasoning. The isolated
   browser database was initialized through the formal bundled migrator.
7. App evidence: 61 targeted tests, seven migration/runtime tests and PG-schema
   production build passed. App CI on e03ba418 succeeded. Whole-repo TypeScript
   still has **263 pre-existing diagnostics**; do not call it clean.
8. App formal `drizzle/0036_agent_harness.sql` uses the existing runtime migrator:
   88 old tables preserved, 30 added / 103 statements, repeat/old journal tested
   in real isolated PostgreSQL. Gateway explicit migration adds three tables;
   see `core/migrations/tools-execution-v1/README.md`.

Frozen local main binaries (builds before documentation-only edits):

- Darwin arm64 SHA256:
  `4e4cc976b2eb54cbfe884a8bf4b803351d55e832b91d0a2b023014b932532ed5`
- Linux arm64 SHA256:
  `d6435bce9a508ca4e86bdda405ab850827b112102dc7e7cfb9978d62bb6da18e`

Local evidence, intentionally not committed with databases/private configuration:
`/Users/andyli7/code/token/.local-logs/musespan-gateway-integration-20261030/verification/`
and `musespan-main-integration-20261030/paired-main-services/` under the same
`.local-logs` root. The frozen source-to-artifact manifest is produced after the
checkpoint commit. Preserve these files for review; do not distribute private
fixture env files, TLS private keys or rescued databases.

## Remaining acceptance and gates

- Full SDK/session/browser simulation passes; scripted success does not
  establish autonomous real-model behavior or visual edit quality.
- Gateway full CI/lint and independent PR review are not replaced by targeted
  local tests. Resolve review/CI blockers before a development-baseline merge.
- No staging deploy, database migration, variable update or production change is
  part of this checkpoint. Do not open ordinary-user Agent execution merely
  because configuration variables are absent. Keep existing telemetry/cookie
  settings and other product controls unchanged.
- Future mock-only environment acceptance must use an isolated test database,
  synthetic credentials/models, an egress-denied fake provider, explicit counted
  requests and preserved sentinels. Never aim the integration fixture at real
  provider URLs/credentials. No actual cost or visual-quality claim comes from
  scripted responses.
- This is an additive recovery baseline for continued development, not proof
  that every requested feature or every historical run has passed acceptance.
