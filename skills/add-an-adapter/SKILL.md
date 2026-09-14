---
name: add-an-adapter
description: Add and certify a provider adapter while preserving shared lifecycle ownership and versioned evidence interpretation.
---

# Add a provider adapter

## Trigger

Use this skill for a new provider adapter or a repair to an adapter contract,
profile, interpreter, registration, fixture, or acceptance gate. Read the
[maintainability contract](../../agents/_data/maintainability-contract.md),
[adapter contract](../../agents/_data/adapter-contract.md),
[delegation invariants](../../agents/_data/delegation-invariants.md),
[code-quality floor](../../agents/_data/code-quality-floor.md), and
[execution plan](../../docs/EXECUTION-PLAN.md) first.

## Inputs

- the immutable provider ID, supported request options, certified profile,
  executable/version/platform scope, and predicate revision;
- the exact owned package, catalog, fixture, harness, and documentation files;
- the provider's configuration precedence, identity/session semantics, native
  envelope, usage fields, and failure markers; and
- the plan's unit, supervisor, containment, continuation, refusal/timeout, and
  replay scenarios.

## Workflow

1. Define the smallest certified profile and reject escalation, arbitrary
   executable loading, unsupported options, and unrecognized effective policy
   before admission. Preserve existing IDs and native login flows.
2. Implement provider-owned preparation, native argv/configuration and identity
   handling, and a versioned interpreter over sealed evidence. The adapter only
   prepares a plan and interprets evidence: it does not own pipes, capture,
   sealing, processes, stopping, deadlines, collection, or publication.
3. Register the adapter once in the explicit catalog. Reuse shared task,
   execution, predicate, usage, lifecycle, and acceptance-harness mechanics;
   do not add provider-specific branches to task, execution, or publication.
4. Add deterministic fixtures for valid and malformed envelopes, blank answers,
   refusal/error/timeout markers, exit conflicts, identity mismatch, policy
   drift, usage scope, and historical predicate revisions. Collection must stay
   observational and must never retry paid work.
5. Run the provider through the shipped dispatcher/runner with isolated pueue,
   finite stdin followed by EOF, existing authentication, and the exact plan
   budgets. Inspect sealed raw evidence and filesystem effects directly; do not
   use direct signals, shell wrappers, or global configuration changes.
6. For the existing agy profile, accept the containment-denial case when the
   positive filesystem control is verified and the attempted write is explicitly
   denied. Do not require a successful published first turn or an empty payload
   for a rejected task. Missing prerequisites and inconclusive controls are
   `BLOCKED`, never pass.
7. Record sanitized platform/version/profile/predicate/configuration, task and
   session identities, timings, commands, observations, and digests. Keep raw
   private evidence outside the repository and let root update normative plans.

## Deliverables

- provider package, catalog registration, and focused deterministic fixtures;
- a profile and interpretation record with compatibility and failure behavior;
- acceptance receipt covering positive, denial, continuation, timeout/refusal,
  and replay scenarios; and
- a bounded handoff naming changed files, commands, results, and blocked cases.

## Exit criteria

- Provider behavior is contained behind a real narrow boundary; shared lifecycle,
  capture, sealing, and publication code is reused without exceptions.
- Success requires valid versioned sealed evidence, matching identities, and a
  non-whitespace answer; conflict, refusal, timeout, or unsealed evidence does
  not publish.
- Unit/fault tests and required supervisor/live acceptance pass with sanitized
  evidence, or a missing prerequisite is recorded as `BLOCKED`.
- Historical collection remains available without the current executable, and
  no paid retry or automatic fallback was introduced.
