# Supported features

What this implementation does, as of the commit that carries this file. The
[specification](../specs/tessera-protocol-v1.md) is the contract and this table is not part of it: a
feature listed here is supported by *this server*, not required of every server. Required operations
are not capabilities and cannot be switched off.

Run the suites yourself rather than trusting the table; the entry point is at the bottom.

For a local server, follow [SQLite development](sqlite-development.md). For operator
configuration and OAuth behavior, use [self-hosting](self-hosting.md). Independent
app/device and server acceptance remains in the [S08 replacement handoff](v0.1.0-s08-acceptance-handoff.md).

## Required surface

| Area | State |
| --- | --- |
| Discovery, dataset listing, watermark, schema version, view version, access | Implemented |
| Effective metadata: list, one type, HEAD, children, create, update, delete | Implemented |
| Eleven transaction operations, aliases, guards, cascade, rollback, failure index | Implemented |
| Single reads, queries, count, projection, grouped selection, sorting, bounds | Implemented |
| Aggregation, grouped by a `text` or `reference` field, bounded by `max_aggregate_groups` | Implemented |
| The `fixed` exact decimal type, with `scale` per field and exact sums | Implemented |
| `max_materialized_bytes`, advertised, with `507 dataset_full` when a write outgrows it | Implemented. This engine charges the whole dataset against it |
| A dataset created with its types installed, in one transaction | Implemented |
| `max_transaction_operations`, declared, with `operation_limit` when exceeded | Implemented |
| Record versions and effective schema versions | Implemented |
| `immutable` fields, enforced on every write path | Implemented |
| The `insert` grant, `access.insert`, and contribute-only callers who add records and read none | Implemented |
| Pagination with cursor replay and permission invalidation | Implemented. Computed a page at a time by keyset continuation where the sort allows it |
| Idempotency, `:status`, digest conflicts, identity-only recovery, crash recovery | Implemented, durable engines only |
| Erasure, replacement discovery, deletion queue, restore fence | Implemented |

## Optional capabilities

Advertised per composition. Anything withheld is refused `403 not_supported` rather than answered
partially, and an unknown capability name fails at startup rather than being advertised.

`aggregation`, `distinct_references` and `text_matching` are no longer here. They are required
operations — implemented, not switchable, and not advertised, because there is no choice to report.
Naming one in `optional_reads` fails at startup rather than being accepted and ignored.

| Capability | State | How to enable |
| --- | --- | --- |
| `sharing` | Implemented, read-only guests, exact concrete type grants | Advertised by the SQL engines |
| `collection_access` | Implemented, exact-type collection reads and inserts | Hosts supply scopes through `store.View.WithCollections` |
| `creator_scoped_mutations` | Implemented, creator-scoped updates and deletes | Hosts supply scopes through `store.View.WithOwnMutations` |
| `creator_attribution` | Implemented, opt-in creator text fields | Advertised when the dataset admits 32-byte text values |
| `content` | Implemented, filesystem bytes | `content` in the configuration |
| `thumbnails` | Implemented, computed per read and never stored | Advertised with `content` |
| `current_representation` | **Not implemented** | — |
| `ranges` | **Not implemented** | — |
| `locales` | **Withdrawn from v1** by the specification author | — |

## The authorization server

Supplied by this deployment rather than required of every server: the protocol names `oauth2` as an
authentication method and leaves the authorization server to whoever runs one. Listed because it is
the surface a client meets first, and because this table was silent on the most consumer-visible
change this implementation has made.

| Surface | State |
| --- | --- |
| `GET /oauth/authorize` | Implemented. `dataset` is **optional**: named, it is shown; omitted, the person chooses at approval from what their account may reach |
| Page language | English only for `v0.1.0`; `Accept-Language` does not select translations, and responses retain `Vary: Accept-Language` |
| Sign-in and approval | Ordinary clients authenticate, then choose dataset/grants unless exactly bound consent can be reused. An operator-designated Console uses its dedicated current-authority policy; dynamic registration cannot enable it |
| Per-step CSRF | Implemented. Each step carries its own single-use token and cookie, so one that survived the first is refused at the second |
| `grant_type=client_credentials` | Implemented. Requires `dataset` in the token request, with no exception for a storage holding one |
| Authorization cardinality | One dataset per authorization, so connecting a second storage is a second authorization, revocable on its own |

