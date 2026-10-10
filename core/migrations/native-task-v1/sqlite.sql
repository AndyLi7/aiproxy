-- Review before applying to the gateway LOG database. Not an automatic migration.
CREATE TABLE `native_tasks` (`recovery_owner` text,`recovery_until` integer,`next_recovery_at` integer,`billing_settled` numeric,`delivery_base` text,`artifact_manifest` text,`delivered_output` text,`billing_receipt_json` text,`frozen_contract` text,`native_input` text,`channel_id` integer,`endpoint` text,`key_fingerprint` text,`credential_scope` text,`prepayment_quote_json` text,`billing_operation_id` text,`error_code` text,`public_error` text,`upstream_id` text,`id` text,`group_id` text NOT NULL,`token_id` integer NOT NULL,`model` text NOT NULL,`fingerprint` text NOT NULL,`output_schema` text NOT NULL,`output_schema_hash` text NOT NULL,`status` text NOT NULL,`native_output` text,`created_at` datetime,`updated_at` datetime,PRIMARY KEY (`id`));
CREATE INDEX `idx_native_tasks_group_id` ON `native_tasks`(`group_id`);
CREATE INDEX `idx_native_tasks_billing_settled` ON `native_tasks`(`billing_settled`);
CREATE INDEX `idx_native_tasks_next_recovery_at` ON `native_tasks`(`next_recovery_at`);
CREATE INDEX `idx_native_tasks_recovery_until` ON `native_tasks`(`recovery_until`);
