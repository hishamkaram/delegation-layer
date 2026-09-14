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

## Inputs

- the behavior contract and affected consumer interface;
- the success, rejection, timeout, malformed, conflict, and writer-failure
  cases that matter;
- existing fixtures, historical predicate revisions, and persistence formats;
- injected filesystem/writer/process doubles allowed by the test boundary.

## Workflow

1. Turn the contract into observable assertions: durable state, returned error
   identity/context, bytes and digests, launch count, session identity, and
   publication verdict where applicable.
2. Build a compact matrix covering positive behavior and meaningful negative
   branches, including blank answers, nonzero exits, duplicate critical fields,
   unsealed evidence, EEXIST conflicts, and missing usage values where relevant.
3. Inject deterministic failures at the boundary that owns them. Keep provider
   interpretation tests pure over sealed input and keep process/lifecycle tests
   on the shared runner; do not copy the same harness into each adapter.
4. Add compatibility fixtures for existing IDs and historical interpretations.
   Assert that collection remains observational and that rejected tasks retain
   their authoritative outcome/payload relationship.
5. Run the focused tests, then `make test-race` and the applicable gate. Keep
   assertions independent of incidental timestamps or internal helper names;
   assert ordering when ordering is the contract, such as closing writers
   before committing a seal.

## Deliverables

- a behavior matrix tied to test names;
- deterministic fixtures, mocks, and tests for the changed contract; and
- command output plus a note about scenarios that require live acceptance.

## Exit criteria

- Positive and failure behavior are tested at the real consumer boundary.
- Writer, malformed-input, identity, conflict, and lifetime errors are covered
  where the change can produce them.
- Tests do not spawn paid providers, weaken existing assertions, suppress
  linters, or impose an arbitrary coverage target.
- Race-enabled project tests pass, or the handoff names the exact external
  blocker and leaves the scenario unclaimed.
