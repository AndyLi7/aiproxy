# Task 2 Report: Opt-in trace runtime

Commit: `feat: manage opt-in request trace runtime` (final short SHA reported to the controller after commit creation).

## Implemented

- Added `core/trace` with the specified `Options`, `Health`, `Start`, `Runtime.NewRequest`, `Runtime.Health`, `Runtime.Close`, `Current`, and `Install` API.
- Disabled startup performs no migration and creates no writer or cleanup worker. Enabled initialization failure returns a non-ready, non-panicking runtime and logs only `request_trace_initialization_failed`.
- Successful startup applies a five-second migration deadline, creates the default bounded request-trace writer, and starts hourly cleanup. Each tick makes exactly one `CleanExpired(time.Now().UTC(), 500)` call with a ten-second deadline.
- Cleanup errors are counted independently and log only `request_trace_cleanup_failed`. Runtime-context cancellation stops cleanup without canceling the writer.
- Close stops and joins cleanup before draining/closing the writer, sharing the caller's context deadline throughout.
- Added atomic global installation/current lookup and nil-safe runtime methods.
- Wired startup to `REQUEST_TRACE_ENABLED` using `env.Bool(..., false)`. Shutdown remains HTTP shutdown, `consume.Wait`, batch/sync worker wait and summary cleanup, then a five-second trace-runtime close before deferred database close.
- Documented the default-off flag, trace-table-only migration, explicit deployment opt-in, and temporary-database-only local testing in both READMEs.

## TDD Evidence

### RED

Command:

```text
docker exec token-trace-b1-test sh -lc 'cd /workspace/core && /usr/local/go/bin/go test ./trace -count=1'
```

Relevant expected failure before implementation:

```text
trace/runtime_test.go:18:13: undefined: Start
trace/runtime_test.go:18:45: undefined: Options
trace/runtime_test.go:21:19: undefined: Health
trace/runtime_test.go:54:15: undefined: newCleanupTicker
FAIL github.com/labring/aiproxy/core/trace [build failed]
```

The failure was expected because the tests described the new runtime API and cleanup timing seam before `runtime.go` existed.

### GREEN

Focused command after implementation:

```text
docker exec token-trace-b1-test sh -lc 'cd /workspace/core && /usr/local/go/bin/gofmt -w trace/runtime.go trace/runtime_test.go && /usr/local/go/bin/go test ./trace -count=1'
```

Output:

```text
ok github.com/labring/aiproxy/core/trace 0.273s
```

Race/focused/compile verification:

```text
go test -race ./trace -count=1
ok github.com/labring/aiproxy/core/trace 1.353s

go test ./common/requesttrace -count=1
ok github.com/labring/aiproxy/core/common/requesttrace 0.242s

go test ./model -run "TestTraceStore|TestRequestTrace|TestCleanExpired" -count=1
ok github.com/labring/aiproxy/core/model 1.390s

go test . -run "^$" -count=1
? github.com/labring/aiproxy/core [no test files]

go vet ./trace
(no output; exit 0)
```

An attempted unrestricted `go test ./model -count=1` was not usable as a green gate because pre-existing PostgreSQL/Redis integration tests tried to launch nested rootless Docker and failed with `get provider: rootless Docker not found`. The trace-focused model tests passed, and no business database was used.

## Files Changed

- `core/trace/runtime.go`
- `core/trace/runtime_test.go`
- `core/main.go`
- `README.md`
- `README.zh.md`
- `.superpowers/sdd/2026-09-08-request-trace-gateway-capture/task-2-report.md`

## Self-review

- Confirmed the runtime context is never passed into the writer worker; it only controls migration during startup and cleanup afterward.
- Confirmed shutdown retains the pre-existing relative order and trace close occurs before the deferred database close.
- Confirmed runtime-owned failure logs contain fixed codes only and never interpolate a DSN or raw error.
- Confirmed the public options surface contains only `Enabled`; ticker injection remains package-internal and test-only.
- Confirmed tests use temporary SQLite files and do not restart services, deploy, push, merge, or touch a business database.

## Concerns

- The repository's full model test package requires nested Docker services unavailable inside the supplied Linux helper. This is an environment limitation, not a failure in the focused trace tests.
