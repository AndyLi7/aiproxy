-- REVIEW REQUIRED. Apply only to the verified gateway LogDB, schema public.
-- Read-only preflight must confirm current_database(), current_schema() and
-- public.image_tasks. Do not apply to the application database.
-- Additive migration: retain these columns if rolling back gateway binaries.
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';
DO $$ BEGIN
 IF current_schema() <> 'public' OR to_regclass('public.image_tasks') IS NULL THEN
  RAISE EXCEPTION 'Unexpected gateway LogDB schema';
 END IF;
END $$;
ALTER TABLE public.image_tasks
 ADD COLUMN IF NOT EXISTS description text,
 ADD COLUMN IF NOT EXISTS prompt text,
 ADD COLUMN IF NOT EXISTS seed bigint,
 ADD COLUMN IF NOT EXISTS billable_units text,
 ADD COLUMN IF NOT EXISTS billing text,
 ADD COLUMN IF NOT EXISTS billing_settled boolean NOT NULL DEFAULT false,
 ADD COLUMN IF NOT EXISTS billing_next_check_at timestamptz,
 ADD COLUMN IF NOT EXISTS prepayment_quote_json text,
 ADD COLUMN IF NOT EXISTS billing_operation_id varchar(128);
COMMIT;
-- Outside a transaction: avoid blocking writes while building the recovery index.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_image_tasks_billing_next_check_at
 ON public.image_tasks (billing_next_check_at);
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_image_tasks_billing_operation_id
 ON public.image_tasks (billing_operation_id)
 WHERE billing_operation_id IS NOT NULL AND billing_operation_id <> '';
