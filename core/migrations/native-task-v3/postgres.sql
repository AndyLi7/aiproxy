-- REVIEW REQUIRED. Own task queue (spec 2026-10-15 §8.1), step 1 of 2: the
-- columns. Apply only to the verified gateway LogDB, schema public, BEFORE
-- deploying a binary whose NativeTask has queue_scope. Read-only preflight
-- must confirm current_database(), current_schema() and public.native_tasks.
-- Do not apply to the application database. Six nullable columns, two with a
-- constant default: metadata-only since PostgreSQL 11, no table rewrite and
-- no existing row changed. ALTER TABLE takes an ACCESS EXCLUSIVE lock that is
-- held until COMMIT (milliseconds here) and waits behind running queries:
-- lock_timeout ends the attempt instead of queueing reads behind it. The
-- indexes are step 2 (postgres-indexes.sql). Keep the columns on rollback.
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';
DO $$ BEGIN
 IF current_schema() <> 'public' OR to_regclass('public.native_tasks') IS NULL THEN
  RAISE EXCEPTION 'Unexpected gateway LogDB schema';
 END IF;
END $$;
ALTER TABLE public.native_tasks ADD COLUMN IF NOT EXISTS queue_scope varchar(128) DEFAULT '';
ALTER TABLE public.native_tasks ADD COLUMN IF NOT EXISTS enqueued_at timestamptz;
ALTER TABLE public.native_tasks ADD COLUMN IF NOT EXISTS dispatched_at timestamptz;
ALTER TABLE public.native_tasks ADD COLUMN IF NOT EXISTS dispatch_owner varchar(64) DEFAULT '';
ALTER TABLE public.native_tasks ADD COLUMN IF NOT EXISTS dispatch_until timestamptz;
ALTER TABLE public.native_tasks ADD COLUMN IF NOT EXISTS dispatch_after timestamptz;
COMMIT;
