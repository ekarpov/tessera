# Client profiles

These descriptive profiles help adopters find the contract features they need. They are
not conformance levels, server configuration or promises about one implementation.
Profiles may overlap; an application with both a device and hosted service can use several.

## Quick guide

| Need | Profile |
| --- | --- |
| Personal data on a device | A |
| A service with its own exchange contract | B |
| A multi-tenant service | C |
| Add entries without reading or changing others | D |
| Read another person's shared data | E |
| Embed the Go engine | F |
| Combine data shared by group members | G |

The profiles below record needs and design boundaries. Use the
[feature matrix](feature-matrix.md) for current implementation support and
[consuming Tessera](consuming-tessera.md) for integration steps.

## A. Device-primary personal client

Ships an application, runs no server. The authoritative copy of a person's records is on their
device; a remote dataset is a **peer**, not a replica — nothing is synchronised, and the application
composes across several storages, reading each one's metadata through the same path it uses for a
type it has never seen.

| | |
| --- | --- |
| **Requires** | Dataset creation, because every path is addressed by an id that must already exist and a client may not invent one — so an account owning no dataset has nowhere to arrive on first sign-in. A recovery window measured in days, not hours: such an application does not control when it next runs, and past the window *it never happened* and *it happened and I may no longer ask* are indistinguishable. |
| **May decline** | Aggregation, sharing, content. One consumer reports its profile does not require aggregation at all, so it can speak to a minimal server. |
| **Watch for** | Deep type hierarchies — one consumer runs to sixteen levels, which makes inherited-constraint questions the common path rather than a corner case. Watermarks used as a compare-and-abort guard rather than a polling signal: this profile does not re-query on change. |
| **Blocked without** | Creation. Everything else it can work around. |

## B. Adjacent service with its own contract

Runs its own service, and its own protocol for records crossing a trust boundary. It may consume a
dataset as a store without adopting this contract for everything, and its chief interest is that the
boundary holds: proposal, review, correction, withdrawal and acknowledged cleanup are its protocol's
job, and importing them here would damage both.

| | |
| --- | --- |
| **Requires** | That guest writes stay out. Copy-back is its crossing: compose in your own dataset, grant read, let the recipient copy what it accepts and own it outright. |
| **May decline** | Everything optional. One such consumer implements no part of this contract today and is not blocked by anything in it. |
| **Watch for** | Client-side encryption of selected values. An encrypted numeric is not a numeric to the server, so it cannot be declared as one and aggregation over it is a type error — a per-field trade that is visible in the schema, not a capability a deployment switches off. |
| **Blocked without** | Nothing. This profile's risk is not a missing feature but a boundary quietly moving. |

## C. Multi-tenant host

Operates a deployment holding many owners' datasets — a service, a firm hosting its customers, an
institution holding participants' records.

| | |
| --- | --- |
| **Requires** | Many datasets in one database with writes that do not contend across them; provisioning per account; a dataset a caller may not use **absent** from the listing rather than refused. |
| **May decline** | Nothing structural, but its per-tenant surface is usually narrow. |
| **Watch for** | Quota and rate limiting, which this protocol cannot express and the [deployment contract](../specs/tessera-deployment.md) must name. Creation allocates storage to whoever holds the right. |
| **Blocked without** | Provisioning it can drive itself. Out-of-band provisioning works and is not portable. |

## D. Append-only contributor

Writes into a dataset it does not own and **retains no continuing relationship with what it wrote**.
A feed asserting a transaction, a meter or a sensor reporting a reading, a form endpoint accepting a
submission. The defining test is not who writes but whether the writer will ever need to revise it:
one that will belongs to profile B's protocol, not to this one.

| | |
| --- | --- |
| **Requires** | A right to insert that conveys no read, update or delete, and a reachable dataset to insert into — which on a device-primary deployment means a small server-side dataset the owner holds for receiving contributions. |
| **May decline** | Reading anything at all. It reconciles by receipt, not by lookup. |
| **Watch for** | What a receipt discloses. A committed receipt returns the resulting identity always, and the stored body only while its owner currently holds read authority — so this profile learns an identity and not a record, because it has no read authority rather than because one is withheld. Attachments are unresolved: content imports require write authority an insert right does not confer. |
| **Blocked without** | The insert right, which is proposed and not adopted. |

## E. Professional reader

Reads part of someone else's dataset under a grant, for a bounded engagement, and produces its own
output — a report, a set of adjustments, a conclusion.

| | |
| --- | --- |
| **Requires** | Sharing: grants scoped to a type or a single object, with expiry, revocable as server-side state. Views applied before grouping, aggregation, pagination and limits, so a count cannot disclose what a read may not return. |
| **May decline** | Writing into the other party's dataset — and should. Its output belongs in its own dataset, granted back, which is also where its retention obligations are met. |
| **Watch for** | Object-scoped grants are not discoverable; whoever grants must say what it granted. At engagement scale this becomes an operational surface of its own. |
| **Blocked without** | Nothing today. Its convenience gaps are held candidates, not blockers. |

## F. Embedding host

Imports the core and runs it in its own process rather than beside it, supplying the host interfaces.

| | |
| --- | --- |
| **Requires** | The seams rather than the wire: an authorized access context applied at the store boundary, current grants checked inside the write transaction, and a unit of work it can join. |
| **May decline** | HTTP. This profile can certify in-process with no listener. |
| **Watch for** | It is in Go. A native implementation in another language is not this profile — it implements the contract from scratch, and the conformance suite is what says whether it did. |
| **Blocked without** | Nothing; this path is implemented. |

