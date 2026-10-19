-- REVIEW REQUIRED. Run once only if the earlier channel-admission-v1 draft
-- was installed without resource_key. Fresh installations use the full v1 SQL.
-- No inferred resource association is backfilled for older use records.
BEGIN;
ALTER TABLE channel_admission_uses ADD COLUMN resource_key VARCHAR(64) NOT NULL DEFAULT '';
CREATE INDEX idx_channel_admission_uses_resource_key ON channel_admission_uses (resource_key);
COMMIT;
