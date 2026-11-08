# Native backup scheduler administration

The reference server produces scheduled backups; collectors discover and retrieve
completed results. This administrative interface exposes observations and durable cadence/
retention settings. It changes neither the record nor content protocol and cannot change
destinations, keys, credentials, deployment lineage or host controls.

## Quick guide

Configure [online acquisition](backup-acquisition.md), GET the observation to obtain its
scope/revision, persist a request ID, then PUT a settings operation. Reconcile a lost
response with the same ID; never infer failure or submit a new command to retry uncertainty.
The detailed admission, persistence and recovery rules follow.

## Authentication and routes

All three routes require ordinary bearer authentication, `administer` authority and current
server-administrator standing. Ordinary clients obtain that authority through explicit consent;
the [operator-designated Console](self-hosting.md#first-party-console-sign-in) uses its bounded policy.
Authentication and authorization are repeated on every request,
including retries and lookup. Retrieval-only collector tokens are not recognized here (`401
authentication_failed`); an authenticated caller without the server right receives `403
permission_denied`. Responses have `Cache-Control: no-store`. No request stores an account ID or
credential. An already durably accepted command continues after sign-out/revocation: it is a retained
administrative command, not a background credential. Further observation/lookup still requires fresh
authority.

| Route | Successful response |
| --- | --- |
| `GET /admin/api/v1/backup-scheduler` | `200`, sanitized observation below |
| `PUT /admin/api/v1/backup-settings-operations/{request_id}` | `202` pending; `200` replay of effective outcome; durable rejected outcome as described below |
| `GET /admin/api/v1/backup-settings-operations/{request_id}` | `200` retained pending/effective/failed outcome |

PUT accepts exactly three required, non-null members. Numbers must be JSON integers, with
`every_hours` in 0–87600 and `keep` in 1–1024. Unknown, duplicate, missing or malformed members and
invalid values are `400 invalid_request` or `400 invalid_field`, without spending an ID.

```json
{"expected_revision":"<scope>.<random>","every_hours":24,"keep":7}
```

`scope` is `<deployment>.<settings-epoch>`: two independent 32-character lowercase random hexadecimal
values. Revisions append a third such random value. Request IDs are
`<scope>.<unix-seconds>-<32 lowercase random hexadecimal digits>`. Obtain scope and revision from
observation, persist the caller-generated ID before PUT, then retry that same ID and payload or GET
its outcome after any lost response. JSON key order/whitespace do not affect binding: comparison is
the complete decoded tuple (expected_revision, every_hours, keep). Deployment and epoch are bound
by the ID and durable journal. Differing valid payloads under a retained ID are `409 conflict`
(`request_reused`). Existing identity is checked before the current revision and before write admission, so retained
identical retries remain readable while settings application is stalled.

First admission requires the issued seconds within five minutes of the server's admission clock.
A mismatched deployment/epoch is `409 conflict` (`request_scope_mismatch`); a stale revision is a
**durably retained** failed outcome, `409 conflict` (`stale_revision`). A second administrator while
one request is pending gets a durably retained `409 conflict` (`settings_pending`). A short concurrent
journal write can instead refuse `409 conflict` (`settings_busy`) without admission; retry the same
ID. Details use fixed `reason` codes; PUT rejection details include the retained `operation` when
one exists. GET of that rejected operation returns `200` with state `failed` and its safe failure.

The dedicated journal holds at most 1024 requests and one pending command; exhaustion is `413
limit_exceeded`. Terminal outcomes remain for seven days after resolution. Pending commands never
expire or get purged while unresolved. Forgotten/expired IDs cannot become a new mutation: stale
issuance is `410 expired`, and a valid recent unknown ID is `404 not_found`. A persisted nondecreasing
clock floor prevents a backwards wall-clock adjustment from reviving purged IDs. Normal UTC clocks
and caller skew within five minutes are assumed; backwards clocks can refuse fresh IDs until they
catch up. Restart preserves this floor. A new settings epoch rejects every old request ID.

State transitions are `pending -> effective` or an admission rejection directly to `failed`.
Acceptance is fsynced before 202. Effective policy, fresh revision and effective request outcome are
one atomic journal replacement with directory fsync. A no-op payload with the current revision is
still a revision-changing command. An interrupted accepted command resumes at startup, never twice.
A persistence error is `503 dataset_unavailable` (`settings_unavailable`): it is uncertainty, not a
failed mutation. Settings/admission fail closed until restart reads the journal; no old-config
fallback or compensating revision is invented. GET also reports 503 while persistence is uncertain.

A successful acceptance (`202`) has this shape (opaque values abbreviated):

```json
{"id":"<scope>.<seconds>-<random>","expected_revision":"<scope>.<previous>","every_hours":24,"keep":7,"state":"pending","accepted_at":"2026-10-17T12:00:00Z","expires_at":"2026-10-24T12:00:00Z"}
```

After effectiveness, GET and identical PUT replay return `200` with the same binding, state
`effective`, `effective_at`, the new `revision`, and `expires_at` seven days after effectiveness.
For unresolved pending commands, the initial `expires_at` is provisional and does not expire the
command. A rejected command has state `failed` and `failure` (`stale_revision` or `settings_pending`),
with no effective timestamp/revision. All errors use the ordinary administration envelope, e.g.:

```json
{"error":{"code":"conflict","details":{"reason":"settings_busy"}}}
```

Durable failed PUT responses additionally include the complete retained command as
`error.details.operation`. Failed GET outcomes are successful lookups (`200`), not a new rejection.
Authentication failures are `401`; standing/consent failures are `403`. Invalid request members or
values are `400`; unknown recent requests are `404`; expired requests `410`; request capacity `413`;
identity/revision/pending conflicts `409`; uncertain settings persistence `503`. GET requests with
bodies are `400 invalid_request`. All administration request bodies have the existing 1 MiB bound.

Without provisioned native backups, GET still returns a not-configured observation and the settings
routes refuse `403 not_supported` with reason `not_configured`. They cannot provision paths or keys.

## Observations and cadence

Example (opaque values abbreviated):

```json
{
  "instance_id":"<random>","observed_at":"2026-10-17T12:00:00Z",
  "history_scope":"process_instance","configured":true,"available":true,"reason":"ready",
  "enabled":true,"settings":{"every_hours":24,"keep":7,"revision":"<scope>.<random>"},
  "settings_scope":"<scope>","pending_request_id":null,
  "last_scheduled_attempt":{"started_at":"2026-10-17T11:00:00Z","finished_at":"2026-10-17T11:01:00Z","outcome":"succeeded","operation_id":"<id>"},
  "active_scheduled_operation":null,"next_attempt_at":"2026-10-18T11:01:00Z","next_reason":"scheduled",
  "retention":{"retained":7,"target":7,"state":"converged","reason":"at_target"},
  "settings_export":{"version":1,"deployment":"<deployment>","every_hours":24,"keep":7,"revision":"<scope>.<random>","exported_at":"2026-10-17T12:00:00Z"}
}
```

Instance ID is random on each process start. Attempt history is instance-scoped, reset on restart;
it never incorporates manual backups, evidence refreshes or `server.last_backup_at`. A dead process's
operation is never active in the new process. Observations do not certify writer health or artifact
freshness. Unavailable settings have null effective settings/export and reason `settings_unavailable`.
Not-configured observations have null settings/scope/export/retention and reason `not_configured`.

The existing immediate first enabled startup attempt remains. **The old ticker cadence changes:**
subsequent attempts are one effective interval after the preceding scheduled attempt terminates or
is refused, without queued ticks or catch-up. An idle interval change schedules from its effective
time; enabling schedules immediately; disabling clears the next attempt. Retention-only changes do
not reset cadence. Settings apply after any scheduled/manual/evidence acquisition's safe terminal
boundary and do not cancel it. Once acceptance is being persisted, new acquisition admissions yield
to the pending command; repeated manual work cannot starve it.

During a scheduled attempt, next time is null (`in_progress`). Other null reasons are `disabled`,
`not_started`, `stopped`, `settings_pending`, `settings_unavailable`, and `acquisition_unavailable`.
A due time is intent, not an admission/completion guarantee. Busy/capacity refusals are recorded once
and retried after the interval, never in a busy loop. Manual/evidence work never resets the cadence.
Attempts use fixed outcomes: `in_progress`, `succeeded`, `failed`, `cancelled`, `refused_busy`,
`refused_capacity`, `refused_settings_pending`, `refused_unavailable`. No underlying error is exposed.

## Effective policy versus pruning

A policy is effective when its journal commits; pruning is a separate convergence activity. The
control worker handles at most one deletion per pass outside retrieval/lease/observation locks.
It retries on acquisition completion, lease release, stream close and a 30-second timer (including
lease expiry). Settings and creation pause at the acquisition boundary, not device transactions.
Administrator GETs, catalog discovery, leases, retrieval and admitted streams remain available while
application or deletion is stalled. A short serialized journal-intent update may precede deletion.

`retention.retained` includes complete sets and expired sets whose durable deletion is still pending;
`target` is the effective keep. State/reason pairs are `converged/at_target`,
`pending/acquisition_in_progress`, `pending/pruning`, `blocked/leases`, `blocked/transfers`,
`blocked/reserved`, `degraded/cleanup_failed`, or `degraded/journal_unavailable`. Expiry/deletion intent
is persisted before removing bytes. Cleanup failure after effectiveness never fails or rolls back
the settings command. Journal uncertainty blocks new creation until reconciliation/restart.

While over target or deletion is pending, new creation refuses `413 limit_exceeded` with reason
`retention_pending`; protected files are never revoked to make room. Existing bounded status/byte
limits remain. A reduction cannot add sets, so an over-target inventory is bounded by the previous
producer limit plus its single staging set (at most 1025 sets), until protection/cleanup permits
convergence.

Evidence-only sets count, but cannot replace an archive. Rotation and expiry preserve the final
complete archive/evidence pair, including during a keep reduction with newer evidence-only results. Batch
expiry reserves the newest accepted complete archive before retiring any entries, breaking equal
acceptance times by greatest operation ID, even when every archive's online status has expired.
With keep at least one, unprotected evidence sets can be removed instead, so no unprotected
above-target exception is needed. Protected sets can defer convergence. Evidence-only creation at
capacity refuses `413 limit_exceeded` (`last_recovery_pair`) if its only possible victim is the last
archive. An archive replacement may retire it only after successful durable publication. Status
retention may end access to old IDs; the retained physical pair is a bounded recovery safeguard,
not an extension of the online reconciliation window. Copy recovery results before ID expiry.

## Startup and sanitized recovery input

Durable effective settings load **before** acquisition startup rotation and scheduler startup. Static
`backup.every_hours` and `backup.keep` bootstrap only when the dedicated settings journal is absent.
An unreadable/corrupt journal refuses startup, never falls back. Pending accepted commands commit at
startup after the previous process's ownership is proved gone and before producer rotation.

Save `settings_export` separately when observing an acknowledged revision. The only new local input
is `-backup-settings-bootstrap /secure/settings-export.json`. With all processes stopped, an operator
recovering a lost settings journal can pass this version-1 sanitized export to the existing
`-config` startup; no configuration-file write or private-journal editing is required. This input is
accepted only when the settings journal is absent and the existing provisioned deployment identity
matches. It cannot adopt another lineage or change paths/keys. If a corrupt journal survives, retain
that failed storage for diagnosis and recover the backup storage from its supported independent
copy; the flag does not overwrite corrupt state or infer a safe latest revision from it.

Recovery installs exported values with a **new random settings epoch and revision**, not continuity
of the old revision. Prior pending commands are not imported; every old-scope ID/revision is refused.
The export is sanitized, not signed or a credential; use a trusted independently retained copy and
verify its deployment identity. Old archives/exports never prove latest settings. If storage holding
the journal is lost, durability is lost: bootstrap cannot know which later commands existed. Loss of
the deployment identity requires the explicit offline action below; the settings-only startup flag
will not mint/adopt a claimed old lineage. Collectors and acquisition descriptors are unchanged.


## Offline producer identity and effective-settings recovery

The current server includes producer recovery; older pre-releases may expose only the settings-only
startup flag. This reference-server operation restores the backup producer identity,
not any record dataset identity, account authority or storage contents.

Before loss, retain the expected producer ID independently from an authenticated
observation and keep a trusted copy of `settings_export` from
`GET /admin/api/v1/backup-scheduler`. Save the export object itself, not the whole
observation. The export is sanitized but **unsigned**; it is neither a credential
nor evidence of the latest policy. A claimed ID inside the export cannot establish
lineage. Supply the independently retained ID explicitly; do not derive that trust
input from the export being recovered.

Stop processes using the destination backup directory. Prepare a fresh private
backup directory and normal `-config` file naming it, with the separately retained
backup encryption key. Then run this action **before ordinary startup or any
provisioning command** initializes that backup destination:

```sh
tesserad -config /secure/server.json \
  -recover-backup-producer \
  -expected-backup-producer "$EXPECTED_PRODUCER" \
  -backup-settings-bootstrap /secure/settings-export.json
```

The action exits without opening record storage, starting a listener, taking a
backup or modifying the configuration. Use it alone; development mode, provisioning,
restore, backup and other command options cannot be combined with it. It requires
exclusive producer ownership and compares the expected ID with the fully validated
export. Unknown/duplicate/missing/null members, malformed timestamps, invalid
versions, revisions, bounds and identities refuse before destination initialization.

Permitted acquisition-directory states are precisely: absent, an existing private
empty directory, the producer lock alone, or the matching producer identity alone
(with an optional lock). A recovery-owned pending transaction or completed receipt
has the retry rules below. Any unrelated settings journal, operation inventory,
collector registry, artifact set or unknown entry refuses. Conflicting/corrupt
identities, symlinks and nonregular/private-file violations refuse; no incompatible
state is overwritten. Keep failed storage for diagnosis and select a different
fresh backup destination instead of deleting or editing private journals.

Recovery installs only the exported interval and retention count, with a fresh
random settings epoch and revision selected once per recorded attempt. Old request
IDs/revisions are refused. It imports no pending commands, approvals, accounts,
collector credentials, leases, operation inventory or artifacts. Start the server
normally afterwards; persisted recovered values take precedence over startup
configuration defaults and survive restart. Collectors must be explicitly
[reprovisioned under the destination's administrative authority](backup-acquisition.md#provision-and-revoke-retrieval-only-authority)
and rebound by their operator. Before rebinding, the operator compares the
independently retained expected producer identity and independently verifies the
artifact metadata MAC and artifact size/digests; never automatically accept a newly
discovered producer ID. The recovered producer ID does not authenticate an
arbitrary archive: artifact provenance, independently trusted expected identity,
metadata MAC/key and size/digest checks remain necessary.

### Interruption and retry

The operation holds the same producer lock as ordinary startup. It durably creates
a pending marker before installing either identity or settings. A validated intent
binds the expected ID and the complete canonical export, including exported revision
and timestamp, to the newly selected epoch. JSON whitespace/member ordering and
standard timestamp re-encoding are normalized; changing validated export values
changes the retry input. A different timestamp/revision with identical effective
values is not the same retry.

Staged files are fsynced and installed exclusively, and destination-directory
changes are fsynced. A final pending-to-completed receipt rename and directory fsync
commit both values before success is returned. Any I/O/fsync error leaves the result
uncertain: preserve the destination and repeat the same command/input. Normal
startup refuses a pending marker rather than minting identity or using defaults.
An identical retry resumes a valid intent, retains its chosen epoch and verifies
any already installed bytes before proceeding. An empty pre-intent marker with no
installed settings can safely begin an attempt because no epoch had been recorded.
A completed retry confirms the original result without replacing a journal; this is
the sole completed-receipt exception to the existing-journal refusal.

Missing/corrupt intent in a **nonempty** pending marker, mismatching installed files,
corrupt completed receipts and unrecognized state fail closed. An interruption
inside the initial atomic intent write can leave an incomplete temporary file;
that nonempty marker is not resumable. Preserve it and recover into a different
fresh destination using the same public command. Private marker deletion is not a
recovery procedure. Successful filesystem durability requires a filesystem that
honors file/directory fsync and exclusive hard-link publication; unsupported
filesystems return an error rather than success.

Ordinary startup permits a valid completed receipt and still validates the current
settings journal normally. Later live settings changes remain valid. Replaying the
recovery command after those changes, or after ordinary initialization has created
other acquisition state, refuses instead of restoring the earlier policy. Recovery
cannot discover commands acknowledged after the retained export or prove that the
old producer has stopped on another host. Stop/retire the old producer before
continuing its lineage elsewhere.

This is not migration, automatic enrollment, custody transfer or a general host
management API. Record/content restoration remains a separate supported operation.
