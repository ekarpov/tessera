# Reference-server backup acquisition

This is an administrative reference-server interface, independent of the record and content
protocols. It requires a server administrator's bearer token with `administer` standing and a
configured backup directory/key. It exports no keys, host paths or retirement identities in JSON.
Scheduling, packaging, external storage, retention and delivery to end users belong to callers. Set
`backup.every_hours` to `0` to disable the internal scheduler (including its startup backup) while
keeping acquisition enabled. Values 1–87600 retain the existing internal schedule; `keep` is 1–1024
published sets.

## Consistency and erasure

S is the first read in the archive's repeatable SQL snapshot. The identity registry is frozen
through that read and its own capture, so SQL grants and registry agree. Content is the exact
committed prefix of each required upload at S, including incomplete imports; uncommitted tails are
excluded. Missing required bytes fail the acquisition. R is a separate SQL snapshot taken **after**
archive export returns. Evidence is the union of every retirement visible at R and the independently
retained local historical ledger, including evidence carried through an earlier restore. Neither
wall-clock times nor the shared `journaled` flag establishes completeness.

Erasure before S is represented in the archive and evidence; erasure between S and R is reapplied on
restore. An erasure concurrent with R belongs to the evidence exactly when its commit is visible to
R's database snapshot. An erasure after R requires a later evidence acquisition. A successful
operation never proves that no later erasure occurred. S and R identifiers are opaque random capture
labels, not database transaction IDs or comparable watermarks. Operation and lease times govern
admission/expiry; they are never proof of a retirement capture boundary.

## Operations

`PUT /admin/api/v1/backup-operations/{id}` accepts `{"kind":"backup"}` or `{"kind":"evidence"}`. IDs
are `<unix-seconds>-<32 lowercase hexadecimal digits>` supplied by the caller, with real entropy.
First admission requires the seconds within five minutes of server time. `GET` at the same URL
reconciles lost acceptance or completion responses. An identical PUT returns the retained operation;
a different kind returns `409 conflict`. Acceptance is durable before 202. Do not choose a new ID to
reconcile an uncertain response.

One operation runs at once, with no waiting queue, a ten-minute execution deadline, at most 1024
retained status entries, and seven days of status retention. New work exceeding a bound is refused
with `limit_exceeded`. `DELETE` requests cancellation; completion may win a race with cancellation.
Request disconnect does not cancel accepted work. Shutdown cancels and joins workers. Restart marks
interrupted operations `failed` and removes their unpublished staging; the same ID never restarts
them. Terminal states are `complete`, `failed`, `cancelled`, and `expired`. Failed operations have a
fixed safe failure code, never an underlying filesystem/database error. Seven days after admission,
status becomes expired and may be removed. An expired timestamp can never be admitted as new work;
old or unknown IDs return `expired`/`not_found`, not a replacement backup.

Completion follows file fsync, atomic directory publication, directory fsync and durable status. The
result names opaque artifact IDs, byte sizes and SHA-256 digests, capture labels S/R, a deployment
identity and a pairing metadata artifact. Metadata authenticates those fields using HMAC-SHA-256
with the separately retained backup key and a domain separator. A digest alone is not
authentication. Trust the configured HTTPS origin and administrator authentication during
acquisition; independently verify the metadata MAC with the retained key before offline restore, and
match deployment and artifact IDs/sizes/digests. The deployment identity is persisted outside
archives. Possession of the backup key permits forging metadata; this is symmetric authentication,
not third-party attestation.

On startup, completed sets beyond the configured `keep` are rotated oldest-first before admission,
including an interrupted rotation or a lowered `keep`. Expiry is durably recorded before deletion;
restart finishes deletion of expired sets left on disk. These retained operation IDs remain expired
and never recapture their archives. Leases still end at restart as described below.

Every successful archive publication updates `server.last_backup_at` through the shared publication
path; evidence-only and failed operations do not. The journal retains the successful publication time
so restart can repair a missed overview update without exporting again. This timestamp is diagnostic,
not an S/R boundary, and does not authenticate artifacts.

## Artifact protection and transfer

