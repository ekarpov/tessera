# SQLite development server

This is a durable, synthetic-data development deployment, not a fully conformant or production-ready
server. The SQL engine now supports pagination, erasure and optional content/sharing. The development
command remains synthetic-only. HTTPS authentication and deployment controls live in the separate
[self-hosted composition](self-hosting.md).

## Run

Install Go 1.27.0 and a C compiler; the pinned SQLite driver requires CGO. Then run:

```sh
make dev
```

The server listens on `127.0.0.1:8080`. It creates `.tessera-dev/dataset.db`, a private bearer-token
file at `.tessera-dev/token`, and neutral `com.example.note` / `com.example.entry` schemas with synthetic records.
The startup message identifies the credential file without printing the secret. Use its contents as
the bearer token in your client. The directory is ignored by Git. Keep it on a private local disk.

For another loopback address or directory:

```sh
go run ./cmd/tesserad -dev -listen 127.0.0.1:8081 -data-dir /private/tmp/tessera-example
```

The command refuses non-loopback listeners and requires `-dev`.

**It is served over `http`, and that is the one case a client is allowed to accept.** The
[protocol](../specs/tessera-protocol-v1.md#discovery) requires a client to refuse `http://` to a
Tessera server unless the host is `127.0.0.1`, `::1` or `localhost`. A client implementing that
refusal without the exemption cannot talk to this server at all — so **write the exemption's test
before the refusal's**, because a careless guard passes the test that proves it refuses a plaintext
LAN address and takes every development run down with it. A private address is not loopback; this
one is. Ctrl-C or SIGTERM drains requests
with a bounded shutdown deadline and closes the database. Restart retains the token, dataset ID,
metadata and value bytes, version secret, watermark, authorizations and recovery receipts. Existing
authorizations are not silently restored on restart. There is no production token issuer in this
command; embedded hosts provide authentication, including service credentials.

For the Swift record-and-attachment fixture, use `make dev-content`. Stop the server before
`make dev-reset`, which retires its data and seeds a fresh dataset while retaining the local token.
See the consuming client's integration handoff for the fixture, HTTP flows and independent
client gate. Existing directories are not reseeded on startup. The local synthetic token on a new or
reset dataset includes erase authority; existing authorizations keep their previous grants.

## Persistence and recovery

One dataset lives in one SQLite file. A process lock permits one owning process, and SQLite WAL
transactions serialize writes. Use a stable local path; do not alias the database with links or
access it from another program while the server owns it. The engine supports Unix process locks
and refuses unsupported platforms. Migrations are hand-written in `db/migrations`.

A keyed mutation first commits an owner-bound claim. The record/schema changes, host SQL changes
when supplied, and successful receipt then commit in one SQL transaction. Failure rolls back the
work before storing a definitive failed outcome. Startup can mark abandoned claims failed because
exclusive process ownership and atomic outcomes prove their data never committed. A crash after
commit preserves the original response; replay never executes the mutation again.

Receipt ownership is the principal plus application authorization, stored independently of grants.
Revocation does not remove recovery rights. Current dataset read access gates record-bearing historical bodies; schema-only receipts also accept
schema authority, and delete-only outcomes contain no values to withhold.
If a created object was deleted, recovery returns created identities with `result_withheld` instead.
Another principal or application cannot use its grants to read that receipt. An authenticated caller
can ask about an unknown key without dataset access.

`PruneReceipts` removes only terminal receipts older than the advertised retention window, measured
from claim time. The development command runs a cancellable maintenance worker once per minute; receipts may remain longer.
Unresolved claims are never pruned. Stored request identity is a digest of method, exact request
target and body bytes; the raw request is not stored. Successful replay preserves the original
watermark and response bytes.

## Bounds and embedding

The current SQLite engine reuses the memory engine's exact evaluator by materializing a bounded
dataset inside each transaction. It persists changed rows rather than rewriting the database.
`max_scanned_rows` bounds record count and `max_materialized_bytes` bounds serialized metadata and
record values (64 MiB by default). These are work/storage bounds, not a promise about total process
memory. Exceeding them fails atomically with `query_result_limit_exceeded`. Persisted limits survive
restart. Dataset listing reads schema and discovery state without loading record values.

Import `pkg/store/sqlite`, open a store, install trusted authorizations using `SetAuthorization`, and
pass it with a credential resolver to `server.New`. SQLite accepts full-dataset views or read-only type/object grants.
`WithTx` exposes `store.Tx` and the same `*sql.Tx` for host tables. `MutateWith` adds durable claims
and receipt outcomes around this shared unit. Its callback runs once for a fresh claim and never
for a replay; the digest must identify the host work as well as the protocol request.

Callbacks must return errors, perform no network I/O, never reenter the store, and never commit,
roll back, retain or concurrently use the supplied transaction. Host code may manage its own tables
but must not change Tessera's tables. These are trusted embedding APIs, not client-accessible SQL.
See `pkg/store/sqlite/composition_external_test.go` for a public-package composition example.

## Evidence

`make check` runs formatting, vet and race-enabled tests. SQLite runs the importable core and recovery
conformance subsets. Additional tests cover exact bytes across restart, concurrent claims, live
in-progress status, cancellation, revocation, schema-only recovery, retention, host rollback, and
actual child-process termination before and after commit. An HTTP test discards a committed response
and verifies retry creates no duplicate. These are subset gates, not full conformance certification.
