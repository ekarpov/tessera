# Installation for agents

Prepare a reproducible Go development environment and synthetic storage. Do not install
an operator deployment as a side effect of building or testing.

## Quick setup

1. On Windows, use the [required WSL workflow](../skills/go-storage-service/references/operations.md#windows-shell).
2. Match Go to `mise.toml` (currently 1.27.0); enable CGO and provide a C compiler for SQLite.
3. Confirm the checkout and local changes before editing. Run `go mod download` if dependencies
   are missing; do not rewrite module pins to make installation pass.
4. Run `make dev`, or `make dev-content` for attachments. Use only synthetic data.

## Development and dependencies

[SQLite development](../../docs/sqlite-development.md) owns listener, token-file and reset
instructions. Stop the running server before an explicitly requested reset; preserve
unrelated state. [Consuming Tessera](../../docs/consuming-tessera.md#getting-the-module)
owns pinned module installation and private-repository authentication.

For PostgreSQL coverage, use two disposable databases and the
[commands and CI guide](../skills/go-storage-service/references/operations.md#commands-and-ci).
Never point conformance suites at real data: they write records and attachments and do not
clean them up. Credentials belong in private files or local settings and must not appear
in commands, tracked documents or test output.

## Choose verification

Use the storage skill's [verification guidance](../skills/go-storage-service/SKILL.md#verification).
A local `make check` may skip PostgreSQL; report that limit. Release checks require both
engines and the separate independent-consumer gate. For documentation changes, check links,
anchors, consistency and `go test ./internal/speccheck -count=1`.
