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

For supervisor changes, apply the recovery and verification section of the
[shared contract](../../agents/_data/maintainability-contract.md#recovery-and-verification).
For process, supervisor, endpoint, or cleanup changes, also apply its
[native portability proof](../../agents/_data/maintainability-contract.md#native-portability-proof).

## Inputs

- the adapter's supported profile, observed CLI version, output-predicate
  revision, and runtime capability result;
- an isolated workspace, state root, supervisor configuration, and finite task
  budget;
- existing authentication and required executable/configuration prerequisites;
- the exact success, continuation, refusal, timeout, containment, and replay
  scenarios required by the plan; and
- the claim class (fixture, live provider, or native lifecycle), affected
  transition row IDs, selected native backend/assurance, and whether packaged or
  default-path behavior is being claimed.

## Workflow

1. Verify prerequisites and record sanitized platform, executable, profile,
   configuration-source, task/session, timing, and digest metadata. Runtime
   preflight must locate an executable, verify it is executable, run version and
   help, and confirm the adapter's stable runtime flags are advertised; any
   reported CLI version is accepted when those checks pass. Provider-specific
   forwarded options are proven by the live turn. Never put
   credentials, tokens, unrelated environment values, or private raw logs in a
   receipt.
   Apply the contract's [native backend matrix and bound receipt
   requirements](../../agents/_data/maintainability-contract.md#native-portability-proof).
   Keep fixture, provider, and native lifecycle claims separate; when credentials
   exist only in an interactive login context, launch the rebuilt Delegate
   inside that context rather than calling the provider directly.
2. Launch through the shipped dispatcher/runner with its isolated private
   supervisor and finite stdin followed by EOF for ordinary tasks, or the
   explicit interactive profile with core-owned protocol stdin until native
   completion. Exercise native
   question replies and steering only where advertised; waiting retains the
   original finite budget. Do not call the provider directly, pass the brief in
   argv, or mutate global config. Apply the invariants' [canonical
   forbidden-signal policy and narrow acceptance-driver
   exception](../../agents/_data/delegation-invariants.md#forbidden-signal-policy).
3. Inspect filesystem and sealed evidence directly. Exercise the profile's
   positive behavior and any advertised native access boundary, then exact
   session continuation and observational replay. Preserve predecessor records
   byte-for-byte.
   Before cleanup, enumerate every consumer of each resource or operational
   record being removed, including workers, observers, recovery, fixtures,
   provider harnesses, and receipt finalizers; record how each consumer reaches
   its verdict from durable evidence. Cleanup remains unauthorized until every
   listed consumer can finish from that evidence.
4. Apply the plan's result predicates. A missing prerequisite or inconclusive
   control is `BLOCKED`, never pass. A rejected task is evaluated from its
   authoritative outcome and sealed evidence; it is not required to have an
   empty rejection payload.
5. Satisfy every applicable item in the contract's [live acceptance
   proof](../../agents/_data/maintainability-contract.md#live-acceptance-proof):
   lifetime closure before cleanup, exact packaged/default-path proof, live
   lease capacity proof, and always-finalized bounded failure receipts.
   Keep fixture, native-lifecycle, packaged/default-path, and live-provider
   claims as separate verdicts with separate receipts; one class cannot upgrade
   another.
6. Finalize sanitized receipts for every exit and report the exact command,
   status, identities, observations, artifact digests, and independent claim
   verdicts. Do not retry paid work automatically.
7. Create one unique, disposable state root for each live test. Remove that
   root only after the canonical lifetime-closure receipt is committed. Record
   the root and parent identities when this invocation creates the root, and
   delete only through that token; a pathname, prefix, or boolean is not cleanup
   authority. Never remove a shared/default parent.
   Successful runs remove only invocation-owned raw process captures while
   retaining compact receipts. On a failure with unresolved process ownership,
   retain the evidence output and every owned root, and record the reason;
   never use broad prefix deletion or remove user state. The skill-routing
   harness additionally proves its private queue and daemon closure; only the
   nested Delegate task records can supply guardian empty-scope evidence.
   Reuse the shared cleanup helper and test both the closed and unclosed paths.

All acceptance harnesses share the process owner in
`../../scripts/acceptance_supervisor_common.py`. A provider harness may supply a
narrow stdin or bounded-output hook, but it must not add its own `Popen`, wait,
process registry, or cleanup implementation. Run the ownership verifier after
changing an acceptance harness.

Use `make acceptance-native` for the shared native mode matrix. Unattended write
scenarios must prove file creation, editing, shell execution, exact continuation,
and observational replay. Do not require outside-workspace denial from a mode
that delegates its access policy to the provider. Keep historical denial
fixtures as tests of their original contracts.

## Deliverables

- the contract's [handoff evidence
  manifest](../../agents/_data/maintainability-contract.md#handoff-evidence) with
  sanitized acceptance receipts;
- task/session identities and direct filesystem observations;
- command, exact source and executable fingerprints, platform, native primitive
  and assurance receipts, provider version, profile, and predicate revision;
- separate fixture, live-provider, and native-lifecycle verdicts; and
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
- Lifetime closure precedes owned-state cleanup; packaged/default-path and live
  capacity claims have the exact receipts required by the shared contract; and
  every bounded failure path has a finalized receipt.
- Every claimed OS-sensitive behavior names the native host kernel and
  architecture; build-only and foreign-kernel evidence remain separately
  labeled.
