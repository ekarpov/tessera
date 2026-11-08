# Tessera

Tessera is a protocol for typed, schema-described records and optional content storage.
This repository contains the specifications, portable test vectors, an importable Go
conformance suite and a Go reference server with SQLite and PostgreSQL engines.
Applications define their own vocabulary and workflows; Tessera stores their data without
interpreting its meaning.

**Status:** [v0.1.0](https://github.com/ekarpov/tessera/releases/tag/v0.1.0) is released.
See the [release notes](docs/v0.1.0-release-notes.md) for qualification results, operator
limits and the explicitly waived client tests. Independent client sign-off remains pending;
a passing backend suite is not a conformance certificate. Start with fresh storage:
pre-release storage is unsupported.

## Quick start

For a local experiment, install Go 1.27.0 and a C compiler, enable CGO, then run:

```sh
make dev
```

The synthetic development server listens at `http://127.0.0.1:8080` and keeps its
SQLite database and private bearer-token file under `.tessera-dev/`. The token is
written to `.tessera-dev/token`, never printed. Use synthetic data only.
Run `make dev-content` to include filesystem attachments. See the
[development guide](docs/sqlite-development.md) for connecting and resetting.

For other uses:

| I want to… | Start here |
| --- | --- |
| Understand the data model and HTTP API | [Protocol overview](specs/tessera-overview.md) |
| Run a private HTTPS server | [Self-hosting](docs/self-hosting.md), then [deployment examples](deploy/README.md) |
| Use or embed the Go packages | [Consuming Tessera](docs/consuming-tessera.md) |
| Check supported features and limitations | [Feature matrix](docs/feature-matrix.md) |
| Work on the project | [Contributing](CONTRIBUTING.md) |
| Find a reference or decision | [Documentation index](docs/README.md) |

## How it works

A server holds datasets with their own identities, schemas and access grants. Clients
read records through bounded queries and submit ordered, atomic transactions. Durable
engines provide idempotent write recovery, pagination, retirement and encrypted backup
export. Optional content storage keeps attachment bytes outside the record API.

The standalone server supplies OAuth/PKCE, accounts, invitations and administration.
SQLite serves many datasets in one process; PostgreSQL supports multiple processes,
subject to the [identity and backup deployment constraints](docs/self-hosting.md#postgresql).
Live-storage encryption is supplied by the operator's filesystem or volume.

The [record protocol](specs/tessera-protocol-v1.md) and
[content protocol](specs/tessera-content-v1.md) define correctness and version independently.
The reference server does not define the contract. Implementers can use the
[conformance suites](docs/feature-matrix.md#running-the-conformance-suite) against their own
HTTP server and share the [frozen, append-only vectors](specs/vectors/README.md).

## Development

```sh
make check
```

This runs formatting, vet and race-enabled tests. PostgreSQL tests can skip when
no disposable databases are configured; use the
[verification instructions](.agents/skills/go-storage-service/references/operations.md#commands-and-ci)
before claiming coverage of both engines. Windows contributors use the
[WSL workflow](.agents/skills/go-storage-service/references/operations.md#windows-shell).
Agent-specific setup and change workflows start in [AGENTS.md](AGENTS.md).

## Licence

[Apache-2.0](LICENSE), including its patent grant.
