# Provider contribution acceptance

Status: planned. No contributor-proof acceptance is claimed yet. This receipt
will record evidence for the contributor handoff in
[the provider redesign](PROVIDER-REDESIGN.md). The catalog prerequisite was
accepted through [PR 6](https://github.com/hishamkaram/delegation-layer/pull/6),
squash `38e3da4cbf4c57eb9e0a0dfc13c70e2990a549a3`.

## Boundary freeze and contributor exercise

First implement and review the generic boundary, then record its commit before
adding the synthetic provider. Record the exercise's exact changed paths and
show that its implementation consists only of a new provider module, fixtures,
and one test-only catalog registration. Test harness preparation belongs in the
baseline. Production discovery must not include the synthetic provider. Reusing
the existing phase2 fixture alone is insufficient evidence of contributor use.

The exercise must follow [add-an-adapter](../skills/add-an-adapter/SKILL.md).
Record any guidance correction it exposes. Shared extraction needs concrete
reuse; provider semantics remain local and the core owns all lifecycle actions.

## Required evidence

These are acceptance requirements, not executed results. Fill in exact test
names, commands, reviewed commits and sanitized receipt identifiers only after
they run.

| Scenario | Expected evidence |
|---|---|
| Declaration validation | Unsafe, duplicate, reserved names and invalid or overlapping argv bindings fail before provider launch; declarations and completion contract bind to immutable metadata |
| Legacy compatibility | Tasks without artifacts retain canonical metadata and two-stream manifest bytes; historical predicates and collection still work |
| Staging ownership | Core supplies create-once paths outside workspace; native files are copied into separate raw evidence inodes before sealing |
| Writer completion | Process exit and stream EOF gate import under the version-qualified writer contract; an inherited open stream prevents early sealing |
| File faults | Symlink, nonregular file, replacement/mutation and import write/sync/close failures cannot produce an accepted seal |
| Optional absence | Missing optional staged output is absent evidence; a missing or corrupt manifest-listed file is an evidence fault |
| Present and conflicting output | Present empty output remains present; provider interpreter rejects conflict with the final stdout answer |
| Success and rejection | New synthetic envelope passes and fails through the compiled dispatcher/runner with isolated real pueue |
| Identity and continuation | Exact recorded session resumes under a new task; mismatch rejects; original records remain immutable |
| Replay | Repeated collection uses sealed evidence with staging removed and executable unavailable; no additional launch occurs |
| Profile and binary identity | Test certification and receipts use the actual built executable digest and executed platform; no invented execution evidence |
| Existing lifecycle | Protocol/supervisor gates preserve admission, at-most-once start, deadlines, stop authority and crash recovery |
| Existing native provider | All three affected agy acceptance turns pass with filesystem, identity and replay evidence |

## Concrete native-provider fit

The requirements below come from the provider specifications in
[EXECUTION-PLAN.md](EXECUTION-PLAN.md). This mapping is not native certification;
Codex and Claude retain their subsequent implementation and live-test gates.

| Requirement | Intended ownership and proof |
|---|---|
| Codex finite prompt and fresh/resume argv | Provider prepares argv, core supplies finite stdin; fixtures prove resume option placement |
| Codex optional last-message output | Typed artifact declaration binds a reserved argv slot; core stages/imports/seals, interpreter checks agreement with completed-turn JSONL |
| Codex commentary/retry/terminal semantics | Versioned provider interpreter selects the final completed-turn answer and rejects ambiguous completion; no core event parser |
| Codex thread and usage | Provider maps verified identity and task-scoped usage into existing shared value types |
| Claude regular settings and empty MCP files | Bounded non-secret input declarations bind exact bytes and reserved argv slots into immutable metadata; core creates regular task-owned files, tested before freezing the baseline |
| Claude managed configuration and tool restrictions | Provider preparation validates effective policy; versioned interpreter checks reported initialization evidence |
| Claude trailing messages and final result | Core waits for stream closure; provider interpreter validates the final result, session and truncation/error semantics |
| Claude session and accounting | Provider maps exact session identity and distinct accounting scopes into shared value types |

## Acceptance gates and merge receipt

Required gates are `make check`, `make acceptance-protocol`,
`make acceptance-supervisor`, the synthetic compiled-CLI exercise, and
`make acceptance-agy`. Record the synthetic command once implemented. A missing
prerequisite, failed assertion or inconclusive native result is blocked/failed,
never a passing skip. Keep raw private artifacts outside git.

Acceptance requires root review and completed direct Codex CLI review with zero
unresolved actionable findings, including ownership, lifetimes, dependencies,
duplication, compatibility and contributor effort. Record the PR's exact reviewed
head, green CI on that head, squash commit, green main CI, and worktree retirement
receipt before marking this handoff accepted.
