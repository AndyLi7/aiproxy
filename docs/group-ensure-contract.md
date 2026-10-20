# Group ensure contract

`POST /api/group/:group/ensure` is protected by the existing admin middleware.
It accepts only a JSON object with optional initial `status` (1, 2, 3; default 1)
and `available_sets`. Unknown fields and malformed bodies are rejected. Initial
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
