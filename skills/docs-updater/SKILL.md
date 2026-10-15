---
name: docs-updater
description: Update repository guidance and local maintenance records so claims match implemented behavior.
---

# Documentation updater

## Trigger

Use this skill when a code or acceptance change requires updates to product
documentation, a contract link, a handoff, a receipt, or contributor guidance.
Keep local planning and evidence records out of the product documentation tree.

Read the [maintainability contract](../../agents/_data/maintainability-contract.md),
[delegation invariants](../../agents/_data/delegation-invariants.md),
[adapter contract](../../agents/_data/adapter-contract.md), and
[contributor guide](../../docs/contributing.md).

## Inputs

- the exact changed head and behavior/test/acceptance evidence;
- the authoritative source and document section to update;
- current provider IDs, profile/predicate revisions, commands, and platform
  scope; and
- deferred work that must remain explicitly scoped.

## Workflow

1. Read the authoritative source and the changed code/receipts before writing.
   Preserve useful history in the local archive and label current behavior from
   actual evidence only.
2. Update the smallest applicable section. Reconcile commands, links, provider
   metadata, task/session identities, digests, and platform limits. Mark work
   that has no implementation or receipt as deferred rather than presenting it
   as available.
3. Keep ownership and boundary guidance in the shared maintainability contract;
   link to it from role skills instead of copying the same rules into several
   documents.
4. Remove stale acceptance claims, future commands presented as available, and
   receipts that expose credentials, tokens, private paths, or raw output.
5. Compare provider tables against the native catalog and execute changed shell
   examples, including nonzero exits with errexit enabled. Keep detailed agent
   response decisions in the canonical result-handling reference and synchronize
   the plugin skill distribution; do not add another decision list.
6. For repository diagrams, pin a reachable implementation commit and refresh
   source ranges from that snapshot. Finalize each canonical JSON/HTML through
   Archify, then capture both themes at the required viewport sizes. Inspect
   every capture before claiming visual acceptance. Keep raw receipts unchanged
   in the ignored local archive: their absolute paths and hashes bind exact
   artifacts. Archive obsolete contact sheets and proof only after replacement
   captures pass. Publish the canonical specs, HTML, and accepted PNGs, then
   generate `diagrams/SHA256SUMS` last. Run `make verify-diagram-checksums`.
   Checksums prove byte integrity only; they do not prove capture freshness or
   visual correctness. Do not rewrite receipt paths to make proof publishable.
7. Run `make verify-skills` for skill/link/command changes and the affected
   documentation or code checks. Report the exact evidence and leave root to
   integrate the final documentation changes.

## Deliverables

- a focused documentation diff with relative links and current behavior;
- reconciled commands, scenarios, and evidence identifiers; and
- a short provenance note naming the source behavior and validation command.

## Exit criteria

- Every implemented claim has matching code or executed evidence; every future
  item is clearly marked as deferred.
- Links resolve, command names match the Makefile, and no unfinished scaffold
  marker remains.
- Historical facts remain identifiable as historical, while incomplete native
  evidence remains `BLOCKED` rather than a passing skip.
- Root can review the changed document without guessing which claims are new.
