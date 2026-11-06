---
name: go-implementer
description: Implement a bounded Go behavior change with meaningful tests and preserved subsystem ownership.
---

# Go implementer

Use for an assigned runtime, catalog, adapter or preparation change. Use the
test-writer skill for test design or concurrency skill for lifetime/race work.

## Inputs

An exact base, owned files, stable interface prerequisites, behavior/non-goals,
real consumers and done condition. Confirm the tools this assignment needs in
your launch context. A setup failure is blocked, not a passing check or reason
to repair global configuration.

Read [architecture guidance](../../agents/_data/maintainability-contract.md#architecture-guidance)
and the [quality floor](../../agents/_data/code-quality-floor.md). Consult
[adapter rules](../../agents/_data/adapter-contract.md) only for provider work,
and [lifecycle tests](../../agents/_data/maintainability-contract.md#lifecycle-tests)
for changed durable or process behavior.

## Work

1. Inspect the affected callers and tests. Preserve others' edits. Confirm an
   upstream post-image is applied and checked before dependent implementation.
2. Name the state owner/invariant; keep data, decisions and I/O separate. Use
   existing values and consumer-owned ports. Do not extract a helper that just
   passes an entire local state through a long positional argument list.
3. Preserve contexts, wrapped errors, persisted behavior, permissions and
   session identity. Use canonical transactions, resolvers and finalizers.
4. Add meaningful success/failure regressions. Prove a bug test fails for its
   intended assertion; unrelated compile/setup failures are not expected red.
5. Run formatting and affected tests, with race detection for concurrent paths.
   Run the applicable lint and type checks. Do not invoke
   unchanged full aggregates after their focused subsets merely for another pass.

## Result

Deliver actual changes when assigned implementation; a patch proposal is not
implementation completion. Report changed files, commands/results and blockers
concisely to Root. Root owns shared integration, Git and final checks/review.
Apply the [installed-executable rule](../../agents/_data/product-boundary.md#installed-executables)
to production code, fixtures and contributor tools. No handoff index or extra
receipt is required.
Changed OS primitives need [native validation](../../agents/_data/maintainability-contract.md#native-platform-checks)
before claiming supported-host behavior.
