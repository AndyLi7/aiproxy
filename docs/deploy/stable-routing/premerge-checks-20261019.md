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


## Local lint/security closure after 3363785d

The preceding failed CI records remain historical evidence. This follow-up fixes the findings locally; cloud CI and exact-source staging acceptance are still required before merge.

- Core golangci-lint 2.15.0: **0 issues** with the existing configuration. No linter was globally disabled and no warning baseline was introduced. Formatting accounts for much of the broad file delta. Function-local exceptions explain preserved transaction order, explicit immediate cursor closes, operator-supplied CLI paths, white-box tests, and non-secret scheduling randomness.
- Security fixes: use unbiased crypto/rand for new API/admin keys, propagate entropy failures without rewriting existing valid keys, set outbound TLS minimum 1.2, replace unsafe string/byte aliases with copies, check cursor iteration and JSON encoding failures, and pin Actions to verified full commit SHAs. The Semgrep job no longer uses continue-on-error.
- Compatibility review retained PascalCase JSON names and field order for persisted/signed records, the exported HoldRefusal type, billing error identity, cancellation/lease ownership, and transaction ordering. Helpers isolate complete validation stages rather than creating a new migration framework. No schema, migration SQL, dependency or route-identity change is included.
- Full core: `go test -json -timeout 600s -count=1 ./...`: **82 packages passed, 6,113 test/subtest pass events, 463 conditional test/subtest skips, no failures**. The main model package completed in 184.609s. These counts include nested subtests, not independent user journeys; opt-in database suites were not enabled.
- Targeted race checks passed for token entropy, stable signed JSON, native admission/wallet boundaries, recovery cancellation, and artifact lease loss/extension. Middleware and registry-validation race checks also passed after their last changes.
- All three Go modules built successfully. MCP/OpenAPI module tests passed. Application code was unchanged, so its previous build and acceptance evidence is retained.
- Semgrep used the same six CI rule packs: 1,383 tracked targets, 214 applicable rules, **zero findings**. Four rule timeouts across three files were rerun with a 60-second budget: zero findings and zero errors. Eleven parser/partial-parser diagnostics remain for existing Helm templates, shell scripts and Actions expression interpolation; these are recorded limitations, not a claim of complete static coverage. Default ignore rules remain unchanged.
- Project security-scan worktree check passed (593 changed files at that point); manual reviews covered secrets, gateway/auth boundaries, settlement/refund behavior, lease timing, and serialized proof compatibility. Final staged scan is required immediately before commit.
- Docker now defaults to UID/GID 10001. A local non-network smoke check using an existing gateway image verified configuration/log/temp/SQLite writes as that user; this is **not** a build of the new Dockerfile. Before staging deployment, inspect actual service mounts and verify their write permissions. Do not recursively change existing data ownership as an unreviewed workaround.
- No database writes, paid provider generation, main merge or Railway deployment occurred in this local closure. Preserve existing backups and acceptance records. Production still waits for explicit user confirmation.

Evidence is retained in the local `a-premerge-20261019/lint-closure-local` backup directory. The next gate is CI on the pushed exact source, followed by staging acceptance and production auto-deployment configuration review before any main merge.
