---
name: go-reviewer
description: Review a Go change or provider handoff for maintainability, contract compliance, and evidence-backed merge readiness.
---

# Go reviewer

## Trigger

Use this skill for a read-only review of a diff, branch head, implementation
handoff, or review fix. It produces findings and evidence; it does not implement
fixes, commit, push, merge, or turn an incomplete acceptance into approval.

Read the [maintainability contract](../../agents/_data/maintainability-contract.md),
[delegation invariants](../../agents/_data/delegation-invariants.md),
[adapter contract](../../agents/_data/adapter-contract.md), and
[code-quality floor](../../agents/_data/code-quality-floor.md).

For supervisor changes, apply the recovery and verification section of the
[shared contract](../../agents/_data/maintainability-contract.md#recovery-and-verification).
Check its [upgrade recovery verification](../../agents/_data/maintainability-contract.md#upgrade-recovery-verification)
for changed installation or publication paths.
For process, supervisor, endpoint, or cleanup changes, also apply its
[native portability proof](../../agents/_data/maintainability-contract.md#native-portability-proof).

## Inputs

- the exact reviewed source snapshot, including changed and untracked inputs,
  plus base and head revisions when available;
- the changed files and their relevant consumers/call paths;
- the implementation handoff, test output, and acceptance receipts; and
- any previous finding and the claimed correction.

For managed workspace changes, review the canonical [W1–W7 transition
rows](../../agents/_data/maintainability-contract.md#managed-workspace-transitions)
and [workspace reference](../agent-integration/references/workspaces.md).
Check one coordinator owner, non-renewing auxiliary deadlines, exact closure
before capture/cleanup, and owned native-proof cleanup without duplicating
Git-subprocess lifecycle policy in the review skill.

## Workflow

1. Validate the [handoff evidence
   manifest](../../agents/_data/maintainability-contract.md#handoff-evidence),
   including a passing `make verify-maintainability-strict` for the exact
   snapshot before peer review. Missing, failed, or stale gate evidence leaves
   review incomplete. Then read the full changed surface and relevant consumers. Verify that each
   claimed contract follows from that exact source and binary snapshot or an
   executed test.
   Trace shared gates through indirect callers to public commands. A helper's
   name or immediate caller does not establish its role. Pair allowed reader
   behavior with refused mutations using the same durable-state fixture.
2. Assess ownership and lifetimes, dependency direction, interface ownership,
   duplicated lifecycle/capability-check/harness logic, error diagnostics and
   secret handling, compatibility, and contributor effort. Review every affected
   [canonical transition
   row](../../agents/_data/maintainability-contract.md#durable-transition-matrix-and-composed-tests)
   against its owner, commit point, recovery, consumer verdict, and named test.
3. Check launch planning versus execution, sealed-evidence interpretation,
   outcome authority, historical revisions, unsupported options, context
   lifetimes, the [ambiguous create-once
   rule](../../agents/_data/delegation-invariants.md#ambiguous-create-once-writes),
   and failure behavior. Confirm each lifecycle test uses every affected
   production record store and real consumer; fake-only evidence proves only
   isolated protocol ordering. Treat a missing or inconclusive receipt as
   incomplete evidence.
   Reject a platform claim supported only by compilation, a container on a
   different kernel, fixture evidence from another OS, or upstream portability.
4. Run the narrowest meaningful checks, then the applicable repository gate.
   Root obtains any required independent review through the session-authorized
   integration; a reported blocker is not review approval. An agent already performing this review does not
   launch a nested reviewer or Duo workflow.
5. Record each finding with severity, trigger, file/line, contract, correction,
   and regression evidence. Recheck accepted fixes on the new exact source
   snapshot.
6. Keep an active review incomplete until its terminal result is observed and
   published. Timeout or silence is not permission to kill the review. A
   changed reviewed source snapshot invalidates prior findings or approval;
   recheck the new exact snapshot.

## Deliverables

- a review record naming base and head when available;
- the exact source and binary fingerprints actually reviewed;
- findings with concrete code evidence and status;
- executed commands and results, including native status; and
- a merge recommendation only when no actionable finding or incomplete gate
  remains.

## Exit criteria

- The changed consumers and both call directions relevant to the claim were
  inspected.
- Every actionable finding is fixed and reverified, or is explicitly unresolved
  with its blocking evidence.
- No approval is reported for pending, stale, skipped, blocked, or inconclusive
  acceptance evidence.
- Native evidence covers each affected supported OS and is labeled separately
  from cross-build, fixture, and provider-authentication results.
- Fixture, live-provider, and native-lifecycle claims retain separate verdicts
  and bound receipts.
- Root retains the review, commit, PR, CI, and merge decision.
