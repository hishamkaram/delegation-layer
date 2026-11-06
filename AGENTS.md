# Working on Delegation Layer

## Product direction

Read the [product boundary](agents/_data/product-boundary.md) before proposing
a design, guarantee or restriction. Delegate owns task lifecycle and evidence;
users and providers own their CLI installations, settings and accounts.

## Shared engineering rules

1. Do the smallest correct change. In the existing plan, name the requested
   behavior, affected area, non-goals and done condition. No mandatory template,
   tracking file or extra approval process.
2. Before changing shared state or an interface, name its authoritative owner,
   writers, readers and invariant. Keep one owner per state; derive other views.
3. Give modules cohesive responsibilities. Separate data, pure decisions and
   I/O; depend inward through narrow consumer-owned interfaces. Reuse the
   existing policy, resolver, transaction and finalizer instead of copying them.
4. Keep explicit control flow, typed boundaries, immutable defaults and useful
   errors. Preserve supported behavior and persisted data. A private signature
   or test name is not a compatibility promise merely because it once existed.
5. Verify source and required native/provider facts before choosing a design.
   An unavailable tool blocks only dependent work. Do not repair global
   configuration or substitute a provider to obtain a pass.
6. Construct, parse and compare values through domain APIs using the contract's
   semantics: value, structure or identity. Check required representation and
   input/safety constraints separately; equivalence alone does not prove validity.
   Derive expected results independently from verified requirements and fixture
   facts, not incidental output from one host. Use native path/temp APIs and
   isolate OS primitives. A cross-build or another kernel does not prove native
   behavior. Windows is a future, unverified host.
7. Tests, fixtures, installers and contributor tools are maintained code. Keep
   architectural boundaries and meaningful checks; never weaken assertions,
   suppress a finding or relabel handwritten code to obtain a pass.
8. Use focused checks while editing and applicable final checks on stable source.
   Do not repeat a suite through an aggregate unchanged. Rerun for changed inputs
   or a concrete concern. Independently review the integrated result and valid
   corrections; do not add the same review pair to every worker as well.
9. Include necessary callers/tests/references; report unrelated findings
   separately. If scope must expand, explain the dependency and smallest
   alternative, ask for that decision, and continue independent work.
10. Report actual changes, commands/results and limitations in the conversation.
    Blocked or unverified checks are not passed. Keep credentials, inherited
    environments and unrestricted private provider output out of diagnostics.

## Coordination

- Root owns integration and Git operations: commits, pushes, PRs, CI observation,
  merges and releases. Other agents leave changes uncommitted unless explicitly
  assigned otherwise.
- Give each worker an exact base, owned files and prerequisites. One writer owns
  overlapping files. Apply and verify a prerequisite before dependent edits;
  independent work proceeds in parallel.
- Preserve others' work and unique local data. Use supported resource commands
  for bounded cleanup; add tooling only for an observed correctness problem.
- Use the user's selected models, effort and permissions. Do not pin the
  repository process to a particular model or silently substitute one.
- Independent reviewers did not implement the reviewed change. A stopped or
  inconclusive check is reported honestly, not converted into approval.

## Test and experiment cleanup

The creator owns cleanup of test-only files, binaries, schedules, admitted
tasks, processes and managed workspaces. Arrange cleanup before creating a
resource and cover success, failure and interruption. Prevent further launches,
close active work through its supported lifecycle owner, and verify closure
before removing owned paths. Preserve user data, shared state and intended
deliverables; retained history in a shared store is not active work and is not
purged. If cleanup or closure is unresolved, retain necessary evidence, report
the exact remaining resource and do not claim cleanup or test completion.

## Code discovery

Prefer codebase-memory MCP search, trace and snippets. Confirm project/index
freshness; use bounded queries and relevant pagination. Check coverage when its
tool is available. Read exact source for stale, missing or partial results.
An empty search is not proof of absence. Use source search when graph tools
cannot answer. Discovery describes relationships, not allowed dependencies.

Use Context7/current primary documentation for uncertain library or CLI APIs.
Never send secrets or private source to external searches.

## Task-relevant guidance

Load only the owner of the rule needed for the change:

| Question | Authority |
| --- | --- |
| Architecture, dependencies and maintainability | [Maintainability](agents/_data/maintainability-contract.md#architecture-guidance) |
| Task authority, durable writes, lifecycle/signals | [Delegation invariants](agents/_data/delegation-invariants.md) |
| Provider preparation and interpretation | [Adapter contract](agents/_data/adapter-contract.md) |
| Toolchain, lint and test floor | [Code quality](agents/_data/code-quality-floor.md) |
| Current implementation and commands | [Architecture](docs/architecture.md), [contributing](docs/contributing.md) |

Use the matching repository skill: delegation-architect, supervisor-architect,
go-implementer, go-reviewer, go-test-writer, go-concurrency, live-e2e, docs-updater
or add-an-adapter. Read its SKILL.md only when applicable. Agent-integration owns
external delegation guidance. Antigravity is for its integration acceptance,
not implementation work.

Run `make verify-skills` after skill/link changes. Local plans and historical
diagnostics belong under ignored `.local/archive/`, not product documentation.
