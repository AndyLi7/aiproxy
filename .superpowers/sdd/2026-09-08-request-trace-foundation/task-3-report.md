# Task 3 report — bounded non-blocking trace writer

## Scope and files

Implemented only the Task 3 writer foundation:

- `core/common/requesttrace/writer.go`: bounded single-worker queue, `Sink`, normalized writer options, non-blocking `Submit`, atomic stats, bounded retry/timeout behavior, deep span snapshots, and context-bounded `Close`.
- `core/common/requesttrace/writer_test.go`: behavior, accounting, snapshot, shutdown, retry, timeout, option-bound, and concurrency tests.

No model/store code, runtime wiring, service lifecycle, migration registration, database, deployment, or later-task surface was changed.

## TDD evidence

### RED

The complete writer test set was written before `writer.go` existed, then the required focused command was run:

```text
> go test ./common/requesttrace -run Writer -count=1
# github.com/labring/aiproxy/core/common/requesttrace [github.com/labring/aiproxy/core/common/requesttrace.test]
common\requesttrace\writer_test.go:15:7: undefined: NewWriter
common\requesttrace\writer_test.go:27:6: undefined: WriterOptions
common\requesttrace\writer_test.go:74:31: undefined: WriterStats
...
FAIL github.com/labring/aiproxy/core/common/requesttrace [build failed]
FAIL
```

This was the expected missing-feature failure: the tests referenced the specified writer API before its implementation existed.

### GREEN

After the minimal implementation and formatting:

```text
> go test ./common/requesttrace -run Writer -count=1
ok github.com/labring/aiproxy/core/common/requesttrace 0.430s
```

The focused writer suite was also repeated to check synchronization stability:

```text
> go test ./common/requesttrace -run Writer -count=20
ok github.com/labring/aiproxy/core/common/requesttrace 2.653s
```

## Behavior and accounting decisions

- `QueueSize <= 0` defaults to 4096 and larger values cap at 4096.
- `WriteTimeout <= 0` defaults to 1 second.
- `MaxAttempts <= 0` defaults to 2 and larger values cap at 2.
- A nil sink creates a safely disabled writer: valid submissions return false and increment `Rejected`; `Close` is safe and idempotent.
- `Submit` validates first, then deep-copies `EndedAt`, `DurationMS`, and every optional attribute pointer before taking the queue lock.
- The mutex covers the closed-state check and non-blocking send, so `Submit` cannot race a channel close into `send on closed channel`.
- The sole worker calls the sink synchronously with a fresh background-derived timeout context. It does not inherit request cancellation and does not create a detached goroutine to simulate timeouts.
- Failed writes increment `WriteErrors`; one retry follows after 50 ms when allowed. Exhaustion increments `Dropped` once for the lost span.
- Normal `Close` seals the queue and drains accepted work. A canceled/expired close context cancels the worker; its in-flight and queued unpersisted spans are counted as dropped before `Close` returns. Sink context compliance is the documented boundary that makes cancellation prompt.
- Stats meanings are documented in code. `Accepted` and `Rejected` describe enqueue admission, while `Dropped` describes data loss both before admission (full queue) and after admission (write exhaustion or canceled shutdown), so those categories intentionally are not all disjoint.

## Verification

Full requesttrace package and vet:

```text
> go test ./common/requesttrace -count=1
ok github.com/labring/aiproxy/core/common/requesttrace 0.484s

> go vet ./common/requesttrace
# exit 0, no output
```

Required race run using the supplied cached toolchain setup (actual Go version 1.27.1):

```text
> docker run --rm --name token-trace-race --label token.task=request-trace-foundation -v token-trace-go-toolchain:/usr/local/go:ro -v D:/AndyLi/token/worktrees/aiproxy-request-trace/core/common/requesttrace:/code:ro -w /code -e GO111MODULE=off -e CGO_ENABLED=1 golang:1.26 sh -c 'go version && go test -race ./...'
go version go1.27.1 linux/amd64
ok _/code 1.148s
```

The command output is also retained in the ignored local diagnostic file `.superpowers/sdd/2026-09-08-request-trace-foundation/task3-race-linux.log`.

## Covered regressions

- full queue returns false without blocking and increments `Dropped`;
- first-write failure followed by second-write success;
- permanent failure stops at the two-attempt cap;
- sink timeout is delivered through context without a per-write goroutine;
- invalid, closed, and nil-sink submissions reject safely;
- zero/negative defaults and upper caps use the exact ruled values;
- mutation after `Submit` cannot change any queued pointer-backed field;
- canceled close accounts for both in-flight and queued spans;
- repeated close is safe;
- concurrent `Submit` and `Close` is race-safe and never sends on a closed channel;
- worker writes use an independent context rather than unrelated caller cancellation.

## Self-review findings and concerns

Self-review found and removed a needless timer-channel drain in the retry-cancellation branch, avoiding a possible blocking cleanup edge. Tests remained green after that refactor.

The implementation intentionally relies on the `Sink` contract to honor context. Calling sinks synchronously is required to avoid detached timeout goroutines; therefore a contract-violating sink can delay `Close` after cancellation. No other correctness or scope concerns remain.
