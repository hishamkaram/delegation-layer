# Delegation layer — complete execution plan

Status: execution authorized 2026-09-13; phase completion is evidence-driven. This document is the normative phase handoff and supersedes conflicting milestones in DESIGN.md and docs/IMPLEMENTATION-PLAN.md. Historical measurements remain historical; tests and live evidence are never claimed before they run.

## Goal and confirmed decisions

Deliver the durable single-machine delegation layer, with antigravity:print, codex:exec, and claude:print all in the first private release. Then port the existing duo review fan-out and deliver a resumable declared-plan workflow that returns task outcomes to its calling AI lead. The workflow never invents tasks or autonomously judges result quality. Keep the repository, release assets, and any tap private. No HTTP/A2A, native Claude background mode, router adapters, or unbounded autonomous lead in this sequence.

The root Codex agent leads implementation, orchestration and review, following the user’s explicit takeover instruction. Bounded agents may implement, investigate and test assigned areas with explicit file ownership; root integrates their work and remains responsible for every acceptance gate. The user's authorization includes branch/worktree creation, normal commits/pushes, PR creation, waiting for CI, and squash merging only after acceptance and review pass.

## Delivery cycle for every phase

1. Start from the latest merged main in an isolated codex/phase-N-* worktree. Record base SHA, task scope, acceptance commands, provider/tool versions and artifacts. Preserve unrelated edits and in-flight work.
2. Assign implementation work with a finite brief containing the applicable specifications, verified findings, file ownership, interface contracts and acceptance criteria. Coordinate shared interfaces before concurrent edits. Root may implement directly or use bounded agents, and must retain substantive handoffs and actual test results. Provider CLI implementation calls are optional after the user’s takeover instruction; any such call still requires finite input, exact session identity, captured output and positive completion evidence.
3. Implementation agents work only on their assigned phase and files and run meaningful checks. Root owns integration, PRs, release publication and phase transitions. Implementation and test code must preserve the prohibition on direct process signals, global provider configuration changes and premature later-phase work.
4. Freeze the proposed head for review. Root independently reads the entire changed surface and relevant consumers, runs meaningful unit/integration tests, and executes the phase's real CLI acceptance. Obtain an independent read-only review using the installed codex review CLI directly, as requested by the user. Do not use the Claude Codex Duo review workflow or companion runtime for these implementation reviews. Every accepted finding has a concrete trigger, code citation, correction and regression test. Verify disputed findings; do not count agreement as proof.
5. Resolve every finding in the responsible implementation area, directly or through its assigned agent. Re-run affected checks and re-review the changed head. Repeat until no unresolved actionable review findings remain. Do not satisfy this condition by relabeling a defect, skipping tests, suppressing diagnostics, or manufacturing terminal output.
6. Push one phase branch and create a reviewable PR with behavior, acceptance commands/results, live versions/platform and known platform limits. For a phase spanning two repositories, use linked PRs with explicit dependency order; keep both pending until their relevant gates pass.
7. Wait for all required GitHub checks on the exact proposed head to succeed and inspect any failure. Implement fixes, push, re-review and wait again. Never merge on a stale green SHA, while checks are pending/skipped unexpectedly, or when required live receipts are missing.
8. Squash merge through GitHub after green CI and zero unresolved findings. Confirm the merged commit, update main with a fast-forward, record phase receipt, and only then start the next phase. Keep private raw logs outside the repo and commit only sanitized fixtures/evidence.

This is a finite phase sequence, not a scheduled background automation. Progress, file ownership, active agent work and any provider attempts are recorded so conversation compaction neither duplicates work nor loses acceptance requirements.

## Phase order and acceptance

| Phase | Outcome | Required unit/behavior verification | Live CLI acceptance |
|---|---|---|---|
| 0 | Corrected design, real help/version executable and reproducible gate | CLI writer/error cases, diagnostic-specific negative/positive lint fixtures, formatting/vet/lint/race/build/vuln/config checks | Compile and invoke actual delegate help/version/invalid requests on local macOS and Linux/macOS CI |
| 1 | Durable task/file protocol and strict config | Tri-state reducers, one-use permits, EEXIST recovery, identity/digests, locks, sync-failure order, seal/immutability | Finite compiled protocol helper processes race and exit at checkpoints on real filesystems |
| 2 | Supervised dispatch/status/collect/cancel/logs and deadline | Fake supervisor/provider failure matrix, no duplicate launch, watcher detachment, no unsafe stop, sealing | Isolated real pueue/pueued4.0.4 plus real delegate/delegate-run and finite fake provider |
| 3 | Antigravity adapter | Versioned success/timeout/refusal/config/session/usage fixtures | Bounded actual agy success, containment, timeout, explicit-session continuation and no-turn collection |
| 4 | Codex adapter | Versioned JSONL/staged-output/config/session/usage fixtures | Bounded actual codex exec read-only success and explicit-session continuation; replay collection |
| 5 | Claude print adapter | Versioned stream/result/config/permission/session/usage fixtures | Bounded actual claude print read/search-only success and same-session continuation; replay collection |
| 6 | Consumer acceptance and private v0.1.0 release | All prior gates, mixed-provider consumer behavior, packaging/checksum/install tests | All three provider targets, installed-path supervised task, private archive/source/tap channel installation |
| 7 | Duo Phase2 fan-out uses layer with skill unchanged | Shim compatibility, sidecar/exit/session/detach/recovery semantics, no duplicate turn | Unmodified review skill reaches actual layer task(s) via opt-in transport and collects verified result |
| 8 | Resumable declared-plan workflow | Manifest/DAG validation, deterministic worker identity, coordinator recovery, concurrency, dependency failure, immutable aggregate | Real supervised multi-provider declared workflow, calling lead exits/re-attaches, exact results preserved |

For early phases a live CLI means the newly built executable and real subprocess boundaries. Paid providers cannot be exercised through an adapter before it exists. From each adapter phase onward, phase-specific live targets are mandatory locally; missing auth/config/binary or inconclusive evidence fails acceptance. CI runs hermetic/unit and isolated supervisor CLI checks without provider credentials. Releases advertise only platform/profile combinations with actual acceptance; cross-builds alone are labeled build targets.

## Cross-cutting acceptance rules

The single terminal authority is a validated outcome record plus the referenced exact payload digest; payload existence alone is pending. One task is one provider turn. Collection never launches, retries or resumes a provider. Unknown admission/liveness is not not-admitted/ended. Submission and provider-start guards cannot be removed to recover progress. Resume creates a new task bound to an exact layer-recorded session and must not overlap another active continuation.

Durability barriers, stage-before-link, close-before-link, stable flock leases and sync error branches are explicit below. Crash checkpoint tests prove process-crash behavior and ordered barriers; they do not assert that a real power-loss experiment ran. No direct signals, process-group handling, exec.CommandContext default cancellation or nonzero WaitDelay. pueue owns stop requests; a watcher bound only detaches. Record stop request, acknowledgment and termination separately.

Safety capability certification is scoped by provider version, OS, requested value and controlled effective configuration. Documented-only safety mappings do not become Executed. Provider startup configuration must not widen agent tool access beyond the declared workspace profile. Preserve auth/session/cache behavior separately. Secrets and unrelated user conversations never enter fixtures, logs in PRs or committed metadata.

Default CLI and record schemas are specified below. Strict unsupported raw argv, token/dollar ceilings and permission intents fail before paid execution. No per-provider fallback silently changes a requested configuration. Provider defaults may be retained and reported honestly where a model/effort override was not requested.

## Phase 6 — private release and install channels

Add a subprocess-only mixed-provider consumer acceptance that dispatches one task to each adapter, preserves all three IDs and provider identities, waits/collects through the public CLI, and proves a repeat collection invokes no provider. Unknown/pending responses expose evidence descriptors; one provider failure cannot be mislabeled a successful aggregate.

Freeze supported versions and publish a capability matrix with executed platform/profile evidence. Baseline build targets are darwin/amd64, darwin/arm64, linux/amd64, linux/arm64. Native installed acceptance receipts state their actual architecture. Do not advertise unexecuted provider-platform safety profiles. Run the complete unit/race/config/gate suite, isolated pueue suite and each provider live target before tagging.

Configure GoReleaser to package BOTH delegate and delegate-run into the same platform archive, include checksums and version metadata, and make installed runner lookup explicit: sibling of delegate first, then an explicitly configured absolute runner path; never rely on an unexplained developer PATH. Snapshot acceptance verifies archive contents, executable bits, checksum validation and binary versions. Install into a disposable prefix and run a complete fake supervised task from a directory that contains neither build sources nor binaries.

Keep releases in the existing private hishamkaram/delegation-layer repository. Use authenticated gh release download for archive installation; do not embed tokens in URLs or artifacts. Document GOPRIVATE and existing GitHub git authentication for go install of both commands at the exact version. Validate source installation with a disposable GOBIN and external Go cache, preserving user's global Go and auth configuration.

Use the same private repository as an explicit-URL Homebrew tap with a Formula/delegation-layer.rb, avoiding creation of a public repository or reliance on deprecated GoReleaser brews generation. Use a tagged git-source formula (git credential authentication, exact tag+revision) with go as a build dependency and pueue as a runtime dependency. The packaging PR includes the formula generator/template. After the release tag fixes its commit, agy generates the concrete formula in a small reviewed follow-up PR; this avoids a formula containing its own impossible self-referential commit hash. Both PRs belong to Phase6 and require green checks before merge. Validate formula syntax/install/uninstall only in a disposable prefix or isolated test environment; do not replace the user's installed tools. Before shipping, verify this private authenticated tap path using current Homebrew tooling; if its mechanics differ, correct the channel before publication rather than claiming an untested install path.

Release packaging PR contains config, scripts, formula template/documentation and acceptance receipts. After its reviewed green merge, root creates private v0.1.0 release at that merged SHA, uploads checksummed assets using the committed configuration, and performs fresh authenticated download/install acceptance. A draft, snapshot or cross-compile is not a completed release. Preserve a failed draft for diagnosis and do not retag silently. The exact-revision formula follow-up must also merge and its authenticated installation pass before Phase6 is complete. Current syntax references: https://docs.brew.sh/Taps and https://docs.brew.sh/Formula-Cookbook (fetched 2026-09-13). Later integration/workflow features get new private version tags after their phases pass, without moving v0.1.0.

## Phase-specific specifications

The following specifications are normative within their assigned phase. Where a future live measurement disproves a proposed flag/schema, stop that phase's advancement, record the counterexample, correct the brief and implementation through agy, and rerun acceptance. Do not treat the untested recipe itself as evidence that its behavior works.


---

# Phase 0 implementation brief — foundation

Implement this phase using `agy` only; this document is a specification, not repository code.
Work in the assigned isolated branch/worktree. Other agents are active; preserve their changes.
The user authorized phase-by-phase implementation, tests, review/fix loops, PRs, green CI, squash merges.
Do not skip verification to advance a phase, and do not start provider work during this phase.

## Outcome and scope

Ship a real `delegate` executable and a recurring engineering gate that proves acceptance and rejection.
This phase contains no task execution, adapters, pueue integration, daemon, provider turn, or signals.
Create behavior-bearing packages only; no `doc.go` placeholder packages or nonexistent build entries.
The v1 release scope is agy, Codex, and Claude print mode, with all three accepted before shipment.
The repository and releases stay PRIVATE; the final workflow follows a declared plan, not autonomous AI orchestration.
The published-runtime target matrix is darwin/{amd64,arm64} and linux/{amd64,arm64}; no Windows/FreeBSD claim.
Cross-compilation is checked for all four targets; native acceptance receipts name the actual OS/architecture.
Linux and macOS family smoke checks do not establish unexecuted architecture-specific runtime acceptance.

## Exact pins and installation

