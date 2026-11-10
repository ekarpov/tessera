# Tessera Protocol, version 1

**Status:** draft for review. Not frozen; see [Versioning](#versioning-and-compatibility).
**Home:** this repository is the protocol's source of truth and carries a reference implementation.
The protocol belongs to no product: it is a general contract for schema-described record storage, and
every application that speaks it is a client on equal terms. For the shape of the thing before the rules, read [the overview](tessera-overview.md);
**this document is the contract**.

A server implementing this protocol is a **Tessera server**: it holds schema-described objects and
answers structured queries about them. It never interprets what they mean.

**MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT** and **MAY** are used in the RFC 2119 sense.
Binary content is out of scope and specified separately; see [Content](#content).

## Contents

1. [Concepts](#concepts) · 2. [Transport](#transport) · 3. [Discovery](#discovery) ·
4. [Creating a dataset](#creating-a-dataset) · 5. [Authentication](#authentication) ·
6. [Capabilities](#capabilities) · 7. [Identifiers](#identifiers) ·
8. [Values](#values) · 9. [Schema](#schema) · 10. [Records](#records) · 11. [Queries](#queries) ·
12. [Operations](#operations) · 13. [Pagination](#pagination) · 14. [The watermark](#the-watermark) ·
15. [Record versions](#record-versions) · 16. [Write safety](#write-safety) ·
17. [Errors](#errors) · 18. [Conformance](#conformance) · [Appendix A: reserved words](#appendix-a-reserved-words)

## Concepts

| Term | Meaning |
| --- | --- |
| **Dataset** | One collection of objects with its own schema and identity. A server MAY host many. |
| **Object** | One record. Has an identity and exactly one concrete type. |
| **Meta type** | A schema type. Types form single-inheritance hierarchies. |
| **Effective metadata** | A type's own fields plus every inherited field, in a stable order. |
| **Record set** | A projection: one metadata description plus rows whose values match it positionally. |
| **Reference** | A value naming another object, qualified by the dataset that owns it. |

## Transport

HTTPS. HTTP/1.1 or HTTP/2. Request and response bodies are `application/json; charset=utf-8`.

- Paths begin `/v1/`. A breaking change takes a new major prefix, served alongside the old one.
- Requests are stateless. A server MUST NOT require an earlier request to interpret a later one.
- Schema names are returned as stored. `Accept-Language` MUST NOT change metadata names or schema
  versions; changing a display language must not invalidate an `expect_type` guard.
- Servers MUST enforce a maximum request body size and advertise it as `max_request_bytes`.
- `429` responses MUST carry `Retry-After`.

Operations that are not naturally resources use a colon-suffixed verb (`…/objects/{type}:query`).
Query bodies are structured and MUST NOT be encoded into the URL.

### Versioning and compatibility

The specification and its conformance suite are the definition of correctness; no implementation's
behaviour is. Within version 1:

- Servers MAY add response fields. Clients MUST ignore unknown fields.
- Clients MUST NOT send fields not defined here; servers MUST reject unknown request fields with
  `invalid_field` rather than ignoring them, so a typo is never silently dropped. Because of this
  rule, a request field this specification does not define cannot be introduced by convention — which
  is why every request and response shape here is given explicitly.
- New capabilities MAY be added. Their absence MUST remain the safe reading.

**Selecting a version.** `protocol_versions` is a nonempty array of distinct major-version
strings. Each is a canonical positive decimal integer (`[1-9][0-9]*`), without a size
bound implied by a machine integer. Its order has no meaning. A client MUST select the
numerically highest version present both there and in its own supported record-protocol
versions. For example, `["2", "10", "1"]` and client support `["1", "2", "10"]` select `"10"`.
If there is no common version, the client MUST refuse the connection before authentication
or data requests, explain the incompatibility, and MUST NOT guess a path or fall back to
an unadvertised version. Malformed or duplicate version identifiers make discovery invalid.
Implementation release versions and independently versioned content protocols do not
participate in this selection. Deprecation does not remove a still-served version from
selection; a client SHOULD warn the person when its selected version is deprecated.

**Serving and retiring a major.** A server MUST keep serving version 1 unchanged at `/v1/`
for as long as released client versions using that deployment require it. A successor
major MUST be served alongside the old major. The minimum overlap is 12 calendar months
from the successor protocol major's first production release, not from an implementation
release, release candidate, announcement or a particular deployment's upgrade. Calendar
months use the same UTC date and time one year later, clamping 29 February to 28 February
when necessary. Retirement MUST satisfy both the minimum overlap and the continued-use
rule; passage of 12 months alone is insufficient. Operators MUST retain dated evidence
of successor availability and consumer confirmation that released clients no longer
require the retiring version. A server MUST give affected consumers at least 14 days'
advance notice of retirement and publish the discovery signal below during that notice.
An ordinary upgrade notice is not evidence that the version may be retired.

**Deprecation signal.** Discovery MAY contain `deprecated_versions`, an array of objects:

```json
{ "deprecated_versions": [{ "version": "1", "sunset": "2030-01-01T00:00:00Z" }] }
```

Each object MUST contain nonempty string `version`, naming a version still present in
`protocol_versions`, and string `sunset`, the planned earliest retirement instant.
`sunset` MUST be a valid UTC timestamp in exactly `YYYY-MM-DDTHH:MM:SSZ` form, without
fractional seconds or an offset; seconds range from 00 to 59. Entries MUST NOT repeat a
version. Unknown response fields, including fields inside these objects, remain additive.
The array MUST NOT be `null`; omission or an empty array means no announced deprecation,
not a promise of perpetual support. Clients MUST NOT infer retirement from omission.

The announcement is a plan, not permission to stop service. Its date MUST respect the
minimum overlap and continued-use requirements; if a successor ships later than planned
or released clients still require the old major, the operator MUST postpone retirement
and update the announcement. A date in the past does not remove the version from service
or selection. A client MUST NOT treat a sunset as a data deletion or stop using an
otherwise supported version solely because its sunset has passed. This discovery signal
is sufficient; no `Sunset` HTTP header is required by this protocol. It does not change
the independently versioned content discovery contract.

## Discovery

```
GET /.well-known/tessera
```

Unauthenticated. Contains no user data.

**Getting here from what a person typed.** These three steps happen before any route in this
protocol is reached, and they are written down because two clients meeting the same server must
take them the same way. They are client obligations; nothing a server does can hold a client to
them.

- **A bare authority resolves to `https://`, and a client MUST NOT try `http://` first or fall back
  to it.** A person who typed a hostname cannot see which scheme was used, so a client that probes
  `http://` on a typo, a captive portal or a name that does not resolve has performed a downgrade
  nobody sanctioned and nobody can observe. If `https://` fails, the attempt fails.
- **Discovery is at the origin root**, per [RFC 8615](https://www.rfc-editor.org/rfc/rfc8615.html).
  A client MUST NOT look for it under a path prefix, and a deployment served under one MUST also
  serve `/.well-known/tessera` at the root of its origin or it is undiscoverable — see
  [the deployment contract](tessera-deployment.md).
- **A client MUST NOT fetch `http://` for anything this protocol names, except on a loopback
  host.** The rule above governs a downgrade a client performs on its own; this one governs the
  downgrade it is handed. They are different requirements and a client satisfying the first does not
  thereby satisfy the second: not falling back to `http://` says nothing about whether an `http://`
  URL from a person, a configuration file or a saved account is used as given. Records, tokens and
  credentials travel that connection, and the person whose records they are cannot see the scheme.

  **A private or link-local address is not loopback.** A server reachable at `http://192.168.1.10`
  or `http://storage.local` is exactly what this refuses: every other machine on that network is a
  third party, and *inside the house* is not a security boundary. The exemption is for a server
  reachable only by the process talking to it, which is what loopback means and what a development
  server is — this implementation's own runs plaintext on loopback with synthetic data, and its
  self-hosted server requires TLS and does not start without it. So this forbids no deployment this
  protocol supports; it forbids a client from being the thing that makes one possible.
- **`authorization_servers` is in the server's order of preference, and a client takes the first it
  can use.** A client MUST try them in order and MUST NOT ask a person to choose: nobody typing a
  hostname knows what an authorization server is, and a wrong choice strands them. A server SHOULD
  list one, and lists several only while migrating from one issuer to another — which is the case
  where order carries the answer, the new issuer first and the old one behind it.

**The rule covers every URL this protocol hands a client, not only the storage origin.** In
particular it covers each entry of `authorization_servers`, and that is the case worth stating
separately: **there the destination is chosen by the far end rather than by the person.** A server
naming `http://192.168.1.10` sends a client somewhere plaintext by its own say-so, and a client that
checks the scheme only when validating metadata it has already fetched has performed the fetch it
meant to prevent. **Check before the request, not on the response.** An entry a client must refuse is
an entry it cannot use, so the first-usable rule above passes over it; a server offering only such
entries is unusable, which is the correct outcome and not a reason to relax this.

**Compare the scheme and the host case-insensitively.** They are case-insensitive in
[RFC 3986](https://www.rfc-editor.org/rfc/rfc3986.html#section-3.1) and implementations do not
normalise them for you: `HTTP://192.168.1.10` is a plaintext URL whose scheme equals neither `"http"`
nor `"https"` under an exact comparison, so a guard written as *refuse `http`* admits it, and
`http://LocalHost` is a valid loopback address that a guard written as *permit `localhost`* refuses.
The first is a hole and the second is an outage; both come from the same omission.

**These are the destinations, and the list is the point.** This rule has been widened twice, each
time by somebody implementing it and meeting a URL the previous wording did not name. So the
destinations are enumerated rather than described, and a future one is a missing row rather than a
case nobody thought of:

| What hands it to the client | Chosen by | Where |
| --- | --- | --- |
| The storage origin | The person | Typed, configured, or restored from an account |
| `authorization_servers` | **The server** | [Discovery](#discovery), above |
| The resolved read URI for an attachment | **The server** | [Content protocol](tessera-content-v1.md) |
| The transfer URI for an upload | **The server** | [Content protocol](tessera-content-v1.md) |

**The rows a server chooses are the ones that need the rule**, because the person cannot see them
and did not pick them. The content protocol states the rule for its own two rows rather than
referring here: it versions independently, and a reader of one document is not helped by the other
being correct.

**Loopback is a property of the address, not of its spelling.** The exemption covers `127.0.0.0/8`,
the IPv6 address `::1` however written — including `0:0:0:0:0:0:0:1` and the IPv4-mapped
`::ffff:127.0.0.1` — and the name `localhost`, which
[RFC 6761](https://www.rfc-editor.org/rfc/rfc6761.html#section-6.3) reserves for exactly this. A
client that compares literal strings and so refuses an unusual spelling of a loopback address is
**conformant**: the asymmetry is deliberate, because refusing a loopback address costs a developer a
confusing error and accepting a non-loopback one costs somebody their records. A client that accepts
anything outside that set is not conformant, whatever it was spelled as.

```json
{
  "protocol_versions": ["1"],
  "server": { "name": "tesserad", "version": "0.1.0" },
  "display_name": "Example Home",
  "auth_methods": ["oauth2", "bearer_token", "client_credentials"],
  "authorization_servers": ["https://tessera.example.org"],
  "server_capabilities": ["dataset_creation", "shared_reads"],
  "server_limits": { "idempotency_retention_seconds": 604800,
                     "max_creation_request_bytes": 262144, "max_creation_types": 128 }
}
```

**`display_name` is an optional, operator-set name for the deployment.** When present it MUST
be a non-empty string, never `null`; when not configured it MUST be absent. No API sets it.
Display name is for humans and MAY change at any time. Clients MUST NOT treat it as identity.
Discovery is unauthenticated, so a client MUST show it as the server's own claim, next to the host
name, and MUST NOT show it instead of the host name. It is separate from `server`, which names
the server software.

**`server_capabilities` describes the server, not a dataset**, and is absent or empty on a server that
offers nothing at this level. The members defined in v1 are `dataset_creation` and `shared_reads`.
`dataset_creation` says that [`POST /v1/datasets`](#creating-a-dataset) exists here. A client MUST
NOT read it as permission: what this caller may do is `server_access` on the authenticated listing
below, and a server that advertises the capability still refuses a caller who holds no right with
`403` `permission_denied`. `shared_reads` says that an authorization request here may ask to
[read everything shared with the account](#reading-everything-shared) instead of naming a dataset.

**`server_limits` are the bounds of work that belongs to no dataset.** `idempotency_retention_seconds`
is how long this server retains the outcome of a server-scoped operation, measured from when its key
was claimed, and is a **floor**: a server MUST retain for at least this long and MAY retain longer. It
is a different window from the `idempotency_retention_seconds` in a dataset's `limits`, which governs
that dataset's own receipts, and a server MUST NOT conflate them. A caller has to be able to read this
before it has any dataset, which is why it is here and unauthenticated.

**A server that stops offering creation MUST keep advertising `server_limits`** for as long as it can
still answer about outcomes already claimed. Withdrawing the capability says *you cannot create*;
withdrawing the window would say *you cannot reconcile*, and those are different statements. A client
that cannot tell them apart has no safe action after a lost response.
```
GET /v1/datasets
```

Authenticated. Lists the datasets this caller may use. A dataset the caller may not use MUST be
absent — never listed as empty or inaccessible.

**This is the cheap poll.** Each entry carries its current `watermark` and `schema_version`, so one
request tells a client which of its datasets moved without reading any records. A client MUST NOT poll
by issuing a query and discarding the rows.

**Three versions answer three different questions**, and none substitutes for another:

| | Changes when |
| --- | --- |
| `watermark` | Anything in the dataset is committed — records or schema. |
| `schema_version` | The dataset's schema changes. A subset of the above, for a client that caches metadata. |
| `view_version` | **What this caller may see changes.** |

`view_version` is per caller, opaque, and compared for equality like the others. It MUST change when a
grant to this caller is created, narrowed, widened or revoked, and when one expires.

**It exists because the alternative is a silent hole.** A caller granted access to a second type
already had `read: true`; the data did not change, the schema did not change, and the expiry did not
change — so without `view_version` nothing in a cheap poll moves and the caller never learns its view
widened. A server that does not implement `sharing` has one view per dataset and MAY return a constant.

**`access.changes_at` is the next scheduled narrowing**, not the end of all access. With several
grants expiring at different times, the useful value is the soonest moment the view could shrink; a
client re-reads then, and finds whatever remains. `null` means nothing is scheduled to expire. When a
client needs to tell a person when their access ends altogether, that is grant administration and
belongs to the deployment — see [the deployment contract](tessera-deployment.md).

A view change also surfaces in a refusal, and in `410` `view_changed` on a cursor. **The watermark
describes the data, not who may see it**, and a client that treats it as a guarantee about its own
visibility will miss the moment it gained access to something.

```json
{
  "server_access": { "create_dataset": true },
  "datasets": [
    {
      "id": "s-7f3a9c21b04e4d1789ab",
      "display_name": "Team Alpha",
      "capabilities": [],
      "access": { "read": true, "write": true, "schema": false, "erase": false,
                  "insert": false, "changes_at": null },
      "watermark": "w:8f21…",
      "schema_version": "s:41c0…",
      "view_version": "v:2d9b…",
      "limits": {
        "max_materialized_bytes": 67108864,
        "max_allowed_concrete_types": 128,
        "max_selection_per_concrete_type": 100,
        "max_sort_terms": 16,
        "max_projected_fields": 256,
        "max_overall_limit": 10000,
        "max_page_size": 1000,
        "max_distinct_references": 256,
        "max_aggregate_groups": 32,
        "max_transaction_operations": 256,
        "max_predicate_depth": 32,
        "max_predicate_width": 256,
        "max_fixed_digits": 64,
        "max_text_bytes": 1048576,
        "max_inline_blob_bytes": 262144,
        "max_request_bytes": 4194304,
        "max_scanned_rows": 200000,
        "max_roles": 16,
        "max_role_length": 64,
        "idempotency_retention_seconds": 86400
      },
      "content_store_id": "c-91ab0e77"
    }
  ]
}
```

An entry MAY also carry `"replaced_dataset_id"`, naming the identity this dataset replaced after an
[erasure](#erasure).

**Identity.** `id` is assigned by the server and belongs to the dataset, not to the connection. A
client MUST register the dataset under exactly this value and MUST refuse the connection if it
collides with a different dataset it already holds. Servers MUST mint identities with at least 64 bits
of entropy and MUST NOT reuse one, including after a dataset is erased or deleted.

**`server_access` is this caller's authority over the server itself**, as distinct from its access to
any dataset. It is an object of named rights, each a boolean, so a later right can be added without
changing the shape. `create_dataset` is the only right defined in v1.

- **A missing or unrecognized right confers nothing.** A client MUST treat an absent member as
  `false`, and MUST NOT infer permission from a name it does not know.
- **A server names every right it defines, whether or not the answer is yes**, and a server that
  offers creation to nobody still reports `create_dataset: false` rather than omitting it. An
  omitted member would mean both *not offered here* and *not yours* on the axis that exists to
  answer the second, and `server_capabilities` already answers the first.

  This is what lets a client read an empty `datasets` correctly, which is the case it meets before
  it has anything: with `create_dataset: true` an empty listing means *make one*, and with `false`
  it means *nothing here is yours yet*. Those want different screens, and a client that cannot tell
  them apart shows the wrong one at the first connection — where a person is most likely to stop.
- **It reports authority, never capacity.** `create_dataset: true` says this caller may ask. It does
  not promise that quota, rate limiting or any other admission control will accept the request; see
  [Creating a dataset](#creating-a-dataset).
- **It follows current server-side state**, exactly as dataset `access` does. It is never carried in a
  token, and a server MUST evaluate it per request rather than trusting what discovery said earlier.
- `view_version` describes what this caller may **see**, which is datasets and records. A change to
  `server_access` need not move it.

**A server right is held twice, and the narrower answer wins.** The *account* holds it — that is
the state a deployment confers and withdraws. An *approval* carries it, or does not, exactly as an
approval carries dataset grants: an installation that was never approved for creation does not get
it because its account has it, and one that was approved for it loses it the moment the account
does. `server_access` reports the conjunction, which is what this caller may actually do right now.

This is the same rule dataset grants follow — an installation never exceeds what the account holds
— and it is why an approval carrying a server right and no dataset is separately revocable rather
than a property of the person. Withdrawing the installation stops that application creating
anything; withdrawing the account's right stops all of them.

**The class is open and its absence is the safe reading.** A later version may define further
rights, and a server MAY define its own. A client MUST treat every name it does not know as
`false`, and MUST NOT conclude from a name's absence that the server is old rather than that the
right is not the caller's. Nothing in this protocol requires a right to exist; `create_dataset` is
an instance of the class, not the class itself.

How a deployment decides who holds a server right is outside this protocol, as grant administration
already is — see [the deployment contract](tessera-deployment.md). What the protocol requires is that
the answer be reported honestly here and enforced consistently.

**Administering a server is not one of these.** A right that governs who may confer rights is not a
right over the data, is answered by no route here, and MUST NOT appear in `server_access`. A
deployment that has such a notion keeps it to itself; a client has no business reading it and this
protocol gives it no way to.

**Display name** is for humans and MAY change at any time. Clients MUST NOT treat it as identity.

**Limits** are the server's and are authoritative. A client uses the lesser of these and its own.

## Creating a dataset

Optional, and advertised as `dataset_creation` in `server_capabilities`. A server that does not offer
it refuses with `403` `not_supported`, and is no less conformant for that: every other route in this
protocol acts on a dataset that already exists, and how one comes to exist may reasonably be a
deployment's own business.

```
POST /v1/datasets
Idempotency-Key: 9f2c1b7e-0f3a-4c8e-9a10-2b6d4e8f0a11

{ "display_name": "Team Alpha" }
```

```
201 Created

{ "dataset_id": "s-7f3a9c21b04e4d1789ab" }
```

**A creation may install types.** An optional `types` member carries type declarations in the form
`define_type` accepts, in order, so a parent precedes its children:

```
POST /v1/datasets
Idempotency-Key: 4a1d7e02-3b9c-4f6e-8d21-7c5b9a0e3f14

{ "display_name": "Participant",
  "types": [ { "id": "study.measurement", "name": "Measurement",
               "fields": [ { "id": "n", "meta_id": "study.measurement", "type": "integer" } ] } ] }
```

The dataset and its types MUST commit together or not at all, so no caller can ever observe a
dataset created this way without them. Each type is subject to every rule `define_type` applies,
and **an invalid one MUST fail the whole request before its key is claimed** — a malformed request
spends no key, which is the rule for every other operation's grammar. A caller that corrects its
schema may resend under the same key. At most `max_creation_types` types, in a request of at most
`max_creation_request_bytes`, both advertised in `server_limits` because this route precedes any
dataset whose `limits` could govern it.

**The key is required.** A request without `Idempotency-Key` MUST fail `400` `invalid_request`, and
MUST fail before anything is created. Creating an identity with no way to recover a lost response is
not an available mode: the response carries the only record of which dataset is now the caller's, and
a caller that loses it and retries without a key creates a second one. This requirement is specific to
this route; it does not extend to mutations generally.

**The response is an identity and nothing else.** It MUST NOT carry capabilities, access, limits,
versions or a display name. Those describe the dataset as the server currently sees it, they are
answered by `GET /v1/datasets`, and a creation that also asserted them would be a second place for the
same answer to live and drift from the first. A caller therefore **reads the listing next** to learn
what it may do with what it made; that read is a required step of the flow, not an optimization a
client may skip.

**The created dataset MUST appear in that caller's listing immediately** — before any subsequent
request by the same caller can observe its absence. A client sequences on this, and a listing that
lagged a creation would be indistinguishable from a server that created nothing.

**The caller that asks becomes the dataset's administrator**, holding `read`, `write`, `schema` and
`erase` on it, and no others are provisioned. This is the policy [erasure](#erasure) already applies
when it mints a replacement. It conveys no authority over the server and none over any other dataset.

**So the authorization that created it now reaches two datasets**, and its listing says so. That is
the one way an authorization comes to be over more than one, and the obligations it carries — what a
withdrawal must reach, and what an approval screen must say — are with the
[authentication rules](#authentication).

**The request carries a display name or nothing.** It MUST NOT propose an identity, bounds, grants or
a content store. Identities are the server's by [the identity rule](#discovery); limits are the
server's and authoritative, and a caller discovers and respects them rather than negotiating them.

### Replaying a creation

A repeat under the same key follows [Idempotency](#idempotency) with no exception, which means the
answer depends on what the retained outcome *is*:

| The retained outcome | A repeat with a matching digest |
| --- | --- |
| `committed` | The same `201` and the same body, with `Idempotency-Replayed: true` |
| `failed` | That failure, with its original status and error |
| `in_progress` | `409` `conflict` with `details.state: "in_progress"` |

A different digest fails `409` `conflict` with `details.reason: "idempotency_key_reused"` whatever the
outcome is. Only a committed creation replays as a creation; a failed one replays as the failure it
was, which is what makes a fresh attempt take a fresh key.

**Recovery precedes admission.** A server MUST resolve a repeat against the retained outcome *before*
it applies capability, permission, quota or any other gate on new work. Concretely: authenticate the
caller, look up the claim, check ownership and digest, and answer from what is retained. Only where no
claim exists do `server_access`, `server_capabilities` and admission decide whether work may begin.

It follows that a caller recovers its own creation after its `create_dataset` right is withdrawn, and
after a deployment stops offering creation at all. A new creation is refused in both cases, and the
receipt is not. **Revocation stops new work; it does not stop reconciling work already done.** A
server that refused a replay in either case would put a caller in the position the key exists to
prevent — holding an outcome it cannot learn.

### Asking about a creation

A creation belongs to no dataset, so its outcome is not addressed under one:

```
POST /v1/operations/{key}:status
```

Answers exactly as [`:status` under a dataset](#recovering-any-mutation) does, with the same ownership
rule, and returns the identity as its result:

```json
{ "state": "committed", "result": { "dataset_id": "s-7f3a9c21b04e4d1789ab" } }
```

- The receipt's **owner** may read it with no current right to create and no grant on what it made.
- Any other authorization MUST be refused `403` `permission_denied`.
- An unclaimed key MUST answer `"unknown"` to any authenticated caller.
- A server MUST keep answering here while it offers recovery, including after it stops offering
  creation, for at least `server_limits.idempotency_retention_seconds` from the claim.

### Admission, and what a refusal leaves behind

A deployment decides its own allocation quota and rate limiting; this protocol does not describe them
beyond `429` `rate_limited` with `Retry-After`. **What a refusal does to a key is not the deployment's
to choose**, because a client cannot interpret an outcome that varies by implementation:

| Where the refusal happens | The receipt | What the same key does next |
| --- | --- | --- |
| Before the claim — authentication, parsing, an early admission check | None is written | May be used again. `"unknown"` therefore does not prove nothing was attempted |
| After the claim, with definite confirmation that nothing committed | Retained `failed`, carrying the original error | Replays that failure. A further attempt MUST take a new key |
| After the claim, outcome uncertain | Remains `in_progress` | `409` `conflict` with `details.state: "in_progress"` |

A server MUST NOT resolve an uncertain outcome into `failed`, and MUST NOT release a claimed key.
`Retry-After` says when another attempt may be admitted; it says nothing about what happened to the
last one and does not reset a retained failure.

**A refusal MUST leave no partial dataset.** Either a dataset exists and its creation is retained as
committed, or neither is true.

## Authentication

Credentials are always presented the same way — `Authorization: Bearer <token>` — and differ only in
how the token was obtained. A server advertises what it supports in `auth_methods`.

### `oauth2` — how an application signs a person in

**Required of any server an ordinary person will use.** Asking someone to copy a secret string from an
administration page into an application is a poor first experience and teaches a habit worth
discouraging; a sign-in screen is what people already understand.

The server is its own authorization server, so a self-hoster runs one thing, and publishes
[RFC 8414](https://www.rfc-editor.org/rfc/rfc8414.html) metadata at
`/.well-known/oauth-authorization-server`. Discovery lists it under `authorization_servers`.

- **Authorization Code with PKCE (S256)**, per OAuth 2.1. The implicit and
  resource-owner-password grants MUST NOT be offered.
- **Public clients, registered dynamically.** A client cannot be pre-registered with a server nobody
  has deployed yet, and a self-hoster must not have to configure one. Servers SHOULD implement
  [RFC 7591](https://www.rfc-editor.org/rfc/rfc7591.html) dynamic registration, **open by default**,
  with rate limits. A server MAY require an authenticated administrator instead, as operator policy —
  but requiring it universally would defeat the onboarding this method exists for.
- **Registration is not trust.** Registering confers no access to any data whatsoever; only a completed
  authorization does. A client's self-asserted name and logo are unverified and a server MUST NOT
  present them as though they were.
- **Redirects.** Native clients SHOULD use claimed HTTPS callbacks. A private-use scheme is a permitted
  fallback, and PKCE protects an intercepted code — but it does **not** authenticate a malicious
  application that starts its own flow, which is why the HTTPS form is preferred rather than merely
  suggested. Servers MUST match registered redirect URIs exactly, with the loopback-port exception of
  [RFC 8252](https://www.rfc-editor.org/rfc/rfc8252.html) where it applies. Authorization MUST happen in
  an external user agent, and the client MUST bind each response to the authorization server and request
  that produced it ([RFC 9700 §2.1](https://www.rfc-editor.org/rfc/rfc9700.html#section-2.1)).
- **A client's authorization is not the account's privileges.** Authorizing an application MUST NOT
  silently hand it everything the person can do, `erase` included. Effective access is the
  **intersection** of the account's dataset grants and what that application was authorized for, both
  held server-side. `access` in discovery reports that intersection, not the account.
- **The approval names its dataset, and the request need not.** An authorization request MAY carry a
  `dataset`, and a server that receives one shows exactly that dataset on the approval screen. A
  request that carries none is **not** an error: the person chooses, at approval, from the datasets
  their account may reach.

  A client cannot be required to name one, because before a token exists it has no way to learn an
  identity — the listing is authenticated, and discovery carries no identities by design. On a first
  connection the dataset was minted for the person and nobody has ever shown it to them.

  **Signing in and approving are therefore two steps**, and a server MUST NOT combine them. The
  account is unknown while the first screen is rendered, so the datasets it may reach cannot be
  offered there; and a person who supplies a password and approves access in one action never
  reviews what they are approving as an authenticated user. A server MUST bind each step to its own
  single-use token, so one that survived the first is not spendable at the second.

  **Where the person chooses, the choice MUST be one their account holds.** A submission naming any
  other dataset is refused, whatever the form said, because the screen is the only thing the person
  saw and a crafted submission is not consent either.
- **An authorization is approved for at most one dataset**, or — where the server offers it — for
  [reading everything shared with the account](#reading-everything-shared), and reaches that and any
  dataset created through it. `GET /v1/datasets` under a token minted from it returns exactly those — or
  nothing, where there are none. A person may hold grants on many; an *installation* is approved
  for one, so connecting a second storage is a second authorization, revocable on its own. A client
  therefore learns which datasets it reaches by listing after it signs in, and MUST NOT expect any
  dataset identity in the token response.

  **At most one approved, not exactly one**, because an authorization may carry server-level rights
  and no dataset grant at all — the case a caller needs when it holds nothing yet and is about to
  create something. Its listing is empty until it creates, and that is the correct answer, not a
  missing one.

  **Two things widen the reach, and nothing else.** [Creating a dataset](#creating-a-dataset)
  makes the caller its administrator, so an authorization carrying `create_dataset` comes to be over
  more than the dataset it was approved for. And an authorization approved to
  [read everything shared](#reading-everything-shared) reaches each dataset shared with its account
  as the share is made. Otherwise a grant installed by an administrator is installed on an
  authorization of its own.

  Two obligations follow, and a server MUST meet both.

  **Withdrawing or revoking an authorization removes its access to everything it reaches**, not only
  to the dataset its approval named. A withdrawal that missed the rest would leave a stopped
  installation holding everything it had ever created, which is the opposite of what was asked for.

  **An approval offering `create_dataset` MUST say that the installation will also reach what it
  creates.** The right is server-level and the reach it confers is not, so a screen that offered it
  as "create datasets" and nothing more would be collecting consent for one thing and delivering
  two. An approval that does not say what it is for is not consent.
- **Short access tokens, rotating refresh tokens.** The access token's lifetime is minutes. Refresh
  rotation makes a stolen refresh token detectable when both copies are used.
- **Revocation** per [RFC 7009](https://www.rfc-editor.org/rfc/rfc7009.html). Note that revoking a
  refresh token does not by itself invalidate access tokens already issued from it: a server whose
  sign-out claims to end access immediately MUST invalidate the whole authorization's access tokens
  too, and one that does not MUST NOT describe sign-out as immediate.
- **Separately revocable authorizations.** Each authorization — one person, one application
  installation — is revocable on its own, so a lost device can be cut off without disturbing another.
  A registration per installation is one way to arrange that and is not required; revocable refresh
  token families achieve the same thing.

#### Reading everything shared

A person who reads what several others share would otherwise need one approval per sharer, and one
more each time somebody new starts sharing. A server advertising `shared_reads` accepts an
authorization request carrying **`shared_reads=true`**, alone or beside a `dataset`; any other value
is `invalid_request`. The approval screen then offers, **unticked**, to read what is shared, beside
whatever else the request asked for; the person may approve it alone, with a dataset of their own,
or not at all. Approved, it covers one thing more than the rest of the approval:

- **Reading, and nothing else**, on **every dataset where the account holds a share** — read, and
  neither `write`, `schema` nor `erase` there. Never a dataset the account writes or administers:
  its own data is approved one dataset at a time, as ever.
- **Within each share.** Its types, its objects and its expiry apply exactly as they do to any
  installation of the same account on that dataset.
- **Including shares made later, without asking again.** The reach grows when somebody shares with
  the account and shrinks when a share ends, which is what makes this worth having and what the
  approval screen **MUST** say in so many words: every other approval covers what existed when it was
  given, and one that grows afterwards without saying so is not consent.

`GET /v1/datasets` under such a token lists the datasets shared with the account now, each with
`access` reporting read alone, beside any dataset the rest of the approval covers — which keeps
exactly the access it was approved for, and which no share, and no share ending, changes. A share ending MUST take effect for new requests at once. A share being
made MAY take effect from the next access token the installation is issued, and a client that has just
been told of a new share refreshes before looking for it. Revoking the installation ends its access to
all of them.

**What it costs a person, stated so a client can say it too.** A token stolen from such an
installation reads every share rather than one, and writes the person's own dataset too where the
approval covered one; stopping the shared part while keeping the dataset takes a new sign-in without
it; the short access token and refresh rotation bound
that as they bound every token. And an approval that grows is an approval whose reach the person no
longer sees at a glance: a client offering this SHOULD show which datasets the installation reads,
and how to stop it.

**`401` means refresh, not failure.** An expired access token is an ordinary event: the client refreshes
and retries once. Only a refusal to refresh — `invalid_grant` — means the person must sign in again.
A refusal with **`account_suspended`**, which a deployment that suspends accounts MAY answer to a
caller that proved it is the account, means the opposite: signing in again will not help, and the
person needs whoever administers their account. A client MUST NOT send them to sign in on it.
This distinction matters beyond tidiness: a client that treats every `401` as a dead connection reports
a routine token expiry to the user as *the server is unreachable*.

### `bearer_token` — long-lived tokens for automation

A token issued from an administration interface, for scripts, continuous integration and the
conformance suite, which have no browser and no person to sign in. Servers MAY offer it; clients acting
for a person SHOULD NOT use it.

### `client_credentials` — one service acting as itself

A service calling Tessera on its own behalf rather than for a person: an application server, a
scheduled job, another product integrating against a dataset. It authenticates with its own
credentials and acts as its own principal, which is what makes its grants separable from anyone's.

- Servers MAY offer it, and MUST NOT let it stand in for a person. A token obtained this way
  identifies the service, so a grant to it is a grant to the service, and revoking it affects nothing
  else.
- Everything in [Common rules](#common-rules) applies unchanged, including that grants are not scopes.

### Common rules

- A missing, malformed or expired token MUST produce `401` / `authentication_failed`.
- A valid token lacking the authority for an operation MUST produce `403` / `permission_denied`.
  This is distinct from `403` / `not_supported`, which means the server does not implement an optional
  capability. *You may not do this* and *this server cannot do this* are different answers and a client
  says different things about them. Discovery's `access` cannot stand in for the error, because grants
  may change between one request and the next.
- A server MUST NOT use `404` **`not_found`** to conceal an object the caller may not read. The
  concealing answer is `404` `unavailable`, which is defined to reveal nothing — see
  [Absence, refusal and unavailability](#absence-refusal-and-what-a-caller-cannot-see).
- **Grants are not scopes.** A token identifies a person or an installation; which datasets they may
  read or write is server-side account state, reported per dataset as `access`. A token therefore never
  carries a dataset list, and revoking a grant takes effect without reissuing anything.
- Clients MUST NOT persist any token in configuration that is exported, backed up or synchronised, and
  MUST store refresh tokens in the platform's protected credential storage.

## Capabilities

A capability is a promise about the protocol, not a report on the storage engine behind it. A server
whose store cannot filter or sort performs that work itself and still advertises the behaviour
truthfully, because a capability describes what a caller may rely on, not how it is achieved.

**What is required is decided by one question: can a client do it itself?** If it can, the operation is
optional and a server that omits it is still conformant. If it cannot — atomicity, durable
idempotency, a truthful answer about absence — the operation is required, because no amount of client
effort reconstructs it.

### Required of every conformant server

Not advertised, because they are not optional.

| | |
| --- | --- |
| **Discovery** | `GET /.well-known/tessera` and `GET /v1/datasets`. |
| **Metadata** | Effective metadata for every type the server owns, in a stable order. |
| **Reads** | An object by identity; structured queries with every predicate kind, sorting, projection and pagination. |
| **Writes** | Transactions: an ordered list of operations that all commit or none do, with preconditions evaluated **inside** the transaction. |
| **Immutable fields** | A field declared [`immutable`](#immutable-fields) keeps the value it was first given. |
| **Recovery** | Durable idempotency — a key bound to the outcome it produced. |
| **Change detection** | The dataset watermark on every read. |
| **Erasure** | `:erase`, with deletion durably scheduled before the response. |
| **Honest answers** | The distinction between `not_found`, `permission_denied` and `unavailable`. |

A server that cannot execute one of these efficiently MUST still implement it, declare a bound such
as `max_scanned_rows`, and fail with `query_result_limit_exceeded` when a request exceeds it. Failing
explicitly at a stated limit is conformant; omitting the operation is not.

**`max_materialized_bytes` is the one bound that is not about a request.** It is the serialized
bytes a server will hold at once to perform an operation, and a server that materialises a whole
dataset to answer or to write charges the whole dataset against it. So unlike every other limit here,
**a caller cannot make a request small enough to satisfy it** — the number that matters is how much
the dataset holds, not what was asked for.

Two consequences, and both MUST be visible to a client rather than inferred:

- A **read** that would exceed it fails `422 query_result_limit_exceeded` with
  `details.reason: "materialization_limit"`, and `details.maximum` carries the bound.
- A **write** that would exceed it fails `507 dataset_full`, with the same reason. Not
  `query_result_limit_exceeded`: there is no query, no result and no scan — the caller may have
  inserted one record — and a client told its query result was too large looks at what it asked for,
  finds nothing excessive, and has nowhere to go. Not `503` either: waiting does not help. **The
  dataset has stopped accepting writes and only the operator can change that.**

**`max_stored_bytes` is an optional storage quota**, distinct from the memory bound above. When
present it is a positive integer. Absent, the dataset has no quota and its size alone never refuses a write. Present, a write that would leave
the dataset storing more than that many serialized bytes fails `507 dataset_full` with
`details.reason: "storage_limit"` and `details.maximum` carrying the quota. A client shows a dataset
as nearly full only when it advertises a quota; a refusal with `"materialization_limit"` is a write
too large for this server to perform at once, not a full dataset, and a smaller dataset or a raised
bound is the only remedy for either.

A server that charges the bound to what a request holds rather than to what the dataset holds
reaches it only when the request itself asks for that much — a query returning very large records,
say — and then the refusal is one the caller caused and can avoid by asking for less. That is the
better place to be, and it is why the bound is declared rather than assumed: a client can see which
kind of server it is talking to before it depends on one.

### Advertised capabilities

Everything a client can do for itself, given the required operations above.

| Capability | Meaning | Without it, a client |
| --- | --- | --- |
| `content` | Attachments, per the [content protocol](tessera-content-v1.md). | Has no attachments. |
| `sharing` | Grants to principals other than the owner. | Sees only its own datasets. |
| `creator_attribution` | Server-populated [creator fields](#creator-fields). | Has no trusted creator value. |

**`aggregation`, `distinct_references` and `text_matching` were on this list and are now required
operations.** A server MUST NOT advertise them: there is no choice left to report.

They were optional on the assumption that a server might find them too expensive, and the paragraph
above the required table already answers that — a server that cannot execute an operation
efficiently declares a bound and fails past it, and failing explicitly at a stated limit is
conformant while omitting the operation is not. Nothing about these three is different.

What made it worth changing rather than leaving alone is the middle column. *Reads the rows and
computes.* *Filters after reading.* **Neither is equivalent to the operation it replaces.** A client
filtering after reading returns a narrowed result set that looks exactly like a complete one, and a
client folding rows stops at a bound it invented, so two clients compensating differently disagree
about the same dataset with no way for a reader to tell which they are looking at. A capability
whose absence forces a client into a wrong answer is not optional; it is missing.

An operation whose capability is not advertised MUST be refused with `403` `not_supported` — never
silently ignored, and never answered with a partial result that looks complete.

**Compensating is the client's business, and it MUST be bounded.** A client that reads rows because a
server lacks an optional capability is doing the server's work with less information than the server
had; that is a legitimate trade, and an invisible one to the person using it, so it has to fail
rather than run forever. A client compensating MUST stop at a limit of its own and report the same
`query_result_limit_exceeded` a declining server would have returned. Silent compensation turns a
missing capability into an unexplained hang.

This applies to `content`, `sharing` and `creator_attribution`, none of which a client can fake. That is
not a coincidence: an optional capability a client *can* compensate for is one where compensation
and operation give different answers, which is the case for making it required rather than
documenting the gap.

**Transactions subsume what used to be advertised.** Conditional insertion, comparison against an
expected baseline and multi-object writes are not capabilities; they are operations inside a
transaction, and a transaction is required.

### Access, not capability

`access` reports what **this caller** may do with **this dataset**: `read`, `write`, `schema`,
`erase`, `insert`. It varies per token, not per implementation, and is answered from grants.
`insert` is true only for a caller that holds an [insert grant](#insert-grants); an owner adds
records through `write` and reports `insert: false`.

## Identifiers

| Kind | Grammar | Notes |
| --- | --- | --- |
| Dataset ID | `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` | Appears in paths. Server-minted. |
| Meta type ID | `^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`, ≤ 255 bytes | Reverse-DNS style permitted. |
| Field ID | `^[A-Za-z_][A-Za-z0-9_]*$`, ≤ 255 bytes | MUST NOT be `id` or `meta_id`. |
| Object ID | Non-empty, ≤ 255 bytes, no control characters, MUST NOT contain `//`. A server MUST NOT **mint** one beginning with `$` | Server-assigned, opaque to clients. Percent-encoded in paths. The reserved prefix is what lets a [transaction alias](#writes) stand where an identity does. |
| Content item ID | Non-empty, ≤ 255 bytes, no control characters, MUST NOT contain `//` | Store-assigned, opaque. Appears in a `media` value after its store. |

A meta type ID or field ID whose **whole** value case-insensitively matches a word in
[Appendix A](#appendix-a-reserved-words) MUST be rejected with `invalid_identifier`. The restriction
applies to the entire identifier, not to the dot-separated components of a type ID.

Servers MUST quote identifiers in any query language they generate. The reserved-word rule exists for
interoperability with implementations that do not, and does not license a server to skip quoting.

Clients MUST NOT parse object IDs, cursors, or infer structure from any identifier.

## Values

A value is `{"type": <type>, "value": <encoding>}`. For `null` the `value` member is omitted.

| Type | Encoding | Rules |
| --- | --- | --- |
| `integer` | JSON number | No fractional part. Range −2⁶³ … 2⁶³−1. Implementations MUST NOT parse through a double; values beyond ±2⁵³ are legal and MUST survive round-trip. |
| `float` | JSON number | IEEE 754 **binary64**. Finite only; NaN and infinities MUST be rejected with `invalid_field_value`. Subnormals are permitted. |
| `fixed` | JSON **string** | An exact decimal in canonical form, at most `max_fixed_digits` significant digits. A field declaring `fixed` MUST declare a `scale`. Never a JSON number. |
| `text` | JSON string | Valid UTF-8, at most `max_text_bytes` encoded bytes. |
| `boolean` | `true` / `false` | |
| `date_time` | JSON string | RFC 3339, UTC, **exactly** millisecond precision: `YYYY-MM-DDTHH:MM:SS.sssZ`. Offsets other than `Z` MUST be rejected. Servers MUST preserve the value exactly. |
| `media` | JSON string | A content reference, `<content_store_id>//<item_id>`. Stored and returned unchanged. Never bytes, never a URL. See [Content](#content). |
| `json` | JSON string | MUST contain valid UTF-8 JSON. Servers MUST validate that it parses and MUST NOT reformat, reorder or canonicalise it. |
| `blob` | JSON string | Base64, standard alphabet, padded. At most `max_inline_blob_bytes` decoded. Larger binaries belong in content storage. |
| `reference` | object | `{"dataset_id", "object_id", "meta_id"}`, each satisfying its grammar. |
| `null` | — | The only representation of an absent value. An empty string, empty reference or zero MUST NOT be used to mean absent. |

Fixed millisecond precision is deliberate: an implementation storing whole seconds and one storing
microseconds would otherwise disagree about the same record. Servers MUST round-trip exactly what
they were given.

#### `fixed`, and why it is a string

**A JSON number would be read through a binary double by most clients, and the exactness would die
there rather than at the server.** That is not hypothetical: it is the cause behind every documented
divergence in this repository's value vectors, and a consumer carrying money through an aggregate
was reducing rows locally rather than trusting a sum, because their decoder declared the member a
double. A string keeps the value away from that reader entirely.

**The canonical form is the only accepted form.** Grammar: an optional `-`, an integer part with no
leading zero (a single `0` where the value is less than one), and — when there is a fraction at all
— a `.` followed by at least one digit and ending in a non-zero digit. No exponent, no `+`, no `-0`,
no empty fraction. So `0`, `42`, `-1.5` and `0.001` are values; `1.50`, `1.0`, `01`, `-0`, `+1`,
`1e5`, `1.` and `.5` are `400 invalid_field_value`.

**A server MUST NOT normalise a non-canonical value; it MUST refuse it.** Rewriting `1.50` to `1.5`
would return a value the caller did not send, and this protocol compares raw values in `expect` and
folds them into [record versions](#record-versions) — so a silently rewritten value becomes a
mismatch the caller cannot account for. Refusing is the loud half of that choice, and it is why one
spelling per value is worth the ergonomic cost of requiring callers to canonicalise.

**Equality is value equality and, for canonical values, string equality too.** That is the property
the single spelling buys: two clients that canonicalise agree byte for byte, and a client may
compare stored values without a decimal library.

**`scale` bounds, it does not pad.** A field's declared `scale` is the greatest number of fraction
digits a value in it may carry. Because the canonical form has no trailing zero, a field at scale 2
stores `1`, `1.5` and `1.25` and refuses `1.255` — which could only be stored by rounding, and
rounding is the caller's decision. A scale MAY be **widened** by a type update and MUST NOT be
narrowed: widening rewrites nothing, because every stored value is already valid at a larger scale,
while narrowing would invalidate values already written.

**`fixed`, `integer` and `float` compare exactly across each other.** A comparison is over the exact
rational each denotes, so `1.00000000000000001` as `fixed` is not equal to `1.0` as `float`, and
`3` as `integer` is equal to `3` as `fixed`. No operand is rounded into another's domain to make a
comparison possible.

**Two values a conformant server MUST carry**, because each defeats a different platform's obvious
implementation and neither catches the other:

| The value | What it defeats |
| --- | --- |
| A value with more than 38 significant digits | A fixed-width decimal type, which silently drops the low digits |
| `1.00000000000000001` | Parsing through binary64, which rounds it to `1` |

A client meeting a `fixed` field whose type it does not know degrades to *not recognized*, the same
as for an unknown role or an unknown type, and never to *rejected*.

Encoding details for independent implementations:

- An `integer` JSON number is accepted when its **exact mathematical value** is an integer in the
  stated range, including integral decimal and exponent forms such as `1.000` and `1e3`. Servers MAY
  return it as plain decimal digits. They MUST NOT round a fractional value into range.
- A wire `float` number uses the same binary64 parsing rule as `text` to `float` below: nearest,
  ties to even, rejecting overflow and accepting underflow. Outer JSON number spelling need not be
  preserved; the resulting binary64 value must be preserved, including the sign of zero.
- A `date_time` must name a valid calendar date, with hours 00–23, minutes 00–59 and seconds 00–59.
  Leap-second spellings (`:60`) are not accepted by this protocol's RFC 3339 subset.
- JSON strings MUST decode to Unicode scalar values. Invalid UTF-8 and unpaired escaped UTF-16
  surrogates MUST be rejected, not replaced by a replacement character. No Unicode normalization is
  applied. This also applies to the JSON text carried by a `json` value.

### Accepting a value for a declared field

A write supplies values whose wire type need not match the field's declared type. A server MUST accept
exactly the conversions below, MUST perform each exactly as specified, and MUST reject every other
combination with `invalid_field_value`. A converted value is stored and returned as the declared type.

| Declared type | Accepted | Conversion |
| --- | --- | --- |
| any | `null` | Stored as absent. |
| `integer` | `integer` | Identity. |
| `integer` | `float` | Only when the value is integral and within −2⁶³ … 2⁶³−1. A fractional part is rejected, never rounded. |
| `integer` | `text` | `-?[0-9]+`, no whitespace, no sign other than `-`, no leading `+`, within range. Leading zeros permitted. |
| `fixed` | `fixed` | Identity. |
| `fixed` | `integer` | Always exact: an integer is a decimal with no fraction. |
| `fixed` | `text` | The same canonical grammar. Text is not a looser door into a type whose point is one spelling per value. |
| `integer` | `fixed` | Only when the value has no fraction and fits −2⁶³ … 2⁶³−1. |
| `float` | `fixed` | **Rejected.** Every binary64 has an exact decimal expansion, so "when exact" would admit almost everything and answer `0.1` with its fifty-odd true digits — technically exact, and not what any caller asking for a decimal meant. |
| `fixed` | `float` | Only when the decimal is exactly a binary64 value, which is the rule `integer` to `float` already follows. `0.5` converts; `0.1` is rejected rather than rounded. |
| `float` | `float` | Identity. |
| `float` | `integer` | **Exactly representable in binary64, or rejected.** The test is representability, not magnitude: above 2⁵³ the representable integers thin out, so 2⁵³ and 2⁵³+2 are accepted while 2⁵³+1 is not. Never rounded. |
| `float` | `text` | The JSON number grammar, parsed to binary64 with round-to-nearest, ties to even. A value whose magnitude rounds to infinity MUST be rejected; one that rounds to zero or a subnormal is accepted, and the rounded value is what is stored. |
| `text` | `text` | Identity. |
| `boolean` | `boolean` | Identity. |
| `boolean` | `text` | Exactly `"true"` or `"false"`, lowercase. |
| `date_time` | `date_time` | Identity. |
| `date_time` | `text` | The protocol's own timestamp form exactly: `YYYY-MM-DDTHH:MM:SS.sssZ`. |
| `media` | `media` | Identity. |
| `media` | `text` | Taken as the opaque reference unchanged. |
| `json` | `json` | Identity. |
| `json` | `text` | Must parse as JSON; stored as given, never reformatted. |
| `blob` | `blob` | Identity. |
| `blob` | `text` | Base64, standard alphabet, padded. |
| `reference` | `reference` | Identity. |

**This is deliberately narrower than convenience conversion in a client.** Four conversions are absent
because no interoperable answer exists for them, and inventing one per implementation is worse than
refusing:

- **Anything to `text`.** There is no defined textual form for a reference, a blob or a timestamp that
  two implementations would agree on. A client that wants text sends text.
- **Timestamps to and from numbers.** Epoch seconds, milliseconds and other origins are all plausible
  and none is implied by the wire format.
- **Numbers to `blob`.** Width and byte order are unanswerable.
- **Integers to `boolean`.** Which integers are true is a per-language convention, not a fact.

A client holding a value in one of those shapes converts it itself, where it knows what it meant.

### Comparing two values

Comparison rules apply to filtering, sorting, absence keys and expected-field checks alike, so they
MUST be identical in all four.

- **Equality is exact.** `text` compares by Unicode code point; `blob` compares its decoded bytes;
  `json` compares its **exact stored string**, since the protocol forbids reformatting and therefore
  cannot promise semantic JSON equality; `media` compares its opaque string; `reference` compares all
  three of `dataset_id`, `object_id` and `meta_id`.
- **Numbers compare exactly, across types.** An `integer` and a `float` MUST NOT be compared by
  converting the integer to a floating-point value: integers beyond 2⁵³ are legal, and rounding one
  would make two distinct values compare equal. Compare exactly — for example by comparing the float's
  integral part and fractional sign against the integer.
- **Ordering of references** is `meta_id`, then `dataset_id`, then `object_id`, each by code point.
- Values of types that cannot be compared have no order and MUST NOT be silently coerced. A predicate
  comparing incomparable types MUST be rejected with `invalid_filter`, not evaluated as false.

## Schema

A field:

```json
{ "id": "recorded_at", "meta_id": "com.example.reading", "name": "Recorded", "type": "date_time",
  "roles": ["timestamp"], "annotations": { } }
```

`meta_id` names the type that *declares* the field, which for an inherited field is an ancestor.
A field may also carry `"immutable": true` or `"creator": true`. See [Immutable fields](#immutable-fields)
and [Creator fields](#creator-fields).
`name` is one human-readable stored string, returned unchanged by language negotiation. v1 has no
translation write path, so servers MUST NOT advertise `locales` or promise translated schema names.
Editing the stored name remains a schema edit.

**Localise from the identifier.** This is the settled answer for v1 rather than a gap waiting to be
filled, so a client can build on it: type and field identifiers are stable and language-neutral, a
client may localise display names from them however it likes, and that presentation MUST NOT change
the effective schema version used by `expect_type` — which is why changing display language cannot
invalidate a guard. A multilingual application keys its own catalogue on the identifier and treats
`name` as the label for a reader who has no catalogue.

A type:

```json
{ "id": "com.example.reading.temperature", "parent_id": "com.example.reading",
  "name": "Temperature", "roles": ["track"], "annotations": { "revision": "7c1e…" },
  "fields": [ … ] }
```

- Metadata returned by a server MUST be **effective**: every field the type has, including inherited
  ones, in a stable order that MUST NOT change between calls unless the schema changes.
- `parent_id` is `null` for a root type. Cycles MUST be rejected with `meta_schema_error`.
- A field ID MUST be unique across a type's whole effective hierarchy.
- Servers MUST expose effective metadata for every type they own, and MUST own the metadata for any
  object they expose.

### Roles and annotations

A type and a field may each carry two optional properties. **A server stores them, returns them, and
never reads them.**

```json
"roles": ["value", "body.mass"],
"annotations": { "dimension": "mass", "meaning": "sample", "unit_source": "record",
                 "unit_field": "unit" }
```

**Tessera defines no role names and no annotation keys, and never will.** What the strings mean is
agreed between the programs that use them, in a vocabulary this specification does not contain and
cannot validate. That is the whole point: the moment a server understands a domain concept, the
concept belongs to whoever put it there, and a contract two unrelated products can share stops being
possible.

**What they are for.** Without them, a client binds its behaviour to field IDs — reading `weight`
because it is called `weight` — so renaming a field is a migration, and a program that has never seen
your type can do nothing with it. With them, a client asks *which field carries this role* and gets an
answer whatever the field is called. Meaning travels with the data.

**The honest cost:** interop depends on a shared vocabulary that nothing enforces. A typo produces a
type nobody recognizes, not an error. Two products that never agree on names share a dataset and
understand none of each other's data — correctly, and uselessly.

#### Rules

- **`roles`** is an array of strings, unordered. Duplicates MUST be rejected with `meta_invalid_field`.
  Each is 1…`max_role_length` characters from `[A-Za-z0-9._-]`, and there are at most `max_roles` of
  them.
- **`annotations`** is a JSON object whose values are arbitrary JSON. It is stored **exactly as
  given** — byte for byte, with no key reordering and no normalisation — as the `json` field type
  already requires. A server that rewrote it would silently change what a client wrote, and nobody
  could tell which.
- **A server MUST NOT interpret, validate, infer, reject or rewrite either**, beyond the structural
  limits above. It does not check that a `dimension` exists, that a `meaning` is one of a set, or that
  a role is one it has seen before.
- **Both count toward `max_request_bytes`**, and both are erased with the dataset.
- **Declared and effective are different things, and the byte-exactness is per value.** A type's
  *declared* roles and annotations are what it was written with. Its *effective* ones are its own
  merged with every ancestor's, which is what metadata reads return, as with fields.
  - The merge is **key-wise for annotations and a union for roles**. Each annotation *value* is
    returned byte-for-byte as it was written; the containing object is assembled, not echoed.
  - **A descendant MUST NOT redeclare an annotation key an ancestor declared**, and MUST be rejected
    with `meta_invalid_field` if it tries. So no key can collide and the merge is unambiguous — there
    is no override rule because overriding is not permitted.
  - Roles are a set, so a descendant repeating an ancestor's role is harmless and MUST be accepted; it
    appears once.
  - **A field's annotations never merge.** A field is declared by exactly one type — its `meta_id` —
    and an inheriting type carries that declaration unchanged.
- **No uniqueness rule.** Two fields of one type may carry the same role — blood pressure has two
  value fields. Deciding between candidates is the client's business, not the server's.
- **Changing them is a schema update** and never touches a stored record. That matters more than it
  sounds: a record carries no record of which annotations were in force when it was written, so
  editing one applies retroactively to everything ever stored under that type.
- **They are not queryable.** There is no predicate over roles or annotations, and none is planned for
  v1. Resolving a role to a type or a field is a **client-side operation over metadata**, which a
  client already holds in full. Querying *across* types by role would be a union over heterogeneous
  types — whose projection, sort order and pagination all have to be answered — and the alternative is
  a handful of ordinary queries the client already knows how to make.

#### Recognition, not validation

A client decides for itself whether a type is something it can use, by checking that for each role it
requires, some field carries it. That check is **structural, per client, and invisible to the server**:
many types may satisfy one client's requirements, one type may satisfy several clients' at once, and a
type that satisfies nobody is still perfectly valid. **The failure mode is *not recognized*, never
*rejected*** — which is what lets one dataset hold another product's types without either program
having to know about the other.

### Immutable fields

```json
{ "id": "administered_at", "meta_id": "com.example.dose", "name": "Administered",
  "type": "date_time", "immutable": true }
```

**A value, once written, cannot be changed.** That is the whole guarantee, and it is stated narrowly
on purpose. Every conformant server MUST enforce it; it is not a capability, because what it promises
is something a client cannot check for itself — that *no other writer* has changed the value — and a
dataset with more than one writer is the premise of this protocol. An optional guarantee is not one:
a client that could not rely on it would validate defensively anyway.

The server reads the **constraint**, never the **role**. It applies a mechanical rule it was handed
and still does not know what the field means.

- **`immutable` is a boolean.** `false` and absent mean the same, and a server returns the member only
  when it is `true`. Any field type may carry it.
- **Null is never written.** A field holding nothing may be given its first value by `insert` or by
  `update`. From then on it is fixed.
- **Restating the stored value is not a change** and MUST be accepted. The comparison is between the
  value as it would be stored — after [conversion](#accepting-a-value-for-a-declared-field) to the
  declared type — and the value stored: so `"005"` sent to an integer field holding `5` restates it.
  It is the stored value, not protocol equality: `-0.0` over a stored `0.0` is a change, because the
  record would return something different, and a `json` value is compared byte for byte, as stored.
- **Anything else is refused**, clearing to `null` included, with `409` / `conflict`,
  `details.reason: "immutable_field"`, and `details.fields` naming every immutable field the write
  would have changed:

  ```json
  { "error": { "code": "conflict", "message": "An immutable field already holds a value",
               "details": { "reason": "immutable_field", "object_id": "8121",
                            "fields": ["administered_at"] } } }
  ```

  An `update_where` that would change one in any matching record fails the same way, as a whole.
- **A subtype inherits it.** A field is declared by exactly one type and an inheriting type carries
  that declaration unchanged, so there is nothing for a subtype to narrow: a record of a descendant
  is held to its ancestors' immutable fields.

**What it does not do.** It constrains update, never deletion: a record holding an immutable value
MAY be deleted, and `delete_type`, field removal and [erasure](#erasure) proceed as for any other
field. It records no writer and no prior value, so it is not an audit trail. A client that needs a
history keeps one — as records whose values never change, which is the shape this flag describes.

**It follows the schema.** Adding the flag to an existing field binds from the commit that adds it,
over values already written; nothing already stored can violate it, since it constrains what happens
next. **Removing it is a schema edit like any other**, open to whoever holds `schema` on the dataset:
the protection boundary is that grant, not the flag. The change alters the type's `schema_version`,
so a client guarding a write with [`expect_type`](#asserting-a-schema) sees it.

**A server that predates this rule refuses a type declaring it** with `400 invalid_request`, because an
unrecognised member of a request is never ignored. So a client cannot silently get a field it believes
is immutable and is not; it learns it at the schema write.

### Creator fields

A dataset advertising `creator_attribution` accepts `"creator": true` on a declared text field.
False and absence are equivalent and the server returns only true. The property is inherited.
A non-text creator field is refused with `400 meta_invalid_field`. A dataset offering the capability
MUST admit text values of at least 32 bytes. A limit change MUST preserve this minimum while the
dataset has any creator fields. Clients check the capability before sending the member.
If provisioning gives the client no capability information for the new dataset, it can declare
creator fields after discovering the dataset's capabilities.
A server that knows the member but withholds the capability refuses the declaration with
`403 not_supported`.

On every insert, the server fills each effective creator field with the same opaque, 32-character
lowercase hexadecimal identifier. The identifier is stable for the authenticated principal in
that dataset, including across application authorizations, server restarts and restores that retain
the dataset identity. Deployments MUST keep authenticated principal identities stable and MUST NOT
reassign one principal's identity to another. Distinct principals
in a dataset receive distinct identifiers with cryptographically negligible collision probability.
Identifiers are unlinkable by value across datasets and MUST NOT encode a public account identifier
or credential. An installation can use a dataset-keyed cryptographic derivation with a separate
domain from other identifiers. Erasing a dataset ends the scope of its creator identifiers.

Every record write supplying a creator field is refused with `400 invalid_field`, including an
explicit null or a restatement. This applies to inserts, updates and bulk updates even if the
bulk filter matches nothing. The server uses the authenticated writer even for insert-only grants.
Attribution participates in projection, filtering, record versions and erasure as a stored text
value under the usual visibility rules. A declaration opts into showing this pseudonym to readers
who can see the field. It grants no authority to edit or delete any record.

Adding a creator field to an existing type leaves it null on older records, including descendants.
Subsequent inserts populate it. A schema update attempting to switch an existing field to or from
creator status is refused with `400 meta_schema_error`. A confirmed removal discards stored values
under the normal rules. A later declaration begins with no value on existing records.

### Changing a schema

A create or update body carries **only the fields the type itself declares**; inherited fields are
never restated, and supplying one MUST be rejected with `meta_invalid_field`.

`PUT` **replaces** the type's own declared field set. The effect of the difference is:

| Change | Required behaviour |
| --- | --- |
| Field added | Permitted. Existing objects of the type and its descendants have `null` for it. |
| Field removed | Permitted. Its stored values are deleted throughout the hierarchy, atomically with the schema change. |
| Field's `type` changed | MUST be rejected with `meta_schema_error`. Remove the field and add a new one if that is what is meant; the protocol does not convert stored values. |
| Field's `name` changed | Permitted. Names are presentation only. |
| Field's `creator` changed | Refused. Add a new creator field to opt in. |
| Field's `immutable` changed | Permitted either way, and never touches a stored value. See [Immutable fields](#immutable-fields). |
| `parent_id` changed | MUST be rejected with `meta_schema_error`. Reparenting rewrites which objects exist in which hierarchy and is not a schema edit. |
| `id` changed | Not expressible; the path names the type. |

The whole update — schema rows, added columns, and deleted values across every descendant — MUST be
atomic. A partially applied schema change is a conformance failure, not a degraded outcome.

A field removal that would delete values is destructive. A `PUT` whose effect removes at least one
field MUST carry `"confirm"` in its body, equal to the type ID, and MUST be rejected with
`meta_schema_error` and `details.reason: "confirmation_required"` otherwise, naming the fields that
would go:

```json
{ "type": { "id": "com.example.reading.temperature", … }, "confirm": "com.example.reading.temperature" }
```

Confirmation is a body field wherever the request has a body, matching [erasure](#erasure). Only
`DELETE` uses a query parameter, because it has no body to put one in. An `update_type` **inside a
transaction** carries the same `confirm` member, with the same value and the same refusal: one rule,
whichever way the change arrives.

## Records

A record set is one metadata projection plus rows positionally matching it.

```json
{
  "meta": { "id": "com.example.reading", "parent_id": null, "name": "Observation",
            "fields": [ { "id": "recorded_at", "meta_id": "com.example.reading",
                          "name": "Recorded", "type": "date_time" } ] },
  "records": [
    { "id": { "id": "8121", "dataset_id": "s-7f3a9c21b04e4d1789ab" },
      "meta_id": "com.example.reading.temperature",
      "values": [ { "type": "date_time", "value": "2026-09-14T08:12:31.500Z" } ] }
  ],
  "watermark": "w:8f21…"
}
```

- `meta.fields` defines the projection and its order. Every record's `values` MUST have the same
  length, in the same order. This is the wire contract, not an optimisation.
- **The order within `meta.fields` is the server's to choose.** It need not follow the order the
  client listed in `fields`, and implementations differ — one returns request order, another schema
  order. A client MUST resolve values through `meta.fields` and MUST NOT assume its request order.
  The server MUST return the same order for the same query, so a client may cache the mapping for a
  repeated query but not across schema changes.
- `meta` describes the **queried** type. Each record's `meta_id` is its own **concrete** type, which
  MAY be a descendant. Both MUST always be present.
- `id.dataset_id` MUST equal the dataset being addressed.
- When a projection selects no fields, `meta.fields` is empty and `values` is empty; identity is
  still returned.
- `watermark` is always present; see [The watermark](#the-watermark).
- `version` is present on each record of a single-object read, and on a query's records only when it
  asked for them; see [Record versions](#record-versions).

### Object identity

An object ID is unique within a **type hierarchy** — one root type and its descendants — and is not
guaranteed unique across hierarchies within a dataset. Two objects of unrelated root types MAY share
an ID.

Consequently **an object ID is only meaningful together with a type**, and every operation that names
one also names a type: reads, writes and deletes carry `{t}` in the path, predicates are evaluated
within a query's own hierarchy, and a `reference` carries `meta_id`. A client MUST NOT treat the
`{dataset_id, id}` pair alone as a durable identity, and MUST NOT use it as a key in a collection
spanning more than one hierarchy.

A server MAY make object IDs unique per dataset, and this is the simpler implementation. It MUST NOT
rely on clients assuming so.

**An object ID MUST NOT be reused within its hierarchy for the lifetime of the dataset**, including
after the object it named is deleted. A monotonic sequence satisfies this; deriving the next ID from
the current maximum does not, because deleting the highest row makes the next insert reuse its ID.

This matters more than it looks. A client may hold an object ID durably — in a user preference, a
saved selection, an export request — and resolve it later against a type it already knows, without
carrying a version or re-checking. If an ID is reused, that resolution silently returns an unrelated
object of the right type, which is indistinguishable from success. Only [erasure](#erasure) resets the
space, and it mints a new dataset identity at the same time, so an identity from before an erase can
never match one after it.

## Queries

```json
{
  "type_scope": { "kind": "descendants", "allowed_concrete_type_ids": ["com.example.reading.temperature"] },
  "filter": { "and": [ … ] },
  "selection": { "kind": "all" },
  "result_sort": [ { "field_id": "recorded_at", "ascending": false } ],
  "fields": ["recorded_at", "value"],
  "maximum_text_length": null,
  "include_versions": false
}
```

**`limit`**, where an operation accepts one — omitted or `null` means the server's
`max_overall_limit`. It is never unbounded.

**`type_scope`** — `{"kind": "exact"}` matches only the named type;
`{"kind": "descendants"}` also matches stored descendants, optionally restricted by
`allowed_concrete_type_ids` (non-empty, unique, at most `max_allowed_concrete_types`).

**`fields`** — omitted or `null` projects every field of the queried type. An empty array projects
none. Names MUST resolve against the queried type's effective schema; a field belonging only to a
descendant MUST be rejected with `invalid_field`.

**`include_versions`** — omitted or `false` returns no [versions](#record-versions). It is a query
field rather than a projected pseudo-field, so `fields` stays positionally consistent with
`meta.fields`. A single-object read always carries a version and does not take this option.

**`maximum_text_length`** — 1…1024 **Unicode code points**, counted on the decoded value and cutting
between code points. Grapheme clusters are deliberately not the unit: their boundaries depend on the
Unicode version an implementation was built against, so two conformant servers would truncate the same
string differently. Truncates returned text only; filtering and sorting still use whole values, and
stored values are unchanged. Requires explicit `fields`, and MUST NOT be combined with grouped
selection.

**`selection`** — `{"kind": "all"}`, or
`{"kind": "first_per_concrete_type", "count": N, "ordered_by": [ … ]}` with `count` in
1…`max_selection_per_concrete_type` and a non-empty ordering. Selection ordering is independent of
`result_sort`: rows are selected under the first, then the result is ordered by the second.

### Predicates

Exactly one member per predicate object.

| Form | Meaning |
| --- | --- |
| `{"field": {"id": F, "exact": V}}` | `F` equals `V`. If `V` is null, matches rows where `F` is absent. |
| `{"field": {"id": F, "range": {"lower": V?, "upper": V?}}}` | Inclusive on both ends. At least one bound MUST be present. |
| `{"text_contains": {"field_id": F, "text": S}}` | Literal, case-sensitive substring. Wildcards have no special meaning. |
| `{"object_ids": [ID, …]}` | Membership. An empty array matches nothing. At most 256 IDs. |
| `{"object_id_after": ID}` | Object ID strictly greater than `ID`, lexically. |
| `{"and": [P, …]}` / `{"or": [P, …]}` | At most `max_predicate_width` children. `and` of none matches everything; `or` of none matches nothing. |
| `{"not": P}` | Negation. |

Nesting depth MUST NOT exceed `max_predicate_depth`. Servers MUST validate depth and width before
evaluating anything.

**Null semantics are normative and easy to get wrong:**

- `exact` against null means *the field is absent*. Its negation means *the field is present*.
  A server MUST NOT emit an SQL `= NULL` or bind null to `=`, whose UNKNOWN result silently drops rows.
- A **range** never matches a null value, whichever bounds are present.
- `text_contains` never matches a null value.

**Ordering is normative:**

- Comparison follows [Comparing two values](#comparing-two-values): numbers exactly and across
  types, text by code point, booleans false < true, timestamps chronologically, references by
  `meta_id` then `dataset_id` then `object_id`.
- **Nulls sort before all non-null values ascending, and after them descending.** Implementations on
  engines with the opposite default MUST correct for it explicitly.
- Object IDs compare as **text, by code point** — not as numbers, even where a server stores integers.
- Every result MUST be totally ordered: servers MUST append object ID ascending as the final
  tie-break of both `result_sort` and `ordered_by`. This is sound because object IDs are unique within
  a type hierarchy and a query never spans two of them; see [Object identity](#object-identity).

Sort and selection terms MAY name fields absent from `fields`. Such fields MUST NOT appear in the
returned metadata or values.

## Operations

`{d}` is a dataset ID, `{t}` a meta type ID, `{o}` a percent-encoded object ID.

### Lifecycle

| Operation | Request |
| --- | --- |
| Connect / validate | `GET /v1/datasets` |
| Erase | `POST /v1/datasets/{d}:erase` — see [Erasure](#erasure) |

There is no open or close. A client's connection lifecycle is local to it.

### Schema

| Operation | Request | Response | Grant |
| --- | --- | --- | --- |
| All types | `GET /v1/datasets/{d}/meta` | `200` `{"types": [ObjectMeta, …]}` | `read` |
| One type | `GET /v1/datasets/{d}/meta/{t}` | `200` `{"type": ObjectMeta}`, or `404` when it does not exist, which is an ordinary answer | `read` |
| Existence | `HEAD /v1/datasets/{d}/meta/{t}` | `200` or `404`, no body | `read` |
| Children | `GET /v1/datasets/{d}/meta/{t}/children` | `200` `{"types": [ObjectMeta, …]}`, direct children only | `read` |
| Create | `POST /v1/datasets/{d}/meta` — `{"type": ObjectMeta}` | `201` `{"type": ObjectMeta}` effective, or `409` if the type exists. One `define_type` | `schema` |
| Update | `PUT /v1/datasets/{d}/meta/{t}` — `{"type": ObjectMeta, "confirm": "{t}"?}` | `200` `{"type": ObjectMeta}` effective. One `update_type`; `confirm` is required to remove a declared field | `schema` |
| Delete |  `DELETE /v1/datasets/{d}/meta/{t}?confirm={t}` | `200` `{"deleted_types": ["…"], "deleted_objects": 412}`. One `delete_type` with `cascade`: **destroys** descendant types and all their objects | `schema` |
| | | A `DELETE` carries no body, so its confirmation is a query parameter; everywhere else it is a body field. | |

**Metadata reads require only `read`.** A caller cannot interpret a single record without the schema
that describes it, so gating metadata behind `schema` would make an otherwise valid read grant
useless. `schema` gates mutation alone.

**Metadata is inside the authorized view like everything else.** A caller sees the types it can reach
and no others:

- Types it holds a grant for, and the concrete types of objects it holds object grants for — without
  those it cannot interpret what it was given.
- **Nothing else.** A type outside the view is **absent** from `GET /meta` and answers `404`
  `unavailable` individually, as a dataset the caller may not use is absent from `GET /v1/datasets`.
- An **ancestor** need not appear in its own right; its fields, roles and annotations reach the caller
  through the effective metadata of the descendant they can see, which is the minimum needed to read
  that descendant and no more.

This matters more than it looks. Type names, field names, roles and annotation values are written by
people and describe what a dataset is *about* — a list of every type is a summary of someone's life.
Granting one object is not consent to enumerate the schema around it.

Removing an empty leaf type together with the configuration row that describes it is an ordinary
transaction, not a special operation — which is the point of having transactions at all:

```json
{ "operations": [
    { "op": "expect", "target": { "type": "com.example.device_registration", "id": "412" },
      "fields": { "schema_id": { "type": "text", "value": "…" } } },
    { "op": "delete", "target": { "type": "com.example.device_registration", "id": "412" } },
    { "op": "delete_type", "type": "com.example.reading.temperature" } ] }
```

`delete_type` without `cascade` fails `409` `conflict` with `details.reason: "type_not_empty"` if the
type has descendants or holds objects, so the schema and the row describing it disappear together or
not at all. The request is well formed and the dataset's state is what refuses it, which is why it is
a conflict rather than a bad request.

**`cascade` is explicit because the damage is invisible.** Removing a type takes every descendant type
and every object under all of them, which in a shared dataset means rows the caller may not be able to
see. `{"op": "delete_type", "type": T}` refuses rather than guesses; `{"op": "delete_type", "type": T,
"cascade": true}` states the intent. The `DELETE /meta/{t}?confirm={t}` form is exactly the cascading
version, which is why it carries a confirmation and returns what it destroyed.

### Reads

| Operation | Request | Response |
| --- | --- | --- |
| One object | `GET /v1/datasets/{d}/objects/{t}/{o}?fields=a,b` | `200` record set of exactly one row, or `404` `not_found` |
| Query | `POST /v1/datasets/{d}/objects/{t}:query` — `{"query": Q, "limit": N?}` | `200` record set |
| Count | `POST /v1/datasets/{d}/objects/{t}:count` — `{"filter": F?}` | `200` `{"count": 412}` |
| Aggregate | `POST /v1/datasets/{d}/objects/{t}:aggregate` — `{"filter": F?, "field": X?, "group_by": G?}` | `200`, see below |
| Distinct references | `POST /v1/datasets/{d}/objects/{t}:distinct-references` — `{"field": X, "filter": F?, "maximum_count": N}` | `200` `{"references": [ … ]}` |

All reads require the `read` grant. `limit` is at most `max_overall_limit`; omitted means that maximum.

**Every read carries the dataset's watermark**, and so does every transaction. See
[The watermark](#the-watermark).

**A single-object read carries the record's [version](#record-versions).** A query carries versions
only when it asks, with `"include_versions": true`, so a ten-thousand-row read pays nothing for a
guard it will not use.

**A single-object read never returns an empty record set.** A missing object is `404` `not_found`, so
absence is reported the same way everywhere. An empty `records` array is reserved for queries.

**Count** is descendant-inclusive, matching a query with `type_scope` `descendants` and no
restriction, so that `:count` and `:query` over the same filter agree.

**Aggregate** is exact-type only. `field` names a numeric field, or is omitted for a count alone.
`group_by` names a `text` or `reference` field and produces at most `max_aggregate_groups` groups;
more MUST fail with `422 query_result_limit_exceeded` with `details.reason: "group_limit"` rather
than truncating.

```json
{ "aggregates": [ { "group": "manual", "row_count": 412, "value_count": 400,
                    "sum": 8123.5, "minimum": 1.0, "maximum": 99.5 } ] }
```

**Grouping by a reference groups by identity.** The protocol already defines equality and a total
order over references, so this introduces no comparison and no vocabulary, and nothing about what a
reference *means* is consulted. `group` is then the bare qualified identity that
[distinct references](#reads) returns — one representation of a reference rather than two:

```json
{ "aggregates": [ { "group": { "dataset_id": "d-4f2a", "object_id": "o-19", "meta_id": "m.subject" },
                    "row_count": 12, "value_count": 12,
                    "sum": 430, "minimum": 5, "maximum": 90 } ] }
```

A server MUST compare the whole triple. Two objects agreeing on `object_id` alone are different
objects, and putting them in one group is a wrong answer rather than a coarse one.

**`group_by` MUST NOT name any other type.** A number or a timestamp would need a bucket, and a
bucket needs a timezone, a day boundary, daylight-saving handling and a week start — all of which
are the client's policy and none of which a storage protocol can decide. Grouping by an identity is
mechanical; grouping by a derived bucket is not, and rollups stay in the client.

**Group order is specified**, so that two servers answering the same query answer in the same
sequence: the null group first, then text groups ascending by code point, then reference groups in
the qualified order — `meta_id`, then `dataset_id`, then `object_id`, each by code point. A client
paging a chart's legend gets a stable axis without sorting the answer itself.

Aggregation edge cases are normative, because implementations diverge on all of them:

- **No matching rows**, ungrouped: exactly one entry with `row_count` 0, `value_count` 0, and `sum`,
  `minimum` and `maximum` `null`. Not an empty array — an ungrouped aggregate always answers.
- **No matching rows**, grouped: an empty `aggregates` array.
- `row_count` counts matching rows; `value_count` counts rows whose `field` is non-null. When
  `value_count` is 0, `sum`, `minimum` and `maximum` MUST be `null` — never `0`.
- Rows whose `group_by` field is null form one group with `"group": null`, which counts toward
  `max_aggregate_groups`. A null group is not dropped, whatever the grouped field's type. This is
  deliberately unlike [distinct references](#reads), which refuses a null: that operation answers
  *which references occur* and a null is not one, while an aggregate answers *what the matching rows
  total*, and dropping the rows with no group would report a total that silently excludes rows the
  same filter counted.
- `field` MUST be an `integer`, `float` or `fixed` field. Any other declared type is `invalid_field`.
- **Over a `fixed` field, `sum`, `minimum` and `maximum` are JSON strings in canonical form, never
  JSON numbers.** A number would be read through a double by most clients and the exactness the type
  exists for would be lost on the way in, after the server took care to preserve it. The sum is the
  exact total — every addend has a finite scale, so the total does too — carried at the widest scale
  any contributing value used, and a total exceeding `max_fixed_digits` MUST fail with
  `422 query_result_limit_exceeded` and `details.reason: "sum_overflow"` rather than be rounded.
- `sum`, `minimum` and `maximum` are JSON numbers for `integer` and `float` fields. `minimum` and
  `maximum` MAY lose integer precision beyond 2⁵³; `sum` follows the rules below. These are statistics, not values, and MUST NOT
  be used as expected-field baselines.
- For an `integer` field with nonzero `value_count`, `sum` MUST be the exact mathematical sum of the
  non-null values in the group when that total is in −2⁶³ … 2⁶³−1, including beyond ±2⁵³. A total
  outside that range MUST fail with `422 query_result_limit_exceeded` and
  `details.reason: "sum_overflow"`, even if the
  implementation can represent a wider total. Integers MUST NOT be rounded through binary64.
- For a `float` field with nonzero `value_count`, `sum` MUST be the exact mathematical sum of the
  stored binary64 values, rounded once to binary64 with round-to-nearest, ties to even. If that
  rounded result is infinite, the request MUST fail with `422 query_result_limit_exceeded` and
  `details.reason: "sum_overflow"`. Subnormal results are permitted; an exact zero is returned as
  positive zero.
- The sum rules apply to each group independently and to the final total, not intermediate partial
  sums. Row order and accumulator width MUST NOT change the result or cause premature overflow:
  cancellation can bring a total back into range. An overflowing group fails the whole request;
  partial aggregates, wrapped totals and non-finite numbers MUST NOT be returned. Separately declared
  scan bounds still apply; accumulator width is not a declared scan bound.

There is no defined `average` member. A client may compute `sum / value_count` when `value_count` is
nonzero; when it is zero there is no average. This needs no second server result that could disagree
with the counts and sum.

**Distinct references** is exact-type only, requires a reference field, and returns at most
`maximum_count` (1…`max_distinct_references`) results sorted by qualified identity. Exceeding it MUST
fail with `422 query_result_limit_exceeded` and `details.reason: "distinct_reference_limit"`;
a truncated list MUST NOT be returned. A null or malformed stored value MUST fail with
`400 invalid_field_value` rather than be skipped.

**That includes ordinary absence, not only corruption.** A null is an unset optional field, which is
the common case rather than the rare one, so this operation is unusable on an optional reference
field until the caller excludes unset rows with a filter — and doing that is the expected response,
not a workaround. The rule is deliberate and the reason is the same one that forbids truncation:
skipping nulls would answer *these are the references* while concealing that some rows had none, and
a caller cannot tell a complete answer from a quietly partial one. Each element MUST be a bare qualified identity with
`dataset_id`, `object_id` and `meta_id`, not a tagged value envelope. The requested reference field
already determines the element type; two encodings would make independent clients disagree.

```json
{ "references": [ { "dataset_id": "s-7f3a", "object_id": "8121",
                     "meta_id": "com.example.reading" } ] }
```

### Writes

**Every write is a transaction**: an ordered list of operations that all commit or none do.

```
POST /v1/datasets/{d}:transact        Idempotency-Key: 9f2c…
{ "operations": [
    { "op": "expect_absent", "type": "com.example.reading", "guards": ["entry"],
      "filter": { "field": { "id": "recorded_at", "exact": { "type": "date_time",
                                                             "value": "2026-09-17T08:00:00.000Z" } } } },
    { "op": "insert", "type": "com.example.reading", "fields": { … }, "as": "entry" },
    { "op": "expect", "target": { "type": "com.example.plan", "id": "8121" },
      "version": "v1:9f2c…" },
    { "op": "update", "target": { "type": "com.example.plan", "id": "8121" },
      "fields": { "latest": { "type": "reference", "value": "$entry" } } } ] }
```

**Each operation requires its own grant, and a transaction requires the union.** Object operations
require `write`; `define_type`, `update_type` and `delete_type` require `schema` and **not** `write`,
so a transaction that only changes types needs only `schema`. A transaction doing both needs both.
Checks require the grant their target would need to read.

#### Operations

| `op` | Carries | Effect |
| --- | --- | --- |
| `insert` | `type`, `fields`, `as`? | Creates one object. |
| `update` | `target`, `fields` | Changes the named fields of one object. |
| `delete` | `target` | Removes one object. |
| `update_where` | `type`, `filter`?, `fields` | Changes the named fields of every matching object. |
| `delete_where` | `type`, and `filter`? or `object_ids` | Removes every matching object. |
| `define_type` | `meta` | **Creates** a type. Fails `409` `meta_type_already_exists` if it exists. |
| `update_type` | `meta`, `confirm`? | Changes an existing type. Removing a declared field requires `confirm`, equal to the type ID; see [Changing a schema](#changing-a-schema). |
| `delete_type` | `type`, `cascade`? | Removes a type. Without `cascade`, fails `409` `conflict` with `details.reason: "type_not_empty"` if it has descendants or holds objects. |
| `expect` | `target`, and `fields` or `version` | Asserts. Mutates nothing. |
| `expect_type` | `type`, `schema_version` | Asserts a type is as the caller last saw it. Mutates nothing. |
| `expect_absent` | `type`, `filter`, `guards` | Asserts that nothing matches. Mutates nothing. |

**An operation carries these members and no others.** A member a server does not recognise MUST be
rejected with `400` `invalid_request` naming the operation index, never ignored. Recognition, not
validation, governs [roles and annotations](#recognition-not-validation), which are vocabulary and
inert; it has never governed this grammar, where an ignored member is a caller who believes it asked
for something that will not happen.

**Removing a field is destructive and MUST be confirmed**, in a transaction exactly as through `PUT`:
an `update_type` whose `meta` omits a field the type currently declares carries `confirm` equal to the
type ID, and fails `400` `meta_schema_error` with `details.reason: "confirmation_required"` without it.
See [Changing a schema](#changing-a-schema). A schema is often assembled from a client's own
definition, and a field dropped by a stale copy of that definition would otherwise erase data nobody
decided to erase.

**Checks are operations, not preconditions.** An assertion sits in the list beside the mutations, and
every entry either passes or the transaction fails at that index. One grammar therefore covers
conditional insertion, comparison against an expected baseline, and multi-object writes — all of which
used to be separate operations with their own capability flags. It costs a repeated target between a
check and the mutation it guards, and buys a check that can stand alone and a failure that names an
index rather than inventing a way to address "the precondition of operation 3".

**Order matters, and later operations see earlier ones' effects.** Insert an object and update it in
the same transaction and the update sees the insert.

**A target names the concrete type as well as the identity** — `{"type": …, "id": …}` — because object
IDs are unique within a *hierarchy*, not within a type; see [Object identity](#object-identity). It is
also an assertion: acting on an object that turns out to be a different concrete type MUST fail rather
than succeed quietly.

**`define_type` creates and never replaces.** Two clients racing to create the same type produce one
winner and one `409`, which the loser resolves by re-reading and deciding whether the existing type
serves. A create-or-replace operation would instead let a race silently discard another client's roles
and annotations.

**Aliases.** An operation may name its result with `as`, and a later operation may reference it as
`$name` anywhere a reference value or a target identity appears. Without them a transaction cannot
create a parent and a child that references it, which is the most common reason to want one, since the
identity is assigned by the dataset.

- **`as` is insert-only.** Only an operation that mints an identity may name one, which is `insert`
  alone. Anywhere else it is `400` `invalid_request`.
- **Names are `^[A-Za-z_][A-Za-z0-9_]{0,63}$`** — deliberately narrower than an object ID, so a name
  is readable in a failure that has to point at one.
- **Declared bare, referenced with `$`.** `"as": "entry"` declares; `"$entry"` refers. `guards` names
  the bare name, because it names a declaration rather than standing where an identity would.
- **The prefix is reserved, not merely conventional.** A server MUST NOT mint an object ID beginning
  with `$`. That one rule is what makes `$entry` unambiguous where a target identity appears, since
  object IDs are otherwise almost unconstrained; without it a transaction could address a real object
  by accident. In a reference *value* the alias replaces the whole `{dataset_id, object_id, meta_id}`
  object with the string, so no ambiguity arises there.
- **Request-scoped, and resolved before execution.** A repeated name, an unknown name and a reference
  to an alias declared later are each `400` `invalid_request` naming the operation index, refused
  before any operation runs rather than discovered part-way through.
- **An alias never appears in a result.** The minted identity arrives at its operation's index, which
  is how the client learns it.

**Scope.** `update_where` and `delete_where` are **descendant-inclusive**, with or without a filter, so
they affect the same rows a query would return.

#### The response

```json
{ "results": [ {}, { "objects": [ { "id": "9004", "meta_id": "com.example.reading" } ] },
               {}, { "affected": 1 } ],
  "watermark": "w:8f21…" }
```

One entry per operation, in order. A check contributes an empty object, an insert the identities it
created, a mutation the number of rows it affected. The watermark is the dataset's value **after** the
commit, so a client can compare later reads against it without a further request.

**An operation's result is the same shape wherever it is asked for.** A result entry carries exactly
what the [single-operation form](#single-operation-forms) of that operation returns, so one grammar
covers both and a client parses one thing:

| `op` | Result entry |
| --- | --- |
| `insert` | `{"objects": [ {"id", "meta_id"} ]}` |
| `update`, `delete`, `update_where`, `delete_where` | `{"affected": n}` |
| `define_type`, `update_type` | `{"type": ObjectMeta}` — the **effective** type, after inheritance |
| `delete_type` | `{"deleted_types": [ … ], "deleted_objects": n}` |
| `expect`, `expect_type`, `expect_absent` | `{}` |

A schema operation returns the effective type rather than what was sent, because that is what the
caller now has to write against, and it differs from the request whenever inheritance contributes
anything.

#### When an operation fails

```json
{ "error": { "code": "conflict", "operation_index": 2,
             "message": "Expected values did not match",
             "details": { "object_id": "8121", "mismatched_fields": ["revision"] } } }
```

- **The first failure, and nothing after it.** All-or-none makes evaluating the rest pointless, and
  reporting it would describe a state that does not exist.
- `operation_index` sits beside `code`, because it applies to every code while `details` is per-code.
- **The HTTP status is the one that operation would have returned alone** — `409` for a failed check,
  `404` for a missing target, `403` for permission, `400` for a malformed field — so a client's
  existing mapping carries over.
- **A failure body carries no `results`**, so partial application is never ambiguous.

#### Bounds

One dataset per transaction, since authority and atomicity are both per dataset. At most 256
operations. The existing `max_request_bytes` applies. One `Idempotency-Key` covers the whole request,
under the byte-exact request identity in [Idempotency](#idempotency).

#### Single-operation forms

These are convenience only. Each is defined as a transaction of exactly one operation and behaves
identically, including its effect on the watermark.

| Form | Equivalent |
| --- | --- |
| `POST /v1/datasets/{d}/objects/{t}` — `{"fields": {…}}` | one `insert` |
| `PATCH /v1/datasets/{d}/objects/{t}/{o}` — `{"fields": {…}}` | one `update` |
| `DELETE /v1/datasets/{d}/objects/{t}/{o}` | one `delete` |
| `POST /v1/datasets/{d}/objects/{t}:update` — `{"filter": F?, "fields": {…}}` | one `update_where` |
| `POST /v1/datasets/{d}/objects/{t}:delete` — `{"filter": F?}` or `{"object_ids": […]}` | one `delete_where` |

A single-operation form returns that operation's result directly rather than a `results` array, plus
the watermark. Anything conditional, anything touching more than one object, and anything touching a
type goes through `:transact`.

A write MUST reject a field that is not in the type's effective schema, and a value that
[cannot be accepted](#accepting-a-value-for-a-declared-field) for its declared type.

**`fields` semantics.** Only named fields change, and a `null` value clears one. A field absent from
`fields` is untouched. There is no whole-object replacement operation.

**A `media` value is authorized when it is written, in the transaction that writes it.** Setting a
field to a content reference attaches that item, and both the authorization check and the attachment
relationship MUST commit in the **same transaction** as the record:

```json
{ "fields": { "photo": { "type": "media", "value": "…" } } }
```

There is no binding token in the request, and no preflight. Asking "may this caller use this item" and
then committing the record separately leaves a window in which permission changes between the answer
and the commit; participating in the transaction is what closes it. A function call into a content
package is not sufficient unless it joins that transaction.

- A server MUST refuse the write if it cannot establish authority over the item inside the transaction.
  **A reference that could not be created can never become access**, so a caller who merely learned an
  item ID cannot read someone else's photograph by naming it in a record of their own.
- **This constrains authority, not deployment.** The content authority may be a package embedded in the
  server, and the bytes may live in object storage, on a filesystem or behind a CDN — a byte store is
  not an authority. What v1 requires is that one transaction decides.
- **Byte transfer happens outside the transaction**, and MUST NOT be attempted inside one; see
  [Transactional composition](tessera-embedding.md) and the
  [content protocol](tessera-content-v1.md).
- A deployment whose content authority **cannot** join the record transaction MUST refuse media writes.
  Making that work needs machinery v1 does not define; the analysis is in
  [the annex](annex-remote-content-authority.md), which is experimental and unimplemented.

### Asserting a schema

A record version guards a write against a changed *record*. It says nothing about a changed *type* —
and a write often assumes more about the schema than about any row: that a field still exists, still
has the storage type it had, still carries the role the client resolved it by.

```json
{ "op": "expect_type", "type": "com.example.plan", "schema_version": "s:41c0…" }
```

`schema_version` is an opaque value the server computes from a type's **effective** definition — its
fields, their storage types, and the roles and annotations in force after inheritance. It follows the
same rules as a [record version](#record-versions): computed rather than stored, opaque, server-local,
compared only against what that same server computes later. **A server computes it without
interpreting any role or annotation**; changing an annotation's value changes the version without the
server knowing or caring what changed.

- It is returned with every type in a metadata read, and per dataset in `GET /v1/datasets` as the
  version of that dataset's whole schema, which is what makes a cheap poll possible.
- A mismatch is `409` `conflict` naming the operation index. As with record versions, it means
  **re-read**, not *someone changed this on purpose*.
- **It covers the effective definition**, so a change to an ancestor moves every descendant's version.
  That is deliberate: a client that resolved a field through an inherited role depends on the ancestor
  as much as on the type it named.

This is what a client reaches for when it reuses a type it did not create — deciding an existing type
satisfies its contract, then writing against that decision in the same transaction.

### Asserting absence

`expect_absent` is how a transaction refuses to create something that already exists. Its `filter` is
an ordinary [predicate](#predicates), and the rules that make it safe are not obvious:

- The check is **descendant-inclusive** over the named type's hierarchy. A check confined to the exact
  type would not exclude an equivalent object stored as a descendant.
- **`expect_absent` MUST name what it guards**, by the aliases of later inserts:

  ```json
  { "op": "expect_absent", "type": "com.example.reading", "filter": { … }, "guards": ["entry"] },
  { "op": "insert", "type": "com.example.reading", "fields": { … }, "as": "entry" }
  ```

  `guards` is required and non-empty. Each name MUST belong to an `insert` **later in the same
  transaction**, and that insert's type MUST be the named type or a descendant of it.
- **Every guarded insert MUST itself satisfy the filter.** If the object being created would not match
  the condition it was created under, two concurrent transactions both find nothing, both succeed, and
  the guard excluded nothing. A server MUST reject that request with `invalid_filter` rather than let
  it through.
- Inserts the check does not name are unaffected, which is what lets one transaction create a guarded
  object and an unrelated one of the same type.
- The check and the insertion happen in **one transaction**, which is what the operation is for.
  Checking and then inserting is not an implementation of it.
- Fields named in the filter MUST exist in the type's effective schema, and a `null` value asserts the
  absence of a value, consistent with predicates everywhere else.

### Erasure

```
POST /v1/datasets/{d}:erase      { "confirm": "{d}" }
```

Requires the `erase` grant, which is **never implied by `write` or `schema`** and MUST NOT be included
in an ordinary application's authorization by default. Destroys every object and schema in the dataset.

Whether only an owner may hold that grant is administration policy, not protocol: a server MAY reserve
its assignment. But note what such a policy does *not* buy. `schema` already permits deleting a type,
which cascades to every object under it, and `write` already permits deleting by filter with no filter
at all. **Reserving `:erase` to owners while leaving those two in ordinary hands does not protect a
shared dataset**; a product that wants that guarantee needs policy on every destructive path, not a
narrower erase permission.

**A caller must be able to describe the consequences before causing them**, so the destructive call is
preceded by a preflight that changes nothing:

```
POST /v1/datasets/{d}:erase-preview   {}
→ 200 { "dataset_id": "s-7f3a…", "object_count": 8412, "type_count": 19,
        "affected_principals": 2 }
```

`affected_principals` counts grant-holders other than the caller who lose access to data, so a client
can say whose data is affected. It is advisory and MAY be stale by the time the erase runs.

Erasure MUST NOT accept `Idempotency-Key`; a request carrying it MUST fail `400 invalid_request`
before retirement. It creates no erasure receipt. The confirmed old identity and replacement discovery
below provide recovery without retaining a response in a dataset that has ceased to exist. Ordinary
record and schema mutations still honour the header.

The erase itself:

```
POST /v1/datasets/{d}:erase           { "confirm": "s-7f3a…" }
→ 200 { "dataset_id": "s-91cd…" }
```

- What remains is a **new dataset**, not the old one emptied, and MUST take a newly minted identity. The
  previous identity MUST NOT be reused, by this dataset or any other.
- **Grants do not transfer.** The new dataset starts with whatever grants the server's administration
  assigns it, and at minimum the erasing caller's. A new identity silently inheriting the old
  authority would defeat the reason it is a new identity.
- `confirm` MUST equal the dataset ID the caller believes it is erasing. This doubles as the retry
  guard: after a lost response the old ID no longer exists, so a repeat fails `404` `not_found`
  instead of erasing a freshly created dataset.
- **After a lost response**, the replacement is identifiable rather than guessed: the new dataset's
  entry in `GET /v1/datasets` carries `"replaced_dataset_id"` naming the identity it replaced, for at
  least `idempotency_retention_seconds` after the erase. A caller matches on that, never on a display
  name, which is mutable and need not be unique.
- A caller MUST NOT conclude the erase failed merely because its own dataset identity is gone — that
  is what success looks like. If neither the old identity nor a dataset naming it is present, the
  erase's outcome is genuinely unknown and MUST be escalated rather than repeated.

**On return, access is revoked and deletion is durably scheduled.** That is exactly what success
means here — not that bytes are already gone. One transaction retires the dataset, revokes access,
stops issuing transfer URIs and durably enqueues the deletion of every object the dataset owns. A
worker finishes the physical deletion afterwards, and that part is the server's problem and MUST NOT
become the client's: there is no erasure handle, no status resource and nothing to poll.

**A caller that received a response is done. A caller that did not has established nothing** — it
resolves the replacement through `replaced_dataset_id` above before concluding anything, exactly as it
would for any lost response.

- **The deletion MUST be durable before the response is sent**, committed with the transaction that
  revokes access, retried until it succeeds, and surviving restart and restoration. A server that can
  lose that work has failed the erase whatever it answered, and this is the obligation that lets the
  rest of the contract stay silent.
- **Cleanup MUST reach what an erase can otherwise miss**: an upload whose transfer URI was issued
  before the erase and could write bytes back under a deleted key, derivatives and thumbnails, object
  versions in a versioned bucket where an ordinary delete only writes a delete marker, CDN caches, and
  backups, whose restoration MUST NOT revive the dataset. A server MUST document what its byte backend
  covers.
- **What a client may say.** On return, access is revoked and the dataset's bytes are irrevocably
  committed to deletion. Two things lie outside that and cannot be resolved by waiting: read URIs
  already issued stay valid until they expire — which is why `url_lifetime_seconds` is minutes, and a
  transfer already running may finish — and copies already downloaded are beyond any server's reach.

## Pagination

| Operation | Request | Response | Grant |
| --- | --- | --- | --- |
| Begin | `POST /v1/datasets/{d}/objects/{t}:page` — `{"query": Q, "page_size": N?}` | `200` `{"cursor": C}` | `read` |
| Next | `POST /v1/datasets/{d}/pages/{cursor}:next` | `200` `{"meta": …, "records": […], "next_cursor": C or null}` | `read` |
| End | `DELETE /v1/datasets/{d}/pages/{cursor}` | `204` | `read` |

### Advancing

**A cursor names a position, not a server-side iterator.** Advancement happens by using the
`next_cursor` a page returns, never as a side effect of reading one.

- `:next` on a given cursor MUST return **the same page every time** it is called, for as long as that
  cursor is valid. It is a safe retry, which is what makes a lost response harmless: the client repeats
  the same request and receives the same page and the same `next_cursor`.
- `next_cursor` is `null` when the sequence is complete. A complete sequence's final page MAY be
  empty, and `records` MUST NOT be used as the end condition — a server MAY return an empty page with
  a non-null `next_cursor` when a page's worth of rows was filtered away.
- `page_size` is at most `max_page_size`; omitted means the server's default, which MUST be at most
  that maximum.
- A cursor is opaque, at most 4096 bytes, and MUST carry or reference the complete validated query, so
  continuation cannot drift from what was begun.
- A tampered or unrecognised cursor is `410` `invalid_cursor`; an expired one is `410` `cursor_expired`.
  A client recovers by starting the query again, and MUST NOT assume it received every row.
- A cursor is bound to the **principal, that principal's application authorization, and the generation
  of the resulting authorized view**. Two installations of one person may see different data, so the
  principal alone is not enough.
- **The generation MUST change when the caller's effective access changes**, including grant
  revocation or expiry and changes to application authorization. v1 visibility depends on grants,
  never record fields; editing a field MUST NOT change the view generation. Otherwise an ordinary
  data write would masquerade as a permission change, while a revoked grant could leave a cached
  page replayable.
- The generation MUST be re-checked **before serving any page, including a replay of one already
  issued**. A stale cursor is `410` `view_changed`, which MUST NOT indicate which records appeared or
  disappeared. A client restarts the query and MUST NOT assume it received every row.
- `DELETE` releases the whole sequence, including cursors already handed out. It is advisory, and
  servers MUST tolerate a client that never calls it.

### What a sequence guarantees

**The protocol does not promise a snapshot.** Holding one for a cursor's lifetime would mean holding a
transaction open across client think-time, and materialising the whole result instead would defeat
paging. What a sequence guarantees is therefore narrower, and stated exactly:

1. **No row is returned twice**, whatever happens to it mid-sequence, under any selection. A server
   continuing by key MUST either snapshot or exclude identities it has already returned.
2. **Under `selection: all` only:** a row that exists throughout the sequence, matches the filter
   throughout, and whose sort values do not change during it, is returned exactly once. Deletions of
   *other* rows MUST NOT cause it to be skipped — that is the offset-drift failure, and it is forbidden.
3. Everything else is unspecified: a row inserted, deleted, or whose sort values change during the
   sequence MAY or MAY NOT appear. A row moved backwards past the continuation point will typically be
   missed, and a client MUST NOT infer anything from its absence.

**Guarantee 2 cannot extend to grouped selection.** Under
`first_per_concrete_type`, whether a row survives selection depends on the *other* rows of its concrete
type, so a row that changes nothing about itself can be displaced by one that did. A stable row is
therefore not promised at all under grouped selection; only guarantee 1 applies, and a client paging a
grouped query is reading a best-effort view by construction.

A client that needs guarantee 2 to cover its rows sorts on values that do not change — a creation
timestamp rather than a mutable one — and a client needing a true point-in-time view obtains it another
way. A server MAY implement a real snapshot and will then exceed this contract; clients MUST NOT depend
on that.

**What guarantee 2 requires of an implementation.** These are the conditions under which keyset
continuation actually delivers it, and a server that compensates in its own process MUST meet all four:

- Scanning follows the query's **global** order, including the object-ID tie-break — not the order of
  whatever chunk the store happened to return. Sorting individual chunks drawn from an otherwise
  unordered store does not satisfy this; compensating in-process needs a globally correct strategy, or
  an explicit failure at a declared bound.
- The continuation boundary advances only past rows actually **examined**. Matches beyond a full page
  are retained or left ahead of the boundary, never discarded.
- Identities already returned are excluded, and an issued page is replayed from preserved results
  rather than recomputed.
- Exhaustion is established from the **source scan**, not from an empty filtered page — which is why an
  empty page may still carry a non-null `next_cursor`.

## The watermark

Every response to a read, and every transaction response, carries the dataset's watermark:

```json
{ "watermark": "w:8f21…", "record_set": { … } }
```

It answers one question — *has anything in this dataset changed?* — and it is the only change-tracking
mechanism in v1. There is no per-object sequence, no tombstone and no change feed.

- **It changes when anything in the dataset is committed**, and is stable while nothing is. That
  includes **schema**: creating or changing a type, its roles, its annotations, or anything a
  descendant inherits from it, MUST move the watermark. A client caches metadata as readily as
  records, and a schema change that left the watermark still would leave that cache silently wrong.
- **It is opaque.** A client compares it for equality and MUST NOT order, subtract or parse it. That
  is not politeness: an opaque value lets a server encode the dataset generation, so a watermark from
  before an erase or a restore is recognisably foreign rather than silently plausible, and it stops a
  caller with a narrow view counting gaps to infer how much is happening elsewhere.
- **A watermark from another dataset is meaningless.** They are not comparable, and a client MUST NOT
  try.

**Where it goes.** `watermark` is a **top-level member of the response body**, a sibling of the data it
describes — never nested inside `record_set` or any other structure. That holds for a record read, a
query page, a transaction response, an operation status and an erase preview alike, so a client reads
it from one place regardless of what it asked for.

- **No body, no watermark.** A response that carries no body carries none, and a server MUST NOT
  invent a header for it. `HEAD` answers what `GET` would answer minus the body, and that includes
  this.
- **An error carries none**, because an error describes no data. A failed transaction changed nothing,
  and pairing a watermark with a failure would invite a client to treat it as a state it had observed.

**A replay carries the watermark of the answer being replayed.** A replayed response — an idempotent
repeat, a receipt body, a cursor page served from a retained result — is the original answer, and its
watermark is the one that was current when that answer was produced. A server MUST NOT substitute the
current value. Historical data beside a current watermark is the one thing this mechanism must never
say: a client would conclude the dataset had not moved since data it is holding from an earlier state.

**A transaction that changes nothing still answers with a watermark.** A check-only transaction and a
commit whose operations matched nothing are genuine observations of dataset state, and they return the
value current at commit — which they do not advance, having committed no change.

**What it is for.** A client that reads several times to build one answer — a chart from six queries —
has no other way to know whether the data moved underneath it. If every response carries the same
watermark, those reads are mutually consistent. If one differs, the client re-reads rather than
publishing a mixture of two states. It is equally the version a cache validates against: without it a
client cache is stale data with no way to know.

**What it is not.** It cannot be resumed from, so it is not a feed; it says *something changed*, never
*what*. And it is per dataset, so a write anywhere moves it — it is useless as a per-record guard and
would manufacture conflicts between callers editing unrelated rows. That is what
[versions](#record-versions) are for.

**There is no snapshot across datasets.** Reads are consistent within one dataset and never across
two, and no protocol can offer otherwise without a distributed commit. A client reading from several
datasets to build one answer MUST treat the result as a mixture and say so if it matters.

## Record versions

A **version** is an opaque value the server derives from a record's current values. It exists so a
write can depend on what was read — see [`expect`](#what-expect-compares).

```json
{ "id": { "id": "8121", "dataset_id": "s-7f3a…" }, "meta_id": "com.example.reading.temperature",
  "version": "v1:9f2c…", "values": [ … ] }
```

- **Computed, never stored.** A dataset holding a run of ten thousand GPS points, written once and
  never edited, pays nothing for a guard none of them will use.
- **Opaque and server-local.** The algorithm is the server's business and is never reproduced
  elsewhere, so no canonical form has to be defined: the only comparison that ever happens is between
  a value a server gave out and what that same server computes later. Two implementations never need
  to agree, and a client MUST NOT parse one or compare versions from different servers.
- **It covers all of the record's values**, inherited and declared alike, since a record of a child
  type is one row. It does **not** cover the schema — hashing field definitions, roles or annotations
  would invalidate every outstanding version in the dataset on a single annotation edit.
- **A version MUST change when the record's values change. It MAY change otherwise** — a schema
  change, a server upgrade that alters the algorithm.
- **Therefore a mismatch means *re-read and retry*, never *someone edited this*.** A client that
  reports a concurrent edit on the strength of a mismatch will eventually tell a person their data was
  modified elsewhere when it was not, which they have no way to check. Compare the values before
  saying anything.
- **A version guards the whole record.** Two callers editing different fields will conflict, where
  field expectations would have merged cleanly. Use a version when a whole record was read and
  reasoned about, or when a field holds a compound value; use field expectations when disjoint edits
  should merge. They may be combined in one transaction.
- A query through an ancestor type returns versions reflecting fields that projection cannot show, so
  a change to a descendant-only field conflicts invisibly. Prefer field expectations when deliberately
  reading through a parent.

**Requesting versions may cost more than the projection suggests.** A version covers the whole record,
so asking for two fields plus versions still requires the server to read the rest. A version computed
over only the projected fields would give the same row different versions to different readers, which
is worse than useless.

## Write safety

Every mutation may lose its response. These mechanisms make that recoverable rather than a guess.

### Idempotency

`:transact` and the single-operation forms MAY carry `Idempotency-Key: <uuid>`. A server MUST honour
the header on every record or schema mutation. A read — a query, a count, an aggregate, a
reference listing — carrying it MUST fail `400 invalid_request` with
`details.reason: "unexpected_idempotency_key"`: there is no outcome for the key to recover, and
naming the header points the caller at it. Erasure is the explicit exception: its
confirmation and replacement discovery are its recovery mechanism. Recovery from a lost response is
not something a client can reconstruct — which is why it is required rather than advertised.

Extending the header past inserts is deliberate. Without it, an atomic update whose response is lost
cannot be retried: the same request now fails its own expected-field check, and the caller cannot tell
its own success from another writer's change. See
[Recovering any mutation](#recovering-any-mutation).

**Returning the stored row.** A request MAY carry `Prefer: return=representation`
([RFC 7240](https://www.rfc-editor.org/rfc/rfc7240.html)). Where a server applies it, each insert's
result also carries `record_set`: the row as a read of it would return, its own concrete type and
its version included. A server that applies it MUST answer `Preference-Applied: return=representation`,
and a client MUST NOT infer from the header alone — the absence of `record_set` on an insert is the
answer, whatever the headers said.

It is a preference and not an instruction. A server MAY decline, and MUST decline for a caller that
may write and not read: a representation handed to somebody who may not read it is a disclosure
rather than a convenience. Declining means sending neither the member nor the header, so a client
learns to keep reading back rather than silently losing the row it asked for.

`objects` is unchanged by this and is always present. The identity is what every caller of an insert
needs and the representation is what some do; both are built from one object inside one transaction,
so they cannot disagree.

This exists because the alternative is two round trips for every record a person writes, and the
second one adds nothing but latency to the most latency-visible thing a client does.

**Scope.** A key is scoped to one dataset: the pair `(dataset, key)`. It is *not* scoped per type, so
the same key sent to two types collides deliberately rather than creating two objects.

**Receipts have an owner, and the owner outlives its access.** Authorization here is ownership of the
receipt, not a grant held now: a record binding a key to its outcome retains
the principal and application authorization that claimed it, together with enough scoped metadata — the
dataset, the type and the resulting identity — to answer a recovery question after the record itself is
gone. Authorization differs by outcome, because the outcomes are not equally revealing:

| Outcome | Who may see it | What they receive |
| --- | --- | --- |
| No record (`unknown`) | Any authenticated caller | `unknown`, which means **no retained claim right now** — not that the request never began. Ownership cannot be checked for a key nobody claimed, and the answer discloses nothing, so requiring a grant would only lock out the owner of a request still in flight. |
| `in_progress` | The owner | The state alone. No record exists yet, so no record authority can be required. |
| `failed` | The owner | The state and the original error, which may name fields. Owner-only for that reason. |
| `committed` | The owner | The resulting **identity**, always. The stored response **body** only while the owner currently has **read** authority over the record. |

Splitting identity from body is what makes the awkward cases reachable. An owner whose access was
withdrawn still learns that its write committed — which is what stops it writing a second object — but
receives no content it may no longer read. An owner whose record was since deleted learns the same
thing from the retained metadata, rather than being unable to complete a recovery because the evidence
was removed.

- A caller that is not the owner MUST receive `403` `permission_denied` for any outcome other than
  `unknown`, whether or not the key exists. Holding a key is not evidence of anything.
- Where the body is withheld, the response MUST say so explicitly rather than returning an empty one,
  so a client cannot read absence as a result.
- **No refusal releases the key or re-executes the write.** The claim stands, so a later authorized
  replay still returns the original outcome rather than creating a second object.

**Request equivalence is byte-exact.** A request's identity is the digest of an unambiguously framed
tuple:

1. the HTTP method,
2. the request target (path and query),
3. the exact bytes of the request body.

Framing MUST make the three unambiguous — for example by prefixing each with its length — so that no
two different tuples can produce the same input. Two requests are the same request when their digests
match.

- **The body is digested as received**, after HTTP transfer framing is removed. A server that accepts a
  content encoding MUST digest the decoded bytes; a server that does not MUST reject an encoded body
  rather than digest a compressed one.
- **Credentials are excluded.** Headers are not part of the identity, so refreshing an access token
  between an attempt and its retry does not change what is being retried.
- **A client retrying MUST send byte-identical body bytes**, and therefore MUST retain the serialised
  body alongside the key until reconciliation is finished. Re-serialising from a model is not
  sufficient: reordered members or different whitespace are deliberately a *different* request and
  MUST fail `409` `conflict` with `details.reason: "idempotency_key_reused"`.

No canonicalisation is defined, and that is the point. Any canonical form must answer what to do with
duplicate members, Unicode normalisation, `1e0` versus `1`, `-0`, and a malformed body that has no
valid type tag to disambiguate its numbers — and it must answer identically in every implementation,
including for the malformed requests whose failures are themselves replayable. Comparing bytes has none
of those questions. It costs a client only the buffer it already needs in order to retry.

Independently of digesting, a server MUST reject a request body containing duplicate JSON member names
with `invalid_field`, and MUST do so **before claiming the key**, together with every other failure of
framing, authentication, authorization and request parsing. A key is claimed once the request is
understood well enough to attempt it; failures before that point leave no record and are not
replayable.

- A repeat with a **matching** digest MUST NOT re-execute, and is answered under the same per-outcome
  authorization as `:status` above, with the header `Idempotency-Replayed: true`. An owner still
  authorized over the record receives the original response and status, an original *failure* included.
  An owner no longer authorized receives the identity-only form below instead of the stored body.
- A repeat with a **different** digest MUST fail `409` `conflict` with
  `details.reason: "idempotency_key_reused"`.
- Deleting the created object MUST NOT release the key.
- Keys MUST be retained for at least `idempotency_retention_seconds`, and that value MUST be at
  least **86400** — twenty-four hours.

  The floor exists because the rest of this section is otherwise unenforceable. Retention was
  required to be at least the advertised value and the advertised value was unconstrained, so a
  server could advertise sixty seconds, keep its word, and leave a client with no way to reconcile
  a lost response — which is the only thing a key is for. A bound that can be set low enough to
  void the guarantee it bounds is not a bound. A day rather than an hour because the client that
  needs this most is on a phone, and the gap between losing a response and being able to ask about
  it is a night's sleep.

**Concurrency.** A server MUST NOT execute the same key twice concurrently. A second request arriving
while the first is unresolved either waits for it and then replays its result, or fails `409`
`conflict` with `details.state: "in_progress"`, which a client MAY retry.

**Atomicity.** The record binding a key to its outcome MUST commit in the **same transaction** as the
write it describes. A crash between the two would otherwise let a retry duplicate a committed write —
the exact failure the key exists to prevent.

**Claiming.** Before beginning the write, a server MUST durably record the key as `in_progress` in its
own committed transaction. This is what makes an absent record meaningful: see below.

### Reconciling an unknown outcome

```
POST /v1/datasets/{d}/operations/{key}:status
```

**Receipt ownership is the authorization, not a current grant.** The caller MUST be the principal and
application that claimed the key; a caller that is not gets `403` `permission_denied` whether or not
the key exists, however broad its grants. No grant substitutes for ownership, and **ownership does not
expire with access**.

That last part is the whole point and it has to be said plainly, because the obvious rule breaks it.
Requiring a current `write` grant would lock out exactly the two callers who most need to reconcile: an
owner whose access was withdrawn between the write and the question, and the owner of a transaction
that only changed types, which requires `schema` and not `write`. Both made a write whose outcome they
cannot otherwise learn, and both would be told they may not ask about their own request.

**Who sees how much is the per-outcome table in [Idempotency](#idempotency)** — current *read*
authority over the resulting record gates the stored **body**, never the answer itself. So a revoked
owner still learns that its write committed, and still receives the identities it created, which is
what stops it writing them a second time. Returns one of:

| `state` | Meaning | What the client may conclude |
| --- | --- | --- |
| `committed` | The write committed. `result` carries the original success body, so a batch returns every identity it created. | The work is done. Do not repeat it. |
| `failed` | The write was attempted and definitively did not commit. `error` carries the original error body. | It did not happen. Fix the request; a bare retry will fail the same way. |
| `in_progress` | The key was claimed and the outcome is not yet recorded. | **Nothing yet.** The operation may still commit. |
| `unknown` | The server holds no record of this key **right now**. | See below. It is an observation, not a verdict. |

`result` is **the original success body, whatever shape that was**, and the two shapes differ. A
single-object write replays as a record set:

```json
{ "state": "committed", "result": { "record_set": { "meta": …, "records": [ … ] } } }
```

A transaction replays as a transaction result, which is what most receipts are — every keyed write a
client makes through `:transact` comes back this way:

```json
{ "state": "committed",
  "result": { "results": [ {}, { "objects": [ { "id": "9004", "meta_id": "com.example.reading" } ] } ],
              "watermark": "w:8f21…" } }
```

**The two shapes qualify identities differently, and a client MUST NOT assume one of them.** Inside a
transaction result, an object carries `id` and `meta_id` and no `dataset_id`, because the request
already named the dataset and repeating it in every entry would say nothing. The withheld form below
carries all three, because there is no body to take the dataset from. A client reading a receipt
therefore qualifies a transaction result's identities with the dataset it asked about, and takes a
withheld identity as it stands.

Both of these have been got wrong by a shipping client that implemented against one example and met
the other, so a server MUST NOT narrow either shape and a client MUST be able to decode both.

When the body is withheld, the response still carries the identities and says plainly that it is
withholding, so a client cannot read absence as a result:

```json
{ "state": "committed",
  "objects": [ { "dataset_id": "s-7f3a…", "object_id": "8121",
                 "meta_id": "com.example.reading.temperature" } ],
  "result_withheld": true }
```

Each identity is **type-qualified**. `(dataset_id, object_id)` alone is not an identity — object IDs
are unique within a hierarchy, not within a dataset — so a receipt that omitted `meta_id` would hand
back something the caller could not safely use.

`result_withheld` carries **no reason**, and that is deliberate. A caller acts identically whether the
record was deleted or has merely left its view: the write committed, these are the identities, do not
write them again, and do not read the missing body as a result. Giving the two cases different names
would oblige a server to decide whether this caller may learn of an absence — making the receipt a
place where a deletion could be disclosed — in exchange for a distinction no client branches on. A
caller that wants to know whether its object still exists holds the identity and can ask.

Exactly one of `result` and `result_withheld` is present. A batch returns every identity it created
either way — that is what a caller needs in order not to write them a second time.

**A transaction does many things, and the receipt is one answer about all of them.** A receipt retains
the response body as it was sent, together with the identities the transaction created. Whether the
body comes back is decided **once, for the whole body**, by whether the caller can currently read
everything it describes:

- **Every part readable now** — the stored body, verbatim, including its original watermark.
- **Any part not** — the identity-only form, with `result_withheld: true`. One partly filled body is
  never assembled: a caller cannot act on an answer whose gaps it cannot see, and deciding
  per-operation would make the receipt a place where a deletion or a narrowed view is disclosed.
- **A schema-only transaction** replays in full to an owner with current `schema` or read authority
  over the dataset. Its body describes types, which are not anybody's records.
- **A delete-only transaction** has identities and no values, so there is nothing to withhold: the
  owner learns it committed and which identities are gone.
- **A created object since deleted** puts the whole body in the withheld form, by the rule above. The
  identity survives, which is the part that stops a second write.

**All-or-none is about effects, not about the answer.** The transaction's changes commit together or
not at all; the receipt is a record of what the server answered, and it outlives the objects that
answer described. A revoked owner, a deleted record and a schema-only writer all still get an answer.

**`unknown` does not mean the operation failed, and never did.** It means only that no record exists
at the moment of the question. Two things can produce it besides an operation that never arrived:

- A request still in flight that has not yet reached its claim. It may claim and commit afterwards.
- A record that has already been discarded after its retention window, including one that committed.

What the claim *does* establish is narrower and still useful: **no write bearing this key began before
the claim**, so `unknown` plus the client's own knowledge that it is still inside the retention window
means a **same-key retry is safe**. Either the retry claims first and the delayed original is then
rejected as concurrent or replayed, or the original claims first and the retry replays it. Deduplication
is what protects the client, not the status answer.

Outside the retention window, `unknown` establishes nothing at all, and a same-key retry is no longer
deduplicated — it would create a second object. A client that cannot complete reconciliation inside the
window MUST escalate to a human rather than retry.

- **`in_progress` is not failure.** A client MUST NOT retry under a different key, and MUST NOT report
  the operation as failed, while a status is `in_progress`. It waits, or retries the same key, which
  replays or is rejected as concurrent.
- A client MUST NOT treat `unknown` as permission to retry a payload *without* a key, and MUST NOT
  infer an outcome from elapsed time.

**Retention timing.** The window runs from the moment the key is **claimed**, not from when its
outcome was recorded, and is at least `idempotency_retention_seconds`. A server MUST NOT discard a
record that is still `in_progress`: it resolves each to `committed` or `failed` during recovery, and an
unresolvable record stays `in_progress` rather than being optimistically closed or swept.

### What `expect` compares

An `expect` operation compares what a caller believes with what is stored, **inside** the transaction
that writes.

- Expectations include explicit nulls: expecting null asserts the field is absent.
- A missing or stale object MUST fail before any write occurs.
- A mismatch MUST fail with `409` / `conflict` and MUST name the fields that differed:

```json
{ "error": { "code": "conflict", "message": "Expected values did not match",
             "details": { "object_id": "8121", "mismatched_fields": ["updated_at"] } } }
```

Naming the fields is what lets a client distinguish a stale baseline from a technical failure.

- All entries commit or none do. This is a guarantee about the submitted rows only — not about query
  membership, and never across datasets.

### Recovering any mutation

**A key and a version answer different questions**, and conflating them is easy because both sound
like *did something change*:

- An **idempotency key** identifies *one attempt by this client*. It exists so a lost response does
  not become a second write.
- A **[version](#record-versions)** identifies *the state of a record on the server*. It exists so a
  write can depend on what was read.

Which yields a rule that keeps keys off most requests: **a guarded update is already idempotent, where
the value it wrote identifies the attempt.** If the transaction committed and the response was lost,
the record's version has moved, so a blind retry with the old expectation fails `409`, the client
re-reads, and finds its own change already applied. No duplicate, and no key was needed.

**Where the value does not identify the attempt, it is not enough**, and these limits are the client's
to know rather than the server's to fix:

- A version is derived from values, so a record changed A → B → A carries the version it had before,
  and an update assigning the value a field already holds moves nothing. Re-reading `B` cannot say who
  wrote it. Where *which attempt succeeded* matters, carry a key.
- An **unguarded** update retried after another writer's change overwrites it, because an absolute
  assignment has nothing to compare.
- A **filtered** write retried later applies to whatever matches then, which may include rows that
  arrived in between.

Computed versions are not weakened by any of this — a stored revision answers none of it either, since
it too would say only that the record is as expected, not who made it so.

**The key is for creation.** An insert has no prior state to expect, so a lost response plus a retry
means two objects. `expect_absent` substitutes only where some condition uniquely identifies the row.

> **A transaction containing an insert carries an `Idempotency-Key`. A transaction of guarded updates
> need not.**

Writes that set absolute values are naturally idempotent and need nothing: `update` and `update_where`
assign rather than apply a delta, `delete` and `delete_where` leave the same rows absent, and
`update_type` replaces. Repeating one of those after a lost response is safe on its own. `define_type`
is the exception among schema operations — a repeat answers `409` `meta_type_already_exists`, which is
an unambiguous report rather than a duplicate.

#### The trap

A failure of framing, authentication, authorization or parsing happens **before** the key is claimed —
a malformed operation, an unknown `op`, an alias used before it was defined — so it leaves no record
and is not replayable. **A failed check happens during execution**, so the key *is* claimed and bound
to that failure. Send the same key again and the same conflict comes back, forever.

Which means the natural instinct — re-read, correct the expectation, retry with the same key — fails
with `idempotency_key_reused`, because the body changed:

```
POST …:transact   Key: aaaa-1111   expect version v1:9f2c…  → 409 conflict, mismatched
POST …:transact   Key: aaaa-1111   (identical body)         → 409 the same conflict, replayed
POST …:transact   Key: aaaa-1111   expect version v1:4e08…  → 409 idempotency_key_reused
POST …:transact   Key: bbbb-2222   expect version v1:4e08…  → 200
```

**A key identifies one attempt, not one intention.** After a conflict the client is making a genuinely
different attempt, with a different expectation, so it takes a different key. Both failures above
carry the code `conflict` and are distinguished by `details.reason`; a client that branches wrongly
there loops forever.

Without a key on an operation that needs one, the recovery guarantee is narrower and MUST be described
as such: a conflict or a missing object after a lost response is **not** evidence about which writer
caused it.

## Errors

```json
{ "error": { "code": "query_result_limit_exceeded",
             "message": "Distinct reference limit exceeded",
             "details": { "reason": "distinct_reference_limit", "maximum": 256 } } }
```

`code` is stable and machine-readable; `message` is diagnostic and MUST NOT be parsed. `details` is
optional and its keys are per-code.

**A body sent to a route that takes none fails `400 invalid_request`** with `details.reason:
"unexpected_body"` — continuing a page, reading one record, erasing. Not `invalid_field`, which says a
field was named that does not exist and invites a client to conclude something about the dataset.

For `query_result_limit_exceeded`, `details.reason` MUST be present when the failure is a sum
overflow (`"sum_overflow"`), more groups than `max_aggregate_groups` (`"group_limit"`), more
distinct references than the requested `maximum_count` (`"distinct_reference_limit"`), a read
that would materialise more than `max_materialized_bytes` (`"materialization_limit"`), or a
transaction with more operations than `max_transaction_operations` (`"operation_limit"`). These are
`422` failures. `dataset_full` carries `"materialization_limit"` when a write needs more in memory
than that bound, so a client that branches on the reason handles both sides of that bound with one
branch while the code still tells a refused read from a refused write, and `"storage_limit"` when
the write would pass the dataset's `max_stored_bytes` quota.

**`details.maximum` names the bound that was exceeded, and is present only where a bound was
named.** It accompanies `group_limit` and `distinct_reference_limit`. It is **absent** for
`sum_overflow`, because nothing was declared: what was exceeded is the range of the value type, not
a limit this server chose. A client that models `maximum` as always present cannot represent that
difference and will either invent a bound or discard the reason along with it. If several bounds are exceeded, the server MAY report any one it has established; it need
not finish the query to discover the others. This does not require a reason for other uses of this
code, including declared scan bounds and oversized requests. Clients MUST still handle the code
when a reason is absent or unrecognized.

| HTTP | Codes |
| --- | --- |
| 400 | `invalid_request` — the request is not a well-formed instance of its grammar: an unknown or missing member, an alias that does not resolve. `invalid_field`, `invalid_field_value`, `invalid_filter`, `invalid_identifier`, `invalid_type`, `invalid_name`, `meta_invalid_field`, `meta_schema_error` |
| 401 | `authentication_failed` |
| 403 | `permission_denied` — the caller lacks the authority. `not_supported` — the server does not implement an optional capability |
| 404 | `not_found`, `meta_type_not_found`, `meta_field_not_found` — absence. `unavailable` — outside the caller's authorized view, revealing nothing about existence |
| 409 | `already_exists`, `meta_type_already_exists`, `conflict` |
| 410 | `cursor_expired`, `invalid_cursor`, `view_changed` — the caller's authorized view changed; restart the query. Reveals nothing about what moved |
| 413 | `query_result_limit_exceeded` when a request is too large to accept |
| 422 | `unsupported_query`, `query_result_limit_exceeded` when a result or scan exceeds a declared bound |
| 429 | `rate_limited`, with `Retry-After` |
| 500 | `internal_error` |
| 501 | `not_implemented` |
| 507 | `dataset_full` — the write would pass the dataset's storage quota (reason storage_limit), or needs more of the dataset in memory than this server will materialise (reason materialization_limit). Not the caller's mistake and not transient: no retry and no smaller request clears it |
| 503 | `dataset_unavailable` |

A transport failure is the client's to classify; a server never reports one.

### Absence, refusal, and what a caller cannot see

Three distinct answers, because a client acts differently on each.

**`404` `not_found` means the thing does not exist.** A client reads it from a reachable dataset as
*this object was deleted*, and may say so to a person. A server MUST NOT answer it for an object the
caller merely may not see — that tells someone their data is gone. For the same reason a dataset the
caller may not use is absent from `GET /v1/datasets` rather than listed.

**`403` `permission_denied` means the caller lacks authority for this operation**, on a resource whose
existence is not in question.

**`404` `unavailable` means the record is outside the caller's authorized view**, and says nothing
about whether it exists. It is returned *regardless* of underlying existence, so it discloses nothing,
and a client MUST NOT treat it as evidence of deletion.

**Nor may a client show the difference to a person.** `unavailable` covers both a thing that never
existed and a thing belonging to somebody else, so an interface saying *you do not have access to
this* has confirmed that the reference points at something real — which is exactly the existence the
server declined to assert. The concealment is undone at the client, however carefully it is worded.

So a client has two obligations and they point the same way: treat neither answer as evidence, and
surface neither distinction. **Collapsing the two in everything a person can see is correct**, not a
shortcut — and a client that keeps them apart internally must be able to say what it does with the
difference that does not reach an interface. A proposal to give clients a richer code so they can
explain better is undoing this property rather than refining it, and will arrive looking like a
usability improvement: the person asking will be right that the answer is unhelpful, and unhelpful
is what it is for.

**Which of the two a server owes depends on whether the caller is authorized to learn absence.** A
caller is so authorized when its view is defined independently of the row — a whole dataset, or a
partition the caller holds outright — because then the absence of a row is a fact about the caller's
own dataset. Where the view is defined *by* the rows in it, an absent row generally cannot be shown to
have belonged to that view, so the server MUST answer `unavailable`.

Concretely: a server with no sharing answers `not_found` for an identity that is not there, exactly as
before. A deployment with row-level views answers `unavailable` for anything it cannot place inside the
caller's view, and `not_found` only where absence is itself within the caller's authority to observe.

#### A dataset addressed directly

The rules above govern a record inside a dataset. A **dataset identity** in a request path is
answered on its own terms, because the listing conceals which datasets exist and a direct address
must not undo that.

| The identity names | The answer |
| --- | --- |
| A dataset this caller may use | The operation's own answer, `403` `permission_denied` included |
| A dataset this server has **retired**, and can prove it | `404` `not_found` |
| Anything else — a live dataset this caller may not use, an identity naming nothing, or a retirement the server cannot prove | `404` `unavailable` |

**Receipt recovery is exempt from all of this.** `POST /v1/datasets/{d}/operations/{key}:status`, and
a keyed replay of a mutation, name a dataset in the path but are not questions about that dataset:
they ask what happened to an operation, and **a receipt's owner outlives its access**. A server MUST
answer them under the receipt's own ownership rule — the owning principal and application
authorization — without first requiring current access to the dataset named, and MUST NOT answer
`unavailable` merely because that access has since been withdrawn or the dataset erased.

This is the same rule [creating a dataset](#replaying-a-creation) states one level up, and it fails
the same way if it is missed: a caller that lost its grant between a write and its lost response would
be unable to learn the outcome, which is precisely what a key exists to prevent. The receipt rules
themselves are in [Recovering any mutation](#recovering-any-mutation) and decide what such a caller
sees — an identity-only form rather than a withheld body — not whether it may ask.

**`not_found` for a dataset means proven retirement and nothing else.** A server MUST NOT infer it
from the absence of a live dataset, and MUST NOT answer it where its evidence of retirement is
unavailable; there it answers `unavailable`, which is always safe because it discloses nothing.

**A conformant server MUST keep durable retirement evidence**, surviving restart and restore. This is
a requirement, not a choice between two conformant behaviours: if a server could decline the
obligation and answer `unavailable` uniformly, then no client could ever rely on `not_found` meaning
retirement, because any given server might be the one that declined — and a guarantee that holds only
sometimes is not one a client can act on. The `unavailable` answer in the third row is for a server
that *has* the obligation and cannot satisfy it for a particular identity, not for one that never took
it on.

**What this deliberately discloses.** An authenticated caller holding a retired identity learns that
it was retired. Current existence stays concealed: a live dataset it may not use is indistinguishable
from an identity that names nothing. The narrow disclosure is accepted because retirement is
irreversible and a client needs positive evidence before telling a person their data is gone — which
is the confusion this whole section exists to prevent. Concealing retirement as well would require
retained evidence of who may learn about a retirement, which v1 does not define.

**Resolving content is governed by the [content protocol](tessera-content-v1.md) and is unchanged by
this section.** A retired store answers rather than disappearing, through the ordinary three answers
above. The rule here is about a *dataset identity in a request path*; it does not extend to item
resolution, where the question asked is a different one.

#### The authorized view

**Sharing is an advertised capability.** A server without it has one view per dataset, equal to the
dataset, never returns `unavailable`, and can ignore the rest of this section. Everything below
describes what a server advertising `sharing` owes.

**A grant is a row**: a principal, a scope, a set of rights, and an expiry. The scope is a **type** or a
**single object**. Grants are server-side state, created however a deployment chooses — this protocol
does not define a way to create one, only what an existing one means.

**The owner's path is not filtered.** A caller reading its own dataset is subject to no view logic at
all. That is what keeps sharing from taxing the deployments that do not use it.

**"Owner" means a caller holding `write` on the dataset, and that is not the same thing as a grant.**
Two mechanisms are easy to confuse:

- **Access** — `read`, `write`, `schema`, `erase` — is what a caller may do with a dataset it is
  entitled to. It is account state, set by whatever administers the deployment, and it is how an owner
  and their applications work.
- **A grant** extends access to *another* principal, and in v1 conveys `read` or
  [`insert`](#insert-grants), each scoped to types, and never both through one scope.

So a **service** authenticating with `client_credentials` is an ordinary principal: it may hold `write`
on datasets it is entitled to — its own, or ones a deployment assigns it — and it may be granted read
on someone else's. What it cannot do is be *granted* write on a dataset owned by a person, because no
grant conveys write. It may be granted `insert`, which adds and conveys nothing else. A service that must write on a person's behalf is either writing in a dataset it
holds `write` on, or it is not writing at all.

This matters for a hosting deployment. Holding a person's dataset does not make the host a writer in
it: if the host's service needs to record something, it does so in a dataset of its own and shares it,
which is the same delivery path everything else uses. How a deployment decides who holds what is
outside this protocol — see [the deployment contract](tessera-deployment.md).

**Visibility comes from the grant table, never from field values.** This is the rule that makes the
rest cheap: because no stored value determines who sees what, **no write can move an object into or
out of anyone's view**, and a mutation never has to be authorized against two states.

- **Rights are `read` and `insert` in v1.** A guest may not update or delete, and may insert only
  through an [insert grant](#insert-grants), which conveys no view. The only writer with any hold on
  a record is the dataset's owner, and the cases that look like guest writes of that kind — a coach
  composing a plan, a comment on someone's record — are served by writing in one's *own* dataset and
  granting read on it, with the recipient copying what they accept. The recipient then owns it
  outright, and keeps it if the relationship ends.
- **Queries run against type grants; object grants serve direct reads.** A guest may query a type it
  holds a grant for, and may fetch by identity an object it holds a grant for. A query is not filtered
  down to a scatter of individually granted objects — that would put per-row view evaluation into the
  query engine of every deployment, to serve a case that reads by identity anyway.
- **A type grant covers only the named concrete type**, never its descendants. An existing or newly
  created subtype requires its own type grant; creating a subtype MUST NOT silently widen a guest
  view. Descendant-inclusive queries and counts intersect their type scope with the caller's exact
  type grants before grouping, counting, pagination and limits. An object grant still authorizes
  only direct reads and MUST NOT add rows to that intersection.
- **Individually granted objects are therefore not discoverable through this protocol.** A recipient
  cannot query a shared type to find what was shared with them, because that type holds other
  principals' objects too. Whoever grants must also tell the recipient what it granted — by placing
  the objects in a dataset the recipient may query, or by handing over identities some other way.
  Tessera deliberately has no inbox: an application knows who it shared with and why, and a storage
  protocol inventing that concept would be inventing workflow.
- Queries, counts, aggregates and distinct references expose only records inside the view, and MUST be
  computed **within** it — applied before grouping, aggregation, pagination and limits, never as a
  filter over results already gathered. A count that includes invisible rows discloses membership
  without returning one.
- A direct lookup outside the view returns `404` `unavailable`. A server MAY return it for any lookup
  it cannot answer without disclosure.
- **References do not cascade.** A shared object referring to objects the caller has no grant for
  resolves those to `unavailable`. Granting one object never silently grants what it points at.
- **A transaction touching anything outside the caller's view MUST fail whole**, before any write, with
  `unavailable`. Partially applying it because one row was hidden would break the guarantee the
  operation exists for.
- **Where a view is row-defined, confirmed deletion requires a lifecycle signal the caller is
  authorized to receive**, because absence there is indistinguishable from withdrawal. Where absence is
  within the caller's authority to observe, `not_found` remains that signal.
- A permission change invalidates affected cursors — see [Pagination](#pagination) — without revealing
  which hidden records caused it.

**`unavailable` is documented for every client**, including those talking to servers that never share.
A client MUST tolerate it wherever a record could appear, so a deployment turning sharing on later does
not change what its clients have to handle.
#### Insert grants

**An insert grant conveys no continuing authority over what was written.** It lets a principal add
records of exactly the concrete types it names, and nothing else. What it adds belongs to the dataset
from the moment it commits: the contributor cannot read it, change it or delete it, unless some
other grant it holds would let it do that to any record of the type — in which case that grant does,
and the record is to it like any other.

It is for a writer with no continuing relationship with what it writes: a form, a survey, a device
or a sensor, a feed reporting transactions, a till reporting totals — something asserted once and
never revised by its sender. **A writer who has to correct and withdraw what it wrote is not a
contributor**, and nothing here should be extended to make it one.

- **Exact concrete types**, never descendants, for the reason a type grant never covers one.
  Inserting any other type fails `403 permission_denied`.
- **It conveys no view.** A caller holding insert and no read is refused every read with
  `403 permission_denied`, including a read of the record it just added, and a query of the type it
  adds to. Its refusal is the same for every identity, so it learns nothing about what exists.
- **It conveys the schema it writes against.** Metadata reads answer for the types a caller may add
  to, as for types it may read, because writing a record needs its fields and a schema discloses no
  record. Other types are `unavailable`.
- **Composition is by type and by right, and nothing else.** A principal may hold read on some types
  and insert on others, or both on one. Each right authorizes its own operations: a record it added
  to a type it also reads is visible because it reads that type, exactly as any other record of the
  type is, and not because it added it. There is no rule of the form *its own records*.
- **An insert is an insert through every route**: the single-operation form, and `insert` inside
  `:transact`. A transaction is authorized per operation, so a contributor's transaction that also
  checks or changes anything fails at the first operation that needs more, before any write.
- **What it learns afterwards is its receipt.** A contributor's insert under an `Idempotency-Key`
  is its to recover: `:status` returns the identity always, and the body only while it has read
  authority over the record — which, holding insert alone, it does not.
  [`Prefer: return=representation`](#idempotency) is declined for the same reason.
- **The watermark on its response is the dataset's**, as for any guest. It is opaque, and says only
  that something changed.
- **Attaching content is not conveyed.** A `media` value needs the authority the
  [content protocol](tessera-content-v1.md) defines, and an insert grant confers none.

A contributor's records count against the dataset like any other, and an insert grant can write as
fast as its limits allow. **Admission, quota and rate are the deployment's**, as they are for every
write; see [the deployment contract](tessera-deployment.md). An owner who did not want a
contribution deletes it, as it would any record.

## Content

Media values are opaque references, never bytes and never URLs. A record response MUST NOT embed
content.

Resolving a reference to a short-lived URI, and uploading content, are the
[content protocol](tessera-content-v1.md), which versions independently — a record backend is
fully conformant without implementing any of it. A dataset's `content_store_id` names the content store
its records refer to. Byte-storage options are compared in [backend options](../docs/backend-options.md).

Item IDs are opaque and store-owned: a client MUST NOT parse one or treat it as a path, and a store
MUST NOT reuse one, for the same reason object IDs are not reused.

**A `media` value names its store.** The encoding is `<content_store_id>//<item_id>` — always
qualified, never a bare item ID:

```json
{ "type": "media", "value": "c-91ab0e77//91ab3f27c0" }
```

This is the same reasoning that qualifies an object reference with its dataset, and it earns its keep
at exactly one moment: **a content-only erase replaces a dataset's store, and the replacement starts a
fresh ID space.** A bare item ID would then resolve against whatever now occupies that ID in the new
store — silently, and as a different picture. Qualified, a stale reference resolves to *gone*, which
is the only honest answer.

- **A client resolves against the store named in the value**, not against the dataset's current
  `content_store_id`. That field says where *new* attachments go; the value says where an existing one
  already lives, and the two differ for every record written before a content-only erase.
- **A server MUST refuse a media write naming any store other than the dataset's current
  `content_store_id`.** Otherwise a caller could plant references into a store it has no relationship
  with, and the authorization the write performs would be against the wrong authority.
- **A server MUST NOT rewrite a stored value** when `content_store_id` changes. A record keeps what
  was written to it; re-pointing it would change what a past record says.
- A reference whose store is retired, unreachable or simply does not hold the item resolves through
  the [content protocol](tessera-content-v1.md)'s own answers. No new error codes exist for this, and
  a client MUST NOT read *the store is gone* as *the record is wrong*.

**One transactional authority.** The authority that decides a caller may attach an item MUST record
that attachment in the same transaction as the record; see [Writes](#writes). A deployment that cannot
do that MUST refuse media writes. The bytes are unconstrained — object storage, a filesystem, a CDN —
because a byte store is not an authority. Splitting the *authority* needs machinery outside v1, and the
analysis is in [the annex](annex-remote-content-authority.md), which is experimental and unimplemented.

**An item belongs to one dataset, from the moment it is reserved.** Ownership is established when an
import is reserved, not when a record attaches it, and an erase MUST enumerate everything the dataset
owns: committed items, items whose record write failed or was rolled back, items whose reference was
replaced or cleared, and abandoned imports still in staging. An erase that only followed live
references would leave exactly the photographs nobody can see and nobody asked to keep. A store MUST
NOT hold an item that two datasets independently own — an item cannot both survive for one and be
destroyed for the other. **Each logical content store used by records is associated with exactly one
record dataset in v1**, established by the authority before accepting imports. Two record datasets
MUST NOT name the same logical `content_store_id`. A service may host many logical stores over one
physical bucket, but MUST keep their reservations and items isolated. Import ownership follows that
immutable association; no caller-supplied item ID can change it. A store with existing reservations or
items MUST NOT be reassigned to another dataset.

**Erasing a dataset erases its content store with it**, in the same transaction and under the same
durable-deletion obligation, so an application erasing a dataset makes one call and gets one answer.
A replacement content store has a fresh identity and belongs to the new dataset. The other direction
does not cascade: a content-only erase replaces that store for the same live record dataset, updating
its `content_store_id` atomically and leaving the records alone, and an application making both calls
separately reports each outcome. Shared-dataset item ownership is outside v1.

## Conformance

A conformant server:

1. Implements every operation in [Operations](#operations) that is not gated on an advertised
   capability, plus every operation gated on a capability it does advertise — failing at declared
   bounds rather than omitting operations. An operation whose capability is not advertised MUST be
   refused with `403` `not_supported`, not silently accepted, and an operation the caller lacks
   authority for MUST be refused with `403` `permission_denied`.
2. Advertises exactly the optional capabilities it implements, and refuses the rest with `403`
   `not_supported` rather than answering partially.
3. Round-trips every value type exactly, including integers beyond 2⁵³, millisecond timestamps, and
   `json` values without reformatting. Vectors MUST cover integer-to-`float` acceptance at the
   representability boundary — 2⁵³ accepted, 2⁵³+1 rejected, 2⁵³+2 accepted, and each negated — since
   an implementation testing magnitude instead of representability passes every other case.
4. Implements the normative null, ordering, comparison and tie-break semantics in
   [Queries](#queries) and [Comparing two values](#comparing-two-values), including exact comparison
   of large integers against floats.
5. Accepts exactly the value conversions in
   [Accepting a value](#accepting-a-value-for-a-declared-field), and no others.
6. Returns effective metadata, with projections and values positionally consistent, and applies schema
   changes atomically across the whole hierarchy.
7. Validates every limit server-side, independently of what a client sent.
8. Distinguishes absence, refusal and unavailability, returns `404` rather than an empty record set
   for a missing single object, and computes every query, count, aggregate and distinct reference
   inside the caller's authorized view rather than filtering afterwards.
9. Replays a page for a repeated cursor, returns no row twice within a sequence, and keeps continuation
   correct regardless of where filtering and sorting are executed.
10. Claims an idempotency key before beginning the write it describes, and commits the record binding
    that key to its outcome in the same transaction as the write.
11. Never reuses a dataset identity, mints a new one on erasure, and does not transfer grants to it.
12. Never reuses an object ID within its hierarchy, including after deletion.
13. Refuses any write that would change a value an [immutable field](#immutable-fields) already
    holds, through a subtype and through `update_where` alike, and accepts its first value and its
    restatement.
14. Honours an [insert grant](#insert-grants) as adding exactly its named types and conveying nothing
    else, when it advertises `sharing`.

The conformance suite is the definition of these requirements. An implementation is conformant when
the suite passes against it, and the suite MUST include negative cases: refused objects that must not
look deleted, replayed and conflicting idempotency keys, a key whose status is asked mid-flight, a
cursor read twice, mismatched expectations, expired cursors, oversized predicates, rejected value
conversions, and every declared bound actually enforced. Erasure cases MUST include a lost response
recovered through `replaced_dataset_id`, deletion work that survives a restart mid-cleanup, a restore
that does not revive an erased dataset, and an upload authorized before the erase that cannot write
bytes back afterwards. Record/content composition MUST reject two record datasets naming one logical
content store, and MUST erase the reservations and unattached items the retired dataset owned.

### Version compatibility conformance cases

- Run [version-selection vectors](vectors/version-selection.json) against each client's
  selection function: unsorted arrays, numeric rather than lexical order, arbitrary-width
  majors, intersection, no overlap, empty support and malformed/duplicate majors. On no
  overlap, verify the client sends no authentication or data request.
- Run [version-discovery vectors](vectors/version-discovery.json): absent/empty metadata,
  valid announced and overdue sunsets, unknown additive fields, null/wrong shapes,
  unadvertised or repeated versions, and missing, malformed or non-UTC sunset timestamps.
  `pkg/conformance.CheckCore` checks served metadata; `CheckVersionMetadata` also permits
  independent checking of captured discovery bytes.
- With deprecation configured, confirm discovery remains unauthenticated and `/v1/`
  remains served, including errors, after the planned date. The reference implementation
  checks startup configuration and never automatically retires a route by date.
- In dated operator acceptance evidence, check that retirement is refused before the
  12-calendar-month boundary, while any released client requires the major, or without
  at least 14 days' notice and a discovery announcement. Include a leap-day successor
  release: 2028-02-29T12:00:00Z permits no retirement before 2029-02-28T12:00:00Z.
  An HTTP snapshot cannot prove historical overlap, future service or consumer notice;
  these obligations require operator records and independent consumer confirmation.

### Author-resolution conformance cases

The suite MUST also prove the following boundaries (review findings 6 and 9–13):

- Guest writes, including filtered and schema mutations, are refused. Data-field edits do not change
  the view generation, while revoked or expired authority invalidates cursor replay. An `unknown`
  status observed before a delayed request claims its key does not prevent that request committing;
  joined host rollback leaves no committed outcome.
- A parent type grant reveals neither existing nor newly created subtypes. Descendant queries/counts
  intersect exact grants before grouping, pagination and limits; aggregates and distinct references
  also respect exact grants. Object-only grants never add query rows. A separate child type grant
  makes that type readable.
- Erasure carrying an idempotency key fails before any retirement. Lost unkeyed erase responses recover
  through replacement discovery, and replay against the old identity never erases the replacement.
  Ordinary keyed mutations still replay their retained outcomes.
- Aggregate counts, sum and extremes permit a client to derive an average for nonzero value counts;
  zero contributors yield null statistics. No average member is required.
- Distinct-reference elements expose the bare three-member identity, with the required ordering and
  deduplication, and reject malformed/null values and oversized results.
- Discovery does not advertise `locales`. Changing or omitting `Accept-Language` preserves stored
  metadata names, effective type versions and whole-schema versions; an unchanged `expect_type`
  succeeds across those reads. Editing the stored schema name remains a schema change.

### Aggregate sum conformance cases (finding 14)

For a server advertising aggregation, the suite MUST prove the following with inputs below all
declared scan bounds. Compare integer sums as exact JSON numbers, without parsing through binary64;
float results are compared as binary64 values, not by their decimal spelling.

- Two integer values of `9007199254740993` produce `18014398509481986`. Both signed 64-bit endpoints
  succeed as single-value sums. `9223372036854775807` plus `1`, two copies of that maximum, and
  `-9223372036854775808` plus `-1` each fail with `422 query_result_limit_exceeded` and
  `details.reason: "sum_overflow"`.
- Integer values `9223372036854775807`, `1`, `-1` sum to `9223372036854775807`; values
  `-9223372036854775808`, `-1`, `1` sum to `-9223372036854775808`. Each permutation succeeds.
- Float values `9007199254740992`, `1`, `-9007199254740992` sum to `1` in every permutation.
  `9007199254740992` plus `1` rounds to `9007199254740992`, while `9007199254740994` plus `1`
  rounds to `9007199254740996`, exercising both directions of ties-to-even. Two largest finite
  binary64 values overflow; two such values and the negative of one sum to the largest finite
  value in every permutation. A smallest positive subnormal plus zero remains that subnormal;
  equal opposite values sum to positive zero.
- Run sum boundaries grouped and ungrouped. Opposite overflowing totals in separate groups do not
  cancel; any overflowing group fails the entire request with no partial aggregates. Null values
  do not contribute, and empty/all-null groups retain null statistics.
- More than `max_aggregate_groups` groups fails with reason `group_limit`; a distinct-reference
  result exceeding `maximum_count` fails with reason `distinct_reference_limit`. Exercise each
  failure alone to make the reason unambiguous; neither response is truncated. Take the group bound
  from the dataset's declared `limits` rather than from a number written into the suite, so that a
  server declaring a different one is still tested against its own promise.

### Reference grouping conformance cases

- `group_by` naming a `reference` field groups by identity. `group_by` naming any other type —
  `integer`, `float`, `timestamp`, `blob` — is `400 invalid_field`.
- **Grouping compares the whole triple.** Prove it with identities built to collide: three
  references agreeing on `object_id` and differing only in `meta_id`, or only in `dataset_id`, MUST
  be three groups. A server comparing `object_id` alone answers with one group whose total is the
  sum of all three, and nothing about that answer is malformed — which is why the case has to exist
  rather than be assumed. A reference is a pointer rather than a foreign key, so these identities
  need no existing targets and the case needs no second dataset to construct.
- **Each group is the bare qualified identity**, with all three members present, exactly as
  [distinct references](#reads) returns one. A group answered as an object identifier alone MUST
  fail the suite: a client whose references all point into its own dataset would decode it without
  noticing what it had stopped being told.
- Rows whose reference is null form the null group, counted, sorted first.
- Groups are ordered: null first, then reference groups ascending by `meta_id`, then `dataset_id`,
  then `object_id`.


## Appendix A: reserved words

Rejected as a whole meta type ID or field ID, case-insensitively:

```
add all alter and as autoincrement between case check collate commit constraint create default
deferrable delete distinct drop else escape except exists foreign from group having if in index
insert intersect into is isnull join limit not nothing notnull null on or order primary references
returning select set table then to transaction union unique update using values when where
```

This is the SQLite keyword set that cannot appear unquoted in column or table position, and is a
portable superset for other SQL engines. It exists so that a schema rejected by one implementation is
rejected by all of them, rather than failing as a syntax error in one and succeeding in another.
