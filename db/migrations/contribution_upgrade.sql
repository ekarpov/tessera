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
