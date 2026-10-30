# AND-76: first OpenAI Images compatibility slice

Status: local implementation; activation closed pending publication evidence.

## Scope and boundaries

Only GPT Image 1.5 `text-to-image` and `image-to-image`, on the existing
`/v1/images/generations` and `/v1/images/edits` endpoints. The explicitly pinned
profile selects the native retail executor. Existing native and legacy image
contracts remain unchanged. No Tools execution, provider configuration, price,
refund policy, task center, or billing system is added.

The profile binds the exact native contract hash and must explicitly enable
inline edit transport. Activation must verify all public identities and aliases
against existing image routes; collisions disable compatibility rather than
stealing a successful route. No profile is installed by this change. Real
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

`Idempotency-Key` alone defines reliable operation identity, namespaced by owner
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
llms text must not advertise activation without proof. Run both builds and
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

Gateway `.github/workflows/release.yml` publishes images on arbitrary branch pushes.
Consequently this checkpoint is local-only: pushing cannot be treated as a harmless
PR preparation step under the current no-release authorization. The project manager
must review/coordinate that release hook separately before any push.

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
