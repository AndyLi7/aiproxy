-- REVIEW REQUIRED. Gateway LogDB SQLite (development). Inspect PRAGMA table_info(logs) and
-- PRAGMA table_info(async_usage_infos) first; skip any ALTER whose column exists.
ALTER TABLE logs ADD COLUMN billing_operation_id varchar(200);
ALTER TABLE async_usage_infos ADD COLUMN usage_model_id varchar(256);
ALTER TABLE async_usage_infos ADD COLUMN usage_source varchar(16);
ALTER TABLE async_usage_infos ADD COLUMN usage_at_ms bigint;
CREATE INDEX IF NOT EXISTS idx_logs_group_reqat ON logs (group_id, request_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_logs_group_token_reqat ON logs (group_id, token_id, request_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_logs_group_billing_op ON logs (group_id, billing_operation_id)
  WHERE billing_operation_id IS NOT NULL;
