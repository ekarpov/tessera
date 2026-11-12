# Consuming Tessera

Use Tessera over HTTP, embed its Go engine, or join its writes to your own SQL work.
Choose the least coupled option that meets your needs. Application vocabulary and
exchange workflows remain in the host; see [storage and exchange](storage-and-exchange.md).

## Quick guide

For HTTP integration, start a [synthetic development server](sqlite-development.md), then
read the [protocol overview](../specs/tessera-overview.md). For Go integration, install a
pinned module and use the embedding choices below.

## Getting the module

```sh
go get github.com/ekarpov/tessera@v0.1.0-rc.4
```

The candidate is not final release acceptance and carries no migration guarantee.
While the repository is private, authenticate Git and set `GOPRIVATE` for the module
before fetching it. Preserve other private-module patterns already configured. If using
SSH, an owner-scoped rewrite leaves unrelated public clones alone:

```sh
# Merge this pattern with your existing GOPRIVATE value if one is already set.
go env -w GOPRIVATE='github.com/ekarpov/*'
git config --global url."git@github.com:ekarpov/".insteadOf "https://github.com/ekarpov/"
```

## Embedding

### Run the server, talk HTTP

`cmd/tesserad` with a [self-hosted configuration](self-hosting.md) is a complete deployment: HTTPS,
OAuth with PKCE, service principals, encrypted backups, an administration surface. A consumer talks to
it over the protocol like any other client, and the two systems share nothing but a network.

Right for a personal store that belongs to the person rather than to the consuming service, which is
the case an exchange's own *personal store* profile already describes.

### Embed the engine, serve the protocol yourself

```go
engine, err := sqlite.Open(ctx, path, protocol.DevelopmentLimits())
handler, err := server.NewWithOptions(engine, authenticate, server.Options{
    AuthMethods:          []string{"oauth2"},
    AuthorizationServers: []string{issuer},
})
```

`pkg/server` gives the HTTP surface; `pkg/store/sqlite` and `pkg/store/postgres` give durable
engines behind one boundary. **The host supplies authentication and Tessera keeps authorization.**
The resolver returns a principal and an application authorization and never a grant derived from
the request; grants come from `SetAuthorization` and are checked on every operation. Missing host
context fails closed.

This is right when the host already owns the identity system and does not want a second
authorization server.

### Join Tessera writes to your own tables

The tightest seam, and the reason the store exposes a transaction at all:

```go
outcome, err := engine.MutateWith(ctx, access, mutation, func(core store.Tx, host *sql.Tx) error {
    _, e := host.ExecContext(ctx, `INSERT INTO "exchange_receipts" (...) VALUES (...)`)
    return e
})
```

Record changes, the host's rows, and the durable idempotency receipt commit together or not at all.
That is what makes an exchange receipt and the record it refers to impossible to separate — the
failure that otherwise shows up as a receipt for a submission that was rolled back.

`pkg/store/sqlite/composition_external_test.go` is a working example that imports only public
packages. The rules are narrow and enforced: the callback must return its error, may perform no
network I/O, must not reenter the store, and must not retain or share the transaction. A callback
error, a panic or a cancellation aborts everything. **The callback may run more than once**: a
write is first evaluated against only the records it touches, and one that turns out to need the
whole dataset — it changes a schema in a way that rewrites records — is rolled back to a
savepoint, host SQL included, and run again. So a callback must not keep anything outside itself
that a second run would add to.


## Verifying it, including your own implementation

The [importable conformance suites](feature-matrix.md#running-the-conformance-suite)
accept your HTTP server. Use disposable synthetic datasets: the suites write data and
attachments and do not clean them up. The specification defines correctness; a suite
pass is evidence for its cases, not the definition of the protocol.

Client decoding needs separate tests. Preserve exact JSON integers beyond floating-point
precision, fixed decimal strings beyond a fixed-width decimal ceiling, and annotation JSON
bytes using `protocol.MarshalWire`. A server suite cannot catch loss in a client decoder.

## What stays the host's, and must not migrate

Review workflows, purpose-scoped consent, ownership transfer, cross-service cleanup and
vocabulary interpretation remain host responsibilities. Storage grants do not encode a
purpose or a subject. Read grants cover exact concrete types, collections or objects.
Insert grants cover exact concrete types or collections. See the
[embedding contract](../specs/tessera-embedding.md) for required host guarantees.

## Current limits

`locales` is withdrawn from v1. `current_representation` and `ranges` are unimplemented
and refused with `403 not_supported`. See the [feature matrix](feature-matrix.md) for
engine and backup limits, [self-hosting](self-hosting.md) for standalone configuration,
and the [rc.4 checklist](rc.4-acceptance-checklist.md) for independent acceptance still required.
