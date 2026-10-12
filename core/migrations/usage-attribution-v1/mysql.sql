-- REVIEW REQUIRED. Run both SELECTs first; execute only statements whose column or index is missing.
SELECT table_name, column_name FROM information_schema.columns
 WHERE table_schema = DATABASE() AND ((table_name = 'logs' AND column_name = 'billing_operation_id')
   OR (table_name = 'async_usage_infos' AND column_name IN ('usage_model_id','usage_source','usage_at_ms')));
SELECT DISTINCT index_name FROM information_schema.statistics
 WHERE table_schema = DATABASE() AND table_name = 'logs'
   AND index_name IN ('idx_logs_group_reqat','idx_logs_group_token_reqat','idx_logs_group_billing_op');
ALTER TABLE `logs` ADD COLUMN `billing_operation_id` varchar(200) NULL;
ALTER TABLE `async_usage_infos` ADD COLUMN `usage_model_id` varchar(256) NULL, ADD COLUMN `usage_source` varchar(16) NULL, ADD COLUMN `usage_at_ms` bigint NULL;
CREATE INDEX `idx_logs_group_reqat` ON `logs` (`group_id`, `request_at` DESC, `id` DESC) LOCK=NONE;
CREATE INDEX `idx_logs_group_token_reqat` ON `logs` (`group_id`, `token_id`, `request_at` DESC, `id` DESC) LOCK=NONE;
CREATE INDEX `idx_logs_group_billing_op` ON `logs` (`group_id`, `billing_operation_id`) LOCK=NONE;
