# Managed storage and shareable type definitions

Draft for review. This proposes an optional provisioning surface and an internal
storage layout. The v1 record and content contracts remain the client interface.

## Compatibility baseline

[Compatibility tests](../pkg/server/profiles_test.go) drive the same REST harness
against SQLite and PostgreSQL. The tests pass on the existing storage before any
new DDL or API is introduced.

| Scenario | Existing API behavior to preserve |
| --- | --- |
| Inherited sorting | Exact and descendant queries sort by an inherited timestamp. Direct reads preserve a custom descendant's own field. |
| Exact aggregation | An invalid field rolls back a multi-record transaction. Grouped decimal totals preserve exact precision. A scoped reader can query the result. |
| Reference pagination | A reader cannot define a type. Replacing a referenced record's text preserves its identity. Pagination returns only records matching that reference. |
| Scoped access | An insert-only contributor can write its granted concrete type. An ungranted descendant is absent from metadata and results. A cursor returns `view_changed` after the read grant expires. |

The existing content suite covers attachment transfer and access. The same
requests must run against each new storage path. When routing is added, test
that the intended path actually ran.

## Publication, installation and access

A public catalog is an admin-owned, discoverable set of type definitions for a
deployment audience. Publishing creates a revision. An app can install a starter
set at setup or install individual types on demand. Publication alone creates no dataset installation or record. When a type
requires a parent, its install brings in the allowed parent revision in the same
transaction.

A user can define private types in a dataset and share an immutable revision of
a definition with an explicit person or group. Export includes the declared
fields and authorized dependencies needed to install it. The recipient gets
an independent, pinned installation and can explicitly adopt a later revision.
Withdrawal ends future discovery and installs. An installed copy and its records
follow the destination dataset's own lifecycle.

Record grants remain separate. A definition share reveals no records or
installation membership. A deployment service can be delegated installation and
compatible upgrades in named datasets. That delegation grants neither record
access nor general schema or SQL administration.

## Proposed database schema

Names and columns below are logical. Physical migrations are a later step.

| Relation | Key and purpose |
| --- | --- |
| `meta_catalog` | `catalog_id`. Owner, public or private visibility, and deployment audience. |
| `meta_revision` | `(catalog_id, revision)`. Immutable definitions with a separately managed publication state. |
| `meta` | `(catalog_id, revision, type_id)`. Declared type and parent identity. |
| `meta_fields` | `(catalog_id, revision, type_id, field_id)`. Declared field definition. Roles and annotations are stored as opaque metadata. |
| `meta_share` | `share_id`. Private revision, explicit recipient, and grant state. |
| `meta_installation` | `(dataset_id, local_type_id)`. Source type and pinned revision, physical layout when managed, and dependency bindings. |
| `dataset_meta`, `dataset_meta_fields` | Dataset and local type keys. Private types and user-owned field additions. |
| `meta_layout`, `meta_index` | Generated layout and index identities, approved logical field bindings, and provisioning state. Table and column names are assigned by the server. |
| `meta_operation` | Operation ID, caller and request digest, approved target, and durable job state. Long-running operations record progress per dataset. |
| `record_identity` | `(dataset_id, object_id)` for the reference implementation. Concrete local type and storage route. Keeps identity-based access and attachment relationships independent of physical placement. |
| One managed table per admin-published type | `(dataset_id, object_id)`. Typed columns for fields declared on that type. A child row joins to its parent with both keys. An ancestor row can be addressed by concrete type for exact and descendant queries. |
| `custom_field_values` | `(dataset_id, object_id, field_key)`. One row per populated custom field, using a typed value column. The row refers to `record_identity` and to the dataset's field definition. |

A managed child uses a root row for its inherited timestamp and a child row
for its declared fields. A private descendant reuses those rows and stores
its own fields in `custom_field_values`. A record with no managed ancestor
uses `record_identity` and the same shared values table. Adding a private
type or field requires no DDL. Every dataset adopting a managed layout can
reuse the same physical tables.

Cache immutable shared revisions by catalog and revision. Cache sparse dataset
overlays separately. Resolve the effective type to its root and child tables,
managed columns, and custom field keys. The physical names are server-owned.
Resolve the mapping against the same database snapshot as the records. A
revision change invalidates the affected mapping. Every request still checks
current record and metadata visibility for the caller.

## Proposed methods

Names are provisional. These are privileged catalog and deployment operations
outside the existing v1 data surface.

