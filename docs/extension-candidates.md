# Extension decisions and candidates

This record preserves proposals, consumer feedback and rejected alternatives. It is not
a current work queue. Several original proposals are now implemented; the
[decision index](asked-and-answered.md) records their outcomes and the
[backlog](backlog.md) lists work still scheduled.

## Quick guide

Look up a question in [asked and answered](asked-and-answered.md), then read the numbered
candidate below for its reasoning. For a new change, follow
[contributing](../CONTRIBUTING.md#the-specification-is-the-contract): author decisions
and conformance cases belong with the contract, not merely in a proposal.

The assessment below was written before these changes shipped. Its ordering, references
to planned clients and original proposal table describe that historical decision window.
They do not assert that today's deployments lack those features.

## The sorting question

Not *core or capability*. That framing asks how important a feature is, which is unanswerable without
a use case, and every use case argues for its own. The useful question is **how badly does adding this
later hurt**, and it sorts candidates cleanly into two classes.

**Encoding-shaped** changes alter what a value, a grant or a field *is*. Added after clients exist,
each one becomes a question every client asks forever — *does this server know `decimal`? does this
server's sharing include `insert`?* — and n such questions are 2ⁿ compositions a client must handle.
These are the ones the window is about.

A cursor looks like it belongs in that class and does not, which §5 works through: watermarks are
opaque and server-local, so a later feed defines its own cursor without invalidating one already
issued.

**Endpoint-shaped** changes add an operation behind a capability. A client that does not call it is
unaffected, and a deployment that does not implement it refuses with `not_supported`, which every
client already tolerates. These are clean additions at any time and should be deferred on purpose.

| Candidate | Shape | Proposal |
| --- | --- | --- |
| Fold `optional_reads` into the required core | Discovery | **Adopt.** Subtracts three capabilities |
| An exact decimal value type | Encoding | **Adopt now, or never.** A consumer already implements the encoding |
| Constraints in `FieldMeta` | Encoding | **Adopt `immutable` only.** The rest has no consumer |
| An `insert` grant right | Encoding | **Adopt the mechanism**, drop the professional-write justification |
| A journal | Endpoint | **Hold.** The reservation argument does not survive; the audit motive does |
| A grant directory | Endpoint | **Defer.** Neither client would call it |
| Cross-row invariants | Transaction grammar | **Hold for want of a consumer.** Neither client needs it |
| Record provenance | Encoding | **Adopt only with §4**, and specify it last |
| Retention binding the owner | — | **Reject.** It dissolves into copy-back; reasoning recorded |
| A conformance fixture off HTTP | Repository | **Withdrawn.** See *corrections* |
| Dataset provisioning (finding 16) | Endpoint, and a grant | **Open in the review.** §4 depends on it |
| Addressing a dataset not yours (finding 15) | Error semantics | **Open in the review.** Not an addition |
| Cross-dataset query, guest update, review, triggers | — | **Reject**, see below |

The first row is the largest single improvement available, and it removes rather than adds.

## 1. The three `optional_reads` are not capabilities

`aggregation`, `distinct_references` and `text_matching` are advertised per composition. All three are
pure compute: a server with the rows can produce the answer, and what varies between implementations
is how fast, not whether.

The repository's own invariant already says what that means:

> **A capability is a promise about the protocol, not a report on the database.** Compute is required
> of every conformant server; only guarantees an implementation cannot manufacture are advertised. A
> server too slow for a query declares a bound and fails at it rather than dropping the operation,
> because the client refuses rather than compensating.
>
> — [AGENTS.md](../AGENTS.md#invariants)

By that rule these three are already required, and the specification disagrees with it. The
specification's capability table is headed *"Everything a client can do for itself"* and then lists
`sharing`, which no client can do for itself; and its compensation paragraph instructs a client to
re-read rows and compute locally under a bound, which is exactly the compensating the invariant says
a client refuses.

The mechanism for requiring them exists and is already applied to mandatory operations:

> A server that cannot execute one of these efficiently MUST still implement it, declare a bound such
> as `max_scanned_rows`, and fail with `query_result_limit_exceeded` when a request exceeds it.
> Failing explicitly at a stated limit is conformant; omitting the operation is not.
>
> — [the protocol](../specs/tessera-protocol-v1.md)

Requiring the three costs a minimal server a loop over rows it is already permitted to bound. It buys
a contract where five capabilities become two, every server computes finding 14's exact sums rather
than one in some deployments, and the compensation path — a client doing the server's work with less
information than the server had — is deleted from every client that will ever be written.

**Client-side encryption does not defeat it.** the profile-B service encrypts measurement values and can filter by
track but not by value, so requiring aggregation of every server looks unsatisfiable for an
encrypting deployment. It resolves as a type
question rather than a capability one: a client-encrypted numeric is not a numeric to the server, so
it cannot be declared `integer`, `float` or `decimal`, and aggregation over it is a type error rather
than a declined capability. the profile-B service's own shape is the worked example — `measurement_ciphertext`
stored beside a plaintext `track_id`, which is what makes filtering by track possible and filtering
by value impossible.

What the change does do is move where that choice lives. Deployment-level opt-out disappears, and
privacy-by-encryption becomes a per-field type decision that permanently forfeits aggregation on that
field. That is the right place for it — the trade becomes visible in the schema, where every reader
sees it, instead of in a capability list.

**This is the one candidate that is a review finding rather than a proposal**, because two texts in
this repository currently disagree about it.

## 2. An exact decimal type

**Adopted 2026-09-25 and implemented, spelled `fixed`.** The window argument held: it went in before
any client shipped, so no client carries a permanent question about whether a server knows it. What
follows is the case as it was made, kept because the reasoning is what a later reader needs — see
[the review](tessera-spec-review.md) for what shipped and where it differs.


There is no exact decimal type: [values](../specs/tessera-protocol-v1.md) are `integer` (int64) and
`float` (binary64). Money can be carried as integer minor units with the scale in an annotation, which
works and is the pattern measured fields already use for units — but annotations are opaque to the
server by design, so two clients can disagree about scale with nothing to catch them, and a `sum` over
mixed scales is meaningless while looking exact.

Proposal: an exact decimal storage and value type, encoded as a string so it is never parsed through
a double, with scale declared in `FieldMeta`, exact comparison, and exact summation on the machinery
finding 14 already built for integers. **Preferably not spelled `decimal`**: a frozen vector case already
uses that name as its example of an unknown type. Keeping it is possible — *what the window does not
cover* sets out an append-only way to restore the lost coverage — so this is a choice both clients
made, not a constraint the freeze imposes.

**This does not have to be argued from first principles, because a consumer already implements it.**
the profile-B service carries exact decimals as bounded canonical strings — a grammar capping integer and fraction
digits, canonicalised so equality is string equality — and computes over them in scaled big integers
with no float on either side of the wire. It arrived at this encoding independently, before seeing
this proposal. Its scale is fixed at six; declaring scale per field generalises what it already does.

The reason it is on this list rather than a later one: **a value type is the encoding, not a feature.**
Every client's decoder, every comparison rule, every conversion row and every vector case is written
once. Added later, the type is a permanent question at the top of every client.

Forward compatibility is already idiomatic. A client meeting a field whose storage type it does not
know degrades to *not recognized*, which is the same failure the design chose for unknown roles and
unknown types — never *rejected*. The cost is honest and bounded: a third numeric type widens the
exact cross-type comparison rules, which are already the most intricate part of the value section.

**Two vector cases, not one, and they trap different mistakes.** A string encoding keeps the value
away from a JSON number reader, which is the cause behind all five documented Swift divergences — but
it then invites whatever decimal type the platform offers, and those fail differently:

| Client | The trap | The case that catches it |
| --- | --- | --- |
| Swift | `Foundation.Decimal` is 38 digits, not arbitrary precision | A value longer than 38 significant digits |
| Go | `strconv.ParseFloat` looks like parsing and is lossy past 17 significant digits | A value exactly representable as a string and not as `float64` |

The Go case is the decimal analogue of `9007199254740993`, which this repository already uses to
catch the same mistake for integers. Neither case catches the other's trap: Go's `math/big` has no
38-digit cliff, and Swift does not reach `ParseFloat`. Both are additions, which the freeze permits,
and the specification should say what a client must preserve rather than leaving it to be inferred.

## 3. Constraints belong in `FieldMeta`

Range, non-null, pattern, allowed-value set, and **immutable-after-write** — of which, after both
clients answered, only the last has a consumer.

This does not breach *never load a vocabulary*. The distinction is exact: the server reads the
**constraint**, never the **role**. It applies a mechanical rule it was handed; it never infers one
from meaning, and it still does not know what a kilogram is. Nothing here asks the server to
understand a unit, balance a ledger or validate a currency — those stay rejected, under *what stays
out*.

The argument for requiring rather than advertising is the argument the required/optional line already
uses. A client can validate its own writes. **A client cannot reconstruct "no other client has ever
written a negative value here"**, and a dataset with more than one writer is the premise of the entire
design. An optional guarantee is not a guarantee: a client that cannot rely on it validates
defensively anyway, so advertising it buys nothing except a branch.

`immutable` is worth the change on its own, and is the part both clients support. What it buys is
narrow and should be stated narrowly: **a value that cannot be changed after it is written.** the profile-B service
would declare one, on an approved plan version.

**It is not an append-only ledger and not an audit trail**, which an earlier version of this section
claimed. It carries no writer identity and no prior state; it does not stop a record being deleted;
it does not survive `delete_type`, which with `cascade` *"destroys descendant types and all their
objects"*; and the proposal does not yet prohibit removing the flag, which is an unsettled
protection boundary rather than established behaviour of a built feature. the profile-B service's own trail is
append-only *event rows* — records whose values never change — so `immutable` expresses the shape of
a row in it and does not reproduce the trail. §5 is therefore no more deferrable for this, and the
open questions below are about when the flag binds rather than about what it preserves.

**The rest has no consumer.** the profile-A client would not use any of it today; the profile-B service would declare `immutable`
on an approved plan version and none of range, pattern, allowed values or non-null, because its
remaining rules are unit and meaning rules that must stay client-side. This repository has a standing
answer for that position — *building the capability for nobody is what the rule against speculative
abstraction exists to prevent* — and it applies here. **Narrow this candidate to `immutable`**, and
reopen the rest on a named rule a client wants enforced against a writer it does not control.

Two questions it opened, both answered in
[finding 43](tessera-spec-review.md#finding-43-immutable-and-when-it-binds) when it shipped. Adding the
flag binds from that commit over whatever is stored, and nothing stored can violate a rule about what
happens next. A subtype cannot narrow it, because a field is declared once and a descendant carries the
declaration unchanged — so the deep hierarchy gets the flag everywhere the field appears.

## 4. An `insert` grant right

**Shipped 2026-09-27** as [finding 44](tessera-spec-review.md#finding-44-an-insert-grant-and-what-it-does-not-convey):
composition with read is by type and never by authorship, and the specification carries the sentence
below. The rest of this section is the reasoning as it stood.

The specification twice qualifies the right set rather than closing it — *"in v1 conveys `read`
only"*, and *"Rights are `read` in v1"* — so the shape is anticipated, not the reasoning against it.

A grant right `insert`, scoped to a type. The inserted object belongs to the dataset owner the moment
it commits; the writer gets no read, no update and no delete, and cannot see what it wrote. The
load-bearing invariant survives untouched — *visibility comes from the grant table, never from field
values*, and *no write can move an object into or out of anyone's view* — because an insert grant
conveys no view at all, so there is still no second state to authorize a mutation against. Operation
status stays available to the writer, which is what an offline producer needs.

**It does not disclose nothing**, which an earlier version of this section claimed. A receipt retains
the resulting identity and *"the owner outlives its access"*, so a contributor can learn the identity
of what it created, after the grant is gone. That is the contract working as specified, not a leak —
but it means this right needs an explicit **identity-versus-body rule**, and the contract already
supplies its shape: a `committed` receipt returns the identity *always*, and the stored body *"only
while the owner currently has read authority over the record"*. So a contributor holding insert alone
learns the identity and not the body — **not because the body is forbidden to it, but because it has
no read authority**. Saying "never the record" would invent a new prohibition on combining
permissions, which is a separate decision and not this one. What this right owes is a statement of how
it composes with a read grant the same principal may already hold.

What it unlocks is append-only fan-in: forms and survey responses, device and sensor ingestion, a
bank feed pushing transactions, a meter or a till reporting totals.

It also dissolves the case usually made for cross-dataset query. A collector's records land in **one**
dataset it owns, where aggregation already works, rather than in n datasets it must fan out across.
The problem was never that reads could not span datasets; it was that there was no legitimate way for
data to arrive in one.

Costs, both real: an insert grant is a write amplifier, so quota and rate limiting become deployment
concerns the [deployment contract](../specs/tessera-deployment.md) does not currently name; and an
owner needs some answer for contributions it did not want.

It is listed as encoding-shaped because a right added after clients exist is a sub-capability of
`sharing`, and sub-capabilities are how a capability list stops meaning anything.

### The boundary this must not cross

A professional writing into someone else's dataset is **not** one of the cases above, and the profile-B service —
which owns the contract that boundary protects — is the reason. Its earlier review produced the
blocker this repository already records: *delegated, scoped,
revocable authority over someone else's records is the exchange's job.* Tested against all four
terms, an insert grant is delegated (an owner confers it), scoped (to a type), revocable (it is grant
state), and over someone else's records (the object is the owner's on commit). All four hold. That the
visibility invariant survives establishes only that the right does not break authorization, which is
a different question.

Insert also misses the shape the exchange serves. A professional writing into someone's dataset has to be
able to correct and withdraw, which the profile-B service treats as first-class: revision chains validated so a
revision is exactly its predecessor plus one, cancellation and removal, and cleanup that stays
pending until acknowledged. Insert conveys none of that, and the writer cannot see what it wrote, so
it cannot even locate the record it needs to fix.

**The distinguishing property is not who writes. It is whether the writer retains a continuing
relationship with what it wrote.** A bank feed does not: the transaction is asserted once and never
revised by its sender. A clinician does. That test decides which cases belong here and which belong
to the exchange, and it is more useful than any list of examples.

**If this is adopted, the specification should say that an insert grant conveys no continuing
authority over what was written.** the profile-B service asks for that sentence rather than a feature, on the
grounds that insert plus a retained operation status resembles submit plus receipt closely enough
that a consumer will start there and then bolt on review state, correction, withdrawal and
purpose-scoped consent one deployment at a time. One sentence does most of the boundary-keeping.

**Under composition it needs restating.** A clinic or a bank cannot insert into a dataset that lives
on a phone, so this right only functions on a dataset a contributor can reach. The shape that serves
a device-primary application is therefore a small **server-side dataset the user owns whose only job
is receiving contributions**, while the bulk of the records stay on the device. Nothing is copied out
of it: under composition it is simply another storage the application reads, alongside the local one.
That is a better fit for the product direction than the framing above, not a worse one — but it is a
different deployment, and the quota question lands on a dataset that exists only to be written to.

## 5. A journal

Today: *"There is no per-object sequence, no tombstone and no change feed."* No client can
reconstruct the answer from a watermark, which says *something changed*, never what, and cannot be
resumed from. That is real, and it is what blocks audit retention.

**No client has a synchronisation need for it.** the profile-A client does not poll; the profile-B service's cross-service
question is already answered by receipts it will not let elapsed time satisfy. Under composition (see
*where the data actually lives*) there is no replica to reconcile either, so nothing in the product
direction asks for a feed.

The endpoint should wait. It is the most expensive item here — storage, a retention window, and
sequencing that survives views, since a *gap* in a guest's feed discloses that a hidden commit
happened, which means per-view sequences rather than one global sequence filtered late.

**Nothing has to be reserved for it.** [Decisions](../specs/tessera-decisions.md) already put that
question and answered it — no per-object sequence, no tombstones, no change feed, *"addable in v2 if
incremental sync is ever needed; nothing has to be reserved now."* A watermark is opaque and
server-local, so a v2 feed defines its own cursor without touching one, and no client carries a
migration. There is no expiry on this candidate.

**What it rests on is the audit motive alone**, and §3 does not relieve it. An append-only record of
who committed what, retained, is what statutory retention and a medical record want; `immutable`
gives none of those three — not *who*, not *retained*, not *what changed* — so the two are not
alternatives. the profile-B service's remark that `immutable` expresses the shape it built by hand is about a row in
its trail, not about the trail. What holds this candidate is that neither client has the gap today,
not that something cheaper covers it.

## 6. A grant directory

`GET /v1/grants` — what has been granted to me. Not grant administration, which stays with
[the deployment contract](../specs/tessera-deployment.md); only discovery, in the same shape as
`GET /v1/datasets`.

The no-inbox reasoning holds for what it actually rejected: a guest cannot query a shared type to find
what was shared, because that type holds other principals' objects. Listing a caller's *own* grants
discloses nothing about anyone else. It removes the out-of-band handshake that makes object-scoped
sharing awkward to operate.

Endpoint-shaped, therefore deferrable. Worth taking whenever object-scoped sharing acquires a second
operator.

## Validation: two candidate applications

The list above was assembled from gaps, which risks fitting a proposal to the cases that produced it.
Two applications neither client is building were used to test it from outside, both assuming §1–§6
adopted: an accounting application for small companies, where an accounting firm is granted access to
do the books and the reports; and a pet health record, where veterinary clinics are granted access,
review what they read, and write conclusions back.

### An accounting application

| Concern | Served by |
| --- | --- |
| Money values and exact report totals | §2, and §1 — every server sums exactly, rather than some of them |
| Posted values resist edits | §3 `immutable`, while the constraint applies — not deletion, not a schema change |
| Double-entry posting | The transaction grammar with aliases — entry and its balanced lines, all or none |
| No duplicate postings from an offline device | Idempotency bound to a payload digest |
| Trial balance, profit and loss, tax totals | Required aggregation with `group_by` |
| The firm's access | Read grants scoped to a type or an object, with expiry, revocable as account state |
| The firm's adjusting entries | Copy-back, which also matches the statutory position: working papers stay the firm's |
| A bank feed, a point of sale, an invoicing service | §4 — they push in and never read the ledger |
| Receipts and invoice documents | The content protocol |

Left to the application: **debits equalling credits**, which no field constraint reaches; period
close and locking; recurring invoices and filing deadlines; tax rules and report layouts; **gapless
document numbering**, which is hard under retries and which opaque minted identities do not provide;
and the firm's portfolio view across its clients, which is a fan-out over datasets.

### A pet health record

| Concern | Served by |
| --- | --- |
| Pet as subject, weight and dose as tracks, a treatment course as a plan | the profile-A client's existing model, almost unchanged |
| Units carried per record | the profile-A client's existing unit source |
| Photographs | The content protocol |
| Dose and observation integrity | §3 `immutable` on an administered dose. Range and allowed values were cut |
| A clinic reading part of the record | Type-scoped read grants with expiry |
| A clinic writing its conclusion into the owner's record | **Nothing. See below** |
| Trends and dose counts | Required aggregation |

**The clinic conclusion is the case insert-only serves worst**, which is the boundary §4 describes
seen from the application side. A conclusion about a dose can be wrong. Its author cannot correct it,
cannot withdraw it, and cannot see what it wrote well enough to find it — so the owner is left
holding a clinical assertion its author has disowned, with no path to reconcile the two copies.

What the pet record needs there is the exchange: submission, review, correction, withdrawal,
acknowledged cleanup. The storage-layer answer is copy-back, unchanged — the clinic grants read on
its conclusion and the owner's application copies what it accepts, which at least leaves the clinic
holding the authoritative version it can revise.

Within this application §4 still serves the parts with no continuing relationship: a connected scale
reporting a weight, a feeder or a sensor. Not the clinician.

Not Tessera at all: **appointment scheduling**, which is an integration with each clinic's practice
management system and is exchange-shaped rather than storage-shaped; medication reminders, which are
workflow; and the clinic's own operational queries — *which patients are due* — which span every
owner who granted it access. That last one belongs against the clinic's own patient index, which it
has regardless, with Tessera as the shared window onto the owner's record rather than the clinic's
database.

### Where the data actually lives

Both applications assume the user's records sit on a server. The product direction they are drawn
from does not: data lives on the device, and external sources connect to it. That is a different
architecture, and the design already has an answer for it —
[decisions](../specs/tessera-decisions.md):

> **Per storage.** `context.types` is plural for a reason. The same track realised in three datasets
> can differ — an older app build wrote one, another product wrote another — and each dataset's
> annotations are authoritative for itself alone. Capability checks already use `allSatisfy`, so a
> track offers only what every realisation supports.

**Composition, not replication.** Not one dataset copied to several places, but several independent
datasets speaking one vocabulary, with the application reading across them through the same contract
path it uses for a foreign type. A device-local store is one dataset; a remote server is another; a
clinic's or a firm's server is a third.

Two problems never arise as a result. Object identity is minted by the dataset and never reused,
which would be fatal for a replica and is irrelevant for a distinct dataset. References are qualified
by the dataset that owns them, which would need rewriting on upload and does not.

**The question this forces, before any candidate below is worth costing:** is a device store a
*different dataset* from the remote one, or a *copy* of it? Composition is supported today and
supported well. Replication is not, deliberately, and every hard problem — merge, identity
reconciliation, reference rewriting — lives there and nowhere else.

Three candidates change under composition, and they are corrected in place below: §4 needs a
reachable dataset the device does not have, §5 loses most of its motivation, and §8 is partly
answered by which storage a record sits in.

### What the exercise found

The positive result: both applications turn on §3 `immutable` and on §4 for their append-only
contributors — a bank feed, a meter, a scale — and neither is blocked by the journal endpoint being
deferred. Nothing in either case argued for cross-dataset query, which is the rejection most often
challenged. What the pet section settles is which contributors §4 serves: a scale or a sensor, never
the clinician.

The negative result is three concerns neither application can meet and no candidate above addresses.
Two unrelated domains arriving at the same three is the reason they are recorded here rather than
left as observations.

## 7. Cross-row invariants

Neither *debits equal credits* nor *this dose belongs to an active prescription* is expressible. §3
is deliberately field-level: it constrains a value, never a relationship between rows.

Two shapes, and they are not equivalent:

- **Ad hoc**, as an operation inside a transaction, in the style the grammar already uses for checks
  — the client asserts that a property holds over the objects this transaction touches, and the
  server evaluates it before committing. Cheap, and additive to the transaction grammar.
- **Declared**, as an invariant on a type, enforced against every writer. Expensive, because the
  server must evaluate a predicate over a set the write does not name.

The ad hoc form is strictly weaker, for the reason §3 already gives: a client can check its own
writes. What it cannot do is stop *another* client posting an unbalanced entry, and a dataset with
more than one writer is the premise. So the cheap form does not actually deliver what the accounting
case needs, and the form that does is the largest item on this list.

**Both clients were asked, and it has no consumer.** the profile-A client reports nothing it cannot express;
the profile-B service reports no cross-row invariant it needs, its own constraints being row-level and its scope
rules referential and handled by foreign keys, and states that no analogue of *debits equal credits*
arises in a coaching workspace. the profile-B service also confirms this is not the referential-integrity question
in new clothes — integrity says a reference resolves, a cross-row invariant says a predicate holds
over a set the write does not name — so nothing here reopens what
[database-like-service](database-like-service.md) left pending.

**Hold, for want of a consumer**, which is the disposition referential integrity already has and for
the same reason. The principled argument survives; building it for nobody is what the rule against
speculative abstraction exists to prevent. Reopen on a named invariant a client wants enforced
against a writer it does not control.

## 8. Record provenance

No record says who wrote it. The application can carry an author field, and the owner can write
whatever it likes into it.

**§4 is what creates this need, and that is the whole of the argument.** Before insert grants, every
record in a dataset was written by its owner, so provenance was answerable by construction and no one
had to ask. After §4 a dataset contains records the owner did not write, and nothing in the record
distinguishes them.

Neither client needs it. the profile-B service answers the question a layer up, storing the producer principal as a
keyed blind lookup rather than an identifier and the accepting reviewer on the record; the profile-A client has
no mixed-writer dataset. the profile-B service notes its blind-lookup technique may transfer — it buys *same writer
or not* without carrying who — but also that the case which wanted a name, the clinician, is the case
§4 should not serve at all. What is left wanting a name is thin.

Type scoping gets close and then stops. A grant is scoped to a type, so `clinic_conclusion` can be a
type only the clinic may insert into — but the owner holds `write` on their own dataset and may write
that type themselves, and no reader can tell the two apart. Provenance is enforceable against third
parties and not against the owner, which is exactly backwards for the case that wants it.

A writer identity set by the server and not by the client closes it. Encoding-shaped, so it carries
the same expiry as §2 and §4. Three costs, none of them fatal and none of them free:

- It puts a principal identifier in every record, which is personal data, and interacts with erasure.
- A guest reading a record learns who wrote it, which is a disclosure the authorized view currently
  does not make. It needs a rule.
- It has to be decided whether the writer participates in the computed record version.

**Composition answers part of this without the protocol.** Where a contributor writes in its own
dataset, provenance is *which storage the record is in*, by construction: a conclusion in the clinic's
dataset is the clinic's assertion and cannot be anything else. The gap is real only where one dataset
mixes writers, which is exactly what §4 introduces.

**Adopt only where §4 is adopted, and only as far as §4 now reaches.** A deployment using
composition alone does not need it. A dataset receiving append-only contributions does, because its
contents otherwise cannot be attributed at all — but the cases §4 keeps are meters, feeds and
sensors, where *which contributor* is often answerable from the type alone. This candidate shrank
twice and should be specified last, if at all.

## 9. Retention that binds the owner

Raised by both applications: statutory accounting retention and medical record retention each require
that the holder cannot simply destroy the record, and Tessera gives the owner `erase` as a
first-class, separately granted right.

**Reject**, because it dissolves on inspection. The party with the retention obligation already holds
its own copy, and that copy is outside the other party's authority: the firm's working papers are in
the firm's dataset, the clinic's medical record is in the clinic's. Copy-back and §4 both produce
this by construction, and it is the same reason the protocol gives for keeping guest writes out.

What remains is a company able to erase its own books, which is a company breaking its own retention
obligation — the same act as destroying a paper ledger, and not something a storage protocol should
try to prevent. The protocol should not grow a lock the owner cannot open: that would collide
directly with erasure as a right, and the deployment contract is where a retention policy on
destructive paths belongs.

## Arrived from the implementation

Everything above was proposed by this assessment. Two more arrived the other way — raised while making
one storage hold more than one dataset, recorded as [review findings](tessera-spec-review.md) 15 and
16, and open. They are not restated here; the review owns them. They are listed because one of them
became a dependency of §4 and neither was on any list when the candidates were ranked.

They are numbered 11 and 12. **§10 stays retired** rather than reused, because *corrections* records
what it was and reusing the number would make that record read wrong.

### 11. How a dataset comes to exist — finding 16

Every grant in the contract is *over* a dataset, every listing lists datasets that already exist, and
erasure retires one and mints its replacement. Nothing says how a first or an additional dataset is
created. While a storage held exactly one, the gap was invisible: the dataset existed because the
storage did. It is visible now.

**This document needs it because §4 depends on it.** The shape §4 takes under composition is a small
server-side dataset the user owns whose only job is receiving contributions — and there is no portable
way for that dataset to come to exist. A deployment can invent one, which is what the implementation
does provisionally behind a route refused by default; a client that wants its second dataset cannot
rely on it.

The part that makes this larger than a route is that **the contract has no grant that could say who
may create a dataset**, since all of its grants are over one. A server-scoped grant is a bigger
commitment than anything in the extension window, and it is the piece worth deciding first. Nothing
here proposes an answer; the finding is where that happens.

### 12. Addressing a dataset that is not yours — finding 15

Not an addition and not a candidate: a question the contract has to answer either way, about whether
an identity naming a dataset the caller holds no grant on is absent, refused, or concealed.

It is recorded here because sharing-heavy deployments are where it is felt — a clinic or an
accounting firm addressing datasets across many owners is exactly the caller the question is about —
and because the answer trades disclosure against erasure being observable, which is the same trade
§9 turns on. The [review](tessera-spec-review.md) states both horns and what the implementation does
provisionally.

## What the clients said

Both were asked, because every candidate above was derived from this author's reasoning rather than
from a client's request, and [AGENTS.md](../AGENTS.md) puts them on equal terms. The question put was
the one [database-like-service](database-like-service.md) established: not whether they approved, but
whether anything here would **break** what they already do. A client's need does not decide the
contract; a client's breakage does.

Neither can be broken by any of it, which was worth establishing and is not the interesting part.
the profile-A client is a client and implements no server. the profile-B service implements no part of Tessera, runs no
deployment and ships no client; its hosting experiment links nothing into the service. So both
reviews turned into something more useful than a breakage check: four claims in this document were
falsified against running code.

**Profile A** — recorded in that client's backend repository at `docs/extension-candidates-response.md`, commit
`fef4c7e`.

| Item | Verdict |
| --- | --- |
| §1 | Would use. Deletes about 95 lines of compensation, already unreachable for its local backend |
| §2 | No view on need; objects to one assumption |
| §3 | Would not use today; cautions that subtype narrowing is its common path |
| §4 | Would not use; costs it nothing |
| §5 | No view, and the premise about the profile-A client is false |
| §6 | Would not call it |
| The collision | Prefer renaming |
| Missing | Nothing |

**Profile B** — recorded in that service's repository at `docs/extension-candidates-response.md`, commit
`3e5ab7a`.

| Item | Verdict |
| --- | --- |
| §1 | No cost, no gain; one omission to record |
| §2 | Would use — and already implements the encoding |
| §3 | Would use `immutable`; no view on the rest |
| §4 | Objects — to the justification, not the mechanism |
| §5, §6 | No view; no gap in the profile-B service |
| §7 | No view; not the integrity question reopened |
| §8 | Would not need it; answers it a layer up |
| §9 | The rejection holds |
| §10 | Would not use; the premise is wrong for Go embedders |

Both tables record the verdicts as they were put and answered. §10 was the conformance-fixture
candidate; it no longer appears above, because that answer withdrew it — see *corrections*.

## Corrections

Every claim this document made and then withdrew, collected rather than scattered, because a reader
deciding what to build needs the candidates to state their current position and needs this list to
know how much the earlier ones were worth. Most were falsified against running code or against the
specification, by a reader who checked; one was settled by a decision already recorded here. Two came
from external review rather than from a client, and one of those was a correction that introduced an
error of its own.

**§5 claimed a synchronisation motive that did not exist.** It said a journal would fix every mobile
client re-querying on each watermark change, *"which is this repository's own consumer."* the profile-A client
checked: no polling anywhere in its storage layer, and the watermark read exactly twice — recorded on
the connection, and re-read after gathering delete-safety evidence to abort if it moved, which is a
compare-and-abort guard rather than a refresh loop. The consumer named as suffering from the gap does
not do the thing it was said to do.

**§5 also claimed the cursor relationship had to be settled now** — watermark as `journal.head`,
cursors as positions in a sequence — on the grounds that retrofitting cursor semantics is expensive.
[Decisions](../specs/tessera-decisions.md) had already put and answered that question: *"addable in
v2 if incremental sync is ever needed; nothing has to be reserved now."* The argument was made
without reading the record it contradicted.

**§4's justification crossed a boundary this repository already guards.** It offered a professional
writing into someone else's dataset as a case for insert grants, and defended the right on the
visibility invariant. the profile-B service applied the four terms of the blocker its own earlier review produced —
delegated, scoped, revocable, over someone else's records — and all four hold. Visibility surviving
answers a different objection. The mechanism stands for append-only fan-in; the justification does
not, and §4 now carries the boundary instead of ignoring it.

**The worked example was the case it serves worst.** The pet application's clinic conclusion was
presented as §4's cleanest flow. Its author can neither correct nor withdraw it and cannot see what
it wrote well enough to find it, which leaves the owner holding a clinical assertion its author has
disowned. the profile-B service named it; the validation section now says so.

**§1 omitted client-side encryption.** Requiring aggregation of every server looks unsatisfiable for
a deployment that encrypts numeric values. It resolves rather than defeats — an encrypted numeric is
not a numeric to the server, so aggregation over it is a type error — but the omission was real and
the resolution is now stated in §1 rather than assumed.

**§3 called `immutable` an append-only ledger closing most of the audit-trail gap.** It is a value
that cannot be changed after it is written, and nothing more: no writer identity, no prior state, no
defence against a record being deleted or against `delete_type` with `cascade`, which *"destroys
descendant types and all their objects"*. The claim also contradicted this document's own argument
under §9, that `immutable` constrains update and not deletion. §5 had been held partly on its
strength and is now held only for want of demand.

**§4 said an insert grant's operation status discloses nothing — and the correction to it overreached
in turn.** A `committed` receipt returns the resulting identity always, so "discloses nothing" was
wrong. The first correction then said the writer learns the identity "and never the record", which
invents a prohibition the contract does not make: the body is returned *"only while the owner
currently has read authority over the record"*, so a contributor that separately holds read is
entitled to it. Both are now stated as the contract states them. This is the one entry here that
records two withdrawals of the same sentence, and it is kept that way deliberately.

### A withdrawn candidate

**A conformance fixture off HTTP** was proposed as candidate 10 and ranked first. Its premise was
that `pkg/conformance.CoreFixture` holding an `*http.Client` prevents an in-process store being
certified, and that certification therefore requires wrapping the store in an HTTP server it has no
other reason to have.

the profile-B service falsified it with running code, verified here: a Go embedder certifies an in-process core with
**no listener**, through an `http.RoundTripper` that serves each request into an `httptest` recorder
against the handler and feeds the result to `CoreFixture`. The transport was never the barrier.

The proposed remedy was wrong for a second reason. Parameterising the transport would help Go
embedders, who do not need it, and would not help a native Swift or Kotlin store at all, because the
suite is a **Go package** — such a store cannot run Go test code in its own process whatever the
transport is.

What survives is a repository gap rather than a candidate, and is recorded here so it is not
rediscovered as one. The [vectors](../specs/vectors/README.md) express *encoding*
language-independently and nothing expresses protocol *semantics* that way, so a level-1 native store
has frozen fixtures for how a value decodes and no portable statement of what a transaction, a guard
or a cursor must do. Its practical path is a test-only HTTP shim, which is cheap, already possible,
and undocumented. Worth writing down when a native client starts; not worth a candidate.

### What the reviews did not change

Two dispositions came from the reviews without any claim being wrong. §3 narrowed to `immutable` and
§7 was held for want of a consumer, both because both clients were asked and neither wanted the rest
— the standing answer this repository already gives, and the one referential integrity received.

The asymmetry worth recording: both clients answered from their own code and declined to estimate
where they had not measured. This document had been arguing from neither.

## What stays out

- **Cross-dataset query, transaction or consistent read.** Unchanged, for the reason
  [the overview](../specs/tessera-overview.md) gives — no protocol offers it without a distributed
  commit. §4 removes the case that keeps asking for it.
- **Guest `update` and `delete`.** These break the visibility invariant directly, and copy-back already
  serves them well: the recipient writes in its own dataset, grants read, and the other party copies
  what it accepts and owns it outright. That is correct wherever the copy is meant to become
  independent, which covers a professional's proposal and a firm's working papers.
- **Consent scoped to a purpose, review states, and erasure that propagates into copies.** These are
  the [adjacent service's own exchange](storage-and-exchange.md), and the boundary should hold. Copy-back is
  wrong exactly where the origin must retain continuing authority over the copy, and that is what the
  exchange's acknowledged cleanup exists for. A journal (§5) would make that layer easier to build
  **on top**, which is where it belongs.
- **Triggers, computed fields, scheduled work.** The line that keeps a minimal server implementable.
- **Semantic validation** — units, currencies, balance by meaning. This ends the property the design
  exists for, and §3 is deliberately not a step toward it.
- **Outside v1, not refused: a per-dataset label from an invitation template, the inviter's name on
  a valid invitation, and an account display name.** These are deployment surface rather than
  protocol, and were offered for the first client release on 2026-10-11. Neither the client nor its
  server needed them: each knows its studies and groups from its own records, names the inviter in
  its own share message, and shows the account name a person chose when redeeming. A display name
  for readers on a "who sees my data" list is the likeliest to return after v1.

## What the window does not cover

`specs/vectors/*.json` are frozen and append-only, verified across Go `1de0bfe` and Swift `35ad924`
with five documented platform divergences. "No production clients" does not make them free:

- **Adding cases is already permitted**, so a candidate that only widens the encoding fits the
  existing rule without suspending anything.
- **Editing or removing a case is not**, and the reasoning in [contributing](../CONTRIBUTING.md) does
  not depend on a client being in production: a changed fixture retroactively alters what a passing
  implementation was certified to do, and a reviewer does not reliably catch a flipped expectation.

Checked against the candidates above, **exactly one collides**, and only over a name.

### The collision

`values.json` carries a frozen negative case, and the example it chose to stand for an unknown type
is the name §2 originally proposed to define:

```json
{"name": "unknown_type", "input": "{\"type\":\"decimal\",\"value\":1}", "error": "invalid_field_value"}
```

| Resolution | Cost |
| --- | --- |
| **Name the type something else** — `fixed`, `money`, `exact` | None. The case stays true, because `decimal` remains a name no type has |
| **Keep the name, rely on the encoding** | No edit, but the case starts passing for the wrong reason |
| **Edit the case** | Suspends the freeze; the bill is the Swift re-verification |

The second needs stating plainly, because it looks free and is not. If `decimal` takes a **string**
value, then `{"type":"decimal","value":1}` — a bare JSON number — stays invalid, so the fixture keeps
passing untouched. What it stops doing is testing an unknown type. That is the exact failure this
directory already has a scar from: seven of the eight date cases passed for the wrong reason, and a
client shipped `dateTime` for `date_time` behind them until the one positive case exposed it.

Both clients prefer the first row and §2 now takes it, so the rest of this subsection records why
rather than proposing. If the name were kept, the honest form would be three additions rather than
none — a case asserting the string encoding succeeds, a fresh unknown-type case named something no
type will ever be, and a note in [the review](tessera-spec-review.md) recording that `unknown_type`'s
coverage moved. All three are additions, so the freeze holds either way.

### What the freeze does not block

The vectors cover values, comparisons and identifiers. Schema, request-boundary, record-set and
digest tests live in `pkg/protocol`, which is not frozen.

| Candidate | Vector coverage | Blocked |
| --- | --- | --- |
| Fold `optional_reads` into the required core | None — discovery and capabilities | No |
| Constraints in `FieldMeta` | None — schema is not in the vectors | No |
| An `insert` grant right | None | No |
| A journal and its cursor semantics | None — cursors are opaque | No |
| A grant directory | None | No |

The freeze is thinner than it sounds: 130 value cases, **17** comparison cases, 47 identifier cases.
`comparisons.json` covers eight types and holds no `date_time` or `media` case at all, so the
cross-type comparison matrix a third numeric type would widen is mostly unwritten — every case
`decimal` needs there is an addition, not a conflict.

**The genuinely blocked class is not on this list.** Anything that *removes or narrows* an existing
acceptance — dropping `float`, tightening a limit, making a currently valid value invalid — requires
deleting or editing cases, and should price the Swift re-verification before it is proposed rather
than treating the absence of clients as permission. Every candidate here only widens the encoding.

## Order

Reordered after both reviews, by consumer support rather than by the author's estimate of reach:

1. **An exact decimal type.** The only candidate a client would use, implements independently, and
   can be specified against running code. Carries two vector cases, not one. Technically independent
   of everything in the next slice; sequenced after it as a delivery choice, not a dependency.
2. **Fold the three `optional_reads` in.** No client gains much and none objects; it stands on the
   contradiction between two texts in this repository, which is reason enough and does not need a
   consumer.
3. **`immutable`, alone.** Both clients support it and neither wants the rest of §3.
4. **The `insert` right**, for append-only fan-in, with the sentence the profile-B service asked for: an insert grant
   conveys no continuing authority over what was written. Encoding-shaped, so it shares §2's expiry —
   but under composition it cannot be used until finding 16 says how the dataset receiving the
   contributions comes to exist, and that finding needs a grant the contract does not have.
5. **Provenance**, only if §4 lands, and specified after it. It shrank twice and may not survive a
   third look.
6. **The journal**, on the audit motive alone, whenever statutory retention demands it. Held because
   neither client has the gap today — **not** because §3 covers it, which it does not.
7. **The grant directory**, whenever an operator needs one. Neither client would call it today.
8. **Cross-row invariants**, held for want of a consumer, on the same footing as referential
   integrity.

Withdrawn: the conformance fixture, under *corrections*. Rejected: §9, and everything under
*what stays out*. Open elsewhere: findings 15 and 16, under *arrived from the implementation*, which
this ordering does not govern — [the review](tessera-spec-review.md) does.

**Two costs, kept apart.** *Adding later* is mixed-version compatibility while implementations
differ, plus migration of values already persisted — real, and not automatically permanent across
protocol versions. *Withdrawing after adoption* is paid by whoever relied on the guarantee, and it
falls hardest on steps 2 and 3, which this ordering once treated as free: making the three reads
mandatory excludes previously conformant servers, and removing `immutable` enforcement invalidates a
correctness guarantee rather than changing a field's semantics.

What survives of the window argument is only that no client has shipped, so *adding* is at its
cheapest now. Every step still owes a withdrawal answer before it is taken.
