# Architecture for agents

Use the [architecture and package map](../../docs/architecture.md) to find the owning
boundary before editing. Avoid solving an integration problem inside the protocol core.

## Quick investigation

1. Read the affected specification section and search the [decision index](../../docs/asked-and-answered.md).
2. Trace the handler, store interface, shared evaluator and both durable-engine wrappers.
3. Locate callers, conformance coverage and security sweeps before choosing a change.
4. For identity or administrative work, also inspect standalone composition and the
   [deployment contract](../../specs/tessera-deployment.md).

## Boundaries to preserve

[AGENTS.md](../../AGENTS.md#invariants) owns repository invariants; the
[storage skill](../skills/go-storage-service/SKILL.md) owns Go, SQL, paging and lifecycle rules.
The [embedding contract](../../specs/tessera-embedding.md) owns trusted host responsibilities.
Keep those rules in their authoritative locations rather than copying them into new guides.

Distinguish record access, dataset administration and server administration. Test the
composition where a grant or error changes meaning across layers. A correct unit test for
each layer does not prove that their composition preserves authority.
