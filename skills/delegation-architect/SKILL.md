---
name: delegation-architect
description: Shape provider and delegation boundaries when ownership, interfaces, or catalog structure must change.
---

# Delegation architect

## Trigger

Use this skill for a design or implementation request that changes provider
registration, preparation, capability selection, task boundaries, or the
relationship between orchestration and an adapter. Do not use it for a local
bug fix whose boundary is already established.

Read the [maintainability contract](../../agents/_data/maintainability-contract.md),
[delegation invariants](../../agents/_data/delegation-invariants.md),
[adapter contract](../../agents/_data/adapter-contract.md), and the
[execution plan](../../docs/EXECUTION-PLAN.md) before deciding on a boundary.

## Inputs

- the bounded request and the files the change may own;
- current consumers, call paths, persisted records, and provider profiles;
- compatibility requirements and the relevant acceptance scenarios; and
- existing interfaces, value types, and failure behavior to preserve.

## Workflow

1. Map the consumer that needs each operation and assign the interface to that
   consumer. Keep preparation and interpretation provider-owned while shared
   execution and publication stay in the core.
2. Trace data and resource lifetimes through admission, launch, evidence
   sealing, collection, and publication. Identify where ownership transfers and
   which failures leave durable evidence.
3. Compare the proposed abstraction with existing task, execution, predicate,
   usage, lifecycle, catalog, and harness types. Reuse a type when it already
   expresses the contract; add a new one only with a concrete consumer.
4. Compare keeping the current boundary or making the smallest refactor with
   the proposed design and any relevant alternative. Choose the option with
   the clearest ownership and lowest supported maintenance cost, then record
   why the alternatives were rejected.
5. Check duplicate registration, historical interpretation, unsupported
   options, identity/session compatibility, and the no-launch collection rule.
6. Write a bounded design or implementation handoff naming consumers,
   invariants, tests, acceptance evidence, and deferred work. Record a reason
   for each new dependency or abstraction.

## Deliverables

- a boundary map with ownership and lifetime responsibilities;
- narrow interface and catalog decisions tied to real consumers;
- compatibility and failure-path notes; and
- a file-bounded implementation plan with validation evidence.

## Exit criteria

- Every operation has one clear owner and no adapter owns pipes, processes,
  stopping, sealing, capture, or publication.
- Existing persisted behavior and historical predicate revisions have an
  explicit preservation or migration decision.
- The design has a meaningful test and acceptance path, with no arbitrary
  coverage or file-size gate added.
- Root can review the handoff without needing an unrecorded architectural
  assumption.
