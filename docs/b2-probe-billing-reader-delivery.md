# Migration probe billing: gateway context and actual app reader

Base: `a73678525321e613df204113b6b8856c68055fa0`. Local-only batch; no SQL, schema, production configuration, price, hold, refund or ordinary settlement changes.

## Installed paths and meaning

- `POST /api/channel-migrations/probe-billing-context/read` uses existing header-only admin authorization, rejects query credentials, sets no-store, strictly decodes bounded input `{billingOperationId,taskId,groupId}`. It performs no wallet/provider/issuer calls and no DML.
- A single existing repeatable-read/control-SHARE transaction checks complete storage, original run/authorization/spec/resource/checkpoint and retained target, durable task/input/quote identity and original open use, plus actual upstream request ID. The response contains only typed nonsecret identities, hashes and status. It never sends full key, quote, input, result or arbitrary DB error.
- Missing online issuer does not erase original facts. Cancelled resources remain readable for financial attribution; they cannot regain Prepare permission. `not_probe` means non-association in current complete storage, not proof that no historical operation ever existed. A missing known probe run is `unknown`; partial storage is unavailable. No read manufactures a lost run or reissues generation.
- `balance.ReadMigrationProbeBilling` is installed in the actual probe controller and uses the existing configured `ExternalHTTP` wallet transport. Other backends return missing provenance. It calls only `POST /api/internal/model-ops/migration-probe-billing`, with the existing internal bearer credential. The app response must use `code:0` and the exact `app-migration-probe-billing-v1` envelope/binding.
- Transport has a 60-second total context deadline, redirects disabled, no retries, bounded 32 KiB response, strict JSON and canonical decimal amounts/timestamp (within JS safe integer range). The app's paired pipeline has a 55-second overall cancellation budget and 20-second individual HTTP cap. These are transport budgets, not business TTLs or proof lifetime changes. Parent cancellation propagates to HTTP.
- `fal-workspace-key-membership-v1` identifies a separate current observation of original/target key membership and the same actual bill, not a permanent account ID or historical continuous membership. The existing finalizer still rechecks current proof/resource/task/settled facts after the network call. No control lock is held during the app callback into the gateway context reader.

## Wire

Context result fixes `version/source/state/reason/epoch/storageGeneration/revision/context`. Revision is decimal text. States: ready, not_probe, pending, unknown, conflict, unavailable. Ready context contains migration operation, authorization/command/resource/checkpoint/spec/task hashes, original use, from/to physical identities, credential IDs, exact `MigrationProbeBillingRequest`, resource lifecycle and task/billing status. Ready is original-chain availability, not migration authorization.

App result fixes version/source/state/reason/evidence. States ready/pending/missing_provenance/conflict/unavailable. Ready evidence requires exact original binding, evidence ID, provenance hash, membership hash/kind, USD and canonical decimal cost/customerDue/charged/settledAtMs. Due must equal charged; existing finalizer checks the original quote and task settlement. No legacy matched bill is silently upgraded.

## Verification

Evidence directory: `/private/tmp/b2-probe-billing-final/`.

- `go test -race -json ./core/model ./core/common/balance ./core/controller ./core/router -run 'MigrationProbe|MigrationRead|MigrationShadow|PreparedMigration|ChannelAdmission' -count=1`: 243 pass, 89 opt-in skips, no failure (`race-final2.jsonl`). The skips are not claimed as executed. After fixture-only feature-source adjustment, controller probe tests passed again: 4 pass / 1 opt-in interop skip (`controller-final2.jsonl`).
- Real temporary PostgreSQL 16: `TestPostgresMigrationProbeBillingContextConsistentNoDML`, four subscenarios plus parent = 5 pass, no skips (`pg-final.jsonl`). Actual lock waits cover live/cancellation/task binding change/missing run. Repeatable-read returns coherent old state or serialization unavailable; retry sees coherent new state. DML-denying triggers verify read behavior. SQLite `query_only` verifies no writes separately.
- `go build ./...` in root, `core`, and `common`: passed (`build-final.log`).
- Real HTTP authorization/body/secret-output checks; no issuer read; cancelled read with rejected Prepare; strict amounts/bindings; 307/308 prevention and cancellation propagation.
- Actual Go native submit/poll + loopback simulated app callback + original context HTTP + Prepare: first callback 503 leaves no Prepared; retry uses original task and succeeds without another generation or task mutation (`TestMigrationProbeBillingHTTPConnectedRetryNoRegeneration`). This test is not the real app implementation.
- Real app interoperability is coordinated separately with the app author; do not attribute their results to the simulated app test. `TestMigrationProbeBillingInteropServer` is an opt-in loopback fixture using actual native Submit/Poll with a synthetic supplier/wallet and a real app URL supplied privately. Its initial metadata is phase `resource`; the actual app builder/resource service must persist shadow_ready before writing `app-prepared`. Only then is one native task generated and metadata atomically replaced with phase `completed`. The app invokes Prepare through the actual Go HTTP reader, which calls back into the gateway context endpoint.
- Fixture setup previously used nil feature lists for model-level source creation while real HTTP uses runtime features; this correctly caused whole-source conflict. The first fixture correction still omitted `store.MigrationFeatures` on its injected probe store; the actual runtime factory already sets it. All three fixture sources (inventory/create, HTTP read and probe store) now use the same runtime features, controller regressions pass, and actual shadow GET must be ready before publishing metadata. No production validator was weakened.
- An initial new HTTP test expected transport 409; existing probe API represents business conflict in a 200 typed envelope. The final test checks state conflict, null observation and no Prepared, preserving the API contract.

PostgreSQL harness: `/private/tmp/b2-probe-billing-final/reproduce-pg.py <exact source> <output-jsonl>`. Temporary container removed in finally; credentials only process environment, no DSN file. Cleanup record is `pg-final.jsonl.cleanup`.

Interop reproduction: `B2_PROBE_BILLING_HTTP_FIXTURE_DIR=<fresh absolute private directory> go test ./core/controller -run '^TestMigrationProbeBillingInteropServer$' -count=1 -timeout 35m`. Private metadata includes only synthetic credentials. The app writes `app-prepared`, then `app.json` with loopback baseUrl/key; write `stop` after verification. Harness removes metadata/app.json and closes servers/DB. Do not publish the metadata or substitute real credentials/provider calls.

## Remaining gates

Only native fal request/bounded_request probes are supported. Runtime issuer, full-fleet continuous fencing, independent recovery watermark, production membership observation/configuration and safe sealed recovery/reverse remain required. No runtime test issuer is registered in production. Missing proof or attribution remains pending/unavailable. This batch does not expose Seal/Commit HTTP or authorize migration rollout; local test fixtures do not prove a live fleet. Existing app coordinator deployment must be accepted separately before this reader can produce ready evidence in a real environment.
