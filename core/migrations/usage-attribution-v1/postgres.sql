-- REVIEW REQUIRED. Gateway LogDB only (LOG_SQL_DSN, else the main gateway DB), schema public.
-- Never the application database. Apply BEFORE the gateway binary that declares
-- logs.billing_operation_id and async_usage_infos.usage_model_id/usage_source/usage_at_ms
-- (that binary reports /api/status 503 until all four exist).
-- Additive, nullable, no default, metadata-only: keep on rollback.
-- Apply at low traffic: the running gateway's cached SELECT * statements on logs and
-- async_usage_infos fail with SQLSTATE 0A000 for up to SQL_MAX_LIFETIME seconds after
-- step 1 (README, "Expected errors right after step 1").
-- Run: psql -v ON_ERROR_STOP=1 -f postgres.sql   (no -1: step 2 cannot run in a transaction)

-- Step 1: one short transaction. On lock_timeout wait 30 s and re-run (idempotent); stop after 10 tries.
BEGIN;
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
DO $$ BEGIN
 IF current_schema() <> 'public' OR to_regclass('public.logs') IS NULL
    OR to_regclass('public.async_usage_infos') IS NULL THEN
  RAISE EXCEPTION 'Unexpected gateway LogDB schema';
 END IF;
 IF to_regclass('public.wallet_ledger') IS NOT NULL THEN
  RAISE EXCEPTION 'This is the application database, not the gateway LogDB';
 END IF;
END $$;
ALTER TABLE public.logs ADD COLUMN IF NOT EXISTS billing_operation_id varchar(200);
ALTER TABLE public.async_usage_infos ADD COLUMN IF NOT EXISTS usage_model_id varchar(256);
ALTER TABLE public.async_usage_infos ADD COLUMN IF NOT EXISTS usage_source varchar(16);
ALTER TABLE public.async_usage_infos ADD COLUMN IF NOT EXISTS usage_at_ms bigint;
COMMIT;

-- Step 2: outside any transaction; builds without blocking inserts.
SET lock_timeout = 0;
SET statement_timeout = 0;
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_group_reqat
  ON public.logs (group_id, request_at DESC, id DESC);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_group_token_reqat
  ON public.logs (group_id, token_id, request_at DESC, id DESC);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_logs_group_billing_op
  ON public.logs (group_id, billing_operation_id) WHERE billing_operation_id IS NOT NULL;

-- Verify (read-only):
-- SELECT c.relname, i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
--  WHERE i.indrelid = 'public.logs'::regclass
--    AND c.relname IN ('idx_logs_group_reqat','idx_logs_group_token_reqat','idx_logs_group_billing_op');
-- SELECT table_name, column_name, character_maximum_length FROM information_schema.columns
--  WHERE table_schema = 'public' AND ((table_name = 'logs' AND column_name = 'billing_operation_id')
--    OR (table_name = 'async_usage_infos' AND column_name IN ('usage_model_id','usage_source','usage_at_ms')));
-- An invalid index: DROP INDEX CONCURRENTLY IF EXISTS public.<name>; then re-run step 2.
-- A long-running transaction stalls CONCURRENTLY: check pg_stat_activity first.
