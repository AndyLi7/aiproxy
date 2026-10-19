-- ISOLATED REVIEW DRAFT ONLY. No runtime migration. Upgrade complete v1 only.
-- Existing resources remain unknown: do NOT infer created/consumed/cancelled.
-- Coordinate binary rollout. v1/partial fail closed in the new commands.
BEGIN;
ALTER TABLE channel_migration_shadow_resources ADD COLUMN lifecycle_state VARCHAR(16) NOT NULL DEFAULT 'unknown';
ALTER TABLE channel_migration_shadow_resources ADD COLUMN lifecycle_revision BIGINT NOT NULL DEFAULT 0;
ALTER TABLE channel_migration_shadow_resources ADD COLUMN prepared_request_hash VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE channel_migration_shadow_resources ADD COLUMN prepared_probe_hash VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE channel_migration_shadow_resources ADD COLUMN prepared_probe_fact_hash VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE channel_migration_shadow_resources ADD COLUMN prepared_at_ms BIGINT NOT NULL DEFAULT 0;
ALTER TABLE channel_migration_shadow_resources ADD COLUMN cancelled_at_ms BIGINT NOT NULL DEFAULT 0;
ALTER TABLE channel_migration_prepareds ADD COLUMN shadow_checkpoint_hash VARCHAR(64) NOT NULL DEFAULT '';
UPDATE channel_migration_shadow_protocols SET version=2 WHERE id=1 AND version=1;
COMMIT;
