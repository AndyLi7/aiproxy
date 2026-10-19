-- REVIEW ONLY: additive draft, requires channel-native-execution-v2.
-- No historical backfill and no production application.
BEGIN;
CREATE TABLE IF NOT EXISTS channel_admission_recovery_handoffs (
 id VARCHAR(64) PRIMARY KEY,
 version INTEGER NOT NULL,
 claim_hash VARCHAR(64) NOT NULL,
 execution_hash VARCHAR(64) NOT NULL,
 authorization_hash VARCHAR(64) NOT NULL,
 state VARCHAR(16) NOT NULL,
 offered_at_ms BIGINT NOT NULL,
 accepted_at_ms BIGINT NOT NULL,
 exited_at_ms BIGINT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_channel_admission_recovery_handoffs_claim_hash ON channel_admission_recovery_handoffs(claim_hash);
INSERT INTO channel_admission_execution_protocols(id,version) VALUES(4,1) ON CONFLICT(id) DO NOTHING;
COMMIT;
