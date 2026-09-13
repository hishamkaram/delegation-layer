# Delegation layer

Design notes for a durable multi-provider agent delegation layer: a frontier model leads, cheaper
agents from any provider CLI do the donkey work, and **a worker's turn survives its launcher.**

**Status: design, not built.** No code exists yet.

- [`DESIGN.md`](DESIGN.md) — the whole thing. Verified facts per provider CLI, the design, decisions
  taken, invariants carried over, open questions, milestone plan.
- `diagrams/components.html` — system components. Open the file in a browser.
- `diagrams/lead-interface.html` — how the lead AI talks to the layer.

Both diagrams are self-contained HTML, generated from the `.json` specs beside them. The
`visual-check.*` files are the browser-evidence receipts for those renders.

## The short version

Durability for LLM work is **resumption-by-reference + at-most-once launch + idempotent
collection**, not replay — an agent turn is non-deterministic, side-effecting and costly to repeat,
so a durable-execution engine's core primitive is unavailable to us.

From that: a worker's finished answer is a **file**, and the file's existence is the completion
signal. `pueue` is the supervisor, so we never write process supervision of our own. The lead talks
to the layer through five shell commands. Per-task config is provider-agnostic **intent** — model,
permission, effort — where an unsupported or unverified key fails the dispatch rather than being
silently dropped.

## What has actually been tested

Marked as such throughout. Live on macOS 26.6.2 / arm64:

- **pueue 4.0.4 vs task-spooler 1.0.4** — kill reach, JSON status, output retrieval
- **the macOS process-tree ceiling** — a `setsid`-escaped descendant survives every supervisor
  tested; this is an OS limit, not a tool defect
- **`agy` 1.2.1** — the `--print-timeout` failure shape (exit 0 + `SUCCESS` + empty response), the
  result JSON, transcript storage
- **`agy` 1.2.2 `--sandbox`** — a real filesystem sandbox, `workspace-write` rather than read-only,
  established with a control run

Where a doc and a test disagreed, the test won and the document says so.

## Two decisions still open

Both block milestone 1, and both are in *Open questions*: whether a Task is one turn or a
conversation, and what `permission: read-only` means on the first adapter.
