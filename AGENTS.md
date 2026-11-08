# Tessera Repository Guidance

## Scope

This repository owns the Tessera record and content protocols and their Go reference implementation.
Deliverables, in order: specification, importable conformance suite, development server, self-hosted
reference server, admin UI. No client defines the contract: all clients and third implementers have equal standing. Client integration guides, deployment and vocabulary belong with the client.

Roles, annotations, media bytes and `json` are opaque: store and return them, never interpret them.
Coach workflow, review, proposals, purpose-scoped consent and cross-service cleanup belong to the
separate record exchange; consult [storage and exchange](docs/storage-and-exchange.md) before
adding anything shaped like them. Protocol design rationale remains in the originating client's remote-storage
feasibility study, not restated in the specification.

## Find guidance by action

Load guidance for the current action; do not preload every skill or reference. Search headings
first (`rg -n '^#{1,4} ' path`), then read the relevant section and enough context to apply it.

| Action | Guidance |
| --- | --- |
| Change protocol | Normative [record](specs/tessera-protocol-v1.md) / [content](specs/tessera-content-v1.md) contracts (version independently); first check [contribution rules](CONTRIBUTING.md#the-specification-is-the-contract) and [decisions](docs/asked-and-answered.md) |
| Agent setup / architecture / extension / deployment | [Agent guides](.agents/guides/README.md) |
| Build/review Go server | [Storage skill](.agents/skills/go-storage-service/SKILL.md) |
| Run repository shell tools on Windows | [WSL workflow](.agents/skills/go-storage-service/references/operations.md#windows-shell) |
| Choose checks / run dev or CI | [Verification](.agents/skills/go-storage-service/SKILL.md#verification); [commands](.agents/skills/go-storage-service/references/operations.md#commands-and-ci) |
| Navigate / choose work | [Package map](docs/architecture.md#package-map), [backlog](docs/backlog.md) |
| Change storage/encryption or engine limits | [Backend options](docs/backend-options.md), [database-like service](docs/database-like-service.md) |
| Embed or support adopters | [Consuming Tessera](docs/consuming-tessera.md), [embedding contract](specs/tessera-embedding.md), [client profiles](docs/client-profiles.md) |
| Assess extensions | [Extension candidates](docs/extension-candidates.md) and the decision index above |
| Release/publish or change a shared deployment | [Release procedure, App Review freeze and consumer gate](docs/releasing.md) |

## Invariants

- **The specification is the contract; the conformance suite is the proof.** This server's behaviour
  never defines correctness. A protocol change without a conformance case is a suggestion.
- **A capability is a promise about the protocol, not a report on the database.** Compute is required
  of every conformant server; only guarantees an implementation cannot manufacture are advertised. A
  server too slow for a query declares a bound and fails at it rather than dropping the operation,
  because the client refuses rather than compensating.
- **Trust no client.** Every bound is validated server-side regardless of what arrived.
- **Three answers, not two.** `404 not_found` is absence, which the application reports as a deleted
  object; `403 permission_denied` is a refused operation; `404 unavailable` is a record outside the
  caller's authorized view, revealing nothing about existence and never meaning deletion. Concealment
  uses `unavailable`, never `not_found`.
- **Identity belongs to the dataset**, is minted with real entropy, is never reused, and is minted
  afresh when a storage is erased.
- **A lost response is never a reason to guess.** Idempotency binds a payload digest to the identity it
  created, and an uncertain caller asks rather than retries.
- **Security is part of the first data-bearing slice.** Authentication, authorization, isolation,
  encryption, secret handling, safe telemetry, retention, erasure and backup are implemented with the
  feature that needs them. Synthetic data only until the applicable controls exist and are tested.
- **Nothing a record contains reaches a log**, including field values, user-supplied identifiers, SQL
  text and credentials.

## Layout

Keep the Go module at the repository root, with the server and `pkg/conformance` here. Reference
clients (`tessera-swift`, `tessera-kotlin`) have their own repositories. Read
[repository layout](docs/repository-layout.md) before proposing a split. Create directories only for
real files; avoid empty packages, speculative abstractions and placeholders.

## Agent execution policy

The active agent handles planning, implementation, verification and review directly; no sub-agent
is required. Stay within the user's scope, permissions and release authorization. When delegation
is authorized and useful, give the worker a concise objective, scope, invariants, relevant paths and
expected return format. Reuse workers for follow-up fixes; prefer completion notifications or bounded
waits to frequent polling. Preserve required independent review and consumer gates. Before any shared deployment action,
check the [active freeze and upgrade rules](docs/releasing.md#app-review-freeze). Preparing a release
does not authorize deployment or modification of review accounts/invitations.

## Working context

- Reuse unchanged guidance already in context. Reread when it changes, necessary detail is lost in
  compaction, or uncertainty requires verification. Keep policies authoritative in one place; link
  instead of repeating them.
- Use `rg` / `rg --files`; start with affected modules, interfaces, callers and tests. Broaden when
  dependencies or findings justify it. Use the package map above rather than dumping directories.
- Batch independent searches/reads when practical. Bound output and summarize long results, retaining
  actionable diagnostics, failure details and evidence paths. Repeat commands only for new evidence.
- Report decisions, findings, changes, checks, limitations and blockers concisely, without repeated
  plans or unchanged status. Handoffs and compaction retain objective, constraints, current state,
  relevant paths, evidence and next actions.
