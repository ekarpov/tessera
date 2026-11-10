-- Candidate counts use the empty type for an unrestricted dataset timeline.
-- The initial fill and trigger installation assume an offline lab seed.
CREATE TABLE "track_count" (
    "dataset_id" integer NOT NULL REFERENCES "dataset",
    "type_id" text NOT NULL,
    "records" bigint NOT NULL CHECK ("records" >= 0),
    PRIMARY KEY ("dataset_id", "type_id")
);
INSERT INTO "track_count"
SELECT "dataset_id", coalesce("type_id", ''), count(*)
FROM "track"
GROUP BY GROUPING SETS (("dataset_id", "type_id"), ("dataset_id"));

CREATE FUNCTION "update_track_count"() RETURNS trigger
LANGUAGE plpgsql
SET search_path TO "track_lab"
AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND (NEW."dataset_id", NEW."type_id") =
                           (OLD."dataset_id", OLD."type_id") THEN
        RETURN NULL;
    END IF;
    IF TG_OP <> 'INSERT' THEN
        UPDATE "track_count" SET "records" = "records" - 1
        WHERE "dataset_id" = OLD."dataset_id" AND "type_id" IN ('', OLD."type_id");
    END IF;
    IF TG_OP <> 'DELETE' THEN
        INSERT INTO "track_count" VALUES
            (NEW."dataset_id", '', 1), (NEW."dataset_id", NEW."type_id", 1)
        ON CONFLICT ("dataset_id", "type_id") DO UPDATE
        SET "records" = "track_count"."records" + 1;
    END IF;
    RETURN NULL;
END;
$$;
CREATE TRIGGER "track_count_changes"
AFTER INSERT OR DELETE OR UPDATE OF "dataset_id", "type_id" ON "track"
FOR EACH ROW EXECUTE FUNCTION "update_track_count"();
