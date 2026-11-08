# Asked and answered

Look up a contract question before proposing a change. This index links to the
[contract review](tessera-spec-review.md), [extension decisions](extension-candidates.md)
and [design rationale](../specs/tessera-decisions.md); it is not normative.

A declined proposal needs new evidence. An approved proposal may still be unscheduled.
An unowned question has no accepted proposal yet. Read the linked reasoning before
reopening a decision.

## Reading the contract

| Question | Answer | Where |
| --- | --- | --- |
| What does a dataset identity answer when it is not mine? | **Settled.** A retired identity answers `not_found`; an identity naming nothing does not; a live dataset you hold nothing on is concealed, not refused | [Finding 15](tessera-spec-review.md#finding-15-what-a-dataset-identity-answers-when-it-is-not-the-callers) |
| How does a dataset come to exist? | **Settled.** `POST /v1/datasets`, advertised as `dataset_creation` | [Finding 16](tessera-spec-review.md#finding-16-the-protocol-describes-no-way-for-a-dataset-to-come-to-exist-nor-for-a-server-scoped-operation-to-be-asked-about) |
| How do I ask about a server-scoped operation? | **Settled.** `POST /v1/operations/{key}:status` | [Finding 16](tessera-spec-review.md#finding-16-the-protocol-describes-no-way-for-a-dataset-to-come-to-exist-nor-for-a-server-scoped-operation-to-be-asked-about) |
| Does a replay outlive the authority that permitted the original? | **Settled.** Yes; both transitions have cases at the HTTP boundary | [Finding 17](tessera-spec-review.md#finding-17-whether-a-replay-outlives-the-authority-that-permitted-the-original) |
| How does a server-level right come to be held? | **Settled.** An authorization is over at most one dataset and reaches what it creates | [Finding 21](tessera-spec-review.md#finding-21-a-server-level-right-has-no-way-to-be-held) |
| Does creating a dataset widen an authorization's reach? | **Yes, and that is the rule.** Approved for one dataset, reaches that one plus what it creates; withdrawal removes the whole reach | [Finding 22](tessera-spec-review.md#finding-22-creating-a-dataset-silently-widens-an-authorizations-reach) |
| What does a bare hostname resolve to? | **`https://` only.** No `http://` probe and no fallback — a scheme the person cannot see must not be downgraded on their behalf | [Finding 20](tessera-spec-review.md#finding-20-the-steps-between-a-hostname-and-a-session-are-not-described) |
| Can an operator name a deployment for people? | **Yes, since 2026-10-07.** Optional top-level `display_name` in discovery, from configuration; an unauthenticated claim shown next to the host name, never identity | [Discovery](../specs/tessera-protocol-v1.md#discovery) |
| Where does discovery live? | **The origin root.** A client must not look under a prefix; the deployment contract carries the matching obligation | [Finding 20](tessera-spec-review.md#finding-20-the-steps-between-a-hostname-and-a-session-are-not-described) |
| Can an account be created, deleted or suspended without the operator's command line? | **Yes, since 2026-09-26.** Invitations from an operator-configured issuer, into named datasets or from a template that creates one per person; suspension, reversible; deletion, which retires the name. From `/admin/principals` or the command line | [Plan](architecture.md) |
| Can I query or aggregate across many datasets at once? | **No.** Cross-dataset query stays out for the reason the overview gives; loop over datasets | [Overview](../specs/tessera-overview.md) |
| Does a type-scoped share limit what a guest can read? | **Yes, since 2026-09-26.** The scope is held on the guest's account: every existing sign-in is narrowed and every later one inherits it, including an application approved on another device. Sharing is refused for an account that writes to the dataset. A share cannot yet be undone | [Finding 29](tessera-spec-review.md#finding-29-a-type-scoped-share-narrows-one-installation-not-the-guest) |
| How many operations may one transaction carry? | **`max_transaction_operations`, in a dataset's `limits`** — 256 by default. Past it the refusal says `operation_limit` and the bound | [Finding 30](tessera-spec-review.md#finding-30-a-transaction-bound-enforced-and-never-declared) |
| Can an operator raise a dataset's limits, or change the defaults? | **Yes.** Defaults by configuration, member by member; one dataset's with `-set-dataset-limits` or the administration API. Retention may only grow | [Self-hosting](self-hosting.md#limits) |
| Behind a reverse proxy, are the rate limits per caller? | **Yes, once `trusted_proxies` names the proxy.** The caller is the right-most `X-Forwarded-For` entry that is not a trusted proxy. Empty by default, so a server reached directly never believes the header. There is no server-wide budget behind the limits any more; a bound on concurrent password derivations protects the processors instead | [Finding 31](tessera-spec-review.md#finding-31-behind-a-reverse-proxy-the-per-address-limits-were-one-budget-for-the-server) |
| Can a server administrator manage accounts without holding their data? | **Yes.** They see and act on every account, but an account's datasets and applications show only through a dataset they administer. They can approve an installation that administers and reaches no dataset. `server_access.create_dataset` is not a signal for this; `server_administrator` in the overview is | [Deployment contract](../specs/tessera-deployment.md) |
| Can a person delete their own account? | **Yes, with their password**, at `POST /oauth/account/delete` — not with a bearer token. A dataset they alone administer is erased with the account, only when they name it back; a refusal lists those datasets | [Deployment contract](../specs/tessera-deployment.md), [Self-hosting](self-hosting.md#suspending-and-deleting-accounts) |
| After somebody erases their dataset, who holds the replacement? | **The person who erased it, with the grants they had.** Everybody else's hold ends, as the storage's does; issuers and template bindings follow the new identity. Administration no longer fails when a dataset cannot be reached | [Finding 34](tessera-spec-review.md#finding-34-one-person-erasing-their-dataset-blanked-their-administrators-pages) |
| Can one caller's page sets stop somebody else paging? | **No, since 2026-09-27.** Page storage is counted per dataset and authorization; it used to be summed over the whole server | [Finding 35](tessera-spec-review.md#finding-35-one-callers-page-sets-refused-paging-to-everybody) |
| Can one account exhaust sign-ins for the whole server? | **No, since 2026-09-26.** A person keeps at most 256 live installations and a new one signs out their oldest; revoked authorizations are dropped when the registry is full | [Finding 33](tessera-spec-review.md#finding-33-every-sign-in-ever-made-counted-against-one-bound-for-the-whole-server) |
| How does somebody who forgets their password get back in, if nobody may reset it? | **Their recovery code**, shown once when the account is made from an invitation, or asked for at `/oauth/account/recovery` with the password; used at `/oauth/account/recover` | [Self-hosting](self-hosting.md#passwords) |
| May a server administrator set anybody's password? | **No, since 2026-09-26.** Setting a password is signing in as the person, so it takes administering every dataset they hold; suspending and deleting do not. The person sees `password_set_by` in `/oauth/account` until they choose their own | [Self-hosting](self-hosting.md#passwords) |
| Does deleting a member erase a dataset a custodian also administers? | **Only if the template said so**, with `erase_with_account`, and still only when the deletion names it | [Self-hosting](self-hosting.md#invitations) |
| Can a product offer "delete my account" without handling the password? | **Yes.** Send the person to `/oauth/account/delete`, a page Tessera renders, with `client_id` and a registered `return_to`. `/oauth/account/password` changes a password the same way | [Self-hosting](self-hosting.md#suspending-and-deleting-accounts) |
| Can I restore a backup taken before somebody erased their data? | **Yes, since 2026-09-26.** The erasure is applied again during the restore and everything else comes back; it used to refuse the whole backup. Erased data stays in older backups until they rotate out | [Finding 32](tessera-spec-review.md#finding-32-one-erasure-made-every-earlier-backup-unrestorable) |
| How does a service learn the identity of its dataset? | **From the file `-dataset-output` writes**, and with `-new-dataset` the dataset is one nobody else holds. A token request must still name the dataset: inferring it when a service holds one would work until a second appears and then fail at a moment unrelated to anything the service did | [Self-hosting](self-hosting.md#services-and-automation) |
| Does administering the server let someone read a dataset? | **No, since 2026-09-27.** Managing a dataset's readers takes administering it, and so does configuring or re-secreting an issuer into it | [Finding 37](tessera-spec-review.md#finding-37-a-server-administrator-holding-no-data-could-let-themselves-into-any-dataset) |
| Can a password replaced mid-check still sign in? | **No, since 2026-09-27.** Every check reads the account again after deriving | [Finding 39](tessera-spec-review.md#finding-39-a-password-replaced-during-its-own-check-still-counted) |
| Can an online backup pair a registry and grants from different moments? | **No, since 2026-09-27.** The registry is frozen while the snapshot is pinned and the registry read | [Finding 40](tessera-spec-review.md#finding-40-an-online-backup-could-restore-broader-access-than-the-live-server-allowed) |
| With several server processes, does each retirement ledger learn every erasure? | **Yes, since 2026-09-27**, at each process's next maintenance pass | [Finding 41](tessera-spec-review.md#finding-41-several-processes-retirement-ledgers-missed-each-others-erasures) |
| What may the eraser do with the dataset an erasure leaves? | **Exactly what it could before, since 2026-09-27**; it used to be given every right | [Finding 42](tessera-spec-review.md#finding-42-erasing-gave-the-eraser-rights-it-had-never-held) |
| Does a sign-in in progress survive a password change or recovery? | **No, since 2026-09-27.** Any password change ends it, and any unconfirmed deletion | [Finding 38](tessera-spec-review.md#finding-38-a-sign-in-begun-before-a-recovery-could-finish-after-it) |
| Can a lost service secret be replaced? | **Yes.** Through `POST /admin/api/v1/services/{account}/secret` by the server's administrator who also administers every dataset the service holds, since the new secret reads what the service reads; with `-rotate-service-secret` on the host, unlimited. The service keeps its authorizations and its receipts | [Finding 36](tessera-spec-review.md#finding-36-replacing-a-services-secret-let-a-server-administrator-read-what-the-service-reads) |
| Can an invitation lead back into the application? | **Yes, since 2026-09-27.** Add `client_id` and `return_to` to the redeem link; the page offers **Continue** only to one of that client's registered redirect URIs. A private-use scheme needs a dot, such as `com.example.app:/connect` | [Self-hosting](self-hosting.md#invitations) |
| Can an application tell a suspended account from a signed-out one? | **Yes, since 2026-09-27.** `account_suspended`, answered only after proof — the password at sign-in, a valid code or refresh token, a service's secret. Do not send the person to sign in again | [Deployment contract](../specs/tessera-deployment.md) |
| Can a Home's owner stop one member sharing one Track? | **Yes, since 2026-09-28, where the Home's issuer was configured with `moderate_sharing`**: `/admin/api/v1/unshare` naming the member's dataset. The owner reads nothing and deletes nothing; the member sees who stopped it and when, and may share again. On the maintainer's decision, opt-in per issuer, so nobody who joined without it is under it | [Deployment contract](../specs/tessera-deployment.md) |
| Can the owner stop a Track being shared with the whole household? | **Yes, since 2026-09-28.** `POST /admin/api/v1/unshare` with the type, first with `dry_run`. Every share of it ends, group and named, on each dataset the owner administers. **Nobody's records are deleted**: an owner never deletes a member's own data, on the maintainer's decision | [Self-hosting](self-hosting.md#administration-api) |
| When did a member join, since when are they paused, when was the last backup? | **In the overview, since 2026-09-28**: `created_at`, `suspended_at`, `invited_by` per account, and `server.last_backup_at` for the server's administrator, kept across restarts. Earlier accounts read `null` | [Self-hosting](self-hosting.md#administration-api) |
| Can a member share with the whole household, including people who join later? | **Yes, since 2026-09-27.** `PUT /admin/api/v1/datasets/{d}/readers/issuer:{issuer}` by the dataset's administrator, for the issuer that made their own account. Every invitation that issuer hands out then reads every such share | [Self-hosting](self-hosting.md#content-and-sharing) |
| Can one sign-in read everything shared with a person? | **Yes, since 2026-09-27, where the server advertises `shared_reads`.** Ask `/oauth/authorize` with `shared_reads=true` instead of a `dataset`: read only, every dataset where the account is a reader and never one it writes, within each share, including shares made later — which the approval says | [Finding 45](tessera-spec-review.md#finding-45-one-sign-in-reading-everything-shared-with-a-person) |
| Can an administration app issue invitations without holding an issuer's secret? | **Yes, since 2026-09-27.** `POST /admin/api/v1/issuers/{issuer}/invitations` on a server administrator's sign-in approved to administer; an issuer into existing datasets also takes administering them. Cap, template and disabling apply, and revoking the sign-in ends the invitations it issued that nobody has redeemed | [Self-hosting](self-hosting.md#invitations) |
| Can an issuer's cap be raised, the issuer stopped, or its secret replaced? | **Yes**, by the server administrator, from the command line or `/admin/api/v1/issuers`. Disabling also stops outstanding invitations; rotation leaves them working. Its datasets, template, readers and grants do not change — configure a new issuer | [Self-hosting](self-hosting.md#invitations) |
| Can a service read every dataset a template creates, without a consent screen per dataset? | **Yes, as a template reader.** The issuer's policy names readers, services included, optionally scoped to types; each gets read on each dataset in the step that creates it, and asks for a token per dataset with `client_credentials`. Read only | [Self-hosting](self-hosting.md#invitations) |
| Does a type-scoped share narrow a service the way it narrows a person? | **Yes.** The scope is held on the account whatever its kind, and every token it gets, `client_credentials` included, carries it. An administrator can set a service's types through the readers API | [Deployment contract](../specs/tessera-deployment.md) |
| How does a client learn whose token it holds? | **`GET /oauth/account`**, with any valid token: the account, its kind, whether it administers the server or may create datasets, and what the installation was approved for. Not in the protocol listing, which never names an account | [Self-hosting](self-hosting.md#administration-api) |
| Can an issuer choose the account name, for pseudonymous participants? | **Yes, with `assign_names`.** The name is reserved when the invitation is issued and fixed on the redeem page. Template datasets may be named `{account}`, and an administrator can rename a dataset | [Self-hosting](self-hosting.md#invitations) |
| Can a product administer the server without scraping the pages? | **Yes, `/admin/api/v1`**, with the administrator's own token approved to administer. Readers are managed by account and get their types in the same step as read | [Deployment contract](../specs/tessera-deployment.md) |
| Does `limit` make a query cheaper? | **Yes, since 2026-09-26** — when the order is identity order. A result sort or per-type selection still reads its whole scope | [Finding 28](tessera-spec-review.md#finding-28-limit-bought-nothing-and-the-test-meant-to-prove-the-fix-could-not-fail) |
| What is `max_materialized_bytes`, and why was my insert refused? | **The bytes a server holds at once to perform an operation** — and unlike every other limit, no smaller request satisfies it. A read past it is `422 query_result_limit_exceeded`; a write is `507 dataset_full`, both with `details.reason: "materialization_limit"` | [Finding 27](tessera-spec-review.md#finding-27-a-dataset-stops-accepting-writes-and-says-so-with-the-wrong-word) |
| Must the content `url` I am handed be `https`? | **Yes, unless its host is loopback** — and it is the sharpest case, because that URI *is* a credential rather than a destination credentials are later sent to. The rule is in the content protocol itself, not by reference, and the suite fails a store that issues one | [Finding 25](tessera-spec-review.md#finding-25-the-content-protocol-issues-a-credential-and-never-says-it-must-be-encrypted) |
| May a server point me at a plaintext `authorization_servers` entry? | **No, and check before you fetch it.** The rule covers every URL the protocol hands a client, not just the storage origin — and this is the one where the far end chooses the destination. Scheme and host compare case-insensitively; loopback is the address, not its spelling | [Finding 24](tessera-spec-review.md#finding-24-a-server-can-send-a-client-somewhere-plaintext) |
| May a client use an `http://` URL it was handed? | **No, except on loopback** — `127.0.0.1`, `::1`, `localhost`. Not falling back to `http` and refusing it are different requirements, and a client can satisfy the first while sending records over plaintext. A private address is not loopback | [Finding 23](tessera-spec-review.md#finding-23-not-falling-back-to-http-is-not-the-same-as-refusing-it) |
| What if `authorization_servers` has more than one entry? | **Server preference order, first usable wins.** Never prompt | [Finding 20](tessera-spec-review.md#finding-20-the-steps-between-a-hostname-and-a-session-are-not-described) |
| Why did my exact integer sum come back rounded? | **It did not.** Sums are exact JSON integers in the signed 64-bit range and refuse outside it with `sum_overflow`; a client decoding the member as a float loses that on its own side | [Finding 14](tessera-spec-review.md), [cases](../specs/tessera-protocol-v1.md#aggregate-sum-conformance-cases-finding-14) |

### Operator-designated website sign-in (owner decision, 5 November 2026)

A dedicated first-party Console may authenticate under an operator policy
without individual OAuth permission selection, including administration only
where the account currently holds it. This is a narrow exception to the previous
consent requirement, not automatic approval of the existing picker. It is bound
to an operator-provisioned identity, exact origin and callback; all other clients
retain explicit consent. Account privacy/view boundaries, study consent,
revocation, suspension and sensitive-operation checks remain in force.
See the [deployment contract](../specs/tessera-deployment.md#what-a-deployment-must-provide)
and [reference configuration](self-hosting.md#first-party-console-sign-in).

### Native/ordinary client consent reuse (owner request, 5 November 2026)

Client names/icons remain untrusted registration metadata. Ordinary system-browser
OAuth may summarize requested held grants, keep individual choices in a disclosure,
and reuse the most recent live, exactly bound consent after fresh sign-in without
expanding it. `prompt=consent` forces review; this does not enable first-party
website policy. See [the client presentation contract](self-hosting.md#native-and-ordinary-oauth-client-presentation).

## Asked for and approved

| Question | Answer | Where |
| --- | --- | --- |
| May `group_by` name a reference field? | **Shipped 2026-09-23.** The group is the bare qualified identity `distinct_references` returns, compared on the whole triple. `max_aggregate_groups` is in `limits`, still 32 | [Finding 18](tessera-spec-review.md#finding-18-whether-groupby-may-name-a-reference-field) |
| An exact decimal value type? | **Shipped 2026-09-25, spelled `fixed`.** A canonical string, `scale` per field, exact across `integer` and `float`, exact sums returned as strings. Not `decimal`: a frozen vector uses that name for a type no server knows | [Finding 26](tessera-spec-review.md#finding-26-an-exact-decimal-type-spelled-fixed), [Candidate 2](extension-candidates.md#2-an-exact-decimal-type) |
| Are the three optional reads really optional? | **No longer.** `aggregation`, `distinct_references` and `text_matching` are required operations; a server MUST NOT advertise them | [Candidate 1](extension-candidates.md#1-the-three-optionalreads-are-not-capabilities) |
| `immutable` in `FieldMeta`? | **Shipped 2026-09-27, alone.** A value once written cannot be changed; null is unwritten, restating is not a change, a subtype inherits it, and a schema holder may remove it. Refused as `409 conflict` with `details.reason: "immutable_field"`. Not an audit trail | [Finding 43](tessera-spec-review.md#finding-43-immutable-and-when-it-binds), [Candidate 3](extension-candidates.md#3-constraints-belong-in-fieldmeta) |
| An `insert` grant right, for append-only fan-in? | **Shipped 2026-09-27.** Adds records of exactly the named concrete types and conveys nothing else — no read, no update, no delete, not even of its own record; it sees the schema it writes against. Composes with read by type, never by authorship. `access.insert` reports it. Managed by account at `/admin/api/v1/datasets/{d}/contributors` | [Finding 44](tessera-spec-review.md#finding-44-an-insert-grant-and-what-it-does-not-convey), [Candidate 4](extension-candidates.md#4-an-insert-grant-right) |

## Version compatibility request

**Resolved by the owner on 7 November 2026.** Select the highest shared numeric major;
refuse a connection with no common version. Keep majors alongside successors for at least
12 calendar months after the successor protocol's first production release, and longer
while released clients require them. Announce retirement at least 14 days ahead using
optional discovery `deprecated_versions` entries with `version` and UTC `sunset`.
See [Finding 47](tessera-spec-review.md#finding-47-selecting-and-retiring-protocol-versions)
and the [versioning contract](../specs/tessera-protocol-v1.md#versioning-and-compatibility).
The [release procedure](releasing.md#app-review-freeze) owns the active review freeze,
isolated qualification, consumer confirmation and upgrade notice.

## Asked for and held

Held is not declined. Each of these is waiting on a consumer who needs it, and saying so is the
answer — a feature with no user is one whose shape gets decided by guesswork.

| Question | Answer | Where |
| --- | --- | --- |
| A journal or change feed? | **Held.** The reservation argument does not survive; the audit motive does, whenever statutory retention demands it | [Candidate 5](extension-candidates.md#5-a-journal) |
| A grant directory? | **Deferred.** Neither consumer would call it today | [Candidate 6](extension-candidates.md#6-a-grant-directory) |
| Cross-row invariants? | **Held for want of a consumer**, on the same footing as referential integrity | [Candidate 7](extension-candidates.md#7-cross-row-invariants) |
| Record provenance — who wrote this row? | **Only where the insert right lands**, and specified after it. It shrank twice and may not survive a third look | [Candidate 8](extension-candidates.md#8-record-provenance) |
| One dataset where each member sees their own records plus what others shared? | **Recorded, not scheduled.** Structurally unreachable: a restricted view is an enumeration, so a writer cannot see the row it just inserted. Privacy by placement — a dataset per member plus a shared one — is the supported arrangement | [Finding 19](tessera-spec-review.md#finding-19-a-view-cannot-be-a-predicate-so-privacy-is-done-by-placement) |
| Grouped-winner selection — which record was the latest in each group? | **Unowned.** Nobody has proposed it. Scalar extrema do not identify source records, and this is the same gap as selection offering first-per-concrete-type | [Finding 18](tessera-spec-review.md#finding-18-whether-groupby-may-name-a-reference-field) |
| A composite group key — two fields at once? | **Not settled, and not a rider on reference grouping.** One call per value of the second field, with that field in the filter, is bounded by that field's cardinality rather than by row count | [Finding 18](tessera-spec-review.md#finding-18-whether-groupby-may-name-a-reference-field) |

## Asked for and declined

The reason is the part worth reading. A repeat proposal has to break the reason, not restate the
need — the need was never in doubt in any of these.

| Question | Answer | Where |
| --- | --- | --- |
| Group by month, or any time bucket? | **No.** A bucket needs a timezone, a day boundary, DST and a week start. A storage protocol that ships those has decided what a month is. Grouping by an identity is mechanical; grouping by a derived bucket is policy | [Decisions](../specs/tessera-decisions.md) |
| Cross-dataset query, transaction or consistent read? | **No.** No protocol offers it without a distributed commit | [Overview](../specs/tessera-overview.md) |
| Guest `update` and `delete`? | **No.** They break the visibility invariant directly. Copy-back serves the case: the recipient writes in its own dataset, grants read, and the other party copies what it accepts | [What stays out](extension-candidates.md) |
| Consent scoped to a purpose, review states, erasure that propagates into copies? | **No.** These belong to an exchange built on top, not to a store | [Storage and exchange](storage-and-exchange.md) |
| Triggers, computed fields, scheduled work? | **No.** The line that keeps a minimal server implementable | [What stays out](extension-candidates.md) |
| Semantic validation — units, currencies, balance by meaning? | **No.** It ends the property the whole design exists for | [What stays out](extension-candidates.md) |
| Retention that binds the owner against erasure? | **No, and it dissolves on inspection.** The party with the obligation already holds its own copy, outside the other party's authority. What remains is a company able to erase its own books, which is that company breaking its own obligation | [Candidate 9](extension-candidates.md#9-retention-that-binds-the-owner) |
| A row predicate or policy hook in a view? | **Not in v1.** `SetAuthorization` refuses a non-full view together with `write`, and it is right to | [Finding 19](tessera-spec-review.md#finding-19-a-view-cannot-be-a-predicate-so-privacy-is-done-by-placement) |
| Locales and translated schema names? | **Withdrawn from v1.** The route promised translations although a schema write carries one name | [Finding 13](tessera-spec-review.md) |
| Edit or remove a frozen vector case? | **No**, independently of whether a client has shipped. A changed fixture retroactively alters what a passing implementation was certified to do | [Contributing](../CONTRIBUTING.md) |

| Keep reading existing records after a participant withdraws, without seeing later ones? | **Yes.** Freeze a reader share at the dataset sequence; only the writing account may restore it | [Finding 46](tessera-spec-review.md#finding-46-a-reader-share-frozen-at-the-dataset-sequence) |

## Maintaining the index

`internal/speccheck` checks that each numbered review finding and extension candidate is
indexed and that document links resolve. It checks reachability, not whether an answer
remains correct. Update both the answer and its source when the author changes a decision.
