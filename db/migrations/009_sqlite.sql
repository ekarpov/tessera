-- A dataset gains a name of its own. The listing answered "Tessera dataset" for
-- every entry, which was adequate while there could only be one and is not once
-- a caller may hold several: a name is how a person tells them apart.
--
-- Existing rows take the constant the listing used to return, so an upgraded
-- database answers exactly what it answered before.
ALTER TABLE "tessera_dataset" ADD COLUMN "display_name" TEXT NOT NULL DEFAULT '';
UPDATE "tessera_dataset" SET "display_name"='Tessera dataset' WHERE "display_name"='';

PRAGMA user_version = 9;
