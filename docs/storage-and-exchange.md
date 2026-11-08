# Storage and exchange

Tessera stores records and enforces access. A separate exchange coordinates review,
purpose-scoped consent, delivery, corrections and cleanup across services. An application
may use both; adding exchange semantics to the store would couple independent contracts.

## Quick guide

Use [consuming Tessera](consuming-tessera.md) to embed storage behind an existing service.
Before proposing a workflow feature, check whether it changes stored values or instead
coordinates people, purposes and copies across ownership boundaries. The latter belongs
to the exchange. The comparison below explains that boundary.

## They answer different questions

| | This storage protocol | The adjacent record exchange |
| --- | --- | --- |
| Question | *Where do my records live, and how do I query them?* | *How does a record cross from one party to another?* |
| Ownership | Does not change. One owner throughout. | Crossing an ownership boundary is the point. |
| Trust model | The user chose this destination for their own data. | Delegated, scoped, revocable authority over someone else's. |
| Core operations | Schema, CRUD, structured query, pagination, aggregation. | Submit, receive, review, correct, withdraw, acknowledge cleanup. |
| Review | None. A write is a write. | Central. A submission is a proposal until a reviewer accepts it. |
| Consent | Configuring a storage is the consent. | Per-grant, per-subject, per-purpose, with expiry. |
| Shape | Query-heavy request/response. | Submission lifecycle with receipts. |

Merging them would put consent, review and proposal semantics into a protocol whose job is to answer
`SELECT`-shaped questions, and query semantics into a protocol whose job is to move a record across a
trust boundary safely. Both would get worse.

For how to actually consume one from the other, see
[consuming Tessera](consuming-tessera.md).

**Conclusion: two protocols, deliberately.** Not a duplication — a separation that model already
anticipates, since *personal store* and *consumer* are distinct profiles in its own vocabulary.

## The application needs both, and they are not alternatives

This is the practical consequence, and it is easy to get wrong:

- **Storage** is where a user's records live. A profile-A client reaches it through one interface,
  the same one its local backend implements. This protocol.
- **Sharing with a professional** is a separate, consented action producing a copy somewhere else,
  under review. That is the exchange's submission profile, and it is not a storage operation.

That service's own interoperability decision already says this: the user chooses storage —
phone-local, a personal-record backend, or another provider — and *sharing is a separate
permission*, scoped to a professional, records, range, purpose and expiry. A storage backend that
silently made records visible to a professional would violate that; an exchange submission that
silently became the user's primary storage would violate it in the other direction.

## Where such a service could fit

| Option | What it means | Assessment |
| --- | --- | --- |
| **Consumer only** | A profile-A client sends approved records through the existing exchange submission profile. The service implements no storage protocol. | **Near-term path.** The contract exists and is implemented; the work is a client in the app, not a server here. |
| **Personal store** | The service implements this protocol, so a user can choose it as where their records live. | Largest integration. Requires the full query surface, which is a real amount of work for a service whose centre of gravity is review workflow. |
| **Sharing store** | The service hosts the shared copies a professional reads, addressed through this protocol with per-storage grants. | Plausible middle. This protocol already carries per-caller `read`/`write`/`schema`/`erase` grants, which is most of what a sharing store needs. |

Nothing here commits it to any of them. Its own notes are explicit that its hosting remains an
optional candidate rather than an implemented backend for such a client, and that compatibility
awaits a model review on both sides.

## Where the two contracts should agree

Independent protocols, shared conventions. Divergence here would be accidental, not meaningful.

- **Bearer transport, opaque token.** This protocol advertises `auth_methods` and defines
  `bearer_token`; the exchange issues RS256 JWTs with issuer, audience, expiry and scopes. A JWT
  *is* a bearer token, so a service implementing both needs one credential path, not two. Any future
  method is added to `auth_methods` rather than to the transport.
- **Bytes never travel through the API.** Both arrive at the same place from different directions:
  the exchange's production guidance is direct signed object-storage transfer after API authorization,
  and
  this protocol resolves an opaque reference to a short-lived URI. A shared byte store is realistic.
- **Idempotency through a caller-supplied operation ID.** The exchange uses a command UUID that is
  also the
  receipt ID, so an offline producer can reconcile after a lost response; this protocol uses an
  idempotency key bound to a payload digest with a status endpoint for the same purpose. The
  principle is identical and should stay so: **a lost response is never a reason to guess.**
- **Advertise only complete profiles.** The exchange's rule — a service may implement several
  profiles but
  must fully implement the lifecycle guarantees of each it advertises — is the same rule this protocol
  states as capabilities being promises rather than aspirations. Keep the wording compatible.
- **Erasure is acknowledged, never assumed.** The exchange tracks remote cleanup as pending until
  acknowledged and refuses to let elapsed time become success. If a user's records live on a store
  reached through this protocol and are erased, the same discipline applies.

## Where they must not be made to agree

- **Do not import review into storage.** A storage backend has no opinion about whether a record is
  true, complete or accepted. Adding proposal states here would make every backend implement review
  workflow.
- **Do not import query into exchange.** A submission receiver does not need grouped
  first-per-concrete-type selection.
- **Do not share a version number.** They will move at different speeds, and a service implementing
  both should be able to upgrade one without the other.
