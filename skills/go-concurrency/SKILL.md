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

For supervisor changes, apply the recovery and verification section of the
[shared contract](../../agents/_data/maintainability-contract.md#recovery-and-verification).
For native process changes, also apply its
[native portability proof](../../agents/_data/maintainability-contract.md#native-portability-proof).

## Inputs

- the state machine and terminal outcomes affected by the change;
- owner and lifetime of every goroutine, file descriptor, lock, process, and
  context;
- deadline, stop-request, acknowledgement, termination, and recovery behavior;
- race, crash, writer-failure, and unknown-state scenarios to prove.

## Workflow

1. Draw the lifetime graph before editing and write every affected durable state
   change in the [canonical transition
   matrix](../../agents/_data/maintainability-contract.md#durable-transition-matrix-and-composed-tests).
   Assign one owner to each resource and state who closes it.
2. Keep the runner's execution lifetime independent from a caller/watcher's
   observation lifetime. Record stop requested, acknowledged, and termination
   observed as distinct facts; do not infer one from another.
3. Preserve at-most-once submission and provider start, immutable predecessors,
   and the single authoritative outcome. Apply the invariants' [four-state
   create-once recovery rule](../../agents/_data/delegation-invariants.md#ambiguous-create-once-writes)
   under the row's owner lease for every ambiguous durable write.
4. Apply the invariants' [canonical forbidden-signal policy and sole acceptance
   cleanup exception](../../agents/_data/delegation-invariants.md#forbidden-signal-policy).
   Also reject unbounded goroutines and hidden shell reparsing. Keep cancellation
   propagation consistent within each lifetime. Trace every nested timeout from
   the authoritative business deadline to storage and subprocess boundaries;
   reject a shorter inner convenience timeout. Give one layer ownership of the
   fallback used only when the caller supplied no finite deadline.
5. Add each row's named composed test through its affected production stores and
   real consumers, including deadline expiry, detach/recovery, writer close
   order, lock contention, create-once races, unknown termination, and goroutine
   cleanup. Run `make test-race` and the affected acceptance gate.
6. Apply the contract's [native backend and assurance
   matrix](../../agents/_data/maintainability-contract.md#native-portability-proof)
   to process identity, descendant discovery, signal delivery, and child
   reaping. Require native evidence whenever an affected backend changes.

## Deliverables

- an ownership/lifetime graph or equivalent written record;
- implementation or review changes with deterministic concurrency tests; and
- race/deadline command results, native receipts, and the contract's complete
  [evidence handoff](../../agents/_data/maintainability-contract.md#handoff-evidence).

## Exit criteria

- Every goroutine and resource has an explicit owner, completion condition, and
  failure path.
- Watcher expiry cannot stop or relaunch detached provider execution.
- Sealing precedes terminal evidence, and collection cannot fabricate completion
  from unsealed, missing, or ambiguous state.
- Race tests and relevant supervisor/protocol acceptance pass without forbidden
  primitives or linter suppression.
