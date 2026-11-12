-- Current schema for a fresh SQLite database.

CREATE TABLE "tessera_sequences" (
    "id" TEXT PRIMARY KEY,
    "dataset_id" TEXT NOT NULL,
    "principal" TEXT NOT NULL,
    "authorization" TEXT NOT NULL,
    "generation" TEXT NOT NULL,
    "expires_at" INTEGER NOT NULL,
    "query" BLOB NOT NULL,
    "meta" BLOB);

CREATE TABLE "tessera_pages" (
    "cursor" TEXT PRIMARY KEY,
    "sequence_id" TEXT NOT NULL REFERENCES "tessera_sequences"("id") ON DELETE CASCADE,
    "body" BLOB NOT NULL,
    "position" BLOB);

CREATE INDEX "tessera_pages_sequence" ON "tessera_pages"("sequence_id");

CREATE TABLE "tessera_receipts" (
    "key" TEXT NOT NULL,
    "dataset_id" TEXT NOT NULL,
    "principal" TEXT NOT NULL,
    "authorization" TEXT NOT NULL,
    "digest" BLOB NOT NULL CHECK (length("digest") = 32),
    "claimed_at" INTEGER NOT NULL,
    "state" TEXT NOT NULL CHECK ("state" IN ('in_progress','committed','failed')),
    "status" INTEGER,
    "body" BLOB,
    "objects" BLOB,
    "watermark" TEXT NOT NULL DEFAULT '',
    "replay_scope" TEXT NOT NULL DEFAULT 'read',
    "executor_id" TEXT NOT NULL DEFAULT '',
    "retain_until" INTEGER,
    PRIMARY KEY ("dataset_id","key"),
    CHECK (("state" = 'in_progress' AND "status" IS NULL AND "body" IS NULL) OR
        ("state" <> 'in_progress' AND "status" IS NOT NULL AND "body" IS NOT NULL))
);

CREATE INDEX "tessera_receipts_retention" ON "tessera_receipts"("state","claimed_at");

