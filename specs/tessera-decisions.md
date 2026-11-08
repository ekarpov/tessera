# Tessera — design decisions

Running record of what is settled and what is not. Decisions from the design discussion of
2026-09-17 unless noted. This is not a specification; it is the reasoning the specifications are
written from.

## Why these decisions look like this

Most of what follows came out of eight recurring arguments rather than eight hundred separate ones.
Each individual decision carries its own reasoning below; these are the through-lines, and they are
worth checking a *new* proposal against before adding it.

**1. The storage never interprets what it holds.** Roles, annotations, media bytes and `json` are all
opaque: stored, returned, never read. This is the single thing that lets two unrelated products share
one contract, because the moment the server understands a domain concept it belongs to whoever put it
there. It is also expensive on purpose — it forbids role queries, server-side unit awareness, and a
server-side guardrail against aggregating mixed units.

**2. If a client can do it itself, it is optional; if it cannot, it is required.** The baseline rule.
A server that cannot aggregate is still a Tessera server, because a client can load rows and add them
up. A server that does not commit all-or-none is not, because no client can fake that.

**3. If the server must enforce it, it is protocol; if it is inert, it is vocabulary.** The placement
rule. A grant is checked on every read, so it is protocol. A grant *request* has no power until
someone approves it, so it is an ordinary object with an agreed role. This is what stopped requests,
approvals and delivery from becoming protocol surface.

**4. Nothing is built before it has a caller.** The expensive lesson of this project: an erasure
lifecycle grew handles, status routes, recovery owners and retention rules to serve a client that
structurally could not call them. Deleting it was the largest single improvement to the spec. The same
test demoted the split content authority to an annex, deferred `expect_type`, made sharing an
advertised capability, and keeps the change feed out.

**5. Meaning never changes in place.** A record keeps what it meant when it was written. Records carry
no annotation version, so editing an annotation applies retroactively to everything ever stored —
which is why units live in records, why corrections add a field rather than edit one, and why
`deprecated` means *closed for writing, open for reading*.

**6. Prefer a mechanism that enforces itself over one that needs discipline.** A `revision` field
works only if every writer remembers to bump it, and a writer that forgets leaves a guard that looks
alive and protects nothing — which is unknowable in a dataset another product can write. Expecting
real values is guarded by the data itself; a computed version is guarded by the server. Same reasoning
put the dataset's annotations above the app's catalog.

**7. Say what is true, not what is convenient.** An erase returns *access revoked and deletion durably
scheduled*, not *deleted*. A version mismatch means *re-read*, not *someone edited this*. There is no
snapshot across datasets, links already issued outlive revocation, and copies already downloaded are
beyond reach. Each of those is a sentence someone would rather not write, and each prevents a product
claim that cannot be kept.

**8. The cost falls on whoever needs the feature.** The owner's path does no view filtering because
sharing is someone else's feature. Versions are opt-in on queries so a ten-thousand-point GPS fetch
pays nothing. Optional capabilities are advertised rather than assumed. A feature nobody uses should
be free.

## Name and layout

- The storage protocol is generalized out of the profile-A client. **Tessera** is not a the profile-A client protocol;
  both consumers are both clients, and this repository is the reference implementation with no
  the profile-A client domain concepts in it.
- Specs live in `specs/`: `tessera-protocol-v1.md`, `tessera-content-v1.md`, and the
  remote-content-authority annex. Discovery is `/.well-known/tessera`.
- **Meta** stays the internal word for a schema; the data model is unchanged.

## Two documents

- **Human overview** — what it is, the model in a page, the REST surface as a table, the
  authorization options compared, one worked example, and what it deliberately does not do.
- **Normative spec** — for an agent to implement or integrate against.
- No Go, no PostgreSQL, no implementation detail in either. Pure API specifications.

## Transactions

Hard requirement.

> **The examples in this section are the sketches the design was argued from, not current grammar.**
> They predate `guards`, the predicate shape and the operation list as published. The protocol
> specification is the contract; read these for the reasoning and nothing else.

- One endpoint, `POST /v1/datasets/{d}:transact`, carrying an ordered list of operations, all-or-none,
  with one `Idempotency-Key` for the whole request. The existing byte-exact request identity applies
  unchanged.
- Per-operation preconditions replace `:update-atomic` and `:delete-atomic`.
- **Local aliases are required.** An operation may name itself (`"as": "parent"`) and a later one
  reference it (`"$parent"`). Creating a parent and a child that references it is the main reason to
  want a transaction, and ids are assigned by the storage.
- Errors name the failing operation index. One dataset per transaction.
- Every existing write verb is defined as sugar for a one-operation transaction, so the semantics are
  written once and the surface shrinks.
- **No interactive transactions** (begin/commit across requests) — locks held across round trips and
  abandoned-transaction cleanup.
- **Schema operations may join a transaction.** Decided: accepting a delivered plan is install-type
  plus insert, and half of that is a type nobody asked for or an object that cannot exist.

### Shape

**The resource is renamed `datasets`.** `/v1/datasets/{d}/...` throughout, replacing `storages`.
"Storage" reads like bytes, which is what Tessera is not, and the spec already used "dataset" in prose
for the same thing.

**Checks are operations.** Rather than a precondition object hanging off each mutation, assertions sit
in the list alongside mutations, and every entry either passes or the transaction fails at that index:

```json
POST /v1/datasets/{d}:transact        Idempotency-Key: 9f2c…
{ "operations": [
    { "op": "expect_absent", "type": "com.example.reading",
      "filter": { "field": "recorded_at", "eq": "2026-09-17T08:00:00.000Z" } },
    { "op": "insert", "type": "com.example.reading", "fields": { }, "as": "entry" },
    { "op": "expect", "target": { "type": "com.example.plan", "id": "8121" },
      "fields": { "revision": 4 } },
    { "op": "update", "target": { "type": "com.example.plan", "id": "8121" },
      "fields": { "latest": { "type": "reference", "value": "$entry" } } } ] }
```

One grammar replaces conditional insert, `:update-atomic`, `:delete-atomic` and `:batch-insert`. It
costs some verbosity — a target repeated between check and mutation — and buys a check that can stand
alone, and an error that names an index without a second addressing scheme.

The alternative was **preconditions hanging off each mutation** — terser, since the target is written
once. Rejected because it needs two grammars: an `expected_fields` object on targeted operations, plus
something else entirely for an absence check that names a type and a filter rather than a row. Checks
as operations is one grammar, evaluated in order like everything else, and a failure names an index
that already exists rather than inventing a way to address "the precondition of operation 3".

- **Operations**: `insert`, `update`, `delete`, `update_where`, `delete_where`, `define_type`,
  `delete_type`, `expect`, `expect_absent`.
- **Applied in order, and later operations see earlier ones' effects**, so insert-then-update of the
  same object works.
- **Aliases** are request-scoped, `$`-prefixed so they cannot collide with an id, must be defined
  before use, and are valid anywhere a reference value or a target id appears.
- **Bounds**: one dataset, at most 256 operations, the existing `max_request_bytes`, one
  `Idempotency-Key` for the whole request under the byte-exact request identity already specified.

