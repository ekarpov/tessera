-- Current schema for a fresh PostgreSQL database.

CREATE TABLE "tessera_access" (
    "principal" text NOT NULL,
    "authorization" text NOT NULL,
    "grants" bytea NOT NULL,
    "generation" text NOT NULL,
    "view" bytea DEFAULT '\x7b2266756c6c223a747275657d'::bytea NOT NULL,
    "dataset_id" text NOT NULL,
    CONSTRAINT "tessera_access_pkey" PRIMARY KEY ("dataset_id", "principal", "authorization")
);

CREATE TABLE "tessera_attachments" (
    "dataset_id" text NOT NULL,
    "object_id" text NOT NULL,
    "type_id" text NOT NULL,
    "field_id" text NOT NULL,
    "store_id" text NOT NULL,
    "item_id" text NOT NULL,
    CONSTRAINT "tessera_attachments_pkey" PRIMARY KEY ("dataset_id", "object_id", "field_id")
);

CREATE TABLE "tessera_content_stores" (
    "id" text NOT NULL,
    "dataset_id" text NOT NULL,
    "active" integer NOT NULL,
    "replaced_store_id" text,
    "principal" text,
    "authorization" text,
    CONSTRAINT "tessera_content_stores_pkey" PRIMARY KEY ("id")
);

CREATE TABLE "tessera_dataset" (
    "dataset_id" text NOT NULL,
    "secret" bytea NOT NULL,
    "next_id" text NOT NULL,
    "watermark" text NOT NULL,
    "limits" bytea NOT NULL,
    "replaced_dataset_id" text,
    "display_name" text DEFAULT ''::text NOT NULL,
    "object_count" bigint DEFAULT 0 NOT NULL,
    "object_bytes" bigint DEFAULT 0 NOT NULL,
    "schema_cache_key" text,
    "managed_key" text,
    "creator_secret" bytea CHECK (length("creator_secret") = 32),
    CONSTRAINT "tessera_dataset_secret_check" CHECK ((length("secret") = 32)),
    CONSTRAINT "tessera_dataset_pkey" PRIMARY KEY ("dataset_id")
);
-- Preserve the v0.1.0 constraint. PostgreSQL merges duplicate keys when both
-- are declared in CREATE TABLE, so add this constraint separately.
ALTER TABLE "tessera_dataset" ADD CONSTRAINT "tessera_dataset_dataset_id_key" UNIQUE ("dataset_id");

CREATE TABLE "tessera_imports" (
    "upload_id" text NOT NULL,
    "store_id" text NOT NULL,
    "operation_id" text NOT NULL,
    "principal" text NOT NULL,
    "authorization" text NOT NULL,
    "content_type" text NOT NULL,
    "declared_bytes" bigint,
    "total_bytes" bigint,
    "maximum_bytes" bigint NOT NULL,
    "received_bytes" bigint DEFAULT 0 NOT NULL,
    "state" text NOT NULL,
    "item_id" text,
    "updated_at" bigint NOT NULL,
    "cleaned" integer DEFAULT 0 NOT NULL,
    "attachment_target" TEXT,
    CONSTRAINT "tessera_imports_pkey" PRIMARY KEY ("upload_id"),
    CONSTRAINT "tessera_imports_store_id_operation_id_key" UNIQUE ("store_id", "operation_id"),
    CONSTRAINT "tessera_imports_store_id_fkey" FOREIGN KEY ("store_id") REFERENCES "tessera_content_stores"("id")
);

CREATE TABLE "tessera_items" (
    "store_id" text NOT NULL,
    "item_id" text NOT NULL,
    "upload_id" text NOT NULL,
    "content_type" text NOT NULL,
    "byte_count" bigint NOT NULL,
    "deleted" integer DEFAULT 0 NOT NULL,
    "cleaned" integer DEFAULT 0 NOT NULL,
    CONSTRAINT "tessera_items_pkey" PRIMARY KEY ("store_id", "item_id")
);

CREATE TABLE "tessera_managed_layouts" (
    "layout_id" text NOT NULL,
    "definition" bytea NOT NULL,
    CONSTRAINT "tessera_managed_layouts_pkey" PRIMARY KEY ("layout_id")
);

