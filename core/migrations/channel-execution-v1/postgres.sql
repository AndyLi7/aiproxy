-- REVIEW REQUIRED: additive execution journal only, never applied automatically.
-- Apply only after the admission-v1 migration in the same physical DB/LogDB.
-- This does not certify whole-worker coverage, close uses or activate C.
BEGIN;
CREATE TABLE IF NOT EXISTS channel_admission_execution_protocols (
 id INTEGER PRIMARY KEY,
 version INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS channel_admission_executions (
 id VARCHAR(64) PRIMARY KEY,
 use_id VARCHAR(64) NOT NULL UNIQUE,
 version INTEGER NOT NULL,
 kind VARCHAR(32) NOT NULL,
 use_hash VARCHAR(64) NOT NULL,
 epoch VARCHAR(128) NOT NULL,
 storage_generation VARCHAR(128) NOT NULL,
 service_id VARCHAR(128) NOT NULL,
 instance_id VARCHAR(128) NOT NULL,
 build_sha VARCHAR(64) NOT NULL,
 authorization_hash VARCHAR(64) NOT NULL,
 state VARCHAR(32) NOT NULL,
 continuation_state VARCHAR(32) NOT NULL,
 started_at_ms BIGINT NOT NULL,
 returned_at_ms BIGINT NOT NULL
);
INSERT INTO channel_admission_execution_protocols (id, version) VALUES (1, 1) ON CONFLICT (id) DO NOTHING;
COMMIT;
