# Reference server architecture

Tessera separates protocol types, authorized storage and HTTP composition. SQLite and
PostgreSQL share execution semantics; standalone identity and administration are outside
the reusable protocol core.

## Quick guide

Follow an HTTP operation from `pkg/server` through `pkg/store` into `internal/sqlengine`.
Read [the embedding contract](../specs/tessera-embedding.md) before adding a host integration,
and the [record protocol](../specs/tessera-protocol-v1.md) before changing observable behavior.
For a runnable composition example, use
[the external-package SQLite test](../pkg/store/sqlite/composition_external_test.go).

## Package map

| Path | Responsibility |
| --- | --- |
| `specs/`, `specs/vectors/` | Contracts and frozen portable examples |
| `pkg/protocol/` | Values, wire encoding, metadata and request validation |
| `pkg/conformance/` | Importable behavioral HTTP suites |
| `pkg/server/` | Reusable HTTP handlers and host authentication composition |
| `pkg/store/` | Authorized operations and transaction interfaces |
| `pkg/store/memory/` | Synthetic, non-durable engine |
| `pkg/store/sqlite/`, `pkg/store/postgres/` | Public durable-engine entry points |
| `internal/sqlengine/` | Shared SQL execution, receipts, paging and content authority |
| `internal/recordstate/` | Shared bounded record evaluator |
| `internal/identity/` | Standalone credentials, OAuth and account authority |
| `internal/admin/` | Server-rendered administration and administrative APIs |
| `internal/standalone/` | Self-hosted composition and lifecycle |
| `internal/devserver/`, `internal/devfaults/` | Synthetic development and fault injection |
| `cmd/tesserad/` | Configuration, composition and process entry point |
| `db/migrations/` | Hand-reviewed forward SQL migrations |
| `deploy/` | Example operator configuration and deployment assets |

## Request and transaction flow

The host resolves a credential into a principal and application authorization. Store
operations check current authority; request parameters cannot supply trusted grants.
Full-dataset and read-only shared views follow the contract's different visibility rules.
Grant changes invalidate access and paging as required, independently of data watermarks.

HTTP mutation conveniences use the ordered transaction executor. Operations see earlier
operations in the same transaction; the first failure aborts the whole transaction and
reports its index. Durable receipts commit with the changes, so dropped responses can
be reconciled without guessing. Erasure retires the dataset identity and preserves an
independent restore fence.

The host may join its own SQL work through `WithTx`/`MutateWith`. Callbacks must return
errors, perform no network I/O, retain no transaction and tolerate being run again.
See [consuming Tessera](consuming-tessera.md#join-tessera-writes-to-your-own-tables)
for the savepoint/re-evaluation rules.

## Deployment boundaries

SQLite uses an exclusive process lock; PostgreSQL uses dataset-scoped write locks and
executor session locks. Multiple PostgreSQL workers do not make independent identity
registries one authority. Coherent online archives require the
[supported registry and filesystem arrangement](backup-acquisition.md).

Identity pages and administration use embedded templates/assets. Operator branding is
bounded configuration; client names/icons are untrusted registration metadata. First-party
Console policy requires operator provisioning and exact origin/callback binding. Ordinary
OAuth retains its consent semantics. See [self-hosting](self-hosting.md).

## Extending a boundary

Keep application vocabulary, purpose-scoped consent and cross-service workflows outside
this repository. Roles, annotations, JSON and media remain opaque. Use
`protocol.MarshalWire` at wire boundaries to preserve annotation JSON bytes; ordinary
`encoding/json.Marshal` compacts raw JSON. Consult the
[agent extension workflow](../.agents/guides/extending.md) for change placement and checks.
