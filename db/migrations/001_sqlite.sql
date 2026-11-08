CREATE TABLE "tessera_dataset" (
 "singleton" INTEGER PRIMARY KEY CHECK ("singleton" = 1),
 "dataset_id" TEXT NOT NULL UNIQUE,
 "secret" BLOB NOT NULL CHECK (length("secret") = 32),
 "next_id" TEXT NOT NULL,
 "watermark" TEXT NOT NULL,
 "limits" BLOB NOT NULL
);
CREATE TABLE "tessera_types" (
 "type_id" TEXT PRIMARY KEY,
 "body" BLOB NOT NULL
);
CREATE TABLE "tessera_objects" (
 "object_id" TEXT PRIMARY KEY,
 "type_id" TEXT NOT NULL REFERENCES "tessera_types"("type_id") DEFERRABLE INITIALLY DEFERRED,
 "fields" BLOB NOT NULL
);
CREATE INDEX "tessera_objects_type" ON "tessera_objects" ("type_id");
CREATE TABLE "tessera_access" (
 "principal" TEXT NOT NULL,
 "authorization" TEXT NOT NULL,
 "grants" BLOB NOT NULL,
 PRIMARY KEY ("principal", "authorization")
);
CREATE TABLE "tessera_receipts" (
 "key" TEXT PRIMARY KEY,
 "dataset_id" TEXT NOT NULL,
 "principal" TEXT NOT NULL,
 "authorization" TEXT NOT NULL,
 "digest" BLOB NOT NULL CHECK (length("digest") = 32),
 "claimed_at" INTEGER NOT NULL,
 "state" TEXT NOT NULL CHECK ("state" IN ('in_progress','committed','failed')),
 "status" INTEGER,
 "body" BLOB,
 "objects" BLOB,
 CHECK (("state" = 'in_progress' AND "status" IS NULL AND "body" IS NULL) OR
        ("state" <> 'in_progress' AND "status" IS NOT NULL AND "body" IS NOT NULL))
);
CREATE INDEX "tessera_receipts_retention" ON "tessera_receipts" ("state", "claimed_at");
PRAGMA user_version = 1;
