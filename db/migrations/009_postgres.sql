-- Queries now read records in identity order and stop once they hold what the
-- answer needs, rather than reading every record and sorting in memory. That
-- is only correct when the database's order is the evaluator's, which is byte
-- order over the object identifier.
--
-- PostgreSQL orders text by the database collation, and a locale collation is
-- not byte order: it can fold case and ignore punctuation, and an object
-- identifier may contain either. So the ordered read asks for COLLATE "C"
-- explicitly. This index is what lets that read use an index at all — the
-- primary key is ordered by the default collation, and an ORDER BY in another
-- collation cannot use it, so without this the database sorts the whole scope
-- before returning the first row and stopping early saves nothing there.
CREATE INDEX "tessera_objects_identity" ON "tessera_objects" ("dataset_id", "object_id" COLLATE "C");
UPDATE "tessera_migration" SET "version"=9;
