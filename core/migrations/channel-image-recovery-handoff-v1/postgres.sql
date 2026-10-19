-- REVIEW ONLY: requires channel-image-recovery-claim-v1. No backfill.
BEGIN;
CREATE TABLE IF NOT EXISTS channel_admission_image_recovery_handoffs (
 id VARCHAR(128) PRIMARY KEY,
 version INTEGER NOT NULL,
 claim_hash VARCHAR(128) NOT NULL,
 use_id VARCHAR(128) NOT NULL,
 use_hash VARCHAR(128) NOT NULL,
 core_hash VARCHAR(128) NOT NULL,
 task_hash VARCHAR(128) NOT NULL,
 contract_hash VARCHAR(128) NOT NULL,
 claim_id VARCHAR(128) NOT NULL,
 claim_owner VARCHAR(128) NOT NULL,
 claim_until_micros BIGINT NOT NULL,
 epoch VARCHAR(128) NOT NULL,
 storage_generation VARCHAR(128) NOT NULL,
 service_id VARCHAR(128) NOT NULL,
 instance_id VARCHAR(128) NOT NULL,
 build_sha VARCHAR(128) NOT NULL,
 executor_proof_json TEXT NOT NULL,
 use_facts_json TEXT NOT NULL,
 authorization_hash VARCHAR(128) NOT NULL,
 state VARCHAR(128) NOT NULL,
 execution_state VARCHAR(128) NOT NULL,
 continuation_state VARCHAR(128) NOT NULL,
 offered_at_ms BIGINT NOT NULL,
 accepted_at_ms BIGINT NOT NULL,
 exited_at_ms BIGINT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_image_recovery_handoff_claim ON channel_admission_image_recovery_handoffs(claim_hash);
CREATE INDEX IF NOT EXISTS idx_image_recovery_handoff_use ON channel_admission_image_recovery_handoffs(use_id);
INSERT INTO channel_admission_execution_protocols(id,version) VALUES(6,1) ON CONFLICT(id) DO NOTHING;
COMMIT;
