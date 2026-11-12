-- Keep the creator key across rotations of the dataset version key.
ALTER TABLE "tessera_dataset" ADD COLUMN "creator_secret" BLOB CHECK (length("creator_secret") = 32);
PRAGMA user_version = 19;
