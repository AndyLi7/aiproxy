-- Runner MUST stop on first error and ROLLBACK; PostgreSQL: ON_ERROR_STOP.
-- REVIEW DRAFT ONLY. Stop all old writers and take a verified backup first.
-- Run once on the selected environment ONLY after explicit migration approval.
-- This is not called by application startup or AutoMigrate.
BEGIN IMMEDIATE;
CREATE TABLE channel_routing_states (
 channel_id BIGINT PRIMARY KEY,
 version INTEGER NOT NULL CHECK(version=1),
 revision BIGINT NOT NULL CHECK(revision>=1 AND revision<=9007199254740991)
);
CREATE TABLE channel_routing_receipts (
 request_id VARCHAR(128) PRIMARY KEY,
 channel_id BIGINT NOT NULL,
 request_hash VARCHAR(64) NOT NULL,
 request_json TEXT NOT NULL,
 before_json TEXT NOT NULL,
 after_json TEXT NOT NULL,
 committed_at_ms BIGINT NOT NULL
);
CREATE INDEX idx_channel_routing_receipts_channel_id ON channel_routing_receipts(channel_id);
-- Include retained soft-deleted physical rows. Do not fabricate historical receipts.
INSERT INTO channel_routing_states(channel_id,version,revision) SELECT id,1,1 FROM channels;
COMMIT;
