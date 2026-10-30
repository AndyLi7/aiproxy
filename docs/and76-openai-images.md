# AND-76: first OpenAI Images compatibility slice

Status: local implementation; activation closed pending publication evidence.

## Scope and boundaries

Only GPT Image 1.5 `text-to-image` and `image-to-image`, on the existing
`/v1/images/generations` and `/v1/images/edits` endpoints. The explicitly pinned
profile selects the native retail executor. Native execution and image payload
schemas remain unchanged; identity-history readiness controls compatibility
admission as described below. No Tools execution, provider configuration, price,
refund policy, task center, or billing system is added.

The profile binds the exact native contract hash and must explicitly enable
inline edit transport. Activation must verify all public identities and aliases
against existing image routes; collisions prevent new compatibility admission.
Unrelated legacy routes retain their original path; reserved compatibility IDs
remain protected for replay recovery. No profile is installed by this change. Real
publication revision, frozen contract, quote, inline transport support and route
collision evidence are release prerequisites. Fixtures are synthetic.

## Request and identity

Strict parameter subset: model, prompt, n=1, size, quality, background,
output_format, response_format; edits additionally take one multipart image and
input_fidelity. Unknown, duplicate, streaming, mask, multi-image and n>1 inputs
are rejected before reservation. Size and quality remain governed by the frozen
contract, including its omission defaults. No automatic quality/size mapping.

Edit files are bounded (2 MiB, PNG/JPEG/WebP), validated as image bytes and
encoded inline in native input. This remains below the existing 4 MiB native
request limit. It requires a frozen upstream contract accepting inline images;
no temporary public uploads or new storage credentials. The existing task store
persists the input needed for queued execution. Input bytes must never enter
logs. Larger uploads are explicitly unsupported in this slice.

Within the compatibility identity scope, `Idempotency-Key` defines reliable
operation identity, namespaced by owner
and API key and hashed to a native task ID. Edge X-Request-ID remains a trace.
No-key first calls receive random identities; recognizable SDK retries without
a key are rejected. This can refuse retries after pre-dispatch transient errors
and does not prevent arbitrary repeated requests. Retrying with the same key
and canonical generation input reuses one native billing operation; a changed
input conflicts. response_format is presentation, so replay can choose another
representation without starting another generation. Explicit n=1 and omitted n
canonicalize to the same native num_images=1. Other omissions remain omissions.

## Waiting, delivery and recovery

Native durable reservation, queue, unknown-acceptance handling, worker recovery,
archive and settlement remain authoritative. HTTP waiting only reads stored
state; its deadline never fails/refunds/cancels the task or resubmits upstream.
Pending returns a structured non-success error with task ID and authenticated
GET /v1/model-tasks/{id} recovery location, plus x-should-retry:false. No 200
success without exactly one archived, verified image. Business timeout remains
15 minutes; waiting is not a generation SLA.

Default response is b64_json. Explicit b64_json verifies and bounds the owned
archive before encoding; failure never falls back to a URL. Explicit url returns a 10-minute signed owned image URL for ordinary anonymous
download. Signing first verifies the owner and binds the task, owner, index,
expiry and immutable archive receipt. It uses the existing private storage
transport secret (EXTERNAL_BALANCE_KEY, at least 32 bytes), domain-separated;
provider key digests are not treated as secrets. Rotation invalidates short-lived
URLs; the owner can replay to obtain another. The existing authenticated artifact
route is unchanged. No provider or customer credentials appear in the URL. Delivery failure does not change generation billing.

## Migration, rollback and validation

Reuse NativeTask and existing owner-checked artifact storage; no schema migration
or shared database operation. Disabling the profile stops new compatibility calls;
accepted native tasks remain queryable and recoverable through their existing API.
Roll back binaries only after considering active tasks, as in the native release
procedure. No deployment is authorized here.

Test strict translation, collisions, owner isolation, changed-input conflicts,
concurrent replay, retries without keys, unknown acceptance, timeout, archive
integrity, exactly-one output and parity with native wallet settlement. Exercise
real official Python and Node SDKs against loopback mocks only. App examples and
llms text must not advertise activation without proof. Run relevant builds and
security scan before local commits; review deployment hooks before any push.

