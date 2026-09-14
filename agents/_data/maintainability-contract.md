# Provider maintainability contract

This is the shared engineering guidance for provider redesign work. The
[delegation invariants](delegation-invariants.md), [adapter contract](adapter-contract.md),
and [code-quality floor](code-quality-floor.md) remain authoritative for their
respective requirements; this document adds the maintainability decisions that
apply across the implementation, review, test, acceptance, and documentation
skills.

## Boundary and ownership

- The core owns task admission, claims, scheduling, process execution,
  deadlines, pipes, output capture, sealing, collection, and publication.
- An adapter owns its immutable identity, supported request options, certified
  profile, native argument/configuration preparation, identity semantics, and
  versioned interpretation of sealed provider evidence. It never owns pipes,
  capture, sealing, processes, stopping, or publication.
- Interfaces belong to the consumer that needs them. Keep each interface
  narrow enough to express the real boundary, and use one explicit catalog at
  startup rather than init registration, mutable global registries, dynamic
  plugins, or a universal provider DSL.
- Reuse existing task, execution, predicate, usage, and lifecycle value types.
  A new abstraction needs a concrete consumer and a lower total maintenance
  cost than the code it replaces. Provider-specific behavior must not leak into
  task, execution, or publication code.

## Review dimensions

For every substantial change or review, record evidence about:

1. ownership and resource lifetimes, including cleanup and failure paths;
2. dependency direction and whether a new dependency is necessary;
3. the interface owner and whether the boundary is real and narrow;
4. duplicated lifecycle, certification, parser, or acceptance-harness logic;
5. error context, compatibility behavior, and accidental secret exposure; and
6. contributor effort, including whether the documented path is discoverable
   and repeatable.

Do not add arbitrary file-size or coverage thresholds. Existing formatting,
lint, race, build, and vulnerability thresholds remain the project floor.

## Compatibility and evidence

Preserve existing provider IDs, persisted request normalization, native login
flows, historical predicate revisions, and nullable usage scope. Explicitly
unsupported options fail before admission; they are never silently discarded.
Collection is observational and must not launch, retry, or resume paid work.
Missing prerequisites or inconclusive native evidence are `BLOCKED`, never a
passing skip. A rejected task may have a rejection payload; rejection is
established by the authoritative outcome record and its evidence, not by an
assumption that the payload is empty.

Use meaningful tests for observable behavior, error branches, writer failures,
boundaries, and compatibility. Do not copy lifecycle or certification tests
into every provider. Keep provider interpretation pure over sealed evidence and
keep acceptance orchestration in the shipped dispatcher/runner and shared
harness.

Root owns planning documents, integration, direct Codex review, commits,
pull requests, CI, and merges. Contributors may prepare bounded changes and
receipts, but must not transfer those responsibilities or install global
skills/configuration. A review record is incomplete when its output is
inconclusive or when actionable findings remain unresolved.

## Repository skill format

The repository skills under `skills/` intentionally use a small, documented
metadata format so the verifier is deterministic without becoming a YAML
parser. Each `skills/<name>/SKILL.md` starts with exactly this frontmatter:

```text
---
name: lower-case-hyphenated-name
description: one non-empty single-line description
---
```

Only `name` and `description` are allowed, each exactly once. The name must
match its directory and use lowercase letters, digits, and single hyphens
between words and be at most 64 characters. Unquoted metadata values are plain strings that begin with an
ASCII letter and do not use YAML-special scalar forms; quote values that begin
with punctuation or digits or contain `: `. Each skill body should make its trigger, inputs, workflow,
deliverables, and exit criteria easy to find; the verifier deliberately does
not require particular Markdown headings.

Repository links in a skill use relative Markdown paths in an inline
parenthesized destination. Parentheses in a destination are balanced, and an
angle-bracket destination may contain spaces; a fragment after `#` is allowed
and ignored for file existence checks. Reference-style links/definitions and
other unsupported Markdown shapes are rejected explicitly. External URLs may
be used when genuinely useful, but they are not a substitute for links to the
applicable repository contracts.

The verifier recognizes a one-target inline `make <target>` command when it is
written as inline code or as a command-shaped line. The target must exist in
the Makefile. Options, variable assignments, extra targets, shell operators,
and other trailing command text are unsupported and fail explicitly. A command
that is intentionally future work must carry the explicit marker `[planned]`
or `(planned)` on the same line; that marker is accepted only when the line has
one unambiguous command. Unfinished scaffolding uses explicit uppercase tokens
such as `TODO`, `FIXME`, `TBD`, `PLACEHOLDER`, `<YOUR_NAME>`, or
`{{REPLACE_ME}}`; ordinary prose such as “todo list” is not a marker.