**Response** carries per-operation results in order, plus the new watermark:

```json
{ "results": [ {}, { "objects": [ { "id": "9004", "meta_id": "…" } ] }, {}, { "affected": 1 } ],
  "watermark": "w:8f21…" }
```

### Errors from a transaction

- **The first failure, in order, and nothing after it.** All-or-none makes evaluating the rest
  pointless, and continuing would report from a state that does not exist.
- **`operation_index` sits beside `code`**, not inside `details`, because it applies to every code
  while `details` is documented as per-code.
- **The HTTP status is whatever that operation would have returned alone** — `409` for a failed check,
  `404` for a missing target, `403` for permission, `400` for a malformed field — so existing client
  mapping carries over unchanged.
- A failed `expect` reuses today's `:update-atomic` conflict body verbatim, `mismatched_fields` and
  all. Nothing new is invented.
- **A failure body carries no `results`**, so partial application is never ambiguous.

### The idempotency trap

By the existing rules, framing, authentication, authorization and parsing failures happen **before** a
key is claimed, so they leave no record and are not replayable — a malformed operation, an unknown op
kind, an alias used before definition. A **check mismatch happens during execution**, so the key is
claimed and the failure is replayable: send the same key again and the same conflict comes back
forever.

Which means the natural instinct — re-read, fix the expectation, retry with the same key — fails with
`idempotency_key_reused`, because the body changed. **Correcting a failed expectation requires a new
key.**

```
POST …:transact   Key: aaaa-1111   expect revision 4   → 409 conflict, mismatched_fields ["revision"]
POST …:transact   Key: aaaa-1111   (identical body)    → 409 the same conflict, Idempotency-Replayed
POST …:transact   Key: aaaa-1111   expect revision 5   → 409 conflict, details.reason
                                                          "idempotency_key_reused"
POST …:transact   Key: bbbb-2222   expect revision 5   → 200
```

**Keys and versions answer different questions**, and conflating them is easy because both sound like
"did something change":

- An **idempotency key** identifies *one attempt by this client*. It exists so a **lost response** does
  not become a second write.
- A **version** identifies *the state of a record on the server*. It exists for compare-and-set.

Which yields a simplification worth stating in the spec: **a guarded update is already idempotent.**
If the transaction committed and the response was lost, the record's version has moved, so a blind
retry with the old expected version fails `409`, the client re-reads, and finds its own change already
applied — no duplicate, no key required.

**The key is for creation.** An insert has no prior version to expect, so a lost response plus a retry
means two objects. `expect_absent` substitutes only where some field combination uniquely identifies
the row. So: **a transaction containing an insert carries an idempotency key; a transaction of guarded
updates need not.**

The trap below therefore applies only where a key was used — and there the reasoning holds
conceptually too, since after a version conflict the client is making a genuinely *different* attempt
with a different expectation, not retrying the first one.

**A key identifies one attempt, not one intention.** Both failures above carry the code `conflict`, so
a client distinguishes them by `details.reason` — that fork is where the wrong branch loops forever,
and it belongs where a client author will read it rather than in the idempotency section.

**`revision` in these examples is an ordinary application field, not something Tessera maintains.**
There is no per-object version, no ETag: the watermark is per dataset. Compare-and-set works against
whatever fields the caller names, which may be a dedicated `revision` the app increments — the pattern
`TrackDefinition.revision` and `TrackDefinitionDraft.expected` already use — or simply the field being
replaced, which needs no extra schema but only detects conflicts on that field.

### Schema operations

- **`define_type` is create-only.** A concurrent creation of the same type fails loudly with
  `meta_type_already_exists`, and the client re-reads and applies its reuse policy. If it meant
  "create or replace", a race would silently overwrite another client's roles and annotations.
- **`update_type` is separate**, for deliberate change.
- **`expect_type` is deferred.** ***Superseded — it shipped in v1, against a computed effective schema
  version, and is required rather than optional. See the protocol. The reasoning below is kept for why
  it was nearly left out.*** It would guard the one remaining hazard — two clients modifying the
  same existing type, last writer wins, the loser's added field or role vanishes. Everything involving
  creation or insertion already fails loudly on its own: a removed field or changed storage type fails
  an insert, and self-describing records killed the annotation-drift case. Today the exposure is a
  single app on a single device editing its own custom tracks. Adding an operation kind later is
  additive, so it waits. The shape, if revived: compare a scalar annotation on the type's own declared
  annotations, subset semantics, `null` asserts absence, `409 conflict` naming the keys.

### Guarding a write

**Expect what you depended on, not what you touched.** Updates change only the fields they name, so two
clients editing different fields of one record merge cleanly with no coordination. Guarding everything
turns that into manufactured conflicts between people who never collided.

A guard is needed when the new value depends on the old one: read-modify-write (a counter), a field
holding a compound value (`TrackDefinition.payload`, where disjoint conceptual edits are one field to
the storage), or a cross-field invariant. For an append-mostly dataset the more common hazard is not
concurrent edits at all but **duplicate creation**, which is `expect_absent`.

**No per-record `revision` field.** Adding one to every type means every write path must remember to
bump it, and a missed bump leaves a guard that looks alive and protects nothing. It also costs storage
on exactly the data least likely to need it — a GPS track is thousands of write-once rows.
`TrackDefinition` keeps the revision it has, because it is a blob with a single writer; it stops being
trustworthy the moment another product can write that record.

**Record versions are computed, never stored.**

```json
{ "id": "8121", "meta_id": "…", "version": "v1:9f2c…", "values": [ … ] }

{ "op": "expect", "target": { "type": "…", "id": "8121" }, "version": "v1:9f2c…" }
```

- An **opaque** value the server derives from the record's current values. The algorithm is the
  server's business and is never reproduced elsewhere, so there is no canonical form to define — a
  client only ever compares a value that server gave it against what that server computes later.
- **Covers all of the record's values, inherited and declared**, since a record of a child type is one
  row. Not the schema: hashing field definitions, roles or annotations would invalidate every
  outstanding version in the dataset on a single annotation edit.
- **Returned always on a single-object read, and on queries only via `include_versions`** — so a
  ten-thousand-point GPS fetch pays nothing. Not part of the projection, which must stay positionally
  consistent with `meta.fields`. Asking for versions with a narrow projection still forces the server
  to read the whole row.
- **MUST change when the values change. MAY change otherwise** — a schema change, a server upgrade
  that alters the algorithm. Therefore **a mismatch means "re-read and retry", never "someone edited
  this"**.
- **A version guards the whole record**, so two clients editing different fields will conflict where
  field expectations would have merged. Use the version for a form edit or a compound value; use field
  expectations when disjoint edits should merge. They can be combined.
- Note for parent projections: a query on an ancestor type returns versions reflecting fields that
  projection cannot show, so a child-only change conflicts invisibly. Prefer field expectations when
  reading deliberately through a parent.
- A **digest form for one field** (`field_digests`) is the known extension for large values, restricted
  to byte-valued types where the spec already guarantees byte-exact storage. Not v1; the plain form
  works and a definition payload is a few KB.

#### How we arrived at a computed version

Four alternatives were considered and rejected. Recorded so they are not re-proposed.

