# Release trigger review — 2026-10-30

This local change removes only `on.push.branches: ["**"]` from Release.
Existing version-tag pushes (`v*.*.*`) and pull-request builds remain. CI is unchanged.
No workflow was disabled, repository setting changed, branch pushed, tag created,
or Actions run dispatched during this review.

| Event (code changes) | Release | CI | Release publishing |
| --- | --- | --- | --- |
| Ordinary branch push (`codex/feature`) | No | No (unchanged) | No |
| `main` push | No | Yes | No |
| Pull request targeting `main` | Build | Yes | No |
| Version tag `v1.2.3` / `v1.2.3-rc1` | Yes | Yes | Existing tag path |
| Non-version tag | No | No | No |

Existing PR path exclusions for `.md` / `.yaml` are unchanged; the matrix assumes
code changes. GitHub's [workflow syntax](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#onpushbranchestagsbranches-ignoretags-ignore)
specifies that a tags-only push filter does not run on branch pushes.
[Tag pushes do not evaluate path filters](https://docs.github.com/en/enterprise-cloud@latest/actions/how-tos/write-workflows/choose-when-workflows-run/trigger-a-workflow#using-filters-to-target-specific-paths-for-pull-request-or-push-events).

Local evidence: `/private/tmp/and76-openai-image-compat-20261030/review-release-trigger.log`.
Reproduce from the Gateway worktree with
`node /private/tmp/and76-openai-image-compat-20261030/check-release-trigger.cjs`.
The check parses YAML and compares against `17a1e4d287a8b11b3bff926de220730adef781bf`:
removing that one trigger must leave the complete workflow identical, and CI must
remain byte-identical. It also asserts the existing PR guards on each Docker login,
image push output, Docker/Sealos publishing job, and tag-only GitHub release step.

Manual review confirms PR builds may upload ordinary Actions build artifacts,
but cannot log in to registries, push container images, create GitHub releases,
or run the Sealos release job. Version-tag job definitions are unchanged.
This is static local verification, not evidence of a remotely executed workflow.
The change is prepared as a separate local commit for review; no push is authorized.
