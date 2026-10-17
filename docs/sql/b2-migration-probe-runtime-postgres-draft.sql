-- ISOLATED REVIEW DRAFT ONLY. Do not add to runtime AutoMigrate.
-- Requires admission, prepared, shadow lifecycle v2 and native task storage.
-- No issuer/provenance is installed; installing these tables authorizes no work.
-- Production SQL requires separate user review. Same DB/LogDB is mandatory.
BEGIN;
CREATE TABLE channel_migration_probe_runtime_protocols (id INTEGER PRIMARY KEY, version INTEGER NOT NULL);
CREATE TABLE channel_migration_probe_runs (
 operation_id VARCHAR(128) PRIMARY KEY,
 version INTEGER NOT NULL,
 authorization_json TEXT NOT NULL,
 authorization_hash VARCHAR(64) NOT NULL,
 command_hash VARCHAR(64) NOT NULL,
 resource_request_hash VARCHAR(64) NOT NULL,
 checkpoint_hash VARCHAR(64) NOT NULL,
 storage_generation VARCHAR(128) NOT NULL,
 task_id VARCHAR(128) NOT NULL UNIQUE,
 spec_json TEXT NOT NULL,
 spec_hash VARCHAR(64) NOT NULL,
 use_id VARCHAR(64) NOT NULL,
 owner_hash VARCHAR(64) NOT NULL,
 task_hash VARCHAR(64) NOT NULL,
 evidence_json TEXT NOT NULL,
 evidence_task_hash VARCHAR(64) NOT NULL,
 created_at_ms BIGINT NOT NULL,
 bound_at_ms BIGINT NOT NULL
);
INSERT INTO channel_migration_probe_runtime_protocols(id,version) VALUES (1,1);
COMMIT;
