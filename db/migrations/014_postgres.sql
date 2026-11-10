-- The key is derived. Legacy definitions may load without a cached image.
ALTER TABLE "tessera_dataset" ADD COLUMN "schema_cache_key" TEXT;

-- A writer using the previous binary can still be connected during an
-- upgrade. Invalidate in SQL, including when that writer updates metadata.
CREATE OR REPLACE FUNCTION "tessera_invalidate_schema_cache"() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        UPDATE "tessera_dataset" SET "schema_cache_key" = NULL
        WHERE "dataset_id" = OLD."dataset_id" AND "schema_cache_key" IS NOT NULL;
    END IF;
    IF TG_OP <> 'DELETE' THEN
        UPDATE "tessera_dataset" SET "schema_cache_key" = NULL
        WHERE "dataset_id" = NEW."dataset_id" AND "schema_cache_key" IS NOT NULL;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER "tessera_schema_cache_changed"
AFTER INSERT OR UPDATE OR DELETE ON "tessera_types"
FOR EACH ROW EXECUTE FUNCTION "tessera_invalidate_schema_cache"();

UPDATE "tessera_migration" SET "version" = 14;
