-- REVIEW REQUIRED. Own task queue (spec 2026-10-15 §8.1). Apply to the gateway
-- LOG database BEFORE deploying a binary whose NativeTask has queue_scope.
-- MySQL DDL commits implicitly and has no ADD COLUMN IF NOT EXISTS: inspect
-- the table first and record each statement. Keep the columns on rollback.
ALTER TABLE `native_tasks` ADD COLUMN `queue_scope` varchar(128) DEFAULT '', ADD COLUMN `enqueued_at` datetime(3) NULL, ADD COLUMN `dispatched_at` datetime(3) NULL, ADD COLUMN `dispatch_owner` varchar(64) DEFAULT '', ADD COLUMN `dispatch_until` datetime(3) NULL, ADD COLUMN `dispatch_after` datetime(3) NULL;
CREATE INDEX `idx_native_tasks_status_queue_scope` ON `native_tasks` (`status`,`queue_scope`);
