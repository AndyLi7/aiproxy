-- REVIEW ONLY: additive ownership draft; requires channel-execution-v1.
-- No executor/exit evidence, historical backfill or production application.
BEGIN;
CREATE TABLE IF NOT EXISTS channel_admission_image_recovery_claims (
 id VARCHAR(64) PRIMARY KEY,
 version INTEGER NOT NULL,
 owner VARCHAR(64) NOT NULL,
 until_micros BIGINT NOT NULL,
 storage_generation VARCHAR(128) NOT NULL,
 use_id VARCHAR(64) NOT NULL,
 task_hash VARCHAR(64) NOT NULL
);
INSERT INTO channel_admission_execution_protocols(id,version) VALUES(5,1) ON CONFLICT(id) DO NOTHING;
COMMIT;