`PUT /admin/api/v1/backup-operations/{id}/leases/{lease}` grants or renews an independent
five-minute lease on the complete operation's exact artifacts. The caller supplies a fresh
32-character lowercase random hexadecimal lease ID. The returned expiry is server time. There are at
most eight leases per operation and 64 total; releasing one does not release another. `DELETE` on
that URL releases it. `GET /admin/api/v1/backup-operations/{id}/artifacts/{artifact}` requires a
current lease ID in the `Tessera-Backup-Lease` header. Up to eight transfers are admitted
concurrently, each with a two-minute write deadline. Authorization is checked on every request. An
admitted transfer pins its open file through EOF/close even if the lease expires or is released.
Leases do not survive restart; obtain another before copying.

Every online producer (scheduler, legacy `POST /backups`, and this API) uses the same admission,
publication and rotation machinery. The legacy endpoint still returns a filename, now for the
archive inside a published pair; callers needing reconciliation must use operations. At most `keep`
published sets are retained plus one staging set. A set protected by a lease or active transfer
cannot be rotated; new work is refused if protection leaves no capacity. Status may outlive
artifacts; a rotated or expired set cannot be leased or downloaded. There is no client-supplied path
or artifact filename. A backup is bounded by the existing 256 MiB plaintext/compressed archive
limits. The materialization budget now charges 64 bytes per SQL cell as well as payload bytes, and
actual uncompressed ZIP output is checked against the restore limit; some archives near the previous
payload-only limit now fail earlier. This bounds empty-cell/row allocation and prevents publishing
archives too large to restore. Evidence is bounded at 16 MiB. Storage is private and encrypted at
rest, including plaintext evidence and historical fences. Operators must provision capacity for
`keep + 1` bounded sets and preserve the operation journal, deployment identity and independent
fence through restarts.

## Content and deployment limits

Online filesystem pinning requires hard links and reliable flock semantics on one shared local
filesystem. Every process accessing a shared content directory must run this implementation and use
the same directory/lock inode; rolling upgrades with older writers and distributed byte directories
are unsupported. A maximum of 256 required files bounds pin preparation and descriptors (one content
file open at a time). Preparation after taking the transfer lock has a two-second cooperative
deadline, with SQL cancellation and context checks during filesystem reads/linking; blocked kernel
I/O may delay cancellation. Prefixes never change once committed; writers serialize filesystem
checkpoints across processes, truncate only uncommitted tails and append. Hard links survive
cleanup/erasure; compression, encryption and archive output run after the transfer lock is released.
Each pinned inode can grow up to the existing 64 MiB item ceiling: worst-case retained content is 16
GiB during the bounded export, not additional copied staging. A separate lifetime flock admits only
one export across processes sharing the byte directory; the next exporter reclaims abandoned
`.backup-pins` only after acquiring that lock, proving no live exporter owns them. Never unlink
either lock file while serving. SQL-only PostgreSQL processes may use independent data dirs;
acquisition journals/directories are owned by one reference-server runtime, never shared by
runtimes.

## Restore

Acquire backup, acquire its lease, copy all artifacts, verify sizes/digests and metadata MAC, then
release the lease. Retain the key separately. For newer erasures, independently acquire `evidence`,
lease/copy/verify it, and pass that newer evidence file as `-retirement-fence` when restoring the
old archive with the existing `-restore` and `-backup-key-file` flags. Evidence is the existing
JSON-lines retirement-fence format (an empty file is valid). Restore targets a fresh destination
with all serving processes stopped; it retains the supplied historical evidence for subsequent
exports. Authenticate the newer evidence metadata and verify it belongs to the same deployment
before using it. Retain newer evidence independently: restoring the archive's old fence can
resurrect erased data.

## Authentication and integration example

The metadata artifact is JSON `{"payload":"<base64>","mac":"<hex>"}`. Decode `payload`; verify
HMAC-SHA-256 over UTF-8 `TESSERA-ACQUISITION-1\n` followed by those exact decoded bytes. Then decode
the payload JSON and check its version, expected deployment and artifact descriptors. `artifacts`
inside the authenticated payload describes archive/evidence; the operation result also includes the
metadata artifact's own descriptor. S is also authenticated inside the encrypted archive.

Use a bearer credential issued after explicit OAuth consent to `administer`. A dataset administrator
without the server right cannot use any operation, lease or artifact route. Example shell workflow
(with `base` an HTTPS origin and `token` a private administrator token):

