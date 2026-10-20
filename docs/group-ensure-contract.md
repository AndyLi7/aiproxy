# Group ensure contract

`POST /api/group/:group/ensure` is protected by the existing admin middleware.
It accepts only a JSON object with optional initial `status` (1, 2, 3; default 1)
and `available_sets`. The complete request body is limited to 16 KiB (16,384
bytes), including whitespace. The controller reads at most 16,385 bytes and
explicitly rejects overflow with HTTP 413 before decoding or inserting; it does
not trust Content-Length. Exact-limit valid JSON is accepted. Unknown fields,
malformed bodies and trailing JSON within the limit return HTTP 400. JSON hidden
beyond whitespace cannot be mistaken for EOF. All rejected bodies leave the
database unchanged. Initial
settings are used only when inserting a new group. Existing persisted values win,
even if the caller proposes different defaults. The response contains the actual
persisted group; callers requiring particular permissions must validate it.

Insertion uses a primary-key-targeted conflict no-op and reads back the winner.
It never updates existing group state, token associations, quotas or model configs.
Unrelated database errors are not converted to success. It invalidates group
caches on success, including local negative entries from an earlier absence read;
it never populates a cache using the proposed defaults. Redis invalidation errors
retain the existing logged-error policy.

`GET /api/group/:group` now returns HTTP 404 plus
`{"success":false,"code":"group_not_found","message":"group not found"}`
only for a typed record-not-found error. Other database errors remain 500.
Legacy `POST /api/group/:group` behavior is unchanged, including strict duplicates.

Deploy gateway before the companion token-platform client change. The new client
reads first, creates only after the typed 404, validates returned group identity,
status and required private permissions, and propagates create failures. If the
create response is lost, that attempt fails; retry reads the committed row.
There is no old-gateway fallback. Roll back the client before the gateway.

No schema migration is required. PostgreSQL/SQLite target ID explicitly. GORM's
MySQL dialect implements its native duplicate-key no-op; groups currently have
only ID uniqueness, and ID readback remains mandatory. This change does not
introduce a global duplicate-error classifier.

Verification: model/controller/middleware targeted tests plus Go build. The model
suite launches eight OS processes against one temporary SQLite database and
checks one persisted group, restart, preservation of state/token, non-PK errors,
and positive/negative cache behavior. The controller suite covers admin auth,
typed absence, first and repeated ensure with differing defaults, malformed
requests, unchanged legacy create, and real DB failure. Local Docker is absent;
PostgreSQL/Redis multi-instance acceptance remains a staging gate, owned by the
reviewing project manager. No staging/production deployment was performed.

Independent-review P2 regression: the original controller accepted 17,002-byte
whitespace-padded JSON and 16,398-byte JSON with a hidden second object, inserting
a group. After the fix the same independent probe returns 413/persisted=0 for
both. Actual authenticated controller tests cover 16,383/16,384/16,385-byte
requests, hidden/short trailing JSON, trailing JSON exactly at the boundary and
oversized strings, with both known and unknown Content-Length. Rejected cases
assert no row for the requested ID and no change in total group count.
