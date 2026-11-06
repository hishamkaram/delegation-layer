---
name: supervisor-architect
description: Design supervisor lifecycle and recovery changes with one policy owner and no duplicate launch or cleanup authority.
---

# Supervisor architect

Use for supervisor binding, migration, observation, maintenance or cleanup
changes. Load the [recovery rules](../../agents/_data/maintainability-contract.md#recovery-and-verification)
and [admission ordering](../../agents/_data/delegation-invariants.md#4-supervisor-admission-ordering).
For native changes, also use [native checks](../../agents/_data/maintainability-contract.md#native-platform-checks).
Apply the [installed-executable rule](../../agents/_data/product-boundary.md#installed-executables)
to supervisor, worker, callback and migration executable selection.

## Inputs

The assigned scope/base, affected state and resource owners, normal/recovery
callers, stored records and public command success/failure behavior. Use current
[architecture](../../docs/architecture.md) rather than a duplicate symbol list.

## Work

1. Trace normal execution and recovery together. One application owner decides
   lifecycle policy; lower layers supply observations and authorized primitives.
   Reuse canonical resolvers/finalizers instead of command-specific shortcuts.
2. Identify commit points, leases, ambiguous writes and every consumer that
   needs an operational record before removal. Apply the [create-once rule](../../agents/_data/delegation-invariants.md#ambiguous-create-once-writes).
3. Distinguish published observability from mutation completion. Revalidate
   under the actual lease; test permitted reads and refused mutations against
   the same state, including joined faults and pending commands.
4. Preserve stashed-add/receipt/enqueue ordering and reusable numeric-ID
   protection. Running supervised workers consume their saved bindings, not
   bootstrap or queue-wide scans. Callback/events trigger cleanup but never
   replace durable task authority.
5. Keep control commands, pending operations and cleanup owned through their
   natural completion. Shutdown acknowledgement is not process exit. Scope
   disposal requires the existing termination and worker-Wait authority.
6. Test production stores and public consumers through interruption, concurrent
   callers, duplicate identities and restart. Deterministically gate exact
   ownership claims; distinguish capacity from dispatch-burst throughput.

## Result and checks

Return a bounded change with its owner/interface decisions, actual test results
and unavailable checks. No mandatory transition IDs or proof manifest. Run
focused checks during edits; Root owns integrated checks and independent review.
Native runtime claims require execution on the affected supported host, not a
cross-build, fixture on another kernel or upstream portability claim.
