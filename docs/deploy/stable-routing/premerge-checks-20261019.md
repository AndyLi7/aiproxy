# A pre-merge checks — 2026-10-19

Status: **not ready to merge or release to production**. This records checks and remaining gates; it does not waive failed CI.

## Accepted source and baseline

- Gateway PR #32: candidate before these fixes `92e1253a2d6cfcf3e825ccc2dadb6f0f6862cc8e`; main `3148ef943f88670fe5b718ac1da9665be3ce3a85`.
- App PR #106: `4fce16bdc48523c9b1f2bb080c3b153d9540e979`.
- Inventory PR #107, stacked on #106: `bade623c32ac4959b57acda6a8fba8e6ce955a82`.
- App main: `ae3032a90a233d29834f5fb4919ee8e03af4819c`.
- Refreshed origin/main and GitHub PR metadata; all three PRs are mergeable drafts. This is not evidence that checks passed.

## Bounded fixes in this follow-up

1. SQLite admission time used a Julian-day floating-point expression which can truncate one millisecond. This caused fresh image submission fixtures to be rejected as future-dated. Use integer seconds plus fractional digits for both the read and CAS guard clock. Regression exercises every fractional millisecond and second boundaries.
2. Check cursor/close errors in routing-storage readiness; fail closed on a storage error.
3. Preserve the exhaustive call-status matrix but format row diagnostics only on failure.
4. Increase core package test timeout from 60s to 600s (the normal Go default). The complete local model package took 177.484s under concurrent verification; no cases or assertions are removed.
5. Pin golangci-lint 2.15.0, replacing 2.13.2 which cannot read the current Go export format.
6. Normalize GHCR repository names to lowercase in image build/publish jobs. No workflow triggers or deployment destinations are added.

## Verification evidence

- Full core: `go test -json -timeout 180s -count=1 ./...`: **82 packages passed, 6,103 test/subtest pass events, 463 test/subtest skip events, no failures**. Counts include nested subtests and are not 6,103 independent user journeys. Conditional integration suites were not enabled by this command.
- Clock/image submission regression repeated 10 times: passed.
- After the final routing cursor fix: race-enabled routing, PostgreSQL-routing-named, clock and image submission tests in core/model and core/controller passed. PostgreSQL opt-in tests may skip; this run is not a new PostgreSQL integration acceptance.
- MCP server and OpenAPI MCP module tests passed; most packages contain no tests.
- `go build ./core/... ./mcp-servers/... ./openapi-mcp/...` passed.
- Application source has not changed; retain its existing 462 related-test, 61 inventory-test, build and staging evidence instead of rerunning unchanged work.
- No paid provider calls, database migrations, production deployment or production data changes were made for these checks.

## CI findings that remain open

The original CI test job failed at its 60s package timeout. Its lint job crashed on Go export-data compatibility. Its push image build failed on uppercase `AndyLi7` in GHCR, while PR image builds and OS build matrix succeeded. The changes above address those causes but must be confirmed on the pushed commit.

Running the compatible linter reveals actual accumulated findings: core candidate at 92e1253a has **12,989**, while exact main has **5,027** under the same tool/config. A multiset comparison by linter, file, message and source line found **8,098 unmatched candidate findings** (mostly whitespace, formatting, unchecked test helpers and complexity). This comparison is triage, not a proof that every unmatched warning is a newly introduced defect. Do not describe all warnings as pre-existing, disable rules, or silently baseline them to pass the PR.

Relevant production-code cursor-error findings were fixed. Other reviewed examples include owned-artifact response bodies closed by defer, controlled runtime-guard process execution, and public-address checks on artifact fetching. This spot review does **not** certify the whole lint delta. Remaining findings must be classified/fixed or receive an explicit, scoped policy decision before treating CI as passed.

The full Semgrep scan reports 69 findings while the PR differential scan passed at the old head. Findings include unpinned action tags, Docker-user hardening, registry-configuration references and pre-existing token generation using math/rand. The latter is a distinct security concern, not dismissed as a style warning. No broad security exception is granted here.

## Remaining release gates

- Confirm pushed follow-up CI and review the actual remaining lint/security findings; keep PRs draft while blocked.
- The gateway follow-up changes runtime source. Deploy and accept that exact reviewed commit on staging before merging; the old 92e1253a acceptance is insufficient for the new commit.
- Preserve current backup/acceptance records. No schema change is introduced by this follow-up.
- Recheck production source/auto-deployment settings before merging. An empty repository-hook list does not prove Railway or GitHub App auto-deployment is disabled.
- Merge accepted gateway #32 and application #106, then retarget/review #107 against main. Verify final merged source matches staging acceptance; do not silently accept merge-only changes.
- Production still requires the user's explicit release confirmation and separately reviewed production SQL.

## Production data preparation (review only)

Minimal existing reviewed SQL concerns gateway routing state/receipt tables, access-configuration identity table/index, and the app reconciliation `claim_owner`/`claim_deadline` nullable fields. Inspect actual production schema and channel inventory first. Retain verified backups and stop incompatible old writers as documented. Do not infer production inventory from staging, install the optional migration-system schema family, remove historical billing records, or run these SQL files merely because main was merged.

## Cloud follow-up at 3ebb1c43

- Push Release run 39192684087 passed: both image architectures, all six OS/architecture binaries, image/cluster publication. These are build artifacts for the feature branch, not a Railway deployment.
- CI confirms MCP and OpenAPI lint now pass. Core lint reaches real findings, but the action stdout buffer overflows (`ERR_CHILD_PROCESS_STDIO_MAXBUFFER`). Its pre-existing automatic-fix diagnostic step still reports 1,260 findings after formatting; this is not a clean result.
- Store full core lint text/JSON as downloadable CI artifacts instead of sending megabytes through the action buffer. Retain nonzero lint exit status and all rules; remove the temporary auto-fix/diff logging step, which neither committed repairs nor changed the failed gate.
- Full Semgrep push run 39192684111 scans 1,383 files and reports 69 blocking findings with exit 1. Its pre-existing `continue-on-error: true` makes the overall workflow appear successful. Differential PR run 39192686742 reports zero findings and exit 0. Report these separately; the overall green badge is insufficient.
- Existing staging health recheck: app, aiproxy, edge HTTP 200; anonymous admin inventory HTTP 401. No staging redeployment in this pre-merge audit yet.
- App main workflow publishes a `latest` image. Empty repository hooks do not exclude image/GitHub-App-based production updates. Verify deployment-side source/auto-update configuration before any merge.

- Push CI run 39192684086: **Test AI Proxy passed** on 3ebb1c43, along with MCP tests and MCP/OpenAPI lint. Overall CI remains failed due to core lint. New report-output mode was locally verified: all 12,994 current findings retained in JSON/text, exit status 1, only 623 bytes on stdout. The 12,989 count above refers to the earlier candidate comparison.

- The parallel PR CI run 39192686744 still hit the 300s **whole model-package** limit, while starting `TestNativeClaimRechecksAgeAtUpdateAfterWorkerPause` (0s elapsed). Its exhaustive call-status matrix had already passed in 135.32s; no assertion failure occurred. The identical push model package passed in 270.950s. Use the normal Go 10-minute package budget to accommodate measured hosted-runner variation, preserving all cases and assertions; no sleep or retry-until-green was added. The failed run remains recorded.
