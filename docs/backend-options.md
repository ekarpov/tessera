# Backend implementation options — encryption and media

Implementation choices behind the [protocol](../specs/tessera-protocol-v1.md). Neither topic changes
the application: that is precisely why they live here and not in the specification. A client cannot
tell which of these a deployment chose, and must never be able to.

## Quick guide

The reference server uses operator-provided volume/filesystem encryption for live storage,
in-process encryption for exported backups and optional filesystem content. Follow
[self-hosting](self-hosting.md) to configure it. The choices below explain alternatives
available to implementers; they are not all supported backends in this server.

## Where the protocol actually constrains encryption

The protocol's only requirement is that **the server can evaluate values**. Filtering, sorting,
grouped first-N selection, aggregation and distinct references are all defined as work the backend
does without transferring source records, so a server that cannot compare two values cannot implement
them. Everything else is free:

- **Encryption the server can see through** — volume encryption, page-level encryption, in-process
  decryption, an enclave — is invisible to the protocol. No spec change, no capability change.
- **Encryption the server cannot see through** would require a different protocol, not a flag: the
  client would have to fetch and filter locally, which the contract forbids as unbounded
  materialisation.

So "how the server holds records is implementation specific" is right, with that one boundary. The
development server encrypts nothing. Production is an operator choice, and the options below differ
in who can read the data and what it costs in capability.

**Reduced capability is not the degradation mechanism — in-process work is.** An earlier draft here
proposed advertising predicate kinds separately so an encrypted store could offer equality without
ordering. That pushes the problem the wrong way: the client refuses unsupported operations rather
than compensating, so a narrower advertisement removes a feature from the app rather than slowing it
down. A server whose store cannot order ciphertext decrypts and orders in its own process, and still
advertises sorting. Where that is too expensive, it declares a bound and fails explicitly at it. See
"What a capability means" in the specification; the consequence for this page is that **an encryption
choice costs the server work, not the client features**.

## Encryption options

"In-database query support" below is what the store evaluates natively. Whatever it cannot becomes
work the server does in its own process; the advertised capability is unchanged either way, so the
column measures cost rather than features.

| Option | Who can read | In-database query support | Notes |
| --- | --- | --- | --- |
| Volume or filesystem encryption | Operator, and anything running as the server | Full | The portable baseline. Protects a stolen disk or a decommissioned volume, nothing else. Core PostgreSQL has no built-in TDE; it exists in commercial forks, so volume encryption is the neutral answer |
| SQLite page encryption (SQLCipher) | Operator | Full | Decryption is transparent to SQL, so every capability survives. Fits the single-binary dev and small self-host shape |
| Column encryption with a server-held key (e.g. `pgcrypto`) | Operator | Reduced | The server decrypts to evaluate, so plaintext exists in memory and in query plans. Buys little over volume encryption unless the key is held elsewhere |
| Queryable / structured encryption | Not the operator | Equality, and range in some schemes | Shipping in some databases. Property-preserving schemes leak by construction, and inference attacks against deterministic and order-revealing encryption are well documented, so the leakage profile must be stated rather than assumed |
| Trusted execution (Nitro, SEV-SNP, enclave-backed database features) | Not the operator | Full inside the enclave | The only mainstream route to full capability with the operator excluded. Costs hosting flexibility and adds attestation and key management, which works against easy self-hosting |
| Client-side encryption with no server evaluation | Only the user | None, and not compensable | The server cannot decrypt, so it cannot do the work either. A different protocol, not a mode of this one |

**Recommendation.** Dev server: nothing. First production shape: volume or page encryption plus
operator discipline, with the privacy promise stated honestly as *the operator can read this*, which
is acceptable precisely because self-hosting makes the user the operator. Treat queryable encryption
and enclaves as investigation items with a written leakage profile before either is offered, and do
not describe any of them as end-to-end.

**One asymmetry worth exploiting.** Media is never queried. Attachments are opaque bytes the server
only stores and serves, so they can be encrypted client-side with no capability loss at all — unlike
records, where every encryption choice trades against query support. A hybrid where records are
server-readable and media is opaque to the server gets the largest privacy gain for the most
sensitive content at no cost to the protocol. It needs a key the user holds, which is a real design
task, but it is the one place where the usual trade-off does not apply.

## Media is a URI, not a payload

Confirmed 2026-09-16: a record response never carries media bytes. This matches what the app already
does — `ContentRef` is documented as identifying a resource without exposing an access URL, and
`ContentStorage` resolves it at read time — so the remote shape is: the record carries the opaque
reference, and a separate call exchanges it for a short-lived URI to whatever actually stores the
bytes.

That keeps three properties. Record queries never hydrate content, the content service can be a
different system from the record service, and the app's existing separation between metadata storage
and content providers is preserved rather than worked around.

### Options for what stores the bytes

| Option | Fit | Trade-offs |
| --- | --- | --- |
| S3-compatible object storage (MinIO self-hosted, S3, R2, B2) | **Recommended default.** Reads presign natively, so the authority issues a URI and carries no download load. MinIO keeps it self-hostable | Presigned URLs are bearer credentials for their lifetime. Resumable upload does **not** map onto a presigned `PUT`: use the provider's multipart upload and keep the part bookkeeping, or terminate transfers at the service |
| The record server's own filesystem, serving signed URLs itself | Simplest self-host: one binary, one directory, no second service | The server carries all upload and download bandwidth, and needs its own range and resume handling |
| WebDAV or an existing personal cloud (Nextcloud and similar) | Appeals to self-hosters who already run one, and keeps media under storage the user already controls | No presigning: needs an authenticated byte gateway under service-held credentials. Using it that way is byte storage — preserving *its* users' own sharing permissions would be a separate authority, which v1 does not support |
| CDN in front of any of the above | Read performance and offload for a shared deployment | Caches copies outside the origin's control, so purge behaviour and erasure completion have to be squared explicitly |

**The abstraction is "the authority issues a short-lived authorized transfer URI", not "every backend
presigns its own origin."** Whether that URI points at the provider or at the service's own endpoint is
a backend detail; whether it can be resumed is not. Resumption is mandatory for any store advertising
`writes`, so a backend that cannot resume natively is translated behind the service's own transfer
endpoint — the client only ever speaks the offset protocol.

A reference implementation should support the first two behind one interface: the filesystem store
for `make dev` and small self-hosts, S3-compatible for anything larger. The protocol sees neither —
it sees an opaque item ID and a resolve call.

### What the URI contract must say

- **Short-lived and single-purpose.** A resolved URI is a bearer credential: anyone holding it can
  read those bytes until it expires. Lifetime in minutes, scoped to one item and one operation.
- **Never stored.** The app persists the `ContentRef`, never a resolved URI, so a leaked or logged
  URI expires into uselessness. The app already does this, since `ContentRef` explicitly is not a URL.
- **Upload is the mirror image.** Request an upload URI, transfer directly to the byte store, then
  commit through the record service, which is what makes the resumable `ContentImport` contract — an
  operation UUID, append at offset, idempotent commit — map onto object storage rather than fighting it.
- **Erasure has to reach it, and finishes asynchronously.** Bytes in an object store or a CDN are not
  erased by deleting a record. One transaction decides and durably enqueues; a worker then deletes,
  invisibly to the client, which is done when the erase returns. Versioned buckets need version
  deletion rather than a delete marker, an upload URI issued before the erase must not write bytes back
  afterwards, and CDN caches and backups are stated limits of the promise rather than details.