Use module `github.com/hishamkaram/delegation-layer`, `go 1.27.0`; development/CI toolchain exactly Go 1.27.1.
Set `GOTOOLCHAIN=local` for gate/tool commands: do not silently download a different Go compiler.
Pin golangci-lint **2.13.2**, gofumpt **0.12.0**, govulncheck **1.8.0**, GoReleaser **2.18.1**.
`make tools` installs project tools into ignored `./bin`; it must not mutate system/Homebrew installations.
Install golangci-lint and GoReleaser from versioned upstream release archives, with pinned SHA256 manifests.
Obtain the four platform hashes from the exact tagged release checksum files and check them into the installer/manifest.
Install gofumpt with `GOBIN=<absolute ./bin> go install mvdan.cc/gofumpt@v0.12.0` under Go 1.27.1.
Install govulncheck similarly from `golang.org/x/vuln/cmd/govulncheck@v1.8.0`.
Do not use `@latest`, mutable install scripts, unverified archive extraction, or global PATH replacements.
Check tool availability and exact version; fail on mismatch or missing prerequisites with an actionable message.
Use standalone gofumpt only: golangci 2.13.2 embeds gofumpt 0.11.0, so do not enable its formatter too.
The GoReleaser archive names use Darwin/Linux and x86_64/arm64; golangci uses darwin/linux and amd64/arm64.
No production dependencies in this phase; do not fabricate an empty go.sum. Keep tool dependencies out of go.mod.

## Concrete files

Create `go.mod`, `Makefile`, `.golangci.yml`, `.github/workflows/ci.yml`, `.goreleaser.yaml`.
Create `cmd/delegate/main.go`, `cmd/delegate/main_test.go`, and genuinely needed gate/installer/smoke scripts.
Create `scripts/verify-gates.sh`; keep its fixture source in script heredocs or named testdata fixtures.
Update `.gitignore` for bin/, dist/, coverage/profiling outputs and temporary artifacts without hiding source.
Create one authoritative `AGENTS.md`, preserving the user-provided Codebase Memory instructions verbatim.
Add the three useful contracts under agents/_data/: delegation-invariants.md, adapter-contract.md, code-quality-floor.md.
Add the recurring `skills/add-an-adapter/SKILL.md`; it must require the real capability/acceptance evidence.
Add two scoped Codex roles in `.codex/agents/` and register them using the installed Codex configuration schema.
Keep `.codex/config.toml` minimal; do not add unrestricted sandbox, blanket approvals, or parallelism overrides.
Update README.md, DESIGN.md, and docs/IMPLEMENTATION-PLAN.md with the decisions below and actual phase status.
Commit the coordinator-supplied complete execution document at its specified docs path alongside those corrections.
Keep future commands and behavior explicitly planned; do not require nonexistent future targets to pass Phase 0.
Do not add an open-source license or change repository/release visibility; private delivery is the confirmed decision.
Stage only files actually created/changed and inspected; never stage nonexistent later-phase paths.

## Executable contract

Implement `delegate` with the standard library; `main` calls a testable function and returns its exit code.
No arguments, `help`, `--help`, and `-h`: exit 0, usage on stdout, empty stderr.
`version` and `--version`: exit 0, `delegate <version>` plus newline on stdout, empty stderr.
Development version is `dev`; support GoReleaser `-X main.version={{.Version}}` injection.
Permit only the build-metadata `version` variable as an explicitly documented exception to no mutable globals.
Unknown command/flag or any extra argument: exit 2, a useful diagnostic plus usage on stderr, empty stdout.
Output writer failures return exit 1; never ignore write errors to satisfy the linter.
Do not advertise dispatch/status/collect/cancel/logs as implemented before their phase exists.
Unit tests cover those cases and output-writer failure using injected writers, without spawning subprocesses.
The shell smoke invokes the freshly compiled absolute binary path for help/version/unknown/extra-argument cases.
Assert the exact exit codes and stdout/stderr split; no test calls a paid provider or installed pueue.

## Lint policy

Use golangci configuration `version: "2"`, `linters.default: none`, and a finite lint timeout.
Enable errcheck, govet, staticcheck, ineffassign, nilerr, errorlint, errname, forcetypeassert, contextcheck,
exhaustive, forbidigo, nolintlint, gocognit, and gocyclo.
errcheck: `check-type-assertions: true`, **`check-blank: true`**.
govet: `enable-all: true`, `disable: [fieldalignment]`; validate the config with the pinned binary.
exhaustive: `default-signifies-exhaustive: false`; gocognit min-complexity 20; gocyclo min-complexity 15.
nolintlint: require-explanation and require-specific true; no broad/generated-directory exclusions for real code.
forbidigo: `analyze-types: true`, `exclude-godoc-examples: false`, with these anchored identifier patterns:
- `^os\.Process\.(Signal|Kill)$`
- `^syscall\.(Kill|Setsid)$`
- `^exec\.CommandContext$`
- `^exec\.Cmd\.WaitDelay$`
Match selectors, not call syntax: do not append an opening parenthesis to those regexes.
Give each pattern a diagnostic explaining supervisor-owned stopping or the implicit-kill hazard.
`exec.CommandContext` defaults to Process.Kill; nonzero WaitDelay can also kill. Both are prohibited.
The future reviewed subprocess constructor uses exec.Command, explicit independent lifetime observation,
Cancel left nil, and WaitDelay left zero; later budget expiry routes through the supervisor, never direct signals.
A context on a helper API does not authorize cancellation propagation into provider execution.
Use no production exception to the above bans in Phase 0; fixtures are compiled and linted, never executed.

## Recurring gate — actual sequential recipe

Expose `tools`, `tool-versions`, `fmt`, `fmt-check`, `config-check`, `vet`, `lint`, `verify-gates`,
`test-race`, `build`, `smoke-cli`, `vuln`, and `check` as .PHONY targets.
Implement `check` as separate, ordered recursive-make recipe lines, **not a prerequisite list**:
`tool-versions → fmt-check → config-check → vet → lint → verify-gates → test-race → build → smoke-cli → vuln`.
Each recursive command must stop the recipe on failure even with `make -j check`; propagate failures unchanged.
config-check runs pinned `golangci-lint config verify` and pinned `goreleaser check` against real repository files.
fmt-check uses standalone `gofumpt -l` and fails on any listed file; gofumpt itself can exit 0 while listing files.
Scan tracked/applicable Go files without descending into tool caches; `fmt` is the explicit mutation target.
vet runs `go vet ./...`; lint runs the full pinned config; test-race runs `go test -race -count=1 ./...`.
Build native `bin/delegate` and all four release-target binaries with CGO_ENABLED=0, writing only ignored outputs.
smoke-cli executes the native build. vuln runs the pinned govulncheck against ./... and never skips on DB failure.
Document that vuln requires the public vulnerability database; the mandatory behavioral tests remain hermetic.
Only `make tools` installs; `make check` must fail loudly rather than auto-install, silently skip, or mutate source.

## Gate rejection fixtures

For each isolated temporary-module fixture: prove it compiles with `go build`, run only its intended linter
using the **same absolute repository config**, assert exit 1 plus the expected diagnostic, then pass a corrected pair.
Fixtures are source input; do not run their compiled functions, tests, init hooks, or binaries.
Required negatives: Process.Signal, cmd.Process.Kill, syscall.Kill, syscall.Setsid, exec.CommandContext,
nonzero Cmd.WaitDelay assignment, ignored bare filesystem error, `_ = f.Sync()`, and a numeric tri-state switch
missing Unknown while carrying default. The latter must produce an exhaustive diagnostic naming Unknown.
Corrected counterparts handle the filesystem error, enumerate Unknown, and use non-signalling API references.
A known-good full fixture must pass the whole config; a missing tool/config/type error is never an expected failure.
Clean up only the harness's own temp directory; no signals or broad cleanup patterns. Run fixtures on every check.

## CI and Phase 0 release configuration

CI triggers push and pull_request, permissions contents:read, with independent Linux/macOS jobs and finite job timeout.
Use hosted ubuntu-24.04 and macos-15; log uname plus Go host/target architecture in each native smoke receipt.
Pin actions/checkout to `fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09` (official v5 ref inspected 2026-09-13).
Pin actions/setup-go to `924ae3a1cded613372ab5595356fb5720e22ba16` (official v6 ref inspected 2026-09-13).
setup-go uses `'1.27.1'`; disable its default dependency cache until a suitable dependency file exists, or key go.mod.
CI invokes `make tools` then the exact same `make check`; it must not duplicate a second list of checks.
GoReleaser version 2 config builds only `./cmd/delegate` as `delegate`, across the four named targets, CGO_ENABLED=0.
Use .tar.gz archives and checksums; omit delegate-run until its source exists in Phase 2, then add it to both channels.
Use a draft release setting, no publishing workflow/token/tap step in Phase 0, and no deprecated `brews` stanza.
`goreleaser check` needs a git checkout with an origin remote; isolated config tests must provide that metadata.
Snapshot packaging and real installation acceptance land in the release phase; configuration validation is not shipment.

## README/design corrections and exit

State Phase 0 help/version exists; remaining task orchestration is planned. Link the implementation plan.
Task means one turn; continuation creates a new task, preserving original identity/result and launch count.
agy read-only is unsupported; do not change the layer default or imply --sandbox means read-only.
Specify finite stdin then EOF (or supported file input), never brief text in argv, and no provider shell invocation.
State outcome.json is the sole terminal authority; result.txt alone is only a payload and cannot prove completion.
Record unique staging, close-before-link, same-filesystem hard-link/sync requirements, directory-sync durability,
sealed evidence before reduction, and unsupported-filesystem refusal; eliminate contradictory “local implies safe” wording.
Advertise wall-clock execution deadline with separate stop-request/termination facts; reject token/dollar ceilings and raw argv.
Remove the unconditional no-goroutines rule: future budget supervision needs a bounded event loop with explicit ownership.
Explain that terminal payloads/results cannot be overwritten and collection cannot launch or resume paid work.
Update all ordering, live-test and release rows so Claude print joins agy/Codex before v1 release.
Record private authenticated release/install access; public repository, public tap, and unauthenticated go-install are out of scope.
The final workflow consumes an explicit declared plan; it must not infer or autonomously invent new work.
Exit: fresh `make tools && make check`, a `make -j check` ordering/failure check, clean relevant diff, independent review,
all review fixes through agy, a PR with current results, green Linux+macOS CI, then squash merge as authorized.


---

# Phase 1–2 implementation brief

Implementation decisions for `agy`, prepared 2026-09-13. Planning artifact only; this does not claim either phase exists. The orchestrator owns sequencing, review, tests, PRs and merges. Implement only the assigned phase and accommodate other contributors' edits. Update the architecture/plan alongside the relevant implementation so their old completion and no-goroutine rules do not remain authoritative.

## Scope and package boundaries

- Phase 1: `internal/task` (values/pure reducers), `internal/config` (strict decoding), `internal/taskdir` (all filesystem paths and persistence), and a finite compiled protocol CLI fixture. No providers or supervisor calls.
- Phase 2: `cmd/delegate-run`, the five public commands (`dispatch`, `status`, `collect`, `cancel`, `logs`), `internal/pueue`, owned execution/deadline/capture code, fake executable adapter and isolated real-pueued acceptance. Real paid providers begin in their adapter phases.
- Standard library plus pinned `golang.org/x/sys v0.48.0` for Unix locks/full flush. This pin compiled and ran on Go 1.27.1/macOS arm64; the Linux amd64 variant cross-compiled. Keep `go.mod`/`go.sum`. Do not add a storage abstraction, general event bus or provider service locator.
- Filesystem mutation is accessible only through typed `taskdir` operations. Pure readers receive no starter/submission permit; recovery receives only sealed evidence and a predicate implementation. No mutable package globals except sentinel errors.

## Public CLI contract for phase handoffs

