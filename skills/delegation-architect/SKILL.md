---
name: delegation-architect
description: Design a bounded ownership or interface change using existing subsystem boundaries and verified behavior.
---

# Delegation architect

Use when ownership, provider preparation, catalog or task interfaces must change.
For an established local bug boundary, use the narrower implementer skill.

## Inputs and relevant authorities

Read the [product boundary](../../agents/_data/product-boundary.md) and the
[ownership guidance](../../agents/_data/maintainability-contract.md#boundary-and-ownership).
For provider changes, consult the [adapter boundary](../../agents/_data/adapter-contract.md#2-launch-planning-and-preflight-policy);
for lifecycle changes, the [delegation invariants](../../agents/_data/delegation-invariants.md).
Use current [architecture](../../docs/architecture.md) for implementation facts.

Start from the assigned scope/base, real consumers, state owners, compatibility
requirements and observable success/failure cases. A dated plan is a pointer;
verify it against source and the user's latest direction. Apply the
[installed-executable rule](../../agents/_data/product-boundary.md#installed-executables)
to runtime, recovery and contributor designs; prior implementation is not an
exception to that rule.

## Work

1. Trace callers, records and resource lifetimes using available graph tools
   and exact source. Name one owner, writers, readers and invariant per changed
   boundary. Keep interfaces consumer-owned and dependencies inward.
2. Compare reuse and the smallest correction before adding an abstraction.
   Disclose any new guarantee, dependency, state or compatibility cost. Resolve
   expanded product scope with Root before dependent work.
3. Keep provider behavior local and reuse shared launch, capture, lifecycle and
   publication owners. Trace indirect public consumers before changing a gate;
   preserve allowed observations, refused mutations and public error guidance.
4. Verify required OS/provider primitives before dependent implementation.
   Missing evidence blocks that work, not independent preparation.
5. Define the narrow interface and meaningful behavior/fault tests in the
   existing plan. Apply/verify prerequisites before dependent edits. No separate
   matrix, source inventory, ledger or handoff format is required.

## Result

Return the selected design, ownership/dependency decisions, rejected alternatives
that affect correctness, tests and unresolved facts. The boundary is ready when
its consumers can implement without guessing and [architecture guidance](../../agents/_data/maintainability-contract.md#architecture-guidance)
is satisfied. Design agreement alone is not runtime or final-code approval.

Report affected guidance and diagram changes under the project
[documentation completion rule](../../AGENTS.md#documentation-completion).
