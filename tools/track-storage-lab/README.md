# Typed Track storage experiment

The PostgreSQL lab compares the released JSON sort with shared typed tables.
It loads the application's exported catalog directly, verifies every type
document against the manifest, and keeps application definitions outside this
repository. All records and dataset assignments are synthetic.

## Run

Use a fresh PostgreSQL 17 container with no published port. The first run
creates a new `track_lab` schema and fails if that name already exists.

```bash
docker run --rm -d --name tessera-track-lab \
  -e POSTGRES_HOST_AUTH_METHOD=trust postgres:17-bookworm
docker exec tessera-track-lab pg_isready -U postgres
catalogue=/path/to/voptrack-ios/catalogue/5
python3 tools/track-storage-lab/run.py tessera-track-lab "$catalogue"
python3 tools/track-storage-lab/run.py tessera-track-lab "$catalogue" --reuse --add-counts
python3 tools/track-storage-lab/run.py tessera-track-lab "$catalogue" --reuse --compact
TESSERA_TRACK_CATALOGUE="$catalogue" go test ./tools/track-storage-lab -count=1
docker stop tessera-track-lab
```

The measured configuration used PostgreSQL 17.11, 2,000 datasets, 1,000
records per dataset, 16 concurrent clients and five seconds per case. Each
transaction selected a dataset uniformly. The app export was v5, source
revision `959774c1c310997260dd55e6fe67ab15d698a319`, with 80 definitions.
One in four datasets had a custom workout subtype. The fixture included
missing timestamps and equal timestamps. The latest results exercised the
object ID tie-breaks.

## Results

| SQL path | Exact type p95 | Exact type reads/s | Timeline p95 | Timeline reads/s |
| --- | ---: | ---: | ---: | ---: |
| JSON sort, candidate count | 7.45 ms | 3,273 | 9.62 ms | 2,453 |
| Typed sort, no ordered index or count (first run) | 2.64 ms | 11,295 | 5.84 ms | 5,055 |
| Typed index, no count | 0.55 ms | 35,647 | 0.49 ms | 40,526 |
| Typed index, scan-based count | 0.72 ms | 26,507 | 0.70 ms | 27,154 |
| Typed index, maintained count | 0.68 ms | 28,451 | 0.65 ms | 29,547 |

Exact-type and timeline results matched the JSON path in the parity checks.
PostgreSQL plans used the ordered index for ten rows and made ten child-key
lookups. The repeated exact-type comparison consumed 4.793 CPU ms per
request for JSON with a count and 0.532 CPU ms for the typed index with a
maintained count. These are aggregate container CPU times including
`pgbench`. Container memory was roughly 2.45 GiB during the repeated run,
including both layouts, PostgreSQL and the OS page cache. It does not
isolate each layout's memory usage.

The baseline object relation and indexes occupied 415 MiB. The typed root
and workout child occupied about 600 MiB with text type IDs, and 462 MiB
with compact internal type keys. The latter omits the separate plain
type index because the ordered exact-type index already starts with the
same keys. In the compact comparison p95 was 0.47 ms for both shapes,
compared with 0.48 ms and 0.43 ms for the text-key layout. Compact keys
reduced disk usage without a clear read speed improvement.

These are SQL read measurements from a local synthetic lab. The timeline
contains all types in one Track tree. Restricted views, selective filters,
write throughput, cold-cache behavior and billion-row capacity require
their own measurements. Counters trade faster reads for extra writes on
inserts, deletes and type changes.

## What to review

- Keep the shared root and child tables as the first candidate. The root
  stores inherited timestamps and user JSON. Workout fields that need
  native SQL types live in the child row. Resolve compact type keys from
  catalog or dataset metadata and keep wire type IDs unchanged.
- Start with the exact-type and timeline indexes shown here. Confirm which
  query patterns justify their disk and update cost before adding more.
- Keep scan-based counting as the first implementation for filtered and
  restricted reads. Compare counters under mixed traffic before adopting
  them. A full-dataset count does not represent a restricted view.
- Wire the prototype into the production evaluator after review. It still
  needs live write, cursor, authorization, migration and restore coverage.

The server change on this branch shares immutable decoded type definitions
by content. Identical schemas can share one cache entry across datasets.
This first cache does not split production catalog rows from extensions.
Pre-upgrade schemas can continue through the ordinary loader until the next
schema edit. The HTTP smoke test installs the same app catalog and checks an
inherited workout timestamp and custom field on the released routes. The
typed tables and benchmark counters are confined to the lab.