```text
delegate [--root ABS] dispatch --provider <antigravity:print|codex:exec|claude:print> --brief FILE --cwd ABS [--id ID] [--permission <read-only|workspace-write>] [--budget DURATION] [--model MODEL] [--effort EFFORT] [--resume-task ID] --json
delegate [--root ABS] status ID --json
delegate [--root ABS] collect ID [--watch DURATION] --json
delegate [--root ABS] cancel ID --json
delegate [--root ABS] logs ID --json
```

Default permission is read-only; default budget is 30m; collect's watch defaults to 0. Unsupported adapters remain refused before admission until their phase supplies them. Phase 2 fake provider injection is a test harness dependency, not an arbitrary production executable/argv escape hatch. The public adapter names are fixed now although Phase 2 only enables fakes in its acceptance build/harness. Human output is optional beside stable JSON; JSON schema carries its own version and returns the task ID plus evidence descriptors.

Exit codes: 0 success, including status observation when the task is pending; 1 operational error; 2 usage/configuration error; collect specifically uses 0 committed, 3 pending/undetermined/watch expired, and 4 rejected. An invariant fault is operational 1 even when its JSON includes a preserved valid terminal outcome. Cancel JSON separates request/acknowledgment/termination and cannot manufacture a terminal result. Logs returns a structured descriptor, not a shell command that automatically executes a provider turn. Dispatch's existing-ID digest comparison includes effective input bytes, cwd, provider, requested config/budget and continuation source, but excludes incidental creation timestamps and generated writer identity.

A task is exactly one turn. `--resume-task ID` references a layer task, never an unchecked user-provided session string. Read its recorded conversation/resume availability, verify the same provider/mode and copy that explicit handle into the new task's immutable execution plan. Always allocate/use a different task ID; original records stay unchanged. Require original terminal evidence or positively established termination before continuation; a stop request or ambiguous liveness is insufficient.

Serialize continuations per provider conversation using `<root>/sessions/<sha256(provider+conversation)>/.session.lock`, a stable O_CLOEXEC flock file. Under this lock, create a durable session claim tied to the new task before submission; claims and release receipts are immutable records under the session directory. A claim is released only after its owning task has terminal evidence or established termination, with a receipt recording that evidence digest. Physical flock release on process exit does not release the durable claim. An unknown owner remains busy and cannot be reset by time, lock availability or a missing supervisor row. Multiple simultaneous continuations from one original therefore grant only one active session claim; identical retries reuse their existing claim. The same-task admission lock still guards submission, and a crash after a session claim but before submission may conservatively strand the session. No automatic claim deletion/retry to unstick it.

## State placement and immutable identity

Default state root: obtain `configDir, err := os.UserConfigDir()` (fail on error), then `filepath.Join(configDir, "delegation-layer")`, used as durable state rather than a cache; explicit global `--root ABS` is supported. Task directories are `<root>/tasks/<task_id>`. The required dispatch `--cwd ABS` is persisted as a canonical absolute path. Resolve existing symlinks before comparing locations. Reject a task when state root and workdir overlap in either direction, or when the root lies inside any additional provider-writable directory. The provider child runs with `Cmd.Dir=workdir`; the publisher runs with its state/config coordinates explicitly supplied, never inferred from current directory.

Create state/task/raw directories as 0700 and files as 0600; reject unexpected ownership, symlinked protocol records and nonregular record files. Use Go 1.27 `os.OpenRoot`/`Root.OpenFile`/`Root.Link` for directory-relative task operations and validate task IDs as exactly 32 lowercase hex characters generated from 16 cryptographically random bytes. No paths, `..`, absolute IDs or arbitrary payload filenames. An explicit `--id ID` uses the same validated format and is an immutable idempotency identity: an existing ID with a different canonical request digest is refused before any external action; matching requests attach to the existing task and never replay its work. Store a random root ID in create-once `root.json`; supervisor labels are `delegate:<root_id>:<task_id>`.

This is a cooperative local store plus verified provider containment, not protection from arbitrary same-UID processes. Do not advertise an unconfined auto-approve mode as preserving the state boundary. A provider-specific output-file argument may name its one staging file outside the workspace, but tools must not receive general write access to the state root. Prefer captured stdout where that extra write is unnecessary. Broad home/root workspaces fail this placement rule rather than quietly putting task state inside a writable tree.

All JSON records carry `schema_version:1`, `root_id`, `task_id`, and `spec_sha256` where a task specification exists. Decode strictly, reject unknown versions/enum strings, duplicate keys, malformed/trailing JSON and identity mismatches. Zero enum values mean unknown. Set explicit maximum control-record size (1 MiB); stream/hash raw output and answers instead of applying that limit to them. An omitted evidence field never becomes false/ended/not-admitted.

Before comparing or claiming a requested task ID, take its admission lock and validate/reuse immutable prepared records; a partially prepared record set with conflicting bytes fails closed. A partially prepared matching set may finish preparation only while submit.json and provider.start are both absent. Use SHA-256 lowercase hex throughout. Canonical bytes are `encoding/json.Marshal` of versioned structs without maps, plus one final newline. Sort any slices that represent sets. Hash the exact persisted bytes. Do not trim, normalize or re-encode answer text during publication.

Minimum records:

| Path | Required content / authority |
|---|---|
| `brief.md` | Exact input bytes, immutable after task preparation. |
| `task.json` | Immutable request: provider, mode, canonical workdir, requested config, finite positive wall budget in integer nanoseconds, optional prior conversation handle, brief digest. Hash of these bytes is `spec_sha256`; it does not contain its own digest. |
| `meta.json` | Immutable prepared execution plan: requested/effective config, containment and approval separately, resolved provider executable/version, publisher build/version, predicate identity, supervisor config/endpoint identity and observed version, creation time. Hash is persisted as `meta_sha256` by launch/evidence records. Session identity arriving later goes in a separate write-once `provider.ref.json`, not an overwrite of meta. |
| `submit.json` | Submission intent, unique label, spec/meta digests and bound supervisor coordinates. Exists before any `pueue add`. Never deleted or reset. |
| `supervisor.ref.json` | Numeric task ID plus matching label, config digest, endpoint and observed version, when positively reconciled. A receipt, never a negative admission proof. |
| `provider.start` | Launch intent with spec/meta digests and budget. Exists durably before `Cmd.Start`; it does not claim the process actually started. |
| `provider.started.json` | Best-effort durable receipt immediately after successful Start: wall timestamp and diagnostic elapsed timing. Absence cannot authorize relaunch. |
| `raw/stdout`, `raw/stderr`, declared adapter staging files | Preserved evidence; only the current runner owns their writers. |
| `provider.exit` | The seal: invocation state (`started` or definite `start_failed`), exit observation, spec/meta/predicate identity, sorted raw manifest `{path,size,sha256}`, and digest of that canonical evidence manifest. Written only after every raw writer is closed and raw bytes/directories are durable. |
| `result.txt` or `publish.reject` | Immutable payload. Rejection uses a deterministic nonempty reason code/text. Neither pathname by itself is a terminal decision. |
| `outcome.json` | Sole publication decision: verdict, task/spec/meta identity, sealed-evidence digest, predicate identity, payload basename/length/digest. No incidental timestamps or writer identity in decision content. |
| `publish.exit` | Publisher/recovery receipt written last, not completion authority. Its absence does not undo a valid outcome. |
| `stop/<request_id>.request.json`, `.reply.json`, `.observed.json` | Separate create-once request, supervisor response and later termination observation. A budget request uses deterministic request ID `budget`; explicit user cancel gets a new random ID. Never confuse these with a publication seal. |
| `faults/<random_id>.json` | Best-effort immutable invariant-fault diagnostic; failure to write it is also returned to the caller. Never replace a winner to report a fault. |

`PredicateRef` is `{adapter, mode, version, sha256}`. Its digest covers the adapter's checked-in versioned predicate contract/fixture manifest; execution build identity is recorded separately. Register implementations explicitly by the full reference, retain prior implementations unchanged if supported, and require a new reference when interpretation changes. Recovery lacking that exact reference returns `incompatible_predicate`/publication-unknown and does not reject or publish. Version labels are compatibility promises, not a claim that differently compiled binaries are identical; docs must say the same versioned predicate is used. Test dispatch with one reference and recovery with a changed/absent reference.

## Durable commit and leases

Crash model: tolerate process exits at every checkpoint; order durable writes for an OS crash/reboot on a local persistent filesystem that honors successful flushes. Do not promise recovery from drive failure, filesystem corruption, operator deletion, malicious same-UID mutation, or hardware lying about flush completion. No power cuts or signal injections are part of these acceptance runs.

Supported release scope is macOS/APFS and Linux filesystems that pass the required integration checks. Do not equate a successful probe with a physical power-loss proof. Reject network/unsupported filesystems and any required primitive failure before dispatch; do not silently downgrade durability or use overwriting rename. Linux CI must run on a persistent filesystem for durability acceptance; tmpfs is acceptable for hermetic process tests but cannot substantiate reboot durability.

Define one internal barrier helper per platform:

```go
// linux: both regular files and opened directories
err := file.Sync()

// darwin: both regular files and opened directories
if err := file.Sync(); err != nil { return err }
_, err := unix.FcntlInt(file.Fd(), unix.F_FULLFSYNC, 0)
```

Do not swallow EINVAL/ENOTSUP/EIO. On this host both calls succeeded on regular files and directories after hard-link creation/removal. Use `_linux.go` and `_darwin.go`; unsupported platforms fail at build/support selection rather than use a fake no-op barrier. When making directories, persist each new directory and its parent before relying on children. Explicit root initialization may create missing parents; persist those namespace changes up to an already-existing ancestor.

Create-once record algorithm:

1. Acquire the store's shared maintenance lease (see below). Create a cryptographically unique stage name with `O_CREATE|O_EXCL|O_WRONLY`, mode 0600, in the destination directory; no shared `result.tmp` name.
2. Write all bytes, checking short writes/errors. Barrier the stage file. Close the writable descriptor successfully **before** linking. A failed write/barrier/close stops the operation.
3. Hard-link stage to the destination with `Root.Link`. Handle EEXIST by the record-specific rules below; all other errors fail.
4. Barrier the destination directory. Only now may successful mutation authorize an external action or acknowledge durable commitment. If linking succeeded but this barrier fails, return a distinct uncertain-durability error, preserve the visible destination, and issue no launch permit. A retry must not interpret the existing guard as unused.
5. Unlink the stage and barrier its directory for cleanup. Cleanup failure is surfaced separately from the already-committed record; it cannot turn the record into absent or authorize a second external action.

Persist the task specification, brief and meta before submission intent. Persist submission intent before `pueue add`, and provider.start before provider Start. Persist raw files and raw directory before the provider.exit seal. Persist the chosen payload and its directory before attempting outcome.json; then persist outcome.json and its directory before acknowledging publication. Never commit outcome.json if payload creation/validation failed.

Stable advisory lock files: `.admission.lock` and `.run.lock` inside each task directory, and `.maintenance.lock` at the store root. Open using `unix.Open` with `O_CREAT|O_RDWR|O_CLOEXEC` (wrap the fd with os.NewFile), persist initial file/parent, and use `unix.Flock(fd, LOCK_EX|LOCK_NB)` or `LOCK_SH|LOCK_NB`; retry EWOULDBLOCK with a bounded observation context, retry EINTR, fail other errors. Do not unlink or recreate lock files, steal a lock by age, infer ownership from a PID, or propagate lock descriptors into child `ExtraFiles`. Close/unlock on normal return; an exiting helper proves kernel release after a crash checkpoint. Lock order is maintenance first, optional session lock second, then admission **or** run; never acquire admission and run together. Assert close-on-exec on every lock fd and never place one in ExtraFiles.

All task mutations hold maintenance SH, including each stage-through-cleanup operation; a runner holds it throughout raw writing. The scavenger takes maintenance EX nonblocking, then removes only recognized stage names. This protects active staging without serializing ordinary publishers, which all hold SH. Never scavenge protocol records, guard files, raw evidence or locks; no whole-task deletion in these phases.