```sh
id="$(date +%s)-$(openssl rand -hex 16)"
# Persist id before sending.
curl -f -X PUT "$base/admin/api/v1/backup-operations/$id" \
  -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
  --data '{"kind":"backup"}'
# Poll until state is terminal. Retry PUT with this
# same id and kind after any lost response. Running and replayed PUTs return 202.
curl -f "$base/admin/api/v1/backup-operations/$id" -H "Authorization: Bearer $token"
lease=$(openssl rand -hex 16)
curl -f -X PUT "$base/admin/api/v1/backup-operations/$id/leases/$lease" \
  -H "Authorization: Bearer $token"
# For EACH descriptor in the completed operation, preserve its id as filename.
curl -f "$base/admin/api/v1/backup-operations/$id/artifacts/$artifact" \
  -H "Authorization: Bearer $token" -H "Tessera-Backup-Lease: $lease" -o "$artifact"
# Renew the same lease with PUT before expiry while copying; release afterwards.
curl -f -X DELETE "$base/admin/api/v1/backup-operations/$id/leases/$lease" \
  -H "Authorization: Bearer $token"
python3 tools/verify_acquisition.py --metadata "$metadata_id" \
  --key-file /secure/backup.key --deployment "$expected_deployment"
./tesserad -config /etc/tessera/restore.json -restore "$archive_id" \
  -backup-key-file /secure/backup.key -retirement-fence "$evidence_id"
```

For independent evidence use a new operation ID with `{"kind":"evidence"}`, then the same
status/lease/copy/verify steps. To restore with newer evidence, verify both sets against the same
previously trusted deployment identity; use the older archive and newer evidence ID in the restore
command. A bare SHA-256 match or an arbitrary deployment string read from untrusted files is
insufficient. The verifier checks sibling artifacts named by their IDs and never prints evidence. No
scheduled task, UI, remote destination or recovery-kit export is created by this interface. The
existing offline `-backup` command remains a local archive adapter outside the acquisition inventory
and requires all serving processes stopped; use the HTTP interface for published pairs.

Upgrade compatibility: the legacy manual route retains its JSON `backup` string and BackupNow
retains its local path result, but archives now live inside private published set directories. Do
not construct a host path from the legacy response or enumerate timestamp filenames for copies.
Pre-upgrade `tessera-backup-*.enc` files are left untouched and outside the new bounded set
inventory; move them to independently managed storage before enabling the new workflow. Provision
space for those old files separately. Only one runtime owns each acquisition directory. The archive
includes this runtime's identity registry; other PostgreSQL compositions' independent authentication
registries and host-native tables are not captured. All changes to the captured registry and its SQL
grants must pass through its owning authority/Freeze boundary. Multiple writers of one registry, or
outside SQL grant edits bypassing that boundary, cannot promise registry/grant coherence and are
unsupported.


The independent-registry limit is enforced for upgraded standalone runtimes: each registers a shared
PostgreSQL session lock before opening its authority. Archive capture must upgrade that lock
exclusively through S and registry capture; with another registered owner present it fails instead
of publishing an incoherent registry/grant snapshot. Evidence-only acquisition still works. Multiple
standalone runtimes can continue serving, but online archive acquisition requires one registry owner
at capture. Stop other registry owners or compose record workers behind one owning authority;
capturing several independent registries needs a coordinated/shared authority design. Every
standalone runtime must be upgraded together for this guard to be meaningful.

## Completed-result catalog and collector credentials

The current reference server includes this catalog and collector interface.
They reuse the native scheduler, manual operation producers, immutable descriptors
and leases above. There is no additional scheduler or evidence-refresh authority.

`GET /admin/api/v1/backup-operations?limit=50` returns
`{"results":[<complete operation>,...],"next_cursor":"..."}`. The default page
size is 50, with an enforced range of 1–100. Both backup and evidence-only results
are included, whatever producer created them. Each result is the existing complete
operation representation: stable operation ID, kind, deployment, S/R labels and
exact artifact IDs, sizes and digests. Partial, failed, cancelled, rotated and
retention-expired results are absent. Timestamps are observations, not evidence of
retirement coverage. Artifact metadata still needs its existing MAC verification.

Pass `cursor=<URL-encoded next_cursor>` for the next page. Omit `limit` or repeat
its original value; changing it is `410 invalid_cursor`. Cursors are authenticated,
deployment-bound, valid for 15 minutes from the first page, and survive restart
with the same deployment and backup key. Malformed/tampered cursors return
`410 invalid_cursor`; elapsed cursors return `410 cursor_expired`. Unknown query
parameters and duplicate parameters are refused.

