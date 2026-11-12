# Embedding the core, and extending it

How a service builds on this backend without forking it, and what the core must
expose for that to be possible. Decided 2026-09-16. Two of the seams shape interfaces the query engine
and write path are built on, so they are cheap now and expensive later — but none of them is worth
anything without the concrete semantics below.

The [protocol](tessera-protocol-v1.md) stays independent and implementable by anyone. What
changes is that the reference implementation becomes a composable core rather than only a server.

## Three levels of adoption

| Level | What you do | What you get | What you pay |
| --- | --- | --- | --- |
| **1. Speak the protocol** | Implement the HTTP contract in any language; pass the conformance suite | A client application can use you as a dataset | Everything, from scratch |
| **2. Embed the core** | Import the Go packages, run them inside your own binary, supply four interfaces | The whole protocol surface, already conformant | You are in Go, and you accept the core's shape |
| **3. Build domain extensions on it** | Level 2, plus your own contracts advertised as extensions | Sharing, review, publication, lifecycle — your product | The substantial part: each extension is a contract with its own conformance tests |

Levels 2 and 3 are deliberately separate. Embedding is cheap; the domain work on top is not, and
calling both "adoption" hides which one a schedule is actually committing to. A service hosting records for other people's clients needs both.

## The seams

### 1. An authorized access context, mandatory at the store boundary

Every operation carries the caller's context, and the store applies it **before** grouping,
aggregation, pagination or limits — never as a filter over results already fetched. It governs
object lookup, queries, counts, aggregates, distinct references, content resolution, updates,
deletes and schema changes. A scope that only guards reads leaves `:count` and `:aggregate` leaking
membership without returning a row.

- **A constrained representation, not host SQL.** The host describes scope in terms the planner
  understands, so the planner can compose it with the query's own predicates and still reason about
  bounds. Accepting arbitrary SQL would surrender both the bounds and the portability between
  engines.
- **Cursors bind to the principal, that principal's application authorization, and the view's
  generation** — two installations of one person may see different data. The generation changes
  when effective grants or application authorization change, including expiry. Ordinary updates
  leave collection membership fixed and do not change this generation. The
  generation is re-checked before every page, replays included, and a stale cursor is
  `410 view_changed`. This is why the replay guarantee in the pagination contract is bounded by cursor
  validity rather than promised indefinitely.

**It fails closed.** A missing or invalid context is an error, never unrestricted access — an
uninitialised host adapter or a forgotten parameter must not become an authorization decision by
omission. The standalone server explicitly constructs a full-dataset view *after* checking the
caller's grants, so simple operation stays simple without omission ever meaning "everything".

Establish it early. Adding a scope parameter later touches every query path, every cursor and every
aggregate, so the cost of deferring it is real — though the boundary still needs concrete semantics
before it is worth anything, which is what the rest of this section is for.

### 2. Current grants inside the write transaction

The core checks current dataset and application authority in the transaction performing the write.
Authorization and the write MUST observe the same snapshot: concurrent revocation either precedes the
decision or follows the write. Guest grants can authorize reads, inserts, and creator-scoped updates and deletes.
Type and collection grants cover exact concrete types. Collection reads also check the membership
fixed at insertion. The host uses `View.WithOwnMutations(update, remove)` with
`Grants.UpdateOwn` and `Grants.DeleteOwn` to grant explicit mutations by exact type and
collection. The store also requires current read authority and a matching creator stamp.

The proposed existing/proposed-state write hook is abandoned for v1. Collection membership is the only field-based authorization rule. Workflow transitions and
arbitrary guest-write hooks remain outside the protocol. A host needing domain workflow defines
its own extension contract outside v1; embedding alone does not promise such a mechanism.

### 3. A unit of work the host can join

Record changes, the host's own domain records and durable events commit together or not at all.
The core exposes a unit of work that defines transaction ownership, rollback behaviour and which
repositories participate. **Network calls stay outside the callback** — a transaction held open
across a remote call is a lock held for an unbounded time. **An implementation MAY run the callback
more than once**, rolling back everything the earlier run did, host writes included, before the
next; a host keeps nothing outside the callback that a second run would add to.

Without this seam a second database is not inevitable, but atomic composition would have to reach
around the core's public interfaces, which is precisely what a composable core exists to prevent.

**Ordering against the durable claim**, which the protocol requires to commit before the write begins:

1. The core claims the idempotency key in its **own** committed transaction. This is deliberately
   outside the unit of work, so a rollback cannot remove the claim. An absent claim means only
   "no retained claim right now": a request may still be in flight and claim later. It does not prove
   that the request never began or cannot still commit.
2. The core starts, or joins, the agreed write transaction.
3. Core record changes, host domain records, durable events and the **receipt outcome** commit
   together.

So a host rollback rolls back the outcome with everything else, leaving the claim in place and
resolvable: the key stays claimed, `:status` reports `in_progress` until recovery resolves it, and a
retry replays or re-attempts rather than duplicating. **A host rollback must never leave a committed
receipt outcome**, which is why the outcome belongs in the joined transaction and the claim does not.

