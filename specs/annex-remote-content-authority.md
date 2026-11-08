# Annex: a content authority that is a separate service

**Status: experimental, non-normative, unimplemented.** Nothing here is part of the record protocol v1 or
the content protocol v1. No server is expected to implement it, no conformance fixture covers it, and a
client MUST NOT depend on it. It is kept because the problem it describes is real and the analysis was
expensive, not because the design is finished.

## Why this is not in the base contract

v1 requires **one transactional authority** over records and attachment metadata: whatever decides that
a caller may attach an item records that attachment in the same transaction as the record. That is a
constraint on authority, not on deployment — the authority may be a package embedded in a host, and the
bytes may live in any object store, filesystem or CDN, because a byte store is not an authority.

Splitting the authority is what creates the problem below. A content service holding an unattached item
cannot distinguish *the record committed and the confirmation was lost* from *the transaction rolled
back* from *the request is still in flight*, and the two mistakes are not symmetric: keeping the item
wastes space, deleting it loses a photograph silently. Everything that follows exists to replace a
timed guess with an answer.

No deployment reviewed in 2026-09 required it. The reviewed service owns attachment state in its own PostgreSQL
service behind a private byte-store adapter. The genuine case — an independently governed library such
as a user's own Nextcloud, whose users, sharing and revocation stay authoritative across several
products — is a plausible deployment class rather than a named requirement. A Nextcloud folder reached
only through one service's credentials is byte storage, not a separate authority.

## Known unresolved issues

These were raised in review and are **not** resolved in the text below. Any revival starts here.

- **Disposition identity.** A disposition keyed by `item_id` alone is ambiguous when one operation binds
  the same item more than once, or binds items from two stores with colliding IDs.
- **Generation identity.** Nothing ties a binding to a dataset generation, so behaviour across an erase
  and re-mint is undefined beyond the `storage_erased` answer.
- **The delegate's own credentials are never specified.** How a record service comes to trust a content
  authority's service identity — registration, discovery, key distribution — is absent, and it is the
  first thing an implementation would need.
- **Route coverage.** Restricting bindings to keyed routes makes every media change on an existing
  record an `:update-atomic` carrying `expected_fields`, which may be an unacceptable client burden.
- **Client-mediated reconciliation was considered as a simpler replacement and rejected as an
  equivalent.** Keeping the record visible with a pending/failed attachment is a reasonable product
  contract, but the client can crash, be uninstalled, lose the source bytes or be replaced by another
  device, and an upload whose confirmation was lost is still ambiguous. It is weaker semantics, not the
  same guarantee.

---

## The record service's side

### Registering an attachment intent

A separate content authority reconciles operations it did not issue. That needs two different things:
an **identity**, so the record service knows who is asking, and **delegated authority** over one
operation, so that identity may act on it. A binding the content authority issued establishes neither
— nothing a content service says about itself can confer authority at an unrelated record service.

Delegation is therefore registered at the record service **first**, by the authenticated caller that
will make the write, before it asks the content authority for a binding:

```
POST /v1/datasets/{s}/operations/{key}:register-attachment
{ "content_authority": "https://content.example.org",
  "items": [ { "content_store_id": "c-91ab0e77", "item_id": "91ab…" } ],
  "on_retirement": "cancel_operation" }

→ 201 { "expires_at": "2026-09-16T18:41:00.000Z" }
```

The record service durably records the **owner** — the principal and application of the caller —
together with the dataset, the operation key, the designated content authority, and the items this
operation will depend on. Then:

- **The content authority authenticates as itself**, with its own credentials, and is authorized by
  this registration alone. It may act on the operation it is registered for and on no other.
- **It receives only what reconciliation needs**: the operation's outcome and the binding relationships
  naming its own items. Never the record body, never another provider's attachments, never the rest of
  a batch.
- **A dependency is qualified by store.** One authority may host several content stores, so an item is
  identified by `content_store_id` **and** `item_id`; the authority URL alone does not make an item ID
  unambiguous, and item IDs are only store-unique.
- **The delegation outlives the caller's session.** An orphan is noticed long after the token that
  created it expired, so cleanup authority is retained with the registration rather than borrowed from
  whoever made the write.

