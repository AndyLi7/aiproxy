# Task 2 report — trace database store and update rules

## Scope and files

Implemented only the Task 2 persistence layer:

- `core/model/request_trace.go`: trace head/span models plus `NewTraceStore`, `Migrate`, and transactional `Write`.
- `core/model/request_trace_test.go`: SQLite behavior and rollback tests using a separately closed `t.TempDir()` database.
- `core/model/request_trace_postgres_integration_test.go`: real PostgreSQL contention test in a random isolated schema, with connection and schema cleanup.

No queue, query/list API, route, runtime migration registration, cleanup scheduler, deployment, or existing business table was changed.

## RED evidence

The complete initial Task 2 test set was run before the production store existed:

```text
> go test ./model -run Trace -count=1
# github.com/labring/aiproxy/core/model [github.com/labring/aiproxy/core/model.test]
model\request_trace_postgres_integration_test.go:18:11: undefined: NewTraceStore
model\request_trace_postgres_integration_test.go:47:11: undefined: RequestTraceHead
model\request_trace_postgres_integration_test.go:53:31: undefined: RequestTraceSpan
model\request_trace_test.go:17:11: undefined: NewTraceStore
...
FAIL github.com/labring/aiproxy/core/model [build failed]
```

The failure was the expected missing feature, not a fixture or environment error.

## GREEN and PostgreSQL evidence

Focused SQLite/model behavior:

```text
> go test ./model -run Trace -count=1
ok github.com/labring/aiproxy/core/model 0.716s
```

The required PostgreSQL race was executed against the supplied disposable test database, not a mock or generated-SQL assertion. The test creates a random schema, races two transactions for positions 256 and 257, verifies exactly 256 rows and `truncated=true`, then drops the schema and closes both pools:

```text
> go test ./model -run TestTraceStorePostgresSerializesConcurrent256thAnd257thSpan -count=3
ok github.com/labring/aiproxy/core/model 13.142s
```

The PostgreSQL test reads `TRACE_TEST_POSTGRES_DSN` and skips with an explicit message when it is absent. No credential or fallback DSN is stored in tracked files.

## Store and schema decisions

- `request_trace_heads` uses `(trace_id, service)` as a composite primary key and stores immutable `group_id`, bounded `span_count`, sticky `truncated`, and indexed `updated_at` for the future 14-day cleanup query.
- `request_trace_spans` uses `span_id` as the primary key and stores the validated safe projection: version, trace/service/group ownership, request/parent/stage/start identity, lifecycle state, typed whitelisted attributes, and timestamps.
- Every write calls `requesttrace.Validate` before opening the transaction. `unknown` is accepted with nil end/duration and is treated as terminal without invented timing.
- A transaction idempotently creates the head, locks its row with `FOR UPDATE` on PostgreSQL, then verifies group ownership. SQLite relies on its serialized write transaction and the caller's bounded busy retry policy.
- Existing span identity is immutable across trace, service, group, request, parent, stage, and start time. Reuse under another owner returns an error and rolls back any head creation.
- A new span reserves quota atomically while holding the head lock. Insert errors roll back both quota and a newly created head. At 256 spans, later new spans are not persisted, return success to avoid retry churn, and make the head's truncation bit sticky.
- Existing rows accept only a greater revision while still `running`. The parameterized update explicitly contains only `status`, `ended_at`, `duration_ms`, `revision`, and `updated_at`; identity and attributes cannot be rewritten.
- Any non-running status is terminal. A terminal row cannot be resurrected by `running` or replaced by another terminal status, even at a larger revision. Thus revision 2 completion may arrive first and revision 1 can never recreate or downgrade it.
- Same/older revision replay does not update span/head retention timestamps. A newly observed sticky truncation bit can still be recorded with `UpdateColumn` without refreshing retention.

## Covered regressions

- completion revision 2 before start revision 1, one terminal row;
- normal running-to-terminal update with immutable attributes;
- all immutable identity fields, cross-group writes, and cross-trace span-ID reuse;
- forced insert failure quota/head rollback;
- `unknown` terminal behavior and no fabricated timing;
- same-revision retention timestamp stability and sticky truncation;
- aggregate attribute validation, safe attribute projection, and canceled context;
- actual PostgreSQL transaction contention at the 256/257 cap.

## Round 1 review fix — PostgreSQL timestamp precision

The reviewer identified that PostgreSQL rounds persisted timestamps to microseconds while recorder snapshots can retain nanoseconds. A revision 1 start inserted with sub-microsecond nanoseconds therefore compared unequal to the otherwise identical revision 2 completion and was rejected as an ownership mismatch.

An actual PostgreSQL start-to-finish regression with `StartedAt` nanoseconds `123456789` reproduced the failure before the fix:

```text
> go test ./model -run TestTraceStorePostgresMatchesLifecycleAfterTimestampPrecisionRoundTrip -count=1
--- FAIL: TestTraceStorePostgresMatchesLifecycleAfterTimestampPrecisionRoundTrip (0.08s)
    request_trace_postgres_integration_test.go:34: Received unexpected error:
        request trace span ownership mismatch
FAIL github.com/labring/aiproxy/core/model 0.416s
```

`StartedAt` is now truncated to microsecond precision both before insertion and during immutable identity comparison. `DurationMS` remains the recorder's independent measurement and is not recomputed from canonicalized wall-clock timestamps.

The same PostgreSQL regression then passed:

```text
> go test ./model -run TestTraceStorePostgresMatchesLifecycleAfterTimestampPrecisionRoundTrip -count=1
ok github.com/labring/aiproxy/core/model 0.474s
```

Final focused verification, with the disposable PostgreSQL DSN supplied through the environment, covered all SQLite and PostgreSQL trace tests:

```text
> go test ./model -run Trace -count=1
ok github.com/labring/aiproxy/core/model 3.903s

> go vet ./model
# exit 0, no output
```