**Atomic composition is limited to repositories participating in the same transaction.** An interface
cannot manufacture atomicity across independent databases, and one that appears to would be the worst
possible thing to offer.

This is exactly why v1 requires **one transactional authority** over records and attachment metadata.
The content authority authorizes and records an attachment **within** the record transaction —
participating in it, not merely called from it — and a host whose content authority cannot join that
transaction MUST refuse media writes rather than manufacture the atomicity this section forbids.

Note what the rule does *not* say. The byte store is unconstrained, because it is not an authority: an
object store or a CDN holds bytes and honours a URI the authority issued. **Byte transfer belongs
outside the transaction** and a repository MUST NOT attempt network I/O inside one. Splitting the
authority itself is the case v1 leaves out, and
[the annex](annex-remote-content-authority.md) records why — experimental and unimplemented.

**What the protocol keeps quiet about is still visible to a host.** An erase answers a client with
access revoked and deletion durably scheduled, and deliberately gives it no way to watch the physical
deletion, because a wire client can do nothing with that information. A host is not a wire client: the
deletion queue is in its own database, and a core MUST let the host observe and acknowledge that work.
A host may have promised its own users more than the protocol promises, and it can only keep such a
promise from the inside.

The direction that matters is the other one. **A successful erase response is not evidence of physical
deletion.** A host that tells a person their data is destroyed satisfies that from the worker's
acknowledgement in its own queue, never by re-reading the protocol's `200`. Translating the one into
the other is the shortest path from an honest contract to a false claim.

### 4. Authentication separated from authorization

The **host** resolves credentials into a principal and that application's authorization. The **core**
checks current resource grants at the time of the request. Token claims must never
become a stale snapshot of access — a grant revoked a minute ago must not survive in a token issued
an hour ago.

The standalone binary ships the OAuth implementation the protocol describes. An embedded deployment
supplies compatible sign-in and discovery through its own host. **Embedding removes the requirement
to run a second authorization server; it does not remove the protocol's user-facing authentication
requirements** — a client signing in against another party's deployment must still get
authorization-code sign-in with PKCE, discovery metadata and revocation.

### 5. Extension registration that declares a contract

An extension registers more than a handler: **namespaced routes**, its **contract and schema
versions**, the **core features it requires**, and its **conformance tests**. Registering a route
must not be what certifies a capability — discovery advertises an extension only when its
conformance tests pass against that deployment.

A server with no extensions registers none, and extensions are genuinely absent from its discovery
rather than advertised and refusing.

Proposed names, not yet contracts: `sharing.v1`, `record-history.v1`, `personal-inbox.v1`,
`coaching.v1`.

## The authorized dataset view

Queries operate within exact type and collection read grants. Object grants permit direct reads.
A [collection field](tessera-protocol-v1.md#collection-fields) fixes membership at insertion.
A lookup outside
the view returns `404 unavailable`, whether or not the record exists. Within an authorized view,
`404 not_found` means absence; `403 permission_denied` refuses an operation. A client must preserve
these three answers so withdrawal is never reported as deletion.

These are settled v1 rules, not a proposed domain extension. Grant changes invalidate cursors without
revealing which hidden records exist; a standalone deployment with full views obeys the same contract.

## Conformance

Authorized views and cursor revocation are **normative v1 behaviour**, not extension behaviour, so
their tests belong to the base suite and MUST NOT depend on registering a domain extension.

Base conformance therefore has two tiers, which is what lets a single-view server and a sharing
deployment run the same suite:

- **Every deployment** must satisfy the invariants that exist regardless of how many views it has:
  a fail-closed access context, cursors bound to principal and application authorization, invalidation
  when anything the view depends on changes, and the absence-versus-unavailable rule as it applies to
  its own views. A standalone server has one full-dataset view, never returns `unavailable`, and passes
  this tier on the same fixtures.
- **Any deployment offering more than one view** must additionally satisfy the isolation tier. It is
  not an extension and is not opt-in: a deployment that can restrict views is claiming this behaviour
  by existing.

The isolation tier gains fixtures for:

- multiple principals, and **distinct application authorizations for the same principal**;
- hidden versus nonexistent identities, and which of `unavailable` and `not_found` each deployment
  owes;
- guest write attempts — insert, update, filtered mutation, atomic batch and schema cascade — all
  refused because a sharing grant conveys read only;
- receipt replay and `:status` by a non-owner, and by an owner whose authority was withdrawn;
- cursors invalidated by a permission change, including replay of an already-issued page.

Run the suite against **both** the standalone server and every composed deployment: a scope predicate
that is slightly too aggressive breaks base conformance, and nothing else will catch it. Extension
suites then cover domain rules on top, and an extension is advertised only where its own tests pass.

## Sequencing

Establish the fail-closed access context and transaction-composition proof during the durable
server stage, including rollback and receipt recovery. Keep authentication supplied by the host
separate from current grants checked by the core. Add read-only sharing only with its isolation tests;
it requires no proposed-state policy hook. Domain extensions and their workflows require their own
contracts and callers, and are outside the v1 implementation plan.