## G. Co-owning group

A household, a team or a small organisation logging into **one dataset they jointly own**, usually on
a server one of them stands up. Several principals hold `write` on the same dataset; there are no
guests, and the combined view is the reason the arrangement exists at all.

This is **not** sharing, and reading it as sharing is the most likely way to get it wrong. Sharing is
grants to a principal other than the owner, read-only, filtered. Co-ownership is
[access](../specs/tessera-deployment.md) — a deployment establishing who holds `read`, `write`,
`schema` and `erase`, for as many principals as it likes. The owner's path is not filtered, so this
profile pays nothing for view logic and cannot use it either.

| | |
| --- | --- |
| **Requires** | One dataset, because nothing spans two: a combined figure over records in separate datasets cannot be asked for at all. Several principals holding `write` on it. **Aggregation**, since the combined view is the point — this is the profile that *cannot* talk to a minimal server, in contrast to A, which can. The watermark, to build one report out of several queries without publishing a mixture of two states. |
| **May decline** | Sharing entirely, and with it every view question. Guest reads, object grants, `unavailable`. |
| **Watch for** | Everything in the dataset is visible to every member — there is no per-member view inside a dataset you co-own, so anything private belongs in that member's own storage, composed alongside. `erase` can be withheld per principal, so one member standing the server up need not mean every member can destroy it. Members running different application versions write into the same dataset, which is the situation the *"a client cannot reconstruct that no other client wrote this"* argument was made for. |
| **Blocked without** | Nothing. It can be built today. What it wants and cannot have is **a record that says who wrote it** and **field rules enforced against a member whose client you do not control** — see below. |

## What no profile currently needs

Recorded here because the absence is a decision. A change feed, a directory of grants issued to a
caller, invariants over sets of rows, and enforced referential integrity were each assessed and each
has no profile asking for it. They are held rather than rejected: a named requirement from any
profile reopens one, and building for none of them is what the rule against speculative abstraction
exists to prevent.

**Two are no longer in that list, because G arrived.** A record carrying the identity of whoever
wrote it, and field rules — a range, a set of permitted values, a required value — enforced against
every writer rather than by each client against itself. Both had been assessed with no consumer and
held on that basis; a dataset with several equal writers and no single controlling application is the
consumer they lacked. That is a finding about the candidates, not a decision about them: it moves
them from *held for want of a consumer* to *assessable*, and the assessment happens where contract
changes happen.

## Implementation note: a profile is not a deployment

Nothing above names an implementation, and none of it should be read as describing this repository's
server. A profile says what a consumer needs *from the contract*; **any conformant implementation
satisfies it**, and there is no privileged one. [AGENTS.md](../AGENTS.md#invariants) puts it as an
invariant — *the specification is the contract, the conformance suite is the proof, and this server's
behaviour never defines correctness.*

This matters most for the question every adopter eventually asks: **where does each consumer's data
physically live?** The reference server currently keys every record table by dataset in one shared
schema, with the write lock scoped to a dataset. That is a layout, not a promise. A server placing
each dataset in its own database, its own shard, its own engine, or no SQL at all is equally
conformant, and an application cannot tell.

**The contract was shaped to leave that open.** *Nothing spans two datasets — not a transaction, not
a query, not a consistent read* reads as a limitation, and is also what makes per-dataset placement
possible: since no operation may ever join two datasets, an implementation may put each one wherever
it likes and forfeit nothing. A deployment that wants per-tenant restore, per-tenant blast radius or
per-tenant hardware takes that option without leaving the contract.

**What is fixed**, and is the whole of it: the wire and its guarantees. Discovery and the REST
surface; value encoding and error codes; all-or-none transactions with preconditions evaluated inside
them; durable idempotency bound to a payload digest, with status; one watermark per dataset and
computed record versions; erasure, with identities never reused; the three answers kept apart; the
required reads, declaring a bound and failing at it rather than omitting an operation; and, where
sharing is advertised, views computed *within*, before grouping, aggregation and pagination.

**What is free**: storage engine, schema, tenancy model, language, and how grants, provisioning and
quotas are administered — the last of which [the deployment contract](../specs/tessera-deployment.md)
owns rather than the protocol.

**What proves it**: `pkg/conformance` is importable and takes any HTTP server, and the
[frozen vectors](../specs/vectors/README.md) settle encoding independently of language. Four things
an implementer should know before starting:

- The fixture is expected to move before `v0.1.0`; building against it today is building against a
  moving harness.
- Passing the suite is not a conformance certificate. [The feature matrix](feature-matrix.md) says
  what is claimed and [the review](tessera-spec-review.md) what is not.
- Optional capabilities are promises: a client that requires sharing or content must refuse a
  server withholding it. Aggregation, distinct references and text matching are required reads,
  not optional capabilities.
- Per-dataset placement makes `GET /v1/datasets` a fan-out, and a guest holding grants across several
  owners reaches every one of them. Nothing in the contract breaks; it is simply the operation to
  design first rather than last.

Profile F is not this. F embeds the Go core and inherits its behaviour; an implementation written
from scratch in any language is the first level of adoption, and the suite is what says whether it
arrived.