| Method | Purpose and boundary |
| --- | --- |
| `PlanCatalog(revision, storage_profile)` | Validate types, dependency closure, field bindings, index requests and resource limits. Return an inspectable plan with expected schema revision, generated layout IDs and estimated DDL work. |
| `PublishCatalog(plan_id, operation_key)` | Admin-only. Provision or reuse managed tables and indexes, then make the revision installable. Persist job state and support safe retry. An incomplete build never appears as an available revision. |
| `ListDefinitions()`, `GetDefinition(type, revision)` | Discover public definitions in the caller's deployment audience and private revisions explicitly visible to that caller. |
| `ShareDefinition(revision, recipient)`, `RevokeDefinitionShare(share_id)` | Owner-controlled discovery and install authority for a private definition. |
| `InstallTypes(dataset, revisions, profile, operation_key)` | Validate source visibility and destination authority. Install the requested definitions and required parents atomically. Reuse an identical install and report a conflict for an unrelated local type with the same ID. |
| `UpgradeTypes(dataset, expected_revisions, target_revisions, operation_key)` | Apply compatible updates explicitly, including a managed rollout. Refuse collisions with user fields. Preserve existing data and verify the effective schema before commit. |
| `RetireType(type_identity, expected_revision, operation_key)` | Admin-only. Close a managed type to new installations while keeping existing installations and records usable. |
| `PlanTypeDeletion(type_identity, cascade_descendants)` | Admin-only, read-only preview. Report the affected installations, records and dependent types across the deployment. |
| `DeleteManagedType(plan_id, confirmation, operation_key)` | Admin-only. Accept a current, explicitly confirmed plan and return a durable deletion job ID. |
| `GetMetadataOperation(operation_id)` | Read authorized job status and the outcome of a retry or lost response. |

An app supplies a structured storage profile at setup. It names typed fields
and declares composite indexes by logical field, order and intended scope.
For example, a storage profile can request a reference followed by position,
or an inherited timestamp order. The deployment approves the profile
before provisioning. An approved compatible layout is shared across datasets.
A dataset install selects from approved layouts and cannot create a new table
or index per tenant. A request for an unsupported storage choice fails during
planning. An ordinary client can continue to create and query types through
v1. New custom types on the new storage path use the shared values table.

Start with bounded indexes on fields within one physical table. Tessera
generates and quotes physical identifiers. It adds dataset scope, compatible
collation, null order and an object-ID tie-break where required by the query.
An inherited or cross-table sort may need a supported lookup index or the
existing evaluator. A physical index is a performance option and does not
change the operations the API accepts.

## Managed type deletion

The target is a stable managed identity such as `(catalog_id, type_id)`, across
its installed revisions. A display name or matching local ID is insufficient.
Unrelated private definitions and independent copies are outside that target.
The existing v1 dataset-local type deletion keeps its dataset scope.

1. The preview identifies affected datasets and installations, counts records
   by concrete type, and lists dependent managed and private descendants.
   The default refuses deletion when descendants exist. A cascade requires
   a preview with `cascade_descendants=true` and explicit confirmation of that
   plan. This includes the descendants' records and custom fields.
2. Execution checks that the confirmation names the target and plan, verifies
   that the affected scope and reviewed impact are still current, and refuses
   a stale plan. It then durably fences the scope against new installations
   and writes, including concurrent transactions, before scheduling deletion.
   The operation key binds the request digest to one job.
3. Each dataset commits its own deletion atomically. Remove the targeted
   records, identities, extension values, type installations and dependent
   local definitions. Apply the existing record and attachment lifecycle.
   Remove affected grants, invalidate affected cursors, and suppress purged
   record bodies in replayable responses while preserving receipt identities
   and request digests. Publish the dataset's new schema version and watermark. An interrupted
   job resumes with the fence in place. Progress is visible per dataset,
   since the whole deployment is not one transaction.
4. Keep the durable deletion record outside restorable snapshots, so a
   restore cannot bring back purged definitions or records. A physical table
   becomes eligible for removal once the job is complete and no surviving
   layout uses it. A retired type with active installations keeps its tables.

## Typed custom fields and query planning

On the proposed storage path, custom values have one authoritative copy in
`custom_field_values`. The field key identifies the declaring field within
the dataset, so descendants reuse the same key for an inherited field.
Metadata selects one value column, such as text, integer, exact decimal, or
timestamp. A missing value has no row. A field declared as opaque v1 `json`
retains its exact string and whole-value comparison semantics.

A shared index such as `(dataset_id, field_key, integer_value, object_id)`
supports an integer equality or range filter without parsing record payloads.
A declared custom field uses the existing v1 filter, sort, projection and
aggregation rules for its logical type. The plan accounts for absent values
when filtering or sorting nulls, preserves exact decimals, and joins values
by dataset as well as object identity. Multiple custom predicates can require
multiple lookups. Index coverage is subject to the deployment's limits and
measured write cost.

