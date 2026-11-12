-- Keep the creator key across rotations of the dataset version key.
ALTER TABLE "tessera_dataset" ADD COLUMN "creator_secret" BYTEA CHECK (length("creator_secret") = 32);
UPDATE "tessera_migration" SET "version" = 17;
