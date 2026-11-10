ALTER TABLE "tessera_dataset" ADD COLUMN "typed_fields" INTEGER NOT NULL DEFAULT 0;
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
CREATE VIEW "tessera_read_objects" AS
SELECT o."dataset_id", o."object_id", o."type_id",
    CASE WHEN o."wire_bytes" IS NULL THEN o."fields" ELSE CAST((
        SELECT json_group_array(json_object('field', v."field_id", 'kind', v."kind",
            'number', v."number", 'data', hex(v."data")))
        FROM "tessera_field_values" v
        WHERE v."dataset_id" = o."dataset_id" AND v."object_id" = o."object_id"
    ) AS BLOB) END AS "fields",
    coalesce(o."wire_bytes", length(o."fields")) AS "wire_bytes"
FROM "tessera_objects" o;
PRAGMA user_version = 17;
