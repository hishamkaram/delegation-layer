---
name: go-concurrency
description: Design and test goroutine, lock, deadline and subprocess changes with explicit lifetime ownership.
---

# Go concurrency

Use for goroutines, channels, locks, deadlines, process waiting, cancellation
or recovery races. Read [lifecycle ownership](../../agents/_data/delegation-invariants.md#3-supervision-deadlines-and-forbidden-signals)
and [recovery guidance](../../agents/_data/maintainability-contract.md#recovery-and-verification).
For OS primitives use [native checks](../../agents/_data/maintainability-contract.md#native-platform-checks).

## Work

1. Name each resource's owner, lifetime, close condition and failure path.
   Trace nested contexts and lock ordering before editing. Wait for upstream
   interfaces rather than create competing policies.
2. Keep observation lifetime separate from task execution. A watcher timeout,
   stop request or shutdown acknowledgement is not proof of worker termination.
3. Reuse launch/finalization owners and [ambiguous-write recovery](../../agents/_data/delegation-invariants.md#ambiguous-create-once-writes).
   Preserve immutable predecessors and the single terminal outcome.
4. Keep finite caller deadlines authoritative through nested operations; do
   not add shorter convenience timeouts. Preserve ownership through durable
   barriers and pending command completion. No hidden shell reparsing.
5. Respect the [forbidden-signal policy](../../agents/_data/delegation-invariants.md#forbidden-signal-policy).
   Every Start has one Wait; all spawned work has bounded cleanup on failures.
6. Test races deterministically at ownership gates, including lock contention,
   interrupted writes, detach/recovery, cancellation and joined cleanup faults.
   Real deadline/capacity claims require actual clocks and concurrent owners,
   not sleeps, launch barriers or a queue length.

## Result and checks

Return the bounded implementation/tests or read-only findings, commands/results
and unresolved limits. Run affected race tests; Root owns the full integrated
gate. Do not add mandatory transition matrices or handoff receipts. Native
runtime assurance is established only on the affected supported OS/backend.

Report affected guidance and diagram changes under the project
[documentation completion rule](../../AGENTS.md#documentation-completion).