CREATE TABLE "tessera_migration" (
    "version" integer NOT NULL,
    CONSTRAINT "tessera_migration_pkey" PRIMARY KEY ("version")
);

CREATE TABLE "tessera_receipts" (
    "key" text NOT NULL,
    "dataset_id" text NOT NULL,
    "principal" text NOT NULL,
    "authorization" text NOT NULL,
    "digest" bytea NOT NULL,
    "claimed_at" bigint NOT NULL,
    "state" text NOT NULL,
    "status" integer,
    "body" bytea,
    "objects" bytea,
    "watermark" text DEFAULT ''::text NOT NULL,
    "replay_scope" text DEFAULT 'read'::text NOT NULL,
    "executor_id" text DEFAULT ''::text NOT NULL,
    "retain_until" bigint,
    CONSTRAINT "tessera_receipts_check" CHECK (((("state" = 'in_progress'::text) AND ("status" IS NULL) AND ("body" IS NULL)) OR (("state" <> 'in_progress'::text) AND ("status" IS NOT NULL) AND ("body" IS NOT NULL)))),
    CONSTRAINT "tessera_receipts_digest_check" CHECK ((length("digest") = 32)),
    CONSTRAINT "tessera_receipts_state_check" CHECK (("state" = ANY (ARRAY['in_progress'::text, 'committed'::text, 'failed'::text]))),
    CONSTRAINT "tessera_receipts_pkey" PRIMARY KEY ("dataset_id", "key")
);

CREATE TABLE "tessera_retirements" (
    "dataset_id" text NOT NULL,
    "replacement_id" text NOT NULL,
    "retired_at" bigint NOT NULL,
    "journaled" integer DEFAULT 0 NOT NULL,
    "cleaned" integer DEFAULT 0 NOT NULL,
    CONSTRAINT "tessera_retirements_pkey" PRIMARY KEY ("dataset_id")
);

CREATE TABLE "tessera_sequences" (
    "id" text NOT NULL,
    "dataset_id" text NOT NULL,
    "principal" text NOT NULL,
    "authorization" text NOT NULL,
    "generation" text NOT NULL,
    "expires_at" bigint NOT NULL,
    "query" bytea NOT NULL,
    "meta" bytea,
    CONSTRAINT "tessera_sequences_pkey" PRIMARY KEY ("id")
);

CREATE TABLE "tessera_transfer_tokens" (
    "digest" bytea NOT NULL,
    "store_id" text NOT NULL,
    "resource_id" text NOT NULL,
    "method" text NOT NULL,
    "expires_at" bigint NOT NULL,
    "representation" text DEFAULT ''::text NOT NULL,
    CONSTRAINT "tessera_transfer_tokens_pkey" PRIMARY KEY ("digest")
);

CREATE TABLE "tessera_types" (
    "type_id" text NOT NULL,
    "body" bytea NOT NULL,
    "dataset_id" text NOT NULL,
    CONSTRAINT "tessera_types_pkey" PRIMARY KEY ("dataset_id", "type_id")
);

CREATE TABLE "tessera_managed_installations" (
    "dataset_id" text NOT NULL,
    "type_id" text NOT NULL,
    "layout_id" text NOT NULL,
    CONSTRAINT "tessera_managed_installations_pkey" PRIMARY KEY ("dataset_id", "type_id"),
    CONSTRAINT "tessera_managed_installations_dataset_id_type_id_fkey" FOREIGN KEY ("dataset_id", "type_id") REFERENCES "tessera_types"("dataset_id", "type_id") ON DELETE CASCADE,
    CONSTRAINT "tessera_managed_installations_layout_id_fkey" FOREIGN KEY ("layout_id") REFERENCES "tessera_managed_layouts"("layout_id")
);

