# Contract review and resolutions

This record explains contract questions and the author's decisions. It includes historical
provisional behavior and bug reports; those passages are not current implementation status.
Use [asked and answered](asked-and-answered.md) to find a question, the normative
specifications for its rule and [the feature matrix](feature-matrix.md) for current support.

## Resolved by the specification author

`1e3f932` resolves four findings: type deletion now requires explicit `cascade` to destroy nonempty
hierarchies; each operation has its own grant and the transaction requires their union; absence checks
name later insert aliases with required `guards`; effective annotations merge by key with byte-exact
values, while roles union and inherited field declarations remain unchanged.

`ab91f85` resolves discovery and grant-change parts of the watermark finding: dataset entries carry
watermark, whole-schema version and access expiry; schema changes advance the watermark, grant
changes do not. It also introduces required `expect_type` against a computed effective schema version.
Those changes are implemented in both transaction engines. `7d0bb1a` adds the independent
`view_version` and replaces the discovery access timestamp with `access.changes_at`.

`8bbc159` resolves receipt authorization: the owning principal and application can recover an outcome
without current dataset grants, including schema-only writers and revoked owners. Current read
authority gates the stored body; created identities remain recoverable. Unknown is answerable to any
authenticated caller. SQLite and HTTP tests cover these rules.

`54d6ba3` resolves the ContentRef encoding: media values always carry `store//item`. The vectors and
Go decoder now enforce it, including item `//` rejection. Integer-to-float representability fixtures
are unchanged. `edf30a7` freezes the cases with Go `1de0bfe` and Swift `35ad924` as independent
consumers, including the five documented Swift platform divergences. Cases are append-only;
independent HTTP integration is a separate gate.

`f43824a` resolves findings 1–5. Schema transaction results are the effective REST results;
field removal requires `confirm` equal to the type ID and reports `meta_schema_error` with
`confirmation_required`. Non-cascading deletion reports `type_not_empty`. Unknown operation members,
duplicate aliases, and unresolved/forward aliases report indexed `invalid_request` before execution.
Alias names follow the 64-character ASCII grammar; minting reserves `$`, while received object IDs
retain their previous validation rules. The shared identifier/value vectors are unchanged.

Watermarks are top-level siblings on data responses; replay retains the answer's original value.
Checks and commits with no net data/schema change do not advance it. Status exposes the retained
observation's watermark, including identity-only recovery. Unknown status has no dataset observation
and no watermark. Legacy error receipts without a recorded observation do not invent one.

Receipt bodies are returned whole or withheld whole. Current full read authority permits full replay;
schema-only receipts also accept schema authority. Delete-only outcomes have no values to withhold.
Created objects that no longer exist force identity-only recovery. Receipt migrations preserve original
watermarks independently of scrubbed bodies, and retain the replay category for new transactions.
Older schema receipts can be classified from their response shape; older count-only receipts cannot
be identified as delete-only and retain the conservative read gate.

The recovery claim now explicitly excludes treating computed versions as attempt identities: same-value
assignments and A→B→A defeat that inference. Idempotency keys identify attempts; no stored revisions
were introduced.

