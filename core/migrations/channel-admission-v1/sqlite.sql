-- REVIEW REQUIRED: additive draft only. No production application is authorized.
-- Same physical DB/LogDB only. Empty epoch intentionally leaves C inactive.
BEGIN;
CREATE TABLE IF NOT EXISTS channel_admission_controls (
  id INTEGER PRIMARY KEY,
  version INTEGER NOT NULL,
  epoch VARCHAR(128) NOT NULL DEFAULT '',
  revision BIGINT NOT NULL DEFAULT 0,
  epoch_sequence BIGINT NOT NULL DEFAULT 0,
  storage_generation VARCHAR(128) NOT NULL DEFAULT '',
  membership_hash VARCHAR(64) NOT NULL DEFAULT '',
  executor_fence_hash VARCHAR(64) NOT NULL DEFAULT '',
  active_proof_id VARCHAR(128) NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS channel_admission_proofs (
  id VARCHAR(128) PRIMARY KEY,
  fact_hash VARCHAR(64) NOT NULL,
  revoked BOOLEAN NOT NULL DEFAULT FALSE,
  consumed_sequence BIGINT NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS channel_admission_families (
  key_digest VARCHAR(64) PRIMARY KEY,
  allowed_channel_id BIGINT NOT NULL,
  allowed_identity_hash VARCHAR(64) NOT NULL,
  revision BIGINT NOT NULL,
  operation_id VARCHAR(128) NOT NULL
);
CREATE TABLE IF NOT EXISTS channel_admission_barriers (
  operation_id VARCHAR(128) PRIMARY KEY,
  request_hash VARCHAR(64) NOT NULL,
  epoch VARCHAR(128) NOT NULL,
  version INTEGER NOT NULL,
  revision BIGINT NOT NULL,
  from_channel_id BIGINT NOT NULL,
  from_key_digest VARCHAR(64) NOT NULL,
  from_identity_hash VARCHAR(64) NOT NULL,
  to_channel_id BIGINT NOT NULL,
  to_key_digest VARCHAR(64) NOT NULL,
  to_identity_hash VARCHAR(64) NOT NULL,
  recorded_at_ms BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS channel_admission_uses (
  resource_key VARCHAR(64) NOT NULL DEFAULT '',
  id VARCHAR(64) PRIMARY KEY,
  request_hash VARCHAR(64) NOT NULL,
  epoch VARCHAR(128) NOT NULL,
  channel_id BIGINT NOT NULL,
  key_digest VARCHAR(64) NOT NULL,
  identity_hash VARCHAR(64) NOT NULL,
  lane VARCHAR(32) NOT NULL,
  state VARCHAR(16) NOT NULL,
  admitted_at_ms BIGINT NOT NULL,
  closed_at_ms BIGINT NOT NULL DEFAULT 0,
  closure_hash VARCHAR(64) NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_channel_admission_barriers_from_channel_id ON channel_admission_barriers (from_channel_id);
CREATE INDEX IF NOT EXISTS idx_channel_admission_uses_channel_id ON channel_admission_uses (channel_id);
CREATE INDEX IF NOT EXISTS idx_channel_admission_uses_key_digest ON channel_admission_uses (key_digest);
CREATE INDEX IF NOT EXISTS idx_channel_admission_uses_resource_key ON channel_admission_uses (resource_key);
CREATE INDEX IF NOT EXISTS idx_channel_admission_uses_state ON channel_admission_uses (state);
INSERT INTO channel_admission_controls (id, version, epoch, revision) VALUES (1, 1, '', 0) ON CONFLICT (id) DO NOTHING;
COMMIT;
