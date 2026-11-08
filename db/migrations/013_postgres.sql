-- A page set is computed a page at a time. A page not yet served holds no
-- body, only the position it resumes from; a sorted sequence keeps the rows it
-- has returned, so a row whose sort value moves forward past the position is
-- not returned twice. Starting a page set used to evaluate the whole query
-- and store every page before answering with the first.
ALTER TABLE "tessera_pages" ADD COLUMN "position" BYTEA;
CREATE TABLE "tessera_page_rows" (
 "sequence_id" TEXT NOT NULL REFERENCES "tessera_sequences"("id") ON DELETE CASCADE,
 "object_id" TEXT NOT NULL,
 PRIMARY KEY ("sequence_id","object_id")
);
UPDATE "tessera_migration" SET "version"=13;
