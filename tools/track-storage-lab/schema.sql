CREATE SCHEMA "track_lab";
SET search_path TO "track_lab";

CREATE TABLE "catalog" (
    "revision" text PRIMARY KEY,
    "root_type" text NOT NULL
);
CREATE TABLE "catalog_type" (
    "revision" text NOT NULL REFERENCES "catalog",
    "type_id" text NOT NULL,
    "definition" jsonb NOT NULL,
    PRIMARY KEY ("revision", "type_id")
);
CREATE TABLE "field_binding" (
    "revision" text NOT NULL,
    "declaring_type" text NOT NULL,
    "field_id" text NOT NULL,
    "column_name" text NOT NULL,
    PRIMARY KEY ("revision", "declaring_type", "field_id"),
    FOREIGN KEY ("revision", "declaring_type") REFERENCES "catalog_type"
);
CREATE TABLE "dataset" (
    "dataset_id" integer PRIMARY KEY,
    "revision" text NOT NULL REFERENCES "catalog",
    "extension_revision" bigint NOT NULL DEFAULT 0
);
CREATE TABLE "extension_type" (
    "dataset_id" integer NOT NULL REFERENCES "dataset",
    "type_id" text NOT NULL,
    "definition" jsonb NOT NULL,
    PRIMARY KEY ("dataset_id", "type_id")
);

-- Released storage shape. Integer dataset keys are used in both arms of
-- the lab, so identifier width does not drive the comparison.
CREATE TABLE "objects" (
    "dataset_id" integer NOT NULL REFERENCES "dataset",
    "object_id" text COLLATE "C" NOT NULL,
    "type_id" text NOT NULL,
    "fields" bytea NOT NULL,
    PRIMARY KEY ("dataset_id", "object_id")
);
CREATE INDEX "objects_type" ON "objects" ("dataset_id", "type_id");

-- Shared root table. Descendants keep their concrete type on the root row.
CREATE TABLE "track" (
    "dataset_id" integer NOT NULL REFERENCES "dataset",
    "object_id" text COLLATE "C" NOT NULL,
    "type_id" text NOT NULL,
    "timestamp" timestamptz,
    "extras" jsonb NOT NULL,
    PRIMARY KEY ("dataset_id", "object_id")
);
CREATE INDEX "track_type" ON "track" ("dataset_id", "type_id");

CREATE TABLE "workout" (
    "dataset_id" integer NOT NULL,
    "object_id" text COLLATE "C" NOT NULL,
    "effort" bigint,
    PRIMARY KEY ("dataset_id", "object_id"),
    FOREIGN KEY ("dataset_id", "object_id") REFERENCES "track" ON DELETE CASCADE
);
