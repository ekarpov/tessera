-- A server-scoped receipt belongs to no dataset, so nothing about a dataset can
-- say how long it is kept. Until now such a receipt fell into the sweep for
-- receipts whose dataset is gone and was pruned by the storage's *dataset*
-- window, while discovery advertised the server's — a server promising a week
-- and deleting after a day, which is worse than promising nothing.
--
-- The deadline is written when the key is claimed, so a later configuration
-- change applies to new claims and cannot shorten a promise already made.
ALTER TABLE "tessera_receipts" ADD COLUMN "retain_until" INTEGER;
CREATE INDEX "tessera_receipts_retention_deadline" ON "tessera_receipts" ("retain_until");
PRAGMA user_version = 11;