Pagination uses ascending lexical operation-ID order and the maximum complete ID
seen on the first page as an upper bound. It is a traversal of the live retained
set, not a frozen snapshot or event log: publication behind the cursor is found on
the next full rescan; publication ahead of the cursor within the upper bound may
appear in this traversal; rotation can remove unseen entries. An entry remaining
complete throughout the traversal is returned once. Repeat full scans and deduplicate
by operation ID. No ordering of S/R or evidence freshness is implied by ID order.

Catalog reads do not acquire leases. Lease a selected ID before copying. If it has
rotated/expired, acquisition fails explicitly with `410 expired`, never substitutes
another set. Once status retention ends, an old valid ID returns `410 expired`;
a valid unknown recent ID returns `404 not_found`. A collector asking for metadata
of a running/failed/cancelled operation receives `404 unavailable`, with no partial
result details. A bounded catalog cannot recover already rotated results.

### Provision and revoke retrieval-only authority

A signed-in server administrator with existing `administer` consent provisions a
collector using a caller-generated 32-character lowercase random hexadecimal ID:

```sh
collector_id="$(openssl rand -hex 16)"
curl -f -X PUT "$base/admin/api/v1/backup-collectors/$collector_id" \
  -H "Authorization: Bearer $administrator_token" -H 'Content-Type: application/json' \
  --data '{"expires_in_seconds":2592000}'
```

The `201` response contains `id`, `created_at`, `expires_at`, and a bearer `token`
shown only at issuance. Store it privately in the collector, not the ordinary
application frontend. Lifetime must be 60–31536000 seconds (at most 365 days);
there are at most 64 retained credentials. Only a SHA-256 token digest is stored
in the private acquisition directory, outside backup archives. Credentials survive
restart. Do not roll back the collector registry when restoring an archive; for a
fresh acquisition directory provision new credentials instead.

An existing credential ID returns `409 conflict` without changing its secret or
expiry. If the issuance response is lost, use administrator-only
`GET /admin/api/v1/backup-collectors` to see IDs and expiry (never secrets), revoke
the uncertain ID, and provision a fresh ID. Expired entries can be reclaimed when
provisioning another credential. No credential is automatically renewed.

`DELETE /admin/api/v1/backup-collectors/{id}` revokes the credential durably and
returns `204`, including for an already absent valid ID. The collector cannot
provision, list or revoke credentials. A persistence error is reported as failure;
collector authentication then fails closed until recovery/restart, while backup
production is unaffected. A failed revocation is not an acknowledgement of durable
revocation.

Use the collector bearer instead of a human token for exactly these routes:

| Route | Authority |
| --- | --- |
| `GET /admin/api/v1/backup-operations` | Completed-result catalog |
| `GET /admin/api/v1/backup-operations/{id}` | Complete result metadata only |
| `PUT` / `DELETE .../{id}/leases/{lease}` | Existing lease acquisition, renewal, release |
| `GET .../{id}/artifacts/{artifact}` | Exact member of the selected operation, with `Tessera-Backup-Lease` |

All requests require fresh authentication. Revoked, expired and malformed collector
tokens receive `401 authentication_failed`. Valid collectors attempting operation
PUT/DELETE receive `403 permission_denied`; general administration and record/content
authentication do not recognize these credentials. They authorize no backup/evidence
creation, cancellation, set deletion, configuration changes, key export, restore,
account/dataset administration or record/content access. No administrator fallback
is provided. The only permitted mutations are lease bookkeeping.

Revocation/expiry refuses new requests, including lease renewal and release. Requests
already admitted at their authentication check may finish; admitted artifact streams
retain open-file protection and a two-minute write deadline. Existing leases
expire naturally within five minutes and may temporarily constrain rotation. Neither
credential revocation nor expiry stops native scheduling or person-authorized manual
operations. New evidence still requires an explicitly authorized administrator
operation; the collector only discovers and retrieves its completed result.


## Live native scheduler settings

[Native scheduler administration](backup-scheduler.md) specifies the separate, administrator-only
observation and durable interval/retention request surface. It uses the same acquisition admission,
publication and leases. Retrieval-only collectors cannot observe or change scheduler settings.
Effective policy and cleanup convergence are separate; new admissions yield to an accepted update,
and evidence-only results cannot displace the final archive/evidence pair. This interface changes neither record nor content protocol.
