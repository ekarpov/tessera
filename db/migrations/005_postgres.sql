-- One database held one dataset, because "tessera_dataset" was a singleton row
-- and the record tables carried no dataset of their own. The protocol has always
-- described "GET /v1/datasets" as a list and said a server may host many, so this
-- keys every record table by the dataset that owns it. Nothing about the wire
-- changes here: the engine still serves the dataset it was opened with.
--
-- Populate before restructuring. While the singleton constraint still holds,
-- "SELECT dataset_id FROM tessera_dataset" is a scalar, and after it does not.
ALTER TABLE "tessera_types" ADD COLUMN "dataset_id" TEXT;
UPDATE "tessera_types" SET "dataset_id"=(SELECT "dataset_id" FROM "tessera_dataset");
ALTER TABLE "tessera_types" ALTER COLUMN "dataset_id" SET NOT NULL;

ALTER TABLE "tessera_objects" DROP CONSTRAINT "tessera_objects_type_id_fkey";
ALTER TABLE "tessera_objects" ADD COLUMN "dataset_id" TEXT;
UPDATE "tessera_objects" SET "dataset_id"=(SELECT "dataset_id" FROM "tessera_dataset");
ALTER TABLE "tessera_objects" ALTER COLUMN "dataset_id" SET NOT NULL;

ALTER TABLE "tessera_access" ADD COLUMN "dataset_id" TEXT;
UPDATE "tessera_access" SET "dataset_id"=(SELECT "dataset_id" FROM "tessera_dataset");
ALTER TABLE "tessera_access" ALTER COLUMN "dataset_id" SET NOT NULL;

-- The dataset row itself stops being a singleton and becomes keyed by identity.
ALTER TABLE "tessera_dataset" DROP CONSTRAINT "tessera_dataset_pkey";
ALTER TABLE "tessera_dataset" DROP COLUMN "singleton";
ALTER TABLE "tessera_dataset" ADD PRIMARY KEY ("dataset_id");

ALTER TABLE "tessera_types" DROP CONSTRAINT "tessera_types_pkey";
ALTER TABLE "tessera_types" ADD PRIMARY KEY ("dataset_id","type_id");

ALTER TABLE "tessera_objects" DROP CONSTRAINT "tessera_objects_pkey";
ALTER TABLE "tessera_objects" ADD PRIMARY KEY ("dataset_id","object_id");
ALTER TABLE "tessera_objects" ADD FOREIGN KEY ("dataset_id","type_id")
  REFERENCES "tessera_types"("dataset_id","type_id") DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE "tessera_access" DROP CONSTRAINT "tessera_access_pkey";
ALTER TABLE "tessera_access" ADD PRIMARY KEY ("dataset_id","principal","authorization");

DROP INDEX "tessera_objects_type";
CREATE INDEX "tessera_objects_type" ON "tessera_objects" ("dataset_id","type_id");
UPDATE "tessera_migration" SET "version"=5;
