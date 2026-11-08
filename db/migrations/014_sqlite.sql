-- An aborted import's staged bytes are removed once, and the import is marked
-- so. The row stays: it is the tombstone that answers a replay of the same
-- import. Without the mark every maintenance pass removed the same files and
-- rewrote the same rows again, for every import ever aborted, holding the
-- transfer lock every other upload and download waits on.
ALTER TABLE "tessera_imports" ADD COLUMN "cleaned" INTEGER NOT NULL DEFAULT 0;
CREATE INDEX "tessera_imports_uncleaned" ON "tessera_imports" ("state","cleaned");
PRAGMA user_version = 14;
