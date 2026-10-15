---
name: supervisor-architect
description: Design or review supervisor lifecycle changes with one policy owner and no duplicated recovery logic.
---

# Supervisor architect

Apply the shared [upgrade recovery verification](../../agents/_data/maintainability-contract.md#upgrade-recovery-verification)
when installation paths, identity publication, or retained pairs change.

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
12. Gate concurrency proofs at the ownership boundary they claim. A launcher
    barrier does not prove every child crossed an internal single-flight entry
    before publication. Keep exact-flight tests deterministic, and let native
    process bursts record coalescing without introducing stale-result windows.
    For runtime-only tasks, use static admission and the task's own fresh launch
    preflight. Do not add an admission capability cache or a second probe; the
    metadata's required capability digest remains authoritative until launch.
13. Produce the contract's [complete evidence
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
- Is terminal cleanup triggered outside admission, with one nonblocking
  per-root election winner before a streamed queue snapshot and application
  authorization?
- Does the callback remain only a trigger/wake-up hint while durable Delegate
  journals remain the sole cleanup and admission authority?
- Does one per-root journal own requested, attempted, and successful
  maintenance generations, does the event inbox recover a failed journal or
  launch handoff, and does the election lock alone identify a live owner?
- Does the event lock recover exact-shaped publisher stages from every
  interrupted create-once window before strict committed-record validation,
  while rejecting every other unexpected directory entry?
- Does bounded quiet settling batch callback bursts, does full scope dominate,
  does each later pass get the same quiet settling, and does a wake published
  after a pass captures its target remain above the attempted watermark?
- Does a child-process death test prove real election-lock release and
  interrupted-pass recovery?
- Does a finished failed attempt require a new wake instead of busy-looping?
- Does candidate-publication failure still persist a conservative wake before
  supervisor recovery, without turning that hint into removal authority?
- Does an independently accepted terminal inspection always publish a wake and
  start an independent `delegate-run` maintenance process, so admission does
  not wait, caller exit cannot kill cleanup, and a missing or failed callback
  cannot leave the wake unowned?
- Does an in-flight control command remain unattempted and owned through natural
  completion instead of consuming its generation at an observer timeout?
- Does a retry observe release of every election lease retained by that command
  before attempting re-election, rather than treating pending completion as
  lock release?
- Does normal cleanup expose only event-backed ordinary and inspection
  candidates, retain same-label rows for ambiguity checks, and include an exact
  application-observation fallback when the configured callback is delayed?
- Do cleanup and migration consume the same canonical inspection terminal
  evidence as admission, including exact failed exits without local completion?
- Do callback-disabled historical roots retain exact journal reconciliation
  instead of becoming permanently ineligible for cleanup?
- Does candidate filtering retain every same-label row so duplicate evidence
  reaches both application authorization and fresh mutation revalidation?
- Does application authorization stay outside queue identity, followed by one
  fresh same-scope identity check and exact removal under the mutation lock?
- Does a waiting queue writer publish intent before it can be overtaken, do
  readers check that intent on both sides of the short gate, and can stale
  intent be repaired only after proving its owner lock is free?
- Does each nested control scope append its generation, intent, gate, or
  identity lease, and does a caller-exit test prove the external command keeps
  every outer lease until its exact `Wait`?
- Is every generated Unix socket path checked against the target OS byte limit
  before config publication, with native packaged-path evidence at the
  boundary? Can an over-limit interrupted config be replaced only when the
  bootstrap journal and absence of every active-generation signal prove it is
  safe?
- Is callback state cleared only after successful removal and retained after a
  failed removal so the same exact row remains retryable?
- Do staged recovery and queued inspection removal prevent callback tickets
  from crossing numeric-ID reuse?
- Can a later terminal callback retry a failed maintenance pass without changing
  the already-recorded task or inspection verdict?
- Can a compatible callback-helper upgrade atomically refresh the stable helper
  path without changing the live daemon's callback contract?
- Is staged add/enqueue capability probed only while publishing an
  exact-generation scheduling receipt, then reused by warm submission and
  staged recovery without help-process fan-out?
- Can any worker or deferred close path destroy a native scope before durable
  empty-scope termination, or bypass the application finalizer?
- Does each affected production store reach the real consumer named in its row?
- Are fixture orchestration assertions labeled separately from native capacity
  evidence with distinct live leases and lifetime-owner contention?
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
