-- One database held one dataset, because "tessera_dataset" was a singleton row
-- and the record tables carried no dataset of their own. The protocol has always
-- described "GET /v1/datasets" as a list and said a server may host many, so this
-- keys every record table by the dataset that owns it. Nothing about the wire
-- changes here: the engine still serves the dataset it was opened with.
--
-- SQLite cannot alter a primary key, so each table is rebuilt. Objects are
-- carried out and their table dropped *before* types is renamed: renaming a
-- table that another table still references by its old name fails, and the
-- carry table holds no foreign key of its own. Deferred enforcement lets the
-- intermediate states exist until commit.
PRAGMA defer_foreign_keys=ON;

CREATE TABLE "tessera_objects_carry" (
 "object_id" TEXT NOT NULL,
 "type_id" TEXT NOT NULL,
 "fields" BLOB NOT NULL
);
INSERT INTO "tessera_objects_carry" SELECT "object_id","type_id","fields" FROM "tessera_objects";
DROP TABLE "tessera_objects";

CREATE TABLE "tessera_types_new" (
 "dataset_id" TEXT NOT NULL,
 "type_id" TEXT NOT NULL,
 "body" BLOB NOT NULL,
 PRIMARY KEY ("dataset_id","type_id")
);
INSERT INTO "tessera_types_new"
 SELECT (SELECT "dataset_id" FROM "tessera_dataset"),"type_id","body" FROM "tessera_types";
DROP TABLE "tessera_types";
ALTER TABLE "tessera_types_new" RENAME TO "tessera_types";

CREATE TABLE "tessera_objects" (
 "dataset_id" TEXT NOT NULL,
 "object_id" TEXT NOT NULL,
 "type_id" TEXT NOT NULL,
 "fields" BLOB NOT NULL,
 PRIMARY KEY ("dataset_id","object_id"),
 FOREIGN KEY ("dataset_id","type_id")
   REFERENCES "tessera_types"("dataset_id","type_id") DEFERRABLE INITIALLY DEFERRED
);
INSERT INTO "tessera_objects"
 SELECT (SELECT "dataset_id" FROM "tessera_dataset"),"object_id","type_id","fields"
 FROM "tessera_objects_carry";
DROP TABLE "tessera_objects_carry";
CREATE INDEX "tessera_objects_type" ON "tessera_objects" ("dataset_id","type_id");

CREATE TABLE "tessera_access_new" (
 "dataset_id" TEXT NOT NULL,
 "principal" TEXT NOT NULL,
 "authorization" TEXT NOT NULL,
 "grants" BLOB NOT NULL,
 "generation" TEXT NOT NULL DEFAULT '',
 "view" BLOB NOT NULL DEFAULT '{"full":true}',
 PRIMARY KEY ("dataset_id","principal","authorization")
);
INSERT INTO "tessera_access_new"
 SELECT (SELECT "dataset_id" FROM "tessera_dataset"),"principal","authorization","grants","generation","view"
 FROM "tessera_access";
DROP TABLE "tessera_access";
ALTER TABLE "tessera_access_new" RENAME TO "tessera_access";

CREATE TABLE "tessera_dataset_new" (
 "dataset_id" TEXT NOT NULL PRIMARY KEY,
 "secret" BLOB NOT NULL CHECK (length("secret") = 32),
 "next_id" TEXT NOT NULL,
 "watermark" TEXT NOT NULL,
 "limits" BLOB NOT NULL,
 "replaced_dataset_id" TEXT
);
INSERT INTO "tessera_dataset_new"
 SELECT "dataset_id","secret","next_id","watermark","limits","replaced_dataset_id" FROM "tessera_dataset";
DROP TABLE "tessera_dataset";
ALTER TABLE "tessera_dataset_new" RENAME TO "tessera_dataset";

PRAGMA user_version = 8;
