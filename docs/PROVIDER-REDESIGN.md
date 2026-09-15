# Provider maintainability and integration workplan

Authorized 2026-09-14. This is the normative provider-redesign amendment linked by EXECUTION-PLAN.md. Phase 3 remains accepted at c094d03; this work adds prerequisites to Phase 4 and completes Phases 4–5. Release, consumer integration and workflows remain later phases. Requirements below are planned until their acceptance is recorded in IMPLEMENTATION-PLAN.md.

## Scope and architecture

Use a compiled catalog with narrow consumer-owned interfaces. An adapter supplies description (ID, supported request options, certified profiles), preparation (existing execution plan, effective policy, observed version, writable roots, identity-observer factory), and all immutable interpreter revisions. Move shared preparation types below app to avoid imports from providers into orchestration. Reuse existing task/execution/predicate value types; do not introduce parallel result, usage or lifecycle models.

Construct one explicit catalog at startup, without init registration or mutable globals. Derive launch selection, capability validation and interpreter lookup from it. Duplicate provider IDs or exact predicate references fail construction. Structural validation stays in task/config; provider capability validation occurs before admission. Preserve existing normalization and persisted requests, including agy native-timeout >0 and <= outer budget; other providers do not advertise this option. Explicit unsupported model/effort requests must not be silently dropped.

Providers own native argument construction, configuration precedence, identity semantics and output interpretation. The core retains admission, claims, scheduling, process execution, deadlines, pipes, capture, sealing and publication. Preparation does not launch; interpretation receives only sealed evidence and a result writer. Preserve preflight immediately before Start, at-most-once launch and observational collection. Historical predicates remain registered independently of whether a current executable is available; semantics changes require a new revision.

Add `delegate providers --json`: schema_version 1, providers sorted by ID, supported request options and certified profile/version/platform metadata. It describes compiled support, makes no host-ready claim and performs no provider/auth probe. Preserve existing provider IDs and native login flows. No arbitrary executable loading, dynamic plugins, universal DSL, ACP migration, SDK replacement, event bus, parser rewrite, permission expansion, automatic fallback or paid retry.

## Delivery handoffs

Each row is one implementation PR with root review, direct Codex CLI review, exact-head green CI, squash merge and green main CI before the next row begins.

| Handoff | Deliverable | Exit evidence |
|---|---|---|
| Engineering guidance | Shared maintainability contract; eight focused skills; repaired add-an-adapter; verify-skills and tests; feature matrix and handoff templates | Skill/tool tests, make check, zero unresolved actionable review findings; no native rerun for docs/tool-only changes |
| Catalog and agy proof | Catalog, common preparation contract, agy wrapper, capability lookup, providers command | Equivalent launch/policy/request behavior, historical predicate fixtures, protocol/supervisor suites and all three agy native cases |
| Contributor proof | Extract demonstrated shared mechanics and acceptance orchestration; synthetic provider using the normal catalog in acceptance binaries; guide exercise | Provider module + fixtures + one registration only; no app/task/storage/execution/supervisor/publication edits; real isolated pueue success/failure/identity/replay; concrete Codex/Claude cases fit |
| Codex / Phase 4 | Read-only native module, versioned JSONL interpretation and policy, exact resume, acceptance-codex | Existing Phase 4 fixtures and two bounded native turns |
| Claude / Phase 5 | Restricted read/search native module, managed policy checks, final-result interpretation, exact resume, acceptance-claude | Existing Phase 5 fixtures and two bounded native turns |
| Integrated verification | Sequential acceptance-providers target; capability/docs reconciliation; contributor and replay verification | Full seven-turn native suite, all matrix rows resolved, maintainability review, CI and merge receipt |

