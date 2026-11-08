-- A page set's schema, kept once on the sequence rather than repeated in every
-- page. A page set of one record per page over thousands of records wrote the
-- type's full description thousands of times. Pages written before this carry
-- their own and are served as they were stored.
ALTER TABLE "tessera_sequences" ADD COLUMN "meta" BYTEA;
UPDATE "tessera_migration" SET "version"=11;
