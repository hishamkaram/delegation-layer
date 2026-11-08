---
name: go-test-writer
description: Add behavior and fault tests with independent oracles and explicit process and storage ownership.
---

# Go test writer

Use when tests or fault scenarios are the primary assignment. Read the
[lifecycle-test guidance](../../agents/_data/maintainability-contract.md#lifecycle-tests)
for durable/process work and [quality floor](../../agents/_data/code-quality-floor.md#3-testing-and-verification-discipline)
for project tooling. Load only the relevant provider or native section.

## Inputs

Requested behavior, owned test files, real callers, current failure, canonical
state owner and success/failure cases. Confirm only the tools the task needs.

## Work

1. Choose an observable invariant, not a private signature, method body, test ID
   inventory or mirror of implementation branches. Preserve genuine persisted
   format and public-consumer compatibility.
2. For a bug, obtain the intended failing assertion before the fix. Record
   missing tools or unrelated failures honestly; do not weaken the test.
3. Use pure fakes for isolated decisions/order. Durable writes, rollback,
   create-once ambiguity and recovery need production stores and real consumers.
   See the [four create-once results](../../agents/_data/delegation-invariants.md#ambiguous-create-once-writes).
4. Gate concurrency at the actual owner boundary. Separate watchdogs from
   latency requirements. Register cleanup immediately; retain and join owned
   subprocesses/goroutines on error and assertion failure.
5. Use native temporary paths and canonical identity controls. Keep relevant
   Git/non-Git and permission-mode cases. Do not erase a provider precondition
   by initializing every fixture as a repository.
6. Run focused tests after dependencies stabilize. Keep each case in ordinary
   discovery once, without fixed historical counts/maps. Root owns the stable
   integrated checks; do not add another full run of unchanged subsets.

## Result

Return meaningful tests, exact commands/results and any unavailable scenario.
Do not introduce a test framework or contributor proof format. A mocked test
does not prove a real provider/account or [native host](../../agents/_data/maintainability-contract.md#native-platform-checks).
Tests and fixtures follow the same engineering guidance and quality checks.

Report affected guidance and diagram changes under the project
[documentation completion rule](../../AGENTS.md#documentation-completion).
