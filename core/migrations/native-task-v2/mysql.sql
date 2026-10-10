-- REVIEW REQUIRED. Apply to the gateway LOG database BEFORE deploying a binary
-- whose NativeTask has public_error. MySQL DDL commits implicitly and has no
-- ADD COLUMN IF NOT EXISTS: inspect the table first. Keep it on rollback.
ALTER TABLE `native_tasks` ADD COLUMN `public_error` longtext;
