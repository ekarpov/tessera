# The deployment contract

**Status: draft.** Normative for a deployment, not for the wire.

[The protocol](tessera-protocol-v1.md) says what a request means. It deliberately does not say how a
dataset comes to exist, who ends up holding `write` on it, or how a grant is created and revoked — a
token identifies a principal, and everything that principal may reach is account state the protocol
reports but never sets.

That silence is a design choice, not an omission: provisioning and identity differ between a personal
app, a hosted service and an embedded library, and a protocol that fixed one of them would make the
others awkward. But *unspecified* is not the same as *unconstrained*. A deployment still owes its
clients the guarantees below, and a client integrating against one should ask for them in writing.

## What a deployment must provide

**Provisioning.** A way to create a dataset and establish who holds `read`, `write`, `schema` and
`erase` on it. The protocol requires only that the result is reported honestly in `GET /v1/datasets`,
that a dataset a caller may not use is **absent** rather than listed, and that identities are never
reused — including after an erase.

**Principals.** A way to name one. Grants are to principals, so a deployment that cannot name a second
principal cannot share, whatever the protocol permits. A person, an application installation and a
service authenticating with `client_credentials` are all principals.

**Server rights.** Where a deployment offers [dataset creation](tessera-protocol-v1.md#creating-a-dataset),
a way to decide who holds `create_dataset` and to withdraw it. The protocol does not describe how that
decision is made, exactly as it does not for dataset grants; it requires only that the answer appears
in `server_access`, is enforced per request rather than carried in a token, and that withdrawing it
stops new creations **without** stopping recovery of outcomes already claimed.

**Discovery at the origin root.** `/.well-known/tessera` is rooted at the origin
([RFC 8615](https://www.rfc-editor.org/rfc/rfc8615.html)), so a deployment reverse-proxied under a
path prefix — `example.org/tessera/` — is undiscoverable unless it *also* serves the well-known path
at the root of that origin. A client is forbidden from guessing a prefix, which is what makes this
the deployment's obligation rather than a client's problem.

This is a real shape and it fails at the first connection rather than at configuration time, so it
is worth checking before a person types the hostname rather than after.

**A deployment MUST serve over `https` unless it is reachable only on loopback.** A conformant
client refuses `http://` to anything else, so a deployment served in the clear on a private
address is not a deployment with a weaker posture — it is one no conformant client will talk to.
The matching client obligation is in [the protocol](tessera-protocol-v1.md#discovery); this side is
stated so that the pair is visible from whichever document an operator is reading.

A development server on loopback is the exemption and nothing else. The reference self-hosted server
requires a certificate and key and does not start without them, which is the intended shape rather
than a strictness this contract could relax.

**Who administers the server.** A deployment that can confer a server right needs somebody entitled
to confer it, and that entitlement **MUST be held explicitly rather than derived**. Deriving it —
from holding the strongest dataset grant, from administering some dataset, from being the only
account — hides a policy inside an accident and grows silently as the server does. The reference
deployment learned this twice in one week: administration derived from holding `erase` anywhere
made every account that owned a dataset an administrator of everybody else's, and a right derived
from a membership anyone could arrange was a right anyone could arrange.

Until a deployment has such a notion, the honest behaviour is to refuse: an approval carrying a
server right is administrable by nobody rather than by whoever is nearest to it.

**Allocation quota and admission.** Creation allocates storage to whoever may call it, so a deployment
offering it owes itself a quota and a rate limit. The protocol supplies the refusal — `429`
`rate_limited` with `Retry-After` — and fixes what a refusal does to a claimed key; it does not fix
the policy. Two obligations come with offering the route: a refusal leaves no partial dataset, and a
caller holding `create_dataset` is not thereby promised capacity.

**Server-scoped receipt retention.** Whatever is advertised in `server_limits` is owed on every claim
already made. A deployment MAY raise the window; lowering it MUST NOT shorten a promise a claimed key
already carries.

**Grant administration.** A way to create, inspect, expire and revoke grants. The protocol requires
that the effect is immediate for new requests, visible in `access`, `access.changes_at` and
`view_version`, and that
withdrawal invalidates affected cursors with `410` `view_changed` without disclosing what moved.

**Group readers.** A deployment MAY let a dataset be shared with everyone one issuer made — a
household sharing with its members, including members who join later — rather than with named
accounts. If it does, the grant decision stays narrow: only an administrator of the dataset, and only
for the issuer that made their own account. **Issuing that issuer's invitations is then a grant of
read on every such share**, which the issuer's cap and disabling govern, and a deployment offering
this MUST say so to whoever issues them. A group reader holds exactly the reader standing a named
share would give — types, expiry, suspension — an account that writes the dataset is never one, and
a share naming an account takes precedence over the group's.

**Moderated sharing.** An issuer MAY be configured so that the server's administrator can end or
narrow what the accounts it makes share from the datasets it makes them — never read those
datasets, never delete from them. It is the issuer's policy, fixed when the issuer is configured, so
everyone who joins through it joins under it and nobody who joined another way ever is; a deployment
offering it MUST say so to whoever is invited. The account whose share was stopped MUST be able to
learn who stopped which type and when, and MAY share it again.

**Contributions.** A deployment that offers [insert grants](tessera-protocol-v1.md#insert-grants)
lets somebody write into a dataset they cannot see, and owes three things with that:

- **Naming a contributor takes administering the dataset.** It is a write into somebody's data, and
  administering the server is not administering a dataset.
- **Admission and rate are its own.** An insert grant is a write amplifier: nothing in the protocol
  bounds how much a contributor adds beyond the dataset's own limits, so a deployment offering it
  decides what a contributor may add and how fast, and refuses with `429` `rate_limited` or the
  dataset's own bound.
- **An installation adds only where it was approved to.** A contributor's application is approved for
  adding as it is for reading, and gains neither because the account later comes to hold it.

**Linking a personal dataset to a service.** Where a service acts on a person's data, the person must
authorize it explicitly and be able to withdraw that authorization. The protocol has no opinion on the
flow; it does require that withdrawal takes effect without reissuing anything.

**How accounts end.** A deployment that provisions accounts must be able to stop one, reversibly
and permanently, because people leave servers and credentials get lost. The reference server
provides both, and the properties are the contract rather than the mechanism:

- **Suspension stops every credential at once and discards none.** Bearer tokens, refresh tokens,
  new sign-ins, new approvals and administrator sign-in all refuse a suspended account, and
  reinstating restores exactly what it held. A refresh attempted during a suspension MUST NOT be
  spent by the refusal: spent, the attempt after reinstatement reads as reuse, and reuse detection
  revokes the installation for good.
- **A suspended account that proves who it is is told it is suspended**, rather than refused as
  though it were signed out: its password at sign-in, a valid code at the exchange, a valid refresh
  token, or a service's secret answers `account_suspended`. Signing in again cannot help it; its
  administrator can, and an application that cannot tell the two apart sends the person the wrong way.
  **Nothing short of that proof learns it** — a wrong password, an invalid token or a wrong secret is
  refused exactly as for an account that is not suspended.
- **Deletion removes an account and every credential it held, and retires its name for good.** A
  receipt answers to its owning principal, so a later account issued the same name could ask about,
  and read, outcomes the deleted one left in datasets that still exist. A dataset somebody else also
  administers is kept — a suspended administrator counts, because a suspension is reversible.
- **A dataset MAY be bound to its person**, by the template that made it, so that deleting the account
  erases it even where a custodian also administers it; otherwise such a dataset outlives the person
  it was for.
- **A dataset the account alone administers is erased with it, and only when named.** Left behind, it
  would be data nobody could administer or erase. **A deletion MUST NOT erase a dataset its caller did
  not name**: a request that does not name exactly those datasets is refused, listing them, so what is
  erased is always what was seen. The erased dataset keeps no holder.
- **Administering the server is not administering a dataset.** Anything that lets its holder read
  a dataset — becoming its reader, an issuer inviting people into it, a person's password, a
  service's secret — takes administering that dataset, whoever asks.
- **Setting another person's password is data access.** Whoever sets it can sign in as them, so the
  standing to set it is administering every dataset the account holds, whoever asks; the right to
  suspend or delete an account is not enough. The person MUST be able to learn that their password
  was set by somebody else, and to set their own, which ends every sign-in made before it — those
  finished and those still in progress.
- **A person MUST have a way back that nobody else holds** where nobody may set their password — a
  pseudonymous participant has no address to send anything to. The reference server gives each
  person a recovery code, shown to them once and kept only as a digest; using it ends every
  sign-in and replaces it.
- **A person can delete their own account, with their password**, on a page the server renders, so
  the password never passes through a product. Not with a bearer token: an
  installation often holds far less than the person, and one able to read a single dataset must not be
  able to end the account and erase what it administers. **A deletion reports what it kept** — each
  dataset the account held that remains, and whether because somebody else administers it or because
  it was somebody else's — since an empty erasure otherwise reads the same as having held nothing.
  The datasets a deletion would erase are listed only to a caller who has proved the password. A suspended account cannot delete itself — the
  suspension is somebody else's decision to reverse — and the last server administrator cannot, since
  nothing could administer the server afterwards.

**How accounts begin without an operator.** A deployment MAY let people create their own accounts
by invitation. If it does, the grant decision stays with the operator: an operator configures an
*issuer* — a service such as a signup portal — with exactly what its invitees receive, and the person
redeems an invitation choosing their own name and password, which never pass through the issuer.
Compromising an issuer then reaches only what the operator allowed it. Two shapes:

- **Into named datasets**, with fixed grants — a household inviting a member into its shared one.
- **From a template**, where each redemption creates a dataset with a fixed set of types installed in
  the same transaction, administered by a named account, with fixed grants for the invitee — a
  study giving each participant a dataset of their own. **A template issuer MUST carry a cap:** each
  redemption creates a dataset, so an uncapped one is an unbounded right to consume storage. **The
  handle that creates the dataset MUST hold nothing in it afterwards**, since creation otherwise
  widens the creator's reach; the issuer is not a member of what it creates.

**A template MAY name readers** — accounts, services included, that get read on every dataset it
creates, in the step that creates it, optionally scoped to types as a share is. They are named in the
issuer's policy, which only the operator writes; nothing an issuer sends at invitation time adds one.
**A reader gets read and nothing else**: write into every participant's dataset is not a right one
policy line should hand out. A dataset's administrator can change or remove a reader's scope later,
including a service's. A template's types are checked when the issuer is configured, so a mistake in
them is the operator's error at configuration, not every invitee's failed sign-up.

**An issuer MAY assign account names**, so a study's participant cannot type their real one. The
assigned name is reserved from the moment the invitation is issued, shown to the invitee as fixed, and
the one the account gets whatever the redemption sends; a name an invitee chooses cannot take one
reserved for somebody else. A template's datasets MAY be named after the account each is made for,
and a dataset's administrator can rename one.

**An issuer's policy is fixed, and its lifetime is the operator's.** The operator MUST be able to
change its cap, stop it — which also stops its outstanding invitations being redeemed — and replace
its secret without disturbing invitations already handed out, because a portal's secret can leak
and a study can close. What an issuer hands out does not change: datasets it already created would
disagree with the new policy. Expired invitations are not kept.

**An invitation MAY be issued on an administrator's own sign-in** rather than the issuer's secret,
for a deployment whose administration tool is an application: a secret that outlives every sign-in
does not belong on a phone. It then takes the standing configuring that issuer takes — and, for an
issuer into existing datasets, administering each of them — and every other rule above applies
unchanged. **Revoking that sign-in MUST end the invitations it issued and nobody has redeemed**,
because a lost device's pending invitations are exactly what revoking it is for.

Redemption creates only new accounts. Redeeming into an existing name would let whoever holds a code
add datasets to somebody else's account.

**Administration a product can call.** A deployment whose operators and store owners use a product's
own interface rather than its pages needs its administration reachable as an API, and the reference
server offers one at `/admin/api/v1`. Two properties are the contract:

- **It holds no credential of its own.** A request carries the bearer token of an installation the
  person approved, and acts as that person with exactly their standing — the same rules the pages
  apply. Standing is per dataset for what belongs to a dataset, and the server's administrator's
  alone for what belongs to the server: limits, issuers and backups, which spend storage every
  dataset shares. **The server's administrator sees and acts on every account** — suspending or
  deleting one is theirs to do whoever holds its datasets — **but not what those accounts hold**: the
  datasets an account belongs to, and the applications it installed, show only through a dataset the
  viewer administers. A server administrator holding no data can approve an installation that
  administers and reaches no dataset.
- **Administering is its own right, granted on the consent screen.** An installation administers only
  if the person ticked that box. Reusing an existing right would change what a past approval meant:
  a dashboard approved to read, or even to erase, did not consent to adding people or deleting
  accounts.

**Ordinary OAuth consent MAY be reused after fresh authentication**, only within
the most recent live approval for the same account, client and exact registered
callback. A request naming a dataset MUST match it; a request naming none MAY
retain that approval's dataset choice only while it remains available. The new
authorization MUST NOT exceed the requested rights, the prior grant or current
account/view bounds, and MUST NOT combine approvals or revive an older broader
choice. Revocation, withdrawal and grant expiry still apply. New or unavailable
requested rights require consent again. Scope hints and client branding alone
are not consent. A client MUST be able to force a new review. Sensitive choices
are never preselected; reusing a previously explicit grant does not bypass an
operation's required confirmation or fresh authentication.

**A narrowly designated first-party website MAY use account sign-in instead of
individual OAuth consent.** This is an explicit exception to the consent-screen
requirement above, including account administration, solely for that Console:

- The operator MUST provision a dedicated website identity, exact HTTPS origin,
  exact registered redirect URI and explicit authority policy. Public OAuth
  registration, client names, origin parameters and scope requests MUST NOT
  confer or expand this designation. Branding alone MUST NOT confer it.
- The website's effective authority MUST be the intersection of its operator
  policy and the signed-in account's current grants, preserving all dataset,
  type, object, frozen-view and expiry restrictions. Administration MUST NOT
  imply reading another account's private records. Role names and application
  workflows remain opaque to the storage. Study or exchange consent remains
  a separate action; signing in MUST NOT perform it.
- Suspension, credential revocation, grant withdrawal and policy/role changes
  MUST continue to apply. The exception MUST NOT bypass confirmations or fresh
  authentication required by a sensitive operation. Every other OAuth client
  retains individual consent, including its administration choice.
- State, S256 PKCE, CSRF, exact callback binding, single-use codes and expiry
  MUST remain enforced. A persistence choice MUST default off, be bound to the
  validated website transaction and be delivered only through its validated
  code exchange. It MUST NOT increase authority or token lifetimes. Website
  cookie/refresh storage and its maximum persistence remain the website's
  responsibility. Credentials MUST go directly to the authorization server.

**Rate limits MUST NOT be a budget anybody can spend for everybody.** A limit counted per caller
protects a server; a limit counted for the whole server hands anyone who reaches it a way to close
sign-in for every account. The reference server counts per address — per /64 for IPv6, and behind a
proxy it trusts, per forwarded caller — and protects its processors from the password derivation by
bounding how many run at once, so a flood slows sign-in rather than closing it. A person holding
too many installations has their oldest signed out rather than anybody refused. **A self-registered
client that no sign-in has used for an hour MAY be removed** when the registry is full; a client
told `invalid_client` for a `client_id` it registered earlier registers again.

**A client can learn whose token it holds.** The reference server answers `GET /oauth/account` for
any valid bearer token with the account's name, kind, whether it is the server's administrator and
may create datasets, and what this installation was approved for — nothing about datasets or other
accounts. The record protocol never names an account, so this is the deployment's answer, not the
listing's: `server_access.create_dataset` in the listing says whether the deployment accepts creation
over the protocol, not who the caller is.

Readers are managed by account, and **adding one names its types in the same step that grants it
read**, so there is no moment at which a new reader reads the whole dataset. Calling it again
replaces the list; removing a reader revokes every sign-in it held there.

**Frozen reader shares.** The reference deployment supports named readers, template
readers and issuer group readers frozen at the dataset's next minting sequence
point. A frozen reader sees only objects minted before that point, within its
existing type or object scope. This MUST be compared as minted sequence numbers,
never identity strings or wall-clock time. Edits to visible records remain visible,
deletions remove them, and schema remains readable. A deleted identity minted
before the cut answers `404 not_found`; identities minted at or after the cut,
or without a parseable engine minting sequence, answer `404 unavailable`. All reads, counts, aggregates,
distinct references, page sequences, sourced-write scans and attachment resolution
MUST be computed within that view. A record outside it answers `404 unavailable`.

An installation approved for `write` on that dataset may list shares with
`GET /v1/datasets/{dataset}/readers` and its `/{account}` detail route. The
recorded owner may also call `POST /v1/datasets/{dataset}/readers/{account}:freeze`
or `:unfreeze`, with an empty body and its ordinary write-approved bearer token.
No administration scope is needed. Ownership is recorded independently of grants:
template redemption records the invitee, and `POST /v1/datasets` records the
creating account. Ownership follows the dataset's replacement identity on erasure.
Giving another account write, schema or erase grants MUST NOT transfer ownership.

A dataset administrator may freeze through
`POST /admin/api/v1/datasets/{dataset}/readers/{account}:freeze`, with the usual
`administer` approval. Unfreeze requires a write-approved installation of the
recorded owner, on either route. An administrator who is not that owner MUST NOT
restore access even when its installation has write approval. For older or
operator-created datasets without a recorded owner, every account administering
the dataset is refused unfreeze; other write-approved accounts retain freeze and
unfreeze access. This legacy fallback cannot distinguish those writers by ownership.

A group is named `issuer:{issuer}`. Its cut applies to existing and future group
members; changing a member who reads only through the group answers `409 conflict`
with reason `group_reader`. Named and template reader shares carry their own cut.
Replacing a share's types, or removing and recreating that share, MUST NOT remove
its freeze. Erasure ends it. Successful actions answer `204`; repeated freeze is
idempotent and keeps its original cut, while unfreezing an unfrozen share answers
`409 conflict`. No standing answers `403 permission_denied`; no such reader answers
`404 not_found`. Both reader listings report `frozen_at` (RFC 3339 time) and
`frozen_by` (acting account) when frozen; neither field defines the cut.

Freeze and unfreeze MUST change the reader's `view_version` and invalidate all
its page cursors with `410 view_changed`, as any grant change does. Unfreeze
restores full reading within the existing scope, including records minted while
frozen. These account-based routes are deployment-defined; the record protocol's
authorized-view obligations apply to their resulting views.

**Limits a deployment can change.** The bounds a dataset declares are stored on it when it is made. A
deployment MUST be able to change them for a dataset that already exists, because the one that stops
writes — `max_stored_bytes`, or `max_materialized_bytes` for a write that needs the whole dataset,
answered with `507 dataset_full` — is otherwise permanent. A deployment MUST be able to remove a
storage quota as well as raise it. Every
bound may move either way except `idempotency_retention_seconds`, which may only grow: clients were
promised the current window and reconcile against it.

**Backups taken while serving.** A deployment SHOULD be able to back up without stopping. A backup
MUST describe one state of the storage across every table it covers: read table by table under an
isolation that gives each statement its own moment, a backup taken during writes can describe a
state the storage was never in.

**Restoring past an erasure.** The protocol forbids a restore from reviving an erased dataset. On a
storage holding many people's datasets, **a deployment SHOULD meet that by erasing again during the
restore, not by refusing the backup**: refusing makes one person's erasure cost everybody their
backups. It MUST tell people that erased data stays in older backups until they rotate out.

**Joining on a device.** A deployment MAY let an application redeem an invitation directly for an
installation on the device, with no browser and no password. It is deployment-defined, and this
implementation offers it per issuer (`device_redemption`), off by default. It does not relax the
protocol's rule that authorization happens in an external user agent: no authorization request is
made and nothing the person knows passes through the application, because the account has no
password. The inviter authorized the invitation when issuing it. What this implementation offers:

- `GET /oauth/invitation/preview?code=` describes a live invitation before it is used: the
  deployment's `display_name`, an assigned account name, whether it is a rebinding, and the access
  the installation will carry. It answers `404` for an invitation that is unknown, spent, expired,
  or from an issuer that does not allow device redemption.
- `POST /oauth/token` with `grant_type=urn:tessera:params:oauth:grant-type:invitation`, `code`,
  `client_id` (a registered public client) and, where the invitation assigns no name, `account`,
  answers the usual access and refresh tokens plus `account` and `dataset_id`. The code is spent once.
  The installation carries what the issuer gives, never `erase`, and reads what is shared with the
  account. Where the account administers the dataset joining made for it, the installation also
  carries administration, as a browser sign-in approving it would, and the preview says so with
  `administer`. The returned `dataset_id` is the dataset the template created, or the
  single dataset explicitly named by the issuer. Group and other reader grants do not select
  or change it. Refusals are OAuth errors: `invalid_grant` for a code that cannot be used, `invalid_client`
  for an unknown or confidential client, and `account_name_unavailable` for a taken name.
- `POST /oauth/device-codes` with an installation's bearer answers a single-use code, valid for ten
  minutes, that the same grant redeems for another installation of the account with the same
  approval.
- `POST /oauth/account/password-link` with the bearer of an account that has no password answers a
  single-use link, valid for ten minutes, to this server's page where the person chooses one. An
  account with a password is refused: an installation must not be able to replace its person's
  password.
- `POST /oauth/account/delete` with the bearer of an account that has no password deletes it, with
  `erase` naming the datasets to erase exactly as with a password.
- `GET /admin/api/v1/overview` includes `rebindable: bool` on every account. It is true only when
  the requesting administrator's installation may issue a rebinding for that account now: a person
  joined on a device, without a password, not suspended, under an enabled device-redemption issuer
  the caller may issue for. This is advice about current state; issuance validates it again.
  For example, `{"account":"member","kind":"person","rebindable":true}` is an account fragment,
  not the complete overview.
- A device-joined member administers only their own dataset: they may manage its named and issuer-group
  shares, read its usage and see moderation stops. A device installation never carries `erase`, so
  they cannot erase the dataset while staying a member. With an `erase_with_account` template,
  leaving first answers `409 erasure_required` naming their own dataset; repeating account deletion
  with exactly that dataset in `erase` erases it and invalidates every device.
- An issuer may **rebind** an account it made by device redemption, which has no password, to a new
  device: an invitation issued with `rebind` naming the account. Redeeming it revokes every
  installation the account has and records when and by which issuer it happened, which
  `GET /oauth/account` reports as `rebound`. Whoever can issue for that issuer can therefore reach
  such an account. **A deployment offering rebinding MUST tell the person so before they join, and
  that setting a password ends it.** It MUST NOT rebind an account with a password, one another issuer
  made, or one made in a browser. A template rebinding selects the uniquely recorded dataset
  owned by that account and created under that issuer, including its replacement after erasure;
  an existing-dataset issuer retains its explicit target. Missing or ambiguous ownership, or a
  missing or expired grant on the target, refuses redemption with `invalid_grant` before consuming
  the invitation, revoking installations or issuing tokens. This refusal leaves the invitation
  and existing installations intact. It does not promise rollback of unrelated storage failures
  after a valid target has been resolved.
- **Removing a device is complete and immediate.** `GET /oauth/account/installations` lists the
  caller's own installations, marking the current one, and `DELETE
  /oauth/account/installations/{id}` removes one of them. An administrator who may administer the
  account uses `GET` and `DELETE /admin/api/v1/accounts/{account}/installations[/{id}]`. A removed
  installation's access token answers `401` at the next request, its refresh tokens renew nothing,
  and no record of it stays behind. A deployment MUST give both the person and an administrator of
  their account a way to remove a device, because a lost phone holds a credential to the account.
- The host may write a single-use code for an existing person's first device, carrying the server
  rights the account holds or its grants on one dataset less `erase`, and may revoke every
  installation the account has in the same step. Nothing on the network can make one.

## What a deployment cannot quietly redefine

- **Only a holder of `write` writes.** No grant conveys write in v1, so a service cannot be granted
  write on a person's dataset. A hosting service that needs to record something records it in a
  dataset of its own and shares it.
- **Erase acknowledgement means access revoked and deletion durably scheduled.** It is not evidence of
  physical deletion. A deployment that reports cleanup complete to its users needs its own evidence —
  the worker's acknowledgement, not the protocol's `200`. See [Erasure](tessera-protocol-v1.md#erasure).
- **Deletion cascades belong to whoever owns the dataset.** Erasing one dataset never reaches another,
  and a deployment holding a person's dataset alongside a service's workspace MUST keep their
  lifecycles separate. Neither product becomes a required hub for the other.
- **Identities are not reused**, and grants do not transfer to the dataset that replaces an erased one.

## What a client should ask a deployment

1. Who may hold `write` on my dataset, and can that change without my involvement?
2. How do I grant read to another principal, and how do I revoke it? How quickly does revocation take
   effect for requests already in flight?
3. Does a grant expire on its own, and is `access.changes_at` populated when it does?
4. What does your erase actually reach — object storage versions, CDN caches, backups — and how do I
   learn that cleanup finished?
5. If you host my dataset and also run a service, what separates the two lifecycles?

A deployment that cannot answer these is not incompatible with Tessera. It is simply one whose
guarantees stop at the wire, which a client should know before depending on it.
