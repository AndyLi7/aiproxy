# AND-44 temporary CI acceptance

This temporary pull request verifies the merged CI optimization on GitHub Actions.

- Documentation-only changes should skip the Go CI, security scan, and release workflows.
- A harmless root YAML marker will trigger the Go CI without publishing release artifacts.
- A newer commit in the same pull request should cancel its older Go CI run automatically.
- This pull request will be closed without merging; it contains no product changes.
