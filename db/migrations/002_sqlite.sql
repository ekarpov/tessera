ALTER TABLE "tessera_access" ADD COLUMN "generation" TEXT NOT NULL DEFAULT '';
UPDATE "tessera_access" SET "generation"=lower(hex(randomblob(32)));
CREATE TABLE "tessera_sequences" (
 "id" TEXT PRIMARY KEY,
 "dataset_id" TEXT NOT NULL,
 "principal" TEXT NOT NULL,
 "authorization" TEXT NOT NULL,
 "generation" TEXT NOT NULL,
 "expires_at" INTEGER NOT NULL,
 "query" BLOB NOT NULL
);
CREATE TABLE "tessera_pages" (
 "cursor" TEXT PRIMARY KEY,
 "sequence_id" TEXT NOT NULL REFERENCES "tessera_sequences"("id") ON DELETE CASCADE,
 "body" BLOB NOT NULL
);
CREATE INDEX "tessera_pages_sequence" ON "tessera_pages"("sequence_id");
PRAGMA user_version = 2;
