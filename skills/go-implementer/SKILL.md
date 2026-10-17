---
name: go-implementer
description: Implement a bounded Go change while preserving delegation contracts, compatibility, and the repository quality gate.
---

# Go implementer

## Trigger

Use this skill when implementing an assigned Go runtime, catalog, adapter, or
shared preparation change. The assignment must name an owned file boundary and
an observable behavior. Use [go-test-writer](../go-test-writer/SKILL.md) when
the primary work is test design, and [go-concurrency](../go-concurrency/SKILL.md)
when lifetimes or races are the primary risk.

Read the [maintainability contract](../../agents/_data/maintainability-contract.md),
[delegation invariants](../../agents/_data/delegation-invariants.md),
[adapter contract](../../agents/_data/adapter-contract.md), and
[code-quality floor](../../agents/_data/code-quality-floor.md).

For supervisor changes, apply the recovery and verification section of the
[shared contract](../../agents/_data/maintainability-contract.md#recovery-and-verification).
For process, supervisor, endpoint, or cleanup changes, also apply its
[native portability proof](../../agents/_data/maintainability-contract.md#native-portability-proof).

## Inputs

- the bounded implementation handoff, affected canonical transition row IDs,
  and fixed interface contract;
- exact owned files, affected consumers, and compatibility constraints;
- existing error and persistence behavior;
- deterministic fixtures or mocks for the relevant success and failure paths.

## Workflow

1. Read the named contracts and inspect the relevant consumers before editing.
   Preserve unrelated worktree changes and keep the change inside its assigned
   boundary.
2. Implement the smallest consumer-owned interface that carries the required
   data. Keep provider argument/configuration/identity interpretation local to
   the provider and shared process/evidence/publication mechanics in the core.
3. Preserve context propagation within each execution lifetime, wrapped error
   identity, durable record semantics, nullable usage values, and explicit
   unsupported-option failures. For every create-once record, apply the
   invariants' [ambiguous-write
   classifications](../../agents/_data/delegation-invariants.md#ambiguous-create-once-writes).
4. Update the [canonical transition
   rows](../../agents/_data/maintainability-contract.md#durable-transition-matrix-and-composed-tests)
   and add their named composed tests through each affected production store and
   real consumer. Use fakes only for isolated protocol ordering; avoid tests
   that merely mirror implementation branches.
   Keep shared lifecycle policy OS-neutral and isolate only primitives whose
   semantics differ; do not emulate one supported kernel to claim another.
5. Run `make fmt-check`, the affected focused tests, and `make verify-skills`;
   require `make verify-maintainability-strict` and the applicable project gate
   before peer review. A staged checkpoint names any blocked shared gate and
   remains incomplete. Do not suppress a linter or weaken an assertion to
   obtain a pass.

## Deliverables

- the bounded Go implementation and any necessary fixtures;
- tests that prove the changed behavior and important failure paths; and
- the contract's [evidence
  handoff](../../agents/_data/maintainability-contract.md#handoff-evidence),
  naming the exact snapshot, binaries, commands, receipts, and known limits.

## Exit criteria

- The implementation compiles and the affected tests pass with race detection
  where the project gate requires it.
- Every affected supported OS has native evidence for its changed runtime path;
  cross-build success is recorded only as a build result.
- The [canonical forbidden-signal
  policy](../../agents/_data/delegation-invariants.md#forbidden-signal-policy),
  global provider configuration boundary, paid-work at-most-once rule, and core
  ownership boundary remain intact.
- Error context, identity/session matching, sealing/publication authority, and
  backward compatibility are covered by evidence.
- The worktree contains no planning-document, commit, PR, or merge mutation
  outside the assignment.
