---
name: docs-updater
description: Keep user and contributor guidance aligned with implemented behavior and actual validation.
---

# Documentation updater

Use when behavior, commands, contributor rules or diagrams need reconciliation.
Read the authoritative source and only the relevant [review guidance](../../agents/_data/maintainability-contract.md#review-dimensions)
or product/provider rule. The project
[documentation completion rule](../../AGENTS.md#documentation-completion) owns
when this pass is required and who verifies it. Documentation work is not
blocked by an unrelated compiler setup failure.

## Work

1. Assess affected README, user/contributor guides, examples, rules, skills,
   packaged references and diagrams against the integrated change. Check
   commands/defaults, capabilities, owners, failure/recovery behavior and stated
   limits against their authoritative implementation and existing evidence.
   Record specific source-backed reasons when guidance or diagrams need no
   change; do not edit correct content merely to show activity.
2. Verify claims against current code, actual commands and observations. Treat
   dated notes as pointers and distinguish current behavior from future work.
   Reconcile executable-copy/fingerprint guidance with the
   [installed-executable rule](../../agents/_data/product-boundary.md#installed-executables),
   including examples/evals and packaged mirrors. Label unimplemented migrations
   explicitly; preserve historical evidence and permitted data-integrity checks.
3. Edit the smallest relevant sections. Keep one owner per rule; link to it
   rather than copy policy, symbol inventories or a mandatory workflow template.
4. Preserve useful public CLI guidance and product receipt semantics. Remove
   retired contributor proof/ledger commands and inaccurate install/runtime
   requirements. Keep local plans/private diagnostics out of product docs.
5. Reconcile examples, links and Make targets. For agent integration, preserve
   the canonical response-handling reference and synchronized plugin copy;
   do not introduce a second provider decision list.
6. For affected diagrams, update the existing Archify source with
   implementation-backed nodes, relationships and source references; regenerate
   the HTML and run repository-backed artifact/browser validation. Publish
   full-diagram SVG/PNG exports, not cropped viewport screenshots. Check both
   themes in the rendered page and exported assets for readable labels, every
   authored node/relationship and clipping. Update diagram checksums last.
   Automated checks and byte checksums do not replace visual inspection.
   Report unavailable rendering/export/inspection as unverified, not passed.

## Result and checks

Return changed guidance, no-change reasons, source/command support, actual
visual inspection and unresolved claims for Root's PR checklist. Reassess after
corrections that change documented behavior. Run
`make check-docs` for the final documentation pass; it includes
`make verify-skills` for skill/link changes and the existing documentation checks.
Diagram changes use `make verify-diagram-checksums`. Reuse unchanged results;
do not run runtime/provider matrices merely for contributor prose. Root owns
integration, Git and the final independent review.
