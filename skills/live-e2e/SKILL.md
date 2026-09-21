---
name: live-e2e
description: Execute bounded live provider and supervisor acceptance through the shipped dispatcher and runner with durable evidence.
---

# Live provider acceptance

## Trigger

Use this skill only when the requested evidence requires a real provider CLI,
real authentication, a real isolated supervisor, or a filesystem containment
probe. Use unit/contract tests for deterministic parser and policy behavior.

Read the [maintainability contract](../../agents/_data/maintainability-contract.md),
[adapter contract](../../agents/_data/adapter-contract.md),
[delegation invariants](../../agents/_data/delegation-invariants.md), and the
[security guide](../../docs/security.md).

## Inputs

- the adapter's supported profile, observed CLI version, output-predicate
  revision, and runtime capability result;
- an isolated workspace, state root, supervisor configuration, and finite task
  budget;
- existing authentication and required executable/configuration prerequisites;
- the exact success, continuation, refusal, timeout, containment, and replay
  scenarios required by the plan.

## Workflow

1. Verify prerequisites and record sanitized platform, executable, profile,
   configuration-source, task/session, timing, and digest metadata. Runtime
   preflight must locate an executable, verify it is executable, run version and
   help, and confirm every adapter flag is advertised; any reported CLI version
   is accepted when those checks pass. Never put
   credentials, tokens, unrelated environment values, or private raw logs in a
   receipt.
2. Launch through the shipped dispatcher/runner with its isolated private
   supervisor, finite
   stdin followed by EOF, and the declared profile. Do not call the provider
   directly, pass the brief in argv, mutate global config, or use direct
   signals.
3. Inspect filesystem and sealed evidence directly. Exercise the profile's
   positive behavior and any advertised native access boundary, then exact session continuation
   and observational replay. Preserve predecessor records byte-for-byte.
4. Apply the plan's result predicates. A missing prerequisite or inconclusive
   control is `BLOCKED`, never pass. A rejected task is evaluated from its
   authoritative outcome and sealed evidence; it is not required to have an
   empty rejection payload.
5. Save only sanitized receipts and report the exact command, status, identities,
   observations, and artifact digests. Do not retry paid work automatically.

Use `make acceptance-native` for the shared native mode matrix. Unattended write
scenarios must prove file creation, editing, shell execution, exact continuation,
and observational replay. Do not require outside-workspace denial from a mode
that delegates its access policy to the provider. Keep historical denial
fixtures as tests of their original contracts.

## Deliverables

- sanitized acceptance receipt and evidence manifest;
- task/session identities and direct filesystem observations;
- command, platform, provider version, profile, and predicate revision; and
- a named `passed`, `failed`, or `BLOCKED` result with the reason.

## Exit criteria

- Every required probe for the selected profile and requested scenario ran through the
  normal dispatch, execution, capture, sealing, collection, and publication
  path.
- Required positive controls, denial controls, session identity, replay
  immutability, and timeout/refusal behavior have direct evidence.
- No provider credential or private raw output entered the repository.
- Missing, unsupported, or inconclusive prerequisites remain explicitly
  `BLOCKED`; they are never reported as acceptance success.