CREATE TABLE "tessera_objects" (
    "object_id" text NOT NULL,
    "type_id" text NOT NULL,
    "dataset_id" text NOT NULL,
    "wire_bytes" integer,
    CONSTRAINT "tessera_objects_pkey" PRIMARY KEY ("dataset_id", "object_id"),
    CONSTRAINT "tessera_objects_dataset_id_type_id_fkey" FOREIGN KEY ("dataset_id", "type_id") REFERENCES "tessera_types"("dataset_id", "type_id") DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE "tessera_page_rows" (
    "sequence_id" text NOT NULL,
    "object_id" text NOT NULL,
    CONSTRAINT "tessera_page_rows_pkey" PRIMARY KEY ("sequence_id", "object_id"),
    CONSTRAINT "tessera_page_rows_sequence_id_fkey" FOREIGN KEY ("sequence_id") REFERENCES "tessera_sequences"("id") ON DELETE CASCADE
);

CREATE TABLE "tessera_pages" (
    "cursor" text NOT NULL,
    "sequence_id" text NOT NULL,
    "body" bytea NOT NULL,
    "position" bytea,
    CONSTRAINT "tessera_pages_pkey" PRIMARY KEY ("cursor"),
    CONSTRAINT "tessera_pages_sequence_id_fkey" FOREIGN KEY ("sequence_id") REFERENCES "tessera_sequences"("id") ON DELETE CASCADE
);

CREATE TABLE "tessera_field_values" (
    "dataset_id" TEXT NOT NULL,
    "object_id" TEXT NOT NULL,
    "field_id" TEXT NOT NULL,
    "kind" TEXT NOT NULL,
    "number" BIGINT,
    "data" BYTEA,
    CHECK (
        ("kind" IN ('integer', 'float', 'boolean') AND "number" IS NOT NULL AND "data" IS NULL)
        OR ("kind" IN ('text', 'fixed', 'date_time', 'blob', 'media', 'json', 'reference') AND "number" IS NULL AND "data" IS NOT NULL)
        OR ("kind" = 'null' AND "number" IS NULL AND "data" IS NULL)
    ),
    PRIMARY KEY ("dataset_id", "object_id", "field_id"),
    FOREIGN KEY ("dataset_id", "object_id") REFERENCES "tessera_objects" ON DELETE CASCADE
);

CREATE INDEX "tessera_attachments_item" ON "tessera_attachments" ("store_id", "item_id");
CREATE INDEX "tessera_field_values_integer" ON "tessera_field_values" ("dataset_id", "field_id", "number", "object_id") WHERE "kind" = 'integer';
CREATE INDEX "tessera_imports_uncleaned" ON "tessera_imports" ("state", "cleaned");
CREATE INDEX "tessera_objects_identity" ON "tessera_objects" ("dataset_id", "object_id" COLLATE "C");
CREATE INDEX "tessera_objects_type" ON "tessera_objects" ("dataset_id", "type_id");
CREATE INDEX "tessera_pages_sequence" ON "tessera_pages" ("sequence_id");
CREATE INDEX "tessera_receipts_executor" ON "tessera_receipts" ("state", "executor_id");
CREATE INDEX "tessera_receipts_retention" ON "tessera_receipts" ("state", "claimed_at");
CREATE INDEX "tessera_receipts_retention_deadline" ON "tessera_receipts" ("retain_until");

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

CREATE TABLE "tessera_contributions" (
    "dataset_id" TEXT NOT NULL,
    "object_id" TEXT NOT NULL,
    "creator_id" TEXT NOT NULL,
    PRIMARY KEY ("dataset_id","object_id"),
    FOREIGN KEY ("dataset_id","object_id") REFERENCES "tessera_objects" ("dataset_id","object_id") ON DELETE CASCADE
);
CREATE INDEX "tessera_contributions_creator" ON "tessera_contributions" ("dataset_id","creator_id","object_id");
CREATE TABLE "tessera_contribution_erasure" (
    "dataset_id" TEXT NOT NULL,
    "creator_id" TEXT NOT NULL,
    "job_id" TEXT NOT NULL UNIQUE,
    "done" INTEGER NOT NULL DEFAULT 0 CHECK ("done" IN (0,1)),
    PRIMARY KEY ("dataset_id","creator_id")
);

INSERT INTO "tessera_migration" ("version") VALUES (21);