`a5a46c8` resolves the remaining eight questions. Reasoning, rejected alternatives and prose
conformance exit criteria are recorded in
[author resolutions](../specs/tessera-decisions.md#author-resolutions-of-review-findings-613--2026-09-19)
and the normative record/content conformance sections.

6. **Embedding's previous sharing model — resolved in `a5a46c8`.** Abandon the proposed-state hook;
   require grant-table-only, read-only sharing and current-grant transaction checks. Remove obsolete
   milestones and field-dependent view generations. An absent claim does not prove “never began”.
7. **Content import identity/resume — resolved in `a5a46c8`.** Store-scoped operation IDs have a
   principal/application-authorization owner; changed reservation ceilings conflict. Abort/reclamation
   retains a tombstone for at least the import retention interval, and clients use fresh IDs.
   Declared zero commits without transfer; unknown-length emptiness cannot commit.
8. **Unavailable representations — resolved in `a5a46c8`.** Missing capability is `403 not_supported`;
   advertised but item-unavailable representation is `415 unsupported_representation`.
   **Breaking implementation change:** absent-capability resolution currently returns `415`.
9. **Type-grant descendant scope — resolved in `a5a46c8`.** Exact concrete types only, including after
   creation of new subtypes; intersect grants before grouping/count/paging. Object grants stay
   direct-read only. Matches current behavior.
10. **Erase idempotency boundary — resolved in `a5a46c8`.** No erasure receipt; reject keys with
    `400 invalid_request` and recover through confirmation plus replacement discovery.
    **Breaking implementation changes:** dataset erase currently uses `invalid_field` for a valid key;
    content erase ignores the header. Both must refuse before retirement.
11. **Aggregation prose/shape — resolved in `a5a46c8`.** Drop “averages”; the client derives an average
    from sum/value_count when nonzero. No new response member. Matches current behavior.
12. **Distinct-reference encoding — resolved in `a5a46c8`.** Elements are bare
    `{dataset_id, object_id, meta_id}` identities, with a normative example. Matches current behavior.
13. **Locales has no write path — filed and resolved in `a5a46c8`.** `Accept-Language` and `/locales`
    promised translations although schema writes carry one name. Withdraw `locales`, its route and
    discovery locale list from v1. Return names as stored; language does not change effective or
    whole-schema versions or invalidate `expect_type`. No locale implementation is required.

14. **Aggregate sum accumulator — resolved in `63144f2`.** Independent implementations exposed
    that “the server's accumulator” let the same integer sum succeed on one server and fail on
    another. Integer sums now return exact final totals within the signed 64-bit range and fail
    outside it; float sums round the exact total once to binary64, nearest with ties to even, and
    fail if that result is infinite. Intermediate overflow cannot defeat cancellation. One
    overflowing group fails the whole request. `422 query_result_limit_exceeded` requires reason
    `sum_overflow`, `group_limit` or `distinct_reference_limit` for these three bounds.

    **Breaking changes required:** the Swift test
    `aLargeSumIsTheNearestNumberRatherThanAFailure` must expect refusal for two signed 64-bit maxima;
    binary64 integer accumulators must preserve exact in-range totals. The Go implementation must
    replace premature overflow refusal and repeated float rounding with the specified final-total
    semantics, and add the required error reasons. Its existing exact `9007199254741000` assertion
    remains valid. Implementation and tests deliberately follow separately.

    Reasoning and rejected alternatives are in the
    [author decision](../specs/tessera-decisions.md#author-resolution-of-review-finding-14--2026-09-19).
    The [prose conformance cases](../specs/tessera-protocol-v1.md#aggregate-sum-conformance-cases-finding-14)
    cover integer boundaries, permutation-independent cancellation, float rounding and overflow,
    null statistics, grouped failure and the three reasons. Frozen vectors are unchanged.

## Contract findings

Indexed by question in [asked and answered](asked-and-answered.md), which is the cheaper way in if
you are checking whether something has already been decided.

### Finding 15: what a dataset identity answers when it is not the caller's

**Closed 2026-09-20.** Settled, specified, implemented on both evaluators, and all three answers
carried by `conformance.CheckCreation`: a retired identity answers `not_found`, an identity naming
nothing does not, and a live dataset the caller holds nothing on is concealed rather than refused.
That last case was the one whose behaviour changed most and the last to get a case; seeding the
defect it exists for fails it.

**Settled: `not_found` only for confirmed retirement, `unavailable` otherwise.** A client
reports deletion only on positive evidence of retirement. An inaccessible live dataset and an
identity that names nothing stay indistinguishable. For a dataset the caller *can* reach, ordinary
operation permissions still apply, `permission_denied` included.

**The disclosure this accepts, stated rather than glossed.** An earlier note here claimed the third
option "keeps both properties". That needs qualifying: it conceals *current existence* but reveals
*historical retirement* to an authenticated caller holding the identity. That narrow disclosure is
accepted, because retirement is irreversible and a client needs reliable evidence before telling
someone their data is gone — which is the confusion the three-answer rule exists to prevent.

**A consumer was asked whether it distinguishes them, said yes, and on inspection does not.** The
answer given was that its reference resolution turns absence into **missing** — permanent, because
identities are never reused — and keeps `unavailable` temporary. That is what the code intends and
not what it does. Both codes map to one client error before that fork is reached, and the fork then
decides permanence by **whether the storage is registered on the device**, not by which answer the
server gave. For a registered storage, `unavailable` therefore resolves to *missing* and reads as
deletion — the exact thing this section forbids a client to do.

**The decision stands; its stated justification did not.** Recorded plainly because the difference
matters: the disclosure was accepted partly on evidence that a client already depended on the
distinction, and that evidence was wrong. What survives is the original argument — a client needs
positive evidence before telling a person their data is gone — and it is now *better* supported
than before, because a client that flattened the distinction got exactly that wrong on its own.
Always answering `unavailable` would make the mistake permanent rather than fixable.

**The flattening also makes a future requirement invisible.** A consumer that cannot tell these two
answers apart would implement any rule that turns on the difference, pass its own tests, and be
wrong. That obligation is now stated: a client
must neither treat either answer as evidence nor show the difference to a person.

**And "a consumer has collapsed them" understates it — collapsing them is correct.** A client that
distinguished the two in anything a person can see would confirm that a reference points at
something real, which is the existence the server declined to assert. So the consumer's mapping was
not a shortcut, and the defect is narrower than it looked: one consumer of the collapsed value,
two directories away, needed the distinction the collapse had discarded. The fix is not a richer
error case; it is that the discarded distinction must survive to the one place that needs it and
reach no interface.

**The defect is live today and this finding only enlarges it**, which is worth stating because the
first account here said otherwise. A record outside a caller's authorized view already answers
`unavailable` — the view check that precedes every object and metadata read, which a consumer
reported at two other line numbers because it reads the tag it pins rather than this tree — so a
guest reading outside its type grant reaches the same misreading now, without any change. What this finding does is move that path from sharing — which that consumer does not
yet use — onto grant revocation, which is ordinary and which a person experiences as their data
being gone. It should be fixed whether or not this finding ships.

**And it is a composition failure, not a wrong file.** Both halves are individually correct and
individually well commented. The error mapping says a client turning `unavailable` into "deleted"
would be inventing the fact the server withheld — and maps it to the same case as `not_found`. The
resolver says a registered storage that answered and lacks the target is permanently missing —
which is true of the answer it thinks it has. Neither reader of either file sees the inversion,
because it lives in the join. That is a different failure from a partially-right sentence: there is
no single place where the wrong thing is written down.

**The cheaper alternative it offered, and why it is a fallback rather than an improvement.** If the
disclosure is ever judged too expensive, what a client needs preserved is the **permanence**, not the
identity — that asking again cannot help, rather than what used to be there. That is a real option
and it removes the durable-evidence obligation below, because a server can answer *permanently gone*
for anything that does not name a live dataset without proving which ones it retired.

It is not free, and the trade runs the other way from the one above: answering that way makes *live
but not yours* distinguishable from *not live at all*, so it conceals a retirement at the cost of
disclosing a live dataset's existence. That is the disclosure the three-answer rule most wants to
prevent. Recorded as the fallback if durable retirement evidence proves too costly to require, not
as a better answer to the same question.

**What it commits every implementation to.** Durable retirement evidence, surviving restart and
restore. Where that evidence is unavailable a server MUST answer `unavailable`; it must never guess
`not_found`. That makes the retirement ledger load-bearing for a wire answer, which is the cost, and
it is taken deliberately.

**What it does not do.** If historical retirement must also be concealed from unrelated callers, none
of the three options considered is sufficient — that needs retained evidence of who may learn about a
retirement, which is machinery this slice deliberately does not add. It is worth adding only if that
stronger privacy requirement is intentional.

The same answer holds one level down, as the content path already does for a retired dataset.

### Finding 16: the protocol describes no way for a dataset to come to exist, nor for a server-scoped operation to be asked about

**Closed 2026-09-20.** Both routes are described in [the protocol](../specs/tessera-protocol-v1.md)
and carried by `conformance.CheckCreation`, which reads what discovery advertises and runs the
matching half — so a server offering nothing still has to refuse, and the refusal is as much the
contract as the behaviour. What the creation route made reachable and did not answer is
[finding 21](#finding-21-a-server-level-right-has-no-way-to-be-held), filed rather than folded in,
because a class of server-level right is a larger question than the one route that raised it.

**Scope widened.** As filed this was about creation alone. Fixing the recovery defect underneath it
added `POST /v1/operations/{key}:status`, which the contract also does not describe, so the finding
now covers both routes. Neither becomes normative by existing, and both are resolved together or
not at all.

**As filed.** `GET /v1/datasets` lists datasets a server holds, every grant in the contract is over a
dataset that already exists, and erasure retires one and mints its replacement. Nothing says how a
first or an additional dataset is created. While a storage held exactly one, that gap was invisible:
the dataset existed because the storage did.

**What the implementation does provisionally.** `POST /v1/datasets` creates one, and is refused
`403 not_supported` unless a deployment has said otherwise twice — the host supplies a decision
function and the engine is one that can provision. The caller that asks becomes the new dataset's
administrator, which is the policy erasure already applies to a replacement. The body carries a
display name and nothing else; bounds are the storage's.

**One part of this has since been fixed and no longer describes the implementation.** An
`Idempotency-Key` was originally refused `400 invalid_request`, because a retained outcome is keyed
by a dataset and a creation has none — the same reason erasure refuses one. That left a caller whose
response was lost unable to discover whether a dataset existed, which contradicts *a lost response is
never a reason to guess*. Creation now retains its outcome under a reserved server scope and
`POST /v1/operations/{key}:status` answers for it. **That route is itself undescribed by the
contract, so this finding is now wider than when it was filed, not narrower.**

**Settled: creation answers with an identity and nothing else.** Decided 2026-09-19, before
implementation. The response to a creation is `201` with `{"dataset_id": "s-…"}`; a matching replay
returns the same status and body with `Idempotency-Replayed: true`; a committed status returns
`{"state": "committed", "result": {"dataset_id": "s-…"}}`.

The receipt's owner recovers that identity after dataset-access withdrawal, creation-right
withdrawal, erasure, or creation being disabled. Recovery needs valid authentication and receipt
ownership and **no current dataset grant**. It neither calls back into the dataset nor returns
display names, bounds, grants or any other discovery state; current information comes from
discovery. The identity outcome is persisted atomically with the dataset, and the existing
digest-conflict, concurrent-claim, retention and recovery rules apply unchanged.

The alternative considered was retaining the original discovery-shaped response and disclosing it
conditionally through `result_withheld`. That repairs the response shape this implementation
happened to have; it is not a reason to keep it. Storing a body does not itself disclose anything —
returning it without a current-authority check does — so the better answer is not to retain a
disclosure-bearing body at all. An identity cannot go stale, cannot leak limits, capabilities or
versions to a caller whose access ended, and makes a replay identical to the original by
construction rather than by a check that has to be right every time.

The cost accepted: creation stops matching the convention that a create returns what it created, as
`POST /meta` does. Creation is already the exceptional operation — the only one with no dataset —
and this is recorded as deliberate so it is not later "fixed" back.

**This settles the response and recovery shape only.** Server-scoped authority, advertisement,
retention and both routes still need normative text and conformance cases, which is why this finding
stays open.

**What breaks either way.** Leaving it out means every deployment invents its own way in, and a
client that can hold several datasets has no portable way to obtain the second. Standardising it
means the contract has to answer who may create one, and it currently has no grant that could say:
its grants are all *over* a dataset. Inventing a server-scoped grant is a larger change than a route,
and is the part worth deciding first. Answering `not_supported` by default keeps a server that says
nothing conformant, and a server that offers the route is making a promise of its own rather than
one the contract made for it.

There are no conformance cases, because there is no contract to conform to yet. The implementation's
own cases cover both refusals and the created dataset's administration.

Finding 14 is implemented. Integer sums accumulate in a big.Int and floats in a wide-precision
big.Float rather than in a running accumulator of either kind, because both the cancellation rule
and the round-once rule are order-dependence bugs in disguise: an int64 that overflows at the second
of three values, and a float64 that rounds at every row, each give answers that depend on the order
rows happen to arrive in. A compensated float expansion was tried first and rejected — it still
overflows internally when two values near the top of the range are added, which the specification
explicitly requires to survive. The float path is checked against `big.Rat` over randomised inputs,
which is what caught that. Reasons are carried on all three bounds, and the conformance suite
asserts the reason rather than the code, since the code alone no longer identifies the bound.

None of findings 6–14 remain open. Finding 14's conformance cases are specified in prose and have
not been implemented by its author-resolution change; findings 6–13 retain the alignment below.

### Finding 17: whether a replay outlives the authority that permitted the original

**Closed 2026-09-20.** The rule was already normative and the implementation was diverging from it;
both transitions now have cases at the HTTP boundary — the caller's right withdrawn, and the
deployment no longer offering creation — each paired with the refusal that makes the transition
real. Seeding the defect they exist for, gating before recovering, fails the first.

**Settled: it does, and the contract already said so.** This finding is narrower than it
was filed as. [The protocol](../specs/tessera-protocol-v1.md) already states that a repeat with a
matching digest "MUST NOT re-execute, and is answered under the same per-outcome authorization as
`:status`", and that "an owner no longer authorized receives the identity-only form". Creation was
**diverging from existing normative text**, not raising an open question.

**The distinction to implement is recovering an existing claim versus admitting new work.** In order:
authenticate the caller, look up the receipt, check ownership and request digest, and apply the
retained-outcome rules. Only when there is no retained claim do current creation permission and
availability decide whether work may begin. A different payload still conflicts with
`idempotency_key_reused`; another caller still cannot recover the receipt.

This sits consistently with the rule that a key is claimed only "once the request is understood well
enough to attempt it", with authorization failures preceding the claim. That gates *claiming a new
key*. A matching repeat already has a claim, and is answered under the outcome's own authorization.

**Consequences.** Withdrawing a caller's permission to create refuses a new creation and leaves its
existing receipts recoverable. Disabling provisioning answers `not_supported` to a fresh creation and
must leave receipt recovery available for the promised retention window: **disabling new work must
not disable reconciliation.**

**The principle this settles, beyond creation:** revocation stops new work; retained receipts still
reconcile old work.

### Finding 21: a server-level right has no way to be held

**Resolved 2026-09-20.** Raised by the bootstrap design rather than by an implementation, and it is
the contract's gap rather than a deployment's. Most of what it asked for had since been written by
the bootstrap work; what was left was one rule and one hole.

The contract now says an authorization is over **at most one** dataset, and that wording exists so
an authorization carrying server-level rights and no dataset grant is expressible. What it does not
say is how a principal comes to hold such a right, or that one exists to hold: `server_access`
reports `create_dataset` as an answer about this caller, and nothing describes what makes the answer
yes.

That is deliberate as far as it goes — grant administration is outside the protocol, and this would
be too. But dataset grants are outside it while their *effect* is fully described, and a server
right currently has neither. A client can read `create_dataset: false` and has no way to know
whether that is a deployment that never grants it, an account that was not given it, or a right the
server does not implement.

**What the implementation did provisionally, and no longer does.** It derived the right from holding
`erase` on some dataset — a stand-in with a policy hidden in it, *administrators may create*, which
made self-service impossible by construction. The bootstrap work replaced that with explicit
account state, and `server_access` has since acquired most of what this finding asked for: the
rights are an extensible object, an unknown name confers nothing, a server names every right it
defines, the answer follows current state and is evaluated per request, and revocation stops new
work without stopping reconciliation of work already claimed.

**What was actually left.** Two things.

*The rule that a server right is held twice.* An account holds it; an approval carries it or does
not; the conjunction is what `server_access` reports. That is the rule dataset grants already
follow — an installation never exceeds what the account holds — and it is what makes an approval
carrying a server right and no dataset separately revocable rather than a property of the person.
It existed in the code and in no document. It is now in the protocol, together with the statement
that the class is open, that `create_dataset` is an instance of it rather than the class itself,
and that a right governing who may confer rights is **not** one of these and MUST NOT appear in
`server_access` — a client has no business reading it.

*Somebody to administer one.* An approval carrying a server right was administrable by nobody,
which was the honest answer while there was no server administrator to weigh it against, and a hole:
a credential that can create datasets could not be ended from the surface that exists to end
credentials. The reference deployment now holds that right explicitly.

**Explicitly, and this is the part worth keeping.** Not derived from holding `erase` somewhere, not
from administering every dataset the server holds, not from being the only account. Each reads as a
reasonable shortcut and each grows silently as the server does — and two of them were this week's
escalations: administration derived from `erase` anywhere made every account that owned a dataset
an administrator of everybody else's, and standing derived from a shared membership was standing
anybody could arrange, since adding an existing account to a dataset needs no password. The
deployment contract now says a deployment MUST hold this entitlement explicitly, and that refusing
is the right behaviour until it has one.

**Still the deployment's, and deliberately.** How an operator confers either right. Here it is a
command-line flag, because the first administrator of a server cannot be provisioned from a screen
that requires being one.

### Finding 18: whether `group_by` may name a reference field

**Resolved 2026-09-20 and implemented 2026-09-23.** Yes, and the wire form settled first. The
author's answers are below each question; what shipped is at the end.

Proposed by a consumer, not by this implementation, with a use it already has: grouping
measurements by the person they are about. `internal/recordstate/aggregate.go` refuses any
`group_by` whose field is not `text`, so grouping by a `reference` — the subject of a record — means
pulling every row and splitting it client-side, which is the unbounded materialisation the aggregate
capability exists to prevent.

**The case for it is that it carries no policy.** A reference is an identity, and grouping by an
identity is structurally what grouping by text already is: equality on an opaque value. The protocol
already defines equality and a total order over references — `meta_id`, then `dataset_id`, then
`object_id`, each by code point — so grouping needs no new comparison and no new vocabulary. Nothing
about what a reference *means* is consulted, which is the line this contract cares about.

**What the same consumer explicitly did not ask for, and why it matters.** Time bucketing — grouping
by month — is the other axis every chart needs, and it was ruled out on its own reasoning: a bucket
needs a timezone, a day-boundary convention, DST handling and a week-start rule, all of which are
product policy. A storage protocol that shipped them would be deciding what a month is. Rollups stay
in the client. That distinction is the useful part of the proposal, and it should survive into
whatever is decided: **grouping by an identity is mechanical; grouping by a derived bucket is
policy.**

**Open questions, answered.**

*The group key on the wire* is the bare qualified identity `distinct-references` already returns.
One representation of a reference rather than two, and reusing it costs nothing.

*A cross-dataset reference groups by the whole triple.* The ordering rule already says `meta_id`,
then `dataset_id`, then `object_id`, and equality follows the same shape. Anything narrower would
put two genuinely different objects in one group, which is a wrong answer rather than a coarse one.

*The aggregate vector cases widen.* They are append-only, so this is permitted. A new grouping kind
with no vector is exactly the shape that keeps costing people time: a rule that is true, stated and
unexercised.

*`maximumAggregateGroups` moves to `Limits`*, keeping 32 as the value. A bound a client cannot read
is one it cannot plan around, and every comparable bound is declared. This is about where it lives,
not what it is.

**Implemented 2026-09-23**, after the security review closed. It was held back while that was open
— each of the last two rounds had found a defect in the previous round's fix, and a protocol change
competing for attention with that is the wrong trade. Nothing about the decision changed in the
interval; only the sequencing did.

What shipped, against each answer above:

- `group_by` accepts `text` and `reference` and refuses every other type, including numbers and
  timestamps. The refusal is the substantive half: a bucket needs a timezone, a day boundary,
  daylight saving and a week start, so grouping by one would be this protocol deciding what a month
  is.
- A reference group is the bare qualified identity, all three members, identical to what distinct
  references returns.
- Group order is specified rather than incidental: null first, then text ascending, then references
  by `meta_id`, `dataset_id`, `object_id`. It was already deterministic and undocumented, which is
  the state where two implementations agree until one of them has a reason not to.
- `max_aggregate_groups` is in `Limits`, still 32. The evaluator reads the dataset's bound rather
  than a constant, the conformance suite reads it from the dataset rather than asserting a number,
  and the note about a zero read back holds: absent in a stored row means the default, because the
  row predates the name rather than declaring a bound of nothing.

**The conformance case is the part worth reading.** The whole-triple requirement is the one a
server can miss while looking correct — key the groups on `object_id` because it is the part shaped
like a key, and every group returned is a real group with a real total. So the case builds
identities that collide on purpose: three references agreeing on `object_id`, differing only in
`meta_id` or only in `dataset_id`. Both defects are seeded against the suite — folding by
`object_id`, and answering a group as a bare object identifier — and both fail it.

That case is constructible only because a reference is a pointer and not a foreign key: nothing
requires its target to exist, so the identities need no second dataset and no colliding minting.
That is worth recording, because the first design of this case needed a second dataset and would
have been gated on optional dataset creation — a case that runs for nobody on a server without it.

The discovery example gaining `max_aggregate_groups` was caught by the specification check built
for exactly that, three days after it was written. It was predicted, in this finding, to be what
would keep the limit honest when this landed. It was.

**The line to keep, whatever the implementation looks like.** Grouping by an identity is mechanical;
grouping by a derived bucket is policy. Time bucketing stays out for the reason the proposal itself
gave — a month needs a timezone, a day boundary, DST and a week-start rule, and a storage protocol
that ships those is deciding what a month is. Rollups stay in the client. Recorded as a standing
distinction in [the decisions](../specs/tessera-decisions.md) rather than only here, because it
outlives this feature and is worth more than it.

### Finding 22: creating a dataset silently widens an authorization's reach

**Resolved 2026-09-20, and the premise I raised it on was wrong.** Not raised by review but by
three rounds of it circling the same root, and named by the author while resolving finding 18.

An authorization is described as being over at most one dataset. Creating a dataset installs full
access for the caller that created it, under the same principal and authorization, on a dataset the
approval never named and the identity registry never hears about. So an ordinary approval's reach
outgrows the dataset it names, and nothing in the contract says it may.

Everything the security review found in this area was a consequence of that, and each was fixed
where it surfaced rather than at the root:

- Withdrawing an approval revoked only the dataset it named, so a withdrawn installation kept
  everything it had created — and kept the right to create more.
- Standing to withdraw was decided by the dataset the approval named, so an administrator of that
  one could cut an installation off from another they have nothing to do with.
- The notice that records a creation ran on replays and wrote the creator's grants, so replaying a
  creation restored what an administrator had removed.

Three fixes, one cause. That is the signature of a contract question answered only in
implementations. **The question is what an authorization is:** an approval for one dataset that may
acquire others, which is what the code does, or an approval for one dataset which cannot, in which
case creation must install access for the *principal* under a new authorization rather than widen
the one that asked.

**What I got wrong in raising it.** I wrote that the second reading is what the wording implies.
The wording I had in mind was the authorization rule — "an authorization is over at most one
dataset", and its listing "returns exactly the dataset it is over". But the creation section says
the opposite in as many words, with reasoning of its own: the caller that asks becomes the
dataset's administrator, and the created dataset MUST appear in that caller's listing immediately.

So this was not an implementation drifting from a contract. It was **two requirements in one
document that cannot both hold**, and nothing had noticed because each was read on its own. The
implementation follows the second. So does the conformance suite, which has asserted since it was
written that a created dataset appears in its creator's listing — a server obeying the first rule
would fail the suite.

**Resolution: the creation rule stands and the authorization rule is amended.** An authorization is
*approved for* at most one dataset and *reaches* that one plus any dataset created through it. The
listing returns the reach, which is how a client learns it. Creation is the only thing that widens
it; a grant an administrator installs goes on an authorization of its own.

This is not the safer-sounding answer and it is worth saying why it is the right one. The reach
only grows through an act the approval was explicitly granted — `create_dataset` is a server-level
right somebody consented to. The three defects came from the reach being *invisible*, not from its
existing: nothing could see it, so withdrawal missed it, standing was decided without it, and a
replay wrote over it. It is visible now, in the listing, in `Reach`, and in the two obligations the
protocol now states:

- Withdrawing or revoking an authorization removes its access to everything it reaches.
- An approval offering `create_dataset` MUST say that the installation will also reach what it
  creates. The consent screen says so now; it previously offered "Create datasets" and nothing
  more, which collected consent for one thing and delivered two.

The three fixes are ratified by this rather than compensating for it, which is what a resolved
contract question should do to the code written before it.

**The suite now tells the two readings apart.** The case that existed asserted only that a created
dataset appears in its creator's listing, which a server implementing either reading passes. A
second creation is made and both must be listed. The first attempt at this assertion was vacuous —
it compared against a listing that is empty in the conformance fixture, because that fixture's
caller holds no dataset before it creates one, which is exactly the case where "lists its reach"
and "lists only the newest" are indistinguishable. Found by seeding the other reading.

**Not resolved, and left to finding 21:** where a server-level right lives. `create_dataset` is
still a property of an account in this deployment and nothing in the protocol says how one comes to
hold it. Administration refuses to act on a server-level approval at all for that reason.

### Finding 23: not falling back to `http` is not the same as refusing it

**Raised and resolved 2026-09-23.** Raised by a consumer implementing
[finding 20](#finding-20-the-steps-between-a-hostname-and-a-session-are-not-described) — checking
their client against the three new rules rather than assuming, which is the whole reason for
writing rules nothing can enforce.

They found that they satisfy two outright, and could not say whether they satisfy the third. Their
client requires `https` for the OAuth endpoints, exempting loopback, and has **no scheme check at
all on the storage URL** — the origin discovery is rooted at and every API call uses. Rather than
guess, they asked which rule it is: *no fallback* or *`https` only*, and whether it carries the
loopback exemption.

**Their reading was right and the prose was the problem.** What finding 20 settled is how a *bare
authority* resolves — a person types a hostname and the client must not probe or fall back. That
closes the downgrade a client performs on its own initiative. It says nothing about an explicit
`http://` URL handed to the client by a person, a configuration file or a saved account, and a
client can satisfy every word of it while sending records and bearer tokens over plaintext.

So the question exposed a gap rather than an ambiguity: one of two halves was written, and the
written half was the one a careful client was already getting right.

**Resolved: a client MUST NOT speak `http://` to a Tessera server except on a loopback host** —
`127.0.0.1`, `::1` or `localhost` — with the matching deployment obligation written into
[the deployment contract](../specs/tessera-deployment.md) so the pair is visible from either
document.

**The loopback exemption is narrow on purpose, and the consumer's worry deserves the direct
answer.** They did not want a rule stricter than the specification that would break self-hosting on
a LAN. A LAN is exactly what this refuses. `http://192.168.1.10` is not loopback: every other
machine on that network is a third party, and *inside the house* is not a security boundary — it is
where the untrusted device someone plugged in also lives. Loopback means reachable only by the
process talking to it.

**It forbids no deployment this project supports**, which is what made it an easy decision rather
than a trade. The reference self-hosted server already requires a certificate and key and refuses
to start without them; the development server is plaintext on loopback with synthetic data, which
is the exemption. The rule forbids a client from being the thing that makes an unsupported
deployment possible.

**No conformance case, and that is now the third.** Like the two rules beside it, this is something
a client does, and this suite tests servers. Three of the four rules in Discovery are unenforceable
here. Recorded rather than glossed: the reason they are written is that silence leaves each client
inventing an answer, and on this one the available inventions included *use whatever URL you were
given*, which a consumer had in fact implemented without deciding to.

### Finding 24: a server can send a client somewhere plaintext

**Raised and resolved 2026-09-23.** Raised by the consumer implementing
[finding 23](#finding-23-not-falling-back-to-http-is-not-the-same-as-refusing-it), from what their
guard found rather than from what they set out to guard. They briefed three call sites; the guard
needed four.

The fourth follows an issuer taken from the server's own `authorization_servers` and fetched it
**before** the clause that validates the metadata ever ran. So a server naming
`http://192.168.1.10` as its authorization server was followed there, unguarded, and the check that
would have refused it ran on a response that had already been fetched over plaintext.

**This is a better hole than the one finding 23 closed, and it is worth being precise about why.**
Findings 20 and 23 are both about a destination the *person* supplied — a typed hostname, a saved
URL. Here the destination is supplied by **the far end**. A server that wants a client to talk
plaintext to an address of its choosing had a supported way to ask, and nothing in the contract said
no. Neither of us looked at it, because both of us were thinking about what a person types.

**Resolved on three points, all now in [the protocol](../specs/tessera-protocol-v1.md#discovery).**

**Scope.** The rule covers every URL this protocol hands a client, not only the storage origin, and
`authorization_servers` is named explicitly. **The check happens before the request, not on the
response** — checking the scheme while validating fetched metadata is checking after the thing you
meant to prevent has happened. An entry a client must refuse is an entry it cannot use, so the
first-usable rule passes over it; a server offering only such entries is unusable, which is correct.

**Case.** Scheme and host are case-insensitive in RFC 3986 and implementations do not normalise them.
`HTTP://192.168.1.10` has a scheme equal to neither `"http"` nor `"https"` under exact comparison, so
a guard written as *refuse `http`* admits it; `http://LocalHost` is valid loopback that a guard
written as *permit `localhost`* refuses. **A hole and an outage from one omission**, and the consumer
found both by running the guard rather than by reading it.

**Spelling.** Loopback is a property of the address: `127.0.0.0/8`, `::1` however written — including
`0:0:0:0:0:0:0:1` and `::ffff:127.0.0.1` — and the name `localhost`. The consumer implemented three
literal strings and refused the alternate spellings, asked whether that was wrong, and it is not:
**a client refusing an unusual spelling of a loopback address is conformant.** The asymmetry is now
stated rather than left for each client to guess at, because the two errors are not comparable —
refusing a loopback address costs a developer a confusing error, and accepting a non-loopback one
costs somebody their records.

**What this says about finding 23, one day old.** It was answered as a question about one URL,
because the question arrived about one URL. The right question was *which destinations does this
protocol hand a client, and which of them can be chosen by somebody other than the person* — and
asking it would have found `authorization_servers` immediately. A rule written to the shape of the
question that prompted it is a rule the next reader has to widen.

**One half of this is checkable and is now checked.** Everything else in Discovery is a client
obligation and this suite tests servers, but the opposite direction is a server's behaviour: a
conformant deployment never advertises an `authorization_servers` entry a conformant client would
have to refuse. `CheckCore` now enforces that, with the consumer's own spellings as cases and three
seeded defects — an exact scheme comparison, an exact host comparison, and literal loopback matching
instead of parsing an address.

**Two of those cases prove nothing in Go, and the suite says so where a reader will see it.**
`url.Parse` lowercases a scheme, so `HTTPS://` and `HTTP://` arrive normalised and seeding an exact
comparison changes no result. They are kept because this suite is portable and the hazard is real
elsewhere — it is exactly what the consumer hit in Swift, where `URL` does not normalise. A case
that is inert on one host and load-bearing on another is worth keeping and worth labelling; what it
must not do is sit there implying coverage it does not have here.

Go does not lowercase a host, so those cases are load-bearing on both sides: seeding an exact
comparison fails `loopback name cased`, and seeding literal matching fails all three of the IPv6 and
`127.0.0.0/8` spellings.

### Finding 25: the content protocol issues a credential and never says it must be encrypted

**Raised and resolved 2026-09-25, by asking the question finding 24 said to ask.** Not reported.
Findings 23 and 24 were the same rule twice — 23 answered in the shape its question arrived, 24
found a day later by an agent walking call sites — and 24 recorded the lesson: *a rule written to
the shape of its prompting question is a rule the next reader has to widen.* So the general question
was asked before a third instance could be reported: **which destinations does this protocol hand a
client, and which of them does somebody other than the person choose.**

It found one, and it is worse than either of the first two.

[The content protocol](../specs/tessera-content-v1.md) hands a client a `url` for every read and
every write. Of the read URI it says, in its own words: **"The resolved URI is a credential. For its
lifetime, whoever holds it can read those bytes."** It then constrains that credential's lifetime,
its scope, whether it may be logged or persisted, and what revoking a grant does to it.

**It said nothing about its scheme.** A store could hand a client
`http://bytes.example.org/o/91ab…?X-Amz-Signature=…` and be conformant.

**Two things make it worse than finding 24's case.** There, the plaintext URL was a *destination*
that credentials would afterwards be sent to; here the URI **is** the credential, so observing it in
flight is the whole attack and nothing further has to happen. And what it protects is the bytes of
the records themselves, at an origin the client neither chose nor can inspect.

**The only statement of the rule was that the examples used `https`.** That is the weakest form a
requirement can take: one reader copies the example, another does not, and nothing fails either way.

**Why finding 24's rule did not already cover it.** It says *every URL this protocol hands a client*
and it lives in the record protocol. The content protocol versions independently, and a reader of it
alone never meets that sentence. This is the same failure `internal/speccheck` found once already in
the error tables — the union of two documents being right does not help a reader of one — and the
difference is instructive: there a check found it, here a rereading did.

**Resolved on three points.**

**The rule is stated in the content protocol itself**, among the consequences at the top of the
document rather than beside each use, because a rule stated per use is one the next use is missing.
It covers the resolved read URI and the transfer URI, with the same loopback exemption and the same
check-before-the-request obligation, and it says plainly that it matters more there than in the
record protocol and why.

**The destinations are enumerated** in [the protocol](../specs/tessera-protocol-v1.md#discovery) as
a table with a column for who chooses each: the person for the storage origin, the server for
`authorization_servers` and for both content URIs. The rule has now been widened twice by somebody
meeting a URL the previous wording did not name, so a future one should be **a missing row rather
than a case nobody thought of.**

**It has a server-side conformance case, unlike most of this family.** `CheckContent` reads an
issued URI in five places and now checks the scheme at every one. Refusing to fetch such a URI is a
client obligation this suite cannot test; never issuing one is a server's behaviour, and that is
tested. Seeded by making the reference server issue `http://bytes.example.org/…`, which fails the
suite on both engines.

**And the examples are bound.** `internal/speccheck` now requires every URL in every fenced
specification example to be `https` or loopback, so the pattern that was carrying this rule cannot
silently stop. Fenced blocks only: prose carries plaintext URLs deliberately, as the thing a client
must refuse, and a check that could not tell a counter-example from an example would be switched off
the first time somebody wrote one.

**What this does not close.** Three of the four Discovery rules remain enforceable by nothing. This
stops the *server-side* half of the family being discovered one instance at a time by whoever is
implementing against it, which is a smaller claim than closing it.

### Finding 26: an exact decimal type, spelled `fixed`

**Adopted and implemented 2026-09-25.** Not a defect. [Candidate 2](extension-candidates.md#2-an-exact-decimal-type)
was ranked first in its own document and carried the only expiry on that list: **a value type is the
encoding, not a feature**, so added after clients ship it becomes a permanent question at the top of
every one of them — *does this server know it?* It went in while the answer is still *every server*.

**The name is not `decimal`.** A frozen vector case uses that name as its example of a type no
server knows, and keeping it would leave a negative case passing for a different reason than it was
written for — the failure this directory already has a scar from. `fixed` names the representation
against `float`'s, and `money` was rejected as a vocabulary, which this protocol does not carry.

**What was settled while implementing, beyond what the candidate proposed.**

**A string, and only in canonical form.** The grammar is an optional `-`, an integer part with no
leading zero, and a fraction that ends in a non-zero digit — so `1.50`, `1.0`, `-0` and `1e5` are
all `invalid_field_value`. The decision that took the most thought was refusing rather than
normalising, because normalising is friendlier and is wrong here: this protocol compares raw values
in `expect` and folds them into record versions, so a server that rewrote `1.50` to `1.5` would hand
back a value the caller never sent and produce a mismatch the caller cannot account for. Refusing is
the loud half of that.

**`scale` bounds rather than pads**, and that falls out of the canonical form having no trailing
zero: a scale-2 field stores `1`, `1.5` and `1.25` and refuses `1.255`, which could only be stored by
rounding. It also made the update rule easy — **a scale may be widened and never narrowed**, because
widening rewrites nothing while narrowing would invalidate values already written.

**Sums are strings on the wire.** Over a `fixed` field, `sum`, `minimum` and `maximum` are canonical
strings rather than JSON numbers. A number is exactly where the exactness would die — on the client,
after the server took care to keep it — and a consumer had already reported reducing rows locally
because their decoder declared that member a double.

**`float` to `fixed` is refused outright** rather than conditionally. Every binary64 has an exact
decimal expansion, so *when it is exact* would admit almost every value and answer `0.1` with its
fifty-odd true digits: technically exact, and not what a caller asking for a decimal meant. The
reverse, `fixed` to `float`, is permitted only where nothing is lost, which is the rule `integer` to
`float` already follows.

**Two vector cases, which the candidate designed and which each catch what the other cannot.** A
value beyond 38 significant digits defeats a fixed-width decimal type; `1.00000000000000001` defeats
parsing through binary64. Twenty-six cases were appended in all — the freeze holds, the diff adds
138 lines and removes none — and both traps are now seeded mutants in the suite that checks the
fixtures can catch a broken encoder.

**One thing worth recording about which case proves what.** The unit test for exact sums has five
rows, and seeding an accumulator that goes through a double fails **one** of them. `0.1 + 0.1 + 0.1`
still answers `0.3`, because the total is rendered at the widest scale any addend carried and
rounding to one fraction digit hides the drift. The famous demonstration is the demonstration; the
eighteen-digit row is the test. That is written into the test rather than left for a reader to
discover by seeding it themselves.

**The discovery example gaining `max_fixed_digits` was caught by the limits binding**, for the
second time in three days — the first was `max_aggregate_groups`. A binding that has now caught the
same class twice is one worth having built.

### Finding 27: a dataset stops accepting writes, and says so with the wrong word

**Raised and resolved 2026-09-26.** Found by taking a measurement to confirm a hypothesis, which it
refuted.

**The hypothesis.** That `max_materialized_bytes` would begin refusing *reads* on a large dataset,
so a client asking for ten rows would be refused because of rows it never asked for.

**What actually happens.** The write goes first. With the bound at 1 MiB to reach it quickly, a
single-record insert is refused at 27,500 rows while a query for ten rows still succeeds. **The
dataset becomes unwritable, not unreadable** — and it had been answering that with
`query_result_limit_exceeded` and empty `details`.

Three things wrong with that answer, and each was fixed:

- **The code described a query the caller had not made.** There is no query, no result and no scan;
  the caller inserted one record. A client told its query result was too large looks at what it
  asked for, finds nothing excessive, and has nowhere to go. It is now `507 dataset_full` — not a
  `4xx`, because the request was legal, and not `503`, because waiting does not help.
- **Nothing named the bound.** `sum_overflow`, `group_limit` and `distinct_reference_limit` are all
  required to carry a reason so a client can tell bounds apart; the bound that ends writing
  altogether carried none. Both sides now carry `"materialization_limit"` and the bound in
  `details.maximum`, so one branch handles the limit while the code still says which side failed.
- **`max_materialized_bytes` appeared nowhere in `specs/`.** The engine enforced it, `Limits`
  carried it, and discovery sent it to every client. It is specified now, including the property
  that makes it unlike every other limit: **a caller cannot make a request small enough to satisfy
  it**, because the number that matters is what the dataset holds.

**The binding that should have caught it had a hole, and the hole is the interesting part.** The
limits check compared the discovery example against the `Limits` struct and **skipped fields tagged
`omitempty`**, reasoning that the code might not send them. `max_materialized_bytes` is tagged
`omitempty` and is never zero in practice, so it was sent to every client and checked by nothing.

*Tagged optional* and *actually optional* are not the same thing, and only one of them is
observable. The check now marshals what a server sends and compares that, which caught the
undefined limit on the first run. Third time a limits binding has earned its keep, first time it has
been found wanting.

**What this does not fix.** The bound exists because this engine materialises a whole dataset to
perform one write. Measured the same day: a query for ten rows costs what a query for ten thousand
costs, within four per cent at every size, and an update of one record in a ten-thousand-row dataset
costs 44 ms and 45 MB. Pushing predicates and bounds into SQL is what turns this from a contract
question into a condition no server reaches — and the specification now says a server bounded by the
request rather than by the dataset never meets it, so a client can tell the two apart before
depending on one.

### Finding 28: `limit` bought nothing, and the test meant to prove the fix could not fail

**Implemented 2026-09-26.** Not a contract change; recorded because two of the defects found while
doing it are the kind this review exists to remember.

**The change.** General reads no longer load the dataset. Records are streamed from storage by type
in identity order, decoded one at a time, and the scan stops once it holds what the answer needs —
which it may do only when the final order is identity order, so a result sort or a per-type
selection still reads its whole scope. Only matching rows are kept, and they are charged against
`max_materialized_bytes`, which makes that bound one on what a request holds rather than what a
dataset holds. Writes still load everything: they compute changes by comparing images, and an image
missing records would describe their absence as deletion.

Measured interleaved against the previous build, same machine state, 10,000 rows: a query for ten
rows went from 48.66 ms to 0.20 ms and is now flat in dataset size. A full scan showed +5%, inside
the ±7% the untouched write path moved by, so there is no measurable change there.

**The differential test could not fail at first.** It answers every query twice — streaming, and
from the complete image — and compares. Seeding *stop early even under a result sort* passed it,
because the stop decision was made by the caller and honoured by both paths, so both computed the
same wrong answer and agreed. The complete-image path now ignores the stop entirely and is exactly
the evaluator that preceded this change. **A reference that shares the logic under test is not a
reference.** Reseeded, it fails at once.

**The runtime order check is blind to a one-row read.** The stream verifies identity order as rows
arrive, by comparing each with the one before. Seeding a descending source at `limit 1` returned the
wrong row and the check never fired — there was no second row. So the guarantee for PostgreSQL is
not that check but `COLLATE "C"` on the ordered read, an index in that collation (migration 9) so
stopping early saves work there too, and a test on a locale-collated database with identifiers a
locale orders differently. Seeding the missing collation fails it at exactly `limit 1`.

**And one latent hazard closed on the way.** An evaluator built without its records used to answer
a scan by ranging an empty map — "nothing matched", a narrowed result that looks complete — kept
from clients only by the caller's wrapper. It now refuses.

### Finding 29: a type-scoped share narrows one installation, not the guest

**Resolved 2026-09-26.** Reported the same day by a consumer running the server, reproduced on
`v0.1.0-alpha.9`, and confirmed by reading the code. What shipped is at the end. A privacy defect: an administrator who limits a guest to chosen
types has limited one of that guest's sign-ins, and the next one reads everything.

**What happens.** Sharing read access for named types (`GrantRead`) sets a type-scoped view on the
single authorization it names and records nothing on the account. Every authorization an account
obtains afterwards is built by `newAuthorization` from the account's own dataset grant with a full
view. That includes a new automation token and an OAuth approval from another device, so this is
the ordinary second sign-in rather than a contrived one.

The account must hold `read` on the dataset to be a guest at all, and that grant is what every
future sign-in inherits. The share cannot reach it.

**Why it is worse than a missing feature.** The authorizations page presents the share under *Share
read access* and says a type grant authorizes queries over exactly the named types. An
administrator reads that as the guest's access. It is one installation's.

**What would resolve it**, and the first is the one this protocol's shape points to: type scoping
held on the account's grant for that dataset, so every authorization the account obtains inherits
it, including ones minted after the share. Failing that, the page and the administration documents
must say plainly that a share is per installation, and say how a type-limited guest account comes
into existence.

**Until resolved, type-scoped sharing is not a privacy boundary.** The consumer that reported it
keeps household sharing by placement — a dataset per member — which this defect does not touch.

**What shipped: the first form.** A share records the types on the account's grant for that
dataset. Every sign-in the account already holds there is narrowed at the moment of sharing — a
guest on two devices is one guest, and narrowing one would leave the other reading everything —
and every sign-in obtained afterwards inherits the scope, whether an operator mints a token or the
guest approves an application on another phone.

**Sharing is now refused for an account that writes to the dataset.** When a share narrowed one
authorization, sharing a writer's sign-in left the writer's account alone. Held on the account, it
would quietly make a writer read-only everywhere, or leave an administrator unable to administer.
Restricting a writer is a different decision from sharing with a guest, so it answers `conflict`
rather than being taken by accident.

Tested three ways and each seeded: the reporter's steps through the security suite, including a
sign-in minted after the share; two sign-ins that existed before it; and step 6 over OAuth consent,
which the reporter asked for because a second phone is the ordinary case. Seeding the full view
back into minting fails the first and the third; seeding a share that narrows only the named sign-in
fails the second.

**Not provided: undoing a share.** Nothing in the page removes the scope from an account, and
re-adding the account to the dataset deliberately does not clear it — that would widen a guest's
access as a side effect of an action that looks like an addition. An account that should read
everything again has to be recreated. Recorded rather than left for somebody to discover.

### Finding 30: a transaction bound enforced and never declared

**Raised and resolved 2026-09-26**, by a consumer measuring capacity who sent 500 operations in one
transaction and was refused with a bare `400 invalid_field`. 256 was accepted.

The bound was real — the transaction parser refused more than 256 operations — and it appeared in no
document and in no dataset's `limits`, and the refusal named neither the bound nor the number. It is
the same shape as finding 27's `max_materialized_bytes`, one step earlier in a request: a limit a
client could only discover by being refused, reported with a code that describes something else.

**Resolved:** `max_transaction_operations` is declared in a dataset's `limits`, 256 by default and
for every dataset stored before it had a name, and exceeding it fails `query_result_limit_exceeded`
with `details.reason: "operation_limit"` and the bound in `details.maximum`. The conformance suite
reads the bound from the dataset and sends one operation more; seeding the old bare refusal fails it.

**Worth noting how it surfaced.** The limits binding in `internal/speccheck` checks that every limit
the code *declares* appears in the specification. This one was never declared, so nothing could
compare it to anything. A binding can only hold a rule that exists in one of the two places it
compares — an enforced constant nobody wrote down is outside it by construction.

### Finding 31: behind a reverse proxy, the per-address limits were one budget for the server

**Raised 2026-09-26** by a consumer whose gate was refused `429` after about eight sign-ins in a few
seconds, and resolved the same day in two steps. Every one of its deployments sits behind a reverse proxy.

The reference server counts sign-ins, token requests, registrations and invitations per address,
and took the address from the connection. Behind a proxy every connection comes from the proxy, so
"ten sign-ins a minute per address" was ten a minute for the whole server. A household shared one
budget, and **anyone on the internet could spend it**: ten wrong passwords a minute, with no
credentials, stopped every account on the server from signing in.

**Resolved:** `trusted_proxies` names the proxies, as addresses or ranges. For a request from one of
them, the caller is the right-most `X-Forwarded-For` entry that is not itself a trusted proxy.
Right-most, because everything to its left was written by the caller. It is empty by default: a
server reached directly that believed the header would let every request name a fresh address and
never be limited. `Forwarded` is not read; the proxies in use write `X-Forwarded-For`.

**The rest, resolved 2026-09-26.** Each bucket also had a server-wide ceiling of four times the
per-address limit, there to keep a flood from spending every processor on the 600k-iteration
password derivation. That ceiling was a budget too: four addresses could still close sign-in for
everyone for a minute. It is gone. What protects the processors now is a bound on how many
derivations run at once, one per processor: a flood fills the slots and each attempt waits its turn,
so sign-in slows under attack instead of closing, and only a wait past ten seconds is refused, as
busy, to be retried. Looking for other budgets of the same shape found four:

- **Two derivations ran inside the lock** every bearer-token check takes: redeeming an invitation and
  adding a person, the second reachable through the administration API. Both now derive first. A
  test holds each derivation path waiting for a slot and checks that a token can still be verified.
- **An IPv6 caller was counted per address.** A subscriber is given a /64, so one household could
  present a fresh address every request and never be limited. IPv6 is counted per /64.
- **A full table of counted addresses refused everybody.** Past the bound, now 100,000, new
  addresses share one window and those already counted keep their own, so a flood of new addresses
  does not take sign-in from anybody already using it. Expired windows are swept every few seconds
  rather than walked on every request.
- **Registration was a budget spent once and never returned.** The registry holds at most 10,000
  clients and nothing removed one, so one address at the registration limit filled it in hours and
  no application could register after that. When the registry is full, a self-registered client
  that no sign-in has used and that registered more than an hour ago is removed to make room. An
  application in that position is told `invalid_client` and registers again.

A per-account limit on failed passwords, also suggested, was not added: it would let anyone lock a
named account out, and the derivation bound already limits how fast any account can be guessed.

**What remains** is what any server has: an attack from enough addresses to fill every derivation
slot for longer than ten seconds at a time makes sign-in answer busy until it stops.

### Finding 32: one erasure made every earlier backup unrestorable

**Found 2026-09-26** in the review for real data, and resolved the same day.

Restore refused any backup holding a dataset that the current retirement ledger says was erased, so
that an erasure could not be undone by restoring. That was right when a storage held one dataset.
It now holds one per person — a household's members, a study's participants — and the rule meant
that **one person erasing their dataset made every backup taken before it unrestorable**. With a
daily backup, a disk lost on the day a participant withdrew left nothing to restore, for anybody.

**Resolved:** restore re-applies each erasure the ledger records, inside the transaction that loads
the backup, so erased records are never committed and never served. A dataset erased since the
backup comes back as an empty dataset held by nobody; a content store erased alone comes back
empty; the bytes of either are not written to disk; and SQLite's pages are rewritten before the
restore reports success. Everything else in the backup is restored as it was. A ledger that cannot
be read still refuses the restore, because then there is no telling what must not come back.

**What remains.** On PostgreSQL the erased rows are written and removed inside the restore
transaction, so they pass through its heap and WAL as they would through any PostgreSQL erasure;
encryption at rest is the protection, as the erasure section already says. And a backup still holds
what was erased until it rotates out: an erasure is complete in the live storage at once and in the
backups after `keep` × `every_hours`.

### Finding 33: every sign-in ever made counted against one bound for the whole server

**Found 2026-09-26** in the review for real data, and resolved the same day.

The registry refused a new authorization once it held 10,000, and it counted revoked ones, which
were never removed. So a server in ordinary use would one day refuse every sign-in, for everybody,
and any one account approving again and again could bring that day forward to a few hours. It is
the shape of finding 31 in a different place: a bound meant to protect the server that anyone could
spend for everyone.

**Resolved:** a person holds at most 256 live installations, and approving one more signs out their
oldest — it costs that person one old session and nobody else anything. A service is not capped: it
holds one authorization per dataset it reaches and cannot multiply them. When the registry is full,
revoked authorizations are dropped; their tokens no longer authenticate and their access is already
gone from the storage. The administration pages lose the history of those revocations then, and
only then.

### Finding 34: one person erasing their dataset blanked their administrators' pages

**Reported 2026-09-26** by a consumer, against `v0.1.0-alpha.15`, and resolved the same day.

After a participant erased their own dataset through the protocol, the administration overview
answered `503 dataset_unavailable` for the study's administrator, and kept answering it. Two defects
compounded. The account registry is not in the path of a protocol erasure, so it went on naming the
erased identity, and nobody's account named the replacement. And the overview failed as a whole when
one dataset it was asked to show could not be reached. One participant withdrawing therefore took
away every administrator's view of every participant.

**Resolved:** the composition tells the registry about each erasure made through the protocol. The
person who erased holds the replacement with the grants they had; everybody else's hold ends, as the
storage's already had; the erasing installation follows the replacement; and issuers and template
bindings naming the old identity name the new one. Separately, administration leaves out a dataset
it cannot reach rather than failing, so a registry that falls behind shows less instead of nothing.

### Finding 35: one caller's page sets refused paging to everybody

**Found 2026-09-27** while moving a page set's schema out of its pages, and resolved the same day.

Beginning a page set checked how much page storage was already held against the dataset's
`max_materialized_bytes` — and summed every page set on the server, whoever started it and on
whichever dataset. A caller holding large page sets, which live for fifteen minutes, therefore made
every other caller's page set fail with `query_result_limit_exceeded` until theirs expired. It is
the shape of findings 31 and 33 in one more place: a bound meant to protect the server that one
caller could spend for everybody.

**Resolved:** a caller's page sets are counted per dataset and per authorization, so each is held to
its own bound and nobody else's. The total a server holds is bounded by that times the
authorizations it has issued, each of which an administrator granted, and by the fifteen-minute
lifetime. The test proves both halves: a second caller is not refused, and the first still meets
its own bound.

### Finding 36: replacing a service's secret let a server administrator read what the service reads

**Found 2026-09-27 by an outside review of `v0.1.0-alpha.17`**, and resolved the same day.

`POST /admin/api/v1/services/{account}/secret` checked only that the caller administered the
server. The new secret signs in as the service, so a server administrator holding no data could
replace the secret of a service that reads somebody's dataset and read it — the reviewer did: a
direct read answered `404`, the rotation `200`, a token as the service `200`, the dataset read `200`.
It is the same shape as setting a person's password, which alpha.16 had already closed, in
the one place that had not been checked against it.

**Resolved:** replacing a service's secret takes being the server's administrator and administering
every dataset the service holds, as setting a person's password takes administering every dataset
they hold. A service holding no dataset has nothing to read. The command line, run by whoever holds
the host, is not limited, and is how a secret is recovered where no administrator may read what the
service reads. The same review found two costs, below, in the plan.

### Finding 37: a server administrator holding no data could let themselves into any dataset

**Found 2026-09-27 by an outside review of `v0.1.0-alpha.18`**, and resolved the same day.

The readers API treated administering the server as administering every dataset, so a server
administrator holding no data could make themselves a reader of anybody's dataset and read it —
confirmed by the reviewer. Looking for the same shape found a second door: an issuer into existing
datasets invites whoever holds its secret, and configuring one, or replacing its secret, took only
the server's administrator. Findings 36 and the password rule of alpha.16 had closed the same
escalation through a service's secret and a person's password; these were the routes not yet
checked against it.

**Resolved:** administering the server is not administering a dataset. Managing a dataset's readers
takes administering that dataset. Configuring an issuer into existing datasets, or replacing its
secret, takes the server's administrator who also administers every one of them; a template
issuer, which only creates new datasets, stays the server administrator's. The host's command line
is unlimited, as before.

### Finding 38: a sign-in begun before a recovery could finish after it

**Found 2026-09-27 by an outside review of `v0.1.0-alpha.18`**, and resolved the same day.

Signing in is two steps: proving the password, then approving. A sign-in that had proved the
password and not yet approved survived the account's recovery, and finishing it issued fresh tokens
— to whoever held the old password, which is exactly who a recovery is meant to shut out. Changing
one's own password and an administrator setting it left the same opening, and so did an unconfirmed
account deletion bought with the old password.

**Resolved:** any change to an account's password — by the person, by recovery or by an
administrator — ends every sign-in in progress for it and every unconfirmed deletion of it, as
suspension and deletion already did.

### Finding 39: a password replaced during its own check still counted

**Found 2026-09-27 by an outside review**, and resolved the same day.

Every password check copies the account, releases the registry lock for the deliberately slow
derivation, and compares against the copy. A recovery landing during that derivation left the old
password good for the one request in flight — reproduced by the reviewer, whose old-password sign-in
obtained a valid token after the recovery meant to shut it out. Finding 38 had ended sign-ins already
past this step; this one had not reached it.

**Resolved:** after deriving, every check — sign-in, the administration pages, a recovery code,
changing one's own password, and deleting one's own account, both as the page and as the API — reads
the account again under the lock and requires the same password and no suspension. Self-deletion
does so in the same lock as the deletion itself.

### Finding 40: an online backup could restore broader access than the live server allowed

**Found 2026-09-27 by an outside review**, and resolved the same day.

An online backup took its SQL snapshot, which holds the grants, and copied the account registry
afterwards, with nothing between them. A share narrowed in that gap was in the registry and not the
grants; restored, the same token read the type the live server had just withheld.

**Resolved:** the registry is frozen while the backup pins its SQL snapshot and reads the registry
file, so both describe one moment. Every registry change holds the registry's lock across both its
grant change and its file write, which is what makes the freeze sufficient. It lasts only for the pin
and the read, not the export.

### Finding 41: several processes' retirement ledgers missed each other's erasures

**Found 2026-09-27 by an outside review**, and resolved the same day.

Each process keeps its own retirement ledger, and several may share one PostgreSQL database. What a
process journaled was the retirements not yet journaled, by a flag in the shared database: the first
process to journal an erasure set it, and every other process's ledger missed that erasure for good.
Restoring with such a ledger brought the erased dataset back.

**Resolved:** each process compares every retirement the storage records with what its own ledger
holds and appends the difference, whoever made it. The flag keeps its other meaning, that some ledger
holds a retirement, which cleanup waits on.

### Finding 42: erasing gave the eraser rights it had never held

**Reported 2026-09-27 by a consumer**, and resolved the same day.

The replacement an erasure leaves was provisioned for the erasing authorization with every right, as
this engine's stated policy. A study participant holding read, write and erase — never schema, since
the study defines the types — gained schema on the replacement by erasing, and could define their own
types in a dataset the study issued.

**Resolved:** the erasing authorization is provisioned on the replacement with exactly the grants it
held on what it erased. The protocol asks for "at minimum the erasing caller's", which this is.

### Finding 43: `immutable`, and when it binds

**Adopted and implemented 2026-09-27.** Not a defect. [Candidate 3](extension-candidates.md#3-constraints-belong-in-fieldmeta)
narrowed to this one constraint after both consumers answered, and held on two blocking questions
and a third nobody had asked. The maintainer took the proposed answer to each.

**A value, once written, cannot be changed** — required of every server, since a client cannot check
writers it does not control. Refused with `409 conflict`, `details.reason: "immutable_field"`, naming
the fields. No new code: a client's existing mapping of `conflict` carries over, and the reason is
what tells it apart from a failed `expect`.

**Adding the flag over values that already exist.** The candidate asked what happens when existing
values violate it. None can: the constraint is on what happens next, so it binds from the commit that
adds it, over whatever is stored.

**Whether a subtype may narrow it.** There is nothing to narrow. A field is declared by exactly one
type and a descendant carries that declaration unchanged — the rule that already forbids redeclaring
an inherited field — so a subtype inherits the flag and cannot drop it. The consumer whose hierarchy
runs deep gets the flag everywhere its field appears, which is what it asked for.

**Whether null counts as written.** It does not. A field holding nothing takes its first value by
insert or by update, and is fixed from then on. So a record can be created before the value is known,
which is the common case for a value that is recorded when something happens.

**Who may remove it.** Whoever holds `schema` on the dataset, as a schema edit like any other. The
protection boundary is that grant, not the flag; the change moves `schema_version`, so a write
guarded by `expect_type` notices. Forbidding removal would make the flag the one thing in a schema
nobody could correct.

**What was settled while implementing.** *Restating* the stored value is accepted, and "the stored
value" means the value as stored after conversion, not protocol equality: `-0.0` equals `0.0` in every
comparison the protocol makes and is a different stored value, so accepting it as unchanged would
change what the record returns. A seeded defect using protocol equality is caught by exactly that
case. A server that predates the flag refuses the member with `invalid_request`, so a client never
holds a field it believes is immutable and is not.

**Not an audit trail**, and the specification says so: no writer, no prior value, no protection
against deletion, field removal or erasure.

### Finding 45: one sign-in reading everything shared with a person

**Adopted and implemented 2026-09-27**, on a consumer's request and the maintainer's decision. Not a
defect. Two rules said otherwise and both changed: *an authorization is approved for at most one
dataset*, and *creation is the only thing that widens the reach*.

**The need.** A household where each member reads what the others share needed one sign-in per
sharer, each with its own approval, and another whenever somebody new started sharing. Fine for two
people, fragile the day one joins.

**The shape.** A server advertising `shared_reads` accepts `shared_reads=true` instead of a `dataset`.
The approval covers reading, and only reading, every dataset where the account is a reader — never one
it writes or administers — within each share's types, objects and expiry, **including shares made
later**. A share ending takes effect at once; a new share may wait for the next access token.

**The alternatives, and why not.** One sign-in per dataset is what already existed and is what failed
the household. A named set approved together is the same flow with fewer screens at the start, and it
still needs a new approval when somebody joins, which was the problem.

**What it costs, said in the specification rather than left to be discovered.** A stolen token reads
every share instead of one, bounded as every token is by its lifetime and refresh rotation. And it is
the first approval whose reach grows after it is given without the approver acting, which is why the
approval screen MUST say so and a client SHOULD show what it reads now.

**Amended the same day: beside a dataset.** First shipped alone, so that an approval naming one
dataset could not quietly cover many. The household then needed a person's own dataset and the
shares in one sign-in, since two meant two passwords. On the maintainer's decision the request may
carry both; the approval page shows the dataset's rights and, **unticked**, reading what is shared,
so it still says everything it covers. The shared part never touches the dataset the approval
names nor any the installation created: it removes only access it installed itself.

**Evidence, and its limit.** The importable suite has no authorization flow and cannot prove an
approval, as it cannot prove creation's approval wording. The reference server's security test signs
in through the real flow and checks the approval's wording, the reach, the type scope, the expiry,
read alone whatever the form ticked, the person's own dataset excluded, growth and shrinkage on the
token already issued, refresh, revocation, discovery, and two malformed requests; eleven planted
defects are caught.

### Finding 46: a reader share frozen at the dataset sequence

**Adopted 2026-10-07.** A study may need to keep reading the records a participant
already shared while seeing nothing added after withdrawal. Removing the grant ends
all reading; leaving it alone includes later records. A frozen share intersects its
existing scope with a cut in the dataset's minting sequence, never a timestamp or
identity string order. Records minted before the cut retain their current contents;
edits and deletion still apply. Schema stays readable. Re-consent removes the cut.

**The route belongs to the deployment contract.** Accounts, named shares, template
readers and issuer groups are deployment concepts, and grant management is already
there. The owner surface is `GET /v1/datasets/{dataset}/readers` (and the single
reader route), plus `POST /v1/datasets/{dataset}/readers/{account}:freeze` and
`:unfreeze`. The owner's ordinary installation approved for `write` on that dataset
may change the share without `administer`. An administrator may freeze through the administration API;
only the dataset's recorded owner with write approval may unfreeze. Listings stay
open to any writer with write approval; listing standing does not establish ownership.

**Follow-up review of `c079825`, resolved 2026-10-07.** The original check treated
any writer as owner. A template administrator also receives full grants, so a
researcher approving ordinary read and write could undo the participant's withdrawal.
The registry now records ownership independently of grants: the invitee at template
redemption, the creating account at dataset creation. That record follows erasure
alongside the creating issuer. Neither grants nor a creation replay transfer it.
Both routes require a write-approved installation of that recorded account for
unfreeze. For older and operator-created datasets without recorded ownership,
all dataset administrators are refused unfreeze; other write-approved accounts
retain it. This fallback cannot prove which non-administrative writer owns the
dataset. Administrators can still freeze through their administration route.
The registry version advances again so older binaries cannot discard ownership.

Freeze is idempotent and preserves its first cut. Unfreeze of an unfrozen share
answers `409 conflict`; no standing answers `403 permission_denied`, and a missing
share answers `404 not_found`. Group actions name `issuer:{issuer}`; an individual
reading only through a group answers `409 conflict` with reason `group_reader`, as
other changes to group-derived standing do. Listings carry `frozen_at` and
`frozen_by`. These describe the action, not the visibility boundary. Replacing or
removing and recreating a share cannot clear its freeze; only the owner's unfreeze
can do that. Erasure ends the share with the retired dataset.

**Every read remains inside the authorized view.** Single reads, queries, SQL
sorts, counts, aggregates, distinct references, continued and snapshot pages,
sourced-write scans and attachment resolution apply the same cut. A record outside
it answers `404 unavailable`; a deleted identity minted before the cut answers
`404 not_found`. Missing identities at or after the cut, or without a parseable
engine minting sequence, remain `unavailable`. The lookup uses numeric sequence,
never lexical identity order. Hidden records contribute neither results nor bounds
that disclose their population. Freeze and unfreeze change `view_version` and make
old cursors answer `410 view_changed`. Existing installations and future sign-ins
inherit the account's share; future group members inherit the group's cut.

The reference server stores the cut in access view JSON and retains it in the
identity registry for subsequent installations; no SQL migration is needed. The
registry version advances so an older deployment refuses to discard this policy.
Security cases use real sign-ins and template-created datasets; comparisons on
both SQL engines use an independent image containing only pre-freeze records.
Six executable seeded defects are caught and restored: omitted SQL-sort cuts,
count/aggregate cuts, continuation cuts, administrative unfreeze, unchanged view
generation, and identity string comparison. The SQL comparison catches sorting,
continuation and comparison defects; the template security case catches the other
three. Group fallback is covered independently.

The follow-up seeds restore any-writer unfreeze and conceal all missing records.
`TestFrozenTemplateReaderWithdrawalAndReconsent` catches the former with the
researcher's ordinary read/write approval, and covers both routes, an added
writer, and the participant's successful unfreeze. `TestFrozenLookupMissingMintedSequence`
catches the latter in both in-memory and sourced lookup; the security case also
checks deleted pre-cut and post-cut records. Ownership creation, replay, erasure,
restart, legacy standing and write approval have narrow identity tests.

### Finding 44: an `insert` grant, and what it does not convey

**Adopted and implemented 2026-09-27.** Not a defect. [Candidate 4](extension-candidates.md#4-an-insert-grant-right)
proposed the mechanism and left one decision open — how it composes with a read grant the same
principal holds — which the maintainer settled on the proposed answer.

**The sentence the candidate asked for is in the specification, in those terms:** an insert grant
conveys no continuing authority over what was written. A contributor adds records of exactly the
concrete types it names and nothing else, and what it adds is the dataset's from the moment it
commits.

**Composition is by type and by right, and there is no rule of the form *its own records*.** A
principal may read some types and add to others, or both to one. A record it added to a type it also
reads is visible because it reads that type. The alternative — letting a contributor see what it
wrote — would make a stored fact (who wrote a row) decide visibility, which is the one thing *visibility
comes from the grant table, never from field values* rules out, and it would be the first step of the
review-and-correction workflow the candidate says this must not grow into.

**Settled while implementing.**

- **It conveys the schema of the types it adds to.** Writing a record needs its fields; a schema
  discloses no record. Other types stay `unavailable`.
- **A contributor with no read is refused reads as an operation, `403`**, including a read of the
  record it just added — the same answer for every identity, so it discloses nothing.
- **`Prefer: return=representation` is declined unless the caller writes.** Read alone was the rule
  and was not enough: a contributor may read one type and add to another, and only a caller that
  writes holds the whole dataset in view. A seeded defect restoring the old rule is caught.
- **An installation adds only where it was approved to.** Consent has an *add records* box, on by
  default and granting nothing to an account that does not contribute. The reference server now
  records what each installation was approved for, so when an account later becomes a contributor its
  existing installations gain adding only if they were approved for it.
- **Naming a contributor takes administering the dataset**, as naming a reader does after finding 37.
- **Content is not conveyed.** Attaching a `media` value needs the content protocol's authority.

**Costs, stated in the deployment contract rather than solved here:** a contributor is a write
amplifier bounded only by the dataset's limits, so admission and rate are the deployment's; and an
owner who did not want a contribution deletes it like any record.

### Finding 19: a view cannot be a predicate, so privacy is done by placement

**Closed 2026-09-21 as recorded rather than resolved.** Nothing is owed and nothing is scheduled;
the requirement is written down so a later extension starts from one. Recorded because a use case is
attached to it, which is what the `View` type's own comment asks for: *"Row predicates and policy hooks require a later, explicitly
tested extension."*

The case: one dataset where each member sees their own records plus what others chose to share. That
is unreachable today, and the reason is structural rather than an omission. A restricted `View` is an
enumerated `ObjectView`, so a writer cannot see the row it just inserted — which is exactly why
`SetAuthorization` refuses a non-full view together with `write`. The refusal is right; the
limitation is the enumeration.

The consumer is **not waiting for it** and will do privacy by placement — a dataset per member, plus
a shared one — which works today and costs nothing operationally now that one storage holds many.
Recorded so that whoever picks the extension up has a requirement rather than a hypothesis.

**Why it closes now.** The structural claim still holds — `SetAuthorization` refuses a non-full view
together with `write`, and it is right to — so there is nothing to fix and nothing to decide. What
changed is the alternative: placement was a workaround when this was filed and is now the supported
arrangement. A storage holds many datasets, administration is per dataset, a dataset can be created
over the protocol or from the administration surface, and an account holds each dataset separately.
The same recommendation was given to a consumer designing exactly this household this week, on those
grounds rather than as a consolation.

So the finding is not a gap being carried. It is a requirement filed against a version that does not
exist yet: **one dataset where each member sees their own records plus what others chose to share**,
which needs a view that is a predicate rather than an enumeration, and needs the writer to be able
to see the row it just wrote. Anyone taking that up should start here and not from a hypothesis.

### Finding 20: the steps between a hostname and a session are not described

**Closed 2026-09-21. All three are written down.** Raised by evaluating whether a person can type `myserver.com` and reach a sign-in screen
without being asked for an endpoint. They can, and the design is deliberate about it: discovery
carries `authorization_servers`, the server is its own authorization server publishing
[RFC 8414](https://www.rfc-editor.org/rfc/rfc8414.html) metadata, and registration is
[RFC 7591](https://www.rfc-editor.org/rfc/rfc7591.html), open by default, because *"a client cannot be
pre-registered with a server nobody has deployed yet."* What is missing is smaller and entirely on the
client side: three steps a client must take that the contract does not describe, so two conformant
clients can take them differently against the same server.

**a. Turning what a person typed into an origin.** Nothing says a client resolves a bare authority to
`https://`. It is the only defensible reading, which is the argument for writing it down rather than
against: a client that tries `http://` first — on a typo, on a captive portal, on a hostname that does
not resolve — has performed a downgrade the contract never sanctioned, and the person who typed a
hostname cannot tell. A `MUST` costs nothing here.

**b. Where `/.well-known/tessera` lives when the server does not own the origin.** Well-known URIs are
rooted at the origin per [RFC 8615](https://www.rfc-editor.org/rfc/rfc8615.html), so a deployment
reverse-proxied under a path prefix is undiscoverable unless it also serves the well-known path at the
root. That is a real deployment shape and the contract is silent, which makes it a surprise found
after configuration rather than before.

**c. Which entry of `authorization_servers` a client uses.** The member is an array and nothing says
how to choose among several: first entry, prompt the person, or refuse more than one. A server
listing two gets different behaviour from different clients, and the one case where several is
genuinely useful — migration from one issuer to another — is also the case where picking wrongly
strands a person.

**What the implementation does provisionally.** It serves one authorization server and publishes its
metadata and registration endpoint, so none of the three is exercised: the array has one entry, the
deployment owns its origin, and the client under test was given an origin rather than a typed
hostname.

**Resolved, and the answers are the ones the finding argued for.** In
[Discovery](../specs/tessera-protocol-v1.md#discovery), as three client obligations stated together
before any route is reached:

*(a)* A bare authority resolves to `https://`, and a client MUST NOT try `http://` first or fall
back to it. The finding's own argument decided it — a person who typed a hostname cannot see which
scheme was used, so a probe on a typo or a captive portal is a downgrade nobody sanctioned and
nobody can observe. If `https://` fails, the attempt fails.

*(b)* Discovery is at the origin root and a client MUST NOT look under a prefix. The obligation that
follows is the deployment's — a server behind a path prefix MUST also serve `/.well-known/tessera`
at the root of its origin — and it is written in
[the deployment contract](../specs/tessera-deployment.md), which is where the finding suggested it
belonged. Putting the client rule in the protocol and the deployment rule in the deployment contract
is what makes the pair coherent: the client is forbidden from guessing, so the deployment cannot
leave it to guess.

*(c)* `authorization_servers` is in the server's order of preference and a client takes the first it
can use. A client MUST NOT ask a person to choose — nobody typing a hostname knows what an
authorization server is, and a wrong choice strands them. A server SHOULD list one, and lists
several only while migrating between issuers, which is the case where the order carries the answer.
"Refuse more than one" was rejected because it forbids the migration; "prompt" was rejected because
it asks a person a question they cannot have an opinion about.

**What this does not have, and it is the honest limit.** No conformance cases, and not for want of
trying: (a) and (c) are things a *client* does, and this suite tests servers. A server cannot be
made to fail because a client downgraded a scheme. So two of the three are prose that nothing can
hold anyone to — which this project has spent a week arguing is the weak form of a rule.

They are written anyway, because the alternative is worse. Silence about a step two clients must
take identically is not neutrality; it is each client inventing an answer, and one of the answers
available for (a) is a silent downgrade. A rule nothing enforces is weaker than a tested one and
stronger than none, and saying which of the three it is belongs in the record.

(b) is the exception and is enforceable from the deployment side: a server that does not answer at
its origin root fails discovery, which every suite here begins with.

**What breaks either way.** Leaving all three out keeps the contract silent about client behaviour,
which is consistent with how it treats clients elsewhere — but these are not behaviours a client
chooses for itself, they are behaviours two clients must agree on to meet the same server. Writing
them in adds three sentences and a `MUST NOT` on scheme downgrade, at the cost of the protocol
describing a step that happens before any of its own routes are reached. The path-prefix rule may
belong in [self-hosting](self-hosting.md) rather than the contract, since it constrains a deployment
rather than a request.

There are no conformance cases, and (a) and (c) may not be reachable by a suite that is handed an
origin and a token. They are client-conformance questions, which this repository has no vehicle for
yet.

**Not part of this finding:** the OAuth flow is implemented and covered by `make integration-auth`,
and has not been exercised by a second implementation — the independent consumer has passed the
record flows and not this one. That is a verification gap tracked as a release precondition, not an
open contract question, and it is noted here only so that it is not mistaken for one.

### Finding 47: selecting and retiring protocol versions

**Resolved by the owner, 7 November 2026.** The request authorizes highest-common
version selection, a 12-month minimum overlap and a deprecation signal. The
[versioning contract](../specs/tessera-protocol-v1.md#versioning-and-compatibility) now defines:

- Unordered, distinct canonical positive decimal majors; choose the numerically highest
  common record-protocol version without machine-width truncation. No common major
  refuses connection before authentication/data requests. Deprecation does not change
  selection. Content and implementation versions are independent.
- At least 12 calendar months after the successor protocol major's first production
  release, extended while released clients require the old major. Leap-day boundaries
  clamp to 28 February. Dated operator evidence and consumer confirmation are required.
- Optional `deprecated_versions` discovery entries with advertised `version` and exact
  UTC-seconds `sunset`. Omitted/empty means no announcement. Dates are plans and cannot
  retire a route automatically. At least 14 days' advance retirement notice and the
  discovery signal are required; postpone the date if continued support is needed.
  No `Sunset` header is required, avoiding two signals that could disagree.

The reusable server and standalone configuration validate and publish this signal;
by default it is omitted and v1 is not deprecated. Invalid entries fail startup without
logging their values. Client selection is exposed by `pkg/protocol.SelectProtocolVersion`.
Importable conformance checks validate discovery and run portable client-selection
vectors. Core discovery now tolerates unknown response fields as v1 already requires.
New selection/discovery vector files leave frozen cases unchanged; seeded broken
selection and served metadata demonstrate that the checks detect behavior defects.

Calendar overlap, continued use, client warnings and absence of auth/data requests on
incompatibility need dated operator or independent client evidence. Server checks cannot
certify them. They remain explicit acceptance obligations, not claims of completed
consumer acceptance. The [review freeze](releasing.md#app-review-freeze) remains active.

## Current implementation

[Supported features](feature-matrix.md) owns current implementation claims and limitations.
[Self-hosting](self-hosting.md) owns configuration, restore and deployment constraints.
Historical alignment notes have been removed; regression tests remain alongside the code.
