---
name: docs-updater
description: Update repository guidance, plans, feature matrices, and evidence receipts so claims match implemented behavior.
---

# Documentation updater

## Trigger

Use this skill when a code or acceptance change requires updates to a plan,
feature matrix, handoff, receipt, contract link, or contributor instruction.
Root owns the normative planning documents and integrates proposed edits.

Read the [maintainability contract](../../agents/_data/maintainability-contract.md),
[delegation invariants](../../agents/_data/delegation-invariants.md),
[adapter contract](../../agents/_data/adapter-contract.md), and
[execution plan](../../docs/EXECUTION-PLAN.md).

## Inputs

- the exact changed head and behavior/test/acceptance evidence;
- the authoritative document and row or section to update;
- current provider IDs, profile/predicate revisions, commands, and platform
  scope; and
- deferred work that must remain visibly planned.

## Workflow

1. Read the authoritative source and the changed code/receipts before writing.
   Preserve historical measurements and label current status from actual
   evidence only.
2. Update the smallest applicable section. Reconcile feature-matrix rows,
   commands, links, provider metadata, task/session identities, digests, and
   platform limits. Use the repository's explicit planned marker for work that
   has no implementation or receipt yet.
3. Keep ownership and boundary guidance in the shared maintainability contract;
   link to it from role skills instead of copying the same rules into several
   documents.
4. Remove stale acceptance claims, future commands presented as available, and
   receipts that expose credentials, tokens, private paths, or raw output.
5. Run `make verify-skills` for skill/link/command changes and the affected
   documentation or code checks. Report the exact evidence and leave root to
   integrate planning-document changes.

## Deliverables

- a focused documentation diff with relative links and current status;
- reconciled commands, scenarios, and evidence identifiers; and
- a short provenance note naming the source behavior and validation command.

## Exit criteria

- Every implemented claim has matching code or executed evidence; every future
  item is clearly marked planned.
- Links resolve, command names match the Makefile, and no unfinished scaffold
  marker remains.
- Historical facts remain identifiable as historical, while incomplete native
  evidence remains `BLOCKED` rather than a passing skip.
- Root can review the changed document without guessing which claims are new.
