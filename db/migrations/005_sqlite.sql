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
 "expires_at" BIGINT NOT NULL
);
PRAGMA user_version = 5;
