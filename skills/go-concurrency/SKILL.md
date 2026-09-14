---
name: go-concurrency
description: Design or review Go concurrency, deadline, locking, and process-lifetime changes under the delegation invariants.
---

# Go concurrency

## Trigger

Use this skill when work changes goroutines, channels, locks, context roots,
deadlines, subprocess waiting, cancellation observation, or recovery races.
For ordinary provider parsing without lifetime changes, use the narrower
implementer or test-writer skill.

Read the [maintainability contract](../../agents/_data/maintainability-contract.md),
[delegation invariants](../../agents/_data/delegation-invariants.md), and
[code-quality floor](../../agents/_data/code-quality-floor.md).

## Inputs

- the state machine and terminal outcomes affected by the change;
- owner and lifetime of every goroutine, file descriptor, lock, process, and
  context;
- deadline, stop-request, acknowledgement, termination, and recovery behavior;
- race, crash, writer-failure, and unknown-state scenarios to prove.

## Workflow

1. Draw the lifetime graph before editing: admission, dispatch, provider start,
   stdout/stderr capture, Wait, deadline observation, sealing, collection, and
   publication. Assign one owner to each resource and state who closes it.
2. Keep the runner's execution lifetime independent from a caller/watcher's
   observation lifetime. Record stop requested, acknowledged, and termination
   observed as distinct facts; do not infer one from another.
3. Use the repository's durable lock and create-if-absent rules. Preserve
   at-most-once submission and provider start, EEXIST winner comparison,
   immutable predecessors, and the single authoritative outcome.
4. Reject direct process signals, process-group manipulation,
   `exec.CommandContext`, nonzero `WaitDelay`, unbounded goroutines, and hidden
   shell reparsing. Keep cancellation propagation consistent within each
   lifetime.
5. Add deterministic tests for deadline expiry, detach/recovery, writer close
   order, lock contention, EEXIST races, unknown termination, and goroutine
   cleanup. Run `make test-race` and the affected acceptance gate.

## Deliverables

- an ownership/lifetime graph or equivalent written record;
- implementation or review changes with deterministic concurrency tests; and
- race/deadline command results plus known platform limits.

## Exit criteria

- Every goroutine and resource has an explicit owner, completion condition, and
  failure path.
- Watcher expiry cannot stop or relaunch detached provider execution.
- Sealing precedes terminal evidence, and collection cannot fabricate completion
  from unsealed, missing, or ambiguous state.
- Race tests and relevant supervisor/protocol acceptance pass without forbidden
  primitives or linter suppression.
