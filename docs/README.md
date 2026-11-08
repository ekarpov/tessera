# Documentation

Start with the [project README](../README.md) for a local server, or the
[protocol overview](../specs/tessera-overview.md) for the model and API.
This index separates practical guides from contracts and design decisions.

## Quick guide

| Task | Guide |
| --- | --- |
| Try a synthetic local dataset | [SQLite development](sqlite-development.md) |
| Install and operate an HTTPS server | [Self-hosting](self-hosting.md) and [deployment examples](../deploy/README.md) |
| Connect over HTTP or embed Go packages | [Consuming Tessera](consuming-tessera.md) |
| Check support and run conformance suites | [Supported features](feature-matrix.md) |
| Understand packages and request flow | [Architecture](architecture.md) |
| Contribute a change | [Contributing](../CONTRIBUTING.md) |
| Work as an agent | [Agent guides](../.agents/guides/README.md) |
| Prepare a release | [Release procedure](releasing.md) and [rc.4 acceptance](rc.4-acceptance-checklist.md) |

## Contracts and configuration

The normative contracts are the [record protocol](../specs/tessera-protocol-v1.md),
[content protocol](../specs/tessera-content-v1.md),
[deployment contract](../specs/tessera-deployment.md) and
[embedding contract](../specs/tessera-embedding.md).
[Portable vectors](../specs/vectors/README.md) are frozen and append-only.

[Self-hosting](self-hosting.md) owns reference-server configuration, identity and operations.
The detailed administrative backup APIs are [acquisition](backup-acquisition.md) and
[scheduler settings](backup-scheduler.md); their limits apply to every deployment option.
[Backend options](backend-options.md) compares encryption and content-storage choices.

## Design and future work

Read [asked and answered](asked-and-answered.md) before proposing a change. It indexes
[contract review findings](tessera-spec-review.md), [extension decisions](extension-candidates.md)
and [design rationale](../specs/tessera-decisions.md). These records explain decisions;
the specifications and feature matrix describe the current contract and implementation.

[Backlog](backlog.md) lists remaining work. [Repository layout](repository-layout.md)
explains package boundaries between repositories. [Client profiles](client-profiles.md),
[storage and exchange](storage-and-exchange.md) and
[database-like service](database-like-service.md) explain integration and scope boundaries.
The [remote content authority annex](../specs/annex-remote-content-authority.md) is experimental,
non-normative and unimplemented.

## Keeping these documents useful

Keep procedures and current limits at the linked authoritative location. Update them with
behavior changes. Completed run logs and per-candidate evidence belong with the relevant
commit or review, rather than in a second status document. The pending acceptance checklist
remains until the teams complete it. Historical change lists and completed verification reports
are available in Git history; they are not current operating instructions.
