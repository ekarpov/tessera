#!/usr/bin/env python3
"""Run against a fresh, disposable PostgreSQL 17 Docker container."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import statistics
import subprocess
import time

WORKOUT = "com.voptrack.track.workout_session"
CUSTOM = "lab.climbing_workout"
RUNNING = "com.voptrack.track.running"


def literal(value):
    return "'" + value.replace("'", "''") + "'"


def invoke(container, args, data=None):
    return subprocess.run(
        ["docker", "exec", "-i", container, *args],
        input=data, text=True, check=True, stdout=subprocess.PIPE,
    ).stdout


def sql(container, statement):
    return invoke(
        container, ["psql", "-XAtq", "-v", "ON_ERROR_STOP=1", "-U", "postgres"],
        "SET search_path TO \"track_lab\";\n" + statement,
    ).strip()


def load_catalog(path):
    manifest = json.loads((path / "manifest.json").read_text())
    types = {}
    for document in manifest["documents"]:
        payload = (path / document["path"]).read_bytes()
        if hashlib.sha256(payload).hexdigest() != document["sha256"]:
            raise ValueError("catalog document checksum mismatch: " + document["path"])
        definition = json.loads(payload)["installation"]["type"]
        if definition["id"] != document["typeID"]:
            raise ValueError("catalog type ID mismatch")
        types[definition["id"]] = definition
    root = manifest["rootTypeID"]
    assert WORKOUT in types and RUNNING in types
    assert types[WORKOUT]["parent_id"] == root
    assert any(f["id"] == "timestamp" and f["type"] == "date_time" for f in types[root]["fields"])
    assert any(f["id"] == "effort" and f["type"] == "integer" for f in types[WORKOUT]["fields"])
    return manifest, types


def seed(container, manifest, types, datasets, records):
    revision = literal(manifest["sourceRevision"])
    root = literal(manifest["rootTypeID"])
    rows = ",\n".join(
        f"({revision}, {literal(name)}, {literal(json.dumps(definition))})"
        for name, definition in types.items()
    )
    custom = literal(json.dumps({
        "id": CUSTOM, "parent_id": WORKOUT,
        "fields": [{"id": "grade", "meta_id": CUSTOM, "type": "text"}],
    }))
    sql(container, f"""
        INSERT INTO "catalog" VALUES ({revision}, {root});
        INSERT INTO "catalog_type" VALUES {rows};
        INSERT INTO "field_binding" VALUES
          ({revision}, {root}, 'timestamp', 'timestamp'),
          ({revision}, '{WORKOUT}', 'effort', 'effort');
        INSERT INTO "dataset"
          SELECT d, {revision}, (d % 4 = 1)::integer FROM generate_series(1, {datasets}) d;
        INSERT INTO "extension_type"
          SELECT "dataset_id", '{CUSTOM}', {custom}
          FROM "dataset" WHERE "extension_revision" = 1;
        INSERT INTO "track"
          SELECT d, i::text,
                 CASE WHEN i % 5 = 0 THEN '{RUNNING}'
                      WHEN i % 5 = 1 AND d % 4 = 1 THEN '{CUSTOM}' ELSE '{WORKOUT}' END,
                 CASE WHEN i % 31 <> 0
                   THEN '2026-01-01 UTC'::timestamptz + (i / 2) * interval '1 second' END,
                 CASE WHEN i % 5 = 1 AND d % 4 = 1
                      THEN jsonb_build_object('grade', jsonb_build_object('type', 'text', 'value', 'V4'))
                      ELSE '{{}}'::jsonb END
          FROM generate_series(1, {datasets}) d CROSS JOIN generate_series(1, {records}) i;
        INSERT INTO "workout"
          SELECT "dataset_id", "object_id", "object_id"::integer % 10
          FROM "track" WHERE "type_id" IN ('{WORKOUT}', '{CUSTOM}');
        INSERT INTO "objects"
          SELECT t."dataset_id", t."object_id", t."type_id",
                 convert_to((t."extras"
                   || CASE WHEN t."timestamp" IS NULL THEN '{{}}'::jsonb
                      ELSE jsonb_build_object('timestamp', jsonb_build_object('type', 'date_time',
                        'value', to_char(t."timestamp" AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))) END
                   || CASE WHEN w."effort" IS NULL THEN '{{}}'::jsonb
                      ELSE jsonb_build_object('effort', jsonb_build_object('type', 'integer',
                        'value', w."effort")) END)::text, 'UTF8')
          FROM "track" t LEFT JOIN "workout" w USING ("dataset_id", "object_id");
        VACUUM ANALYZE;
    """)


def queries(dataset, timeline=False, count=False, compact_key=None):
    scope = "" if timeline else f""" AND "type_id" = '{WORKOUT}'"""
    table = '"track"'
    typed_scope = scope
    selected_type = '"type_id"'
    if compact_key is not None:
        table = '"track_compact"'
        typed_scope = "" if timeline else f' AND "type_key" = {compact_key}'
        selected_type = '(SELECT "type_id" FROM "type_key" k WHERE k."type_key" = t."type_key") AS "type_id"'
    order = """"timestamp" DESC NULLS LAST, "object_id" ASC"""
    legacy = f"""
        SELECT "object_id", "type_id",
               convert_from("fields", 'UTF8')::jsonb AS "fields"
        FROM "objects" WHERE "dataset_id" = {dataset}{scope}
        ORDER BY (convert_from("fields", 'UTF8')::jsonb -> 'timestamp' ->> 'value')
            COLLATE "C" DESC NULLS LAST, "object_id" ASC LIMIT 10;
    """
    if count:
        legacy = f'SELECT count(*) FROM "objects" WHERE "dataset_id" = {dataset}{scope};\n' + legacy
    typed = f"""
        WITH "latest" AS MATERIALIZED (
          SELECT "dataset_id", "object_id", {selected_type}, "timestamp", "extras"
          FROM {table} t WHERE "dataset_id" = {dataset}{typed_scope}
          ORDER BY {order} LIMIT 10
        )
        SELECT l."object_id", l."type_id",
               l."extras"
               || CASE WHEN l."timestamp" IS NULL THEN '{{}}'::jsonb
                  ELSE jsonb_build_object('timestamp', jsonb_build_object('type', 'date_time',
                    'value', to_char(l."timestamp" AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))) END
               || CASE WHEN w."effort" IS NULL THEN '{{}}'::jsonb
                  ELSE jsonb_build_object('effort', jsonb_build_object('type', 'integer',
                    'value', w."effort")) END AS "fields"
        FROM "latest" l LEFT JOIN "workout" w USING ("dataset_id", "object_id")
        ORDER BY l."timestamp" DESC NULLS LAST, l."object_id" ASC;
    """
    return legacy, typed


def resources(container):
    output = invoke(container, ["sh", "-c",
        "head -1 /sys/fs/cgroup/cpu.stat; cat /sys/fs/cgroup/memory.current"])
    _, cpu, memory = output.split()
    return int(cpu), int(memory)


def bench(container, name, query, options):
    path = f"/tmp/{name}.sql"
    log = f"/tmp/{name}-{time.monotonic_ns()}.latency"
    invoke(container, ["sh", "-c", 'cat > "$1"', "sh", path],
           f"\\set dataset random(1, {options.datasets})\n" + query)
    cpu_before, memory_before = resources(container)
    start = time.monotonic()
    out = invoke(container, [
        "env", "PGOPTIONS=-c search_path=track_lab", "pgbench", "-n", "-U", "postgres",
        "-c", str(options.clients), "-j", str(min(options.clients, 8)), "-T", str(options.seconds),
        "-l", f"--log-prefix={log}", "-f", path, "postgres",
    ])
    elapsed = time.monotonic() - start
    cpu_after, memory_after = resources(container)
    latency = invoke(container, ["sh", "-c", "awk '{ print $3 }' \"$1\".*", "sh", log])
    invoke(container, ["sh", "-c", 'rm -- "$1".* "$2"', "sh", log, path])
    values = sorted(int(v) / 1000 for v in latency.split())
    throughput = re.search(r"tps = ([\d.]+)", out)[1]
    mib = 1 << 20
    print(f"{name:25} tps={float(throughput):8.0f} p50={statistics.median(values):6.2f}ms "
          f"p95={values[int(.95 * (len(values) - 1))]:6.2f}ms "
          f"p99={values[int(.99 * (len(values) - 1))]:6.2f}ms n={len(values)} "
          f"cpu_ms/op={(cpu_after - cpu_before) / len(values) / 1000:.3f} "
          f"cgroup_mib={memory_before / mib:.0f}->{memory_after / mib:.0f} "
          f"elapsed={elapsed:.1f}s", flush=True)


def size(container):
    print(sql(container, """
        SELECT relname, pg_relation_size(oid), pg_indexes_size(oid), pg_total_relation_size(oid)
        FROM pg_class WHERE relnamespace = 'track_lab'::regnamespace AND relkind = 'r'
        ORDER BY relname;
    """), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("container")
    parser.add_argument("catalogue", type=Path)
    parser.add_argument("--datasets", type=int, default=2000)
    parser.add_argument("--records", type=int, default=1000)
    parser.add_argument("--clients", type=int, default=16)
    parser.add_argument("--seconds", type=int, default=5)
    parser.add_argument("--compact", action="store_true", help="compare a compact type key on a seeded lab")
    parser.add_argument("--add-counts", action="store_true", help="install the experimental counters on a seeded lab")
    parser.add_argument("--reuse", action="store_true", help="repeat using the same seeded lab and indexes")
    args = parser.parse_args()
    if min(args.datasets, args.records, args.clients, args.seconds) < 1:
        parser.error("workload parameters must be positive")
    if args.compact and not args.reuse:
        parser.error("--compact requires --reuse")
    manifest, types = load_catalog(args.catalogue)
    print(f"catalogue v{manifest['catalogueVersion']} {manifest['sourceRevision']} "
          f"types={len(types)} datasets={args.datasets} rows/dataset={args.records}", flush=True)
    if not args.reuse:
        sql(args.container, Path(__file__).with_name("schema.sql").read_text())
        start = time.monotonic()
        seed(args.container, manifest, types, args.datasets, args.records)
        print(f"seed_seconds={time.monotonic() - start:.1f}", flush=True)

    if args.compact:
        sql(args.container, Path(__file__).with_name("compact.sql").read_text())
        compact_key = int(sql(args.container, f"""SELECT "type_key" FROM "type_key" WHERE "type_id" = '{WORKOUT}';"""))
        for timeline in (False, True):
            name = "timeline" if timeline else "exact"
            legacy, compact = queries(1, timeline, compact_key=compact_key)
            assert sql(args.container, legacy) == sql(args.container, compact)
            _, typed = queries(":dataset", timeline)
            _, compact = queries(":dataset", timeline, compact_key=compact_key)
            bench(args.container, name + "-typed-index", typed, args)
            bench(args.container, name + "-compact-index", compact, args)
        size(args.container)
        return

    if args.add_counts:
        sql(args.container, Path(__file__).with_name("counts.sql").read_text())
        expected = sql(args.container, f"SELECT count(*) FROM track WHERE dataset_id = 1 AND type_id = '{WORKOUT}';")
        actual = sql(args.container, f"SELECT records FROM track_count WHERE dataset_id = 1 AND type_id = '{WORKOUT}';")
        assert actual == expected
        changed = sql(args.container, f"""
            BEGIN;
            INSERT INTO track VALUES (1, 'count-test', '{WORKOUT}', NULL, '{{}}');
            SELECT records FROM track_count WHERE dataset_id = 1 AND type_id = '{WORKOUT}';
            DELETE FROM track WHERE dataset_id = 1 AND object_id = 'count-test';
            SELECT records FROM track_count WHERE dataset_id = 1 AND type_id = '{WORKOUT}';
            ROLLBACK;
        """)
        assert changed.splitlines() == [str(int(expected) + 1), expected]
    for d in sorted({1, min(args.datasets, 2), args.datasets}):
        for timeline in (False, True):
            legacy, typed = queries(d, timeline)
            assert sql(args.container, legacy) == sql(args.container, typed), (d, timeline)
    print("result parity passed for exact and descendant timeline queries", flush=True)

    for timeline in (False, True):
        name = "timeline" if timeline else "exact"
        legacy, typed = queries(":dataset", timeline)
        bench(args.container, name + "-json", legacy, args)
        if not args.reuse:
            bench(args.container, name + "-typed-scan", typed, args)
    if not args.reuse:
        sql(args.container, """
            CREATE INDEX "track_recent_exact" ON "track"
                ("dataset_id", "type_id", "timestamp" DESC NULLS LAST, "object_id" ASC);
            CREATE INDEX "track_recent_timeline" ON "track"
                ("dataset_id", "timestamp" DESC NULLS LAST, "object_id" ASC);
            ANALYZE "track";
        """)
    for timeline in (False, True):
        name = "timeline" if timeline else "exact"
        legacy, typed = queries(":dataset", timeline, count=True)
        bench(args.container, name + "-json-with-count", legacy, args)
        bench(args.container, name + "-typed-index", typed, args)
        scope = "" if timeline else f""" AND "type_id" = '{WORKOUT}'"""
        count = f'SELECT count(*) FROM "track" WHERE "dataset_id" = :dataset{scope};\n'
        bench(args.container, name + "-typed-with-count", count + typed, args)
        if args.add_counts:
            count_type = "" if timeline else WORKOUT
            indexed_count = f"""SELECT coalesce((SELECT "records" FROM "track_count"
                WHERE "dataset_id" = :dataset AND "type_id" = '{count_type}'), 0);"""
            bench(args.container, name + "-typed-counter", indexed_count + typed, args)
        plan = queries(1, timeline)[1]
        print(sql(args.container, "EXPLAIN (ANALYZE, BUFFERS) " + plan), flush=True)
    print("relation | heap bytes | index bytes | total bytes", flush=True)
    size(args.container)


if __name__ == "__main__":
    main()
