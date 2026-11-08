# Contributing

Contributions may improve the Tessera contracts, conformance suites or reference server.
The specification defines behavior; a change to the implementation cannot silently redefine it.

## Quick guide

1. Read [repository guidance](AGENTS.md) and the relevant specification section.
2. Check [asked and answered](docs/asked-and-answered.md) before proposing behavior.
3. Make the change with the required negative cases and documentation.
4. Run focused checks, then the appropriate [verification gate](.agents/skills/go-storage-service/SKILL.md#verification).

Use Go 1.27.0, CGO and a C compiler for SQLite. Run `make dev` for synthetic local work
and `make check` for formatting, vet and race tests. PostgreSQL may skip without disposable
databases; [commands and CI](.agents/skills/go-storage-service/references/operations.md#commands-and-ci)
explains both-engine verification. Windows contributors use the [WSL workflow](.agents/skills/go-storage-service/references/operations.md#windows-shell).
Agent-specific workflows are in the [agent guides](.agents/guides/README.md).

## Public development workflow

`main` is the shared, releasable baseline. Start a short-lived topic branch from current
`main`; agent-created branches use `codex/<topic>`. There is no long-lived `dev` or release
integration branch. Never merge archived private development history into a public branch.

Keep each commit focused on one coherent change and explain its purpose. No special commit
prefix is required. Review staged content and commit messages before pushing: credentials,
private client details, deployment addresses, record payloads and raw operator logs belong
in protected storage. Use synthetic examples and publish only reviewed, sanitized evidence.
The same rule applies to PRs, issues, reviews and attachments.

Open a PR against `main`. Use a draft early for substantial unfinished work; small completed
changes can open ready for review. The description should explain the problem, resulting
behavior, checks actually run, skipped coverage and material limitations. Keep specification,
implementation and conformance changes together when the protocol changes.

Before merge, bring the branch up to date when needed, run the checks appropriate to the
final changes, and resolve review conversations. Skips are not passes; missing CI is not
passing CI. A maintainer authorizes the merge. Squash merge by default and use a final title
and description that explain the resulting change without conversational history. Preserve
separate commits when their sequence helps review and the maintainer agrees. Delete the
topic branch after merge; retain useful private evidence separately. Do not rewrite shared
`main` history or move published tags.

Protect `main` with PR review, required configured CI checks and resolved conversations;
block force pushes and branch deletion. Maintainers configure these repository settings
separately; this document does not assert that a particular GitHub rule or check is enabled.
Agent opening of a PR does not itself authorize merging or changing protection settings.

A green PR qualifies its tested revision, not a new squash commit. Releases must pass the
[clean committed-candidate gate](docs/releasing.md#cutting-it) on the exact commit to be
tagged. Tagging, publication and deployment require the applicable maintainer authorization;
merging a PR does not authorize them or lift a deployment freeze.

## The specification is the contract

The normative specifications in `specs/` are author-owned. Record ambiguities as findings
in [the contract review](docs/tessera-spec-review.md), including the provisional behavior
and consequences of each choice. Link new findings from the decision index. Obtain an
author decision rather than settling the question in code.

A protocol change needs a conformance case. `specs/vectors/*.json` are frozen and
append-only: add cases, never edit or remove an existing case. Application vocabularies
belong to their own repositories; Tessera never loads one.

State guarantees, refusals and recovery behavior explicitly at the boundary where a
consumer needs them. Distinguish absence, refusal and concealment, and document unusual
but valid inputs. Do not leave a consumer to infer a missing guarantee and build a
permanent workaround. Mechanically check examples and tables where practical.

## What a change is expected to carry

- A negative case for each feature or bound, including a refusal past the declared limit.
- A test that fails without the change. For subtle behavior, seed a compiling defect and
  confirm the named case catches it; a compilation failure proves no behavior.
- Both engines where semantics are shared. Prefer the shared evaluator over two fixes.
- Hand-reviewed forward migrations and tests starting from the previous schema.
- Updated current API/configuration guidance and feature claims where behavior changes.

Test across component boundaries, repeat stateful cases when retained state matters, and
check that the relevant branch actually executed. A skipped test is not a pass. Run
platform-sensitive cases on the target platform; virtualized filesystem ownership can
hide deployment failures.

`internal/security` carries composed HTTP isolation and availability regressions. Add new
routes to existing sweeps. Keep regression tests when removing historical reports:
the tests preserve the failure conditions. Browser automation does not establish native
callback delivery, real password-manager behavior or deployment acceptance.

For documentation-only changes, check links/anchors, consistency and preservation of
requirements; run `go test ./internal/speccheck -count=1` and `git diff --check`.

## Style

- Prefer few dependencies and explicit interfaces at external boundaries.
- Comments explain why a rule exists and what it prevents.
- Keep `cmd/tesserad` to configuration, composition, lifecycle and exit status.
- Never log record values, user-supplied identifiers, SQL text or credentials, including
  in test failures.
- Pass `context.Context` through blocking operations; own, cancel and join background work.

## Reporting a security issue

Open an issue saying only that you have one, and wait to be contacted. Do not include
details, a reproduction or affected data in a public issue.

## Licence

Contributions are made under [Apache-2.0](LICENSE), including its patent grant.
