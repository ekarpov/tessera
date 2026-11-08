# Tessera Content Protocol, version 1

**Status:** draft for review. Versions independently of the
[dataset protocol](tessera-protocol-v1.md) — a record backend is fully conformant without
implementing any of this, and a content service need not run on the same host or be written by the
same people.

A server implementing this protocol is a **Tessera content store**: it holds opaque byte resources
and hands out short-lived URIs to read and write them. It has no opinion about what the bytes are.

**MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT** and **MAY** are used in the RFC 2119 sense.

## What this is, and what it is not

A record carries an opaque `ContentRef` — a store ID and an item ID — and **never** bytes and never a
URL. Resolving that reference to something transferable is this protocol's whole job.

Three consequences shape everything below:

- **Bytes never traverse the control API.** Every transfer happens directly between the client and a
  byte store, at a URI the control API issues. This is what lets one deployment use object storage with
  presigned URLs and another serve from its own disk, with the same client.
- **The service never fetches a URL the client supplies.** There is no "import from this address"
  operation, in either direction. A service that fetched client-supplied URLs would be a request
  forwarder positioned inside someone's network.
- **Nothing here interprets content.** Deduplication by hash, media kinds, lifecycle states and
  transfer checkpoints are the application's, held in its own records. A store that guessed at them
  would be wrong for the next application.
