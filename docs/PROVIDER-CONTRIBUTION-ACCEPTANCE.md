# Provider contribution acceptance

Status: local verification passes; direct review and merge gates remain
required. The fifteen-case contributor exercise and three affected agy turns
passed. This receipt records evidence for the contributor handoff in
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

## Executed contributor evidence

The generic boundary was first frozen at `78ad21e` on main `38e3da4`.
Independent review found that declared artifact names could collide with the
scavenger's temporary files. Corrected baseline `9a95018` reserves the `stage.`
namespace for core durability. Input/output/manifest rejection and actual
scavenging followed by sealed replay passed under race; the direct correction
review reported no actionable findings. Final baseline `e7ce3e0` also
makes acceptance-driver test discovery generic, so new provider tests need no
provider-specific Makefile edit. The contributor proof was repeated after
this baseline freeze.

The contributor implementation is limited to
`internal/testutil/contributorprovider/`, its executable fixture driver
`scripts/acceptance_contributor.py` and
`scripts/test_acceptance_contributor.py`, and one registration in
`internal/testutil/contributorcli/catalog.go`. Root separately updates the
receipt, progress documents and guide. The repeated proof needs no application,
task, storage, execution, supervisor or publication exception.

Root review and the guide exercise corrected four provider/fixture issues:

- Continuation needs `PreparedProfile.Identity` to persist a reference through
  the runner-owned callback; returning `Interpretation.Session` alone is
  insufficient. Chunked, bounded, exact and once-only observation is tested.
- Oversized envelopes and conflicting output must produce semantic rejection,
  while actual evidence and sink I/O faults still propagate. Two regressions
  failed before the fix and pass afterwards, including real CLI cases.
- Invalid UTF-8 is rejected before JSON decoding can replace malformed bytes
  with a replacement character. The regression committed an altered answer
  before the fix and rejects afterwards, including the real CLI case.
- Outcome publication precedes completion of session cleanup. The acceptance
  driver requires the exact supervisor row to finish successfully before inspecting it;
  an error-bearing collection remains a failure. Earlier failed receipts are
  retained privately with natural daemon shutdown.

The original contributor-module review identified additional acceptance and
preparation gaps. The final correction checks each case's exact sealed envelope,
artifact, exit code and refusal reason; replay checks the returned outcome and
CLI exit code as well as unchanged payload/raw hashes. Preparation rejects
invalid session grammar, non-private runtime roots/children and briefs beyond
the fixture's admitted limit before launch. Three real negative dispatches
proved no task admission, queue change or provider launch.

The synthetic input bound is 174,677 bytes: `(1 MiB - 512) / 6`, reserving
worst-case JSON escaping and fixed metadata within the existing 1 MiB strict
JSON limit. Near-bound ASCII and escaped answers, stored-session reads and
continuation are covered by unit/race tests. Fresh turns without an explicit
nonce remember their answer; empty remembered state remains valid and can
produce a semantic empty-answer rejection on continuation. The live exercise
includes a near-bound answer and continuation without an explicit nonce.
These corrections affect only the new synthetic module and harness; the frozen
generic baseline remains unchanged. The initial, unmerged predicate contract
now explicitly records these bounds and session grammar in its digest.

The final private receipt `contributor-reviewed-proof` passed fifteen cases:
present, resume, absent, resume-answer, near-bound, empty, conflict, rejected, malformed, wrong-task,
wrong-session, nonzero, oversized-envelope, oversized-output and invalid-utf8. It records
five committed outcomes, ten rejected outcomes, fifteen actual launches,
zero replay launches, unchanged immutable records and natural daemon shutdown.
Replay removes both generated configuration/output staging and the provider
executable before collecting every task again. The fresh task
`79312b8cf2984b61b77c901af68d7c76` continues as `78c6297d508d4bfbbed23606505dbdda`,
with the nonce omitted from the continuation request and the same exact
recorded session returned.

Build the finite synthetic fixture reproducibly without VCS/dirty metadata:

