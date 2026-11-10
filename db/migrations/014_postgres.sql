-- Pin the creator derivation key when a restore first rotates the version key.
-- A null key means the dataset still uses its original version key.
ALTER TABLE "tessera_dataset" ADD COLUMN "creator_secret" BYTEA CHECK (length("creator_secret") = 32);
UPDATE "tessera_migration" SET "version"=14;
