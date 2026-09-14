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

## Orchestrator and Reviewer: Root Codex
Root Codex owns task orchestration, code review, branch commits, PR lifecycle, waiting for CI, and squash merges. No other agent may commit, push, create PRs, or squash merge unless explicitly requested.

## Implementation and testing
Root Codex implements directly or delegates bounded implementation and review fixes to agents using `gpt-5.6-luna` with `max` reasoning effort. Delegated agents have explicit file ownership and must not modify git history or remote repositories.

The installed Antigravity CLI (`agy`) is used only to test its provider integration and live acceptance. Do not use agy to implement code or fix review findings. Native tests retain the required finite inputs, isolated supervision, and recorded evidence.

## Invariant Contracts
All contributors and subagents must strictly adhere to the project contracts in `agents/_data/`:
1. `agents/_data/delegation-invariants.md`: Terminal outcome authority, single-turn tasks, at-most-once launch, create-once staging, and forbidden signals.
2. `agents/_data/adapter-contract.md`: Certified launch profiles, containment verification, input transport, and result predicates.
3. `agents/_data/code-quality-floor.md`: Pinned toolchain, strict linter policy, zero-blank errcheck, forbidigo patterns, and test hygiene.

## Planning and Normative Documents
- `docs/EXECUTION-PLAN.md`: Complete, authoritative normative execution plan for all phases.
- `docs/IMPLEMENTATION-PLAN.md`: Phase-by-phase implementation progress and tracking.
- `DESIGN.md`: Architecture design, preserved historical measurements, and provider facts.

## Maintainability Guidance

Read the shared [maintainability contract](agents/_data/maintainability-contract.md)
alongside the three invariant contracts before changing provider boundaries. Use
the focused repository skill that matches the work:

- [delegation architect](skills/delegation-architect/SKILL.md) for ownership and
  interface design;
- [Go implementer](skills/go-implementer/SKILL.md) for bounded runtime changes;
- [Go reviewer](skills/go-reviewer/SKILL.md) for evidence-backed review;
- [Go test writer](skills/go-test-writer/SKILL.md) for behavior and fault tests;
- [live E2E](skills/live-e2e/SKILL.md) for real provider/supervisor acceptance;
- [Go concurrency](skills/go-concurrency/SKILL.md) for lifetimes, races, and
  deadline paths;
- [docs updater](skills/docs-updater/SKILL.md) for plan, matrix, and receipt
  changes; and
- [add an adapter](skills/add-an-adapter/SKILL.md) for a new provider or a
  provider-contract repair.

Skills are repository instructions and are read explicitly by the working CLI;
they require no global installation or configuration. Run `make verify-skills`
after changing a skill or its local links. Root retains ownership of planning
documents, integration, review, commits, PRs, CI, and merges.
