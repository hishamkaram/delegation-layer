<!-- codebase-memory-mcp:start -->
# Codebase Memory

## Codebase Knowledge Graph (codebase-memory-mcp)

This project uses codebase-memory-mcp to maintain a knowledge graph of the codebase.
ALWAYS prefer MCP graph tools over grep/glob/file-search for code discovery.

### Priority Order
1. `search_graph` — find functions, classes, routes, variables by pattern
2. `trace_path` — trace who calls a function or what it calls
3. `get_code_snippet` — read specific function/class source code
4. `check_index_coverage` — validate candidate paths and missed ranges before claims
5. `query_graph` — run Cypher queries for complex patterns
6. `get_architecture` — high-level project summary

### Evidence tiers
- **Scout (Tier 1):** quick positive lookup with few calls and targeted source checks. Mark it provisional; do not make negative or exhaustive claims.
- **Verify (Tier 2, default):** task-directed graph evidence, relevant trace directions, exact snippets for material claims, and relevant pagination.
- **Auditor (Tier 3):** bounded-scope full verification with current generation, complete relevant pagination, both call directions and broader relationships when material, and every limitation disclosed.
- After candidate paths are known in any tier, call `check_index_coverage` once with every evidence path. Add relevant scopes for negative or exhaustive claims. A clean result means no recorded gap, not proof of completeness. For partial, skipped, excluded, stale, pending, or unknown coverage, read/grep the reported ranges or scope before relying on graph results.

### When to fall back to grep/glob
- Searching for string literals, error messages, config values
- Searching non-code files (Dockerfiles, shell scripts, configs)
- When MCP tools return insufficient results

### Examples
- Find a handler: `search_graph(name_pattern=".*OrderHandler.*")`
- Who calls it: `trace_path(function_name="OrderHandler", direction="inbound")`
- Read source: `get_code_snippet(qualified_name="pkg/orders.OrderHandler")`

### Session resets and subagents
- At session start or after compaction, confirm the nearest graph project and generation with `list_projects` or `index_status`, then choose Scout, Verify, or Auditor.
- Before spawning a subagent, query the graph and coverage in the parent. Pass the tier, project, generation/freshness, bounded scope, queries and pagination state, qualified symbols, paths, call-chain findings, coverage evidence with ranges/reasons, source fallback already performed, and unresolved questions in the delegated task context.
- Do not assume subagents inherit MCP access or the parent conversation. If a child lacks MCP tools, it must not call or claim MCP access. It should use the supplied evidence and read/grep exact source, especially every reported missed-coverage range.
<!-- codebase-memory-mcp:end -->

# Agent Roles and Responsibilities

## Golden engineering rules

Maintainability and clear state ownership are acceptance conditions for every repository
change. Delivery speed does not waive them. The shared
[maintainability contract](agents/_data/maintainability-contract.md) owns the
detailed rules; the [subsystem registry](agents/_data/subsystems.json) owns
machine-checked boundaries and budgets.

Do the simplest work that reaches the requested outcome. For a routine
operation that only changes local state, such as removing worktrees, branches,
or workspaces, inspect the requested scope and a bounded inventory of where the
project creates those resources; ask only when a material destructive ambiguity
remains or unique work lacks deletion authorization. Use each resource’s
supported command. Preserve unique work and data, including ignored files, that
Git, a remote, or a rebuild cannot verifiably restore. One controller owns each
overlapping resource set and may parallelize independent targets; to replace
it, stop it and have the successor observe its accepted pending operations
without reissuing them. Add tooling or evidence only for an observed
correctness or safety blocker or a missing operation. Claim only the end state
you verified. Repository changes, including maintained tests, fixtures,
tooling, CI, and behavior-bearing rules or docs, keep every applicable gate.

1. Name the authoritative owner, writers, readers, and invariant before changing
   state, a journal, a lease, a cache, or an interface.
2. Give each subsystem one cohesive responsibility and an isolated package
   boundary. Keep data declarations, pure decisions, and I/O separate. A small
   pure domain may keep its values and validity rules in separate files of one
   package; do not create empty layers or generic dumping grounds.