Admission holds `.admission.lock` from intent examination through the single submission attempt/observation. No intent plus a valid prepared task grants a private, pointer-backed one-use permit only after its intent is durable. The sole supervisor Submit entry consumes that permit atomically before its one external call; a copied/reused permit fails. An intent already present grants no permit, even when byte-equivalent: reconcile the bound supervisor label; exactly one identity match means admitted/attach, zero/multiple/unreadable/version-changed means unknown. Reachable empty queue is still unknown. Release the lock when the attempt returns or its observation ends; keep intent forever. Crash before the add is allowed to sacrifice liveness rather than duplicate work.

Runner holds `.run.lock` while it owns raw writers, through sealing/publication. A second runner failing the lock returns running/unknown. After acquiring it, any existing provider.start prohibits another Start, even if equivalent or there is no started receipt. Existing seal permits recovery; no seal means unrecoverable/unsealed evidence and no relaunch. Before the first Start, create raw files without truncating pre-existing ones; unexpected raw bytes before a start guard are an invariant fault.

## Publication, EEXIST and reduction

1. Validate task/meta identities and exact predicate reference; verify the seal's complete allowed raw manifest and digests before evaluating. Refuse to follow raw symlinks. Missing/changed bytes or unsupported predicate are evidence faults/unknown, not provider failure. No seal means no finalization, even if stdout looks successful or the provider PID ended.
2. Predicate returns a validated nonempty answer or a deterministic nonempty refusal reason. Only this value enters publication. OS exit zero/nonempty diagnostics never authorize success.
3. On **payload EEXIST**, read/validate the existing payload's exact bytes/length/digest against the candidate. Equal means continue to the outcome claim after a directory barrier; different/malformed means preserve it and report invariant fault. An absent outcome at this point is expected publication-pending, not an error requiring deletion or a new launch.
4. On **outcome EEXIST**, strictly parse/validate the winner and its actual payload, then compare its complete semantic decision with the candidate. Equal means idempotent completion; different/malformed/unrelated means preserve the winner and report conflict. A valid existing outcome is returned as terminal with a separate conflict diagnostic, never replaced. An invalid winner is unknown/invariant-fault, never done.
5. If both payload names exist, only the valid outcome selects a verdict; report inconsistent extra evidence if its content conflicts. Losing payloads do not become alternate terminal records.

Reducer precedence: valid outcome + matching payload => committed/rejected even without exit receipts; invalid outcome => unknown/fault; no outcome + valid seal => pending/recoverable; payload alone => pending only; no seal => unknown/running according to independent supervisor evidence. `status` is observational. `collect` may perform exactly this recovery but has no Start/Submit dependency. A status snapshot is derived/cache data and cannot override authoritative evidence; it may be replaced atomically if persisted.

## Phase 2 execution and budget

Use the explicit `--budget DURATION` or the documented default of 30 minutes; require a finite positive `TaskBudget` and record it in nanoseconds with an overflow-checked maximum representable duration. Reject token/dollar ceilings and nonempty raw argv before admission. `WatchBound` is optional (`--watch`, default zero/nonblocking) and wholly separate; expiring it returns pending/descriptors and changes no job state. Queue delay is excluded from the task budget.

Amend the no-goroutines rule: the runner owns a small fixed set of goroutines for stdout capture, stderr capture, Wait, and deadline/supervisor observation. The root is independent of dispatcher/watcher cancellation. Every owned worker has a documented completion channel; no anonymous background retry loops. All provider launch uses `exec.Command`, argv slices, `Cmd.Dir=workdir`, no shell, no `CommandContext`, no `Process.Signal/Kill`, no syscall signals/setsid, and no `WaitDelay` (its timeout path can kill). Keep these bans in the mechanical gate, including indirect API spellings where feasible.

Feed brief from a finite read-only regular file to provider stdin and close the parent's descriptor after successful Start; EOF is inherent after its bytes. Create stdout/stderr OS pipes, pass only their write ends to the child, close the parent's write ends immediately after Start, and have owned copy goroutines write raw files. Wait once for the provider and drain both pipes to EOF before syncing/closing raw writers and committing the seal. Do not fabricate a seal when Wait, capture, raw write or flush fails. A definite Start failure may be sealed as `start_failed` with its captured diagnostic; it never removes provider.start or permits a retry. A provider-owned additional output file is permitted only by an adapter whose terminal contract proves it no longer has a writer; copy it into the immutable sealed evidence set before publication.

Establish the monotonic in-memory deadline immediately before Cmd.Start and arm observation before invoking Start; the durable guard must already exist. This bounds launch plus execution conservatively; record successful-start time separately for accounting and document that unavoidable Start bookkeeping is included, while queue time is not. Do not persist/reconstruct Go's monotonic time; stored wall times are evidence, not authority to restart an expired task. If Start blocks, its owned operation is still under observation and may require a supervisor stop; never launch again. Keep the deadline active until provider wait and capture completion, so a descendant holding a pipe cannot make drain unbounded silently. On completion disarm the timer before return; handle expiry/completion races explicitly and verify no stop is requested once completion has been observed.

On expiry: first durably create `stop/budget.request.json` with cause, deadline, task identity and intended supervisor coordinates; then freshly reconcile the exact task ID/label/config/version; request stop only when unique and consistent. Failure to persist the request means report an I/O fault and do not issue the side effect. A failed/unknown stop cannot create a seal, reject, exit code or termination fact. Keep capture/publication available when the provider completes naturally despite a failed stop. Stop requested, stop acknowledged and termination observed are separate status fields; an ended supervisor task is evidence about its supervised job, not proof that every escaped descendant died.

Supervisor subprocess observation also uses exec.Command and Wait ownership, never process cancellation. Give each observation a finite caller bound (default 5 seconds). If it expires, report `in_flight/unknown`, do not retry the mutation, and continue reaping its process if the one-shot CLI remains alive; a CLI may exit with that request still in flight. This is explicitly not a timeout guarantee on the external pueue client. A delayed stop may still take effect. Supervisor/observer hangs are reported failures of enforcement, not silently converted to success. Tests use finite delayed helpers and await their natural exits. Provider completion/publication must not wait indefinitely for an unresponsive supervisor reply.

Resolve a pueue reference immediately before a stop. Reject blank, negative, malformed, duplicate or mismatched numeric targets before exec. The only running-job form is `pueue -c <pinned-config> kill <one-validated-id>`: never no IDs, --all, --group or --signal. For a positively queued task, cancel may request `remove <one-validated-id>`; if it races into running, report the refusal/uncertainty rather than broadening the action. Removal acknowledgment is a separate cancellation fact and cannot manufacture provider.exit/outcome. An explicit later user cancellation may make a new recorded attempt; automatic deadline retries are out of scope. A terminal valid outcome makes cancellation a no-op.

The supervisor binding includes canonical config path, config content digest, explicit endpoint, unique label and observed supported version. Version/config/label changes fail closed. A saved numeric ID alone is never sufficient. pueue has no atomic compare-label-and-stop API: concurrent external reset/restart/edit/replacement of the bound daemon/task is outside the supported operating contract; a preceding status check cannot eliminate that race. Record this limitation rather than claiming identity reuse impossible.

## Verification gates and phase exits

All verification commands must fail on missing prerequisites or violated assertions and preserve receipts. Every spawned helper exits finitely by itself. No test sends a signal, including test cleanup. Always run the Phase 0 gate after changes.

**Phase 1 unit/race:** strict schema/config/enum tests; table-driven health precedence; permit consumed twice; concurrent processes producing at most one submission/start permit; immutable winner; same/different payload EEXIST with no outcome; same/different/malformed outcome; exact digest validation; unsupported predicate; symlink/traversal/root-workspace overlap; stable flock inode and no inherited lock; maintenance scavenger respecting active writers. Inject write, short-write, file/dir/full-flush, close, link and cleanup failures and verify none grants external authority or publishes without valid payload.

**Phase 1 real CLI fixture:** `make acceptance-protocol` builds a finite helper executable from `internal/testutil/protocolfixture` that calls the real taskdir API. Exercise explicit prepare/claim/seal/collect/inspect operations through subprocess invocations against a disposable supported filesystem. At each named checkpoint the helper calls os.Exit itself: before/after guard link and directory barrier, before/after provider.start, before seal, after payload link/barrier, after outcome link/barrier, before cleanup. Another process must observe the specified state, never acquire a second paid-work permit, and recover sealed publication. Include two independently started publishers/collectors and a lock holder exiting without deferred cleanup. This is live CLI/protocol verification, not live provider verification. Exit: `make check`, relevant `go test -race -count=1`, and `make acceptance-protocol` passing on macOS and Linux CI. A process-exit harness is not described as a power-loss test; injected persistence-order tests cover the claimed barrier contract.

**Phase 2 hermetic executable suite:** fake supervisor accepts then loses reply, returns zero/duplicate labels, malformed/unknown status, changed version/config, delayed reply and stop refusal. Fake provider captures literal argv/stdin/invocation count and emits valid success, success-shaped empty response, diagnostic-only failure, malformed/truncated output, delayed stderr/pipe EOF and delayed session identity. Require launcher exit then collection, repeat collection zero turns, two runners at most one Start, watcher expiry zero stop requests, short budget stop requested before the finite provider's natural exit, completion wins timer race, failed stop followed by successful natural publication, and unsealed interrupted capture stays unknown. Test empty-target stop never invokes the executable. Use fake-clock unit tests plus finite real-clock subprocess cases.

**Phase 2 isolated real pueued:** `make acceptance-supervisor` must build and exercise installed `delegate` and `delegate-run` against a fake provider executable injected by absolute path. It also runs an installed pueue/pueued 4.0.4 using an explicit private config, state directory, Unix socket, alias file, pid path and secret path, all outside the workdir. Use a short unique `/tmp/dl-<random>` base so macOS Unix-socket path limits are not exceeded. Never rely on HOME/XDG/PUEUE_CONFIG_PATH overrides to isolate a default config, never read/alter the developer's queue, and never use PATH shadowing. Pass `-c` on every client/daemon call. Version mismatch fails acceptance.

The generated private YAML sets `shared.pueue_directory`, `runtime_directory`, `unix_socket_path`, `alias_file`, `pid_path`, `shared_secret_path`, `daemon_cert` and `daemon_key` below that base; `use_unix_socket:true`; `client.show_confirmation_questions:false`; `daemon.callback:null`; `daemon.env_vars:{}`; and `daemon.shell_command:["/bin/sh","-c","{{ pueue_command_string }}"]`. Verify resolved locations before daemon launch; do not start if isolation is incomplete. Start pueued in foreground using exec.Command and retain its Wait ownership. Query only its endpoint until ready. Submit via `add --escape --label <bound-label> --print-task-id -- <absolute-delegate-run> --root <root> <task-id>`; no brief text in this shell-reparsed command. Validate label visibility from inside the finite runner by its own status reconciliation; do not invent a PUEUE_TASK_ID environment variable.

Prove literal paths/arguments containing spaces, quotes and shell metacharacters, actual task identity reconciliation, captured logs, naturally completed publication after dispatcher exit, and later repeat collection. Record the observed pueue JSON as versioned fixtures. Keep live stop/cancel signal behavior out of this acceptance: stop routing is exercised against the fake supervisor. After every finite job is positively finished, call `pueue -c <private-config> shutdown`, wait for that daemon's natural exit, then remove only the private fixture directory. If readiness/cleanup cannot complete without signals, fail with the exact private locations and leave them for inspection. Exit: `make check`, `make acceptance-protocol`, and `make acceptance-supervisor` green locally and on both OS-family CI runners. Adapter phases will add actual provider execution.

## API evidence and receipts