**Registering reserves ownership without claiming execution.** They are separate dimensions, and the
spec keeps them apart:

- **Ownership is reserved.** Another caller MUST NOT register over the key, add a delegate to it, or
  claim the eventual write under it. Such a request fails `409` `conflict` with
  `details.reason: "idempotency_key_reused"` — the same answer and the same remedy, a fresh key, as
  any other collision, so a reservation is not a thing a stranger can probe for.
- **Execution state is untouched.** No write has begun, so `:status` answers `unknown`, to the owner
  and to everyone else alike. A reservation is never disclosed through it.
- **Registration, claim and cancellation MUST serialize on `(dataset, key)`.** A registration racing a
  claim must produce one order or the other. A write that claimed a key without seeing a reservation
  committed against it would defeat the reservation entirely.
- **A registration grows and never shrinks.** The owner MAY add dependencies under the same key,
  including for a second content authority, and each authority still sees only its own. A dependency
  MUST NOT be withdrawn by re-registering without it: an authority that may already have issued a
  binding cannot have its cleanup authority taken back. An identical registration is idempotent.
  Dependencies end by acknowledgement — see [Retiring an operation](#retiring-an-operation).

**What `expires_at` bounds is the reservation, not the cleanup.** Until a dependency is **live**, an
expired registration is discarded and the key becomes free: nothing claimed it and nothing referenced
it. A dependency becomes live when the delegate **activates** it below, which it MUST do before issuing a
binding, so from that moment the record service assumes one exists. Once any
dependency is live, expiry stops the registration accepting new dependencies and changes nothing else.
The delegate may still ask, retire and close, and its authority ends only by acknowledgement.

**`on_retirement` is required and has one defined value.** Writing it down is the caller accepting the
consequence: retiring a required attachment retires the *operation*, and `:batch-insert` commits all or
none, so one unreclaimable item can prevent every object in that batch from committing. A server MUST
reject a registration that omits it rather than infer consent to cancel a write.

**A delegate MUST activate its delegation before issuing a binding**, and activating is the same act
as reading it:

```
POST /v1/datasets/{s}/operations/{key}:activate-attachment
→ 200 { "items": [ { "content_store_id": "c-91ab0e77", "item_id": "91ab…" } ],
        "expires_at": "2026-09-16T18:41:00.000Z" }
```

This is a write, which is why it is not a `GET`. The same operation that answers marks the delegate's
own dependencies live, and **the expiry check and that marking MUST be atomic**: a registration already
expired at that instant is not activated and answers `404` `unavailable`, and one that activates cannot
afterwards be swept by a concurrent expiry. A weaker rule leaves a store holding a binding whose
cleanup authority expired underneath it — which is the failure the reservation exists to prevent, so it
cannot be left to a `SHOULD`.

Answered to the owner and to the designated content authority, each seeing only its own items; to
anyone else, and where no registration exists, `404` `unavailable`. The owner's call activates nothing;
it is informational. A store MUST NOT issue a binding it has not activated — a binding whose orphan can
never be resolved is worse than a refused write.

This is a requirement of remote content only. An embedded content authority authorizes inside the
record transaction and needs no delegate, no registration and no retirement.

### Retiring an operation

```
POST /v1/datasets/{s}/operations/{key}:cancel
→ 200 { "state": "cancelled" }
   or { "state": "committed", "objects": [ … ], "content_bindings": { … } }
   or { "state": "closed" }
   or { "state": "closed", "reason": "storage_erased" }
```

`:status` reports what is known *now*; it cannot establish that a commit is impossible **later**, which
is what a party waiting to reclaim a resource actually needs. This operation makes that decision
durable: *cancel this operation if it has not committed*.

- The server MUST serialize cancellation against commit. The answer is either `committed`, carrying the
  identities and the binding relationships the commit attached, or `cancelled`.
- **Cancellation is a retirement marker, not an absence.** Once returned, a delayed request bearing
  that key MUST NOT commit — including a request that had not yet arrived when cancellation was
  decided. A marker that only suppressed requests already seen would let a partitioned client commit
  afterwards. What holds that true *indefinitely*, rather than only while the marker is retained, is
  the registration requirement below.
- Markers MUST survive retry and **backup restoration**. A restore that loses them re-opens every
  cancelled operation at once.

**Who may retire an operation.** The receipt owner, or the content authority registered for it under
[Registering an attachment intent](#registering-an-attachment-intent), authenticating as itself. The
owner acts under its `write` grant; the delegate acts under the registration **alone** and needs no
grant on the dataset — handing a content service `write` so that it can clean up would give it far
more than retirement. Receipt ownership alone cannot carry the cross-service case: the point of
retiring a key before its write arrives is that there is no receipt yet to own, and ownership of an
unseen operation is unverifiable. The registration is what establishes it in advance.

- **Knowing a key authorizes nothing.** A caller that is neither owner nor registered delegate MUST
  receive `403` `permission_denied`, whether or not the key exists — including a content authority
  naming an operation it was not registered for. Otherwise any service that could learn a key, or
  persuade something to name one, could cancel a stranger's write.
- **A delegate sees only its own dependency, and is told its disposition.** Every answer to a
  registered delegate carries `bindings`, a map from its own `item_id` to `attached` or `not_attached`:

  ```json
  { "state": "committed", "bindings": { "91ab…": "attached" } }
  ```

  It receives the identities its own bindings attached to and those binding relationships, and nothing
  else; the owner's `:cancel` answers as `:status` does.
- **Reclamation follows the disposition, not the state.** A binding may be reclaimed on an explicit
  `not_attached`, on `cancelled`, or on `closed` with `reason: "storage_erased"`. A bare `closed` is
  **not** permission to reclaim — it follows a perfectly successful attachment just as readily, and a
  delayed worker acting on it would detach live content.

**A commit never becomes cancellable.** A server MUST NOT answer `cancelled` for a key it holds commit
evidence or a closed marker for; retirement and commitment are mutually exclusive for one key. The
failure this prevents is quiet: an attachment confirmation is lost, the ordinary receipt reaches
`idempotency_retention_seconds` and is discarded, the key now looks unseen, and a retirement marker
written over it reclaims a binding a committed record is still using.

A server MUST therefore retain **compact commit evidence** — the key, its owner, the registered
delegation, the outcome, the resulting identities and the binding relationships — for as long as any
registered dependency is unacknowledged, after the stored response body has been discarded on the
ordinary schedule. That evidence MUST survive backup restoration, as retirement markers do.

**A dependency ends by acknowledgement, not by time.**

```
POST /v1/datasets/{s}/operations/{key}:close-attachment
{ "content_store_id": "c-91ab0e77", "items": [ "91ab…" ] }
→ 200 { "state": "closed" }
```

The delegate calls this once it has either attached its binding or reclaimed it. Both outcomes end
here, and nothing else discharges the record service's obligation to keep evidence. It is idempotent,
and it closes only the caller's own dependencies.

**Closing a dependency never shortens the client's receipt.** The two retentions are independent and
answer different parties: the receipt answers the **client's** recovery question for at least
`idempotency_retention_seconds` from the claim, and the attachment evidence answers the **delegate's**
for as long as any dependency is unacknowledged. A key answers `closed` only once *both* are finished.
Until then `:status` and `:cancel` answer `committed` or `failed` as they always would. An attachment
confirmation that lands quickly must never cost a client the identities it has not yet read back.

`{"state": "closed"}` means **the lifecycle is finished**: no further retirement is authorized, and the
operation MUST NOT be recreated under this key. It says nothing about whether any particular binding
attached, and it is not `cancelled` and MUST NOT be read as one — a delayed duplicate arriving after
closure learns that there is nothing left for it to do, not that its write never happened.

A closed marker MUST be retained for at least `idempotency_retention_seconds` after the later of those
two windows. Past that the key answers `unknown` and the out-of-window rules in
[Reconciling an unknown outcome](tessera-protocol-v1.md#reconciling-an-unknown-outcome) apply unchanged.

**A delayed original cannot slip through as the markers expire.** The gap is real: the request that had
not yet arrived arrives long afterwards, finds the key `unknown`, claims it and commits, and the rule
forbidding a *client* to retry outside retention says nothing about a request already in flight. What
closes it is the registration, not the marker's lifetime.

**A write carrying `content_bindings` that would newly claim a key MUST find a live registration for
that key, owned by the same caller.** Otherwise it MUST be refused `409` `conflict` with
`details.reason: "attachment_registration_missing"`, and MUST NOT execute. Deduplication is evaluated
first, so a legitimate retry inside the window still replays its original outcome rather than being
refused. Retiring an operation retires its registration, so the delayed original is refused however
long it took to arrive — and no marker has to be kept forever to make that true.

For an operation with **no** registration, cancellation holds only while its marker is retained, at
least `idempotency_retention_seconds` from cancellation, after which the out-of-window rules apply to
it as to every other key. A caller that needs the unbounded guarantee registers the operation; that is
what registration is for.

An [erase](tessera-protocol-v1.md#erasure) would discard this evidence with everything else
the dataset holds. A split authority therefore also needs an obligation that survives the erase,
carrying only the erased dataset identity, the operation key, the authority and its
`(content_store_id, item_id)` references, and the terminal answer — no record content, no field values
and no object identities — and answering `{"state": "closed", "reason": "storage_erased"}` under the
old identity until acknowledged. **v1 has no such mechanism**, which is another reason this is an annex.

This exists for cross-service lifecycle — see [Content](tessera-protocol-v1.md#content) — and is the only way a second
service may conclude that work it was waiting on will never happen.


## The content store's side

**A preflight check is not a binding.** Asking "may this caller use this item" and then committing the
record separately leaves a window in which permission changes between the answer and the commit. How
the binding is made durable depends on where the content authority sits:

**Embedded** — the content authority shares a database with the record service. Authorize and create
the binding **inside the same transaction** that writes the record. A function call is not sufficient;
it must participate in that transaction, or the window reopens.

**Separate services** — no transaction spans them, and neither service may pretend otherwise. The
binding is established at the content authority **before** the record transaction opens, and the record
transaction references it.

First, though, the client registers the attachment intent at the **record service**, naming this store
by its `content_store_id` — one authority may host several, and an item ID is only unique within one —
as the content authority for that operation, see
[Registering an attachment intent](#registering-an-attachment-intent).
That registration, not the binding, is what later lets this store reconcile the operation under its own
identity: a binding it issued itself confers nothing at a record service it does not control. The store
**MUST activate** its delegation before issuing a binding, and MUST NOT issue one it could not
reconcile. Then:

```
POST /v1/stores/{store}/bindings
{ "item_id": "91ab…",
  "record_service": "https://tessera.example.org",
  "record_dataset_id": "s-7f3a…",
  "operation_id": "6f1c…",
  "use": "attach" }

→ 201 { "binding_id": "b-3e77…", "expires_at": "2026-09-16T18:41:00.000Z" }
```

A binding names the **item**, the **principal and application** it was issued to, the **destination
record service and dataset**, the **record or operation** it is intended for, and the **permitted
use**. It names an operation rather than a record because the record does not exist yet — the record
write carries the same `operation_id`, and committing it is what attaches the binding.

`record_dataset_id` is part of that identity, not a convenience. **An operation is identified by
`(dataset, key)`**, and one record service hosts many datasets, so without it a store can neither
locate the registration it must activate nor scope the binding to the dataset it was issued for.

- **Authorization happens when the content authority creates the binding.** Record commit attaches it;
  it does not re-authorize it.
- **Revocation still reaches the attachment.** Resolution checks current authority, so withdrawing
  access makes an attachment `unavailable` even after the record committed. A binding is permission to
  attach, not a permanent grant.
- **A rolled-back record transaction leaves an unattached binding.** `binding_idle_seconds` triggers
  **reconciliation, not deletion** — see below. That is the honest cost of two services: an
  unresolved orphan rather than a false claim of atomicity, and never a guess.
- **Neither service claims an atomic cross-service commit**, and neither should be described as
  providing one.
- **Reading is not sharing.** Authority to read an item does **not** include authority to create a
  further binding, or to bind it for another recipient. Onward sharing is a separate grant.

#### Reclaiming a binding nobody attached

An idle binding is ambiguous: the record may have committed and its confirmation been lost, or the
transaction may have rolled back, or it may be about to commit. Deleting on a timer resolves that
ambiguity by guessing, which is the one thing neither service may do.

1. **Record commit is atomic across three things**: the binding relationship, the operation outcome,
   and an outbox notification. Either all of them are durable or none is.
2. **The content authority receives an idempotent attachment confirmation.** A lost response is
   handled by retrying it, not by inferring anything from the silence.
3. **At `binding_idle_seconds` the content authority asks the record service to retire the
   operation** — *cancel it if it has not committed* — using
   [`:cancel`](#retiring-an-operation), **authenticating as itself under
   the registered delegation**, never as the caller that made the write. Ordinary `:status` cannot
   answer this: it reports what is true now, not that a commit has become impossible.
4. **The record service serializes that against commit** and answers either `committed`, with the
   binding relationship — including for a commit whose ordinary receipt has since expired — or durably
   `cancelled`. Cancellation must stop any delayed request from committing afterwards, including one
   that has not arrived yet.
5. **The content authority reclaims only on a disposition**, never on a state alone: an explicit
   `not_attached` for this binding, `cancelled`, or `closed` with `reason: "storage_erased"`, which says
   the record dataset is gone and the binding will never attach. A bare `closed` means the operation's
   lifecycle finished and says nothing about this binding — it follows a successful attachment just as
   readily, and reclaiming on it would detach live content. Unreachable, `unknown`, `in_progress` and a
   bare `closed` all leave the binding unresolved, and it is retried.
6. **It then acknowledges**, with
   [`:close-attachment`](#retiring-an-operation). Attaching and
   reclaiming both end there, and nothing else releases the record service from keeping evidence for
   this operation — an obligation nobody closes is one the record service goes on holding, an erase
   included.

Retirement is not consequence-free, and the consequence was authorized once, at registration: a record
write commits all or none, so cancelling an operation in order to reclaim one item discards every object
that operation would have written.

Cleanup removes the **unused binding**, never the underlying item. Retirement markers on the record
side must survive retries and backup restoration; a restore that loses them re-opens every cancelled
operation at once.

The tradeoff is explicit and deliberate: **during a partition, unresolved bindings consume quota.**
Bound admission and bound the retry work rather than converting uncertainty into deletion — an orphan
costs dataset, and a wrong reclamation costs someone their attachment.

**Until that contract is implemented, a deployment MUST fail closed on remote media writes** rather
than accept a binding it cannot make durable.

---

## Annex: writing within a row-defined view

**Status: experimental, non-normative, unimplemented**, like everything else here.

v1 decides visibility from a **grant table**, not from field values, and grants are read-only. Those
two choices together mean no write can move an object between views, so none of the rules below are
needed: a mutation is authorized once, against the grant, and never against two states.

They become necessary again only if a deployment defines a view *by the rows in it* — "objects whose
owner is me" — because then changing a field changes who can see the row. That is the design v1
declines. The analysis is kept because it is correct, and because anyone proposing row-defined views
should see what it costs first.

### The rules a row-defined view would need

Reads are filtered; writes cannot be. Filtering rows the caller already owns says nothing about whether
a *new* row may be created, or moved. A server with restricted views MUST therefore authorize every
mutation against both states:

- **Insert** — the proposed state. Creating a record whose owner, sharing state or other
  policy-relevant fields place it outside the caller's authority MUST be refused with
  `permission_denied`, not accepted and then hidden from its creator.
- **Update** — the existing state **and** the proposed one. A caller must be authorized over the row as
  it stands, and the change MUST NOT move it into or out of a view the caller could not otherwise
  reach. A row the caller cannot see is `unavailable`, exactly as on read.
- **Delete** — the existing state, per row.
- **Filtered mutations** — `:update` and `:delete` affect only rows inside the caller's view. Rows
  outside it MUST NOT be modified and MUST NOT be counted in the reported total.
- **Atomic batches** — if any target is outside the view, the whole batch MUST fail before any write,
  with `unavailable`. Partially applying a batch because one row was hidden would break the all-or-none
  guarantee the operation exists for.
- **Schema mutations** — a cascade destroys rows the caller may not be able to see. A server MUST
  refuse a schema change whose effect reaches outside the caller's authority, rather than deleting
  hidden rows on their owners' behalf.

**Authorization and the write commit together.** A decision evaluated before the transaction can be
true when made and false when applied, so the check MUST be made inside the transaction that performs
the write, against the state that transaction sees.