3. Depend inward through narrow consumer-owned ports. Runtime implementations
   stay sealed; other subsystems consume their public boundary or pure contracts.
4. Reuse the canonical policy, resolver, transaction, and finalizer. Do not copy
   a decision for another command, provider, record family, or test harness.
   Relocated tests call their owning subsystem directly; do not retain a private
   forwarding API solely to keep those tests at their old call sites.
   Before tightening a shared gate, trace its indirect public-command consumers
   and pair permitted observations with refused mutations in the same state.
   Preserve each consumer's error identities, exit codes and JSON guidance;
   helper tests must not replace the public command's existing contract.
   Classify pending or busy from the whole returned error with the owning
   domain's canonical policy, not a marker `errors.Is` alone, which can hide a
   joined concrete fault; the
   [whole-error rule](agents/_data/maintainability-contract.md#whole-error-pending-classification)
   owns the details.
   Do not replace a long function with helpers that pass its entire local state
   through long positional argument lists. Use named call contracts; introduce
   an immutable record only when it represents an actual cohesive domain value.
5. Verify an OS primitive or provider protocol on the real supported OS/provider
   before implementing work that depends on it. Missing evidence blocks that
   work and its dependents; independent proven work may continue. Keep shared
   data and decisions OS-neutral; the
   [portable paths and temporary files](agents/_data/maintainability-contract.md#portable-paths-and-temporary-files)
   rules own native temporary-path, identity, and platform-adapter requirements.
6. Treat copying policy, exposing private mutable state, or changing unrelated
   modules to make an ordinary change as a design finding. Stop the dependent
   change, trace the cause, and resolve ownership before adding another patch.
7. Run all maintainability gates and applicable tests before peer review. Tests,
   fixtures, acceptance tools, installers, and contributor tools are maintained
   code. A passing size check alone does not prove cohesion or correct ownership.
   Keep four levels apart: a worker-local result, a setup failure (a required
   tool is unusable, so dependent checks are blocked, not failed), the integrated
   gate Root runs on the combined snapshot, and independent review. A regression
   test that fails on its intended assertion before the fix is expected red, not
   a setup failure; any other failure is not. The
   [worker check levels](agents/_data/maintainability-contract.md#worker-check-levels)
   own the details; the contract's
   [check and review stages](agents/_data/maintainability-contract.md#check-and-review-stages)
   own which checks apply at each stage.
8. Never weaken assertions, suppress a meaningful check, relabel handwritten
   code as generated, or add an exception to make a change pass. The refactor's
   baseline inventory permits only unchanged, owned findings; completion requires
   an empty inventory and zero size exceptions.
9. Give each worker the exact base revision, owned files, and interface
   prerequisites. One writer owns each module; consumers wait for their
   dependencies. A prerequisite counts once its post-image is applied and
   verified in the worker's checkout; it need not be merged. The worker confirms
   that the tools Root provided run in its own launch context, and needs only
   the tools its assignment uses: documentation and read-only review work is not
   blocked by an unusable Go toolchain. Never repair a global cache, toolchain,
   or configuration to obtain a pass; report the setup failure. Root owns
   integration and Git operations. Independent reviewers verify the final result.
   The [worker check levels](agents/_data/maintainability-contract.md#worker-check-levels)
   also govern delivery sequencing and evidence reuse.
10. Bind claims to source and binary fingerprints, commands, exit codes, native
    receipts, and unresolved facts. Source captured, index complete, receipt
    validated, and review approved are four separate statuses; none implies
    another, and a complete index only declares input completeness. A memory
    note or other dated reference is a pointer: verify it against current source
    before relying on it. The
    [status rules](agents/_data/maintainability-contract.md#evidence-status-is-not-approval)
    own the details. Preserve owned resources until cleanup is
    authorized by lifetime closure; tests clean their own resources automatically.
    Never dump inherited environments or enable diagnostics such as
    `NODE_DEBUG=child_process` that print them. Keep credentials out of transcripts
    and evidence; report only sanitized facts needed for the task.

## Roles and responsibilities
- **Root Codex** orchestrates, may implement directly, integrates, reviews, and owns every Git mutation:
  branch commits, pull requests, waiting for CI, squash merges, and releases. No
  other agent commits, pushes, creates PRs, merges, or releases unless explicitly
  requested.
- **Implementers** are the models the session user selects for bounded work with
  exact file ownership (currently Claude Opus or Sonnet 5.5). Repository policy
  does not pin a model ID for future users. External workers run through Delegate
  with the exact requested model, effort, and permission mode.
- **Independent review** is Astra plus Root on the exact final snapshot.
- **Model effort** follows the user's explicit instruction: Astra runs at high
  effort for architecture and consequential review and at medium for routine
  bounded work; Claude models run at high. Astra never runs at max.
- Workers may read Git state. They never change history or remote repositories,
  and they leave their changes uncommitted for Root to capture.

The installed Antigravity CLI (`agy`) is used only to test its provider integration and live acceptance. Do not use agy to implement code or fix review findings. Native tests retain the required finite inputs, isolated supervision, and recorded evidence.

## Invariant Contracts
All contributors and subagents must strictly adhere to the project contracts in `agents/_data/`:
1. `agents/_data/delegation-invariants.md`: Terminal outcome authority, single-turn tasks, at-most-once launch, create-once staging, and forbidden signals.
2. `agents/_data/adapter-contract.md`: Runtime capability checks, containment verification, input transport, and result predicates.
3. `agents/_data/code-quality-floor.md`: Pinned toolchain, strict linter policy, zero-blank errcheck, forbidigo patterns, and test hygiene.

## Product and Contributor Documents
- `README.md`: user-facing overview, requirements, installation, and safety summary.
- `docs/architecture.md`: current lifecycle, ownership, and recovery boundaries.
- `docs/providers.md`: supported provider profiles and runtime capability checks.
- `docs/contributing.md`: development checks and adapter extension guidance.
- Local planning notes and historical evidence, when needed for maintenance, live
  under the ignored `.local/archive/` directory and are not product documentation.

## Maintainability Guidance

Read the shared [maintainability contract](agents/_data/maintainability-contract.md)
alongside the three invariant contracts before changing provider boundaries. Use
the focused repository skill that matches the work:

- [delegation architect](skills/delegation-architect/SKILL.md) for ownership and
  interface design;
- [supervisor architect](skills/supervisor-architect/SKILL.md) for supervisor
  lifecycle, migration, cleanup ownership, and duplicate-policy prevention;
- [Go implementer](skills/go-implementer/SKILL.md) for bounded runtime changes;
- [Go reviewer](skills/go-reviewer/SKILL.md) for evidence-backed review;
- [Go test writer](skills/go-test-writer/SKILL.md) for behavior and fault tests;
- [live E2E](skills/live-e2e/SKILL.md) for real provider/supervisor acceptance;
- [Go concurrency](skills/go-concurrency/SKILL.md) for lifetimes, races, and
  deadline paths;
- [docs updater](skills/docs-updater/SKILL.md) for product, contributor, and
  receipt changes; and
- [add an adapter](skills/add-an-adapter/SKILL.md) for a new provider or a
  provider-contract repair.

Skills are repository instructions and are read explicitly by the working CLI;
they require no global installation or configuration. Run `make verify-skills`
after changing a skill or its local links. Root's responsibilities are listed
under Roles and responsibilities.

Supervisor lifecycle changes must use the supervisor architect skill and the
shared contract's [durable transition matrix and composed-test
rules](agents/_data/maintainability-contract.md#durable-transition-matrix-and-composed-tests),
[recovery rules](agents/_data/maintainability-contract.md#recovery-and-verification),
the [native backend proof](agents/_data/maintainability-contract.md#native-portability-proof),
and the [live acceptance proof](agents/_data/maintainability-contract.md#live-acceptance-proof).
Every handoff follows the contract's [source, binary, receipt, and unresolved
evidence requirements](agents/_data/maintainability-contract.md#handoff-evidence).

For repository changes involving process identity, signals, sessions/groups, waiting,
supervisor endpoints, descriptors, or cleanup, apply the maintainability
contract's native portability proof. Cross-compilation and upstream
cross-platform support are not native runtime evidence. Root may claim Linux or
macOS behavior only after the affected path passes on that operating system.
