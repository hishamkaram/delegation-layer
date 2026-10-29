# Contributing and extending providers

Contributions should keep the shared task lifecycle predictable while making
provider differences explicit at the adapter boundary.

## Development setup

Install the exact Go release named in the root [`.go-version`](../.go-version)
file, Python 3.12, Node.js 22 or newer, and the repository's pinned
verification tools. `make tool-versions` requires `go version` to report that
release with `GOTOOLCHAIN` unset or `local`. Node.js is used by the npm and
plugin package checks.
`make acceptance-private-cli` executes the current build against a Pi-shaped
executable fixture and a real private supervisor on macOS and Linux. This gate
checks startup, dispatch, status, collection, model discovery, and
ordinary-task parallelism; it does not build historical releases. The unit and
hermetic acceptance suites provide their own supervisor fixtures. The private
inspection-journal handoff is covered by `make acceptance-supervisor-handoff`
on Linux and native macOS CI. It uses fixture providers and real supervisors
to check automatic and explicit handoff while existing external work continues;
it does not use provider credentials or claim live model acceptance.
`make acceptance-workspaces` runs the managed workspace lifecycle and lineage
scenarios through the current `delegate` build, a real private supervisor, and
the finite private fixture provider; CI runs it on Linux and native macOS. It
is fixture-only and does not launch a real provider. The private
CLI capacity proof establishes 200 counted tasks Running while live provider
leases, durable native-scope registrations, and contended lifetime-owner locks
overlap. Its four concurrent dispatch callers are a recorded load-generator
setting, not a product task limit or a 200-call burst requirement; running
capacity and dispatch throughput remain separate measurements. The Python
orchestration unit tests validate receipt and gate ordering with fakes; they are
never capacity evidence. Only the executable private CLI gate may make the
200-running-task claim. Its process-level status burst must reduce the number of
queue snapshots below the number of callers. Exact one-flight behavior is
proved separately by the deterministic package test, which holds the first
snapshot open until every counted caller has crossed the observation boundary;
a process-launch barrier alone does not establish that overlap.

