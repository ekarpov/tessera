ALTER TABLE "tessera_receipts" ADD COLUMN "watermark" TEXT NOT NULL DEFAULT '';
UPDATE "tessera_receipts" SET "watermark"=coalesce(json_extract(CAST("body" AS TEXT), '$.watermark'), '') WHERE "state"='committed';
ALTER TABLE "tessera_receipts" ADD COLUMN "replay_scope" TEXT NOT NULL DEFAULT 'read';
PRAGMA user_version = 6;
