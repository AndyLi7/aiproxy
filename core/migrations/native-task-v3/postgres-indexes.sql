-- REVIEW REQUIRED. Own task queue (spec 2026-10-15 §8.1), step 2 of 2: the
-- indexes, built without blocking reads or writes. Run after postgres.sql, on
-- the same gateway LogDB, before NativeQueueEnabled is turned on (the binary
-- runs without them, and they keep each queue decision cheap). CREATE INDEX
-- CONCURRENTLY cannot run inside a transaction block: run each statement on
-- its own (psql does), never wrapped in BEGIN/COMMIT. If one fails it leaves
-- an INVALID index: DROP INDEX CONCURRENTLY it and run the statement again.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_native_tasks_status_queue_scope ON public.native_tasks (status, queue_scope);
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_native_tasks_group_created ON public.native_tasks (group_id, created_at);