- **Expect the old values of the fields you depended on.** This *is* the base mechanism and it stays —
  it needs no stored state, no writer discipline and no clock, because the data is its own guard. It
  was rejected only as the answer for **compound values**: guarding `TrackDefinition.payload` means
  shipping the whole blob back to compare it, and the client must have kept it.
- **An application-maintained `revision` field.** Cheap and familiar, and the profile-A client already uses one.
  Rejected as a general answer because it only works if **every** writer remembers to bump it, and one
  that forgets leaves a guard that looks alive and protects nothing. In a dataset another product can
  write, that is unknowable — the profile-B service writing a plan without bumping the profile-A client's `revision` breaks the
  guarantee silently. It also costs a field on every row of types that are never edited.
- **A record update timestamp.** Same discipline problem if the app maintains it, plus its own failure:
  **two writes in the same millisecond produce the same timestamp**, so a stale expectation passes
  exactly in the burst case where it matters. If the *server* maintained it instead, a monotonic
  counter would beat a date on every axis — no same-tick collisions, no clock adjustment, no restore
  reintroducing old values — so the date form is strictly the worse version of something else.
- **A stored opaque token, ETag-style.** The strongest rejected option: it works across products with
  zero coordination, which is the one thing expectations on a shared `revision` cannot do. Killed by a
  concrete case — **a single run produces thousands of GPS points**, write-once rows that will never be
  edited, each carrying a 16-byte token on a 40-byte payload. It also re-adds per-object state we had
  just removed, and invites the argument that a feed should expose it.

**Computing it instead answers all four.** No storage, so the GPS case costs nothing. Correct by
construction, so no writer has to cooperate. Opaque and **server-local**, so there is no canonical form
to define — the only comparison is between a value that server gave out and what that server computes
later, and no two implementations ever have to agree. And the cost lands only where a client asks for
it, which is the guarding path and never the bulk read.

What it gives up: it cannot be ordered, filtered or resumed from, so it is useless for synchronisation
— **deliberately**, since that is what stops it growing into the change feed we decided against.

## Change tracking — deliberately minimal

- **One opaque watermark per dataset**, returned with every read: *this answer is as of N*. Nothing on
  objects.
- Rationale: caching belongs in the storage backend implementation, and a cache needs a version to
  validate against. The same number detects torn multi-round reads.
- **No** per-object sequence, **no** tombstones, **no** change feed, **no** sequence on schema changes.
  Addable in v2 if incremental sync is ever needed; nothing has to be reserved now.
- Opaque rather than an integer: carries the dataset generation, so a token from before an erase or a
  restore is rejected, and gap-counting side channels are closed.
- Shared datasets: one user's write bumps the number for another, costing a wasted refetch. Harmless,
  and it does not arise for single-user datasets.

## Semantics — roles, annotations, contracts

**Shipping in v1.** Two optional properties, on **fields and types**, never on objects:

```json
"roles": ["track.value"],
"annotations": { "dimension": "mass", "unit": "kg" }
```

- The storage stores and returns both and **never interprets them**. Tessera defines no role names and
  no annotation keys, ever.
- The **vocabulary** — role names, units, dimensions — lives in a separate shared document that
  products agree on. Units and measurement are not Tessera concepts; they go inside annotations.
- Nothing is mandatory. A type with no annotations is perfectly valid, and a generic client works
  against it.
- Meaning lives on the **schema**, not on rows: every object of a type is what its type says it is.
  Per-record variation — a unit that differs per entry — is ordinary data in an ordinary field, with
  the schema saying which field carries it.

### Contracts

A **contract** is a set of roles and annotations a *client requires*. It is not a type.

- The type is what the dataset holds; the contract is a predicate applied to it. Conformance is
  **structural, decided per client, and invisible to the storage**. Nothing declares conformance.
- Many types satisfy one contract; one type satisfies many. A delivered plan is a Plan and a Delivery
  at once, to two different readers, from the same metadata.
- **A required subset, not an exact match.** Extra fields with other roles or none do not disqualify.
- **Annotations are part of the test.** A `track.value` field with no dimension is not a Track — that
  is the recognition rule, and the failure mode is *not recognized*, never rejected.
- **Roles are not unique within a type.** Blood pressure has two value fields; recognition finds
  candidates and something else picks one, exactly as a metric names its field today.
- **Type-level roles are optional and structure decides.** A foreign type with a timestamp and a
  dimensioned numeric value is a Track whether or not its author knew the role existed. Requiring an
  explicit type role would discard the interop this design exists for.
- Not called a *meta type*: `ObjectMeta` and `MetaID` already mean a concrete type here.

### Not queryable in v1

- Role-to-type resolution is a **client-side operation over metadata**, which clients already load in
  full. The spec says so explicitly, so nobody looks for a filter that does not exist.
- What would actually add something is querying *across* types by role, and that is a union over
  heterogeneous types: projection, sorting and pagination all have to be answered. The client
  alternative is N cheap queries where N is the handful of types carrying the role.
- Purely additive later, unlike the annotations themselves, which are schema and had to land early.

### Documenting roles and annotations

The human doc explains **why**: meaning travels with the data, so renaming a field is not a migration,
and a program that has never seen your type can still use it. One worked example. State the honest
cost — interop depends on a shared vocabulary that nothing enforces.

The normative doc pins down:

- `roles`: array of strings, unordered, duplicates rejected, character set, max length, max count.
- `annotations`: a JSON object, arbitrary values, stored **exactly as given** — byte-for-byte, no key
  reordering or normalisation, the same rule the `json` field type already has.
- A server MUST NOT interpret, validate, infer, reject or rewrite either.
- Inheritance: an ancestor's roles and annotations appear in effective metadata; a child may not
  override them (proposed).
- Changing them is a schema update and never touches stored records.
- **No uniqueness rule.** Two fields may carry the same role. Recognition is the client's business.
- They count toward request size limits and are erased with the dataset.

### Units — per record by default

