DROP TRIGGER "tessera_field_layout" ON "tessera_objects";
DROP FUNCTION "tessera_check_field_layout"();
DROP VIEW "tessera_read_objects";
CREATE TABLE "tessera_managed_layouts" (
    "layout_id" TEXT PRIMARY KEY,
    "definition" BYTEA NOT NULL
);
CREATE TABLE "tessera_managed_installations" (
    "dataset_id" TEXT NOT NULL,
    "type_id" TEXT NOT NULL,
    "layout_id" TEXT NOT NULL REFERENCES "tessera_managed_layouts",
    PRIMARY KEY ("dataset_id", "type_id"),
    FOREIGN KEY ("dataset_id", "type_id") REFERENCES "tessera_types" ON DELETE CASCADE
);
ALTER TABLE "tessera_dataset" ADD COLUMN "managed_key" TEXT;
UPDATE "tessera_migration" SET "version" = 16;
