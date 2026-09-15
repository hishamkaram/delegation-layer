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

## Inputs

- the exact reviewed head and base commit;
- the changed files and their relevant consumers/call paths;
- the implementation handoff, test output, and acceptance receipts; and
- any previous finding and the claimed correction.

## Workflow

1. Read the full changed surface and relevant consumers, then verify that each
   claimed contract follows from code or an executed test.
2. Assess ownership and lifetimes, dependency direction, interface ownership,
   duplicated lifecycle/capability-check/harness logic, error diagnostics and
   secret handling, compatibility, and contributor effort.
3. Check launch planning versus execution, sealed-evidence interpretation,
   outcome authority, historical revisions, unsupported options, context
   lifetimes, and failure behavior. Treat a missing or inconclusive receipt as
   incomplete evidence.
4. Run the narrowest meaningful checks, then the applicable repository gate.
   Root obtains the independent review required by the execution plan with the
   direct Codex review CLI; an agent already performing this review does not
   launch a nested reviewer or Duo workflow.
5. Record each finding with severity, trigger, file/line, contract, correction,
   and regression evidence. Recheck accepted fixes on the new exact head.

## Deliverables

- a review record naming base and head;
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
- Root retains the review, commit, PR, CI, and merge decision.
