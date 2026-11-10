ALTER TABLE "tessera_dataset" ADD COLUMN "typed_fields" INTEGER NOT NULL DEFAULT 0;
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
CREATE VIEW "tessera_read_objects" AS
SELECT o."dataset_id", o."object_id", o."type_id",
    CASE WHEN o."wire_bytes" IS NULL THEN o."fields" ELSE convert_to(coalesce((
        SELECT json_agg(json_build_object('field', v."field_id", 'kind', v."kind",
            'number', v."number", 'data', encode(v."data", 'hex')))::text
        FROM "tessera_field_values" v
        WHERE v."dataset_id" = o."dataset_id" AND v."object_id" = o."object_id"
    ), '[]'), 'UTF8') END AS "fields",
    coalesce(o."wire_bytes", length(o."fields")) AS "wire_bytes"
FROM "tessera_objects" o;
-- A process still running the preceding version must not write a JSON
-- payload into a typed dataset or add an object without a typed marker.
CREATE OR REPLACE FUNCTION "tessera_check_field_layout"() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM "tessera_dataset"
        WHERE "dataset_id" = NEW."dataset_id" AND "typed_fields" = 1)
        AND (NEW."wire_bytes" IS NULL OR length(NEW."fields") <> 0) THEN
        RAISE EXCEPTION 'inconsistent object storage layout';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER "tessera_field_layout"
BEFORE INSERT OR UPDATE ON "tessera_objects"
FOR EACH ROW EXECUTE FUNCTION "tessera_check_field_layout"();
UPDATE "tessera_migration" SET "version" = 15;
