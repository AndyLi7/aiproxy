-- REVIEW REQUIRED. Apply to the gateway LOG database BEFORE deploying a binary
-- whose NativeTask has public_error. Additive: keep the column on rollback.
ALTER TABLE `native_tasks` ADD COLUMN `public_error` text;
