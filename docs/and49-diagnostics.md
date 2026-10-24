# AND-49: capability diagnostics match native route resolution

## Problem and root cause

`GET /api/capability_resolution?model=...` is an admin configuration check.
Previously `CheckCapabilityResolution` called `ResolveImageCapabilityRoute`, which
only recognizes `text-to-image` and `edit`, then tried a separate video suffix
fallback restricted to `text-to-video` and `image-to-video`. It did not consult
native identities at all. A configured native speech capability, its callable
`public_api_id`, and its aliases were consequently reported as unmapped, with
zero channels, despite being resolvable by native routing.

The actual native entry point is `NativeTasks` in `core/controller/native_task.go`
→ `middleware.NewDistribute(mode.NativeTasks)` → `resolveDistributionModels`
→ `resolveNativeCapability` → `model.ResolveNativeCapabilityRoute`. That resolver
calls `PublicCapabilityIdentityFromConfig`, honors exact capability ID / public
API ID / alias priority, supports older configs containing only
`public_capability_model`, and rejects multiple matches at the same priority.

## Reproduction (no real keys or paid calls)

The executable reproduction is `TestCheckCapabilityResolutionRouting` in
`core/controller/capability_check_test.go`. It installs and restores a synthetic
in-memory cache and invokes the handler with Gin/httptest. It uses no database,
API credentials, token, billing flow, generation request, or network client.

1. Use Go 1.27 (this run used go1.27.3 linux/amd64).
2. From `core`, run:

   ```sh
   go test ./controller -run '^TestCheckCapabilityResolutionRouting$' -count=1 -v
   ```

3. The main fixture has route `demo/voice::text-to-speech`, type
   `mode.NativeTasks`, and these config values:

   ```json
   {
     "capability_contract_version": 1,
     "public_model": "demo/voice",
     "capability": "text-to-speech",
     "public_capability_model": "demo/voice/text-to-speech",
     "public_api_id": "demo/voice",
     "public_capability_aliases": ["demo/alias"]
   }
   ```

4. Assign synthetic channel ID 42 to the internal route in two channel sets.
   This should count as one distinct enabled channel.
5. Query each public ID. The test also directly confirms that the native
   resolver finds the expected route for the canonical ID.

### Observed before the production fix

The regression test was written and run **before modifying the handler**. It
failed (exit 1) in the native, native public API ID, alias, and legacy native
identity subtests. The original seven-case run passed legacy image protocol,
missing config, and the old-field assertions for ambiguous config (the old
response could not distinguish ambiguity from missing config).

For `demo/voice/text-to-speech`, the original handler returned:

```json
{"data":{"capability":"","enabled_channels":0,"mapped":false,"public_id":"demo/voice/text-to-speech","registered":false,"routing_model":"demo/voice/text-to-speech"},"success":true}
```

The shared native resolver returned `NativeRouteFound` and
`demo/voice::text-to-speech` for the same config and ID. The public API ID and
alias similarly returned their input unchanged, unmapped and zero channels.
The older native fixture (`public_capability_model` only) also failed to map.

## Fix and response compatibility

The diagnostic now passes `EnabledModelConfigsMap` values to the existing
`ResolveNativeCapabilityRoute`. It reads the returned identity rather than
parsing native IDs itself. It preserves the existing legacy image/video
fallbacks when no native route matches. An ambiguous native result is terminal:
no fallback, guessed route, or channel count from a conflicting ID. Internal
`::` inputs bypass native identity resolution, matching the middleware guard.

The admin check spans globally enabled configs. Actual routing resolves among
configs the customer's token may call. Thus global ambiguity can coexist with
a unique resolution for a token entitled to only one claimant. This endpoint
is not a token permission check or proof of customer access.

Existing fields remain with their existing types:
`public_id` (string), `routing_model` (string), `capability` (string),
`registered` (boolean), `mapped` (boolean), `enabled_channels` (number).
`mapped` now correctly includes native matches, including older native configs
without a full identity (whose `capability` remains empty). Distinct channel IDs
are still counted across sets. On ambiguity, the requested ID is retained as
`routing_model`, with `mapped=false`, `registered=false`, and zero channels.

Additive fields:

| Field | Values / meaning |
| --- | --- |
| `resolution_status` | `resolved`, `unmapped`, or `ambiguous` |
| `config_ok` | A mapped, registered route with at least one enabled channel |
| `check_scope` | Always `global_enabled_config` |
| `generation_status` | Always `not_attempted` |

`config_ok` is only evidence of route configuration, not parameter validation,
provider credentials/availability, permission, price, wallet balance, or real
generation success. The existing `success: true` envelope means the diagnostic
request completed; it does **not** mean generation succeeded. No generation is
attempted by this endpoint, even when `config_ok=true`.

The token-platform consumer was inspected read-only: its parser validates and
selects the existing fields and ignores additions. Its access summary calls the
result `configured`. No UI changes are required for compatibility.

## Regression results after the fix

| Synthetic case | Result |
| --- | --- |
| Native canonical capability ID | Resolved native route, speech capability, one channel |
| Native public API ID | Same native route and channel |
| Native alias | Same native route and channel |
| Legacy image protocol | Existing image mapping preserved |
| Legacy video protocol | Existing video mapping preserved |
| Older native config with published ID only | Resolved, mapped, empty capability, one channel |
| Missing config | Unmapped, unregistered, zero channels, config not OK |
| Duplicate public API ID | Ambiguous, unmapped, zero channels, config not OK |
| Duplicate canonical ID | Ambiguous; no legacy fallback |
| Duplicate alias | Ambiguous, unmapped, zero channels |
| Ambiguous native image ID | Ambiguous; legacy image resolver cannot override rejection |
| Canonical ID also claimed as an alias elsewhere | Canonical match wins, as in actual routing |
| Native route with no enabled channels | Resolved and registered, config not OK |

Every case asserts `generation_status=not_attempted`, including successful
configuration checks, and checks the unchanged response fields and added status
fields. The fixture also verifies channel deduplication across sets.

## Scope and validation

Production changes are confined to `core/controller/capability_check.go`.
Tests are in `core/controller/capability_check_test.go`; this document records
the reproduction and root cause. Actual routing, permissions, and billing code
are unchanged. No commit or push is performed.

Validation from `core`:

- Before fix: the initial seven-case regression run failed as recorded above.
- After fix: `go test ./controller -run '^TestCheckCapabilityResolutionRouting$' -count=1 -v` passed all 13 cases.
- `go test ./model -run 'Test(PublicCapabilityIdentity|ResolveNativeCapabilityRoute|ResolveImageCapabilityRoute)' -count=1` passed.
- The full `go test ./...` completed with exit 1. `controller` and `middleware` passed. Docker-dependent PostgreSQL/Redis tests failed in `common/nativetask`, `controller/mcp`, and `model` with `get provider: rootless Docker not found, failed to create Docker provider`. These require a working Docker provider; the full suite is not green in this environment.
- `go build ./...` passed (exit 0).
- `git diff --check` passed.
