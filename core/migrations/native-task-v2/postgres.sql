-- REVIEW REQUIRED. Apply only to the verified gateway LogDB, schema public,
-- BEFORE deploying a binary whose NativeTask has public_error. Read-only
-- preflight must confirm current_database(), current_schema() and
-- public.native_tasks. Do not apply to the application database.
-- Additive and metadata-only (nullable, no default): keep it on rollback.
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
DO $$ BEGIN
 IF current_schema() <> 'public' OR to_regclass('public.native_tasks') IS NULL THEN
  RAISE EXCEPTION 'Unexpected gateway LogDB schema';
 END IF;
END $$;
ALTER TABLE public.native_tasks ADD COLUMN IF NOT EXISTS public_error text;
COMMIT;
