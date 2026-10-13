---
name: supervisor-architect
description: Design or review supervisor lifecycle changes with one policy owner and no duplicated recovery logic.
---

# Supervisor architect

## Trigger

Use this skill for changes to private supervisor binding, continuation,
migration, cleanup, recovery journals, queue ownership, or lifecycle evidence.
Load the [maintainability contract](../../agents/_data/maintainability-contract.md),
[delegation invariants](../../agents/_data/delegation-invariants.md), and
[architecture guide](../../docs/architecture.md) first.

## Workflow

1. Confirm the indexed project and generation. Use the codebase graph to find
   the current lifecycle entry points, callers, and both dependency directions;
   fall back to exact source searches only for literals or uncovered ranges.
2. Write the affected rows using the contract's [canonical durable transition
   matrix](../../agents/_data/maintainability-contract.md#durable-transition-matrix-and-composed-tests)
   before choosing a design. Give every row a stable ID and cover normal entry,
   interrupted entry, recovery, and each consumer verdict.
3. Apply the contract's [boundary and ownership
   assignments](../../agents/_data/maintainability-contract.md#boundary-and-ownership).
   Record the policy owner and named lease in every row, and use one narrow
   consumer-owned interface when a lower layer needs authorization.
4. Search for existing predicates and resolvers before adding one. Reuse the
   canonical policy and route every command path through it. Do not create a
   second cleanup verifier for a new record family or a command-specific
   migration shortcut.
5. Apply the invariants' [ambiguous create-once
   rule](../../agents/_data/delegation-invariants.md#ambiguous-create-once-writes)
   to every affected record and map all four classifications to matrix rows.
6. Apply the shared [recovery and verification
   rules](../../agents/_data/maintainability-contract.md#recovery-and-verification),
   including the canonical production scope-creation path and backend assurance
   matrix in its native portability section.
7. Keep lifecycle control out of running-worker startup. Dispatch or an
   explicit observer owns bootstrap, repair, migration, and queue scans. A
   supervised worker is first added as an exact stashed row; the application
   publishes its durable numeric receipt before enqueueing that row. The worker
   consumes that validated receipt and must not repeat those lifecycle
   operations. Retain reconciliation only for
   legacy or direct invocations without the supervised launch marker.
8. Keep healthy admission free of supervisor subprocesses. Validate the exact
   published generation and scheduling receipt under its shared lease, then
   probe the private Unix endpoint directly. Route missing, refused, changed,
   or stale state to one elected exclusive protocol check and repair.
9. Before authorizing cleanup or disposal, enumerate every consumer of each
   operational record or resource being removed, including workers, observers,
   recovery, fixtures, provider harnesses, and receipt finalizers. Record how
   every consumer finishes from durable evidence; a queue row or worker exit
   alone cannot authorize cleanup.
10. Satisfy the matrix section's composed-test requirement for every affected
   production record store and real consumer. Name the tests in their rows and
   keep fake-only tests scoped to isolated protocol ordering.
11. Apply the [native portability and backend
    proof](../../agents/_data/maintainability-contract.md#native-portability-proof)
    and, for live runs, every [live acceptance
    requirement](../../agents/_data/maintainability-contract.md#live-acceptance-proof).
    Keep fixture, native-lifecycle, packaged/default-path, and live-provider
    claims separate with their own verdicts and receipts; one class cannot
    upgrade another.
12. Produce the contract's [complete evidence
    handoff](../../agents/_data/maintainability-contract.md#handoff-evidence),
    keyed by transition-row ID.

## Review questions

- Can one function or policy gate answer this lifecycle question for every
  record family and command path?
- Does any lower layer know Delegate schemas or make a decision owned by the
  application?
- Can a continuation or cleanup path create a second queue, delete ambiguous
  work, or bypass the recovery journal?
- Does an already-running worker perform bootstrap, recovery, or a queue-wide
  status scan that belongs to dispatch or observation?
- Does healthy warm admission launch a supervisor client process, or can it use
  the exact-generation receipt and direct endpoint probe?
- Can any worker or deferred close path destroy a native scope before durable
  empty-scope termination, or bypass the application finalizer?
- Does each affected production store reach the real consumer named in its row?
- Does any production path bypass persisted intent or create a backend other
  than the row selected by the native backend matrix?
- Is every platform claim backed by native evidence for the exact changed
  primitive and lifecycle path?

## Exit criteria

- The owner and dependency direction are recorded.
- The canonical matrix has one implementation and a named composed test for
  each decision.
- No duplicate lifecycle predicate or schema-specific lower-layer policy was
  added.
- Focused tests, race-sensitive tests where applicable, and repository skill
  verification pass.
- Native Linux and macOS gates pass when the change affects their lifecycle
  behavior; unsupported or untested paths remain explicitly unclaimed.
- The handoff binds source, binaries, receipts, claim classes, and unresolved
  evidence to the affected row IDs.
