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

## Inputs

- the bounded implementation handoff and fixed interface contract;
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
   unsupported-option failures.
4. Add or extend meaningful tests for observable behavior, error branches,
   writer failures, malformed input, and compatibility. Avoid tests that only
   mirror implementation branches.
5. Run `make fmt-check`, the affected focused tests, and `make verify-skills`;
   finish with the applicable project gate before handing off. Do not suppress
   a linter or weaken an assertion to obtain a pass.

## Deliverables

- the bounded Go implementation and any necessary fixtures;
- tests that prove the changed behavior and important failure paths; and
- a handoff naming the exact files, commands, results, and known limits.

## Exit criteria

- The implementation compiles and the affected tests pass with race detection
  where the project gate requires it.
- No forbidden process primitive, global provider configuration mutation, paid
  retry, or provider-specific core exception was introduced.
- Error context, identity/session matching, sealing/publication authority, and
  backward compatibility are covered by evidence.
- The worktree contains no planning-document, commit, PR, or merge mutation
  outside the assignment.