The [contributor acceptance checklist](PROVIDER-CONTRIBUTION-ACCEPTANCE.md) records the required boundary, compatibility and executable proof. The contributor proof blocks further integrations if it needs provider-specific core exceptions. Fix the boundary and repeat the proof, rather than adding a workaround. Shared extraction must be supported by actual reuse; retain provider source-discovery/precedence and success semantics locally. Do not copy lifecycle/certification/harness logic. The synthetic provider is test-only, never part of production discovery. The implementer must use add-an-adapter and fix missing instructions exposed by the exercise. Map actual Codex and Claude cases onto the contract before accepting it.

## Contributor-proof artifact boundary

The contributor handoff includes the shared extra-output capability needed by
the existing Codex recipe. A typed output declaration identifies a safe logical
name and a reserved argv index; it is not a path/template language. Bind these
declarations and the certified writer-completion contract into immutable task
metadata. Bounded non-secret input-file declarations similarly bind exact content
and reserved argv slots; the core creates their regular configuration files.
The core supplies task-local staging paths and owns import, sync,
close, raw-manifest validation, sealing and named-evidence reads. Preserve old
metadata and two-stream manifests byte-for-byte when no artifact is declared.

Use separate create-once native staging and final raw evidence. A present empty
or conflicting file remains evidence; an absent optional file is distinguishable
from a read or validation fault. Replays read only the sealed manifest and never
need the current executable, staging file, or launch profile. Additional artifact
writers require version-scoped certification of completion at process exit/EOF;
file existence or a stable snapshot alone cannot prove an arbitrary writer ended.

Freeze this generic baseline before adding a new synthetic provider. Its module,
fixtures and single test-only registration must then exercise present output,
absence/stdout fallback, conflict, identity, continuation and replay without
provider-specific core edits. The existing phase2 fixture alone does not count
as a new contributor exercise. Real Codex/Claude integrations remain subsequent
handoffs; mapping their concrete cases here does not claim native certification.

## Engineering guidance

Create skills delegation-architect, go-implementer, go-reviewer, go-test-writer, live-e2e, go-concurrency, docs-updater and repair add-an-adapter. Each has a scoped trigger, applicable contract links, inputs, workflow, outputs and exit conditions. Shared rules live once under agents/_data; do not duplicate them or import router/SQLite-specific policy. AGENTS.md routes contributors to the relevant skill. No global skill installation/configuration is needed: agents read the repository files explicitly.

The maintainability review covers ownership and resource lifetime, dependencies, real interface boundaries, duplication, failure diagnostics, backward compatibility and contributor effort. Existing deterministic quality thresholds remain mandatory. Do not add arbitrary line-count/coverage scores. Skills must not transfer pipe/seal ownership to adapters, reject the verified agy containment-denial case solely because it lacks a successful first answer, or equate rejection with absence of a rejection payload.

`make verify-skills` (introduced in the engineering-guidance handoff) checks required skills, constrained valid metadata, local links, unfinished scaffolding and commands against actual targets, with tests for failures and legitimate inputs. Planned commands must be explicitly distinguished. Wire it into sequential make check. Keep the checker small; it is not a general Markdown/YAML framework.

## Feature-to-test matrix

The rows name required scenarios, not claims that tests have already passed. Each implementation receipt must map these IDs to actual test names/commands and evidence. Native behavior requires real-provider evidence; deterministic fault injection uses executable fixtures rather than forcing unpredictable paid failures.

