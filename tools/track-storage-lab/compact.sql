-- Resolve the concrete type to a narrow key before writing the root row.
-- In the lab the custom type has the same definition in every dataset
-- that declares it. A production key would include catalog or dataset scope.
CREATE TABLE "type_key" (
    "type_key" integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    "type_id" text NOT NULL UNIQUE
);
INSERT INTO "type_key" ("type_id") SELECT DISTINCT "type_id" FROM "track";

CREATE TABLE "track_compact" (
    "dataset_id" integer NOT NULL REFERENCES "dataset",
    "object_id" text COLLATE "C" NOT NULL,
    "type_key" integer NOT NULL REFERENCES "type_key",
    "timestamp" timestamptz,
    "extras" jsonb NOT NULL,
    PRIMARY KEY ("dataset_id", "object_id")
);
INSERT INTO "track_compact"
SELECT t."dataset_id", t."object_id", k."type_key", t."timestamp", t."extras"
FROM "track" t JOIN "type_key" k USING ("type_id");

CREATE INDEX "track_compact_exact" ON "track_compact"
    ("dataset_id", "type_key", "timestamp" DESC NULLS LAST, "object_id" ASC);
CREATE INDEX "track_compact_timeline" ON "track_compact"
    ("dataset_id", "timestamp" DESC NULLS LAST, "object_id" ASC);
VACUUM ANALYZE "track_compact";