CREATE TABLE "tessera_retirements" (
    "dataset_id" TEXT PRIMARY KEY,
    "replacement_id" TEXT NOT NULL,
    "retired_at" INTEGER NOT NULL,
    "journaled" INTEGER NOT NULL DEFAULT 0,
    "cleaned" INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE "tessera_content_stores" (
    "id" TEXT PRIMARY KEY,
    "dataset_id" TEXT NOT NULL,
    "active" INTEGER NOT NULL,
    "replaced_store_id" TEXT,
    "principal" TEXT,
    "authorization" TEXT
);

CREATE TABLE "tessera_imports" (
    "upload_id" TEXT PRIMARY KEY,
    "store_id" TEXT NOT NULL REFERENCES "tessera_content_stores"("id"),
    "operation_id" TEXT NOT NULL,
    "principal" TEXT NOT NULL,
    "authorization" TEXT NOT NULL,
    "content_type" TEXT NOT NULL,
    "declared_bytes" BIGINT,
    "total_bytes" BIGINT,
    "maximum_bytes" BIGINT NOT NULL,
    "received_bytes" BIGINT NOT NULL DEFAULT 0,
    "state" TEXT NOT NULL,
    "item_id" TEXT,
    "updated_at" BIGINT NOT NULL,
    "cleaned" INTEGER NOT NULL DEFAULT 0,
    "attachment_target" TEXT,
    UNIQUE ("store_id","operation_id")
);

CREATE TABLE "tessera_items" (
    "store_id" TEXT NOT NULL,
    "item_id" TEXT NOT NULL,
    "upload_id" TEXT NOT NULL,
    "content_type" TEXT NOT NULL,
    "byte_count" BIGINT NOT NULL,
    "deleted" INTEGER NOT NULL DEFAULT 0,
    "cleaned" INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY ("store_id","item_id")
);

CREATE TABLE "tessera_attachments" (
    "dataset_id" TEXT NOT NULL,
    "object_id" TEXT NOT NULL,
    "type_id" TEXT NOT NULL,
    "field_id" TEXT NOT NULL,
    "store_id" TEXT NOT NULL,
    "item_id" TEXT NOT NULL,
    PRIMARY KEY ("dataset_id","object_id","field_id")
);

CREATE INDEX "tessera_attachments_item" ON "tessera_attachments"("store_id","item_id");

CREATE TABLE "tessera_transfer_tokens" (
    "digest" BLOB PRIMARY KEY,
    "store_id" TEXT NOT NULL,
    "resource_id" TEXT NOT NULL,
    "method" TEXT NOT NULL,
    "expires_at" BIGINT NOT NULL,
    "representation" TEXT NOT NULL DEFAULT '');

CREATE TABLE "tessera_types" (
    "dataset_id" TEXT NOT NULL,
    "type_id" TEXT NOT NULL,
    "body" BLOB NOT NULL,
    PRIMARY KEY ("dataset_id","type_id")
);

CREATE TABLE "tessera_objects" (
    "dataset_id" TEXT NOT NULL,
    "object_id" TEXT NOT NULL,
    "type_id" TEXT NOT NULL,
    "wire_bytes" INTEGER,
    PRIMARY KEY ("dataset_id","object_id"),
    FOREIGN KEY ("dataset_id","type_id")
   REFERENCES "tessera_types"("dataset_id","type_id") DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX "tessera_objects_type" ON "tessera_objects" ("dataset_id","type_id");

CREATE TABLE "tessera_access" (
    "dataset_id" TEXT NOT NULL,
    "principal" TEXT NOT NULL,
    "authorization" TEXT NOT NULL,
    "grants" BLOB NOT NULL,
    "generation" TEXT NOT NULL DEFAULT '',
    "view" BLOB NOT NULL DEFAULT '{"full":true}',
    PRIMARY KEY ("dataset_id","principal","authorization")
);

CREATE TABLE "tessera_dataset" (
    "dataset_id" TEXT NOT NULL PRIMARY KEY,
    "secret" BLOB NOT NULL CHECK (length("secret") = 32),
    "next_id" TEXT NOT NULL,
    "watermark" TEXT NOT NULL,
    "limits" BLOB NOT NULL,
    "replaced_dataset_id" TEXT,
    "display_name" TEXT NOT NULL DEFAULT '',
    "object_count" INTEGER NOT NULL DEFAULT 0,
    "object_bytes" INTEGER NOT NULL DEFAULT 0,
    "schema_cache_key" TEXT,
    "managed_key" TEXT,
    "creator_secret" BLOB CHECK (length("creator_secret") = 32));

CREATE INDEX "tessera_receipts_executor" ON "tessera_receipts" ("state","executor_id");

CREATE INDEX "tessera_receipts_retention_deadline" ON "tessera_receipts" ("retain_until");

CREATE INDEX "tessera_imports_uncleaned" ON "tessera_imports" ("state","cleaned");

CREATE TABLE "tessera_page_rows" (
    "sequence_id" TEXT NOT NULL REFERENCES "tessera_sequences"("id") ON DELETE CASCADE,
    "object_id" TEXT NOT NULL,
    PRIMARY KEY ("sequence_id","object_id")
);

CREATE TABLE "tessera_field_values" (
    "dataset_id" TEXT NOT NULL,
    "object_id" TEXT NOT NULL,
    "field_id" TEXT NOT NULL,
    "kind" TEXT NOT NULL,
    "number" INTEGER,
    "data" BLOB,
    CHECK (
        ("kind" IN ('integer', 'float', 'boolean') AND "number" IS NOT NULL AND "data" IS NULL)
        OR ("kind" IN ('text', 'fixed', 'date_time', 'blob', 'media', 'json', 'reference') AND "number" IS NULL AND "data" IS NOT NULL)
        OR ("kind" = 'null' AND "number" IS NULL AND "data" IS NULL)
    ),
    PRIMARY KEY ("dataset_id", "object_id", "field_id"),
    FOREIGN KEY ("dataset_id", "object_id") REFERENCES "tessera_objects" ON DELETE CASCADE
);

CREATE INDEX "tessera_field_values_integer" ON "tessera_field_values"
    ("dataset_id", "field_id", "number", "object_id") WHERE "kind" = 'integer';

CREATE TABLE "tessera_managed_layouts" (
    "layout_id" TEXT PRIMARY KEY,
    "definition" BLOB NOT NULL
);

CREATE TABLE "tessera_managed_installations" (
    "dataset_id" TEXT NOT NULL,
    "type_id" TEXT NOT NULL,
    "layout_id" TEXT NOT NULL REFERENCES "tessera_managed_layouts",
    PRIMARY KEY ("dataset_id", "type_id"),
    FOREIGN KEY ("dataset_id", "type_id") REFERENCES "tessera_types" ON DELETE CASCADE
);

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

PRAGMA user_version = 23;
