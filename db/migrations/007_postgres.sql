-- Startup resolved every in-progress claim as abandoned, which is only safe
-- while one process can write to the database. It was made safe by a session
-- advisory lock on a constant: a second process could not open the database at
-- all. That lock is the last thing pinning a dataset to one executor.
--
-- A claim now records which executor made it. A claim is abandoned when its
-- executor is gone, which is answered by trying to take that executor's own
-- session lock — not by a clock, because a slow executor and a dead one look
-- the same to a clock.
--
-- Existing claims record no executor. The empty string means exactly that, and
-- they are resolved on the next startup, which first proves no process of the
-- previous kind is running by taking the lock such a process would hold.
ALTER TABLE "tessera_receipts" ADD COLUMN "executor_id" TEXT NOT NULL DEFAULT '';
CREATE INDEX "tessera_receipts_executor" ON "tessera_receipts" ("state","executor_id");
UPDATE "tessera_migration" SET "version"=7;
