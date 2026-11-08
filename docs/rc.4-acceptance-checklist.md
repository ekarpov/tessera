# rc.4 acceptance checklist: app and server teams

Prepared 6 November 2026. **All team results start as NOT RUN.** This document
requests qualification; it does not record acceptance or authorize deployment,
publication or the final release.

## Candidate and decision rule

| Item | Required value |
| --- | --- |
| Backend tag | `v0.1.0-rc.4` |
| Backend commit | `5691f26bf50f52b625b18fb2b9e2fb9959b59d3b` |
| Runtime version | Binary and served discovery both report `v0.1.0-rc.4` |
| Data | Isolated, disposable synthetic fixtures only |
| Existing backend evidence | Annotated tag and `git notes --ref=ci`: clean-commit SQLite/PostgreSQL gate, integration, authentication, faults and WebKit passed |
| Consumer evidence still owed | Native app/device, web Console integration, and server-team deployment/backup/recovery results on the candidate |

Acceptance is for a **revision pair**, not just a tag: record each app/Console
revision and the backend commit above. Also record server packaging/configuration
revisions. Run each applicable case on every deployment profile and platform the
client release will ship. At minimum, native acceptance includes a Release build
on a physical supported device, the supported simulator, and the real system
browser/authentication session. Website acceptance includes its supported desktop
and mobile/narrow browsers. Include light/dark presentation checks.

An applicable case must PASS. FAIL, NOT RUN, an unexplained skip or missing
evidence blocks sign-off. N/A needs an explanation and maintainer agreement;
an unavailable shipped feature is not N/A. Backend-only results and headless
WebKit do not substitute for these independent runs. A fix changing candidate
behavior requires a new candidate and rerunning affected cases; retain earlier
results with their original revision pair.

The maintainer/server team supplies the qualification server. The app team does
not start servers or change shared deployment configuration. Coordinate destructive
cases and resets with the server team, using dedicated fixture accounts/storage.