- **A measured field carries its unit per record** (the profile-A client's existing `unitSource: .recordField`).
  Records are then self-describing, and an annotation edit can never retroactively reinterpret stored
  values — the failure with no symptom, where a chart keeps drawing a continuous line that is now
  wrong.
- **`inherited` stays** for fields with one unit or none: counts, ratios, anything dimensionless.
  Per-record is the default for measured fields, not the only mode.
- **The annotation declares which mode a field uses.** That is the portable part, and the safety
  interlock: a reader that has never heard of a kilogram can still tell that this column is not
  addable as it stands.
- One unit field may cover several values — systolic and diastolic share one — so per-record units do
  not mean a companion field per value.
- **What it costs.** Aggregation becomes *grouped*: ask for a sum grouped by the unit field, get one
  row per unit, convert and combine client-side. Range filters stay pushable, because the client knows
  the units and can compile `(unit = kg AND v > 80) OR (unit = lb AND v > 176.4)` from a unit-naive
  one. The only unconditional loss is **sorting by a measured value**, which no rewriting fixes;
  ordering by time is unaffected, and that is how records are almost always ordered.
- **An `inherited` unit is immutable once records exist.** The app refuses to change a dimension or
  unit on a field that holds data; a correction goes through a new field. Cheap, because the app
  controls its own schema writes.

### Aggregation — optional, and compensated in the backend

- **Tessera keeps `:aggregate`, optional and advertised.** A server that cannot do it does not
  advertise it; one backed by a real database gets it for nothing. `group_by` earns its place — it is
  what makes per-record units workable.
- **The Swift `Storage` interface keeps `aggregateObjects`.** The model asks for an aggregate and never
  branches on who can perform it, exactly as with `withConsistentReads`.
- **`RemoteStorage` uses the server's aggregate when advertised and computes locally when not** —
  **bounded**, failing at the bound with the same `query_result_limit_exceeded` a declining server
  would return.
- This **reverses a deliberate earlier rule** — that a client refuses an unsupported operation rather
  than compensating, since a narrower advertisement removes a feature rather than slowing one down.
  The reasoning still holds, which is why compensation must be bounded and fail visibly: silent
  compensation turns a missing feature into an invisible performance cliff.
- **Compensate only where the query already bounds the result.** A date-filtered aggregate, yes.
  Distinct references across a whole type, or sorting a large set, no — refuse those.
- **the profile-A client's client profile does not require aggregation**, so the app can talk to a minimal server.
- None of this touches Tessera; it belongs with S1.8 on the app side.

### The vocabulary is authoritative, and it is the profile-A client's

- **It is not part of Tessera and never will be.** A Tessera implementer never reads it: the server
  stores roles and annotations and interprets neither, so the vocabulary is irrelevant to anyone
  writing a backend. Placing it beside the protocol would only imply it is normative.
- **It is domain-specific, and the name should say so.** `sample`, `additive`, `counter` are
  time-series measurement semantics; dimensions and units are physical measurement; `track` and `plan`
  are product concepts. Only `subject`, `source` and `timestamp` come close to universal. This is a
  *tracking* vocabulary that the profile-A client — and later perhaps the profile-B service — shares.
- **It lives in the backend repository**, not in an app repository, because **there will be an Android
  app**. Neither platform may own it, and that client's backend repository is the one of its
  repositories that is not a platform.
- **It is authoritative, not a transcription.** With two platforms, `fieldCatalog` cannot be the source
  of truth — Android would hold its own copy of the same constants, and two hand-maintained copies of
  `lb = 0.45359237` is exactly how numbers quietly diverge. The apps implement *from* the vocabulary.
- **The constants are a data file**, `vocabulary-v1.json`, beside the prose. Both platforms generate
  their tables from it, so adding a unit is one edit and neither platform can mistype a scale. the profile-B service
  later consumes the same file rather than reading a table and retyping it.
- **A consumer MUST fail loudly on an entry marked pending** rather than assume a scale. Pressure,
  volume, energy and several base units are still to be transcribed from `BuiltInPhysicalUnits`.
- Open: whether `meaning` belongs on the field as an annotation or is inferred from roles plus
  dimension. It is app-side only today, and two products aggregating the same data both need it.

### Vocabulary discipline

**Annotate what crosses a boundary; leave the rest alone.** Types that never leave one app — profile
settings, display order — need no roles. One vocabulary file; no core/product split until it is big
enough to be annoying. The moment someone wants the server to validate or act on a vocabulary entry,
it has stopped being vocabulary and must be argued as protocol on its own merits.

## Sharing

The rule that decides where a feature lives: **if the server must enforce it, it is protocol; if it is
inert, it is vocabulary.**

### Grants

- Grants are rows: *(principal, scope, rights, expires)*, where scope is a **type** or a **single
  object**. Object-only scoping does not work — granting read on ten thousand records one at a time is
  not a mechanism.
- **The owner path is unfiltered.** All view logic lives on the guest path, which only runs when
  something was shared. This is what keeps sharing from taxing the single-user case.
- **Visibility comes from the table, never from field values.** No write can move an object into or out
  of anyone's view, which deletes the whole both-states checking problem.
- **v1 grants are read-only.** No guest insert, update or delete. **The only writer into a dataset is
  its owner** — that invariant is what makes everything else cheap.
- Queries run against **type** grants; object grants serve direct reads by id. This keeps per-row
  filtering out of the query engine entirely.
- **References do not cascade.** A shared object pointing at ungranted ones resolves them
  `unavailable`.
- **A transaction touching anything ungranted fails whole**, carried over from the batch rule.
- **`unavailable` stays documented** so every client tolerates it, even against servers that never
  share.
- **Sharing is an advertised capability, not baseline.** The reference server and the profile-A client can ignore
  it entirely. The row-level *Writing within a view* rules move to the annex.

### Delivery, not shared write

The coach case — create a plan for a user, comment on progress, revise it later — is served by
**share read, copy on accept**, not by writing into someone else's dataset:

- The sender composes in their **own** dataset and grants the recipient read.
- The recipient's app **copies it in**, with the recipient's own credentials. They own it outright:
  the sender can vanish, revoke everything, or delete their copy, and the plan is still there,
  editable, exportable, backed up with the rest of the record.
- **The copy carries values, not references into the sender's dataset.** Reference the source for
  provenance; copy everything that matters. Otherwise revocation empties the plan.
- Revision is a new delivery the recipient accepts. Divergence — the recipient edited their copy — is
  a product decision about replace versus merge, detected with a digest of the accepted state stored
  on the copy.
- What is given up: the sender cannot edit *the* plan afterwards, and there is no simultaneous editing
  of one object. Nobody has asked for either.

### Requests and approvals

Inert, therefore **vocabulary, not protocol**. A request is an ordinary object in the *requester's own*
dataset — they can always insert there — carrying a role such as `grant.request`, with the owner
granted read on it. The owner discovers it because a dataset they hold a grant on appears in their
dataset list; no notification mechanism is needed.

Tessera never learns what an approval is. A pending state, an approval state, a notification: that is
workflow, and it stays out. Protocol support would also open the one hole nothing else in Tessera has
— an inbound path for strangers writing into a dataset they cannot see — which needs abuse controls
that buy nothing a client cannot already do.

### Identity — the standing blocker

None of the sharing above can be built until one person can name another: accounts on the server, or
an invite the other party claims. An invite code that the first redeemer binds to is the cheapest
option, since it avoids a user directory and the privacy questions that come with one. Out of v1.

## The delivery contract (vocabulary)

- `delivery.source` — reference from a copy to the object it was copied from.
- `delivery.supersedes` — reference from a revision to what it replaces.
- A **queryable timestamp**, `delivered_at` or equivalent. Since roles are not queryable, discovering
  new deliveries is an ordinary field predicate within a known type. Without it, step three of the
  refresh degrades into fetching everything.

## Authorization

Three options: OAuth 2.1 + PKCE for interactive apps, bearer tokens for automation, client credentials
for service-to-service.

## Baseline profile — confirmed

The rule: **if a client can do it itself, it is optional; if it cannot, it is required.**

- **Required**: discovery; metadata reads, effective and in a stable order; object reads; queries with
  predicates, sorting, projection and pagination; all-or-none transactions; preconditions evaluated
  inside the transaction; durable idempotency (key → outcome); the dataset watermark; erase; the three
  answers (`not_found` / `permission_denied` / `unavailable`).
- **Optional, advertised**: server-side aggregation, grouping, distinct reference values, text search,
  locales, attachments, sharing.
- **Consequence to confirm in review:** every capability flag the spec advertises today collapses.
  `recoverable_insert` and `atomic_object_updates` become required; `acknowledged_insert` and
  `acknowledged_removal` are transaction preconditions; `bulk_operations` is what a transaction *is*.
  The advertised list is replaced wholesale by the optional list above.
- "Required for a backend the profile-A client will use" is removed. the profile-A client's needs become a client profile in
  the app repository.
- Not advertising is not the same as trusting: a server that claims v1 and breaks a required guarantee
  is broken, and the conformance suite is what catches it, not the client.
- Keep separate from `access`, which reports what *this caller* may do — a different question in the
  same document.

## Workflow

Out of the storage spec, now and later. Storage answers what is true; a workflow engine answers what
happens next. the profile-B service builds that on top.

## the profile-A client side

- **S1.8** (one item): add a transaction member to the Swift `Storage` protocol; add
  `withConsistentReads` — a client-side scope with no wire concept, where the model declares which
  reads belong together and the backend honours it through the watermark, retrying internally; and
  **delete `summaryRevision`**, which only observes local writes and fails silently against a second
  device. Keep the `generations` and configured-store checks: those are session state, not data
  consistency.
- **The bundled catalog becomes data, not code.** Swift was a simplicity choice, not a fundamental
  one; the catalog can equally be JSON copied in at build. With an Android app coming that stops being
  a preference: **type ids, field ids, storage types, roles and annotations must be byte-identical
  across platforms**, because they are written into datasets. A hand-written Kotlin catalog means the
  same user logging weight on a phone and a tablet ends up with two different types for one concept in
  one dataset, and `context.types` reconciliation papering over a split that should never have
  existed.
  Two files, shipped from the client's own repository and copied in at build — they moved there when
  Tessera was separated, because a vocabulary this server never loads has no reason to live beside it:
  * a vocabulary document — role names, meanings, dimensions, unit scales.
  * a track document — the bundled type definitions themselves. A **sketch** exists;
    it is not generated from `BundledTrackCatalog` yet.
  `TrackFieldCatalog` and friends become loaders over that data rather than hardcoded tables, so
  adding a unit or a track is one edit and neither platform can mistype a constant. The backend
  example can serve the same definitions for testing.
  **Three tiers in the file, and confusing them is the failure**: what must be identical (ids, storage
  types, roles, annotations); what is localized per platform (display names, carried as keys, as they
  already are); and what is a platform asset (an SF Symbol name means nothing on Android, so the file
  carries a neutral icon name each platform maps).
- **Editing a record**: load fresh values *and the version* when the editor opens, not from whatever
  the list view cached — a long-open form otherwise guarantees a conflict. Save with `expect`. On
  `409`, re-read, **diff the loaded snapshot against what came back, and only tell the person the
  record changed elsewhere if the values actually differ** — a version mismatch can come from a schema
  edit or a server upgrade, and claiming someone modified a health record when nothing did is alarming
  and unverifiable. When they do differ, say what: *"Weight was changed on another device:
  81.2 → 80.4."* Both halves are in hand, so the diff is free. Then save again **under a new
  idempotency key**. The pattern already exists as `TrackDefinitionDraft.expected`.
- Proactive detection while a form is open — poll the watermark, re-read only if it moved — is cheap
  but unnecessary in the first pass; the save-time check carries it.
- **S1.9**: that client's own profile — a table of contracts, one row per concept it handles, saying
  what roles a type must carry to be a Track, a Plan, a Subject, and what each unlocks. Plus renaming
  spec references across the app's docs.
- Contracts replace `TrackTypeResolver`'s `switch field.id`, which infers roles from magic field ids.
  Two storages can then use different field ids for the same role.
- The app already handles differing shapes across storages: a ProfileTrack's context holds
  `context.types` (one per storage) and capability checks use `allSatisfy` — a track offers only what
  every copy supports.
- **the profile-A client writes annotations for its bundled types.** `installTrackMetadata` already writes the
  `ObjectMeta` when a track is first used in a storage; the portable part of
  `BundledTrackCatalog.fieldCatalog` — roles, dimension, unit source — is projected onto its fields in
  that same write. The catalog stays the authoring source and keeps what is product-specific:
  presentation, analytics, validation. Nothing is restructured.
  Two observations: `fieldCatalog` is keyed by field id and merged globally across domains, so it
  **already is a vocabulary** — publishing it means naming entries that exist, not inventing a
  taxonomy. And `BundledTrackCatalog.Fields` (`weight`, `loadUnit`, `doseValue`) is exactly the magic-id
  coupling roles remove: the ids stay as ids, and the meaning moves into the metadata.
- **Custom tracks already persist their semantics; they are in the wrong place.** `TrackDefinition`
  stores a custom track's `schema`, its per-field `TrackFieldOverride`s (role, measurement,
  collectionMeaning, analytics, presentation) and its `units` as an **opaque blob** in a
  a client-private `com.example.track_definition` object. The meaning is there and the editor already
  gathers it — a custom definition carries its own units and per-field measurement — but it is written
  in a private format *beside* the schema instead of *on* it, so no other product can read it.

  The change is therefore a split, not new authoring UI:
  * **Portable half → the meta's `roles` and `annotations`**: what each field means, its dimension,
    where its unit comes from, and any custom unit definitions.
  * **Product half stays in the blob**: presentation, symbol, colour, library declaration, analytics.

  Bundled and custom then converge — both end up annotated on the `ObjectMeta`, differing only in who
  authored it (`fieldCatalog` versus the editor), and a foreign type is a third case of the same
  shape. `TrackDefinition.namedSchema`, which already overlays authored metadata onto backend
  metadata at read time, is the precedent: this makes that overlay unnecessary rather than adding a
  mechanism.
