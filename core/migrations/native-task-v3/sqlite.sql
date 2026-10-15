-- REVIEW REQUIRED. Own task queue (spec 2026-10-15 §8.1). Apply to the gateway
-- LOG database BEFORE deploying a binary whose NativeTask has queue_scope.
-- Additive: keep the columns on rollback. Each ADD COLUMN fails, and changes
-- nothing, if the column exists. Run the statements in one transaction.
ALTER TABLE `native_tasks` ADD COLUMN `queue_scope` text DEFAULT '';
ALTER TABLE `native_tasks` ADD COLUMN `enqueued_at` datetime;
ALTER TABLE `native_tasks` ADD COLUMN `dispatched_at` datetime;
ALTER TABLE `native_tasks` ADD COLUMN `dispatch_owner` text DEFAULT '';
ALTER TABLE `native_tasks` ADD COLUMN `dispatch_until` datetime;
ALTER TABLE `native_tasks` ADD COLUMN `dispatch_after` datetime;
CREATE INDEX IF NOT EXISTS `idx_native_tasks_status_queue_scope` ON `native_tasks`(`status`,`queue_scope`);
CREATE INDEX IF NOT EXISTS `idx_native_tasks_group_created` ON `native_tasks`(`group_id`,`created_at`);