Clone the repository and run `make tool-versions` to confirm the pinned
toolchain. Checks then follow the contract's [check and review
stages](../agents/_data/maintainability-contract.md#check-and-review-stages):
focused tests while editing, `make verify-maintainability-strict` and the
applicable checks before peer review, and the full gate on the final combined
snapshot:

```sh
make check
```

`make check` already runs `make test-race`, `make verify-maintainability-strict`,
`make skill-package-check`, and `make plugin-package-check`. It does not run
`make test-native-harness`; also run that target when a change touches the
native acceptance harness or its tests. Its discovery commands differ from
`make test-python`, and the native acceptance targets depend on it.

The repository's `AGENTS.md`, invariant contracts under `agents/_data/`, and
focused skills under `skills/` are the authoritative contributor instructions.
They cover ownership, path and process invariants, concurrency, testing, and
documentation hygiene.

## Maintainability checks

The [maintainability contract](../agents/_data/maintainability-contract.md)
defines subsystem ownership, dependency direction, source budgets, and the
evidence required for a change. The [subsystem
registry](../agents/_data/subsystems.json) records each module's owner, layer,
and exact allowed imports, including test and fixture modules. Update the
relevant fragment after reviewing a changed boundary. The checker reports
observations; it never grants new dependency permissions.

Run `make verify-maintainability-strict` before peer review. It checks maintained
Go, Python, JavaScript, scripts, and documentation, including untracked files
and Go files excluded by the host's build tags. Unknown modules, unused
permissions, dependency cycles, parser failures, and exceeded budgets fail the
gate. `make check` includes this strict target. A passing size check alone does
not establish correct ownership; review the affected consumers and state
transitions too.

`make fmt` and `make fmt-check` use that same maintained-file inventory,
including untracked source and excluding ignored scratch and proof snapshots.

The migration inventory is only for staging this refactor against unchanged
baseline bytes. Strict mode requires it to be empty. Changed source cannot use
an old exemption, and finished changes must not leave exceptions behind.

Python acceptance entry scripts import canonical implementations under
`scripts.delegate_acceptance`. Import the same implementation namespace from
helpers and tests; patch the module where a function is actually looked up.
Use the shared process registry and directory owner instead of adding another
subprocess or cleanup implementation. When moving tests, keep each case in one
discovered module and reconcile its old and new IDs across both discovery
commands in `make test-native-harness`.

Agent integrations should follow the provider-agnostic
[agent-integration skill](../skills/agent-integration/SKILL.md), which uses the
same capability, admission, evidence, authentication, and bounded-output
criteria as the runtime and acceptance tests.

The Claude Code and Codex plugin under `plugins/delegation-layer/` is a
packaging layer around that skill. Codex uses the supported
`.codex-plugin/plugin.json` compatibility manifest so its session-start hook
is discovered by current Codex releases; do not add a root Agent Plugin
manifest unless the Codex hook loader changes. The hook only verifies or
bootstraps the self-contained Delegation Layer CLI through the checked-in
release installer. It does not install provider CLIs or modify provider
authentication. Keep the plugin skill synchronized with the canonical skill
tree and run `make plugin-package-check` after changing either one.

## Skill selection acceptance

Build the candidate CLI before routing acceptance. The driver defaults to
`bin/delegate`, records its digest, version and provider catalog, and keeps that
candidate on the initiating harness's shell path. Use `--delegate-bin ABS` for a
specific packaged candidate; it must not silently fall back to an installed CLI.
Add `--routing-context` to exercise the shipped context helper with native hook
configuration. Codex uses reviewed, invocation-scoped hook trust for this test;
that is not proof of ordinary installation or persisted host trust. Summaries
must use the same binary and routing-context options as their captures.

The routing suite tests natural requests without naming Delegate or preloading
its skill. It keeps the user's normal harness profile and competing plugins.
The runner installs the candidate skill in disposable Claude/Codex workspaces;
it never rewrites user settings. An older personal Claude skill with the same
name blocks the attempt because it takes precedence over the project candidate
([Claude skill precedence](https://code.claude.com/docs/en/skills#resolve-skills-that-share-a-name)).
Hermes requires an already installed, identical
candidate under `HERMES_HOME/skills/agent-integration`; otherwise its receipt is
blocked, because project-local discovery is not supported by the tested build. Run only against a trusted test profile:
Claude and Codex initiators use unattended permissions without a filesystem
sandbox so the Delegate supervisor can operate. Child task permissions still
come from the agent’s dispatch request. Routing mode puts a recording Delegate
wrapper on the test PATH. The wrapper runs the real Delegate parser and admission
with a private PATH containing only the test supervisor executables, so provider
work is unavailable. CCR model discovery supplies one controlled alias after
valid CLI parsing; request errors and stop-required failures retain their real
responses. Competing integrations
remain enabled, so a routing bypass can still execute paid work; inspect those
attempts as failures. Use `--mode live` for real Delegate execution.

```sh
python3 scripts/acceptance_skill_routing.py --harness claude \
  --split development --output /tmp/skill-routing-claude
```

Use `--harness codex` or `--harness hermes` for the other initiating harnesses,
`--model` for an explicit model, and `--case` for a focused probe. Each case
runs three times by default in a fresh workspace/session. Outputs are private
local evidence and must not be committed. Each attempt runs under an isolated Pueue supervisor with a 180-second deadline
(`--budget-seconds` overrides it). Only that private queue may be stopped; the
runner uses the existing acceptance process/evidence helpers. It replaces any
inherited `DELEGATE_PUEUE_CONFIG` with that attempt’s private queue, so nested
   Delegate work is observed before cleanup is declared complete. The routing
   harness pauses new scheduling and removes only rows that never started. It
   never signals running work; an unresolved row fails cleanup and retains the
   evidence output and every owned root. It removes roots only after all rows
   end naturally and the private Pueue daemon shuts down. Real Delegate task
   records retain their own guardian evidence; the outer routing fixture's
   Pueue state is not guardian empty-scope proof. Pueue and pueued
must be on the test PATH. Delegate state roots are separate private directories
under `~/delegation-routing-acceptance`, outside provider-writable `/tmp` and
cache directories. Each disposable root is identity-pinned with its parent at
creation; cleanup rejects replacement and never removes that shared parent. A
shell that bypasses the PATH fixture cannot establish
routing-only proof; inspect the recorded fixture calls and use live mode when
needed. The receipt distinguishes routing fixtures from live execution; fixture
results are never live-provider proof. The runner returns `2` until reviewed;
a successful harness exit alone cannot establish correct selection. Missing
executables and authentication are blocked prerequisites, not passing skips.

Review each complete transcript, including nested tool calls. For positive
cases, require the candidate skill to be loaded, actual Delegate commands,
preservation of named destinations, and no direct external-provider CLI,
plugin, or API bypass. An unsupported destination should be reported from the
catalog without dispatching a substitute. Cases with `native_alternative_for` also permit native delegation in that
initiating harness; a separate CLI session still requires Delegate. For
negatives, require no unwanted external delegation. Text saying “I will use Delegate” is insufficient.
Record the actual model from the transcript and competing integrations present.

Beside each receipt, write `judgment.json` with `status` (`passed`, `failed`, or
`blocked`), `receipt_sha256` (SHA-256 of that receipt), and `evidence` (transcript
line/tool-event references and the observed reason). Review the full transcript
for bypasses, including provider plugins called through a native subagent.
Then summarize the exact same case selection:

```sh
python3 scripts/acceptance_skill_routing.py --harness claude \
  --split development --output /tmp/skill-routing-claude --summarize
```

Summaries return `0` only when every selected repetition has a passing reviewed
receipt, `1` when any attempt failed, and `2` for incomplete or blocked evidence.
They reject changed transcripts, stale judgments, and mismatched candidate,
model, mode, repetition, or budget selections. Summarize with the same options used to run. Summaries of selected
cases are focused evidence, not full-matrix acceptance.

The original twenty cases retain their twelve development and eight validation
prompts. Six CCR cases add alias selection, writes, continuation, comparison,
and a non-delegation control. Freeze validation prompts before tuning. Run the full suite on the final candidate; retain all attempts rather
than rerunning failures until they disappear. If validation informs another
skill revision, disclose that it is no longer an unseen holdout. Review routing
and live task evidence separately: real review, authorized write, and comparison
or follow-up tasks need Delegate task IDs and collected outcomes. Skill-only
routing cannot guarantee precedence over every model or competing plugin.

## Managed workspace boundary

Managed Git workspaces are optional and must leave ordinary non-Git delegation
unchanged. Use the canonical [W1–W7 workspace transition
rows](../agents/_data/maintainability-contract.md#managed-workspace-transitions)
and the [workspace reference](../skills/agent-integration/references/workspaces.md)
instead of copying lifecycle rules into adapters, scripts, or skills. The
application coordinator owns workspace use; Git subprocesses run through the
existing supervised execution boundary. Auxiliary operation deadlines do not
renew provider tasks, and exact native closure plus the managed-worker
disposition precede capture and cleanup. Owned native proof controls cleanup;
the Lead owns landing changes.

Git checkpoints follow rows CP1–CP7 of the same matrix. Keep admission,
qualification, removal ranking, and pending classification in
`internal/workspaces`; the `checkpoint` command projects acknowledgements and
descriptions and decides no lifecycle policy. Classify pending from the whole
returned error with `workspaces.PendingError`, as the [whole-error
rule](../agents/_data/maintainability-contract.md#whole-error-pending-classification)
requires. A checkpoint change pairs accepted-pending with refused-busy
responses and a joined concrete fault through the public command.

## Adapter boundary

An adapter is responsible for:

- registering a stable profile ID, supported modes, options, help arguments,
  and required runtime flags;
- declaring native model discovery when the profile can report model IDs or
  effort metadata, while preserving unknown effort data as null;
- preparing a command, bounded environment, canonical writable roots, policy,
  and input/output declarations;
- observing the provider session identity; and
- interpreting the provider's output with a strict predicate.

The shared core remains responsible for request validation, task storage,
runtime capability inspection, supervisor admission, and lifecycle policy.
The application lifetime state machine owns native-lifetime transition policy
for both the live guardian and recovery. The execution runner owns provider
`Start`/`Wait`, capture, and local handle release; task storage owns durable
evidence. Do not
add provider-specific process control or raw argument passthrough to the core.

## Adding or changing a provider

Start with the runtime-facing contract in `internal/provider/contract.go`,
the pure facts under `internal/provider/contract/`, and the existing adapters
under `internal/provider/`. Keep preparation free of
provider-process launches and writes. It may perform bounded executable
discovery and policy metadata reads; provider version/help probes and other
native inspection work must run through the shared core. Define the help
command and the stable runtime flags the adapter can observe reliably. Keep
rapidly changing or pipe-truncated forwarded options in the provider launch
and live acceptance contract rather than turning them into a stale admission
allowlist.

When an output format changes, add a versioned predicate interpreter only when
the old format must remain collectible. Preserve strict task/session identity,
containment, policy, and result-artifact checks. Update the catalog description,
provider guide, troubleshooting notes, and meaningful unit tests together.
Model discovery is an advisory projection, not a model allowlist. Its projector
must distinguish model-enumeration completeness from effort metadata, preserve
null versus an explicitly empty effort list, and avoid inferring per-model
compatibility from names or harness-wide choices. The shared core owns the
inspection process, `model-discovery-v1` five-minute bound for first-run provider
model/cache initialization, state root, and supervisor evidence; the
adapter only projects bounded nonsecret facts.

## Validation expectations

Use fake executable fixtures to prove that a newer arbitrary version is accepted
when declared runtime flags exist, and that missing flags, missing executables, and
non-executable files are rejected. Test malformed or conflicting output at the
predicate boundary, including model discovery status, null effort metadata,
empty effort lists, duplicate model IDs, and exit-code mapping. Run the
provider's live acceptance command only with a real signed-in account and a
disposable workspace; it must not modify account credentials or global
settings.

Before opening a change, run `gofmt` (or the repository's `make fmt`), the full
unit and race suites, script tests, `make check`, and
`make skill-package-check`. Review the resulting diff for private paths,
account identifiers, raw provider output, and stale release or compatibility
claims. The package version must stay aligned with the release tag used by the
release workflow.

When native provider credentials are available, run the relevant live gate with
a disposable workspace. `make acceptance-native` covers the shipped native
profiles, including `codex:review`, AGY read-only, positive unattended writes,
continuation, and replay. Set `CCR_LIVE_MODEL` to a configured CCR alias for
the aggregate; without it, CCR is explicitly blocked. Set `OPENCODE_LIVE_MODEL`
to an explicitly configured tool-capable OpenCode route; without it, OpenCode
is explicitly blocked before dispatch. Set `AGY_LIVE_MODEL` to an exact AGY
model ID to pass it to both AGY cases; leave it unset or empty to use AGY's
native default model. `make acceptance-agy`, `make acceptance-codex`,
`make acceptance-claude`, `make acceptance-pi`, and `make acceptance-opencode`
select individual profiles. A live
gate returns `0` for a pass, `1` for a real behavior or evidence failure, and
`2` for a sanitized `BLOCKED` prerequisite receipt. The aggregate target keeps
blocked profiles neutral so an unavailable login does not block unrelated
development. Report its status and counts alongside the exit code; an aggregate
`BLOCKED` result is not authenticated live proof.
Use `make acceptance-codex-review` for the native read-only review profile by
itself.

Model discovery is a separate acceptance entry so the task matrix does not
repeat native discovery for every mode. Run it once per provider when validating
the discovery command:

```sh
python3 scripts/acceptance_native.py --provider antigravity:print \
  --permission read-only --scenario read-only --discover-models
```

The harness records the JSON observation and applies the command's `0`, `1`,
and `2` status mapping. A run must produce its own receipt before it is treated
as live evidence; documentation and tests do not imply that a live run has
already passed.

The CCR foreground fixture runs real Delegate, CCR, and Claude binaries against
an isolated local HTTP provider, without account credentials or paid requests:

```sh
PATH="$PWD/bin:$PWD/bin/test-supervisor:$PATH" python3 scripts/acceptance_ccr_fixture.py
```

It proves overlapping sessions, successful and budget-stopped continuation,
file editing and shell execution, cancellation, rejected aliases, observational
replay, and upstream connection cleanup. The `CCR integration` workflow runs it
on Linux and native macOS. This fixture proves CLI integration, not third-party
account availability; keep authenticated acceptance receipts separate.

## Release promotion

A release publishes one immutable, unpublished development candidate. The bytes
that the native, installation and review proofs exercised are the bytes that
reach GitHub, npm and the Homebrew tap. No tag push builds anything, and nothing
is rebuilt, repacked, reinstalled or provider-tested after publication.

Authenticated provider acceptance runs locally on a maintainer's existing host,
using its existing provider logins. Do not register that host as a GitHub
Actions runner or copy provider credentials into CI. Provider setup remains
owned by the [provider guide](providers.md).

### Candidate pipeline

Each step uses an existing entry point; `--help` lists its exact options. The
candidate store is the `development-proof/evidence` branch, defined by
`PRODUCTION_STORE` in
[`native_release/contract.py`](../scripts/delegate_acceptance/native_release/contract.py).
The record commands take its four `--store-*` values; `run-candidate` takes the
branch, genesis and marker values and reads the repository from the locator.

| Step | Entry point | Result |
|---|---|---|
| 1. Build | [`ci.yml`](../.github/workflows/ci.yml) `development-candidate` job, dispatched on the task branch | Payload and locator artifacts |
| 2. Register | `scripts/record_candidate.py register` | Locator and manifest in the store |
| 3. Native proof | `scripts/record_candidate.py intent`, then `scripts/release_native.py run-candidate` | Native attempt on each planned Linux and macOS platform |
| 4. Install proof | `scripts/prove_candidate_install.py`, then `scripts/record_candidate.py install` | Shell, npm and Homebrew proof on each planned platform |
| 5. Review | `scripts/record_candidate_review.py` | Source review of the candidate's change scope |
| 6. Readiness check | `scripts/record_candidate.py check` | Local canonical verdict; appends nothing |
| 7. Freeze | `scripts/record_candidate.py freeze` | Freeze commit F in the store |
| 8. Trusted readiness | [`development-readiness.yml`](../.github/workflows/development-readiness.yml) | `Development readiness` status on the pull request head |
| 9. Merge | Required CI, then squash merge | Merge commit M on `main` |
| 10. Publish | `scripts/release_native.py publish-candidate` | GitHub release, npm package, tap formula |

Build the candidate from the task branch. The version must equal the
`package.json` version, and the base is the exact `main` commit the branch
builds on:

```sh
gh workflow run ci.yml --ref TASK_BRANCH -f build_candidate=true \
  -f candidate_version=1.2.3 -f candidate_base_commit=BASE_COMMIT
```

The job retains the payload (four platform archives, checksums, the npm TGZ,
the Homebrew formula and `candidate-manifest.json`) and, separately, the
locator for 90 days. The manifest binds those eight subjects by digest; the
locator binds the manifest. Every later step reads that payload through the
locator.

`run-candidate` takes the locator, the store values, the recorded attempt and
the model options described under [Provider models](#provider-models). Use
`resume --receipt PATH` to recover an interrupted attempt; it never reruns
provider cases. `prove_candidate_install.py` proves exactly one channel
(`shell`, `npm` or `homebrew`) on the host that runs it. Run Homebrew proof only
on a fresh, disposable Homebrew host: that installation is retained. Each proof
runs one local fixture task under a short private directory below `HOME`; if your
home path is too long for the private supervisor socket, pass an existing short
directory with `--bootstrap-parent`.

To prove all six hosted legs without touching your own host, add
`-f prove_candidate_installs=true` to the build dispatch. After the build, the
`development-candidate-install` job reads the build job's locator and runs
`prove_candidate_install.py` on `ubuntu-24.04` (amd64) and `macos-15` (arm64)
for each channel; the Ubuntu Homebrew leg uses the runner's preinstalled
Homebrew. Each leg uploads `receipt.json`, `install-proof.json` and
`install-witness.json` as `candidate-install-OS-CHANNEL-RUN_ID-RUN_ATTEMPT`.
Import a pair with `record_candidate.py install` only when the leg's receipt
reports a passed, cleaned-up proof; an exported pair alone is not success.

Before opening or updating the pull request, run the readiness check from a
clean checkout of the reviewed head:

```sh
python3 scripts/record_candidate.py check \
  --store-repository hishamkaram/delegation-layer \
  --store-branch development-proof/evidence \
  --store-genesis-commit d3582a5613e0194899170b5034833d1b88ddaa09 \
  --store-marker-blob 8b5bcb36f5acbed53dcef51220d7056643ed70b9 \
  --candidate-id CANDIDATE_ID --review-sha256 REVIEW_SHA256 \
  --repository-root "$PWD" --receipt /absolute/path/to/new-receipt.json
```

Exit `0` means ready, `1` refused or not ready, `2` usage error and `3`
ambiguous. A ready verdict is an observation, not a freeze, merge or
publication grant. Freeze uses the same review and checkout.

After the freeze, dispatch the trusted readiness observation from `main`. It
reads the production store and posts the status on the pull request's current
head:

```sh
gh workflow run development-readiness.yml --ref main \
  -f pull_request=PR_NUMBER -f candidate_id=CANDIDATE_ID -f freeze_commit=F
```

Merge only after required CI passes. Publication rechecks the required checks,
the readiness run and the merged tree: M's tree must equal the tree the
candidate was built from.

**First installation.** The readiness workflow runs only from `main`, so it
cannot observe the pull request that adds it until that pull request merges.
Complete every affected development, native, installation and review proof, and
a ready `check`, before that first merge. A readiness run after the merge is an
after-merge observation; it is not retroactive pre-merge proof and does not
waive a missing proof.

### Publication

Readiness may run while the repository is private. Actual publication requires
a confirmed public repository before any destination write; no command or
workflow changes visibility.

From a clean checkout of the current protected `main` commit, with Python 3.12
and an authenticated `gh` CLI, run:

```sh
python3 scripts/release_native.py publish-candidate \
  --candidate-id CANDIDATE_ID --freeze-commit F --merged-commit M \
  --readiness-run-id RUN_ID --readiness-run-attempt RUN_ATTEMPT \
  --resume-published false
```

The command prints its receipt path. It runs the shared preflight, records the
dispatch intent, dispatches [`release.yml`](../.github/workflows/release.yml)
once and observes that exact run. The trusted-main job alone writes
destinations, in order: draft release and assets, the tag and public GitHub
release, the exact npm TGZ, then the exact tap formula. The npm package
installs that version's archives from the public release, so npm never names a
version before its release is public. The job uses the
repository secrets `NPM_TOKEN` and `HOMEBREW_TAP_TOKEN`; the tap token needs
write access to `hishamkaram/homebrew-tap`. The workflow's
`release-promotion` concurrency group queues runs (`queue: max`) and never
cancels one in progress, so publications across all tags are serialized.

Only when it creates the draft does the job ask GitHub to generate release
notes, a read-only request that saves nothing, for the merged commit since the
latest release it just checked. The draft is titled with its tag. Its body is
those notes followed by a collapsed **Build verification details** block that
holds the exact canonical promotion text. The notes are presentation only:
maintainers may edit them on GitHub, but the block must stay byte-exact and
last. A release whose block differs, or with text after it, is refused and
never rewritten. A release holding only the canonical text, as before notes
existed, is still accepted. Existing drafts and releases are never regenerated.

To edit the notes, use `gh release edit TAG --notes-file FILE`, where `FILE`
holds the complete body: the edited human text, then the unchanged block as
its last bytes, ending with the single newline after `</details>`. Change only
the text before the block. Whether an edit in the GitHub web form keeps the
block's line endings byte-exact is unverified, so such an edit may be refused.

Exit `0` is `PUBLISHED`: the hosted report passed its validator with exact
bindings and the local receipt is finalized. Exit `2` is `BLOCKED`: nothing was
dispatched, or the hosted run failed before any mutation. Exit `3` is
`INCOMPLETE`: the outcome is unresolved or a mutation may have started.

The publication code revision (the `main` checkout running the command) is
separate from the readiness run's own harness revision and from the store head.
The store head may advance only while the candidate, freeze, review,
publication-source and scope bindings, and fresh canonical readiness, stay
valid. The preflight owns those rules.

To recover after an interruption, observe the same invocation:

```sh
python3 scripts/release_native.py resume-publication --receipt /absolute/path/to/receipt.json
```

Recovery only observes the run uniquely bound to that invocation. It never
dispatches again, including after an unknown dispatch acknowledgement. Only the
first run attempt counts; do not rerun the hosted job. If nothing was ever
dispatched, recovery reports `BLOCKED`; start a new `publish-candidate`.

If npm and the GitHub release already match the candidate but the tap did not
finish, start a new `publish-candidate` with `--resume-published true`. It
mutates neither npm nor GitHub; it verifies them and may finish the tap.

If the GitHub release is already published but npm did not finish, start a new
`publish-candidate` with `--resume-published false`. It verifies the matching
published release, which must still be GitHub's latest, and skips it; it then
may publish the exact npm TGZ once and finish the tap. Nothing retries or rolls
back a partial publication automatically.

After publication, checks cover delivery, integrity and provenance only. The
validated delivery report records the original pull request head and tree and
the merged commit M. The hosted job publishes the candidate's existing bytes;
it neither builds them nor creates new attestations.

### Provider models

The model options are exact configured values, not suggested model names.
`--opencode-model` is required. Omit `--ccr-model` when no alias is available:
those cases are explicitly prerequisite-blocked. A supplied alias must pass
unchanged; a passing control never erases another submitted failure.

Launch native commands from an existing logged-in shell in which `command -v`
resolves each selected provider executable, including `claude` for CCR cases.
An explicit SSH or tmux launch command may inherit a different `PATH` from your
interactive shell, so run the check in the exact launch context before paid
work. A resolved executable does not prove authentication.

`--agy-model configured-model-id` is optional. It passes that exact
caller-selected AGY model ID to both AGY cases; when omitted, AGY uses its
native default model. A pass with an explicit model proves only that
configuration. It is not evidence for AGY's native default model or for any
other model ID.

For a candidate, only an unavailable credential or executable is neutral, and
only in a full-matrix scope for a provider the change did not affect. An
authentication block is never a pass. An interrupted native phase remains
incomplete; it is not an authentication block. Failed or unresolved native work
retains its private evidence, and a failed attempt stays in the candidate's
history: record its triage with `record_candidate.py triage`; a later attempt or
candidate does not erase it. A `BLOCKED` case keeps the exact bytes of its
`failure.json` as the private `native/receipts/<case-id>.failure.json` after raw
cleanup; it is never staged or published, and it may hold provider error text,
so do not print or upload it. Keep the checkout unchanged while a native command
runs: changing its captured source closure invalidates the proof.

### Legacy tag-based native receipts

`release_native.py run --tag` and its `resume`, `release-native-acceptance.yml`
and `validate_native_release.py` remain supported for existing tag-based native
receipts and historical releases. They do not authorize the candidate pipeline.
The current `Release` workflow no longer builds a draft on tag push or accepts a
tag and `native_validation_run_id`. Archive attestations and their readers
belong to those historical releases; they are not provenance of a candidate.

```sh
python3 scripts/release_native.py run --tag v1.2.3 \
  --opencode-model provider/model --ccr-model configured-alias
python3 scripts/release_native.py resume --receipt /absolute/path/to/receipt.json
```

Run from the current protected-main checkout with an authenticated `gh` CLI.
`--no-submit` permits local harness testing; its proof authorizes nothing.
Resume never reruns provider cases. Projection resumes from the saved source
digests and stage-directory identity, and does not rewrite source receipts.
Only the versioned public proof ZIP is submitted. It contains bounded case,
option, session, lifetime, cleanup and artifact facts; it excludes host paths,
raw command arguments, environment values and provider output.
`release-native-acceptance.yml` validates it as **maintainer-attested native
execution validated by CI**; hosted CI does not execute the authenticated
provider cases. An admitted failure for a tag stays in that tag's history; a
passing rerun or a new tag does not hide it. Attempt reports request 90-day retention ([GitHub retention limits](https://docs.github.com/en/organizations/managing-organization-settings/configuring-the-retention-period-for-github-actions-artifacts-and-logs-in-your-organization)).

To check a legacy proof locally, use a clean checkout of the current
protected-`main` commit and a new, empty absolute work directory:

```sh
mkdir /absolute/path/to/new-empty-dir
python3 scripts/validate_native_release.py promotion --tag v1.2.3 \
  --repo hishamkaram/delegation-layer --source-revision CURRENT_MAIN_SHA \
  --native-validation-run-id RUN_ID --work-dir /absolute/path/to/new-empty-dir \
  --github-auth local-config
```

`--github-auth local-config` removes the token variables from the `gh`
environment and uses `gh`'s own stored login; it never reads or forwards a
stored token. The command changes nothing on GitHub. Its result is legacy
native validation only, not a publication preflight.

## Documentation changes

Product documentation belongs in the README or focused files under `docs/`.
Keep internal planning notes, acceptance evidence, and local experiment output
outside the public documentation tree. Describe behavior that is implemented
and verified today; avoid promising commands, release artifacts, or provider
features that are not present in the current CLI.
