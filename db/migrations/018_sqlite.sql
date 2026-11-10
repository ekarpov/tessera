-- Upgrade from the v0.1.0 schema. The engine converts legacy record values
-- before dropping the payload column and advancing the schema version.

-- A null key preserves the ordinary loader for pre-upgrade schemas.
ALTER TABLE "tessera_dataset" ADD COLUMN "schema_cache_key" TEXT;
ALTER TABLE "tessera_objects" ADD COLUMN "wire_bytes" INTEGER;
CREATE TABLE "tessera_field_values" (
    "dataset_id" TEXT NOT NULL,
    "object_id" TEXT NOT NULL,
    "field_id" TEXT NOT NULL,
    "kind" TEXT NOT NULL,
    "number" INTEGER,
    "data" BLOB,
    CHECK (
        ("kind" IN ('integer', 'float', 'boolean') AND "number" IS NOT NULL AND "data" IS NULL)
        OR ("kind" IN ('text', 'fixed', 'date_time', 'blob', 'media', 'json', 'reference') AND "number" IS NULL AND "data" IS NOT NULL)
        OR ("kind" = 'null' AND "number" IS NULL AND "data" IS NULL)
    ),
    PRIMARY KEY ("dataset_id", "object_id", "field_id"),
    FOREIGN KEY ("dataset_id", "object_id") REFERENCES "tessera_objects" ON DELETE CASCADE
);
CREATE INDEX "tessera_field_values_integer" ON "tessera_field_values"
    ("dataset_id", "field_id", "number", "object_id") WHERE "kind" = 'integer';

CREATE TABLE "tessera_managed_layouts" (
    "layout_id" TEXT PRIMARY KEY,
    "definition" BLOB NOT NULL
);
CREATE TABLE "tessera_managed_installations" (
    "dataset_id" TEXT NOT NULL,
    "type_id" TEXT NOT NULL,
    "layout_id" TEXT NOT NULL REFERENCES "tessera_managed_layouts",
    PRIMARY KEY ("dataset_id", "type_id"),
    FOREIGN KEY ("dataset_id", "type_id") REFERENCES "tessera_types" ON DELETE CASCADE
);
ALTER TABLE "tessera_dataset" ADD COLUMN "managed_key" TEXT;
