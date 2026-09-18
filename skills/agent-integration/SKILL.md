---
name: agent-integration
description: Use Delegation Layer safely from a provider-agnostic agent workflow.
---

# Agent integration

## Trigger

Use this skill when an agent must choose a provider, submit a bounded task, or
interpret Delegation Layer JSON. The skill applies to every provider profile
and does not encode provider names, release versions, or native CLI syntax.

## Inputs

- the requested permission mode, options, workspace, and finite brief;
- the output from `delegate providers --json` or
  `delegate capabilities --provider PROFILE --json`; and
- the versioned JSON responses and sanitized live-acceptance receipts returned
  by the normal command path.

Read the [integration API](../../docs/api.md), [delegation invariants](../../agents/_data/delegation-invariants.md), and [adapter contract](../../agents/_data/adapter-contract.md) before implementing a new integration.

## Workflow

1. Use `delegate providers --json` only to discover compiled profiles, modes,
   options, help arguments, and required flags. It does not prove that a host
   is ready.
2. Use `delegate capabilities --provider PROFILE --json` to read the
   provider-neutral contract. `status=unknown` with `verification=catalog`
   means that the bounded runtime probe has not run; it is not a compatibility
   failure and not live acceptance.
3. Submit work through `dispatch --json`. Treat `capability.status=ready` and
   `capability.verification=runtime` as proof that the shared capability probe
   observed the required behavior for that admission boundary. Inspect the
   task-level `admission`, `liveness`, and `publication` fields separately.
4. Never compare a provider version with a hardcoded allowlist or use a web
   release lookup. Version text is diagnostic evidence; behavior, flags, and
   executable identity establish compatibility.
5. Treat live acceptance as a separate result. `passed` requires authenticated
   provider evidence. `blocked` means authentication or another prerequisite
   was unavailable and is neutral; it must never be converted into success or
   reported as incompatibility.
6. Poll with `status` or `collect` using the original task ID. Do not launch,
   retry, resume, or replay work after uncertain admission. Trust only the
   validated terminal `outcome` and its sealed evidence.
7. Keep credentials, tokens, private raw output, and unrestricted diagnostics
   out of prompts, tool results, receipts, and task metadata.

## Acceptance contract

An integration is acceptable only when the same criteria hold for every
provider:

- required behavior is verified at the runtime boundary;
- arbitrary valid version text remains acceptable;
- missing flags, executable identity drift, malformed facts, and timeouts fail
  closed;
- authentication is represented separately from compatibility;
- missing authentication is `blocked`, never a passing skip;
- task admission remains at most once and collection remains observational;
- terminal results come from durable validated evidence; and
- JSON output is bounded, deterministic where the command is observational,
  versioned, and free of credentials.

## Deliverables

- the original task ID and the bounded JSON response;
- the terminal outcome and validated evidence descriptors; and
- a sanitized live-acceptance receipt with `passed`, `failed`, or `blocked`.

## Exit criteria

- The selected profile advertised the requested mode and options.
- Runtime capability status and task admission were read from JSON rather than
  inferred from version text or provider prose.
- Any live prerequisite failure remains explicitly `blocked`.
- No automatic retry or credential disclosure occurred.
- The [repository contracts](../../agents/_data/maintainability-contract.md)
  and the integration API remain satisfied.

Run `make verify-skills` after changing this skill or its links.
