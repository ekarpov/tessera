-- Upgrade from the v0.1.0 schema. The engine converts legacy record values
-- before dropping the payload column and advancing the schema version.

-- The key is derived. Legacy definitions may load without a cached image.
ALTER TABLE "tessera_dataset" ADD COLUMN "schema_cache_key" TEXT;

-- Invalidate the stored key whenever the declared metadata changes.
CREATE OR REPLACE FUNCTION "tessera_invalidate_schema_cache"() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        UPDATE "tessera_dataset" SET "schema_cache_key" = NULL
        WHERE "dataset_id" = OLD."dataset_id" AND "schema_cache_key" IS NOT NULL;
    END IF;
    IF TG_OP <> 'DELETE' THEN
        UPDATE "tessera_dataset" SET "schema_cache_key" = NULL
        WHERE "dataset_id" = NEW."dataset_id" AND "schema_cache_key" IS NOT NULL;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER "tessera_schema_cache_changed"
AFTER INSERT OR UPDATE OR DELETE ON "tessera_types"
FOR EACH ROW EXECUTE FUNCTION "tessera_invalidate_schema_cache"();

ALTER TABLE "tessera_objects" ADD COLUMN "wire_bytes" INTEGER;
CREATE TABLE "tessera_field_values" (
    "dataset_id" TEXT NOT NULL,
    "object_id" TEXT NOT NULL,
    "field_id" TEXT NOT NULL,
    "kind" TEXT NOT NULL,
    "number" BIGINT,
    "data" BYTEA,
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
