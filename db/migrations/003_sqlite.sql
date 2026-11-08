ALTER TABLE "tessera_dataset" ADD COLUMN "replaced_dataset_id" TEXT;
ALTER TABLE "tessera_receipts" RENAME TO "tessera_receipts_v1";
CREATE TABLE "tessera_receipts" (
 "key" TEXT NOT NULL,
 "dataset_id" TEXT NOT NULL,
 "principal" TEXT NOT NULL,
 "authorization" TEXT NOT NULL,
 "digest" BLOB NOT NULL CHECK (length("digest") = 32),
 "claimed_at" INTEGER NOT NULL,
 "state" TEXT NOT NULL CHECK ("state" IN ('in_progress','committed','failed')),
 "status" INTEGER,
 "body" BLOB,
 "objects" BLOB,
 PRIMARY KEY ("dataset_id","key"),
 CHECK (("state" = 'in_progress' AND "status" IS NULL AND "body" IS NULL) OR
        ("state" <> 'in_progress' AND "status" IS NOT NULL AND "body" IS NOT NULL))
);
INSERT INTO "tessera_receipts" SELECT * FROM "tessera_receipts_v1";
DROP TABLE "tessera_receipts_v1";
CREATE INDEX "tessera_receipts_retention" ON "tessera_receipts"("state","claimed_at");
CREATE TABLE "tessera_retirements" (
 "dataset_id" TEXT PRIMARY KEY,
 "replacement_id" TEXT NOT NULL,
 "retired_at" INTEGER NOT NULL,
 "journaled" INTEGER NOT NULL DEFAULT 0,
 "cleaned" INTEGER NOT NULL DEFAULT 0
);
PRAGMA user_version = 3;
