-- REVIEW REQUIRED. PostgreSQL; execute only after the compatible wallet is deployed.
-- Target: the gateway LogDB (may differ from its primary DB).
-- Additive only. Do NOT backfill old rows or assign new operation IDs to old tasks.
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
ALTER TABLE public.async_usage_infos
  ADD COLUMN IF NOT EXISTS billing_operation_id varchar(128);
ALTER TABLE public.consume_errors
  ADD COLUMN IF NOT EXISTS billing_operation_id varchar(128);
-- Fail instead of accepting an incompatible pre-existing column.
DO $$
BEGIN
  IF (SELECT count(*) FROM information_schema.columns
      WHERE table_schema = 'public'
        AND table_name IN ('async_usage_infos', 'consume_errors')
        AND column_name = 'billing_operation_id'
        AND data_type = 'character varying'
        AND character_maximum_length = 128
        AND is_nullable = 'YES'
        AND column_default IS NULL) <> 2 THEN
    RAISE EXCEPTION 'billing_operation_id schema verification failed';
  END IF;
END $$;
COMMIT;
-- Gateway rollback: retain both nullable columns; do NOT DROP while tasks may exist.