- **Precedence — decided.** Authoring and reading are different jobs and do not compete:
  * **Authoring** — creating or changing a type in a storage — the app's definition wins and is
    *translated* into that storage's type: field types, plus the portable meaning as roles and
    annotations. One way only; the app never reverse-engineers its intent back out of a dataset, and
    the translation is deliberately lossy — presentation, symbol, colour and analytics do not travel.
  * **Reading** — interpreting objects already stored — **that storage's annotations win**, because
    they are what every other reader sees. The alternative is fatal: an app update would silently
    reinterpret existing records, and the profile-A client would disagree with the profile-B service about the same data while
    both read it correctly, which makes the portable annotation a lie.
  * **Per storage.** `context.types` is plural for a reason. The same track realised in three datasets
    can differ — an older app build wrote one, another product wrote another — and each dataset's
    annotations are authoritative for itself alone. Capability checks already use `allSatisfy`, so a
    track offers only what every realisation supports.
  * **The catalog authors; it never overrides.** It seeds annotations at install and may fill *gaps*
    on types the app authored in that storage. It never replaces a value already there. Changing
    meaning is a schema write, which propagates to every reader instead of only to the profile-A client.
  * **Scope gap-filling to types the app authored.** `fieldCatalog` is keyed by field id globally, so
    a foreign type naming a field `weight` would otherwise inherit the profile-A client's dimension by accident.
  * **Consequence: one reading path for everything.** The app recognizes its own storages' types the
    same way it recognizes foreign ones — read the metadata, match the contract. No "these are my
    types so I already know" shortcut, which is the shortcut that breaks the moment an older build or
    another product wrote the type.
