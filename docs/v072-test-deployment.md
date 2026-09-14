# Token Platform gateway v0.7.2 test deployment

This branch integrates upstream tag `v0.7.2` (3f11d928) with the Token Platform fork and the original checkout's pending capability/demo/image changes. Do not deploy the upstream image: it does not include the external wallet and capability contract integration.

## Verified locally

- `core`: `go test ./...` passed.
- Gateway executable: `go build` passed.
- `web`: locked dependency install, 45 tests and production build passed.
- Token Platform production build passed with its current local Registry changes.
- No live provider generation, PostgreSQL integration environment, Docker image or test-server deployment has been verified by these checks alone.

## Before deployment

1. Push this gateway integration branch to the fork and fetch that exact commit on the test server. Commit/push the platform's pending Seedream Registry fixes separately before building the platform there.
2. Preserve the current platform and gateway commit IDs and image tags. Back up both databases. Review new gateway schema fields (channel `remark`/`backup_only`, model/group retry budgets and pending log fields); the gateway may migrate tables at startup. Do not assume image rollback reverses database migrations.
3. Use the fork checkout as `AIPROXY_BUILD_CONTEXT` in the platform's `deploy/production` Compose environment. Default is the sibling `aiproxy-src` directory; point it explicitly at the new checkout if retaining the old one.
4. Build a uniquely tagged gateway image via the existing production Compose configuration. Do not overwrite the previous rollback image. Keep the gateway admin API private and retain the existing wallet/config secrets.
5. Do not enable `ENABLE_ADMIN_BYPASS_CHANNEL_MODEL_CHECK` simply because the upstream supports it. Existing admin-demo flows use dedicated internal entitlements; customer requests must remain isolated.

## Test-server acceptance

- Check health/version, administrator model/channel list, and capability resolution.
- Generate one admin demo on an unlisted capability with an enabled route; verify it records an operational log and does not debit a customer wallet.
- Call a published image capability with a customer key and funded test wallet; verify successful output and a single ledger debit.
- Run one video task through submission, polling and content download; verify task ownership and one final charge.
- Check an ordinary failed provider request is reported with its request ID and no customer charge.
- If using multiple routes, verify primary/backup switching and bounded retries with the test environment's controlled providers.

## Rollback

Restore the previous image and deployment configuration if application checks fail. If a schema change prevents rollback, stop writes and use the reviewed database restoration procedure; do not blindly restore a pre-deployment database after accepting new billable traffic.

Local source path: `/Users/andyli7/Documents/code/token/.worktrees/aiproxy-v072-integration`.
Original gateway checkout and its staged/unstaged files were intentionally left unchanged.

## Existing follow-up identified during review

`core/middleware/distributor.go` currently falls back to legacy routing after any capability-resolution error. This behavior is inherited unchanged from the original pending code, not introduced by the upstream merge. It can hide ambiguity/parameter-mismatch errors when a legacy route exists. Track a separate correction before broader rollout; include mixed legacy/capability catalog behavior in test acceptance.