```sh
CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -buildvcs=false -o bin/contributor/provider ./internal/testutil/contributorprovider/cmd/provider
bin/contributor/provider --self-test
CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -buildvcs=false -o bin/contributor/delegate ./internal/testutil/contributorcli/cmd/delegate
CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -buildvcs=false -o bin/contributor/delegate-run ./internal/testutil/contributorcli/cmd/delegate-run
python3 scripts/acceptance_contributor.py --tools bin/contributor --pueue bin/test-supervisor/pueue --pueued bin/test-supervisor/pueued --output <new-private-evidence-directory>
```

Executed platform: Darwin/arm64, Go 1.27.1, pueue/pueued 4.0.4.
Fixture version `contributor-provider 1`; measured binary SHA-256
`3c8d0e26ac6b917da95944251ad593d00cdaed430d707327be96d0d97ff5e85d`.
The executable's self-test passed before updating the embedded certificate.
Other platform/build combinations are intentionally uncertified for live
fixture preparation. Unit tests use explicit identities and do not claim
execution on another platform. This is synthetic certification, not native AI
certification; production discovery contains no synthetic provider.

| Requirement | Concrete verification |
|---|---|
| Declaration validation and legacy bytes | `internal/task/output_contract_test.go`: malformed declarations, reserved names/slots, canonical order, counts and legacy metadata/two-stream bytes |
| Prepared binding and certification | `internal/provider/contract_artifact_test.go`: empty slots, changed content, normalization and matching writer contracts across profiles |
| Ownership, import faults and replay | `internal/taskdir/launch_files_test.go`: consumed permits, private staging, absence, independent inodes, file faults, mutation, write/barrier/close failures, import fencing, scavenging and staging-free replay |
| Launch ordering | `TestLaunchFilesBindBeforeStartAndImportAfterCapture`, changed-declaration permit test and definite-start-failure test |
| New provider semantics | Predicate tests cover strict/bounded envelopes, exact bytes, absence/empty/conflict, identities, refusal/nonzero exit, evidence and sink failures |
| New provider preparation | Profile tests cover exact fresh/resume argv, generated configurations, immutable policy, binary/platform drift and runtime/workspace placement |
| Session persistence | Identity tests cover chunking, exact fresh/resume identity, malformed/mismatched/oversized input, once-only callbacks and persistence errors |
| Acceptance-driver failures | Six hermetic tests reject failed runners, wrong rejection evidence, incorrect replay outcomes and unrelated rows, wait for successful completion and enforce a finite deadline; shared discovery runs all 19 agy/contributor harness tests |
| Executable fixture | Direct `fixture.Run` unit/race tests cover configuration reads, continuation without a supplied nonce, output variants, duplicate tasks and writer failures |
| Full quality | `make check` passed at the corrected contributor boundary: tools, 8 skills/32 validator tests, formatting/config/vet/lint, 43 tooling enforcement cases, full race suite, native-harness tests, build, 12 CLI smoke cases and vulnerability scanning. Subsequent review fixes passed focused provider/CLI race tests, lint and the fifteen-case live exercise; final full-gate rerun and direct correction review remain pending |
| Existing protocol | `make acceptance-protocol`: 49 fault matrix cases and compiled CLI workflow passed |
| Existing supervisor | `make acceptance-supervisor`: 48 hermetic cases and five isolated real-pueue cases passed; native A=S=E=1, K=M=0, natural shutdown |
| Existing native agy | Three turns passed in `agy-20260914T125447Z-5855f5c1`: A4/S3/E3/seal3, policy drift zero launches, workspace positive control/outside denial, exact continuation and native timeout. Later staging-name and synthetic-only fixes do not change agy preparation or its two-stream evidence path |

## Acceptance gates and merge receipt

Required gates are `make check`, `make acceptance-protocol`,
`make acceptance-supervisor`, the synthetic compiled-CLI exercise, and
`make acceptance-agy`. The synthetic command is recorded above. A missing
prerequisite, failed assertion or inconclusive native result is blocked/failed,
never a passing skip. Keep raw private artifacts outside git.

Acceptance requires root review and completed direct Codex CLI review with zero
unresolved actionable findings, including ownership, lifetimes, dependencies,
duplication, compatibility and contributor effort. Record the PR's exact reviewed
head, green CI on that head, squash commit, green main CI, and worktree retirement
receipt before marking this handoff accepted.