- **A type reuse policy is needed.** If an existing type satisfies the contract, use it; install only
  when nothing does. Without this, every coach and every import adds another near-identical Plan type.
- No data migration anywhere; nothing has shipped.
- **Cross-dataset**: no snapshot is possible across storages. Consistent within each, never across.
  Document as a limit.

## Refreshing a delivered plan — worked example

With the coach's dataset **C** connected read-only and the user's dataset **U**:

1. Poll **C**'s watermark. Unchanged, stop — one small request.
2. Changed: refresh C's metadata, find types satisfying the Plan contract by their roles.
3. Query those types in C for `delivered_at > lastSeen`.
4. Query U for objects whose `delivery.source` points at a candidate.
5. Classify: a candidate that supersedes what U already holds is a revision. Compare the copy's digest
   against its accepted digest to see whether the user edited it.
6. Present, warning if the user's own edits would be replaced.
7. On accept, **one transaction in U**: install the Plan type if missing (or map the sender's fields
   onto an existing type by role), insert the copy with values, set `delivery.source`, supersede the
   old copy, record the accepted digest.
8. Store C's watermark.

No push anywhere: foreground, pull-to-refresh, or a periodic check, at one request per shared dataset
when nothing has changed.

## Contract questions the implementation raised

Five things the reference implementation could not decide for itself, answered after building against
them. Where the answer matches what was built, it says so: an implementation that had to choose is
evidence about which choice is liveable.

**Schema results are the REST results.** An operation's entry in `results` is exactly what the
single-operation form of that operation returns — `{"type": …}` for define and update, effective after
inheritance, `{"deleted_types": …, "deleted_objects": …}` for delete. One grammar per operation name,
whatever carried it, so a client that can read a `PUT` response can read a transaction. As built.

**Unknown members are refused.** Recognition-not-validation is about roles and annotations, which are
vocabulary and inert. Protocol grammar is the opposite case: a member a server ignores is a caller who
believes something will happen that will not. `400 invalid_request`, a new code, because overloading
`invalid_field` — which means a field of a record — would have made the two indistinguishable.

**The watermark sits beside the data it describes, and travels with a replayed answer.** One
placement: a top-level member of any body carrying dataset-derived data, never nested. No body, no
watermark, and no invented header for `HEAD`. Errors carry none. A replay — an idempotent repeat, a
receipt, a retained cursor page — carries the watermark the answer had when it was produced. Handing
back historical data beside a current watermark would tell a client the dataset had not moved since
data it is holding from an earlier state, which is the one lie this mechanism must not tell.

**The alias prefix is reserved, which is what makes it work.** The spec claimed `$name` could not
collide with a minted identity without saying why not. Now a server MUST NOT mint an object ID
beginning with `$`. One sentence, and the ambiguity is gone where a target identity appears; in a
reference value the alias replaces the whole object, so none existed there. Names are narrower than
object IDs, `as` is insert-only, and duplicates and forward references are refused before execution —
as built. No vector case follows from this: `$foo` remains a *valid* object ID to receive, and the
constraint is on what a server mints.

**A receipt decides once, for the whole body.** A transaction creates, updates, deletes and changes
types in one answer, and per-operation visibility would make the receipt a place where a deletion or a
narrowed view is disclosed — as well as handing a caller an answer whose gaps it cannot see. So: the
body if the caller can currently read all of it, identity-only otherwise. Schema-only results replay
to a `schema` or read holder; delete-only results have nothing to withhold. All-or-none governs
effects; the receipt is a record of the answer and outlives them. As built.

**Computed versions make a guarded update idempotent only where the value identifies the attempt.**
The claim was too broad: A → B → A returns the same version, and assigning a value the field already
holds moves nothing, so re-reading `B` cannot say who wrote it. Unguarded retries can overwrite, and
filtered retries can acquire rows that arrived meanwhile. These are documented limits, not an argument
for stored revisions — a stored revision answers none of them either.

## An authorization is approved for one dataset and reaches what it creates

The protocol said both "an authorization is over at most one dataset, and its listing returns
exactly that one" and "a created dataset MUST appear in its creator's listing". Both were
requirements and they cannot both hold. Each had been read on its own, so neither looked wrong.

The second stands. An authorization is *approved for* at most one dataset and *reaches* that one
plus any dataset created through it; the listing returns the reach. Creation is the only thing that
widens it.

Three security defects came out of the first reading being alive in code — a withdrawal that missed
what an installation had created, standing decided by the dataset an approval named, and a replay
that rewrote grants an administrator had removed. They came from the reach being invisible rather
than from its existing: it grows only through `create_dataset`, which is a right somebody
consented to. So the answer is to make it visible and say what it obliges, not to remove it. A
withdrawal takes everything the authorization reaches, and an approval screen offering creation has
to say that the installation will reach what it creates.

The general lesson is about the document rather than the rule. Two requirements in different
sections, each sound alone, contradicted each other for as long as nobody read them together —
and the only thing that would have caught it earlier is a case that fails under one of them.
The conformance suite asserted the weaker half, which both readings satisfy.

## Grouping by an identity is mechanical; grouping by a derived bucket is policy

Recorded when `group_by` was allowed to name a reference field, and kept because it decides more
than that one question.

A reference is an identity. Grouping by one is equality on an opaque value, which is structurally
what grouping by text already is, and the protocol already defines equality and a total order over
references. Nothing about what a reference *means* is consulted, so the protocol is not deciding
anything on the caller's behalf.

Time bucketing is the other axis every chart wants, and it is refused on the same line. A month
needs a timezone, a day-boundary convention, DST handling and a week-start rule. Those are product
policy, and a storage protocol that shipped them would be deciding what a month is for everybody
who stores a date in it. Rollups stay in the client.

The test is not whether the operation is useful or even whether it is cheap. It is whether
answering it requires the protocol to adopt a convention that its callers do not share.

## Still open

1. **`superseded_by` / `deprecated`** for additive schema evolution — purely additive, deferrable at
   no cost.
2. **A delivered type with a field the local type lacks** — install a second type, extend the local
   one, or drop the field.
3. **`spec/` versus `specs/`** — two directories one letter apart; the vectors one belongs to another
   agent.
4. **the profile-B service's answers** — is watermark polling enough, do they need schema changes visible, do they
   need client credentials.

## Process

Claude writes the specifications; another agent reviews. Earlier prompts drafted for the backend and
the profile-B service agents are superseded by this note where they conflict.

## Author resolutions of review findings 6–13 — 2026-09-19

These decisions supersede earlier sketches where they disagree. They change the contract, not the
implementation; the prose cases below are the implementation exit criteria.

