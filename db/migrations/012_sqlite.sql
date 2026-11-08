-- A dataset's record count and stored bytes, kept on its row. A write is held
-- to bounds over the whole dataset, and working them out meant counting and
-- summing every record on every write; a usage report did the same. Kept here,
-- each write moves them by what it changed and a report reads one row.
ALTER TABLE "tessera_dataset" ADD COLUMN "object_count" INTEGER NOT NULL DEFAULT 0;
ALTER TABLE "tessera_dataset" ADD COLUMN "object_bytes" INTEGER NOT NULL DEFAULT 0;
UPDATE "tessera_dataset" SET
  "object_count" = (SELECT count(*) FROM "tessera_objects" o WHERE o."dataset_id" = "tessera_dataset"."dataset_id"),
  "object_bytes" = (SELECT coalesce(sum(length(o."fields")), 0) FROM "tessera_objects" o WHERE o."dataset_id" = "tessera_dataset"."dataset_id");
PRAGMA user_version = 12;
