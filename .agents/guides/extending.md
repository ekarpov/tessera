# Extending Tessera

Start from a concrete consumer need and the existing contract. Add behavior at the
smallest owning boundary, without introducing application vocabulary or speculative hooks.

## Quick workflow

1. Check [asked and answered](../../docs/asked-and-answered.md), then the relevant specification.
2. If the contract is unclear, record a finding in the [contract review](../../docs/tessera-spec-review.md)
   and link it from the index. Obtain the author's decision before making code the rule.
3. For a protocol change, update normative text and add portable conformance cases together.
   Existing frozen vectors are append-only: never alter or remove a case.
4. Implement through the shared execution path, with both SQL engines covered where behavior is shared.
5. Update the [feature matrix](../../docs/feature-matrix.md) and the owning configuration/API guide.
6. Run focused checks, then the required gate; report exact coverage and any skipped engine.

## Place the change

| Change | Owning boundary |
| --- | --- |
| Wire values, grammar or errors | `pkg/protocol`, normative specs and conformance |
| Query or transaction semantics | `internal/recordstate`, `internal/sqlengine`, engine suites |
| Durable schema | Hand-reviewed `db/migrations`, previous-schema and recovery tests |
| HTTP routing or authentication composition | `pkg/server` and composed HTTP tests |
| Accounts, OAuth, consent or revocation | `internal/identity`, deployment contract and security sweeps |
| Operator configuration or UI | Standalone/admin composition, self-hosting guide and browser checks |
| Host-specific workflows | Host repository; use the [embedding contract](../../specs/tessera-embedding.md) |

## Prove the behavior

Follow [contribution requirements](../../CONTRIBUTING.md#what-a-change-is-expected-to-carry)
and [verification](../skills/go-storage-service/SKILL.md#verification).
For subtle regressions, seed a compiling defect and confirm the named case fails. Check
that the case executes, crosses the relevant boundary and survives a second run when state
is retained. Include new routes in existing isolation and availability sweeps.

Run platform-dependent cases on the target platform. Virtualized mount ownership does not
prove Linux credential-file behavior; browser automation does not prove native callback
delivery or a real password manager. Keep these acceptance limits explicit.

No new tag, publication, ref deletion or deployment is implied by an extension task.
