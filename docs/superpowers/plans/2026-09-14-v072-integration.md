# AIProxy v0.7.2 integration plan

**Goal:** Prepare a verified gateway branch for test deployment without changing the running checkout.

**Architecture:** Merge the upstream release into the existing fork, then copy the original checkout's tracked and untracked custom source changes. Preserve wallet authority, operational demo isolation, capability routing and outbound network policy.

## Tasks

- [x] Verify the original baseline: seven critical Go packages pass.
- [x] Create isolated worktree on codex/aiproxy-v072-integration and fetch v0.7.2.
- [x] Resolve upstream conflicts in channel tests, relay controller, distributor and HTTP client. Retain both upstream functionality and fork safeguards.
- [x] Apply original uncommitted source changes without altering original index or files; compare all copied files and resolve overlap explicitly.
- [x] Run Go tests across core, frontend tests/build, and platform build. Review any regression before declaring readiness.
- [x] Run secret scanner and manual security review before committing. Record release ancestry and deployment/rollback steps.

Independent static review found no merge-induced blocker. An inherited capability-resolution fallback issue is documented in the deployment notes for follow-up. Original dirty source differs only in the two files intentionally combined with upstream (distributor and relay controller); other copied tracked changes are byte-identical.

## Verification

Run `go test ./...` from core, `pnpm install --frozen-lockfile`, `pnpm test` (if configured) and `pnpm build` from web. Run `pnpm build` in the platform. No live upstream generation, deployment or database migration is part of this preparation.

## Constraints

Do not modify the original gateway checkout, copy credentials, publish models, restart containers, or apply production migrations. Preserve all original uncommitted changes. Build the fork, not the upstream official image, for deployment.
