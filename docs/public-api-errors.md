# Public API errors

For public /v1 relay requests, authentication failures return HTTP 401 with
error.type = authentication_error and error.code = invalid_api_key.

Customer balance failures return HTTP 402:
```json
{"error":{"message":"Your account balance is insufficient.","type":"insufficient_quota","code":"insufficient_balance"}}
```
Clients should prompt for a balance top-up, not retry automatically or replace
the API key. Internal account/group identifiers are retained in server logs,
not included in this public balance response.

`POST /v1/model-tasks` returns the same code and type when the balance does not
cover the task's prepayment hold, with the message "Your balance does not cover
this request's hold; see pricing.prepayment in /v1/models." When
pricing.prepayment has `scales_with`, the hold is the price of the request's own
metered input, at most `amount` (a request with an undeclared top-level field,
another top-level text over 64 characters that is not an `enum` or `const`
value, or more than 64 characters of text inside another top-level list or
object, holds `amount`); otherwise it is `amount`. The refused request
leaves nothing behind: after a top-up, retry with the same X-Request-Id.

Model access/resolution and published image-contract validation run before
the wallet lookup. Invalid model IDs or image-contract parameters therefore
remain diagnosable with an empty balance. This does not move all provider-side
validation ahead of billing admission.

Responses expose X-Request-ID (also accessible through CORS). Include this ID
when reporting a failure. Safe caller-provided IDs are retained; otherwise
the gateway generates one.

Published image-schema validation errors return HTTP 400 with
error.type = invalid_request_error, error.code = invalid_parameter, error.param,
and error.expected. Enum constraints also expose error.allowed_values.
Size constraints describe supported presets, pixel bounds and aspect ratios.
Submitted prompts and raw schema-validator diagnostics are never echoed.
Provider-side errors and legacy routes without a published contract may still
return a generic error.

The gateway /v1/models response omits legacy permission, parent and root fields;
records retain id, object, owned_by and created. Rich capability/pricing discovery
remains available through the application catalog APIs.

The application /v1/videos/models endpoint requires a valid Bearer API key,
returns only entitled listed video models, uses no-store caching, and returns
401 invalid_api_key for absent, malformed or rejected credentials.

Async generation tasks (`POST /v1/model-tasks` and `POST /v1/images/tasks`)
have one deadline for every model: a task the provider accepted but has not
finished 15 minutes after acceptance ends with status = failed and
error.code = generation_timeout:
```json
{"status":"failed","error":{"code":"generation_timeout","message":"The provider did not finish this task within 15 minutes, so it was stopped. You were not charged; submit a new task with a new X-Request-Id."}}
```
The clock starts when the provider accepts the task, not at the first request.
The prepaid hold is refunded in full and the provider is asked to cancel (best
effort); a result that arrives later is discarded and never billed to the
customer. Retrying with the same X-Request-Id returns the same failed task, so
submit again with a new X-Request-Id. Clients should keep polling for about 20
minutes before giving up locally. A submission the provider never confirmed
(its answer was lost or ambiguous, so there is no provider task) ends the same
way 15 minutes after it was submitted: generation_timeout, refunded in full;
it is never resubmitted and there is nothing to cancel. On /v1/images/tasks, an
image the provider already returned that is still being stored (status
in_progress, phase result_processing) is not covered.

When the provider refuses a submission for reasons on its or the platform's
side (provider HTTP 401, 402, 403, 404 or 429: credentials, payment or an
exhausted provider balance, an unknown endpoint, throttling), or the connection
to the provider failed before the request was sent (for example a DNS or
connect error), it created no task. The task fails at once with error.code =
upstream_unavailable and the prepaid hold is refunded in full:
```json
{"status":"failed","error":{"code":"upstream_unavailable","message":"The provider is temporarily unavailable, so this task was not started. You were not charged; try again later with a new X-Request-Id."}}
```
On /v1/images/tasks another configured channel is tried first when failover is
enabled. Provider answers that reject the customer's input (400, 413, 422, 451)
keep their existing codes. Every other /v1/images/tasks failure is still
reported as generation_failed.

