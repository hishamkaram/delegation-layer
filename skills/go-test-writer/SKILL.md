---
name: go-test-writer
description: Design meaningful Go tests for provider contracts, durable evidence, failure paths, and compatibility behavior.
---

# Go test writer

## Trigger

Use this skill when a change needs new Go unit, integration, contract, or
fault-injection tests. Use [live-e2e](../live-e2e/SKILL.md) when real provider
processes or pueue behavior is required.

Read the [maintainability contract](../../agents/_data/maintainability-contract.md),
[delegation invariants](../../agents/_data/delegation-invariants.md),
[adapter contract](../../agents/_data/adapter-contract.md), and
[code-quality floor](../../agents/_data/code-quality-floor.md).

For supervisor changes, apply the recovery and verification section of the
[shared contract](../../agents/_data/maintainability-contract.md#recovery-and-verification).
For process, supervisor, endpoint, or cleanup changes, also apply its
[native portability proof](../../agents/_data/maintainability-contract.md#native-portability-proof).

## Inputs

- the behavior contract, affected canonical transition rows, production record
  stores, and real consumer interfaces;
- the success, rejection, timeout, malformed, conflict, and writer-failure
  cases that matter;
- existing fixtures, historical predicate revisions, and persistence formats;
- injected filesystem/writer/process doubles allowed by the test boundary.

## Workflow

1. Turn each affected [canonical transition
   row](../../agents/_data/maintainability-contract.md#durable-transition-matrix-and-composed-tests)
   into observable assertions for its durable facts, lease, commit point,
   recovery result, and real consumer verdict.
2. Add a compact test-case set covering positive behavior and meaningful
   negative branches, including blank answers, nonzero exits, duplicate critical
   fields, unsealed evidence, EEXIST conflicts, missing usage values, and all
   applicable [ambiguous create-once
   classifications](../../agents/_data/delegation-invariants.md#ambiguous-create-once-writes).
3. For lifecycle rows, compose each affected production record store with the
   real consumer named by the row. Inject deterministic failures around that
   store without replacing its serialization or create-once behavior. Fakes
   remain limited to isolated protocol ordering. Keep provider interpretation
   pure over sealed input and process/lifecycle behavior on the shared runner;
   do not copy the same harness into each adapter.
4. Add compatibility fixtures for existing IDs and historical interpretations.
   Assert that collection remains observational and that rejected tasks retain
   their authoritative outcome/payload relationship.
5. Run the focused tests, then `make test-race` and the applicable gate. Keep
   assertions independent of incidental timestamps or internal helper names;
   assert ordering when ordering is the contract, such as closing writers
   before committing a seal.
6. For an OS-sensitive lifecycle change, apply the contract's [native backend
   matrix and receipt requirements](../../agents/_data/maintainability-contract.md#native-portability-proof).
   Include nested group/session descendants, exact `Wait`, interruption
   recovery, identity reuse protection, and the upgrade path when relevant.
   Cross-compilation is an additional check and never satisfies the native gate.

## Deliverables

- canonical transition rows tied to test names, production stores, and real
  consumers;
- deterministic fixtures, mocks, and tests for the changed contract; and
- the contract's [evidence
  handoff](../../agents/_data/maintainability-contract.md#handoff-evidence),
  including command output and unresolved live-acceptance scenarios.

## Exit criteria

- Positive and failure behavior are tested through every affected production
  store at the real consumer boundary.
- Writer, malformed-input, identity, conflict, and lifetime errors are covered
  where the change can produce them.
- Tests do not spawn paid providers, weaken existing assertions, suppress
  linters, or impose an arbitrary coverage target.
- Race-enabled project tests pass, or the handoff names the exact external
  blocker and leaves the scenario unclaimed.
- Platform receipts identify the actual kernel and architecture and do not
  promote cross-build or foreign-kernel fixture evidence to a native pass.
