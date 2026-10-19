-- REVIEW REQUIRED: additive draft; requires channel-execution-v1 first.
-- No automatic production application or historical execution coverage.
BEGIN;
CREATE TABLE IF NOT EXISTS channel_admission_task_executions (
 id VARCHAR(64) PRIMARY KEY,
 use_id VARCHAR(64) NOT NULL,
 version INTEGER NOT NULL,
 kind VARCHAR(32) NOT NULL,
 use_hash VARCHAR(64) NOT NULL,
 task_hash VARCHAR(64) NOT NULL,
 task_created_at_micros BIGINT NOT NULL,
 epoch VARCHAR(128) NOT NULL,
 storage_generation VARCHAR(128) NOT NULL,
 service_id VARCHAR(128) NOT NULL,
 instance_id VARCHAR(128) NOT NULL,
 build_sha VARCHAR(64) NOT NULL,
 recovery_owner VARCHAR(64) NOT NULL,
 recovery_until BIGINT NOT NULL,
 dispatch_owner VARCHAR(64) NOT NULL,
 dispatch_until_micros BIGINT NOT NULL,
 authorization_hash VARCHAR(64) NOT NULL,
 state VARCHAR(32) NOT NULL,
 continuation_state VARCHAR(32) NOT NULL,
 started_at_ms BIGINT NOT NULL,
 returned_at_ms BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_channel_admission_task_executions_use_id ON channel_admission_task_executions(use_id);
INSERT INTO channel_admission_execution_protocols(id,version) VALUES(2,1) ON CONFLICT(id) DO NOTHING;
COMMIT;
