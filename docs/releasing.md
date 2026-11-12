# Releasing

A release identifies verified code, states its limits and gives operators a recovery path.
`v0.1.0` is released; its [release notes](v0.1.0-release-notes.md) record acceptance and limits.
The first-release [replacement handoff](v0.1.0-s08-acceptance-handoff.md) and earlier
[base handoff](v0.1.0-acceptance-handoff.md) retain historical case bodies and evidence scope.
For each subsequent release, prepare a new qualification packet for its exact revision;
prior results describe the revisions that were tested.

Development follows the [public PR workflow](../CONTRIBUTING.md#public-development-workflow).
A merged PR, release tag or published release never authorizes deployment by itself.

## Quick guide

1. Confirm the exact server candidate commit and clean working tree.
2. Complete the [preconditions](#preconditions) and the candidate-specific acceptance packet.
   The [rc.4 checklist](rc.4-acceptance-checklist.md) remains historical evidence for that candidate.
3. Obtain explicit maintainer approval for protected release/publication actions.
4. Follow [cutting it](#cutting-it): gate the exact final commit, then tag, publish and
   verify installation and discovery within the approved scope.

Do not create, move or push a tag merely because documentation or acceptance preparation
is complete. Published tags are immutable; fixes receive a new version.

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

<a id="compatibility-and-consumer-qualification"></a>

## Server qualification and app adoption

1. Identify the exact server candidate, binary or image digest, packaging and configuration
   revision, protocol versions and comparison release.
2. Prepare release notes, a specification diff and an append-only vector comparison against
   the previous release. Record changes to routes, validation, grants, defaults, error codes
   and persistence. State migration and recovery requirements.
3. Run the server, protocol, compatibility and recovery checks in the
   [preconditions](#preconditions) on disposable infrastructure. Record the exact server and
   wire-client revisions tested, along with results and coverage.
4. Make the verified version and its release packet available for consumer testing. Each app
   validates the release against its profile, decides when to pin it and reports compatibility
   issues or requests. App and device results inform adoption. Tessera release approval is
   based on the server and protocol checks.

For an app-connected deployment, qualify the proposed upgrade in isolation, verify recovery
and coordinate timing with affected operators and consumers. Give at least 14 days' notice
identifying the target version, behavior changes, planned time and recovery path. Obtain
deployment authorization and observe the App Review freeze where it applies.

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

- [ ] Run the full gate on a clean committed candidate. Use `make ci` (or the documented
  shared-host equivalent), which runs `make release-check` with both `TESSERA_TEST_POSTGRES_DSN` and
  `TESSERA_TEST_POSTGRES_RESTORE_DSN` pointing at disposable databases. `make check`
  can skip PostgreSQL and is insufficient for this gate.
- [ ] Run `make integration`, `make integration-auth` and `make faults` on the candidate.
- [ ] Run released wire-client checks against the candidate and verify backup and recovery.
  Record the comparison release, exact revisions, commands, outcomes and coverage.
- [ ] Confirm a clean tree and a matching [feature matrix](feature-matrix.md). Each required
  operation and advertised capability must have passing coverage.
- [ ] Resolve or explicitly accept remaining [contract findings](tessera-spec-review.md).
  Verify protocol selection, overlap and retirement behavior against the version vectors.
- [ ] Complete the server release packet. State migration and reconfiguration needs, protocol
  changes and recovery limits so consumers can assess the version for their own pins.

The [operations guide](../.agents/skills/go-storage-service/references/operations.md#commands-and-ci)
owns local and shared-host CI commands. Keep full logs in protected run storage or attached
to the relevant commit/review. Do not add a second historical status document to the tree.

## Public refs and release evidence

`main` carries the releasable public history. Changes reach it through reviewed topic PRs,
with squash merging as the default. Do not restore the former private development branches,
merge archived private ancestry or push private bundles, backup refs or operator notes.

1. Review the final tree, commit/tag messages and intended public refs for private client
   names, deployment addresses, key material and credentials. Record scan scope, reviewed
   exceptions and results in the release review. An earlier scan does not qualify new changes.
2. Keep full operational logs, client revision/configuration details that are private and
   unsanitized evidence in protected storage. Publish only reviewed summaries and artifacts;
   public CI output and PR attachments need the same review as committed files.
3. Before deleting historical refs, preserve private history and any removed material at a
   maintainer-approved private destination. Obtain consumer pin migration confirmation before
   removing a version consumers may still resolve. Cleanup does not authorize changing a
   running deployment or deleting its evidence.
4. Obtain maintainer authorization for merging, tagging, publication, historical-ref deletion
   or repository visibility changes. Ordinary topic-branch deletion after merge follows the
   contribution workflow. Existing authorization remains valid within its stated scope.

Deleting a ref does not guarantee removal of its objects or cached views from a hosting
service. Assess retained history before a visibility change. Public tag immutability and
private-history custody remain separate from local worktree cleanup.

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
Prepare candidates through topic PRs from `main`; creating or publishing a candidate tag
requires maintainer authorization. Never retarget an existing tag to a fix.

**Final `v0.1.0` starts from fresh storage.** Pre-release databases, identity registries and
content directories are unsupported inputs even if forward migrations happen to open them.
The migration obligation begins with the release after `v0.1.0`, for data it wrote. Registry
format versions and rejection of unsupported state remain necessary.

## Cutting it

1. Complete candidate qualification and review the release PR under the public contribution
   workflow. Confirm maintainer authorization covers the intended merge, tag and publication.
   Squash merge by default; the resulting commit on `main` is the release candidate.
2. Check out that exact commit with a clean tree and run the full committed-candidate gate
   and integration/authentication/fault checks in [preconditions](#preconditions). Record its
   SHA, build information, binary digest and outcomes. Pre-merge checks on another SHA do not
   replace this gate. If a fix changes the candidate, review it and qualify the new commit
   before tagging; do not move a tag to the fix.
3. Complete any server qualification still owed for that exact candidate and record tested
   wire-client revision pairs and operator recovery limits. Compare the specification and
   vectors against the previous release; report additions explicitly and never
   alter retained vector cases. The first v0.1.0 also retained its alpha.28 comparison audit;
   evidence does not depend on restoring deleted pre-release tags to the public repository.
4. Within the authorized scope, create an annotated version tag on the verified SHA and
   publish release notes with exact gate results, acceptance limits, migration requirements
   and remaining qualifications. Review all public text and attachments before uploading.
   Do not rewrite a published release tag; later documentation corrections may advance `main`
   without changing which commit was released.
5. Verify module installation from the published tag, `tesserad -version` and discovery using
   disposable synthetic storage. A checkout pseudo-version does not prove tagged installation.
   Record the outcome and give consumers the final commit/tag so pins can move. If verification
   fails, disclose it and resolve it without silently retargeting the tag.

Release publication does not authorize deployment, ref cleanup or a visibility change.
Check the freeze, deployment qualification, notice and authorization separately.

### First-release backup acceptance

Online filesystem acquisition requires local hard-link/flock semantics and at most 256
required content files. Independent PostgreSQL registry owners cannot capture coherent
archives together. Recovered producer identity does not authenticate arbitrary artifacts.
Real cloud and separate-host recovery remain unaccepted, and there has been no complete
privacy audit. Retain keys, trusted producer identity and current retirement evidence
independently. Repeat these qualifications in candidate/final tag messages and release notes.
The [acquisition contract](backup-acquisition.md#content-and-deployment-limits) owns detailed limits.

## Upgrading a deployment

Check the [App Review freeze](#app-review-freeze), complete
[deployment qualification and notice](#server-qualification-and-app-adoption), and obtain
deployment authorization. Publication of a release does not authorize an upgrade.
Follow [self-hosting](self-hosting.md#upgrading). Migrations are forward-only; an old binary
must not open newer state. Take a backup before upgrading and verify isolated restore.
Rollback means restoring compatible state and its independent retirement evidence, not
starting an old binary against the upgraded database.
