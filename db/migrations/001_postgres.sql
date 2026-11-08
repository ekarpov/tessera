CREATE TABLE "tessera_dataset" (
 "singleton" INTEGER PRIMARY KEY CHECK ("singleton" = 1),
 "dataset_id" TEXT NOT NULL UNIQUE,
 "secret" BYTEA NOT NULL CHECK (length("secret") = 32),
 "next_id" TEXT NOT NULL,
 "watermark" TEXT NOT NULL,
 "limits" BYTEA NOT NULL,
 "replaced_dataset_id" TEXT
);
CREATE TABLE "tessera_types" (
 "type_id" TEXT PRIMARY KEY,
 "body" BYTEA NOT NULL
);
CREATE TABLE "tessera_objects" (
 "object_id" TEXT PRIMARY KEY,
 "type_id" TEXT NOT NULL REFERENCES "tessera_types"("type_id") DEFERRABLE INITIALLY DEFERRED,
 "fields" BYTEA NOT NULL
);
CREATE INDEX "tessera_objects_type" ON "tessera_objects" ("type_id");
CREATE TABLE "tessera_access" (
 "principal" TEXT NOT NULL,
 "authorization" TEXT NOT NULL,
 "grants" BYTEA NOT NULL,
 "generation" TEXT NOT NULL,
 "view" BYTEA NOT NULL DEFAULT '{"full":true}',
 PRIMARY KEY ("principal", "authorization")
);
CREATE TABLE "tessera_receipts" (
 "key" TEXT NOT NULL,
 "dataset_id" TEXT NOT NULL,
 "principal" TEXT NOT NULL,
 "authorization" TEXT NOT NULL,
 "digest" BYTEA NOT NULL CHECK (length("digest") = 32),
 "claimed_at" BIGINT NOT NULL,
 "state" TEXT NOT NULL CHECK ("state" IN ('in_progress','committed','failed')),
 "status" INTEGER,
 "body" BYTEA,
 "objects" BYTEA,
 PRIMARY KEY ("dataset_id","key"),
 CHECK (("state" = 'in_progress' AND "status" IS NULL AND "body" IS NULL) OR
        ("state" <> 'in_progress' AND "status" IS NOT NULL AND "body" IS NOT NULL))
);
CREATE INDEX "tessera_receipts_retention" ON "tessera_receipts" ("state", "claimed_at");
CREATE TABLE "tessera_sequences" (
 "id" TEXT PRIMARY KEY,
 "dataset_id" TEXT NOT NULL,
 "principal" TEXT NOT NULL,
 "authorization" TEXT NOT NULL,
 "generation" TEXT NOT NULL,
 "expires_at" BIGINT NOT NULL,
 "query" BYTEA NOT NULL
);
CREATE TABLE "tessera_pages" (
 "cursor" TEXT PRIMARY KEY,
 "sequence_id" TEXT NOT NULL REFERENCES "tessera_sequences"("id") ON DELETE CASCADE,
 "body" BYTEA NOT NULL
);
CREATE INDEX "tessera_pages_sequence" ON "tessera_pages"("sequence_id");
CREATE TABLE "tessera_retirements" (
 "dataset_id" TEXT PRIMARY KEY,
 "replacement_id" TEXT NOT NULL,
 "retired_at" BIGINT NOT NULL,
 "journaled" INTEGER NOT NULL DEFAULT 0,
 "cleaned" INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE "tessera_migration" ("version" INTEGER PRIMARY KEY);
INSERT INTO "tessera_migration" VALUES (1);
