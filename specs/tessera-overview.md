# Tessera

**A protocol for schema-described record storage.**

Tessera stores structured records and answers structured questions about them. What makes it unusual
is that the *schema is data*, and so is the meaning attached to it — which means two programs that
have never heard of each other can read the same dataset and each understand the parts they care
about.

It is a contract, not a product. [The specification](tessera-protocol-v1.md) is normative and this
document is not; read this first, then that.

## The model, in a page

**A dataset** holds types and objects, and owns its own identity. A server may host many. Nothing
spans two datasets — not a transaction, not a query, not a consistent read.

**A type** (`ObjectMeta`) describes objects: an id, an optional parent, and fields. Types form
hierarchies, and a query against a parent returns its descendants, projected onto the parent's fields.
That is already a form of duck typing: a client that understands `reading` works with every descendant
it has never seen.

**A field** (`FieldMeta`) has an id, a storage type — text, number, date-time, reference, media, json
and so on — and a human name returned as stored.

**An object** (`ObjectRecord`) is one row of one concrete type, carrying values for the fields its type
declares and inherits. Object identities are unique within a *hierarchy*, so an operation naming one
object names its concrete type as well.

**A reference** is a value naming another object, qualified by the dataset that owns it, so a record
can point across datasets.

### The part that matters most

A type and a field may carry **roles** and **annotations**:

```json
{ "id": "_w", "type": "number", "name": "Weight",
  "roles": ["value", "body.mass"],
  "annotations": { "dimension": "mass", "unit_source": "record", "unit_field": "u" } }
```

**The server stores these and never reads them.** Tessera defines no role names and no annotation keys
— what they mean is agreed between the programs that use them, in a vocabulary this protocol does not
contain and cannot validate.

The point is what it removes. Without roles, a client finds the weight by looking for a field called
`weight`, so renaming the field is a migration, a second product cannot read your data at all, and two
datasets built by different people are mutually unintelligible. With roles, a client asks *which field
carries this role* and gets an answer whatever the field is called. **Meaning travels with the data.**

The honest cost: interop depends on a vocabulary nothing enforces. A typo produces a type nobody
recognizes, not an error. And a client decides for itself whether a type is usable — the failure mode
is *not recognized*, never *rejected*, which is what lets one dataset hold another product's types
without either program knowing about the other.

## The surface

| | |
| --- | --- |
| `GET /.well-known/tessera` | What this server is and how to authenticate. Unauthenticated. |
| `GET /v1/datasets` | The datasets this caller may use. One a caller may not use is **absent**, never listed as forbidden. |
| `GET /v1/datasets/{d}/meta` | Every type, with effective fields. Also `/meta/{t}`, `/meta/{t}/children`. |
| `POST /v1/datasets/{d}/meta` · `PUT` · `DELETE` | Create, change and remove a type. |
| `GET /v1/datasets/{d}/objects/{t}/{o}` | One object by identity. |
| `POST /v1/datasets/{d}/objects/{t}:query` | Structured query: predicates, sorting, projection, pagination. |
| `:count` · `:aggregate` · `:distinct-references` | Counting and summarising. The last two are optional. |
| `POST /v1/datasets/{d}:transact` | **Every write.** An ordered list of operations that all commit or none do. |
| `POST /v1/datasets/{d}/operations/{key}:status` | What happened to a write whose response was lost. |
| `POST /v1/datasets/{d}:erase` | Destroy a dataset's contents. Separately granted. |

Single-object writes have convenience forms — `POST` an object, `PATCH` it, `DELETE` it — each defined
as a transaction of exactly one operation, so the semantics exist in one place.

## Writing

Every write is a transaction, and **checks are operations** rather than conditions attached to
mutations:

```json
{ "operations": [
    { "op": "expect", "target": { "type": "com.example.plan", "id": "8121" },
      "version": "v1:9f2c…" },
    { "op": "insert", "type": "com.example.reading", "fields": { … }, "as": "entry" },
    { "op": "update", "target": { "type": "com.example.plan", "id": "8121" },
      "fields": { "latest": { "type": "reference", "value": "$entry" } } } ] }
```

