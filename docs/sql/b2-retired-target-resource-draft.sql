-- ISOLATED REVIEW DRAFT ONLY. No runtime auto migration or production authorization.
-- Narrow optional v1 retired-target resources; repeated targets are intentional.
CREATE TABLE channel_migration_retired_target_resources (
  operation_id VARCHAR(128) PRIMARY KEY,
  version BIGINT NOT NULL,
  request_json TEXT NOT NULL,
  request_hash VARCHAR(64) NOT NULL,
  checkpoint_hash VARCHAR(64) NOT NULL,
  epoch VARCHAR(128) NOT NULL,
  storage_generation VARCHAR(128) NOT NULL,
  from_channel_id BIGINT NOT NULL,
  target_channel_id BIGINT NOT NULL,
  source_revision BIGINT NOT NULL,
  created_revision BIGINT NOT NULL,
  created_at_ms BIGINT NOT NULL,
  retirement_operation_id VARCHAR(128) NOT NULL,
  retirement_revision BIGINT NOT NULL,
  retirement_request_hash VARCHAR(64) NOT NULL,
  lifecycle_state VARCHAR(16) NOT NULL,
  lifecycle_revision BIGINT NOT NULL,
  prepared_request_hash VARCHAR(64) NOT NULL DEFAULT '',
  prepared_probe_hash VARCHAR(64) NOT NULL DEFAULT '',
  prepared_probe_fact_hash VARCHAR(64) NOT NULL DEFAULT '',
  prepared_at_ms BIGINT NOT NULL DEFAULT 0,
  cancelled_at_ms BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX idx_retired_target_from ON channel_migration_retired_target_resources (from_channel_id);
CREATE INDEX idx_retired_target_target ON channel_migration_retired_target_resources (target_channel_id);
