ALTER TABLE "tessera_receipts" ADD COLUMN "watermark" TEXT NOT NULL DEFAULT '';
UPDATE "tessera_receipts" SET "watermark"=coalesce((convert_from("body", 'UTF8')::jsonb ->> 'watermark'), '') WHERE "state"='committed';
ALTER TABLE "tessera_receipts" ADD COLUMN "replay_scope" TEXT NOT NULL DEFAULT 'read';
UPDATE "tessera_migration" SET "version"=3;
