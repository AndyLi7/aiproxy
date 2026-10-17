-- REVIEW ONLY. Requires channel-admission-v1. No backfill, activation, or proof installation.
-- Commands are unavailable without a separately verified live issuer/probe adapter.
BEGIN;
CREATE TABLE IF NOT EXISTS channel_migration_protocols (
 id INTEGER PRIMARY KEY, version INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS channel_migration_probe_records (
 evidence_hash VARCHAR(64) PRIMARY KEY,
 fact_hash VARCHAR(64) NOT NULL,
 revoked BOOLEAN NOT NULL
);
CREATE TABLE IF NOT EXISTS channel_migration_prepareds (
 operation_id VARCHAR(128) PRIMARY KEY,
 request_json TEXT NOT NULL,
 request_hash VARCHAR(64) NOT NULL,
 storage_generation VARCHAR(128) NOT NULL,
 probe_hash VARCHAR(64) NOT NULL,
 probe_fact_hash VARCHAR(64) NOT NULL,
 state VARCHAR(16) NOT NULL,
 seal_revision BIGINT NOT NULL,
 commit_revision BIGINT NOT NULL,
 committed_at_ms BIGINT NOT NULL,
 after_pin_hash VARCHAR(64) NOT NULL,
 from_channel_id INTEGER NOT NULL,
 to_channel_id INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_channel_migration_prepared_from ON channel_migration_prepareds(from_channel_id);
CREATE INDEX IF NOT EXISTS idx_channel_migration_prepared_to ON channel_migration_prepareds(to_channel_id);
INSERT INTO channel_migration_protocols(id,version) VALUES(1,1) ON CONFLICT(id) DO NOTHING;
COMMIT;
