---
name: go-storage-service
description: Build or review the Go storage server and its SQLite and PostgreSQL engines, including HTTP handlers, query execution, migrations, cursors, idempotency, concurrency, testing and service lifecycle. Use for server or engine implementation; do not use for specification wording.
---

# Go Storage Service

Adapted from a client's `go-postgres-service` skill; the Go and PostgreSQL boundaries below are
largely its rules, which apply unchanged to a different domain. Protocol rules live in
the repository guidance in `AGENTS.md`.

Apply [repository invariants](../../../AGENTS.md#invariants) and
[execution policy](../../../AGENTS.md#agent-execution-policy). Use the
[package map](../../../docs/architecture.md#package-map) to locate the affected boundary; consult
only relevant specification sections and the sections below. Reuse guidance as directed by
[working context](../../../AGENTS.md#working-context).

For Windows shell work, load [operations: Windows shell](references/operations.md#windows-shell).
For development commands or CI, load [operations: commands and CI](references/operations.md#commands-and-ci).
Server release preconditions remain in
[releasing](../../../docs/releasing.md#preconditions).

## Go boundaries

- Follow [Go style and lifecycle rules](../../../CONTRIBUTING.md#style). Prefer explicit interfaces
  at external boundaries, rather than one per type.
- Keep `cmd/tesserad` limited to configuration, composition, signal handling and exit status.
- Put protocol types, engines, server handlers and the admin UI in focused packages. `pkg/` is what
  another implementation may import — the protocol types and the conformance suite — and its
  compatibility is a promise. `internal/` is this server's own business.
- Accept `context.Context` at every blocking boundary, and make goroutines owned, cancellable and
  joined during shutdown.
- Bound queues and background work. A slow client must never hold a connection or a transaction open
  on someone else's behalf.
- Prefer standard-library HTTP primitives. Add a router or framework only when it materially reduces
  complexity.
- Return structured protocol errors at the HTTP boundary without leaking driver messages, SQL text,
  field values or credentials. The error code is the contract; the message is diagnostic.

## Engine boundaries

- One engine interface, two implementations. Anything a query needs that an engine cannot do is the
  server's work to finish in its own process — never a reason to advertise less.
- Follow [hand-written migration requirements](../../../CONTRIBUTING.md#what-a-change-is-expected-to-carry);
  make forward migrations transactional where the engine permits it.
- **Quote every identifier.** The specification's reserved-word rule exists for implementations that
  forget; it does not license skipping quoting here.
- Use `timestamptz` and preserve millisecond precision exactly. A stored value that returns different
  from what was written is a conformance failure, not a rounding detail.
- Never emit `= NULL` or bind null to `=`. Exact-match-against-null is `IS NULL`, and its negation is
  `IS NOT NULL`; the SQL `UNKNOWN` result would drop rows silently.
- Order nulls explicitly. The protocol requires nulls before values ascending and after them
  descending, which is the opposite of PostgreSQL's default for ascending order.
- Compare object identities as text, by code point, even where the engine stores integers.
- Append the object identity ascending as the final tie-break of every ordering, so a result is
  totally ordered and a cursor cannot drift.
- Make idempotent operations idempotent in the database, not in a handler: the record binding a key to
  its result commits in the same transaction as the write it describes.
- Keep transactions short and free of network calls.
- Index what authorized queries actually traverse, and justify every additional index with a plan or a
  measurement rather than an intuition.

## Cursors

- Opaque and signed, carrying the validated query and position. A client must never be able to alter
  one, and must never need to parse one.
- Reject a tampered or unrecognised cursor as `invalid_cursor` and an expired one as `cursor_expired`,
  both `410`.
- Continuation stays correct while rows change underneath: a row present throughout is returned
  exactly once, and deletions must not cause later rows to be skipped.

## Verification

Follow [contribution requirements](../../../CONTRIBUTING.md#what-a-change-is-expected-to-carry):
use the narrowest useful tests, with table-driven unit tests for validation/encoding,
integration tests for engines, and the conformance suite as the protocol gate. Negative cases include
concealed objects, replayed/conflicting keys, mismatched expectations, expired cursors, oversized
predicates and enforcement of each declared bound. Run `-race` for concurrency changes; never replace
missing fixtures with real data.

Prefer incremental builds and focused checks appropriate to the change. Reuse successful results
only while source inputs, configuration and external dependencies remain valid; PostgreSQL state can
invalidate a cached result (see the Makefile's `-count=1` rationale). Broaden for shared changes,
failures, unresolved risks and required project gates. Do not rerun unchanged successful checks just
to refresh them; never weaken required gates or claim skipped coverage. Use the
[operations reference](references/operations.md#commands-and-ci) for commands and coverage limits.
Documentation-only edits require link/anchor, consistency, duplication and requirement-preservation
checks, not unrelated builds or tests.