For a supported query shape, Tessera builds one parameterized SQL statement
from the cached mapping. A point read joins managed rows and custom values
by `(dataset_id, object_id)`. A list first selects authorized record IDs
using the query's predicates and ordering, including any custom fields used
for filtering or sorting. It applies the record limit there, then joins the
projected values. The query can combine compatible branches for descendant
types. Tessera assembles each record from its rows and retains the existing
evaluator for query shapes without an equivalent SQL plan.

## Execution and lifecycle

- Keep the existing evaluator for effective metadata, validation, visibility,
  arithmetic, and cursor semantics. Push down only plans that preserve those
  rules. Apply authorized concrete-type scope before ranking, counting or
  pagination. Keep exact decimal bounds and rounding behavior across layouts.
- Use the same mapping for inserts. Allocate the record ID, write identity
  and managed ancestor and child rows, and batch custom values into multi-row
  inserts. Commit them with attachments, the receipt and the watermark in one
  transaction. A nullable managed field can be added without rewriting old
  records. Partial writes preserve omitted fields.
- Treat physical publication as recoverable deployment work. Bound concurrent
  DDL and make index creation separately observable. Keep old revisions usable
  while installed datasets still depend on them. Inventory generated tables for
  backup and restore. Erasure removes every row for the retired dataset across
  all layouts and preserves the existing retirement fence.
- Introduce the layout for new opted-in installs first. Keep current datasets
  on their existing path until migration and rollback have been verified.
  The typed storage benchmark supplies read-cost evidence for the managed
  layout. The shared values table still needs measurements against legacy JSON
  for point reads, selective and multi-field filters, writes, and storage.

## Typed-value implementation checkpoint

The SQL engines now provide the host opt-in `EnableTypedFields` for empty
datasets with a full schema grant. The implementation uses
`tessera_field_values` and the existing object table for identity. A typed
object stores no duplicate JSON record payload. The new SQL view assembles
a transient transport for the evaluator. Catalog publication, generated
managed-type tables, and the proposed admin methods remain design work.

Supported integer predicates are pushed down through a shared index, including
bounded positive conjunctions. Integer and timestamp orderings run in SQL
before the server assembles the returned records. Other valid predicates and
orderings can use the existing evaluator. JSON datasets retain their current
behavior. The v1 data and content API is unchanged.

The same synthetic v1 scenarios run on both layouts and engines. The typed
path is also covered by wire-value round trips, attachment access and
retirement, and encrypted backup and restore. The external catalog smoke
test exercises the existing routes with typed fields enabled.

### PostgreSQL measurements

The in-process v1 handler used one synthetic dataset per layout, with
10,000 records and four fields each. The additional decimal and reference
fixtures used 1,000 records each. The benchmark used one client and warm
local PostgreSQL, with 120 measured operations per core case and 30 per
compatibility query. The page case includes start, first page and cleanup.

| Operation | JSON p95 | Typed p95 |
| --- | ---: | ---: |
| Point read | 3.76 ms | 4.00 ms |
| Selective integer range | 87.58 ms | 5.03 ms |
| Two-field integer conjunction | 86.46 ms | 7.34 ms |
| Patch one field | 10.65 ms | 13.68 ms |
| Insert four fields | 8.99 ms | 10.35 ms |
| Inherited timestamp top ten | 21.28 ms | 38.15 ms |
| Exact grouped decimals | 10.84 ms | 16.17 ms |
| Reference-filtered page lifecycle | 20.65 ms | 24.92 ms |
| Scoped integer query | 89.41 ms | 4.42 ms |

The measured object and field relations, including indexes, used
5.37 MiB for JSON and 13.22 MiB for typed storage.
These sizes exclude WAL, metadata, and the additional query fixtures. They
describe this payload and workload. The benchmark does not establish
multi-tenant throughput.

The initial typed scan regressed on the two-field filter and inherited list.
Pushdown fixed the two-field scan and SQL ordering reduced the inherited
list cost by avoiding full record assembly for every candidate. Grouped
decimals and reference predicates still assemble records for evaluation.
The extra field rows and indexes account for the measured disk tradeoff.

Run with a disposable PostgreSQL 17 database using the same DSN convention
as the integration suite.

```sh
TESSERA_MEASURE_TYPED_FIELDS=1 go test ./pkg/server -run '^TestTypedFieldPerformance$' -count=1 -v
```

## Decisions for review

1. Confirm one shared physical layout per managed type revision family, with
   approved storage profiles selecting or adding shared indexes.
2. Confirm public catalog publication, private revision sharing, and
   destination installation as separate permissions.
3. Confirm an additive provisioning API and an unchanged v1 data API, with
   PostgreSQL and SQLite evaluated against the same compatibility baseline.
4. Confirm separate retire and delete operations, the default refusal for
   dependent types, and the fence and restore guarantees for a cascade.
5. Use typed rows as the sole custom-field storage for the new layout. Measure
   the read and write tradeoffs before deciding which shared indexes to ship.
