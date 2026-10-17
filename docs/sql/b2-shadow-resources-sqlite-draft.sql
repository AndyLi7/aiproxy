-- ISOLATED REVIEW DRAFT ONLY. Never added to runtime/AutoMigrate.
-- Requires reviewed admission + prepared/probe schemas and same DB/LogDB.
-- Installing this resource schema enables no issuer, probe, seal or migration.
-- Production SQL requires independent user review and a verified rollout plan.
BEGIN;
CREATE TABLE channel_migration_shadow_protocols (id INTEGER PRIMARY KEY, version INTEGER NOT NULL);
CREATE TABLE channel_migration_shadow_resources (
 operation_id VARCHAR(128) PRIMARY KEY,
 version INTEGER NOT NULL,
 request_json TEXT NOT NULL,
 request_hash VARCHAR(64) NOT NULL,
 source_snapshot_hash VARCHAR(64) NOT NULL,
 source_family_hash VARCHAR(64) NOT NULL,
 source_epoch VARCHAR(128) NOT NULL,
 source_revision BIGINT NOT NULL,
 storage_generation VARCHAR(128) NOT NULL,
 from_channel_id BIGINT NOT NULL,
 shadow_channel_id BIGINT NOT NULL UNIQUE,
 shadow_key_digest VARCHAR(64) NOT NULL,
 shadow_identity_hash VARCHAR(64) NOT NULL,
 shadow_projection_hash VARCHAR(64) NOT NULL,
 created_revision BIGINT NOT NULL,
 created_at_ms BIGINT NOT NULL,
 checkpoint_hash VARCHAR(64) NOT NULL
);
CREATE INDEX idx_channel_migration_shadow_resources_from ON channel_migration_shadow_resources(from_channel_id);
INSERT INTO channel_migration_shadow_protocols(id,version) VALUES (1,1);
COMMIT;