**Implementation change required (finding 8):** the current resolve handler uses `415` even when a
representation capability is absent. It must return `403 not_supported` in that case. `415` remains
correct for an advertised capability unavailable for the particular item. No implementation is changed
in this specification commit.

**Implementation change required (finding 10):** dataset erase currently rejects a valid key as
`invalid_field`, not the newly specified `invalid_request`; the content erase handler does not check
the header. Both handlers must enforce the specified refusal before retirement.

### 6. Embedding follows grant-table-only, read-only sharing

Abandon the proposed-state write hook in v1. Keep current-grant checks in the write transaction and the
joinable unit of work: those enforce authority and atomicity without understanding domain fields.
A separate host extension can define domain behavior when it has a caller; v1 promises no future hook.
Reject preserving the hook as a required future seam, because it would require machinery for a sharing
model v1 explicitly excludes. Permission generations track grants and application authorization, not
record content. Remove milestone numbers rather than assigning obsolete work to a new stage.

An absent durable claim means no retained claim now, not “never began”: a delayed request can claim
later and still commit. Claim-before-work protects the outcome transaction; it cannot prove a request
is not in flight. Reject absence as proof of failure because it invites duplicate writes.

**Conformance case:** guest insert/update/delete, filtered mutations and schema writes fail; a field
edit leaves view generation unchanged; grant revocation invalidates cached-page replay. Delay an
original request before claim, observe `unknown`, then allow it to commit exactly once. A host rollback
must leave no committed receipt outcome. This matches the stated implementation model.

### 7. One import identity, fixed reservation, explicit emptiness

Use `(logical store, operation_id)` as identity, owned by the claiming principal and application
authorization. The owner alone can recover it, still subject to current write authority. This matches
the inspected implementation: the prompt's shorthand “store plus owner/application” described ownership,
not independent namespaces for each caller. Reject caller-local namespaces: a repeated store operation
ID should not silently become another import because a caller changed application authorization.
Different stores remain independent.

Fix `maximum_bytes` with the reservation and reject changed ceilings, as with changed metadata. It is
a bound rather than item metadata, but changing it while a transfer URI is in flight creates two
competing limits for one operation. Reject both silently accepting a higher ceiling and retroactively
lowering the limit; a fresh operation expresses a new bound. This matches current behavior.

Abort and idle reclamation end the import, never reset it. Retain a tombstone for at least
`import_retention_seconds` from termination and answer an unchanged owner replay with `410 expired`.
Clients must use fresh operation IDs even after that retention period; forgetting an outcome is not
permission to repeat the attempt. Reject immediate reuse because a delayed retry could reopen an
abandoned transfer. The implementation already refuses aborted/idle imports; durable retention and
transfer invalidation still need the stated conformance evidence.

An empty item must declare `byte_count: 0`; it commits without sending a chunk. Unknown-length empty
imports cannot commit, in either transfer mode. Reject adding an empty-finalization member or special
zero-byte range: a caller that discovers emptiness can abandon and reserve declared zero. This keeps
an interrupted upload distinguishable from an intentional empty item and matches provisional behavior.

**Conformance case:** same-store same-owner retry returns the same upload and received offset; another
principal or application cannot resume/commit/abort it; another store can use the same operation ID.
Changed content type, declared length/presence or ceiling conflicts, including after commit and when
two ceilings imply the same effective bound. After abort and idle reclamation, including restart,
replay is expired throughout retention and old transfer/commit attempts cannot revive bytes. Declared
zero commits to one identity on retry; unknown-length empty commit conflicts and remains unreadable.

### 8. Capability absence and item-specific inability differ

An unadvertised `thumbnails` or `current_representation` capability means `403 not_supported`.
An advertised capability that cannot supply the requested representation for the authorized item means
`415 unsupported_representation`. Neither permits substituting the original. Reject making every
failure `415`: that erases the discovery contract and conflicts with the general optional-capability
refusal rule. Reject making every failure `403`: an item-specific limitation says nothing about whether
the store supports the capability. No byte interpretation or format validation is introduced.

**Conformance case:** test both representations with capability absent (`403`), advertised but rendition
unavailable for the item (`415`), and available (the requested rendition). Original remains readable;
neither refusal may silently return it. **Breaking implementation change:** absent-capability failures
currently use `415` and must change to `403`.

### 9. Type grants are exact, including future schema growth

A type grant covers the named concrete type only. Descendant queries/counts intersect their requested
scope with exact grants before computing results. Object grants remain direct-read only. Creating a
subtype does not grant its records to anyone. Reject descendant-inclusive grants because an unrelated
schema writer could silently widen an existing guest's access, without the grant administrator choosing
that disclosure. This is an authority boundary, not query convenience. Matches current behavior.

**Conformance case:** grant a parent, seed parent and child records, and prove direct reads, descendant
queries/counts, grouped selection, aggregates, distinct references and pages expose only authorized
concrete types. Create another subtype after the grant and prove it stays hidden; grant that type
explicitly and prove it becomes readable. An object-only grant must not add a query row.

### 10. Erasure has its own recovery boundary

Record and content erasure reject `Idempotency-Key` with `400 invalid_request`, before retirement,
and retain no erasure receipt. Confirmation names the old identity and replacement discovery recovers
a lost response. Reject adding a receipt that outlives retirement: it needs a new ownership and
retention mechanism even though the existing replacement identity answers the caller's question.
Ordinary record/schema mutations retain their existing receipt rules. The recovery choice matches
provisional dataset erase behavior. **Breaking implementation differences:** dataset erase currently
uses `invalid_field` for a valid key; content erase ignores the header. Both must instead return
`400 invalid_request` before retirement.

**Conformance case:** a keyed erase fails without changing identity or scheduling deletion; an unkeyed
confirmed erase whose response is lost is recoverable through replacement discovery, and repeating
the old confirmed target cannot erase the replacement. Run for datasets and content stores. A keyed
ordinary mutation still replays its receipt.

### 11. Aggregation promises the members it defines

Drop “averages” from the capability description. The response defines counts, sum and extremes;
clients can derive an average from sum/value_count when the count is nonzero. Zero contributing values
have no average. Reject adding an `average` member and its extra null/rounding contract because there
is no caller needing a separately computed result. Matches current behavior.

**Conformance case:** numeric inputs produce the specified counts, sum and extremes; all-null and empty
ungrouped inputs have zero counts and null statistics. A client can derive the nonempty average using
only those defined members. There is no required average member; response extensibility is unchanged.

### 12. Distinct references return bare qualified identities

Use `{dataset_id, object_id, meta_id}` elements, with an explicit response example. The request names
a reference field, so the list is already homogeneous. Reject tagged value envelopes: they repeat a
type the client already knows and disagree with the qualified-identity ordering definition. This
settles endpoint encoding only and changes no general tagged-value vector. Matches current behavior.

**Conformance case:** decode the three identity members directly from each element, with no required
`type`/`value` envelope; deduplicate and sort by the specified qualified-identity order. Malformed/null
stored references fail rather than disappear, and exceeding the maximum fails rather than truncates.

### 13. Withdraw locales from v1

