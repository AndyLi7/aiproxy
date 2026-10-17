-- Isolated reviewed draft only. Never automatically installed by handlers.
-- Requires channel-execution-v1. No use closure or historical backfill.
BEGIN;
CREATE TABLE IF NOT EXISTS channel_admission_measured_producers (
  id TEXT PRIMARY KEY,
  execution_id TEXT NOT NULL,
  execution_auth TEXT NOT NULL,
  use_id TEXT NOT NULL,
  use_hash TEXT NOT NULL,
  storage_generation TEXT NOT NULL,
  scope_json TEXT NOT NULL,
  price_json TEXT NOT NULL,
  request_at_ms BIGINT NOT NULL,
  disable_fuzzy BOOLEAN NOT NULL,
  authorization_hash TEXT NOT NULL,
  state TEXT NOT NULL,
  started_at_ms BIGINT NOT NULL,
  returned_at_ms BIGINT NOT NULL,
  handoff_kind TEXT NOT NULL,
  usage_id INTEGER NOT NULL,
  log_id INTEGER NOT NULL,
  usage_hash TEXT NOT NULL,
  frozen_hash TEXT NOT NULL,
  log_identity_hash TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS channel_admission_measured_producers_execution_id ON channel_admission_measured_producers (execution_id);
CREATE UNIQUE INDEX IF NOT EXISTS channel_admission_measured_producers_use_id ON channel_admission_measured_producers (use_id);
CREATE INDEX IF NOT EXISTS channel_admission_measured_producers_usage_id ON channel_admission_measured_producers (usage_id);
CREATE TABLE IF NOT EXISTS channel_admission_measured_claims (
  id TEXT PRIMARY KEY,
  source_id TEXT NOT NULL,
  usage_id INTEGER NOT NULL,
  owner_hash TEXT NOT NULL,
  until_micros BIGINT NOT NULL,
  epoch TEXT NOT NULL,
  storage_generation TEXT NOT NULL,
  service_id TEXT NOT NULL,
  instance_id TEXT NOT NULL,
  build_sha TEXT NOT NULL,
  proof_json TEXT NOT NULL,
  authorization_hash TEXT NOT NULL,
  state TEXT NOT NULL,
  offered_at_ms BIGINT NOT NULL,
  accepted_at_ms BIGINT NOT NULL,
  exited_at_ms BIGINT NOT NULL,
  release_json TEXT NOT NULL,
  release_hash TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS channel_admission_measured_claims_source_id ON channel_admission_measured_claims (source_id);
CREATE INDEX IF NOT EXISTS channel_admission_measured_claims_usage_id ON channel_admission_measured_claims (usage_id);
INSERT INTO channel_admission_execution_protocols (id, version) VALUES (9, 1) ON CONFLICT (id) DO NOTHING;
COMMIT;
