-- A resolved read URI now records which representation it was issued for, so
-- the read side produces exactly what resolve authorized. Thumbnails are
-- generated on demand and never stored, so there is no derivative table to
-- keep, and nothing here for an erase or a restore to miss.
ALTER TABLE "tessera_transfer_tokens" ADD COLUMN "representation" TEXT NOT NULL DEFAULT '';
PRAGMA user_version = 7;