Finding filed here and in the implementation review: `Accept-Language` promised translated schema
names and `/locales` advertised them, but schema writes provide only a single `name`. Withdraw the
capability, discovery locale list and dataset route. Return names as stored, independent of language.
Client presentation may localise them without changing effective type or whole-schema versions.
Editing the stored name is still an ordinary schema edit.

Reject a translation map/write API: no current caller supplies one, and adding it would expand every
schema operation to repair an unused promise. Reject “server-supplied names” because it leaves the
source of translations outside the interoperable contract and privileges a server's vocabulary.
Reject language-dependent schema versions because changing language must not break `expect_type`.
The implementation advertises no locale support already; the required work is to prove stable names
and versions, not implement a fourth optional read capability.

**Conformance case:** discovery omits `locales`; metadata reads under different and absent
`Accept-Language` return identical stored names, effective versions and whole-schema versions. An
`expect_type` using a version read under one language succeeds under another with unchanged schema.
A schema name edit remains observable as a schema change. No locale route is required by v1.

**2026-09-21: a caller now exists, and the answer is unchanged.** One premise of the rejection above
was "no current caller supplies one". That is no longer true — a consumer is designing a household
dataset that two people read in different languages, where a schema installed by one member carries
names the other sees. They asked for the question to be answered deliberately rather than left
open, which is the right way to ask it, so the record should not go on saying nobody wanted this.

Out of scope for v1 all the same, and for a reason that is specific to the case rather than a repeat
of the general one. **In the household case the schema is installed by the consumer's own
application.** That application already knows the type and field identifiers, which are stable and
language-neutral by contract, and it can ship labels in every language it supports. Carrying the
translations in the schema would move into every schema operation data the renderer already has.

A translation map earns its cost only where the supplier of a schema is not the renderer of it —
and in that case, which is a person freehand-typing a type name, there is no translation for the
protocol to carry either. So the feature helps exactly where it is not needed and is absent exactly
where it could not help.

What a client builds on instead, all of it already required and proved by the suite: identifiers are
stable and language-neutral, `Accept-Language` MUST NOT change a name or a version, and an
`expect_type` guard read under one language holds under another. Localise from the identifier.

If this returns it will be because the supplier and the renderer have genuinely come apart —
somebody installing a schema for people whose applications they do not write. That is a different
protocol question from the one answered here and it should be raised as one.

## Author resolution of review finding 14 — 2026-09-19

### The sum belongs to the contract, not the accumulator

**Breaking changes required:** the Swift test
`aLargeSumIsTheNearestNumberRatherThanAFailure` must expect `422 query_result_limit_exceeded`
with reason `sum_overflow` for two signed 64-bit maxima. A server accumulating integer fields in
binary64 must instead return exact in-range integer sums and refuse out-of-range totals. The Go
reference implementation's sticky intermediate-overflow flag must give way to checking the final
mathematical total, so cancellation can succeed; its float accumulation must round only the final
exact total. Its sum, group-limit and distinct-reference-limit failures must supply the reasons below.
The existing conformance assertion `9007199254741000` remains correct, but the suite needs the new
boundary and cancellation cases. No implementation or test is changed by this author resolution.

An integer sum is exact within −2⁶³ … 2⁶³−1 and refused outside it. This is a fixed result range,
not merely a minimum accumulator width: allowing wider servers to return more would preserve the
disagreement. The range comes from the protocol's integer type, not the reference server's choice of
machine word. Two values of `9007199254740993` therefore sum to `18014398509481986`; two signed
64-bit maxima fail. The previous permission to round integer sums beyond 2⁵³ is withdrawn;
the existing latitude for minimum and maximum is unchanged.

The boundary is on the final sum. Rejecting an intermediate overflow would still let a database's
row order decide success: maximum, one, minus one has a valid total. Float fields have the same
problem, plus rounding at each addition. Define their result as the exact sum of stored binary64
values rounded once to binary64, nearest with ties to even; refuse if that rounds to infinity.
This does require more than a native running float accumulator. It closes the same accumulator
loophole for the other numeric field type without prescribing a scan order, a database algorithm or
a new numeric wire type. Exact zero is positive zero; subnormals remain legal.

**Rejected alternatives:**

- Binary64 semantics for integer sums would make very large totals available but silently discard
  exact integer information in otherwise representable results. Calling them statistics does not
  establish that every caller can tolerate that loss. A fixed integer range gives a portable refusal
  boundary using a type the protocol already requires. It also avoids confusing conversion of each
  input followed by repeated rounded addition with a once-rounded exact total; those are different
  contracts, especially under cancellation.
- Deliberately implementation-defined success beyond 2⁵³ would be honest but would make callers
  branch on the server's arithmetic. Two independently conformant implementations already exposed
  this cost. Recording that disagreement in the suite would certify it rather than resolve it.
- Unbounded exact integer results or a configurable accumulator capability would expand the numeric
  result contract or discovery surface without a caller needing either. A minimum width alone is
  insufficient: both narrower refusal and wider success must be ruled out at the chosen boundary.
- Fixing an accumulation order would require ordering work just to make intermediate failures and
  rounding repeatable. Checking the mathematical total gives the same answer for the same group
  independently of storage layout and parallel execution.

This passes the eight opening arguments: only the declared numeric type and values matter, with no
vocabulary interpretation (1); aggregation remains optional and bounded client compensation remains
possible (2); arithmetic and refusal are enforced protocol behavior (3). The independent client's
failed portable test supplies the caller, while no new endpoint or capability is added (4). Stored
records and their meaning do not change (5). Fixed totals and conformance cases enforce agreement
without implementation discipline or client heuristics (6). The withdrawn result latitude and actual
implementation costs are stated rather than hidden (7), and only users of aggregation pay for its
arithmetic, with declared scan bounds still available (8). Advertising aggregation is a promise about
these results, not a report that a database has a native `SUM` function.

### A reason distinguishes the bound the caller hit

Require `details.reason` for the three failures raised in this finding: `sum_overflow`, `group_limit`
and `distinct_reference_limit`. The existing `422 query_result_limit_exceeded` code is unchanged.
The caller can distinguish an unrepresentable total from too many groups or an exceeded requested
reference ceiling without parsing a message. If several bounds are exceeded, any established one may
be reported; there is no obligation to keep computing after refusal. Other uses of the code, including
scan bounds and oversized requests, retain optional details, and clients handle absent or unfamiliar
reasons.

Reject optional reasons for these three cases: that would leave the caller's ability to distinguish
them dependent on the server again. Reject a separate error code for each, or a taxonomy for all
possible execution limits: the existing per-code details mechanism suffices for the concrete need.

**Conformance case (prose only):** the normative
[aggregate sum cases](tessera-protocol-v1.md#aggregate-sum-conformance-cases-finding-14) cover exact
integer sums past 2⁵³, both signed 64-bit endpoints and out-of-range totals, cancellation in every
permutation, float ties-to-even and overflow/cancellation, subnormals and zero. They run within
declared scan bounds, both grouped and ungrouped, preserve null statistics, and require whole-request
failure for an overflowing group. Separate cases exercise all three reasons without competing
failures. Compare mathematical integer values and decoded binary64 values, not one JSON spelling.
The frozen value vectors are untouched; these are HTTP aggregation and error-envelope cases.
