# Local cache observation — provisional delivery

## Source and scope

This temporary source is a verified tracked-file copy/combination of 07f63e81de1f74afb3b0616397f188dd921b3a80 plus the five reviewed 195431a9db8d4f746e7400d07ed4268bf0eba9b5 files listed in `/private/tmp/b2-gateway-read-combined-lqu6d3qw/combination.json`. It was not produced by a successful git archive while the iCloud pack is unavailable, and is not an exact 63f4e707 checkout. Git worktree creation is blocked by a macOS cloud-backed pack download; no temporary Git repository/commit, push or deployment has been made. The final branch must receive only this patch, then complete the staged security scan before commit.

GET `/api/channel-migrations/operations/:id/cache` uses the existing strict header AdminAuth, rejects query credentials and bodies, and returns no-store typed facts. `loaded` means this process has actually published the matching effective cache. It is not a fleet ACK, authenticated membership, switch confirmation, drain timestamp, disable or revoke permission.

The existing rebuild reads config rows, channels, control and heads in one database snapshot. PostgreSQL uses repeatable-read READ ONLY without FOR SHARE; SQLite uses one read transaction. Effective caches and private provenance publish through the same ModelCaches atomic pointer. Failed rebuilds leave the previous pointer intact. Exact raw DB pin hashes remain authoritative; numeric comparison never rounds through float64. Unsupported serializer precision loss, YAML overrides, disabled DB model configs, partial storage or bounded-source overflow cannot produce loaded. Numeric normalization rejects huge exponent expansion. Ordinary legacy cache loading remains available without migration extensions.

The boot nonce is generated once per process and exposed through ProcessBootIncarnation. It is not a credential or issuer. A separately reviewed runtime bootstrap may authenticate/bind the same nonce; this endpoint does not register any runtime. Response fields contain only operation/head/hash/revision/time and local boot/cache-version facts, never keys, raw configs or arbitrary errors.

## Tests

Evidence is in `/private/tmp/b2-cache-final/`.

- Final broad relevant race suite: review-fixed-race.jsonl, 136 pass / 5 opt-in skips. This includes the numeric exponent guard and cross-review admission-only legacy compatibility fix. Prior race.jsonl is retained (134 pass / 5 skips), not substituted for final evidence.
- Cache-specific final race suite: cache-final-race.jsonl (counts in accompanying manifest); covers actual Gin auth, paused pointer publication, effective config mutation, actual YAML overlay in an isolated child, SQLite coherent snapshot and legacy loading.
- Disposable PostgreSQL: pg-review-fixed.jsonl, 3 parent cases pass, including two actual Go processes, one stale cache and one refreshed cache, old-cache Admit refusal after cutover, graceful process exit and fresh boot/cache-version reset, superseded head rejection, concurrent database snapshot changes, and triggers rejecting DML.
- Builds: core review-fixed build; mcp-servers and openapi-mcp module builds. There is no root go.mod and no separate common module.

Commands:

```sh
go test -race ./core/model ./core/controller ./core/router -run 'Test(MigrationCache|PostgresMigrationCache|CacheRebuild|ConcurrentCache|PreparedMigration|AtomicCutover|MigrationRetention|MigrationRead|MigrationCutover|ChannelMigration)' -count=1 -timeout=180s -json
go test -race ./core/model ./core/controller -run '^Test(MigrationCache|CacheRebuild|ConcurrentCache)' -count=1 -timeout=90s -json
python3 /private/tmp/b2-cache-final/reproduce-pg.py /absolute/source /private/tmp/result.jsonl
# In each of core/, mcp-servers/, openapi-mcp/:
go build ./...
```

The PostgreSQL runner uses an already cached postgres:16-alpine image, fresh tmpfs storage, synthetic credentials and loopback port only. It removes the container; no DSN file is written. Child connection metadata is removed when each child exits. Existing native fixtures use a synthetic HTTP supplier; there are no paid provider calls.

The cache concurrency fixture now explicitly uses SQLite WAL, because its assertions require a writer commit while a snapshot reader is paused. Production DSNs were not changed. Earlier default-journal test blocking and expectation corrections remain in temporary logs rather than being called passing evidence.

## Limits / remaining integration

No schema, database mutation, new refresh endpoint, business TTL, billing rule, UI or application ACK was added. The actual process-level observations must later be joined to an independently authenticated, complete and continuously fenced participant set. A deployment inventory or this endpoint's success cannot prove there are no old replicas. Recovery watermarks and sustained issuer/fleet proof remain separate gates. The old identity remains protected by existing DB admission barriers even when a process retains its old cache.

Provisional security review uses the project scanner's filename, added-secret and file-size checks plus manual full-diff review. Git-specific tracked/ignore/staged checks remain pending while cloud pack hydration blocks the real worktree; this document does not claim the full pre-commit scan has passed.

## Cross-review correction

The reviewer reproduced an admission-only installation regression: all five admission tables existed, control was never activated, and no migration extension tables existed. The original metadata reader attempted migrationStorageReady and blocked ordinary cache rebuild. The final code first checks complete migration-extension absence, returns nonauthorizing legacy provenance, and permits normal refresh. Any partial extension still fails migration readiness. The added real database regression updates a channel and verifies the new name is actually refreshed. The reviewer independently re-ran the original failure after the correction; no additional concrete defects were reported.
