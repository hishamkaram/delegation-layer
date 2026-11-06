---
name: docs-updater
description: Keep user and contributor guidance aligned with implemented behavior and actual validation.
---

# Documentation updater

Use when behavior, commands, contributor rules or diagrams need reconciliation.
Read the authoritative source and only the relevant [review guidance](../../agents/_data/maintainability-contract.md#review-dimensions)
or product/provider rule. Documentation work is not blocked by an unrelated
compiler setup failure.

## Work

1. Verify claims against current code, actual commands and observations. Treat
   dated notes as pointers and distinguish current behavior from future work.
   Reconcile executable-copy/fingerprint guidance with the
   [installed-executable rule](../../agents/_data/product-boundary.md#installed-executables),
   including examples/evals and packaged mirrors. Label unimplemented migrations
   explicitly; preserve historical evidence and permitted data-integrity checks.
2. Edit the smallest relevant sections. Keep one owner per rule; link to it
   rather than copy policy, symbol inventories or a mandatory workflow template.
3. Preserve useful public CLI guidance and product receipt semantics. Remove
   retired contributor proof/ledger commands and inaccurate install/runtime
   requirements. Keep local plans/private diagnostics out of product docs.
4. Reconcile examples, links and Make targets. For agent integration, preserve
   the canonical response-handling reference and synchronized plugin copy;
   do not introduce a second provider decision list.
5. For diagram changes use Archify and implementation-backed content. Publish
   full-diagram SVG/PNG exports, not cropped viewport screenshots. Check both
   themes, every authored node/relationship and clipping; update diagram
   checksums last. A byte checksum alone is not visual validation.

## Result and checks

Return changed guidance, source/command support and unresolved claims. Run
`make verify-skills` for skill/link changes and affected documentation checks.
Diagram changes use `make verify-diagram-checksums`. Reuse unchanged results;
do not run runtime/provider matrices merely for contributor prose. Root owns
integration, Git and the final independent review.