On /v1/model-tasks, when the provider rejects the input and names the fields,
at submission or after accepting the task, the task fails with error.code =
invalid_parameters and up to 8 issues. Each issue has only a parameter path
(`voice`, `voice_setting.voice_id`, `image_urls[0]`) and a rule: required,
string, integer, number, boolean, array, object, allowed_value, range, length,
multiple, invalid, unsupported_value, file_size, file_format, file_unreadable,
duration or content_policy. Provider messages and the submitted values are
never returned. The prepaid hold is refunded in full:
```json
{"status":"failed","error":{"code":"invalid_parameters","message":"The provider rejected the listed input parameters. You were not charged; fix them and submit a new task with a new X-Request-Id.","issues":[{"field":"voice","rule":"unsupported_value"}]}}
```
Fields the platform sets itself (not in the model's input schema) are never
listed. A rejection that names no other field keeps upstream_rejected (at
submission) or upstream_result_rejected (after acceptance).

## Model IDs

The `model` of a request is an `id` from `GET /v1/models`. Image and video IDs
end with the capability (`alibaba/wan-2.7/text-to-image`); an audio model's ID
is the model ID (`elevenlabs/eleven-v4`). A model group ID (`group.id` when it
differs from `id`) is not callable. Task responses (`POST` and
`GET /v1/model-tasks`) return the callable ID as `model`, also when the request
used an older ID that still calls the same model.

`POST /v1/model-tasks` with an ID this API key cannot call returns 404 in the
native envelope. Do not resend it: use an ID from `suggested_models` (callable
IDs of the same model or group, at most 10, possibly empty) or an `id` from
`GET /v1/models`:
```json
{"error":{"code":"model_not_found","type":"not_found_error","param":"model","message":"This ID cannot be called with this API key. Do not resend it: use an ID from suggested_models or an `id` from GET /v1/models.","suggested_models":["alibaba/wan-2.7/image-to-image","alibaba/wan-2.7/text-to-image"]}}
```
When the ID names a model this key calls on another endpoint (for example an
image model served by `/v1/images/generations`), it returns 400:
```json
{"error":{"code":"native_model_unavailable","type":"invalid_request_error","param":"model","message":"This model is served on a different endpoint. Read its openapi_url in GET /v1/models; suggested_models lists the IDs to use there.","suggested_models":["bytedance/seedream-4.5/text-to-image"]}}
```
A 404 when querying a task (`GET /v1/model-tasks/{id}`) is `task_not_found`,
which is different: it concerns the task ID, not the model.

The fingerprint of `X-Request-Id` covers the exact request body. Resending the
same body is idempotent; resending the same `X-Request-Id` with another
spelling of the model ID (an older ID instead of the listed one, or the
reverse) returns 409 `request_id_conflict`.

`POST /v1/images/generations` and `POST /v1/images/tasks` answer a model ID
that calls nothing on them (unknown, retired, or another endpoint's model) with
404 `model_not_found`, type `not_found_error`, param `model`. `suggested_models`
lists the capability IDs of the requested model group the key may call, and is
otherwise empty:
```json
{"error":{"code":"model_not_found","type":"not_found_error","param":"model","message":"This ID cannot be called with this API key. Do not resend it: use an ID from suggested_models or an `id` from GET /v1/models.","suggested_models":[]}}
```
A model group ID (`bytedance/seedream-4.5`) still selects a capability by the
request parameters until the gateway sets `DISABLE_IMAGE_GROUP_IDS=true` (owner
decision D3). From then on it returns 404 with the group's capability IDs:
```json
{"error":{"code":"model_not_found","type":"not_found_error","param":"model","message":"This model ID names a group of capabilities and cannot be called. Do not resend it: use an ID from suggested_models or an `id` from GET /v1/models.","suggested_models":["bytedance/seedream-4.5/text-to-image"]}}
```
The video endpoints keep accepting group IDs; an unknown video model returns
400 `unsupported_by_model` with `allowed_values`.

`GET /v1/models/{id}` accepts any ID that calls a listed model and returns that
model's entry. An unknown ID returns 404 `model_not_found` with
type `not_found_error` (the same type as the schema's `schema_not_found`).
