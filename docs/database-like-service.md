# Tessera as a database-like service

This assessment separates protocol constraints from reference-engine limitations. The
three questions are multi-tenant placement, server-visible values and cross-record
integrity. It records why some changes were made and others remain outside the contract.

## Quick guide

For current engine choices, read [architecture](architecture.md) and
[self-hosting](self-hosting.md). For integration, use [consuming Tessera](consuming-tessera.md).
The analysis below preserves the original objections and decisions; historical engine
limitations are not a description of the current server.

| Question | Current outcome |
| --- | --- |
| Many datasets and concurrent workers | Implemented with PostgreSQL; SQLite remains single-process |
| A server that cannot evaluate values | Cannot provide the required server-side query semantics |
| Referential integrity across records | Held pending a concrete consumer; no new contract here |

## 1. One box, one writer

> Tessera keeps records in one box per owner, and only one server can write to a box at a time.

The second half described the original SQLite composition. PostgreSQL now supports multiple
workers. The first half is not what the protocol says:
[the overview](../specs/tessera-overview.md) states that a dataset holds types and objects and
**a server may host many**. Nothing in the protocol assigns one dataset per owner, and nothing in
it requires a single writer.

The limit was entirely in the reference engine, and most of it is gone:

| Where | What it did | Now |
| --- | --- | --- |
| `db/migrations/001_postgres.sql` | `tessera_dataset` was a singleton row — `CHECK ("singleton" = 1)` | Keyed by identity since `005_postgres.sql`; a database holds as many datasets as it is given |
| `internal/sqlengine/pgdriver.go` | `pg_advisory_xact_lock` on every record write: one writer, globally | The engine takes it per dataset, where the dataset is known; the driver takes nothing |
| `internal/sqlengine/postgres.go` | `pg_try_advisory_lock` on open: one process per database | Gone. Each process holds a session lock under its own executor identity, and a claim records the executor that made it |
| — | This row is PostgreSQL's alone. SQLite has no equivalent Tessera lock; its writes serialize on the database file | Unchanged, so writes to different datasets in one SQLite database still wait for each other |
| [self-hosting](self-hosting.md#postgresql) | States the consequence: one dataset per database, one process at a time | One process at a time, over as many datasets as that process holds |

the profile-B service measured what that costs rather than inferring it: concurrency buys no throughput on one
dataset and the slowest write takes the whole window, while separate datasets do proceed in
parallel. The ceiling is per dataset, not global — which is the useful half of the finding, because
it says the serialization is not load-bearing across tenants.

So the real chain was: writes serialize per dataset, the engine pins one dataset per database,
therefore one database and one process per tenant. The middle link is broken — many datasets per
database, and a write lock scoped to a dataset rather than to the connection — and doing it changed
no normative text and invalidated no conformance case, as predicted. Writes to different datasets in
one PostgreSQL database now proceed in parallel, which a test holds open and checks rather than
asserting.

The process lock was not the same kind of problem, and was not removed the same way. It was not there
to serialize writes; it was there so that a starting process could not resolve another process's live
receipt claims as abandoned, which startup did for every `in_progress` row. A claim now records the
executor that made it, each process holds a session lock under its own identity, and one process
decides another's claims are abandoned by taking that executor's lock — which succeeds exactly when
the session that held it ended. No clock is consulted, because a slow executor and a dead one look
the same to one.

So nothing in the chain remains on PostgreSQL. SQLite keeps one process per database, by an exclusive
lock on a file beside it, and that is a decision rather than a leftover: the alternative was a
timestamped lease, which resolves live work as failed whenever an executor is slower than the lease.

**What must not change with it.** Nothing spans two datasets: not a transaction, not a query, not a
consistent read. That one is contract and should stay, for the reason the overview already gives —
no protocol offers it without a distributed commit. A workspace needing atomicity across all of
itself is therefore one dataset, which is fine once one database can hold many and more than one
process can write.

## 2. Reading your text

> Tessera needs text readable so it can search and filter.

True, and [backend options](backend-options.md#where-the-protocol-actually-constrains-encryption)
already states it more sharply than the objection does: the protocol's only requirement is that the
server can evaluate values, and encryption the server cannot see through "would require a different
protocol, not a flag." Filtering, sorting, aggregation and distinct references are all defined as
work the backend does without shipping records to the client. A server that cannot compare two
values cannot implement them.

Where the objection overreaches is in treating this as all-or-nothing. the profile-B service does not encrypt a
workspace; it encrypts *fields*, through `internal/platform/securedata` with AES-GCM. Tessera's
constraint is per-field too, and the two line up better than the objection allows:

- **Free text stays opaque.** Media is never queried, so it can be client-encrypted at no capability
  cost at all. Backend options names this as the one place the usual trade-off does not apply.
- **Structured fields become readable, and therefore searchable.** Dates, status, tags and the
  client reference are what a filter actually runs against.

**This is not a retreat the profile-B service would have to accept — it is what the profile-B service already runs.** An earlier
version of this document called it one, and the profile-B service corrected it: in
`db/migrations/00002_client_timeline.sql` the columns `kind`, `visibility`, `status`, `due_at`,
`occurred_at`, `source_type`, `source_timestamp` and `client_id` are plaintext and indexed, and only
`content_ciphertext` is encrypted.

So the objection dissolves — but it dissolves symmetrically, and that is the part worth keeping.
Adopting Tessera would **reproduce** the split the profile-B service has rather than improve on it. Its own
concession that search was the one real thing Tessera would give it does not stand, and neither does
this document's earlier repetition of it. Free-text search over encrypted content is unsolved in both
designs; Tessera relocates that choice rather than resolving it.

**One residual is real**, and it is the only capability either client identified as genuinely
missing: the profile-B service encrypts measurement values, so it can filter by track but not by value. Range
filtering and aggregation over numbers would be new to it. That is squarely what Tessera's optional
reads already do — and it needs the same readable-at-rest decision, so it is a gain conditional on
the same choice rather than one that comes free.

**Nothing to adopt.** The field-by-field split works on the contract as written. Pursuing
server-blind encryption instead would mean the client fetching and filtering locally, which the
contract forbids as unbounded materialisation — a different protocol, as backend options says.

## 3. Holding the pieces together

> A plan points at goals, an observation points at where it came from, and deleting a client has to
> sweep all of it. Tessera keeps records in a box — it doesn't enforce those links.

Accurate on both sides. the profile-B service's schema carries 24 foreign-key references and 17 `ON DELETE CASCADE`
clauses, which is why its delete and export are provably complete. (An earlier version of this
document said 22, which was an arithmetic error; the profile-B service supplied the correct figure and confirmed
the cascade count was exact.) Tessera's `reference` is an inert
triple of `dataset_id`, `object_id` and `meta_id`: nothing validates that the target exists, and
nothing sweeps referrers when an object is deleted. The `cascade` the protocol does define is
schema-level, on `delete_type`, and is a different thing.

It is tempting to rule this out as vocabulary. It is not. **A `reference` is a storage type the
protocol itself defines**, and enforcing that one resolves is not the server understanding your data
— it is the server keeping a typed value honest. Roles and annotations stay inert; this is a
different category, and the distinction is worth holding precisely because it is the one that keeps
the vocabulary rule meaningful.

**What the objection was really about, on the profile-B service's own correction: migration, not greenfield.**
Asked whether rebuilding the glue is genuinely expensive given that a dataset already provides an
all-or-none transaction to sweep in, the profile-B service answered that inside one dataset it is cheap. The cost
it was describing is losing enforcement it already has — a regression for an existing workspace,
where a new dataset has none to lose. That narrows who this capability is for considerably, and it
is worth stating before anyone builds it.

A version that fits the existing grain would be optional, declared on the field, advertised as a
capability, and evaluated inside the transaction where a dataset already guarantees atomicity:

```json
{ "id": "_c", "type": "reference", "name": "Client",
  "on_missing": "reject", "on_delete": "cascade" }
```

That is the same shape `delete_type` already uses: opt-in, explicit, and confirmed because the
damage is invisible. The honest costs, which belong in any proposal that follows:

- **Same-dataset only means exempt, not restricted.** A cross-dataset reference in an enforcing
  field is *not checked*; it is not rejected. The alternative reading — that declaring `on_missing`
  restricts a field to same-dataset values — is rejected, because it would create two kinds of
  reference field, it would have the server refuse values it merely cannot verify, and the
  restriction would surface only at write time. The cost of the reading taken is that the guarantee
  is partial and varies per value, which the capability must state rather than let a client assume.
  Cross-dataset enforcement is impossible anyway: it needs a distributed commit.
- **Enabling `on_delete` on a populated field requires `confirm`.** Both settings change what
  already-stored records are permitted to mean — `reject` forbids a deletion that was allowed
  yesterday, `null` silently stops a record saying what it said. That is the same hazard as removing
  a declared field, which already requires `confirm` equal to the type ID, and it takes the same
  answer.
- **Sweeps are bounded work inside one transaction**, so a cascade has a declared ceiling and fails
  at it rather than running unbounded, exactly as every other bound here behaves.
- **Tessera still has no opinion about which cascade is right.** Whether deleting a client should
  remove its plans is the client's contract with its own vocabulary.
- **`cascade` is doubtful and may not be worth defining.** the profile-A client reports it as harmful on a
  provenance reference, where a sender's deletion would reach into a recipient's dataset — exactly
  what the copy-values rule exists to prevent — and sufficient nowhere. Restrict-and-null covers
  every case either client could name. Defining `cascade` needs a consumer that asks for it.
- **It is a capability, so a server may refuse it**, and a client that requires it must be able to
  decline to connect.

Per [contributing](../CONTRIBUTING.md), none of this is a decision until it reaches
[the review](tessera-spec-review.md) as a finding with a conformance case behind it.

## What the clients say

Both clients were asked, because every proposal above was derived from one client's needs and
[AGENTS.md](../AGENTS.md) puts them on equal terms. The question put to each was not whether they
approved but whether anything here would **break** what they already do — a client's need does not
decide the contract, a client's breakage does.

**the profile-A client — no objection to any of the three, and would use none of them today.**
Recorded in the profile-A client's backend repository at
`docs/database-like-service-response.md`, commit `e9a6481`.

- It offered no measurements, and said so rather than inferring: there is no deployment, `deploy/`
  holds only an example configuration, and nothing has run on real data. That is the right answer.
  A second round of inference would have subtracted.
- **Multi-dataset breaks nothing it can check.** Its client selects a dataset by configured
  identity rather than by position, so a list with several entries does not confuse it.
- **Referential integrity: both its reference roles are designed to dangle.** `delivery.source` must
  be allowed to break, for the reason [design decisions](../specs/tessera-decisions.md) already
  gives — a copy carries values, not references into the sender's dataset, or revocation empties the
  plan. `delivery.supersedes` is the same-dataset case and still wants to dangle.
- **It found the ambiguity that mattered**, in "same-dataset only", and asked which reading was
  meant before it could be specified. Both readings above are its framing; the resolution is
  recorded in the proposal rather than here.
- **`cascade`: no, and harmful on one field.** See the proposal.
- **Cross-type role query: no, with the concrete question asked and answered.** Asked to name a
  query it cannot express today, it reported there isn't one. Its roles are field roles inside one
  type's schema, resolved over metadata the app has already loaded.
- Its own caveat, which it volunteered: deliveries are unimplemented, so section 2 of its answer is
  design evidence and should be weighed below the profile-B service's 22 foreign keys.

**the profile-B service — no, and it would not change its decision if all three shipped.**
Recorded in the profile-B service's repository at `docs/tessera-database-service-reply.md`,
commit `82e48eb`.

Asked directly whether the working assumption was right, it confirmed it without hedging: the three
objections were accurate descriptions of limits noticed while declining for an unrelated reason. It
parked the track because nothing demands it, not because Tessera cannot do it.

- **Build item 1 for Tessera's own reasons.** Its words: an untestable requirement beats a consumer
  who has not committed to anything.
- **It corrected this document twice**, on the encryption split and on the foreign-key count. Both
  corrections are folded into the sections above, and the first one removed an argument this
  document had been making in Tessera's favour.
- **Referential integrity is not the hinge and never was.** It asked explicitly that the capability
  not be built for the profile-B service, while endorsing the principled argument for it — that a `reference` is a
  storage type this protocol defines, and keeping a typed value honest is the server's job.
- **Multi-dataset alone removes an operational absurdity and is not a reason to host.** Hosting
  would still need a registry, provisioning, routing and an owner-identity story it does not have.
- **The no-write-grant rule is not its obstacle, and it thinks the rule is right.** One shape it
  forbids outright; the other is blocked instead by owner identity, since a person owning a dataset
  the profile-B service hosts would need an the profile-B service account, and the profile-B service's rule is that clients need none. The shape
  that works today needs neither: records stay at the person's own host, which grants the profile-B service `read`
  on a sharing collection.

**What would change its decision is still demand-side**, and unchanged from its parking note. The
one thing that would make it re-run the arithmetic rather than the decision is a stable Tessera
release arriving together with a product need for value-range filtering or aggregation over
measurements — the residual identified under objection 2. It reports that is not on its roadmap and
that it would say so if it appeared.

## The decision

"A database-like service with flexible metadata on top" is coherent, and Tessera is nearer to it
than the three objections suggest. What decides the order is that **neither spec item gets more
expensive by waiting** — a capability is advertised, and role query is already recorded as purely
additive later — so nothing here has a deadline, and the only reason to build either now would be a
consumer asking. Neither client is.

**1. Many datasets per database and dataset-scoped PostgreSQL write locks: implemented.**

See [current engine support](feature-matrix.md#engines). Multi-dataset composition must
also reject two datasets naming one logical content store; the composed regression is
`TestTwoDatasetsNeverNameOneContentStore`.

**2. Optional referential integrity. Hold, pending a consumer.**

The largest of the three and the only one carrying contract risk. **It now has no consumer at all.**
the profile-A client will not use it and found a load-bearing ambiguity in it; the profile-B service, whose measurements
produced the objection, has asked that it not be built for the profile-B service and clarified that the cost it
described was losing enforcement on an existing workspace rather than an obstacle to a new one.

What survives is the principled argument, which the profile-B service endorsed while withdrawing the demand: a
`reference` is a storage type this protocol defines, and keeping a typed value honest is the
server's job. That is a reason it would be defensible, not a reason to build it now. Resolving the
ambiguity, as the proposal above now does, costs nothing and is worth having recorded either way.
Building the capability for nobody is what the rule against speculative abstraction exists to
prevent.

**3. Querying across types by role. Closed, not deferred.**

Both clients were asked and neither wants it. the profile-A client was asked for a query it cannot express today
and reported there isn't one; the profile-B service never raised it. [Design
decisions](../specs/tessera-decisions.md) already names the cost — a union over heterogeneous types,
with projection, sorting and pagination all to be answered — against a documented client-side
alternative that works. Reopen only on a named query that the alternative cannot answer.

## One blocker not in the list

[The deployment contract](../specs/tessera-deployment.md) says no grant conveys write in v1, so a
service cannot be granted write on a person's dataset. That is fine where a deployment owns the
workspace outright. It blocks the sharing-store shape, where a professional writes into a dataset
belonging to someone else — and it blocks it deliberately, since v1 sharing is read-only guests with
exact type grants and not a delegation model. Anything wanting delegated, scoped, revocable
authority over someone else's records is the exchange's job, per
[relationship to an adjacent exchange](storage-and-exchange.md).
