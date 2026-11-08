-- The executor that made a claim, recorded for the same reason PostgreSQL
-- records it, and unused here. SQLite excludes other processes with an
-- exclusive lock on a file beside the database, so a starting process has
-- already proved that no other executor is alive before it reads a claim.
-- Recording it anyway keeps one shape for both engines and keeps the column
-- available if SQLite ever gets a cross-process answer.
ALTER TABLE "tessera_receipts" ADD COLUMN "executor_id" TEXT NOT NULL DEFAULT '';
CREATE INDEX "tessera_receipts_executor" ON "tessera_receipts" ("state","executor_id");

PRAGMA user_version = 10;