The account is unknown before sign-in, so an ordinary client may omit `dataset` and let
the person choose afterwards. Credentials go directly to Tessera. Scope hints affect
held, safe grant defaults; Schema, Erase and administration remain explicit. Operator
branding, registered PNG icons, recovery, consent reuse and first-party policy are described
in [identity configuration](self-hosting.md#identity-page-branding).

## Server-level protocol surface

| Surface | State |
| --- | --- |
| `POST /v1/datasets`, creating a dataset | Implemented. Advertised as `dataset_creation`; refused `403 not_supported` unless the host supplies a decision and the engine can provision. Requires an idempotency key and answers `201 {"dataset_id": …}` |
| `POST /v1/operations/{key}:status`, a server-scoped outcome | Implemented. Creation is the only operation with no dataset, so its receipt is addressed here rather than under one |
| Server-scoped receipt retention | Implemented. One week, written as a deadline with each claim, swept on its own terms rather than by any dataset's window |
| `server_capabilities`, `server_limits`, `server_access` | Implemented. Capability and window in discovery, the caller's own authority on the listing |
| `shared_reads`, one authorization reading everything shared with the account | Implemented. Advertised in `server_capabilities` |
| Discovery `display_name` | Implemented. Present only when the operator configures one |
| Discovery `deprecated_versions` | Implemented, validated operator configuration; omitted by default, with no automatic route retirement |
| Highest-common protocol selection | Importable `pkg/protocol` helper and portable conformance vectors; independent client acceptance required |

## Deployment surface

Defined by [the deployment specification](../specs/tessera-deployment.md) rather than the record
protocol: another server may organise accounts and sharing differently.

| Surface | State |
| --- | --- |
| Invitations and issuers, including templates that create a dataset per invitee | Implemented |
| Readers: named, template and issuer-group (`issuer:{issuer}`), with type scope and expiry | Implemented |
| Frozen reader shares: `:freeze` and `:unfreeze`, unfreeze only by the dataset's recorded owner | Implemented |
| Moderated sharing per issuer: an administrator stops a member's share, never reading or deleting | Implemented. Opt-in per issuer |
| Account lifecycle: suspend, reinstate, delete, password reset and recovery codes | Implemented |
| Per-dataset administration pages and `/admin/api/v1` | Implemented. Overview account `rebindable` reflects the caller's current issuance standing |
| Device-member administration | Own-dataset shares, usage and moderation stops; confirmed leaving erases `erase_with_account` datasets and revokes every device. Device erase while remaining a member is unavailable |
| Restore identity reconciliation | Implemented before standalone startup serves; missing-dataset grants are removed, bound installations revoked and affected fixed-policy issuers disabled without granting replacements |
| Online backup acquisition, catalog and retrieval-only collectors | Included. [Contract and limits](backup-acquisition.md) |
| Native backup scheduler, policy and offline producer recovery | Included. [Contract and limits](backup-scheduler.md) |

Dataset creation and server-scoped recovery are defined by
[the record protocol](../specs/tessera-protocol-v1.md#creating-a-dataset).
A server withholding creation must refuse it with the specified error.

`current_representation` and `ranges` are refused `403 not_supported`. A thumbnail of an item this
server cannot decode is `415 unsupported_representation`, which is a different answer and means
something different: the capability exists but not for that item.

## Engines

| Engine | State | Datasets | Processes |
| --- | --- | --- | --- |
| Memory | Synthetic and non-durable; rejects idempotency keys | One | One |
| SQLite | Durable, the development and default self-hosted engine | Many | One, by an exclusive lock on a file beside the database |
| PostgreSQL | Durable, same suites, same behaviour | Many | Many, each holding a session lock under its own executor identity |

Writes to different datasets proceed in parallel on PostgreSQL and wait for each other on SQLite,
where one writer at a time is a property of the file. SQLite staying single-process is a decision,
not a gap: the alternative was a timestamped lease, and a lease resolves live work as failed whenever
an executor is slower than it. See [self-hosting](self-hosting.md).

## Deliberately absent

Live storage encryption is an operator-provided volume or filesystem control, required explicitly by
configuration; backup encryption is in-process. The filesystem content backend implements no CDN or
provider-version cleanup because it has neither. Review findings and provisional interpretations are
recorded in [the review](tessera-spec-review.md).

## Running the conformance suite

`pkg/conformance` is importable and takes any HTTP server, not only this one:

```go
fixture := conformance.CoreFixture{URL: "https://your-server.example", Token: token,
    RefusedToken: writeOnlyToken, Client: http.DefaultClient}
err := conformance.CheckCore(ctx, fixture)
```

| Suite | Covers |
| --- | --- |
| `CheckCore` | Required metadata, records, queries, versions, watermarks, language stability |
| `CheckPaging` | Cursors, replay, continuation, permission invalidation |
| `CheckRecovery` | Idempotency, `:status`, conflicts, identity-only recovery |
| `CheckContent` | Imports, resume, commit, resolve, representations, erasure |
| `CheckCreation` | Dataset creation, the mandatory key, identity-only responses, replay against each retained outcome, the server-scoped receipt, and recovery surviving erasure. Runs the withheld half — a `not_supported` refusal — against a server offering nothing |
| `CheckSharing` | Read-only guests, exact type grants, absence versus invisibility. Needs `Share` when the dataset advertises sharing |
| `CheckOptionalReads` | Required aggregation, distinct references and text matching; historical function name retained for compatibility |

`CheckSharing` tests both offered and withheld sharing. An advertised sharing capability
without the required fixture `Share` callback fails, rather than skipping.
`CheckCreation` likewise covers an offered creation route and a withheld refusal.

**Use disposable synthetic datasets.** The suites define schemas, write records and upload
content; they do not clean up. Provision a fresh disposable fixture for each run; do not rely on repeat-run compatibility.

`CheckCore` enforces the bounds the dataset advertises rather than any it assumes, and skips a case
whose request would exceed `max_request_bytes` on that server, since the size bound legitimately
fires first. A server enforcing no limits at all fails.

Locally:

```sh
make check
```

That gate skips PostgreSQL unless `TESSERA_TEST_POSTGRES_DSN` names a disposable database, and
prints `ok` either way; `make release-check` is the same gate with an absent DSN made a failure that
names the variable. `make integration` and `make integration-auth` add HTTP and OAuth scenarios;
`make faults` runs the fault-injection suite, which exists only in a `devfaults` build.

**Passing these suites is not a conformance certificate.** Full conformance requires every required
operation and every advertised optional suite, and the specification is the contract.

## Backup limits

The [acquisition storage model](backup-acquisition.md#content-and-deployment-limits) defines filesystem,
archive-size and identity-registry limits. The [release qualifications](releasing.md#first-release-backup-acceptance)
state what remains unaccepted, including real cloud and separate-host recovery. Retain keys,
trusted producer identity and current retirement evidence independently.
