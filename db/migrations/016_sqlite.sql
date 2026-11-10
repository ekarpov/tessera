-- A null key preserves the ordinary loader for pre-upgrade schemas.
ALTER TABLE "tessera_dataset" ADD COLUMN "schema_cache_key" TEXT;
PRAGMA user_version = 16;
