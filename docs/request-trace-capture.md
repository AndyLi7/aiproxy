# Request trace capture (B1)

Request tracing is an opt-in diagnostic facility for the AI proxy gateway. B1 records bounded lifecycle metadata for supported image and video requests without changing routing, responses, retries, billing, or task state.

## Scope

B1 captures `POST /v1/images/generations`, `POST /v1/images/edits`, `POST /v1/videos`, and the supported video status/content/delete routes. A trace is created before authentication and is bound once to the group resolved by `TokenAuth`; client group, trace, source, `traceparent`, and request-ID headers never establish ownership. Unsupported and legacy routes are not synthesized into traces.

The gateway stages are request, authentication, balance check, model resolution, channel selection, validation, and upstream attempt. Video submission records submission only. Completion observed by later asynchronous work is intentionally not inferred from the submit response.

## Safety and privacy

The typed attribute contract permits only public/upstream model identifiers, numeric channel and attempt identifiers, normalized error code and HTTP status, and bounded media dimensions/duration flags; B1 populates only values established at trusted gateway boundaries. Prompt or request bodies, API keys, raw upstream errors, media URLs, authorization headers, and arbitrary client attributes are never trace fields. Each trace is group-scoped, limited to 256 spans, and retained for 14 days. Each serialized attribute set is limited to 2 KiB.

Trace writes use a bounded non-blocking queue (maximum 4096 updates). A full queue, blocked or failing sink, disabled runtime, failed initialization, and cleanup failure are observable through trace health but do not change the gateway response or cause an extra provider call. Writes retry at most once; completion updates remain admissible after a dropped start so the idempotent store can recover lifecycle state.

## Operation

Tracing is disabled unless explicitly enabled at process startup. Enabling it requires a ready log database, runs the trace migration, and starts periodic bounded cleanup under that same opt-in runtime. Do not enable it merely by deploying this code. The administrative trace endpoints are read-only, require the configured admin bearer credential, enforce exact group ownership, use stable cursors, default to 50 records, and cap pages at 100.

Health counters distinguish disabled/not-ready initialization, accepted and persisted writes, invalid/closed rejection, queue or exhausted-write drops, write errors, and cleanup errors. UTC timestamps are for display. Durations come from the process monotonic clock and must not be used to infer cross-service network time.

## Verification boundary

The acceptance fixture uses the real Router, TokenAuth, distributor, relay controller, and OpenAI adaptor against a local fake HTTP provider and disposable databases. It exercises image generation, multipart image edit, and video submission without provider network access, and waits for asynchronous consumption to finish before restoring global fixtures.

B1 does not implement cross-service signatures, replay protection, or asynchronous app-to-gateway correlation; those belong to B2. Broader platform correlation remains C scope. Therefore B1 completion must not close issue #55, deploy the gateway, or automatically turn collection on.