- Linux namespace durability requires a containing-directory fsync in addition to syncing file content: [Linux fsync(2)](https://man7.org/linux/man-pages/man2/fsync.2.html).
- Darwin full flush requests disk-buffer flushing after fsync; successful API return still cannot prove a physical device honored it: [Apple fcntl(2)](https://developer.apple.com/library/archive/documentation/System/Conceptual/ManPages_iPhoneOS/man2/fcntl.2.html).
- Locks attach to open file descriptions and close releases their ownership: [Linux flock(2)](https://man7.org/linux/man-pages/man2/flock.2.html). Go APIs: [x/sys/unix](https://pkg.go.dev/golang.org/x/sys/unix).
- Pueue's explicit configuration fields/defaults are verified in [v4.0.4 settings.rs](https://raw.githubusercontent.com/Nukesor/pueue/v4.0.4/pueue_lib/src/settings.rs); local 4.0.4 help confirms explicit -c, add --escape/--label/--print-task-id, numeric kill targets, and shutdown.
- Host Python syscall receipt: `protocol-checks/flush-api-probe.json`. Go API probe: `protocol-checks/flush-go/` with pinned x/sys, successful native run and Linux amd64 cross-build. These are API-feasibility checks, not completed release-platform or power-loss acceptance.


---

# Provider implementation specifications — Antigravity, Codex, Claude print

Prepared 2026-09-13. Incorporate these as consecutive adapter phases after the durable task/supervisor phases and before release: **A: Antigravity, B: Codex, C: Claude print**. All three are required release dependencies. Claude background mode remains out of scope. No implementation, provider turn, global configuration mutation, or live permission test was performed while preparing this specification.

This supplements the exact CLI help/version captures and official citations in `provider-checks/REPORT.md`. Installed baselines: agy 1.2.2, codex-cli 0.154.0, Claude Code 2.1.268, all under `/opt/homebrew/bin`. These are candidate supported versions; only passing the phase's live acceptance qualifies a launch profile. New versions begin unverified until their fixtures and safety cases pass. No additional model or effort override is required for these phases.

## Decisions common to all three adapters

### Scope of the permission guarantee

Containment concerns agent-initiated operations against the task workspace. Provider-owned auth, session, log, and cache writes are outside that guarantee and must be disclosed as provider runtime behavior. Put delegation task state, configuration snapshots, raw evidence, and outcomes outside every provider-writable workspace and provider-writable temp/cache root. A user directory merely outside the workspace is insufficient if it is inside a sandbox's permitted temp root.

The threat model includes model mistakes, inherited configuration widening permission, stale capability records, and ordinary configuration drift. It does not promise protection against a hostile administrator replacing files between validation and provider startup. Never modify the user's global provider configuration to make an acceptance test pass.

Each adapter has one minimal supported profile, not an arbitrary flag escape hatch:

| Adapter | Required initial capability | Approval behavior | Deferred |
|---|---|---|---|
| antigravity:print | workspace-write within validated roots | deny requests requiring an unavailable approver; pre-approved sandboxed commands may run | read-only, unrestricted auto-approve, additional writable roots |
| codex:exec | read-only | never ask; no approval escalation | workspace-write certification, unrestricted execution |
| claude:print | read-only via restricted built-in tool surface | dontAsk plus no permission host | writing, general Bash, Agent/subagents, MCP, background mode |

Use embedded, versioned certification records covering provider executable/version, profile revision, supported OS, declared workspace effects, approval behavior, configuration-source validation rules, and evidence status. Ordinary dispatch validates its effective configuration against that certified profile; it does not require a new local receipt or run a paid probe. An explicit `delegate doctor --provider MODE --verify` may run the controlled acceptance probe and return evidence, but must never auto-launch it during dispatch. `Documented` is not `Executed`. Release requires `Executed` for the minimum profile on the supported runtime platforms; a missing prerequisite fails the named live target and cannot become a skip or a passing fake test. Unsupported permission intents fail before supervisor admission. `--allow-unsupported` cannot bypass safety profiles or nonempty raw argv rejection.

### Effective configuration and launch plan

A provider-specific resolver produces a typed launch plan before admission: absolute executable, exact argv, canonical workspace, finite stdin file, required environment changes, settings-source inventory, non-secret normalized policy digest, binary version, model/effort request, and expected session identity when resuming. Do not expose credentials in metadata or config snapshots. Preserve existing authentication; remove only documented conflicting provider control variables, not the entire environment blindly. Keep secrets out of command lines.

Resolve all relevant sources for that provider and supported version, including platform/managed sources that still apply after the chosen suppression flags. Validate the fields affecting tool availability, writable roots, bypass rules, hooks, MCP and custom executables. Benign appearance settings do not need rejection; unknown values in policy-affecting fields do. An unreadable applicable source, unrecognized policy mechanism, or conflict yields `unsupported-effective-config` before launch, with the source path/type and reason rather than secret contents.

Re-resolve just before provider launch. If the normalized policy digest or workspace identity differs from admission, refuse this launch with explicit evidence; do not silently adapt. Persist the policy digest and relevant normalized decisions. This catches ordinary changes and makes the check auditable; a fingerprint is not an OS isolation primitive. On resume, resolve again rather than inheriting the predecessor's permission certification.

Use the global CLI contract: `delegate [--root ABS] dispatch --provider antigravity:print|codex:exec|claude:print --brief FILE --cwd ABS [--id ID] --permission read-only|workspace-write --budget DURATION [--model/--effort] --json`. Default permission is read-only and default task budget is 30m; therefore Antigravity requires explicit workspace-write. Default task state is `os.UserConfigDir()/delegation-layer`, subject to the writable-root exclusion above. Allow omitted `model` and `effort=default` without adding flags. Record them as provider-default, plus any actual provider-reported values; never invent an effective value from the requested value. Add explicit model/effort flags only when requested and supported by the tested model/profile combination. Router aliases and fallback-provider selection are excluded from these phases.

### Input, evidence, and task identity

- Deliver one nonempty UTF-8 brief from a regular, task-owned file on stdin, then EOF. Adopt a documented common brief limit of 8 MiB; reject larger briefs before admission. Do not put brief text in argv, use stream input for multiple prompts, open a TTY, or silently truncate input.
- Use Go argv slices and an explicit working directory. No provider command is shell-reparsed. The executable must resolve to the preflighted provider binary, not a shell alias or a mutable PATH guess at launch.
- Open task-owned raw stdout/stderr files before starting the child and preserve complete bytes. An adapter may parse a copy incrementally for progress; only the common seal and publication protocol can authorize a terminal result. Raw writes and any output-file staging finish before sealing. Parser limits may refuse interpretation but must not silently truncate captured evidence.
- The collector only parses sealed task-bound evidence. It never runs a provider, issues a prompt, resumes a session, or scans unrelated conversations. Repeated collection produces the same outcome and no usage increase.
- One submitted brief is one task. The public `--resume-task ID` option creates a new task linked to its predecessor and explicitly uses the recorded provider session ID. Never use newest-session selectors (`--last`, `--continue`, bare `--resume`), names, an interactive picker, or a session ID guessed from a filename.
- Limit v1 continuation to sessions produced and identified by this layer. Serialize active continuations for a provider session using the existing task/lease mechanism; otherwise two new tasks could concurrently mutate one conversation. A held/unknown session lease refuses continuation; it does not trigger another launch. `resume_handle.available` may be false.
- Provider-reported session identity must match the requested continuation identity. Conflicting IDs, multiple independent turns, or ambiguous final messages reject publication. A provider ID alone is never evidence that admission succeeded or that a turn completed.
- Keep the common task deadline and watch bound separate. Only the existing supervisor seam can request a stop; the adapter never signals, kills, or owns an independent timeout wrapper. Preserve `stop_requested` and `termination_observed` separately.

### Publication and accounting decisions

Publication requires common durable evidence checks plus adapter-specific positive success, a non-whitespace answer, correct task/session binding, and absence of a known interruption/incompleteness marker. Normal exit is corroboration, never sufficient. A nonzero exit with a success-shaped record is a conflicting completion and cannot be published until a version-specific rule has explicitly been verified; v1 rejects it conservatively.

Distinguish `provider-failed`, `incomplete-output`, `invalid-output`, `identity-mismatch`, `permission-policy-mismatch`, and `publication-pending`. Unknown/truncated data cannot manufacture a successful or failed provider execution claim. A committed rejection can describe invalid evidence without asserting the task never executed.

Usage data carries a scope (`task`, `conversation-cumulative`, `main-agent`, `whole-tree`, or `unknown`), source, reliability, and nullable counters. Missing is not zero. Store provider counters without merging overlapping fields. A cumulative delta is task usage only when both endpoints belong to this exact serialized continuation chain and the provider's reset semantics are verified. No token/dollar ceiling is advertised in v1. Claude's native caps are a deliberate deferral, not evidence that no provider can enforce one.

## Phase A — Antigravity print

Own `internal/provider/antigravity/**`, provider fixtures, acceptance cases, documentation table row, and the explicit adapter wiring entry. Avoid changes to generic task/publication semantics merely to accommodate one envelope.

### Minimum launch profile

Working directory is the canonical requested workspace. Start fresh with this argv shape (brackets below describe placeholders, not literal shell syntax):

```text
agy --sandbox --output-format json --input-format text
    --disable-slash-commands --print-timeout <positive-task-duration>
```

The brief is stdin, closed after its bytes. Piped input selects headless mode on the installed CLI, as the existing project measurement demonstrated. Do not append `-p <brief>` or an empty positional prompt. Resume adds exactly `--conversation <recorded-conversation-id>` and otherwise uses the same profile. If the installed supported version needs an explicit print switch with stdin, establish that exact spelling in acceptance and record it; never guess by launching a second task.

**Do not add `--dangerously-skip-permissions` to this minimum profile.** Auto-approval can approve requests to escape the sandbox. The baseline allows workspace file operations and any validated pre-approved commands that stay sandboxed, while headless policy denies unresolved requests. A profile needing all tool approvals is a separate, deferred capability.

Preflight reads the applicable non-secret policy from the documented global settings location and every additional project/agent/workspace source established for 1.2.2. The CLI exposes no per-run settings-file flag in captured help. Do not manufacture one, rewrite global settings, or repurpose HOME. The supported baseline works with an already-compatible validated provider configuration.

Reject before launch when any applicable effective setting:

1. enables `allowNonWorkspaceAccess`, bypass approval (`always-proceed`), or equivalent behavior not included in the tested profile;
2. adds `write_file(*)`, an absolute/relative writable target outside the canonical workspace, or a path whose traversal/symlink resolution cannot be proven contained;
3. includes any `unsandboxed(...)` allow rule, including wildcard or regex forms; do not attempt a regex safety theorem in v1;
4. introduces extra workspace folders, enabled MCP/custom tools, executable hooks, or extension-provided effects not disabled or bounded by this profile;
5. changes an applicable policy source after its admission digest was computed.

Normal system temp/build-cache writes allowed by the native sandbox are enumerated in the effective profile and excluded from delegation state placement. Keep `read-only` unsupported. For a compatible no-bypass configuration, workspace file reads/writes are the required useful minimum; shell execution is not required to qualify this phase. Scoped `command(...)` allow rules are acceptable only where the tested sandbox keeps them within the declared effects and no escape rules exist.

Precise blocker, if encountered: relevant source discovery or an inherited permissive setting cannot be neutralized per invocation by any verified 1.2.2 mechanism. Report that profile as unavailable on this host and fail the phase. Do not call all Antigravity installations unsupported merely because this condition is possible, and do not downgrade containment to a warning.

### Envelope, continuation, and usage

Require one complete JSON object, no trailing non-whitespace output, a syntactically valid nonempty `conversation_id`, exact `status == SUCCESS`, a non-whitespace string `response`, no error payload, and no `[agy] print timeout` stderr marker. Non-success status, timeout marker with any response, or SUCCESS with an empty response cannot publish. Empty SUCCESS without a marker is `incomplete-output`, not proof that a timeout occurred. Preserve unknown enum values as invalid/unsupported evidence.

On continuation, require conversation_id equal to the recorded predecessor ID. Treat num_turns as cumulative metadata, not the number of tasks launched. Store usage and duration as conversation-cumulative unless supported-version fixtures establish otherwise for this mode. Timed-out or incomplete usage is unreliable even when every counter is zero. Do not parse Antigravity's private conversation database in v1; expose an unavailable transcript descriptor where no supported lookup exists. Raw task evidence remains recoverable.

### Required fixtures and live gate

Fixtures: valid fresh/resume success; whitespace response; SUCCESS with empty response; timeout marker with empty and nonempty responses; every known non-success state; error with diagnostic response; wrong conversation ID; missing/duplicate IDs; extra envelope/trailing bytes; malformed/truncated JSON; invalid usage type; cumulative usage delta and reset; unsafe configuration cases above; config changed after admission; exact brief transport containing quotes/newlines/metacharacters.

`make acceptance-agy` includes these fixtures plus **three bounded provider turns**: (1) read a random nonce from a scratch workspace file, create a workspace sentinel, and attempt an explicitly named sibling scratch write that must be denied; inspect actual filesystem outcomes, not the answer's claim; (2) continue that exact conversation asking for the prior nonce and verify a new task, same provider ID, and byte-identical original result; (3) a deliberately long answer with a short native print timeout, verifying no result publication and preserved raw evidence. Any outside write fails the phase immediately. The normal profile's file-write capability and containment both need observation; model refusal to attempt the positive control is inconclusive.

Malformed/unsafe settings are tested with injected resolver fixtures or a verified temporary provider configuration mechanism; never alter global settings for testing. Failure to produce the historical empty-SUCCESS timeout shape on 1.2.2 is not by itself a defect: record the actual timeout envelope and require correct non-publication for it while retaining the old regression fixture.

## Phase B — Codex exec

Own `internal/provider/codex/**`, its fixtures, acceptance cases, docs and adapter map entry. Changes to generic interfaces require an explicit rationale and rerunning earlier phases; a diff-size rule must not prevent a necessary correction.

### Minimum launch profile

```text
codex exec --json --color never --ignore-user-config --ignore-rules
    --strict-config --sandbox read-only -c approval_policy="never"
    -c allow_login_shell=false --cd <workspace>
    --output-last-message <task-state>/raw/codex-last-message.txt -
```

Here each `-c` value is one argv token, generated as valid TOML. No prompt text appears in argv. The output path is new for this task and cannot be a preexisting result. Do not use --ephemeral because exact continuation is part of acceptance.

This isolates the base user config and execpolicy rules, **not every config source**. Validate remaining trusted project, system, cloud-managed and requirements sources relevant to the supported version. Pin sandbox and approval in the invocation; neither substitutes for the other. Reject effective custom executable hooks, enabled MCP/custom tools, extra write grants, or conflicting managed policy not neutralized by verified overrides. For a discovered MCP server, the documented `mcp_servers.<id>.enabled=false` override may disable it; generate safely quoted TOML paths and verify the effective result. Do not assume `mcp_servers={}` erases inherited tables. Hook arrays can be cleared only where the installed resolver's merge behavior is verified; otherwise reject the affected profile. A clean compatible config is the minimum supported case; no requirement to support arbitrary personal plugins.

Do not relocate CODEX_HOME or copy authentication into scratch directories. --ignore-user-config deliberately keeps the existing auth location. No model/effort flags are necessary for the minimum case; provider-default behavior must be recorded honestly.

Continuation shape: put the same exec options that are not resume-local **before** `resume`, then `resume <recorded-thread-uuid> -`. Use captured `codex exec resume --help` for allowed child-option placement. A hermetic argv test and one actual resume must prove this exact invocation; do not reuse a fresh-launch argv by concatenating it after `resume`.

### Events, answer, and transcript

Parse JSONL without assuming a final newline. Track one thread.started/thread_id, one turn.started, subsequent item events and one successful turn.completed. A turn.failed event or other terminal interruption rejects publication. Recoverable error/retry events earlier in a stream do not alone defeat a later successful turn; retain them as diagnostics.

Require a non-whitespace final agent message associated with this invocation's completed turn, and the raw -o file to agree when present. Ignore earlier commentary/plans, reasoning, tool output and subagent messages as answer candidates. For the exact installed schema, fixture the final-message identification rule rather than assuming the last text of any type is the answer. Missing turn.completed, mismatched thread IDs, conflicting finals, or output-file content without a successful completed turn cannot publish. If stdout alone contains a verified final answer and -o is absent due to a publication crash, recovery uses stdout under the same predicate; the -o file is not the authority.

Usage from turn.completed is stored as task/turn scoped according to the verified event schema. Record cached input and reasoning counters without double-counting. Transcript lookup is optional and bound to the exact recorded thread. The installed CLI exposes paginated-history migration; do not hardcode a glob over ~/.codex/sessions or inspect unrelated sessions. A null descriptor is preferable to a guessed file.

### Required fixtures and live gate

Fixtures: complete success; commentary plus final; retry/error followed by success; turn.failed; missing terminal event; partial final record; blank final; stale/contradictory -o; multiple IDs; resumed identity mismatch; subagent message never selected; final JSONL line without newline; unknown event types tolerated unless they make termination ambiguous; relevant config overrides and remaining-source conflicts.

`make acceptance-codex` performs fixtures and **two bounded provider turns**: (1) read a nonce from a scratch workspace and attempt writes to workspace/sibling sentinels; no sentinel may change, successful Read must be observed, and no permission host may be consulted; (2) resume the exact resulting thread with a new task, recover the nonce, verify the identity and immutable original result. Workspace-write and arbitrary existing-session resume remain unsupported until separately certified.

## Phase C — Claude print, required before release

Own `internal/provider/claude/**`, its fixtures, acceptance cases, docs and adapter wiring. This phase is not satisfied by Claude background commands or by the later consumer-plugin port.

### Minimum read-only launch profile

Create task-owned regular configuration files before launch: `claude-profile.json` containing the following object, and `empty-mcp.json` containing `{ "mcpServers": {} }`:

```json
{
  "disableAllHooks": true,
  "permissions": {
    "defaultMode": "dontAsk",
    "disableBypassPermissionsMode": "disable"
  }
}
```

Fresh argv:

```text
claude --print --input-format text --output-format stream-json --verbose
    --safe-mode --restricted --tools Read,Glob,Grep
    --disallowedTools mcp__* --strict-mcp-config --mcp-config <empty-mcp.json>
    --settings <claude-profile.json> --permission-mode dontAsk
    --permission-prompts none --disable-slash-commands --no-chrome
    --session-id <new-task-owned-uuid>
```

Execute from the canonical workspace and pass only stdin as the prompt. `mcp__*` is a literal argv token, never shell expansion. --restricted excludes user/project/local settings; adding --setting-sources with an empty value is unnecessary unless its exact supported semantics are established. --safe-mode excludes unmanaged customizations; --tools restricts built-ins, while --disallowedTools and strict empty MCP address the separate MCP surface. If EndConversation remains exposed by the CLI, accept it as a non-mutating terminal control, not as unexpected filesystem access.

**Do not use --bare as a convenient replacement:** it changes Anthropic authentication and would invalidate subscription-login support. Do not use --allowedTools alone as a restriction, --permission-prompts none as containment, or permission-mode plan as a filesystem guarantee. The minimum intentionally has no shell, Edit/Write, Agent, tool discovery, browser or custom tools; useful read/search/answer work must still succeed. No extra model/effort flag is required.

Validate managed settings before admission. disableAllHooks set through ordinary settings does not disable managed hooks. Reject active managed executable hooks/status-line/file-suggestion commands, managed MCP conflicts, or policies that replace/loosen this profile, unless effective managed policy itself disables them. Preserve organizational requirements; do not try to bypass them. If the sources cannot be resolved or a required setting is silently ignored in print mode, return a precise unavailable-profile error. That is the conditional blocker; it is not proof that the baseline cannot run on an unmanaged compatible host.

SessionStart evidence must confirm the actual tools/permission mode wherever the installed schema reports them; unexpected mutating tools or a changed mode mean no publication and profile certification failure. Preflight performs the protection before startup; checking init afterward is additional evidence, not a way to undo startup hooks.

Resume removes --session-id and adds exactly `--resume <recorded-uuid>`, preserving all permission/configuration flags. Do not use --fork-session or a display name. Only sessions created by this profile are eligible initially; verify a resumed init cannot restore a broader tool surface from history.

### Result predicate and accounting

Drain the entire stream and wait for the common sealing conditions. Find the final top-level result for the task's expected session UUID. Require type=result, subtype=success, is_error=false, and a non-whitespace string result. Reject error_max_turns, error_max_budget_usd, error_during_execution, error_max_structured_output_retries, provider interruption, and identity conflicts. Preserve stop_reason; an explicit max_tokens truncation is incomplete and refusal is a distinct non-success outcome. Missing optional fields are handled by version-qualified schema, not guessed as success. Fixtures decide the exact presence requirement for is_error in the supported CLI output.

Do not publish an assistant text event on its own. Trailing system messages may follow a result. Background-work waits can outlive an initial answer; even though the minimum tool surface disallows spawning that work, fixtures must ensure an early result never seals still-open output.

Store total_cost_usd as a provider client estimate. Store main-loop usage and whole-tree modelUsage as distinct scopes, not sums of overlapping totals. Null/missing/zeroed crash accounting is unreliable. Native dollar/agentic-turn caps remain deferred under the wall-only v1 contract; their error envelopes are still required parser fixtures.

### Required fixtures and live gate

Fixtures: success and each error subtype; nonempty error diagnostic; empty success; assistant-only stream; malformed/truncated stream; result followed by benign system events; multiple result/session IDs; explicit refusal/truncation; permission denials; missing/zeroed usage; unexpected mutating tools in init; inherited acceptEdits neutralized by restricted source loading; managed hook and MCP conflicts refused before start; no-session-persistence/environment conflict rejected; fresh/resume argv parity.

`make acceptance-claude` performs fixtures and **two bounded provider turns**: (1) read/search a nonce from scratch workspace files while requesting workspace and sibling writes, shell execution, and a subagent; Read/Glob/Grep must work and mutating/escape routes must be unavailable or denied, with sentinels unchanged and no hooks/MCP execution; (2) continue the exact session in a new task, recall the nonce, and reassert the same tool/permission profile and unchanged original outcome. The positive read control is required; an agent that merely refuses every request does not establish usable support.

## Shared acceptance discipline and release boundary

Use random nonce data and isolated scratch paths owned by the acceptance harness. Never target actual home config, credentials, existing files or production repositories with write probes. Default per-turn task deadline is 120 seconds, with a separate 150-second acceptance watch bound; the intentional agy timeout uses a shorter native print timeout. If the task has not terminated when the watch ends, report the live test failed/undetermined with its exact task handle; do not retry the paid turn, kill by PID, or fake a terminal outcome. Stop routing remains the generic supervisor's responsibility. Deliberate process-tree/signal tests remain outside these provider targets.

The default suite needs at most seven paid turns across the three adapters. A paid rerun is justified only by a specific changed implementation or invalid/inconclusive measurement, and its evidence is kept. Fixtures and preflight refusal tests do not launch paid providers. Required targets fail on missing binaries, auth, unsupported config, wrong versions, inconclusive probes or missing expected evidence.

Sanitize newly recorded fixtures before committing: synthetic scratch paths/nonces are fine; redact credentials and unrelated environment, preserving schema and semantic fields. Record provider version, OS, profile revision/digest, requested/default model and effort, invocation structure, expected/actual observations, and artifact paths. Evidence of a success-shaped timeout must preserve both stdout and stderr. Run collector replay after each live task and assert no additional provider process starts or usage appears.

The release phase depends on all three named provider targets and the shared supervisor/recovery suite. It must exercise the shipped CLI through subprocesses against each adapter. A platform without passing containment evidence must not be advertised as supported for that profile. Any unresolved minimum-profile blocker fails the adapter phase and prevents release; keep the implemented fixtures and honest unsupported result instead of claiming that an exposed flag or one successful answer certifies the mapping.

## Additional official evidence used for this specification

The preceding audit report lists baseline sources. These fetched primary pages clarify the new launch decisions:

- https://antigravity.google/docs/cli/permissions/ — documents action namespaces, inheritance, default approval behavior, and unsandboxed grants.
- https://antigravity.google/docs/cli/sandbox/ — write mounts and sandbox escapes depend on permission rules.
- https://learn.chatgpt.com/docs/config-file/config-basic — flags override values but project/system/managed sources still exist.
- https://learn.chatgpt.com/docs/config-file/config-reference — documents per-server enabled=false, lifecycle hook configuration, and allow_login_shell.
- https://code.claude.com/docs/en/cli-reference — distinguishes restricted tools from allowedTools, MCP from built-ins, safe-mode from bare, and managed policy from ordinary settings.
- https://code.claude.com/docs/en/settings — setting sources and precedence; settings lists may merge, so an empty object is not a blanket reset.
- https://code.claude.com/docs/en/hooks — disableAllHooks respects managed hierarchy; managed hooks need managed disablement or profile refusal.
- https://code.claude.com/docs/en/headless — stdin, structured output, permission routing, and output/process lifetime.
- https://code.claude.com/docs/en/agent-sdk/agent-loop — terminal result variants and separate usage scopes.

Evidence level: command availability and documented semantics were checked; no profile is newly marked Executed here. Version-qualified live gates above establish those claims during implementation.


---

# Phase 7 consumer integration and Phase 8 declared workflows

Decision-complete implementation plan, 2026-09-13. This is a design artifact; no provider turn, repository edit, integration test, or behavioral certification was performed for this document. All repositories, linked PRs, release assets, and test artifacts remain private. Provider controls and terminal normalization are defined in `implementation-provider-spec.md`; this document consumes that public contract rather than duplicating native provider logic.

## Evidence and scope

The sibling consumer source was read at `claude-codex-duo` commit `c3559f63923ceee9b0cc6728894dab5cc7a1dcf1`. Graph project `claude-codex-duo-review`, generation `2026-09-03T08:45:55Z`, was ready but stale. Targeted runner search returned zero symbols, not evidence of absence. Coverage marked the runner, skill, protocol, gate, CONTRIBUTING, validator and argument-test paths `metadata_changed`; direct source reads supplied the evidence below. Re-pin and reread these files when Phase 7 starts; do not port stale line numbers mechanically.

- `plugins/codex-pr-review/scripts/codex-run.sh:5-64` defines fresh/resume/attach commands, mandatory expected job on attach, claim ownership, sidecars, terminal-only `.exit`, and detached exit 6. Its Codex branch at lines 1389-1586 currently uses companion task/status/result/cancel and extracts the session from result trailers.
- `plugins/codex-pr-review/skills/two-model-pr-review/SKILL.md:425-437` dispatches one fresh read-only Phase 2 call per available participant, using independent prefixes and claims. Before join, only control sidecars may be consumed; the original skill remains unchanged in Phase 7.
- `plugins/codex-pr-review/skills/two-model-pr-review/references/codex-protocol.md:247-248` supplies the existing Phase 2 invocation; lines 269-291 define control/result handling and exits. Lines 314-327 and 345 use `--resume-last` for Codex consultation/resolution, with thread comparison and an explicit fresh fallback policy.
- `plugins/codex-pr-review/skills/two-model-pr-review/scripts/phase-gate.sh:961-1046` currently resolves Codex claim release through the companion, including a job regex and repository-wide fallback. `confirm-terminated` begins at line 1141 and also consults that backend. A runner-only transport change would therefore leave recovery attached to the wrong authority.
- `scripts/validate.sh:68-71` requires byte-identical runner copies in the three plugins. CONTRIBUTING requires read-only review, backend-owned cancellation, permanent kernel lock files, paired manifest/marketplace/changelog changes, and source validation. No installed cache/global configuration change is part of this phase.

## Phase 7: opt-in transport for the existing review runner

### Deliverable and ownership

Create a private, dedicated worktree in `claude-codex-duo`, with a private PR linked to the delegation-layer integration PR. Add an opt-in runner transport selected by `CODEX_RUN_TRANSPORT=delegate`; unset retains the existing backend. Keep the review `SKILL.md` byte-identical and its command shapes unchanged. `--via codex` maps to `codex:exec`; `--via ccr:<alias>` continues to use its existing CCR backend. Do not claim migration of arbitrary CCR aliases, additional providers, or the other skills' entire behavioral surface.

The runner copies, a small copied `delegate-job` transport helper if useful, relevant phase-gate paths, validation/tests, protocol documentation, manifests and changelogs are the integration ownership boundary. Respect the sibling's copy/version rules. Do not replace the Codex companion, patch live installed plugin caches, or change global user settings. Use source-package entrypoints and a task-scoped environment in acceptance.

### Durable transport selection

1. `CODEX_RUN_TRANSPORT=delegate` is consulted only for a new attempt or `--probe`. Resolve a configured absolute delegate executable and an explicit/default state root using the public delegation contract. Record executable/version/root before dispatch. Never silently fall back to the companion if delegate is unavailable.
2. After existing argument validation and claim ownership, but before any submission, atomically create a versioned `<prefix>.delegate-attempt.json` under the existing permanent claim lock. It contains transport, canonical prefix, claim/attempt identity, native provider mode, delegate root, deterministic task ID, canonical request hash, prompt digest, original launch mode, and nullable exact predecessor task. No prompt/result body belongs in control metadata.
3. The delegate task ID is 32 lowercase hex characters derived from a domain-separated hash of root namespace, canonical prefix, claim/attempt identity, and immutable request identity. Persist the chosen ID before submission. A reused prefix with a new authorized claim gets a new ID; recovery of the same claim keeps its ID. The public dispatch API rejects ID reuse with a different request.
4. Attach, cancel, release, recovery, rotation, terminal verification, and result publication select the recorded transport even if the current environment changes. Conflicting/malformed records fail closed. Keep outer `backend=codex` for participant compatibility and add `transport=delegate`; `job=` is the exact delegate task ID, not a companion-shaped fabricated value. All transport-aware branches must run before companion regex/process/repository-wide fallback logic.
5. The helper invokes only public delegate commands and consumes their versioned JSON. It must not read private pueue schemas or reproduce native provider normalization. It may reconcile an existing deterministic task; it must never resubmit unknown admission or infer non-admission from missing output, PID, elapsed time, or a failed status call.

### Launch, attach, and continuation

- Existing Phase 2 command remains `codex-run.sh PREFIX --claim TOKEN --fresh --prompt-file FILE --stall-min 12 --max-min 40`. Submit `dispatch --provider codex:exec --permission read-only --brief FILE --cwd REVIEW_CWD --id ID --json` with the recorded runtime budget. No model or effort override is added unless explicitly configured by the caller.
- `--max-min` is a watch bound, never proof that work stopped. Runtime budget is a separately recorded delegate duration (default 30m; task-scoped transport configuration may override it explicitly). `--stall-min` on this transport can emit an observation, but idle output cannot trigger workload cancellation or a terminal stalled result. Existing argument forms remain accepted.
- On watch timeout, return 6 with `.detached` and no `.exit`. `--attach --expected-job ID` checks exact current identity before any write, takes no new claim, supplies no prompt, rotates nothing, and collects only the recorded task. The printed attach/cancel command includes the expected ID. A stale saved command cannot operate on a reused prefix.
- `--attach --expected-job ID --cancel` invokes the public cancellation API for that exact task. A request/acknowledgment is not a terminal result. Retain the attempt and detached record while termination/admission is uncertain; later attach publishes a terminal result only after delegate seals one. Never signal a workload from this shim.
- Preserve the unchanged review's `--resume-last` invocation through a narrow artifact-bound resolver: for `04-consultation` and `06-resolution`, resolve the exchange participant and its gate-accepted predecessor from the current artifact directory's control metadata. Require a completed delegate-backed predecessor with the matching exact native session; use `dispatch --resume-task PREDECESSOR` to create a new deterministic delegate task. Corrections may use their gate-authorized predecessor in the same session. Store the selected predecessor and session in the new record. Do not invoke provider `--last`, select the most recent repository/global session, or scan unrelated history. Unknown parents, unsupported prefixes, mixed transports, or ambiguous continuity are errors before launch. The existing skill may explicitly authorize its documented fresh fallback; the shim must not invent that fallback itself. Phase 7 acceptance proves the Phase 2 path and this narrow resolver with fixtures; do not claim complete migration of debate/deep-plan continuation semantics.

### Sidecars, gates, and result fidelity

- Keep `.stdout` as the normalized final answer body without rewriting its content, followed only by the existing documented Codex session/resume trailers built from the exact recorded session. A nullable session stays unavailable; do not fabricate a resume link. `.joblog` preserves saved raw provider output. `.stderr` carries transport diagnostics. `.meta` contains control evidence only, including delegate ID, native session, original mode, outcome, timing and immutable result digest.
- Do not publish `.stdout` from partial live output or collect twice from the provider. Read the saved sealed delegate outcome/raw files through the public result API. Under the existing claim publication lease, atomically publish sidecars and `.meta`, then `.exit` last; remove `.detached` only as part of the completed publication. Repeated attach of an already committed outcome is idempotent.
- Preserve exits 0 completed, 1 terminal provider/task failure, 4 definite launch/refusal error, and 6 detached/unfinished observation. The delegate transport does not emit 2 merely for inactivity or 3 for watch expiry. Unconfirmed cancellation must retain unfinished state, rather than write the legacy terminal-looking exit 5; existing readers of old exit-5 records continue to work. Document this transport-specific distinction and update affected gates/tests accordingly.
- Claim release and `confirm-terminated` must query the exact delegate identity. Proven-not-admitted may release a pre-submission claim; a sealed terminal outcome may be collected and then released/rotated under the existing protocol. Unknown admission, unknown termination, or inaccessible delegate retains the claim. Never use companion `status --all`, PID existence, missing process evidence, or status timeout as delegate proof.
- Blindness remains procedural and gated: before join, the calling reviewer reads only allowlisted control fields, never answer/raw/progress last-log content. The transport does not add task brief/result text to status JSON consumed by gates.

### Acceptance and phase exit

Run the sibling's `bash scripts/validate.sh`, its relevant argument/gate suites, and plugin validation from the source worktree. Add focused fake-delegate fixtures for fresh launch, ID/request conflict, lost dispatch response, stale attach, uncertain cancellation, sidecar publication crash, simultaneous collectors, release authority, prefix reuse, exact-session predecessor resolution and unavailable binary. Assert the `SKILL.md` digest and Phase 2 invocation are unchanged. Legacy unset/CCR fixtures must still pass; copied files must remain identical.

Run one bounded actual private review Phase 2 through the source plugin with the opt-in environment: its real skill instruction performs independent fanout, the Codex participant uses delegate, the runner detaches/attaches by exact ID, and only sealed control/results cross the existing join gate. Demonstrate worker survival after collector exit using cooperative finite fixtures, not signals. A real read-only safety probe belongs to the provider certification gate; reuse its evidence rather than claim fresh certification from a review answer. Record private evidence and linked PR SHAs. If the skill cannot use the shim without a skill change, Phase 7 fails until the cause is resolved or scope is explicitly changed.

## Phase 8: resumable execution of a declared DAG

### Public CLI and immutable manifest

The public commands are:

```
delegate [--root ABS] workflow start --manifest FILE [--id 32HEX] --json
delegate [--root ABS] workflow status ID --json
delegate [--root ABS] workflow collect ID --json
delegate [--root ABS] workflow resume ID --json
```

Use `status`/`collect` polling from the calling AI; these commands return durable identities and results to their caller. They do not post to chats or create an automation. An optional bounded watch can be composed from status without becoming a scheduling dependency. `start` returns promptly after workflow/coordinator admission, including uncertainty if needed. A separate internal `delegate-run --workflow ID --epoch N` executes the coordinator; users do not supply internal scheduler ownership arguments.

Version 1 manifest:

```json
{
  "version": 1,
  "concurrency": 2,
  "tasks": [
    {
      "id": "review",
      "needs": [],
      "provider": "codex:exec",
      "brief_file": "briefs/review.md",
      "cwd": ".",
      "permission": "read-only",
      "budget": "30m"
    },
    {
      "id": "summary",
      "needs": ["review"],
      "provider": "claude:print",
      "brief_file": "briefs/summary.md",
      "cwd": ".",
      "permission": "read-only",
      "budget": "30m"
    }
  ]
}
```

The workflow ID is 32 lowercase hex characters, caller-supplied or generated once and returned. Node IDs are unique lowercase slugs. `needs` contains node IDs, not commands or expressions. Require positive bounded concurrency, a nonempty bounded node set, supported provider/permission combinations, valid durations, existing canonical working directories, readable finite brief files within the global input size limits, existing dependency IDs, and an acyclic graph. Unknown fields fail validation. Permission and budget may use the global read-only/30m defaults; no model/effort field is required in this v1.

Resolve `brief_file` and relative `cwd` from the manifest directory. Before coordinator admission, copy every brief's bytes into the workflow record, record its digest, canonicalize the manifest and hash that immutable snapshot. Resume never rereads the source manifest or mutable brief files. Reject a workflow ID reused with different immutable content; identical start is idempotent. Source-file mutation during snapshot creation is detected or rejected, not silently mixed. Runtime provider config is still validated immediately before each task launch.

Dependencies gate execution only. No upstream prose is automatically appended to downstream briefs; no autonomous task invention, replanning, shell actions, model loop, condition language, automatic retries, or review-specific policy is part of v1. A deliberate rerun is a new workflow ID. The caller owns the declared plan and interpretation of its collected results.

### Identity and durable records

The immutable workflow record contains version, workflow ID, canonical manifest hash, canonical inputs, copied briefs, node order, concurrency and creation time. Each child task ID is the first 32 lowercase hex characters of a domain-separated SHA-256 over workflow ID, manifest hash and node ID. Coordinator epoch is deliberately excluded. Check collisions against immutable request hashes through ordinary task admission rather than trusting probability alone.

Maintain versioned control records for coordinator epochs/admissions and node reservations. For each node store its deterministic task ID, immutable task-request hash, eligibility/block reason and known admission state. Delegate remains the authority for admitted child state and sealed outcomes. Do not maintain a second mutable provider outcome model. Never delete or replace permanent workflow lock inodes.

All records, coordinator logs, snapshots and final aggregates live under the selected delegate state root outside allowed provider workspaces. The workflow passes no state-root paths in briefs. Provider workers never inherit coordinator lock descriptors. Ordinary platform auth/session/cache writes remain outside the workspace-operation containment claim.

### Detached coordinator and bounded scheduling

Run the finite coordinator through pueue in a dedicated coordinator group, separate from provider worker groups; otherwise a waiting coordinator could consume all worker slots. Configure enough coordinator capacity for the supported workflow limit and expose queued status honestly. No custom daemon is introduced. Once admitted, a live coordinator keeps scheduling after the caller exits. It exits when the workflow is terminal or the runtime prevents further scheduling; a crash requires explicit `workflow resume` unless a separately verified scheduler restart feature is later adopted.

The coordinator holds a permanent workflow kernel lease for its scheduling lifetime. On acquisition it checks its supplied epoch against durable current epoch before any scheduling or state mutation; stale coordinators exit without admitting a child. Use a distinct bounded admission critical section if a future cancellation operation needs to insert a durable no-new-work barrier. Do not infer ownership from a timestamp, PID, process name, or heartbeat alone.

Reconcile all known children through their deterministic identities before admitting more work. Visit eligible nodes in stable node-ID order. A node is eligible only when every declared dependency has a sealed successful outcome. A sealed failed/cancelled dependency makes descendants durably skipped with the causal dependency recorded; independent branches continue. Unknown dependencies remain unresolved, not failed or skipped.

Reserve a child's deterministic ID/request durably before dispatch. Reserved/admitted nonterminal children, including unknown admission, count against workflow concurrency. Global provider/scheduler limits may further reduce throughput. Dispatch only when there is capacity and the child is absent with proof of no admission, or has an explicit recoverable proven-not-admitted state in the common task protocol. Merely missing a task directory, scheduler row, receipt or stdout is not proof. For a durable reservation interrupted before the admission call, use the common protocol's pre-admission proof; otherwise retain uncertainty. Once dispatch was attempted, reconcile that exact ID: admitted means observe, terminal means reuse the sealed outcome, unknown means never repeat or replace it.

### Resume and epochs

`workflow resume` does not rerun terminal nodes or edit the manifest. It tries the same permanent workflow lease nonblockingly. If held, return the current workflow/coordinator identity as active. If available, reconcile durable state, commit a new coordinator epoch under that lease, and admit a coordinator for that epoch while retaining immutable prior admission evidence. The new process can begin only after the resumer releases the lease.

An unleased queued/unknown old coordinator admission may be superseded by the new epoch: every delayed old coordinator must acquire the same lease, compare epochs and exit before scheduling. This is a narrow exception for the internal scheduler process, whose side effects are fenced by that lease and epoch. It never authorizes replacing or replaying a provider task with unknown admission. If a submission helper can outlive the coordinator, the helper must participate in the epoch/admission protocol until its durable reservation transition is finished; it cannot launch later based solely on a stale in-memory decision. Prefer synchronous public task dispatch with its own exact-ID admission protection and no inherited life lease.

Crash after an epoch commit but before a coordinator receipt can be recovered by another fenced epoch. Crash after a child admission but before a workflow update is recovered by reconciling that exact child ID. Completed child results are reread from immutable outcomes. Unknown provider admission, purged scheduler state or unavailable authority remains visible and occupies its reservation; resume does not translate uncertainty into failure or non-admission. No automatic provider retry occurs.

### Terminal aggregation and collection

A workflow is terminal only when every node has a sealed child outcome or an immutable workflow-local skip record, with no unresolved reservation/admission. Overall success requires all declared tasks to succeed; any failed/cancelled/skipped node yields an unsuccessful aggregate while preserving all independent results. Unknown never becomes a terminal workflow status because a watch expires.

Publish one immutable final aggregate atomically after validating every referenced child outcome. It records workflow/manifest identity, node IDs/task IDs, final state and reason, exact native session where available, normalized result descriptor or null, child outcome/raw digests, and usage with its original scope/availability. Do not sum cumulative session usage as if it were per-turn, estimate unavailable fields, or parse fresh native logs during collect. The aggregate may expose per-turn totals only when every contributing metric has compatible known scope.

`status` emits control state and all known task IDs, including unknown admissions and the coordinator epoch. `collect` before finalization returns a structured nonterminal response, not a partial successful aggregate. After finalization, repeated collect returns the same committed aggregate/results from saved raw only. This is the durable return channel to the calling AI, whether it polls immediately or resumes days later.

### Fixtures, real acceptance, and phase exit

Use deterministic fake-provider fixtures for: manifest validation/cycle/duplicate rejection; copied brief mutation resistance; same workflow ID conflict; stable child IDs across epochs; concurrency with unknown reservations; fork/join success; failed dependency skipping with independent progress; immutable aggregation; nullable outputs; cumulative-usage non-summing; repeated collect; two simultaneous coordinators; delayed stale epoch; and resume against a live lease.

Exercise admission crash windows at explicit cooperative failpoints: epoch committed before enqueue acknowledgment, child reserved before dispatch, child admitted before workflow receipt, child completes before coordinator observes it, and aggregate written before caller receives it. Admission-unknown fixtures must assert no second provider admission and no released concurrency slot. Use helper finite lifetimes/private stopfiles; do not test by sending workload signals or scanning unrelated processes.

Run one isolated real-pueue fork/join workflow with finite fake provider executables to demonstrate separate groups, bounded concurrency, caller-detached scheduling, immutable results and a deliberately exited coordinator recovered by a new epoch. Children admitted before coordinator exit continue; resumed scheduling does not duplicate them. Real native provider smoke coverage is inherited from the adapter phase and one existing bounded private consumer run where relevant, not multiplied into an unbounded DAG test.

Phase 8 is complete only when CLI JSON/schema docs match these commands, `delegate-run --workflow` is packaged, private CI passes the meaningful unit/integration fixtures, isolated pueue evidence is saved, and the calling-AI example starts a declared plan, detaches, resumes observation and collects the exact aggregate. State the honest boundary in user docs: caller detachment is automatic; scheduler crash recovery uses explicit resume; unknown provider work is never automatically repeated.


## Orchestrator reconciliations (precedence over supplement wording)

- The phase numbers are 0 foundation, 1 protocol, 2 supervisor, 3 Antigravity, 4 Codex, 5 Claude, 6 private release, 7 consumer integration, 8 declared workflow. Provider supplement letters A/B/C mean phases3/4/5. The unchanged skill condition applies to the actual review SKILL.md, not all helper scripts or protocol documentation.
- Public task IDs and workflow IDs are exactly32 lowercase hex characters; declared node names match `[a-z][a-z0-9-]{0,63}`. Workflow concurrency defaults3 and accepts1..16; node count1..256; global brief size maximum8MiB. All manifest inputs are snapshotted before coordinator admission. No implicit upstream answer injection.
- Add global `--pueue-config ABS` for initial dispatch/workflow start, with optional DELEGATE_PUEUE_CONFIG as the explicitly documented default. If neither is supplied and no matching stored supervisor binding exists, refuse with configuration error2. Require a real non-symlink supported config, persist its canonical path/digest and resolved endpoint; later status/collect/cancel use saved binding, never current ambient defaults. Live fixtures always supply their own isolated config. Provider/runner executable paths resolve absolutely; installed delegate-run defaults to sibling of delegate; an explicit --runner absolute override may be supplied for source acceptance.
- Safety certification applies to agent-initiated tool effects, excluding ordinary native auth/session/log/cache writes. All enabled permission mappings require controlled execution evidence for their version/platform/profile. Build targets may precede live-supported provider platforms; the release capability table distinguishes them explicitly.
- Real live acceptance is required for each phase's own CLI behavior. Phase8 additionally runs one small actual multi-provider declared workflow (one agy, one Codex, one Claude node, supported profiles, bounded test briefs) to prove the public workflow uses all three installed adapters; existing isolated fake-provider workflow tests prove crash/concurrency cases. Avoid arbitrary extra paid reruns absent changed code or inconclusive evidence.
- The developer can read the private release with authenticated GitHub tooling. No public visibility changes and no secrets added to CI are authorized implicitly. Unit/isolated supervisor CI does not need provider credentials; live provider verification runs on the configured local host and its sanitized receipts state that scope.
- Retain the two original publication demonstrations only as historical evidence if desired; their lack of error assertions must be stated. New implementation tests must fail on wrong payloads/outcomes and injected errors. Runtime guards/predicates are implementation, not merely static lint claims.
- Phases6/7 can require linked or metadata follow-up PRs; each receives the same review/green-CI/squash gate and the phase remains incomplete until all its deliverables and installed/live checks pass. Never self-reference an as-yet-uncreated merge SHA inside release metadata.
