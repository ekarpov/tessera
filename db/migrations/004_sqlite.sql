ALTER TABLE "tessera_access" ADD COLUMN "view" BLOB NOT NULL DEFAULT '{"full":true}';
PRAGMA user_version = 4;
