-- REVIEW ONLY: requires channel-execution-v1; optional protocol 8/version 1. No backfill or runtime migration.
BEGIN;
CREATE TABLE IF NOT EXISTS channel_admission_image_producers (
 id VARCHAR(128) PRIMARY KEY,
 parent_id VARCHAR(128) NOT NULL,
 version INTEGER NOT NULL,
 kind VARCHAR(32) NOT NULL,
 use_id VARCHAR(128) NOT NULL,
 use_hash VARCHAR(128) NOT NULL,
 task_id VARCHAR(128) NOT NULL,
 usage_id INTEGER NOT NULL,
 core_hash VARCHAR(128) NOT NULL,
 attempt_index INTEGER NOT NULL,
 epoch VARCHAR(128) NOT NULL,
 storage_generation VARCHAR(128) NOT NULL,
 service_id VARCHAR(128) NOT NULL,
 instance_id VARCHAR(128) NOT NULL,
 build_sha VARCHAR(128) NOT NULL,
 authorization_hash VARCHAR(128) NOT NULL,
 state VARCHAR(32) NOT NULL,
 continuation_state VARCHAR(32) NOT NULL,
 started_at_ms BIGINT NOT NULL,
 returned_at_ms BIGINT NOT NULL,
 receipt_json TEXT NOT NULL,
 receipt_hash VARCHAR(128) NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_image_producer_use ON channel_admission_image_producers(use_id);
CREATE INDEX IF NOT EXISTS idx_image_producer_parent ON channel_admission_image_producers(parent_id);
INSERT INTO channel_admission_execution_protocols(id,version) VALUES(8,1) ON CONFLICT(id) DO NOTHING;
COMMIT;