| ID | Feature | Unit/contract evidence | Executable/live evidence | Initial status |
|---|---|---|---|---|
| guidance | Skill metadata/links/commands and usability | verify-skills negative/positive suite | Contributor follows add-an-adapter during synthetic proof | Planned |
| catalog | Registration, discovery, unsupported options | Duplicate IDs/references; deterministic JSON; errors/writer failures; no admission | Shipped providers command and rejected dispatch, zero provider launches | Planned |
| preparation | Argv, stdin, policy, identity and drift | Fresh/resume argv; finite input; source conflicts; binary/config drift | Native probes through dispatcher/runner; controlled prestart drift fixture | Planned |
| agy | Existing write profile, denial, timeout and resume | Existing versioned envelope/policy/usage regressions unchanged | Existing three-turn acceptance-agy, real workspace positive control and outside denial | Reverification required after refactor |
| codex | Read-only, completed turn, exact resume | Commentary/final/retry/failure/JSONL EOF/output-file conflict fixtures | Two-turn acceptance-codex; reads work, writes denied, exact thread | Accepted in PR #8 for pinned 0.154.0 profile 2; main CI passed |
| claude | Restricted reads, managed policy, final result, resume | Result/error/truncation/init tools/session/accounting fixtures | Two-turn acceptance-claude; reads/search work, forbidden effects absent | In progress; [candidate and policy evidence](CLAUDE-ACCEPTANCE.md), certification Pending |
| evidence | Malformed, conflicting or missing completion | Parser bounds, duplicate critical fields, exit conflicts, blank answer, writer failure | Controlled fake provider errors through real runner; raw sealed before collection | Existing core gates plus adapter extensions |
| lifecycle | Admission, stop/deadline, unknown termination | Permit/claim/race/unknown-state regressions | Protocol and supervisor acceptance; no duplicate launch or fabricated terminal state | Existing gates retained |
| replay | Historical identity/outcomes and zero paid collection | Old predicates; current binary unavailable; nullable usage; cumulative scope | Repeated collect on every native task; unchanged predecessor outcome | Required for each adapter |
| contribution | Add provider without core exceptions | Test-only provider contract fixtures | Compiled CLI + isolated real pueue; success, rejection, continuation and replay | Planned |
| integrated | All three use common interface | All tests and contributor regression | acceptance-providers: three agy + two Codex + two Claude turns | Planned |

## Verification and evidence

Runtime handoffs run make check, make acceptance-protocol and make acceptance-supervisor; guidance-only handoff runs make check and its dedicated tooling tests. Run affected native gates after behavior stabilizes. Final integration runs acceptance-providers sequentially, stopping on failure. Existing named provider specifications remain authoritative for exact flags/envelopes/configuration and positive/negative controls.

Use 120s task budgets, 150s acceptance watch and agy's prescribed short native timeout. Missing binary/auth/config/certification, inconclusive positive controls, absent receipts or an unknown task at watch expiry are blocked/failed acceptance, never a passing skip. Keep exact task handles; no automatic paid retries, direct signals or global configuration changes. Inspect filesystem/tool evidence rather than trusting prose claims of success or denial.

Record commit, built binary digests, provider version/platform/profile, commands, task/session identities, expected/actual observations and artifact digests. Keep raw private logs outside git, sanitized fixtures/receipts only. Launch/policy/interpretation/certification/assertion changes invalidate affected evidence; documentation-only changes do not force paid reruns. Use purpose-based names in commit-identified artifact directories, never review-round filenames.

## Handoff and review templates

Implementation handoff fields: base commit; bounded objective; owned files/responsibilities; applicable skills/contracts; fixed interfaces; acceptance commands/scenarios; deferred scope. Root Codex implements directly or delegates work and fixes to Luna/max agents with bounded ownership and no git/remote mutation. agy is used only for provider integration and live acceptance tests, never implementation or review fixes. Root owns docs/integration/review/commits/PR/CI/merge; one active implementation worktree.

Review record fields: reviewed head and base; changed surface and consumers inspected; maintainability assessment (ownership/dependencies/abstractions/duplication/compatibility/contributor impact); findings with severity, trigger, file/line, resolution and regression evidence; executed commands/results; native status; unresolved items. Direct Codex review CLI only, never Duo. Incomplete or failed review output is not approval. Disputed findings require code evidence, not relabeling. Fix all actionable findings and rerun affected checks/review on the new head.

Merge receipt fields: PR, exact reviewed head, required CI results on that head, evidence manifest, squash commit, main CI result, retired worktree/evidence location. Never merge pending/stale green CI. Scope remains incomplete until all required evidence and zero unresolved actionable findings are recorded. Do not publish a release or begin workflows as part of this amendment.
