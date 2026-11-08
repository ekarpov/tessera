# Releasing

A release identifies verified code, states its limits and gives operators a recovery path.
`v0.1.0-rc.4` is the current tagged candidate. Independent consumer and server evidence
and explicit maintainer decisions govern the final release; the scoped first-release
app-test waiver is recorded under [preconditions](#preconditions). Backend checks alone
do not authorize the final release.

Draft [v0.1.0 release notes](v0.1.0-release-notes.md) describe the current compatibility
changes and the qualification packet still owed. They are not an acceptance record.
Use the [928c09a S08 replacement acceptance handoff](v0.1.0-s08-acceptance-handoff.md)
for the current CI runtime, candidate-specific app/server checks and evidence requirements.
The earlier [c46beb0 handoff](v0.1.0-acceptance-handoff.md) retains the base case bodies.

## Quick guide

1. Confirm the exact candidate commit, client revisions and clean working tree.
2. Complete the [preconditions](#preconditions) and [rc.4 acceptance checklist](rc.4-acceptance-checklist.md).
3. Obtain explicit maintainer approval for protected release/publication actions.
4. Follow [cutting it](#cutting-it), then verify installation and discovery from the tag.

Do not create, move or push a tag merely because documentation or acceptance preparation
is complete. Existing candidate tags stay unchanged unless the maintainer explicitly
instructs otherwise.

## App Review freeze

Effective 7 November 2026, the designated home and study App Review demo deployments
stay on `v0.1.0-rc.4` until the app team explicitly confirms review is over. No deploys,
upgrades (including final `v0.1.0`), resets, data-losing restarts or configuration changes.
Keep App Review accounts and invitations 1-3 untouched. Keep deployment addresses and
credentials in protected operator records, not release notes.

Release preparation, qualification and publication do not lift this freeze. Use a separate
qualification server and fresh synthetic fixtures for all testing and destructive cases.
The existing [rc.4 checklist](rc.4-acceptance-checklist.md) does not authorize changing a
frozen deployment. Record the app team's explicit end-of-review confirmation before
scheduling an upgrade; all normal upgrade gates still apply.

## Compatibility and consumer qualification

Starting with `v0.1.0` final, every new Tessera release goes to a separate test server
before any server used by released apps. This applies to patches as well as larger releases.

1. Identify the exact candidate commit/tag, binary or image digest, packaging/configuration
   revision, protocol versions and supported client revision pairs. Final `v0.1.0` uses fresh
   storage; do not reset a frozen review deployment to create it.
2. Prepare a release packet: release notes, specification diff and append-only vector diff
   from the last consumer-accepted release, plus exact comparison refs. For the first final,
   also compare with `v0.1.0-rc.4`; retain the alpha.28 vector audit required below.
   Explicitly flag behavior changes even when specification text is unchanged: stricter
   validation, consent, scopes, OAuth, error/status codes, capabilities, defaults and limits.
   State migration/reconfiguration needs and recovery limits. State explicitly when a diff
   or behavior-change category is empty.
3. Supply the app team with the isolated test endpoint, packet and 2-3 fresh test invitations
   through an authorized channel. Do not reuse review invitations or put credentials in
   release artifacts. Record delivery and the tested revision pair in protected evidence.
4. Allow the app team's approximately half-day qualification: independent vector tests,
   live integration tests and both home and study walks. Obtain explicit confirmation
   before upgrading any app-connected deployment. Time elapsed is not acceptance.
   A behavior-changing fix requires updated artifacts and affected acceptance reruns.
5. Give at least 14 days' notice before an app-connected server upgrade, identifying the
   target, version, behavior changes, planned time and recovery path. The corresponding
   client agreement requests the same notice before an app release changes required
   protocol features. Record notice and consumer confirmation separately; neither replaces
   the freeze, acceptance gates or deployment authorization.

Keep serving protocol `"1"` at `/v1/` for as long as released app versions use it. A future
major protocol is served alongside it; an implementation version such as `v0.1.0` is not a
protocol major. Within v1, preserve the existing additive compatibility rules: clients
ignore new response fields and missing capabilities remain safe. Implementation fixes
must be checked against the contract and disclosed rather than silently redefining v1.

The [versioning contract](../specs/tessera-protocol-v1.md#versioning-and-compatibility)
requires highest-common numeric major selection and at least 12 calendar months of
old/new overlap after the successor protocol major's first production release, extended
while released clients require the old major. Retirement also needs at least 14 days'
notice and discovery `deprecated_versions` with a planned UTC `sunset`. Keep dated
successor-release, notice, announcement and consumer-confirmation evidence. A date alone
never authorizes removal; delay the date if the support conditions still require service.
No v1 retirement is planned for `v0.1.0`; leave deprecation configuration unset.

## What a release asserts

The required suites pass, the reported version identifies the running code and the
operator has a verified recovery path. A release does not make the draft specifications
final or constitute a full privacy audit. The specifications remain the contract.

## Preconditions

For this first v0.1.0 only, the maintainer's 8 November 2026
[app-test waiver](v0.1.0-s08-acceptance-handoff.md#maintainer-app-test-waiver-8-november-2026)
permits proceeding without the remaining consumer iOS app/device tests and independent
app sign-off. Record those cases as waived/unrun, retain the interim evidence and do
not count them as passes. A subsequent explicit
[first-party Console-client waiver](v0.1.0-s08-acceptance-handoff.md#maintainer-first-party-console-waiver-8-november-2026)
also removes the remaining first-party W01-W05 client-test deferral for this release.
Both waivers preserve actual passes, skips, unsupported features and evidence limits;
neither asserts independent client sign-off. Server-owned tests and server/operator
qualifications are not waived. Other release, publication, deployment, notice and
freeze requirements remain. These exceptions do not alter the consumer qualification
rule for subsequent releases.

- [ ] Run the full gate on a clean committed candidate. Use `make ci` (or the documented
  shared-host equivalent), which runs `make release-check` with both `TESSERA_TEST_POSTGRES_DSN` and
  `TESSERA_TEST_POSTGRES_RESTORE_DSN` pointing at disposable databases. `make check`
  can skip PostgreSQL and is insufficient for this gate.
- [ ] Run `make integration`, `make integration-auth` and `make faults` on the candidate.
- [ ] Obtain the independent consumer run, including app/device and server backup/recovery
  acceptance. Record candidate and consumer revisions, commands, outcomes and coverage limits.
  Prior alpha passes do not qualify the current candidate.
- [ ] Confirm a clean tree and a matching [feature matrix](feature-matrix.md). Each required
  operation and advertised capability must have passing coverage.
- [ ] Resolve or explicitly accept remaining [contract findings](tessera-spec-review.md).
  Finding 47 is [resolved](tessera-spec-review.md#finding-47-selecting-and-retiring-protocol-versions);
  obtain independent consumer results for the new version vectors and incompatibility UI.
- [ ] Complete the release packet and isolated [consumer qualification](#compatibility-and-consumer-qualification).
  Record any pending app confirmation; do not infer it from backend gates.
- [ ] Review changed routes, grants, defaults, persistence formats and error semantics with
  consumers. State migration/reconfiguration needs and client-side decoder/mock effects.

The [operations guide](../.agents/skills/go-storage-service/references/operations.md#commands-and-ci)
owns local and shared-host CI commands. Keep full logs in protected run storage or attached
to the relevant commit/review. Do not add a second historical status document to the tree.

## Before the first public release

`main` carries releases; development happens on `dev`. The first release is prepared on
`release/v0.1.0` and reaches `main` through one approved, squashed pull request. Do not
fast-forward development history into the public release.

1. Scan the final tree and intended public refs for private client names, hostnames,
   addresses, key material and credentials. Record scope and results in the release review;
   an earlier scan is not a guarantee about the final commit.
2. Development commit and annotated tag messages still contain private names. Preserve
   a private copy of the history and the removed translations at a maintainer-approved
   destination before deleting refs.
3. Have every consumer move its pre-release pin to `v0.1.0` and confirm before deleting
   any pre-release tag. Private Go modules stop resolving a deleted tag.
4. Obtain explicit approval before merging, tagging `v0.1.0`, deleting `dev` or any alpha/rc
   tag, or making the repository public.

Removing historical documentation does not remove private material from Git history.

## Versions

The module and implementation use `vMAJOR.MINOR.PATCH`, starting with `v0.1.0`.
Record/content protocol versions are independent of the implementation version.
The binary derives its version from build information:

| Build | Reported version |
| --- | --- |
| `go install github.com/ekarpov/tessera/cmd/tesserad@v0.1.0` | `v0.1.0` |
| Clean checkout build | Pseudo-version containing time and commit |
| Dirty checkout build | The same, with `+dirty` |

A checkout build is not a tagged installation. Verify `tesserad -version` and discovery
from the installed tag after publication. `release.Released` distinguishes pseudo-versions.

## Pre-releases

Pre-releases let consumers pin a fixed version and carry no migration guarantee. Candidate
tag messages must state exact checked revisions, completed gates and unmet acceptance.
First-release candidates use `release/v0.1.0`; creating another candidate requires explicit
authorization. Never retarget an existing tag to a fix.

**Final `v0.1.0` starts from fresh storage.** Pre-release databases, identity registries and
content directories are unsupported inputs even if forward migrations happen to open them.
The migration obligation begins with the release after `v0.1.0`, for data it wrote. Registry
format versions and rejection of unsupported state remain necessary.

## Cutting it

After the preconditions and approval, squash the release pull request onto `main` and tag
that commit with an annotated `v0.1.0` tag. Record exact gates, consumer revision pairs,
backup limits and remaining qualifications. Compare vectors against `v0.1.0-alpha.28`;
report additions explicitly and never alter existing cases.

Verify module installation from the tag, reported version and discovery. Give the maintainer
the final commit/tag and acceptance result so consumer pins can move. Ref cleanup and
publication remain separate approval steps.

### First-release backup acceptance

Online filesystem acquisition requires local hard-link/flock semantics and at most 256
required content files. Independent PostgreSQL registry owners cannot capture coherent
archives together. Recovered producer identity does not authenticate arbitrary artifacts.
Real cloud and separate-host recovery remain unaccepted, and there has been no complete
privacy audit. Retain keys, trusted producer identity and current retirement evidence
independently. Repeat these qualifications in candidate/final tag messages and release notes.
The [acquisition contract](backup-acquisition.md#content-and-deployment-limits) owns detailed limits.

## Upgrading a deployment

First check the [App Review freeze](#app-review-freeze), complete
[consumer qualification and notice](#compatibility-and-consumer-qualification), and obtain
deployment authorization. Publication of a release does not authorize an upgrade.
Follow [self-hosting](self-hosting.md#upgrading). Migrations are forward-only; an old binary
must not open newer state. Take a backup before upgrading and verify isolated restore.
Rollback means restoring compatible state and its independent retirement evidence, not
starting an old binary against the upgraded database.