Three things to notice. **Aliases** (`as` / `$entry`) let one transaction create an object and
reference it, which is impossible otherwise because the dataset assigns the identity. **A failure
names the operation that failed** by index, and nothing is written. And one grammar covers what would
otherwise be a family of operations — conditional insert, compare-and-set update, bulk insert — so
there is less to learn, not more.

## Knowing what changed

Two mechanisms, deliberately small.

**A watermark** rides on every read and every transaction: one opaque value per dataset that changes
when anything in it is committed. If several reads report the same watermark, they are mutually
consistent — which is how a client builds one answer out of six queries without publishing a mixture
of two states. It is also the version a cache validates against.

**A view version** tells a caller that what *it* may see has changed — a grant created, widened,
narrowed or revoked. It is separate because the other two describe the dataset, not the caller: gaining
access to a second type moves neither the data nor the schema, so without it a cheap poll shows nothing
and the caller never learns.

**A version** rides on each record you ask for: an opaque value derived from that record's current
values, so a write can depend on what was read. It is computed rather than stored, so a dataset of ten
thousand write-once GPS points pays nothing for a guard none of them will use.

There is no change feed, no per-object sequence and no tombstones. A watermark says *something
changed*, never *what*, and cannot be resumed from.

## Authorizing

| Method | For | Notes |
| --- | --- | --- |
| `oauth2` | A person using an application | Authorization code with PKCE, per [RFC 8252](https://www.rfc-editor.org/rfc/rfc8252.html). The normal case. |
| `bearer_token` | Automation with no browser | Scripts, CI, the conformance suite. Clients acting for a person should not use it. |
| `client_credentials` | A service acting as itself | Another product integrating against a dataset. Its grants are separable from any person's. |

**Grants are not scopes.** A token says who you are; what you may reach is server-side account state,
reported per dataset as `access` — `read`, `write`, `schema`, `erase`. Revoking a grant takes effect
without reissuing anything, which is why a client must handle a refusal at any moment rather than
trusting what discovery told it earlier.

**Sharing** — granting another principal access to a type or a single object — is optional, read-only
in v1, and costs nothing in a deployment that does not use it: an owner reading their own dataset is
subject to no view filtering at all.

## What a server must do, and what it may skip

The line is drawn by one question: **can a client do this itself?**

**Required**, because no client can reconstruct them: all-or-none transactions, preconditions evaluated
inside the transaction, durable idempotency, the watermark, erasure, stable effective metadata, reads
and queries, and the distinction between *not found*, *refused* and *not visible*.

**Optional**, because a client can: aggregation, distinct references, text matching,
attachments, sharing. A server advertises what it has and refuses the rest with `not_supported`; a
client either adapts or declines to connect.

## What it deliberately does not do

- **Workflow.** Tessera answers what is true, not what should happen next. No triggers, no rules, no
  scheduled work. Something else builds that on top.
- **Blobs.** Media values are opaque references; bytes live in the separate
  [content protocol](tessera-content-v1.md), and a record server is fully conformant without it.
- **Sync.** There is no change feed and no replication. The watermark tells a client to look again.
- **Cross-dataset anything.** No transaction, no query and no consistent read spans two datasets, and
  no protocol can offer that without a distributed commit.
- **Understand your data.** It will never validate a unit, know what a kilogram is, or refuse to add
  up numbers that should not be added. That is the client's contract with its own vocabulary, and
  making it the server's would end the property this whole design exists for.

## Where to go next

- [The specification](tessera-protocol-v1.md) — normative, and detailed enough to implement from.
- [The content protocol](tessera-content-v1.md) — attachments, resumable uploads, short-lived URIs.
- [Design decisions](tessera-decisions.md) — why it looks like this, including what was rejected.
- [The deployment contract](tessera-deployment.md) — what a deployment owes you outside the wire:
  provisioning, principals, grant administration, and what it cannot quietly redefine.
- [Embedding](tessera-embedding.md) — running the core inside a host application rather than beside it.