- **Every URI this protocol issues MUST be `https`, unless its host is a loopback address.** That
  covers the resolved read URI and the transfer URI, and it is stated here rather than beside each
  because a rule stated per use is a rule the next use is missing. Loopback is `127.0.0.0/8`, the
  IPv6 address `::1` however written, and the name `localhost`; a private or link-local address is
  not loopback. A client MUST refuse a URI that fails this and MUST NOT fetch it first — the check
  is on the URI, before the request, not on what comes back.

  This is the same rule the [record protocol](tessera-protocol-v1.md#discovery) states for every URL
  it hands a client, repeated because the two documents version independently and a reader of this
  one would otherwise never meet it. **It matters more here.** There, a plaintext URL is a
  destination that credentials would later be sent to; here the URI *is* the credential — see
  [below](#the-resolved-uri-is-a-credential) — so observing it in flight is the whole attack, and
  the bytes it protects are the records themselves.

## Transport and discovery

HTTPS. JSON control requests. Paths begin `/v1/`.

```
GET /.well-known/tessera-content
```

```json
{
  "protocol_versions": ["1"],
  "server": { "name": "tessera-contentd", "version": "0.1.0" },
  "auth_methods": ["oauth2", "bearer_token"],
  "authorization_servers": ["https://tessera.example.org"]
}
```

Authentication is as in the [dataset protocol](tessera-protocol-v1.md#authentication):
the same bearer presentation, and normally the same authorization server, so one sign-in covers
records and their attachments. A dataset's `content_store_id` in the record service's listing names
the store its records refer to; `GET /v1/stores` lists the stores this caller may use.

```json
{
  "stores": [
    {
      "id": "c-91ab0e77",
      "capabilities": ["writes", "thumbnails", "ranges", "current_representation"],
      "transfer_modes": ["offset", "whole"],
      "access": { "read": true, "write": true, "delete": false },
      "limits": {
        "max_item_bytes": 2147483648,
        "max_chunk_bytes": 1048576,
        "min_chunk_bytes": 262144,
        "url_lifetime_seconds": 300,
        "import_idle_seconds": 600,
        "import_retention_seconds": 86400
      }
    }
  ]
}
```

A store the caller may not use is **absent**, never listed as inaccessible — as in the record
protocol, and for the same reason.

**Import ownership is determined by the logical store.** A store used by records belongs to exactly one
record dataset, configured by the shared authority before imports are accepted. Each reservation and
item inherits that association; clients cannot override it. Several datasets may use the same physical
byte backend only through distinct logical stores. A standalone content store is its own dataset;
attaching it to a record dataset requires an empty store with no reservations, then the association is
immutable until retirement. Record-dataset discovery supplies the associated `content_store_id`.

| Capability | Meaning |
| --- | --- |
| `writes` | Imports and deletion are available. A read-only store — an archive, a mirror — omits it. |
| `thumbnails` | Bounded image renditions can be resolved without transferring the original. |
| `ranges` | Resolved read URIs honour HTTP `Range`. Without it a reader transfers whole items. |
| `current_representation` | The store distinguishes an item's `current` rendition from its `original`. Without it, only `original` exists. |

## Identity

An **item ID** is opaque, server-assigned, at most 255 bytes, free of control characters, and
percent-encoded in paths. Clients MUST NOT parse one, infer structure from one, or treat one as a file
path — a store MAY encode resource selection inside it.

**An item ID MUST NOT be reused**, including after its item is deleted. An application holds these
durably inside records: a reused ID would silently resolve a stale reference to unrelated bytes, which
in this protocol means showing someone another person's photograph. An item ID MUST NOT contain `//`,
because a record refers to it qualified by its store.

**A record names an item as `<store_id>//<item_id>`**, and that is the form a `media` value carries —
see the [record protocol](tessera-protocol-v1.md#content). Non-reuse is a promise *within* one store,
and a replacement store created by an erase starts an empty ID space, so the store must travel with
the item or the promise above does not survive a content-only erase. A resolver MUST use the store
named in the reference rather than whichever store a dataset currently points at.

**A retired store answers, it does not disappear.** A reference into a store that has been erased and
replaced resolves through the ordinary three answers — absence, refusal, or `unavailable` for anything
the caller cannot place. It MUST NOT be silently redirected to the replacement, and a store MUST NOT
accept a resolve for an item ID it has no record of merely because the ID looks plausible.

## Reading

### Inspecting without transferring

```
GET /v1/stores/{store}/items/{item}?representation=original
→ 200 { "content_type": "image/jpeg", "byte_count": 184232, "representation": "original" }
```

`byte_count` MAY be `null` when the store genuinely does not know it. It MUST NOT be a guess.
This call issues no URI and MUST NOT be billed or rate-limited as a transfer.

### Resolving a URI

```
POST /v1/stores/{store}/items/{item}:resolve
{ "representation": "original" }

→ 200 {
  "url": "https://bytes.example.org/o/91ab…?X-Amz-Signature=…",
  "method": "GET",
  "headers": { },
  "expires_at": "2026-09-16T18:41:00.000Z",
  "accepts_ranges": true,
  "content_type": "image/jpeg",
  "byte_count": 184232
}
```

`representation` is `original`, `current` (requires `current_representation`), or `thumbnail` with
`maximum_pixel_size` (requires `thumbnails`). If the required capability is not advertised, resolve
MUST fail `403 not_supported`. If it is advertised but the representation cannot be produced for this
item, resolve MUST fail `415 unsupported_representation`. `original` remains required. Neither failure
permits silently substituting another representation — the original and an edited
rendition are different resources, and quietly returning one for the other corrupts what the
application believes it stored.

**A thumbnail MUST NOT require the original to be transferred to the client.** If a store cannot
produce one without that, it does not have the `thumbnails` capability.

### The resolved URI is a credential

For its lifetime, whoever holds it can read those bytes.

- Lifetime is minutes, and MUST NOT exceed `url_lifetime_seconds`.
- It grants exactly one item, one representation and one method.
- **It is `https`, or its host is loopback**, per the transport rule above. A credential issued in
  the clear is one every intermediary on the path holds for as long as it lives, and the client that
  was handed it chose neither the origin nor the scheme.
- Clients MUST NOT persist it, place it in logs or diagnostics, or pass it outside the process that
  requested it. The application persists the `ContentRef` and resolves again; that is why a
  `ContentRef` is documented as not being a URL.
- **Revoking a grant does not invalidate URIs already issued.** A store MUST stop issuing new ones
  immediately and MAY let outstanding ones expire. Any product statement about revocation must say
  this rather than imply instant cut-off.

## Writing

Requires `writes` and the `write` grant. The shape follows the application's existing import contract:
one durable operation, identified by the caller, resumable across sessions and crashes.

### Beginning or resuming

```
POST /v1/stores/{store}/imports
{ "operation_id": "6f1c…", "content_type": "image/jpeg", "byte_count": 184232, "maximum_bytes": 2097152 }

→ 200 {
  "upload_id": "u-41bc…",
  "state": "open",
  "received_bytes": 0,
  "url": "https://bytes.example.org/u/41bc…?…",
  "method": "PATCH",
  "headers": { },
  "expires_at": "2026-09-16T18:41:00.000Z"
}
```

- **`operation_id` is scoped to the logical store**, with the claiming principal and application
  authorization retained as its owner. Only that owner may resume, commit or abort it; another
  principal or application authorization MUST receive `403 permission_denied`, never the import or
  its item identity. A different store has an independent operation-ID space.
- **`operation_id` is the caller's, and identifies the import.** Repeating an open import MUST return the same
  import — the same `upload_id`, the current `received_bytes`, and a fresh transfer URI — so a client
  that crashed mid-transfer resumes rather than restarting. This is the whole reason the caller
  supplies the identifier instead of the server.
- Repeating it with **different** metadata — another `content_type` or `byte_count` — MUST fail `409`
  `conflict`. One operation describes one resource. Changing `maximum_bytes` on replay MUST also
  fail `409 conflict`, even when both ceilings exceed the received bytes or the store limit. The
  ceiling is fixed at reservation so a resume cannot silently revise the bound another in-flight
  transfer is enforcing. Omitting `byte_count` and declaring it are different requests even after
  the total has been established. These checks also apply to committed imports.
- `byte_count` MAY be omitted when unknown. `maximum_bytes` is the caller's own ceiling; the effective
  limit is the lesser of it and `max_item_bytes`.
- After a successful commit, repeating `operation_id` MUST return `{"state": "committed", "item_id": …}`
  for at least `import_retention_seconds` — which is what makes a lost commit response recoverable.

### Transferring

The client sends bytes directly to `url`, in chunks between `min_chunk_bytes` and `max_chunk_bytes`,
each carrying `Content-Range`.

- **A chunk below `min_chunk_bytes` is permitted only when it completes the item**, which also covers
  an item small enough to send in one chunk. The minimum exists to stop a client trickling a large
  upload, not to make a small one impossible — the example above is 184,232 bytes against an advertised
  minimum of 262,144. A store MUST accept an undersized chunk that completes the item and MUST reject
  one that does not.
- **The completing chunk names the total.** While the total is unknown a chunk carries
  `Content-Range: bytes 0-262143/*`; the chunk that completes the item MUST carry a concrete total —
  `bytes 262144-446375/446376` — and the store MUST check that against the bytes it actually received
  rather than trusting the claim. A client that declared `byte_count` when it opened the import MAY use
  that total throughout. This is what marks the end of an item whose length was unknown; there is no
  separate flag.
- **An identical retry at the same offset MUST succeed.** A dropped connection is the normal case, not
  an error, and re-sending the last chunk MUST NOT duplicate it.
- **A conflicting offset, or different bytes at an offset already written, MUST fail** rather than
  overwrite. A resumed caller MAY replay from offset zero; bytes already persisted and identical MUST
  NOT be stored twice.
- **A retry is identified by its offset and its bytes, not by the whole request.** So a retry MAY
  carry a concrete total where the earlier send carried `*`, and where that retry completes the item
  it establishes the total. A store MUST accept it.

  This is not a corner. It is the first thing a client reaches for on an unknown-length import: the
  completing chunk must name a total, and a client streaming from a source that will not state its
  length does not know which chunk is last until the stream ends, so it sends the last one again
  with the total filled in. Neither rule above reaches that case — it is not an identical request,
  and it is not different bytes — and which way a store answered depended on what it happened to key
  idempotency on. A client that read the retry rule as settling it wrote a defect on the strength of
  it.

  A total that contradicts one already established is still `409 conflict`, which is the rule above
  and not an exception to it.

  **The identity is the import, not the transfer URI.** A client that lost its URI — an expiry, a
  restart — rejoins by replaying the import, which returns the same upload with a fresh URI and the
  store's own `received_bytes`. A chunk re-sent to that fresh URI is the same retry as one re-sent
  to the original: the offset and the bytes decide, and where the request arrived does not. A store
  MUST NOT treat rejoining as starting something else.
- **Limits are enforced during streaming**, not after the store has buffered an oversized body.
  Exceeding `maximum_bytes` or `max_item_bytes` MUST abort the transfer at the point of exceeding it.
- A transfer URI expires like any other. `POST` the same `operation_id` for a fresh one; `received_bytes`
  tells the client where to continue.

**What a transfer URI is, and what it is not.** The contract is that *the authority issues a
short-lived authorized URI*. It is **not** that every byte store can presign its own origin for this
protocol. Offset-resumable `Content-Range` transfer is not what an S3-style presigned `PUT` offers —
those cover whole-object `GET`, `PUT`, `HEAD` and `DELETE`.

**A store advertises which transfer modes it offers**, and an import names the one it will use. A
store advertising `writes` MUST offer at least one.

| Mode | Transfer | Resumable |
| --- | --- | --- |
| `offset` | The `Content-Range` protocol above, chunk by chunk | Yes, from `received_bytes` |
| `whole` | One request carrying the entire item, to the issued URI | No — a failure restarts |

`whole` exists so a store can hand out a presigned `PUT` and stay out of the byte path entirely. That
is the shape an object store offers natively, and requiring resumption of every store would force a
transfer gateway on deployments that want direct-to-provider uploads — a real cost, and not one this
protocol should impose to buy a property some clients do not need.

**Both modes are genuine, and a client must handle the one it chose.** A client uploading a large item
over a mobile network should prefer `offset` and treat its absence as a reason to look elsewhere; a
client uploading small items may not care. `import_idle_seconds`, idempotent commit and the
`operation_id` → `item_id` mapping work identically in both, so a lost *response* is recoverable
either way. Only a lost *transfer* differs.

A store MAY implement `offset` over a provider that cannot resume by terminating transfers at its own
endpoint and translating — onto that provider's multipart upload, or a write-through of its own —
keeping the part bookkeeping itself. A client never performs a native multipart transfer, because
nothing here specifies one and "uses multipart" is not an interoperable contract.

Reads are the easy direction: a plain presigned `GET` is a fine resolved URI wherever the provider
offers one.

### Committing, abandoning

```
POST /v1/stores/{store}/imports/{upload}:commit   → 200 { "item_id": "91ab…" }
DELETE /v1/stores/{store}/imports/{upload}        → 204
```

- **Commit is idempotent** and MUST return the same `item_id` every time for that operation.
- **An item has exactly one total, and it is fixed once established** — by `byte_count` when the import
  opened, or otherwise by the concrete total on the chunk that completed it. A chunk naming a different
  total MUST fail `409` `conflict`, and a completing chunk MUST agree with any `byte_count` declared up
  front. A retry cannot revise it.
- **Commit MUST fail until a total is established and exactly that many bytes have been received.** An
  unknown-length import interrupted before any chunk completed it therefore has no total and is not
  committable: it is resumed or abandoned, never committed on the strength of whatever happened to
  arrive. A truncated item MUST NOT be committed and later discovered.
- **An empty item requires `byte_count: 0` at reservation.** It commits without a transfer. An
  unknown-length import MUST NOT commit as an empty item in either transfer mode: no completing
  nonempty range can establish zero. Commit without an established total MUST fail `409 conflict`;
  the caller must abandon it and reserve a fresh operation with declared zero, rather than treat
  an interrupted transfer as successful emptiness.
- **Abort removes staging only.** It MUST NOT touch an item that has already been committed, in this
  operation or any other. An abandoned import that is never aborted MUST be reclaimed after
  `import_idle_seconds`. Abort or reclamation MUST invalidate further transfer and commit attempts
  and retain an operation-ID tombstone for at least `import_retention_seconds` from that event.
  During retention an owner replay with the original parameters MUST fail `410 expired`, never
  create a new import; conflicting parameters remain `409 conflict`. Clients MUST use a fresh
  operation ID after abort or reclamation and MUST NOT reuse an old ID after retention either.
  After retention the server may forget the outcome; absence then proves nothing about the old attempt.
- An import that is neither committed nor aborted holds no item ID, and its bytes MUST NOT be
  reachable by any read.

### Deleting

```
DELETE /v1/stores/{store}/items/{item}   → 204
```

Requires the `delete` grant, which is **not implied by `write`**: adding an attachment and destroying
one are different authorities.

Deletion MUST make the bytes unreachable through this protocol immediately. A store fronted by a cache
or a CDN MUST state its propagation bound; "deleted" that means "no longer served by the origin" is a
weaker promise and must be described as one rather than assumed.

## Erasure

```
POST /v1/stores/{store}:erase   { "confirm": "{store}" }   → 200 { "store_id": "c-…" }
```

Erasure MUST reject `Idempotency-Key` with `400 invalid_request` before retirement and creates no
erasure receipt. Confirmation and replacement discovery below are its recovery mechanism.

Destroys every item in the store. As in the record protocol, the empty store that remains takes a
**newly minted identity** which MUST NOT be reused, and `confirm` doubles as the retry guard. **Grants
do not transfer**: the replacement starts with whatever grants administration assigns it, and at
minimum the erasing caller's, as in the record protocol. That floor is not optional — a store the
caller may not use is absent from `GET /v1/stores` rather than listed as inaccessible, so without it a
caller could erase a store and then be unable to see that a replacement exists at all.

**After a lost response the replacement is identifiable rather than guessed**: the new store's entry in
`GET /v1/stores` carries `replaced_store_id`, naming the identity it replaced, for at least
`import_retention_seconds` after the erase. A client that reaches this store through a record storage's
`content_store_id` never needs it, since that field is updated in the same transaction — but a
standalone content client has nothing else to match on, and matching on a display name is not a
mechanism.

**On return, access is revoked and deletion is durably scheduled** — which is what success means,
not that bytes are already gone. One transaction retires the store, refuses new reads, imports and
finalizations, stops issuing transfer URIs and durably enqueues the deletion of every object it holds. The worker that deletes bytes is the store's business: there is no erasure handle and
nothing for a client to poll, and the enqueued work MUST be committed with that transaction, retried
until it succeeds, and survive restart and restoration. The erase requires the `delete` grant.

That durability is what lets the contract stay quiet, so it MUST reach what an erase can otherwise
miss: **an import whose transfer URI was issued before the erase**, which could write bytes back under
a deleted key unless expiry and reconciliation prevent it; derivatives and thumbnails; **object versions**, since a delete in a versioned bucket may
only insert a delete marker; CDN caches; and backups, whose restoration MUST NOT revive the store. A
store MUST document which of these its byte backend covers, because the honest scope of the promise
differs between a filesystem and a CDN-fronted bucket.

**Already-resolved URIs and downloaded copies are outside the promise** — see the revocation limit
below. A transfer already running may outlast its URI's expiry, which bounds when a URI may be
presented rather than when a connection is cut. An erase destroys what the service holds; it does not
reach what has left it, and no waiting changes that.

**An item belongs to one dataset, from reservation onward.** Ownership attaches when an import is
reserved, not when a record references the result, so an erase MUST reach committed items, items no
record ever attached, items whose reference was replaced, and abandoned imports still in staging. A
store MUST NOT hold an item that two record datasets independently own, because it cannot both survive
for one and be destroyed for the other.

**Erasing the record storage erases this store with it**, so a dataset is destroyed by one call. This
call is the other direction, which does not cascade: it replaces the store for a live record dataset
without touching its records. An application that makes both calls separately reports the outcome of
each, since one can succeed while the other fails.

## Encryption

Because nothing here ever queries content, a client MAY encrypt an item before uploading it. A store
MUST treat item bytes as opaque and MUST NOT require a recognisable format, inspect content to
determine a type, or reject bytes that do not match the declared `content_type`.

This is the one place where end-to-end encryption costs nothing: the capability trade-off that rules it
out for records does not exist for attachments. A store cannot produce thumbnails for encrypted items,
which is the whole price.

## Errors

The body shape and the general codes are those of the
[dataset protocol](tessera-protocol-v1.md#errors). Content-specific codes:

| HTTP | Code | Application meaning |
| --- | --- | --- |
| 400 | `invalid_request` | Malformed control request, bad range, bad representation arguments |
| 403 | `permission_denied` | The caller lacks `read`, `write` or `delete` |
| 403 | `not_supported` | The store lacks the capability — `writes`, `thumbnails`, `current_representation` |
| 404 | `not_found` | No such item or import. **Absence only**, and only where the caller may see the store at all |
| 404 | `unavailable` | Outside the caller's authorized view: a store whose dataset the caller cannot reach, or an item no record they may read names. Reveals nothing about existence, and is indistinguishable from an identifier naming nothing |
| 409 | `conflict` | Conflicting import metadata, or conflicting bytes at an offset |
| 410 | `expired` | The transfer/read URI expired, or an import was aborted or reclaimed; refresh a URI, but use a fresh operation ID for a terminated import |
| 413 | `limit_exceeded` | Beyond `max_item_bytes`, `maximum_bytes` or a chunk bound |
| 415 | `unsupported_representation` | The capability is advertised, but the store cannot produce that representation for this item |
| 429 | `rate_limited` | With `Retry-After` |
| 503 | `store_unavailable` | The store or its byte layer is unreachable |

**Both `404`s reach item routes, and which one a caller can see depends on their view.** The view is
consulted before existence is, so a caller whose view does not reach an item is told `unavailable`
whether or not the item exists — an identifier that was never issued and one they may not see are
the same answer to them, which is the point. A caller whose view does reach the store is told
`not_found` when the item is genuinely absent, including when it has been deleted.

So a client mapping these needs both on item routes, not only on store routes, and cannot infer
from a `404` alone which it received. `code` is the only thing that distinguishes them.

**A client that will not use the network** — the application's read options can forbid it — MUST NOT
call this protocol at all rather than expecting the store to detect the intent. Every operation here
is a network operation by construction.

## Authorization, and what it cannot promise

An item has an authorized view of its own, and the same three answers apply: `not_found` for absence,
`permission_denied` for a refused operation, and `unavailable` for an item outside the caller's view,
returned regardless of whether it exists.

**Knowledge of an item ID confers nothing.** Access derives from an **authorized attachment**, not
from a reference existing. The attachment is made when a record comes to reference an item, and it is
authorized **at that moment**: the writer MUST be authorized over the item, or the write is refused.
Otherwise a caller who learned an item ID — from a record shared with them once, from an export, from a
log — could reference it in a record of their own and read someone else's photograph by reading their
own record.

Resolution then checks that an authorized attachment exists for this caller, not merely that the caller
can read *some* record naming the item. A caller without one gets `unavailable`, with no distinction
between an item that never existed and one it may not see.

**Reading is not sharing.** Authority to read an attachment does **not** include authority to attach
that item to another record, to attach it in another dataset, or to widen who can reach it. Creating an
attachment relationship and expanding its recipients are separate permissions, checked separately —
otherwise anyone who could see a photograph could re-attach it somewhere they control and hand it on.

**The attachment commits with the record.** A preflight check is not an attachment: asking "may this
caller use this item" and then committing the record separately leaves a window in which permission
changes between the answer and the commit. v1 closes it by requiring **one transactional authority** —
the authority that decides a caller may attach an item records that attachment in the same transaction
that writes the record. A function call into a content package is not sufficient unless it joins that
transaction.

That constrains authority, not deployment. The authority may be a package inside the record server, and
the bytes may live anywhere this protocol can address, because **a byte store is not an authority**: it
holds bytes and honours a transfer URI that the authority issued. Byte transfer therefore happens
outside the transaction and MUST NOT be attempted inside one.

A deployment whose content authority cannot join the record transaction MUST refuse media writes. The
analysis of what it would take to make that safe is in
[the annex](annex-remote-content-authority.md) — experimental, unimplemented, and deliberately outside
v1.

A store deployed independently of any record service cannot evaluate attachments on its own, and MUST
be configured with its own explicit grants rather than defaulting to open access.

**Revocation is not instant, and the composed system must not claim otherwise.** A resolved URI stays
valid for its lifetime: a store stops issuing new ones immediately and MAY let outstanding ones expire.
Withdrawing access to a record therefore withdraws the ability to *obtain* a URI for its attachments,
not the ability to use one already obtained. Any product statement about revoking a shared attachment
must be bounded by `url_lifetime_seconds`, which is the reason that limit is minutes rather than
hours.

## Conformance

A conformant content store:

1. Implements inspect, resolve and read for `original`, plus every capability it advertises, and
   refuses the rest with `403` `not_supported`.
2. Never reuses an item ID, including after deletion.
3. Resumes an import from its `operation_id`, reports `received_bytes` honestly, accepts an identical
   chunk retry at the same offset, and refuses conflicting bytes.
4. Commits idempotently to one `item_id`, refuses to commit a short item, and keeps the
   `operation_id` → `item_id` mapping for at least `import_retention_seconds`.
5. Enforces size limits during streaming, not after buffering.
6. Never serves bytes of an uncommitted import.
7. Issues read and transfer URIs bounded by `url_lifetime_seconds`, scoped to one item and one method.
8. Treats item bytes as opaque, never inspecting them to determine type or validity.
9. Never fetches a URL supplied by a client.
10. Distinguishes absence from refusal.
11. Authorizes an attachment and records it in the same transaction as the record, and refuses a media
    write it cannot authorize there.
12. Denies new access immediately on erasure, commits the deletion work durably in that same
    transaction, and finishes it afterwards — including across a restart part-way through cleanup.

The conformance suite is the definition of these requirements, and MUST include the negative cases:
a resumed import after a killed transfer, a conflicting chunk, an oversized item cut off mid-stream,
a commit short of a declared length, an unknown-length import interrupted before any chunk completed
it and therefore not committable, a completing chunk whose total contradicts one already established,
a read of an uncommitted import, an expired URI, a refused item
that must not look deleted, a repeated commit returning one identity, a rolled-back record transaction
leaving no attachment, a transfer URI issued before an erase that MUST NOT write bytes back afterwards,
a restored backup that MUST NOT revive an erased store, and deletion work that survives a restart
mid-cleanup. Ownership cases MUST reject two datasets sharing one logical store and the reassignment of
a store holding reservations, and MUST prove that separate logical stores over one byte backend isolate
imports and erasure, unattached items included.

Author-resolution cases (review findings 7, 8 and 10) MUST additionally prove:

- An operation ID resumes one import in a logical store for its owning principal and application
  authorization; another principal or application cannot resume, commit or abort it. Another store
  has an independent ID space. Changed metadata, presence of `byte_count`, or `maximum_bytes`
  conflicts, including after commit and when two ceilings imply the same effective bound.
- Abort and idle reclamation survive restart, refuse unchanged owner replay with `410 expired`
  throughout retention, and prevent old transfer and commit attempts from reviving staging bytes.
- Declared-zero imports commit without transfer and replay one item identity. Unknown-length empty
  imports cannot commit in either mode and never become readable.
- Both optional representation kinds fail `403 not_supported` when unadvertised and
  `415 unsupported_representation` when advertised but unavailable for an authorized item. Available
  representations resolve as requested; neither refusal substitutes the original.
- An erase with an idempotency key fails without retirement. A lost unkeyed erase response is
  reconciled through replacement discovery, and a repeat against the old store cannot erase its
  replacement.