## Input evidence and bounds (2026-10-30)

The [provider GPT Image 1.5 edit API](https://fal.ai/models/fal-ai/gpt-image-1.5/edit/api),
Files → Data URI, explicitly accepts base64 data URIs for file inputs. Its schema
names image_urls as the edit input. This verifies supplier capability, not our
current private frozen publication. Activation still requires its exact contract
hash and inline support. Full decoding rejects damaged files after a 16,777,216
pixel check; the 2 MiB raw limit becomes at most 2,796,204 base64 characters.
The complete canonical JSON is also checked against native's 4 MiB limit; HTTP
input has a 3 MiB bound. No resizing or re-encoding silently changes the image.

Accepted replay checks the owner-scoped persistent identity before current
profile selection, so disabling/upgrading the profile does not strand tasks.
A reserved HTTP replay does not resolve a new route while the profile is disabled;
existing native background recovery remains authoritative for work already reserved.

## Local verification checkpoint (2026-10-30)

Baselines: App `5cbb1a99931c5d5f2023bbb387de288f066dd93f`, Gateway
`cc9bc6c1d4e29aacfe8c7e1bf794cb7a50bd0f12`. Both isolated branches are
`codex/and76-openai-image-compat`. No production data, configuration or migrations
were used. No real provider calls, deployments, merge or push occurred.

- Official Node `openai@7.45.1` and Python `openai==3.41.0` (isolated Python 3.12)
  ran against loopback HTTP mocks. Generate/edit, default base64, explicit signed
  URL with ordinary anonymous GET, unsupported options, duplicate operation input,
  pending and SDK network-timeout retries passed. Five intentional operations per
  SDK produced five provider-stub submissions; retries and format changes did not
  add submissions. The full native and compatibility wallet command sequences,
  including delivery/settlement callbacks and frozen quote, matched after removing
  operation-specific IDs. This is synthetic integration evidence, not provider
  availability or production pricing evidence.
- Gateway tests passed for `common/imagecompat`, `common/nativetask`, `controller`,
  `middleware`, `router`, including the SDK harness with explicit SDK environment
  paths. The HTTP distributor uses native mode for opted-in generate/edit calls;
  legacy successful routing remains intact. New tests also check disabled-profile
  replay, owner isolation, signed URL tampering/expiry, unchanged authenticated
  artifact access, archive integrity and no billing changes on HTTP wait expiry.
- Gateway build passed with Go 1.27.0. Incremental golangci-lint 2.15.0 versus the
  baseline passed with zero issues. Existing native unknown-acceptance, concurrent
  claim, wallet recovery and business-deadline regressions remain in the run.
- App targeted suite: 264 tests, 263 passed, one pre-existing skipped test; no
  failures. Covers profile opt-in, native publication, catalog proof gating,
  bilingual/machine documentation and model-ops validation/publishing. `pnpm build`
  passed. TypeScript still reports 260 existing diagnostics, equal to a clean
  baseline tree with generated route/i18n files; no new file/error-code diagnostics.
- Both deterministic security scans and the scoped manual security review passed.
  Native ownership and signing are enforced before downloads; keys stay server-side;
  query inputs are parameterized; input decode is bounded to two concurrent full
  decodes; owned archive integrity is checked; no provider URL fallback exists.

The HTTP completion wait is 30 seconds from parsed admission, not a latency SLA.
Existing native admission/submission has its own two-minute bound; a slow admission
can therefore exceed the completion wait. Output reads have a 30-second bound and
20 MiB cap. Neither wait budget changes the native 15-minute business policy.

### Release prerequisites / rollback

Actual enablement still requires reviewed internal frozen revision and quote,
verified inline-input contract, supported gateway runtime, collision-free public
identities, configured owned storage, a valid PUBLIC_IMAGE_BASE_URL and the existing
private EXTERNAL_BALANCE_KEY (at least 32 bytes). The implementation installs none
of these profiles. A route must explicitly opt in via `config.openaiImages`, and
the publisher checks the exact native contract digest and gateway feature before
writing `x_token_platform_openai_images_v1`. Public SDK examples additionally need
an accepted release record; a draft flag alone does not advertise availability.

Remove the route profile and republish to stop new compatibility admission while
accepted tasks keep native recovery and owner access. Keep the signed download
handler for the 10-minute token lifetime before a binary rollback. Native storage
and billing are unchanged, so no rollback SQL is needed.

At the initial checkpoint, Gateway's remote Release workflow publishes images on
arbitrary branch pushes. A separate local commit
`764d4d0556a3e5f2ef4caa0dcb8a3368f2ae203c` removes the ordinary-branch push trigger,
retaining version-tag publishing and PR builds. Its trigger matrix and static
checks are recorded in Gateway `docs/and76-release-trigger-review.md`. This change
has not been pushed and no remote settings changed; the no-push boundary remains.

### Reproducing SDK checks

Install the pinned official SDKs in isolated environments, then run from Gateway
`core/` with `AND76_PYTHON` pointing to the Python executable and `AND76_NODE_SDK`
pointing to the Node package's absolute `index.mjs` path:

```sh
go test ./controller -run TestImageCompatOfficialSDKs -v
```

The harness rejects non-loopback API origins, uses fake credentials, injects only
synthetic provider/archive transports, and creates its own temporary SQLite DB.
Without both SDK paths this optional test is explicitly skipped; the checkpoint
above ran it with both paths configured.


### Review corrections — 2026-10-30

The first local review requested three corrections; these preserve the same native
execution, billing and publication architecture:

- A keyed operation lookup now fails closed on a task-store error or missing
  LogDB, before current-profile selection or legacy routing. The response includes
  the original owner-derived task ID, status URL, and `x-should-retry: false`.
  Only a confirmed not-found lookup allows keyed legacy fallthrough. Ordinary
  unkeyed legacy requests still work without LogDB. The selector lookup itself
  spends the existing per-key customer read budget. Fault-injection tests cover
  an accepted operation followed by store failure, missing LogDB, confirmed absence,
  and unkeyed legacy success; failed/unknown lookups never reach the synthetic
  provider/wallet dispatch handler.
- Image waits use the existing customer read limiter: at most two concurrent
  waits per owner/API key, with every actual poll spending its shared status-read
  tokens (20/second, burst 40). A saturated waiter returns its latest pending
  state through the existing recovery response; it does not submit, cancel or
  settle. Query context inherits cancellation and the wait deadline. The 32-wait
  concurrency regression leaves three tokens and observes exactly three DB reads,
  no new wallet commands and one original provider submission. Cancellation and
  timeout both release slots; separate owner/key and repeated-release cases pass.
- Publication checks the online configuration set after overlaying this release's
  replacements/removals before writing, then rechecks the complete live namespace
  before recording SDK evidence. Scoped-out and unmanaged configs participate.
  Carried SDK evidence must still exist in that effective set, including new
  metadata-only releases. Tests cover full/scoped legacy ID and alias conflicts,
  no-write rejection with original config/evidence preserved, a late alias with
  rollback, and replacement of the same route without a false collision.

Final local results: App targeted suite 271 tests, 270 passed, 1 existing optional
real-Go HTTP integration skip; seven OpenAI publication cases also passed in
isolation. `pnpm build` succeeded. TypeScript remains at 260 baseline diagnostics
with no additional file/error-code entries. Gateway imagecompat, nativetask,
controller (both official SDK paths configured), middleware and router suites all
passed; the accepted-replay cancellation fixture was then updated and its focused
test passed. Incremental lint reports zero issues and Go build succeeded. All
fixtures are local synthetic data; no real model, shared DB or environment was used.

Evidence under `/private/tmp/and76-openai-image-compat-20261030`:
`review-app-tests.log`, `review-app-focused.log`, `review-app-build.log`,
`review-typescript-comparison.json`, `review-gateway-tests.log`,
`review-replay-test.log`, `review-gateway-lint.log`, `review-gateway-build.log`.

Commands (from the corresponding worktree; SDK dependencies remain isolated):

```sh
# App
DATABASE_URL=file:/private/tmp/and76-openai-image-compat-20261030/validation-fixture.sqlite node --import tsx --test tests/openai-image-profile.test.ts tests/native-result-publication.test.ts tests/native-catalog-docs.test.ts tests/model-capability-llms.test.ts tests/model-ops-validation.test.ts tests/model-ops-publisher.test.ts tests/model-ops-stable-publisher.test.ts
pnpm build
pnpm exec tsc --noEmit

# Gateway core/ (Go 1.27 toolchain)
export GOTOOLCHAIN=local
export GOCACHE=/private/tmp/and76-openai-image-compat-20261030/go-cache
export AND76_PYTHON=/private/tmp/and76-openai-image-compat-20261030/sdk/python312/bin/python
export AND76_NODE_SDK=/private/tmp/and76-openai-image-compat-20261030/sdk/node/node_modules/openai/index.mjs
/Users/andyli7/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.0.darwin-arm64/bin/go test ./common/imagecompat ./common/nativetask ./controller ./middleware ./router -count=1
/Users/andyli7/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.0.darwin-arm64/bin/go test ./controller -run TestImageCompatReplayAfterProfileDisabled -count=1
/Users/andyli7/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.0.darwin-arm64/bin/go build -o /private/tmp/and76-openai-image-compat-20261030/gateway-review-build .
```

Gateway lint used golangci-lint 2.15.0 with `run --fix --new-from-rev=HEAD
--timeout=5m`, while HEAD was `17a1e4d287a8b11b3bff926de220730adef781bf`.
The later workflow-only commit does not change the Go diff or baseline.


### Identity lifecycle correction after second review — 2026-10-30

The project manager approved the lifecycle below. The initial WIP counterexamples
are retained in `identity-lifecycle-counterexamples.log`; they exposed incorrect
allowlist ownership, rejection of never-enabled legacy deployments, and ownership
left by an invalid/conflicting candidate. The implementation and acceptance tests
now preserve the original canonical-ID legacy success cases alongside unrelated-ID
cases. Final local verification is recorded below; manager acceptance remains separate.

An allowed canonical capability is not acquired ownership. A configuration's
accepted IDs become owned only when the existing Gateway configuration transaction
atomically commits a valid opt-in profile, its frozen contract/identity, a complete
final-namespace conflict check, and the internal append-only declaration. This is
a technical activation boundary, not an App publication acceptance or completion
claim. App readback happens later; if it fails after activation, rollback retains
history because requests may already have been admitted. No activation API,
second publication workflow, table, UI, billing state or task/key registry is added.

`OpenAIImageIdentities` is an internal versioned Option containing only the two
supported canonical capabilities and their acquired accepted IDs. Ordinary
`UpdateOption` and `UpdateOptions` calls cannot append, replace, clear or delete it.
It is absent from generic defaults, Option sync and YAML Option overrides. The
existing `SaveModelConfig(s)` / `withMigrationConfigWrite` transaction maintains it.
The same internal Option row serializes first claims and ordinary configuration
writes, including installations without the migration extension. A no-op row write
lock precedes the namespace read, and declaration updates also compare-and-swap.
Batch saves validate the final configuration state. A transaction failure rolls
back both config changes and declaration changes. Invalid/disabled candidates
acquire no identities; conflicts fail before either write. Existing owned IDs
cannot be reassigned to an unrelated legacy/native capability.

The profile and frozen-contract validator is a single pure implementation in the
model layer, shared by persistence and runtime. Namespace validation includes
legacy internal/public names, aliases, disabled configurations and effective YAML
overlays. YAML cannot itself activate a new identity without durable registration.
Successful activation retains ownership through profile disable, alias changes,
config deletion, restart and App readback rollback. No history reclaim operation
is provided.

A successful coherent configuration read with no declaration and no previously
activated compatibility evidence is the normal never-enabled upgrade state. It
allows initial `openai_images_native_v1` capability negotiation subject to the
existing native storage/wallet/signing prerequisites. It does not require manual
empty-history initialization. Unowned legacy routes, including ones named exactly
like the supported canonical capabilities, retain their original behavior.
Unrelated valid-key legacy requests do not query native task storage or consume
native read quota, including when that storage is nil, absent, failing, or the
native read quota is exhausted.

Owned compatibility IDs retain owner/key-scoped operation lookup and fail closed
when native acceptance is unknown. A new compatibility submission requires a valid
selected profile, durable ownership and current configuration readiness. An
unregistered but otherwise valid selected profile cannot admit. Existing accepted
operations remain recoverable after profile/config removal; they never turn into
a new legacy provider invocation.

Identity evidence and routing are loaded as one cache snapshot. A warm configuration
read failure, damaged declaration, or missing/shrunken known history preserves the
last trusted routing pointer/version and its exact identity classification. It
withdraws new activation/readiness until recovery, while unrelated legacy traffic
continues through that trusted snapshot without native-storage dependence. Known
historical operations keep their fail-closed recovery path. Cold unreadable,
malformed or unsupported history is not verified empty: unclassified valid-key
requests fail with `image_identity_history_unavailable` without native queries or
read-token charges. The allowed capabilities alone never establish ownership.

Supported rollout and recovery preconditions:

- Verified Gateway main `cc9bc6c1d4e29aacfe8c7e1bf794cb7a50bd0f12` contains only
  legacy image distributor/relay handlers; this AND-76 branch has not been
  deployed. This supports the first-installation distinction. A cold database
  with all evidence manually erased is indistinguishable from never-enabled;
  the application does not claim to solve that information loss automatically.
- Out-of-band history deletion, older unregistered adapters, and partial database
  restores require separate recovery acceptance before serving affected traffic.
  Do not silently bootstrap them as empty history. Normal rollback and backup
  preserve the complete declaration. `GET /api/option/` does not list this
  internal Option and is not a complete backup; use the existing full database
  backup process. No new backup system is introduced.
- A binary rollback must retain identity protection, or stop admission/reassignment
  of affected IDs. Rolling back to a binary that cannot interpret this declaration
  does not preserve the new code's safety guarantee merely by retaining the row.
- Validate declaration/profile and coherent caches on every serving replica during
  controlled acceptance. One config-write acknowledgement is not a fleet-wide
  cache acknowledgement. Real activation, shared databases, deployment and paid
  generation remain outside this isolated local implementation/test work.

Local verification for this lifecycle correction:

- Full Gateway suites passed: model 175.660 s, imagecompat 4.250 s, nativetask
  15.408 s, controller 34.251 s, middleware 5.088 s, router 10.445 s. Official
  Python/Node SDK fixture paths were explicitly configured. SQLite and isolated
  PostgreSQL tests cover both migration-extension states.
- Race checks for concurrent ownership writes and cache-fault recovery passed
  (3.086 s). The final duplicate-key batch and cold unregistered-adapter boundary
  tests passed separately (1.050 s) after the full run had started.
- Gateway Go build succeeded. Incremental golangci-lint reports zero issues.
  App changes in this correction are documentation only, so its previously
  accepted application tests/build were not repeated.
- Security scan and manual diff review found no secrets, new network calls,
  changed billing/auth boundaries or injectable queries. All data is synthetic;
  shared databases and real providers were not accessed.

Evidence under `/private/tmp/and76-openai-image-compat-20261030`:
`identity-lifecycle-regression.log`, `identity-lifecycle-final-boundaries.log`,
`identity-lifecycle-race.log`, `identity-lifecycle-build.log`,
`identity-lifecycle-lint-complete.log`, `identity-lifecycle-security-staged.log`,
`identity-lifecycle-app-security-staged.log`. The executable is
`gateway-lifecycle-build`, outside Git. Validation used the Go/SDK commands above,
adding `./model` to the full test suite; race used `-race ./model -run
'TestOpenAIImageIdentityConcurrentAppendsPreserveAllSuccessfulIDs|TestOpenAIImageIdentityFailedRefreshKeepsHistoryClosed'`.
