-- Review required before production application. Additive only; no old tasks
-- are declared archived, no phase history is fabricated, no data is deleted.
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
ALTER TABLE image_tasks
 ADD COLUMN IF NOT EXISTS archive_required boolean NOT NULL DEFAULT false,
 ADD COLUMN IF NOT EXISTS queued_at timestamptz,
 ADD COLUMN IF NOT EXISTS running_at timestamptz,
 ADD COLUMN IF NOT EXISTS result_received_at timestamptz,
 ADD COLUMN IF NOT EXISTS completed_at timestamptz,
 ADD COLUMN IF NOT EXISTS result_expires_at timestamptz,
 ADD COLUMN IF NOT EXISTS retain_until timestamptz;
COMMIT;
-- Rollback: deploy the previous binary; retain these unused additive columns.
