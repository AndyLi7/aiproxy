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