The [App Review freeze](releasing.md#app-review-freeze) takes precedence over every
setup, configuration, account, invitation, restart and reset step below. Run these steps
only on a separate qualification deployment with fresh fixtures. Review accounts and
invitations 1-3 are excluded. Final-release qualification additionally requires the
[release packet, app walks and consumer confirmation](releasing.md#compatibility-and-consumer-qualification).

## 1. Joint setup and fixture sheet

| ID | Owner | Required check and expected result | Evidence back |
| --- | --- | --- | --- |
| P01 | Server | Verify the deployed binary/image comes from the candidate commit. Check binary version and discovery through the public edge. Record the engine and packaging revision. | Tag/commit, image or binary digest, both version results, packaging revision, timestamp. |
| P02 | Server + app | Check HTTPS/system trust and the full CSP through the actual reverse proxy. Remove the old callback-scheme CSP workaround for this rc.4 qualification deployment. Preserve Tessera's generated style hashes, default deny and `form-action 'self'`; only configured local PNG paths may load images. | Sanitized complete CSP, proxy configuration revision, TLS/trust result and browser CSP-violation result. No certificate-validation bypass. |
| P03 | Both | Seed an owner, two members, a study administrator, two participants, a server operator, an ordinary OAuth client and a separate untrusted client. Give peers distinct private sentinel data and explicitly shared views, including an ungranted household/study boundary. For device cases use a template issuer with device redemption, moderated sharing, `erase_with_account` and an issuer-group share. Establish expected grants and prove an authorized owner can read each private sentinel before its negative tests. | Versioned fixture description with role aliases, expected grant/view matrix and fixture-generator revision. Use aliases rather than credentials or record identifiers in the returned report. |
| P04 | Server + web | Provision a dedicated first-party Console identity with exact HTTPS origin/callback and explicit data/admin policy. Keep native and ordinary web clients separate. Confirm the web app uses the configured ID and does not dynamically register a replacement. | Sanitized effective policy, configuration digest, exact-binding match results and Console revision. |
| P05 | Both | Keep initial sign-in and reuse cases independent. Use a fresh client/approval for defaults; use deliberate existing approval for reuse; use `prompt=consent` when a case needs the picker. Maintain a second installation for revocation-isolation tests. | Fixture state recorded per case: fresh/reused/forced review and installation aliases. |

Record real OS, browser, device, build configuration and certificate-trust setup.
Do not silently replace a physical-device case with simulator or Linux WebKit
results. Use normal server time; accelerated client-session tests must identify
their controlled test clock and must not change the shared server's clock.

## 2. App team: native app and ordinary OAuth web clients

The native login stays in the system browser (for example
ASWebAuthenticationSession). No embedded login, password proxy, automatic consent
submission or broader service credential is an acceptable workaround.

| ID | Required tests and expected result | Evidence back |
| --- | --- | --- |
| A01 | Run the independent vector, live HTTP/record and content suites against rc.4 using disposable fixtures. Include real create/read/update/delete/query/paging, conflict/expectation guards, lost-response idempotency recovery, and upload/download byte equality. Exercise the app's actual sync and record display in its Release build. | Client revision, suite commands, pass/fail/skip counts, record/content integrity outcomes and any fixture mismatch explained. Do not report a suite that skipped as passed. |
| A02 | Fresh sign-in: authenticate, review consent, Allow, tap **Return to app**, and reach the exact registered native callback in the actual app. Validate state/issuer and exchange the code once. Repeat with another independently registered native scheme. Ordinary OAuth web clients may reuse consent but still retain the explicit return link. | Per-platform results, short sanitized recording/screenshots, callback scheme/host/path without query, state/issuer match booleans, exchange and replay outcomes. |
| A03 | Cancel before sign-in, Cancel at consent, and close/dismiss the authentication sheet. Cancellation returns `access_denied` with matching state and no code when a callback is delivered. Dismissal before authorization grants nothing. Closing after Allow/completion does not undo that grant: recover by normal sign-in without duplicate invitation redemption. Invalid callback, wrong verifier/client/redirect and code replay must not connect the app. | Each outcome separately, error/status codes, no-code/no-extra-grant assertions. Never attach raw callback URLs or codes. |
| A04 | Study join sends `scope=read write` for an account holding Read/Write/Erase but not Insert. Initial summary and hidden choices select Read + Write only. After Allow, dataset access has Read/Write, no Schema/Erase/Insert, and the join's first participant/source writes succeed only after explicit study consent. | Scope, sanitized access booleans, study-consent action result and first-write outcomes. Signing in alone must not enroll someone. |
| A05 | Open **Change what the app may do**, untick Write and Allow. Access is read-only; the app detects missing Write and refuses the join with a clear message before attempting its required writes. Separately request Insert alone without holding it: an unavailable-hint warning is visible and Read is not silently substituted. With no hint, initial defaults are only held Read/Insert. | Before/after grant booleans, refusal UI and write-attempt count/outcome; screenshots of warning and edited choices. |
| A06 | Request Schema, Erase and administration explicitly on a fresh/forced-review flow: none is prechecked. Confirm Allow/Cancel remain visible while choices are collapsed, and only submitted checks determine the grant. With several datasets, a choice must not grant rights absent there; use the documented conservative defaults. | Collapsed/expanded screenshots, selected dataset alias and actual access. If testing a sensitive operation, use its existing explicit authorization/confirmation path. |
| A07 | Create an account through one invitation, then perform normal sign-in with the retained hint. Recover after an expired page and after a spent/replayed form. Hint survives a restart of authorization; credentials/CSRF and consent are fresh where review is needed. After account creation or a failed join, reauthorize that account; invitation reuse is refused and no duplicate account/join is created. | Account-count/side-effect assertions, recovery results, scope retained, invitation-replay refusal. Observe normal expiry or identify a supported test fixture; do not change server time. |
| A08 | Reauthorize with the same scope and then a narrower scope: fresh authentication can reuse the most recent live consent for the same account/client/callback/previous dataset. New rights require review. Test `prompt=consent`, another account/callback/dataset, unavailable rights, revoked/withdrawn consent and an older broader approval followed by a newer narrower one. None may silently expand access. Omitted scope retains only prior held Read/Write/Insert; explicit empty scope is distinct. | Review-shown/skipped matrix and actual access for every resulting installation. Include a paging continuation through reused/current credentials. |
| A09 | Refresh rotation succeeds; replay of a spent refresh token revokes its family. Suspension and installation revocation block access/refresh; grant/share expiry and role withdrawal stop affected access. Per-installation revocation/reuse detection leaves a separate independently authorized installation usable; account suspension applies to all installations of that account. Recover through normal sign-in, not a reused invitation. | HTTP/error results, affected/unaffected installation aliases and observed UI state. Confirm a remembered/browser session does not override server refusal. |
| A10 | Run invitation/device preview and single-use redemption, second-device installation, removal and rebinding. A member can manage their own sharing/group share, see own usage and an administrator's stopped-share indication, but cannot manage a peer's dataset. Leaving first requires exact own-data erasure confirmation; confirmed leaving removes that account/data and invalidates all its devices. A device installation cannot erase while retaining membership merely by assuming Erase permission. | Fresh and rebound-device results, wrong/missing/confirmed erasure outcomes, old-device `401` assertions and peer refusals. |
| A11 | Check the role/privacy matrix below with actual app views and API access, including referenced content. Private data must not leak through records, lists, counts, paging, metadata-visible scope or media. Authorized-view concealment is `404 unavailable`, not evidence of deletion. Explicitly shared data stays within its type/frozen-view/expiry bounds. | Allowed/denied matrix, error codes and synthetic content integrity results; no record payload dump. |
| A12 | Test the branded system-browser pages at supported phone/narrow and desktop widths, light/dark, keyboard/touch and supported text-size settings. Verify no clipped actions/horizontal overflow, readable text/focus/contrast, correct username/current-password autofill, and the actual supported password manager. Registered client name/icon remain visibly separate from operator identity/host. No CSP violations occur in legitimate flows. | Platform/build matrix, sanitized screenshots, keyboard/focus notes, measured contrast result, password-manager/version and actual callback-dispatch result. |

### Role/privacy matrix (app and server teams verify independently)

| Actor | Allowed | Must remain unavailable/refused |
| --- | --- | --- |
| Household owner | Household/account administration already granted to that account, own data and explicit shared views | Members' private records solely because of ownership or administration |
| Member A | Own records, authorized shared dashboard and own sharing controls | Member B's unshared records, content and account/dataset administration |
| Study administrator | Study/participant administration and data already granted to that researcher account | Unrelated participant/study/private data not granted to that account |
| Participant A | Own records, participation and consent controls | Participant B's private data; implicit study enrollment/consent merely from sign-in |
| Server operator without data grants | Authorized server operations | Private record access or credential changes that require administering the affected datasets |

These are fixture expectations, not role names interpreted by Tessera. The server
team must provision grants/shares to express them. Repeat after changing a role,
ending a share and expiring a grant, not just immediately after initial login.

## 3. Web team: first-party Console integration

| ID | Required tests and expected result | Evidence back |
| --- | --- | --- |
| W01 | Link directly to the configured authorization request; remove the separate welcome/remember-me step. One compact branded card has account/password, unchecked **Keep me signed in**, and one Sign in button. One successful submission enters the exact Console callback without a permission picker or another click. No dataset selector or account-management permission checkbox is shown during this mode. | Actual browser recording for desktop and mobile/narrow, Console revision and redirect-match result. Reuse A12's accessibility/password-manager matrix. |
| W02 | Test checked and unchecked persistence. Consume `console_session` only from the successful validated PKCE code exchange, matching its configured origin. Unchecked is `false`/`0`; checked is `true`/`2592000`. Callback/query or token-request `remember` parameters cannot override the signed-in choice; ordinary OAuth responses do not grant this mode. Wrong-password retry preserves the live choice; a fresh/recovery sign-in starts unchecked. | Sanitized handoff fields, origin/state/issuer match booleans, both UI paths and tampering outcomes. No raw code/token/cookie values. |
| W03 | Verify opaque session cookies and protected durable refresh storage. Unchecked sessions are not made durable. Checked sessions have an absolute cap of 30 days from sign-in; rotation/refresh and browser restart do not reset it. Logout, revocation, suspension and access changes remain effective. Tessera access-token lifetime is unchanged. | Cookie attributes with values removed, encrypted-storage/control attestation, configured maximum and controlled-clock/session-expiry tests. No 30-day real-time wait is required; identify accelerated fixtures. |
| W04 | Change the operator policy and the signed-in account's current grants; check fresh requests and refresh honor the ceilings and role/privacy matrix. Dataset choice occurs in normal site navigation. Schema/record-protocol Erase/creation are not inferred from Console designation. Sensitive operations keep their confirmations and required fresh authentication; Console sign-in does not perform study consent. | Before/after access/admin outcomes and denied-operation results. Show an unaffected authorized workflow still works. |
| W05 | Copy the operator/client name or icon in a dynamically registered client; add origin/first-party parameters and equivalent scope hints. It must stay an ordinary client with consent. Test an unlisted identity and mismatched callback origin, port, path and query. Existing public registrations cannot be promoted into reserved Console identities. Removing Console policy must not fall back to ordinary permission approval for that reserved identity. | Negative-test matrix, registration/binding refusals and proof no Console session metadata or unintended grant was issued. |

The Console's cookie/refresh storage and introductory page are client-owned.
The server's generic branding configuration is not the first-party designation.
Native clients must not be switched to the Console identity to bypass consent.
See [the first-party contract](self-hosting.md#first-party-console-sign-in) and
[ordinary/native client contract](self-hosting.md#native-and-ordinary-oauth-client-presentation).

## 4. Server team: packaging, operation and recovery

Run the team's actual image/pin/ownership and deployment gates, not just a source
checkout's Go tests. Qualify each engine/storage topology intended for the first
release; state which source/restore combinations were actually run. Existing
backend CI covers SQLite and PostgreSQL, but not the team's packaging or off-machine
workflow. Coordinate these cases with the app team; destroy only their isolated
qualification fixtures.

| ID | Required tests and expected result | Evidence back |
| --- | --- | --- |
| S01 | Complete P01/P02 and the team's image/pin-change/ownership gate. Confirm effective configuration, certificate trust, data/key permissions and actual encrypted-storage controls. Exercise ordinary/native and first-party callbacks through the real edge without a CSP relaxation. | Gate commands/results, artifact digest, version/discovery results, sanitized proxy/CSP/configuration evidence and encryption/ownership attestation. |
| S02 | Test optional branding and an unbranded deployment. Valid transparent operator/client PNGs render; invalid/remote/active-format/oversized assets and unsafe/low-contrast palettes are refused at the documented boundary. Public icons cannot change operator trust/policy. Server-side request, grant and registration bounds still refuse excess input. | Valid/negative matrix, status/error results, safe rendering evidence and configuration revision. |
| S03 | Exercise current authority and isolation independently of app UI: role/share changes, expired grants, suspended accounts, withdrawn/revoked installations, exact callback policy removal/change, and unaffected sibling installations. Ensure no Console or ordinary client can acquire broader service credentials or copy Console policy into device credentials. | API matrix against fixture aliases, relevant access flags/error codes and peer-data denial results. |
| S04 | On protected copies of synthetic pre-upgrade state, follow the upgrade procedure: preserve the backup, configuration, keys and independent current retirement evidence. Verify rc.4's registry-format-5 behavior and restart. Demonstrate rollback by restoring the pre-upgrade backup with the appropriate old version in an isolated target; never run an older binary on format-5 writes. | Before/after format/version results, backup verification, restart and rollback outcomes. Pre-releases have no migration guarantee; final `v0.1.0` starts from fresh storage. |
| S05 | Run scheduled and manual backup acquisition while normal synthetic reads/uploads/writes continue. A completed archive has its paired immutable evidence/metadata; successful archive publication updates `server.last_backup_at`. Evidence-only/failed attempts do not falsely advance it. Restart preserves/repairs the completed result and the reported timestamp. | Operation-alias outcomes, sanitized overview timestamps, exact artifact sizes/digests and foreground-operation integrity/progress results. No latency promise inferred from one run. |
| S06 | Exercise the Console's actual scheduler/retention controls: update interval/keep, restart and verify durable effective values; stale/conflicting requests are refused and identical retries reconcile to one outcome. Lose an acknowledgement and reconcile by the same request ID rather than guessing. Reduce retention with a protected copy/lease; expect documented deferral/refusal and preserve the final complete archive/evidence pair. | Request aliases, revisions/outcomes before/after restart, convergence/refusal results and final pair-integrity proof. |
| S07 | Use the real collector/retrieval path: catalog a completed set, obtain an independent lease, copy exact artifact IDs and verify size/digest/pair authentication. Test unauthorized callers, missing/expired/revoked leases/credentials and collector attempts to create backups or change scheduler policy. Rotation must not delete protected artifacts mid-copy. | Collector scope/refusal matrix, lease/rotation results and offline verification outcomes. Keep keys and encrypted artifacts in protected storage, not the acceptance report. |
| S08 | Restore a copied, verified archive into a fresh isolated target using independently retained current retirement evidence and trusted producer/key material. Verify account/grant coherence, client reconnect/re-authorization as needed, synthetic records and committed content integrity. Include an erasure after the archive: newer evidence must keep that dataset/content retired. Wrong key/provenance, altered bytes, missing evidence and an existing target must refuse safely. Inventory client/host-owned tables and session state excluded from the Tessera archive, and qualify their separate recovery/session-handling procedure. | Actual source/target engine/profile, verifier and restore commands with secrets removed, integrity/authority comparisons, post-archive erasure result and each negative outcome. Cursors/transfer URLs are not expected to survive restore. |
| S09 | Verify the intended deployment fits the supported backup topology: one owning identity authority for coherent PostgreSQL registry/grant capture; all writers upgraded together; shared local hard-link/flock semantics; at most 256 required content files. Exercise required bound/refusal cases without losing the last recovery pair. | Process/authority topology, filesystem type and probe results, required-file count and bounded-operation outcomes. Unsupported required topology blocks adoption; do not waive it as N/A. |
| S10 | Inspect controlled-test application, server and reverse-proxy telemetry for credential or record leakage. Check recovery material, keys, producer trust and retirement evidence are retained separately under the team's access/retention controls. | Sanitized inspection/negative assertions and operator attestation. Do not attach raw logs containing passwords, tokens, codes, cookies, SQL, record payloads or user-supplied identifiers. |

Use [upgrade](self-hosting.md#upgrading),
[acquisition/verification/restore](backup-acquisition.md),
[scheduler administration](backup-scheduler.md) and
[backup storage-model limits](backup-acquisition.md#content-and-deployment-limits)
as the contracts. Do not copy an unsupported topology into a release promise.
Real cloud and separate-host recovery are not already accepted by backend tests;
if the team's intended workflow depends on them, report that dependency and its
independent evidence for explicit review. Recovered producer identity does not
by itself authenticate arbitrary artifacts. There has been no complete privacy
audit. Filesystem/registry/file-count restrictions remain stated release limits.

## 5. Evidence packet each team returns

Return one report for each tested revision/profile combination, with a stable
artifact location accessible to the maintainer. Keep the raw artifacts in the
team's protected store; return sanitized references, not secrets or database dumps.

```yaml
team: native-app | web-console | server
result: NOT_RUN | PASS | FAIL | BLOCKED
run_date_and_timezone: <actual run date and timezone>
backend_tag: v0.1.0-rc.4
backend_commit: 5691f26bf50f52b625b18fb2b9e2fb9959b59d3b
client_commit: <native/Console commit, or not applicable with reason>
server_packaging_commit: <revision>
binary_or_image_digest: <digest>
reported_binary_version: <observed>
reported_discovery_version: <observed>
platforms: <device/model, OS, browser, build configuration, password manager>
engine_and_storage_profile: <actual source/restore engine and topology>
fixture_revision: <generator revision and role-policy matrix reference>
configuration_digest: <sanitized effective configuration reference/digest>
case_report: <case results table reference>
artifacts: <protected bundle reference plus artifact checksums>
blockers: <issue references, or none>
exceptions: <N/A/skip/untested items and maintainer decision, or none>
tested_by: <team reviewer>
team_signoff: <explicit accepted/rejected/pending and date>
```

For **every applicable case ID**, provide:

| Case ID / platform | Status | Expected vs observed | Reproduction or suite command | Evidence reference | Defect/exception |
| --- | --- | --- | --- | --- | --- |
| `<ID / platform>` | `NOT RUN` | `<fill after execution>` | `<sanitized>` | `<artifact path/checksum>` | `<issue or approved N/A reason>` |

For automated suites, include total passed/failed/skipped counts and the actual
command, with credential-bearing arguments replaced by placeholders. For manual
flows, include ordered steps, observed state/error and a short sanitized recording
or screenshots where useful. Report grants as booleans and callbacks as
scheme/host/path plus binding-match results; omit query strings and credential
values. Do not attach unredacted HAR files, password-manager vault contents,
private keys, connection strings or an archive together with its decryption key.
Do not enable record-bearing debug logs just to collect evidence.

A failure report needs the exact revision pair, case ID, fixture preconditions,
expected/observed result, safe reproduction, impact and issue reference. Preserve
failed-run evidence when rerunning a fix; do not replace it with a green summary.

## 6. Return and release decision

- [ ] App team returns A01-A12 and P02/P03/P05 results for all shipped native/ordinary-client profiles.
- [ ] Web team returns W01-W05 and its A12/P03-P05 coverage for the actual Console integration.
- [ ] Server team returns P01-P04 and S01-S10 results for the intended packaging/storage profiles.
- [ ] Teams reconcile shared role/privacy and revocation results; no applicable failure or unexplained skip remains.
- [ ] Maintainer reviews blockers, evidence and explicit limitations and records candidate acceptance.

These are independent acceptance gates, not permission to squash into `main`,
tag `v0.1.0`, change consumer pins, delete branches/tags or make the repository
public. Those remain the separate approved steps in the
[release procedure](releasing.md).
