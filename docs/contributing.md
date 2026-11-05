# Contributing

## Development setup

These repository guides describe main; published installs may lag. For a source
build, install the Go release in [`.go-version`](../.go-version) yourself and put
it on `PATH`, with `GOTOOLCHAIN` unset or `local`. Use Python 3.12, Node.js
22.13+ on the 22.x line or Node.js 24+, and repository-local pinned tools. Node is for installer/plugin checks;
Python is for repository tooling and black-box tests, not the Go CLI runtime.
Run `make tools` to install the project's tools under ignored `bin/`; it does
not install Go. `make tools-go` installs only the core Go tools for the platform
checks. Then run `make build`, which requires both that compiler and the
pinned tools checked by `make tool-versions`. Keep the native `bin/delegate`
and `bin/delegate-run` from the same build colocated.

Supply a compatible `pueue`/`pueued` pair beside those binaries, in `../libexec/`
relative to their directory, or through `PATH`. `PATH` lookup selects `pueue`;
`pueued` must sit beside the resolved client, rather than elsewhere on `PATH`.
`make build` does not supply that pair. Runtime compatibility follows required
command/schema behavior; `4.0.4` is the
[test download](../scripts/install-test-supervisor.sh) and
[fixture baseline](../internal/pueue/testdata/README.md), not a runtime version
pin or provider allowlist.

Keep the behavior, affected area, non-goals and done condition in the existing
plan. [AGENTS](../AGENTS.md) owns collaboration/scope defaults; load only the
specialized rule or skill relevant to the work.

## Development checks

Run focused tests while editing. Recognized documentation, skill-content, and
diagram changes use `make check-docs`. Pull requests and pushes to main use the
light content checks only when every changed entry matches the content paths
and ordinary non-executable file modes in the authoritative
[CI selector](../scripts/ci_scope.py). Unknown paths, including historical
fixtures under `internal/`, unexpected modes, runtime, install/build logic, CI
policy, and uncertain or mixed changes use the full project gate on stable source:

```sh
make check
```

It covers Go race tests, Python discovery, architectural/static checks, package
checks, formatting and vulnerability scanning. Do not run those suites again
as separate targets after the aggregate unchanged. `make test-native-harness`
is useful during acceptance-driver edits; its Python tests are already in
`make test-python` and the final aggregate. Required CI runs the complete gate on
Linux and `make check-platform` on macOS. The platform gate keeps Go vet, lint,
builds, race tests, and Python/Node runtime tests on both hosts, including
platform-specific source and behavior. It omits only the shared static checks
already run on Linux. Do not dispatch a second full workflow merely to certify
the first.

Fix actual failures and rerun affected checks. Broaden only for changed inputs
or a concrete concern. A setup failure is blocked, not a passing test. Reviewers
can reuse trustworthy unchanged results. Final independent review covers the
integrated change; do not repeat it separately for every worker. Unrelated
findings are reported separately, not automatically added to the ticket.

### Static analysis and diagnostic pilots

`make tools` also installs Ruff, ShellCheck, actionlint, and the separately
locked JavaScript lint dependencies into `bin/`. `make lint-static` runs the
mandatory language checks; `make verify-static-gates` proves rejection and
acceptance with temporary fixtures. Both are included in `make check`.
CI runs these shared source and workflow checks once on Linux; macOS installs
`make tools-go` and runs `make check-platform`. The default `make check` runs
the shared checks followed by the platform gate.
The source inventory is authoritative: `go run ./cmd/maintainability --sources
python` (or `javascript`, `shell`, `go`) emits absolute NUL-separated paths,
including maintained untracked additions and excluding ignored scratch.

Workflow syntax checks cover all repository workflows except release. The release
workflow remains outside actionlint 1.7.12 because it uses GitHub's supported
`concurrency.queue` property, which that version rejects. No suppression hides
that coverage gap.

Install exploratory tools separately with `make diagnostic-tools`, then run
`make diagnostics`. This checks Python with pinned ty and all workflows with
offline zizmor. The separate [Static diagnostics workflow](../.github/workflows/static-diagnostics.yml)
runs once on Linux for main pushes, pull requests, and manual dispatch. ty still
checks all target platforms; running it on macOS would repeat the same analysis.
Findings intentionally fail that workflow and remain visible; tool exit codes
are not masked. Its result does not change the main CI workflow conclusion or
its release eligibility. Diagnostic failures are outstanding findings, not
clean results. Review and fix real defects before promoting either tool. Do not
hide findings in a baseline or add ignore rules
to make a gate pass. Ruff automatic fixes are disabled; in particular, review
facade consumers before changing an apparently unused import.

Goleak supplements the existing race detector in adopted test packages. Its
shared helper checks after test cleanup and preserves failing exit statuses.
Tests which deliberately transfer work to another process keep their explicit
helper exit path; normal tests must settle owned work before the suite ends.
Neither check establishes native child-process closure by itself.

### Maintainability

The [maintainability policy](../agents/_data/maintainability-contract.md#subsystem-boundaries-and-maintainability-gates)
and [registry](../agents/_data/subsystems.json) own module boundaries and imports.
Review a boundary before changing its permissions. The graph describes source;
it does not approve observed dependencies. Tests and fixtures are maintained
code. Unknown modules, cycles, parser failures and exceeded budgets fail.
Strict mode requires an empty migration inventory and no size exceptions.

Use `make verify-maintainability-strict` for focused architectural checks.
`make fmt` and `make fmt-check` share the audit's maintained-file inventory,
including untracked and build-excluded source, excluding ignored scratch.

Python helpers import canonical implementations under scripts.delegate_acceptance.
Use the existing process/directory owners and patch the namespace the consumer
actually uses. Keep each test in ordinary discovery once; no historical
test-name map or frozen private-signature inventory is required.

## Provider changes

Use the [adapter contract](../agents/_data/adapter-contract.md) and
[add-an-adapter skill](../skills/add-an-adapter/SKILL.md). Provider preparation
and evidence interpretation belong to the adapter; launch, capture, lifecycle
and publication belong to the core. Native help/model inspection uses the
shared execution owner, not hidden commands in pure preparation callbacks.

The [product boundary](../agents/_data/product-boundary.md) owns provider/user
and Delegate responsibilities. The packaged
[model contract](../skills/agent-integration/references/command-reference.md#choosing-models-and-effort)
owns discovery semantics; saved predicates interpret historical task evidence.

Use meaningful envelope/error/identity/replay regressions. For actual provider
behavior use the relevant existing live scenario with a disposable workspace
and existing credentials, not a full paid matrix for an unchanged runtime.
Do not modify global provider settings or credentials for a pass.

## Native and CLI scenarios

The [native-check guidance](../agents/_data/maintainability-contract.md#native-platform-checks)
defines what actual supported-host execution establishes. A fixture is not a
real account; a cross-build or a different kernel is not native runtime proof.

| Target | Purpose |
| --- | --- |
| `make acceptance-private-cli` | Current CLI/private-supervisor bootstrap, dispatch/status/collect and 200-running-task capacity with finite provider fixtures. |
| `make acceptance-private-cli-default-stop` | User and budget stop behavior. |
| `make acceptance-scheduler` | Native ticks and stop isolation with finite fixtures. The driver defaults to `--hosting managed-host` for foreground `scheduler serve`, without an OS service manager. Optional `--hosting service` retains the legacy `scheduler run` service path. |
| `make acceptance-supervisor-handoff` | Inspection handoff while existing external work continues. |
| `make acceptance-supervisor-upgrade-recovery` | Private-supervisor upgrade recovery. |
| `make acceptance-workspaces` | Managed workspace lifecycle, lineage and checkpoint fixtures. |
| `make acceptance-native-lifetime` | Selected native lifetime backend behavior. |

Choose affected scenarios; CI's actual configured checks are visible in its
workflow. The private capacity scenario proves concurrent live owner leases and
scopes, not merely queue depth. Its four dispatch callers are a load-generator
setting, not a product limit or a 200-call burst guarantee.

Verify the user-started scheduler host natively on Linux and macOS.
`scripts/acceptance_scheduler.py` accepts
`--hosting managed-host|service` and defaults to `managed-host`, which needs no
systemd user manager or launchd. Choose `--hosting service` explicitly for the
optional legacy service cases. Keep results from those modes separate; the flag's
presence and finite-fixture coverage do not establish real-provider or recovery
verification.

Use `--scenario fresh-default --hosting managed-host` for the fixture-only
native default-root check. It starts plain `scheduler serve` on absent storage
in an owned, isolated native configuration context, checks readiness with zero
definitions, then uses ordinary no-root commands from different working
directories and observes one finite fixture occurrence. It makes no AI calls
and requires `managed-host`. Select it explicitly: `--scenario all` continues
to run the existing `successive` and `overlap-stop` cases.

With the candidate binaries and fixture supervisors built, run from the
repository root using an absent output directory (change the example output
name for later runs):

```sh
python3 scripts/acceptance_scheduler.py \
  --scenario fresh-default --hosting managed-host \
  --delegate "$PWD/bin/delegate" --runner "$PWD/bin/delegate-run" \
  --pueue "$PWD/bin/test-supervisor/pueue" \
  --pueued "$PWD/bin/test-supervisor/pueued" \
  --output "$PWD/.local/archive/scheduler-fresh-default-results"
```

Use `python3 scripts/acceptance_scheduler.py --help` for the current driver's
scenario and binary-path options.

For the new path, run the actual candidate CLI/runner on Linux and macOS with a
user-started `scheduler serve` in an independent terminal or tmux pane. Verify
child failure followed by owned Wait, backoff, restart and a later scheduled
provider result; stop and terminal signals must fence/join host and child without
respawn while independent tasks retain their lifetimes. During joined stop,
check authenticated status/stop remain responsive while the owned child or a
predecessor holds execution ownership. Completed stop needs the owner-written
closure witness and exact host absence; killing the host before the witness
must not manufacture completion. Preserve authenticated acceptance when later
readback fails. Also check no-host
create/resume preserves its saved ID and start guidance, status stays passive,
explicit task-scope markers and native Linux Delegate cgroup positives refuse
serve, and host loss requires explicit user restart. The guard does not detect
all ancestors or reconstruct guardian ownership; negative marker observations
cannot establish independent placement. Verify the before-Ready callback checks
all current saved definitions and an incompatible definition produces degraded
state with automatic child restart stopped for that fault. After saving,
transport-only unreachability must remain pending exit `3` without a top-level
error; an owner-derived permanent `scheduler.fault: true` must yield exit `1`
and retain `schedule.saved: true`. Check a raw `scheduler run` after an older
managed host stopped: status must report its current `foreground` generation,
and public stop must leave it running with `stop_accepted: false` and
`next_action: stop_foreground_runtime`. End that owned invocation in its original
pane with Ctrl-C. These corrections still require integrated native verification.
Prototype results, simulated clocks and cross-builds do not pass these checks.
Native suspend/resume remains unverified until actually exercised. The hosting
and [upgrade procedure](cli-reference.md#scheduler-upgrades) own operator guidance;
agents must not launch the persistent host inside a bounded task.

Use the [shared workspace boundary](../agents/_data/maintainability-contract.md#managed-workspace-changes)
and [workspace guide](../skills/agent-integration/references/workspaces.md).
The application owns lifecycle; the Lead owns landing changes. Checkpoint
pending/busy classifications use workspaces.PendingError over the whole error,
not a marker match that hides a joined fault.

For Teams changes, [the guide](teams.md) owns user behavior and
[the architecture](architecture.md#optional-team-selection) names catalog,
project-identity and immutable-task owners. Exercise the real SQLite catalog,
CLI stdin/JSON boundaries and saved direct/scheduled task consumers. Keep
no-Team controls, worktree/clone scope controls, revision/tombstone checks and
same-ID/continuation cases alongside new-ID latest-default cases. Validate the
[scheduler stop–update–restart path](cli-reference.md#scheduler-upgrades);
mixed-version readers are not a compatibility target. Fixture checks do not
establish native model availability or live agent routing.

Live profile targets include `make acceptance-agy`, `make acceptance-codex`,
`make acceptance-codex-review`, `make acceptance-claude`, `make acceptance-pi`
and `make acceptance-opencode`. `make acceptance-native` is the explicit full
aggregate, not a prerequisite for every change. CCR_LIVE_MODEL and
OPENCODE_LIVE_MODEL must name configured tool-capable routes for their aggregate
cases. AGY_LIVE_MODEL is optional; absent means the provider's native default.
Explicit model IDs pass unchanged. A resolved executable does not prove auth.

Live drivers report pass, failure or blocked prerequisites separately. An
aggregate with unavailable profiles is not proof they authenticated. Preserve
task outcomes, real cleanup and diagnostics; no development-proof branch,
registration or proof upload is needed. Keep raw private output off public CI.

The CCR foreground fixture uses real Delegate/CCR/Claude binaries against a
local HTTP fixture, without account credentials or paid requests:

```sh
PATH="$PWD/bin:$PWD/bin/test-supervisor:$PATH" python3 scripts/acceptance_ccr_fixture.py
```

It covers overlapping sessions, writes, stops, continuation, aliases and replay.
This proves the fixture integration, not an upstream account's availability.

## Agent integration and skill selection

The [agent-integration skill](../skills/agent-integration/SKILL.md) owns external
delegation guidance. Claude/Codex plugins package that skill and a bootstrap
hook; they do not install provider CLIs or rewrite authentication. Keep the
canonical and plugin skill trees synchronized. Use `make plugin-package-check`
when either distribution changes.

For affected routing changes, build/use the actual CLI and run focused cases
through scripts/acceptance_skill_routing.py. It uses fresh sessions and natural
requests without naming Delegate or preloading the skill. Normal competing
integrations stay enabled, so a bypass can execute paid work. Only run against
a trusted authorized test profile. Fixture routing and real Delegate execution
are different modes and claims.

Use an explicit new private output path and preserve the selected options:

```sh
python3 scripts/acceptance_skill_routing.py --harness claude \
  --split development --output "$HOME/delegation-routing-output"
```

The driver also supports Codex/Hermes initiators, explicit model/case selection
and an explicit CLI path. Hermes needs the identical candidate installed in its
global skill directory; missing setup is blocked. Claude/Codex candidate skills
use disposable workspaces; user settings stay unchanged. A competing higher
precedence personal skill can block discovery and must not be silently removed.

Review complete transcripts for actual skill loading, Delegate invocation,
preserved destination/options and direct provider/plugin/API bypasses. Native
same-harness alternatives are permitted where the case allows; separate CLI
sessions use Delegate. Negative local cases must avoid unwanted delegation.
Text promising Delegate is insufficient. Review results are recorded by the
existing routing driver, not a contributor ledger: its judgment.json binds
status, receipt_sha256 and transcript evidence. Summarize with the same options:

```sh
python3 scripts/acceptance_skill_routing.py --harness claude \
  --split development --output "$HOME/delegation-routing-output" --summarize
```

Exit 2 is unreviewed/blocked/incomplete, not success. Preserve failed attempts;
do not rerun until failures disappear or claim a selected case proves all
models/plugins. Holdout prompts cease to be unseen once used for tuning.
No full routing matrix is required when the integration surface is unchanged.
The driver's owned private supervisor/roots must close before cleanup; their
queue state never replaces task guardian evidence or authorizes shared-parent
deletion. SSH/tmux launch PATH and auth can differ from an interactive shell.

## Release publication

Full main CI runs build/package once and test installation of those files. After
a content-only main push, run main CI manually with `build_candidate=true` for
full build/install checks before publishing that exact commit. A light content
run supplies no distribution artifact or native-runtime proof.
The main-only Release workflow publishes its selected immutable distribution
artifact by artifact_id; it does not rebuild, repack or regenerate the npm
package or formula. Keep the source package version aligned with the release.
Allowing the package job on main is not direct development or automatic publish.

PR distribution jobs build and install the PR head commit, rather than GitHub's
temporary merge commit. They run for same-repository PRs; fork PRs retain the
ordinary gates. Main builds always run the distribution install checks before
their artifacts can be published.

Publication validates the trusted workflow/run/source, successful build/install
chain, artifact digest/expiry/archive contents and version before mutation.
Reruns use full job history so unchanged successful jobs can be reused, while
a superseded or unproven artifact refuses. Existing matching delivery steps
are skipped on explicit retry; conflicting bytes refuse. Existing resume mode
keeps its restricted destination behavior. No first-attempt rule, freeze,
readiness status, evidence branch or local dispatch receipt is required.

Normal development does not publish. Before merge, real build/install checks
and publisher tests with fake clients/real archives validate the path without
publishing. A real hosted publication is verified only when an authorized
release runs. Missing or expired artifacts need an explicit new build/test
cycle; the publisher never starts it silently.

Before removing a retired required status from branch protection, obtain that
settings authorization and preserve all other checks. Do not post a fake
Development readiness success or use an unapproved admin bypass.

## Repository skills

Each skills/name/SKILL.md uses this deliberately small frontmatter:

```text
---
name: lower-case-hyphenated-name
description: one non-empty single-line description
---
```

Only name/description are allowed, once each. Name matches the directory,
uses lowercase letters/digits/single hyphens and is at most 64 characters.
Unquoted values begin with an ASCII letter; quote YAML-like scalars or a colon
followed by whitespace. Explain the trigger, work and useful result without
a mandatory body-heading template.

Use relative inline repository links; angle brackets support paths with spaces.
Balanced parentheses are allowed; reference links and link titles are not.
External URLs are useful sources, not substitutes for relevant owning rules.
Authority links target the section the skill actually needs, not every contract.

The verifier recognizes one-target make commands in inline code or command
lines. Targets must exist; compound commands/options in those references are
unsupported. A future command is explicitly marked planned on the same line.
Do not leave unfinished scaffolding. `make verify-skills` checks metadata,
links, commands and optional evaluation structure; it does not run a model.

An optional skills/name/evals/cases.json declares schema_version 1, the skill
name and nonempty cases. Each case/control has an ID, visible prompt/inputs
and hidden expected actions/artifacts plus rationale. IDs are unique and never
leak into visible text. Inputs are synthetic. Both sides get the same bounded
assignment and differ only in the fact/constraint being tested. Grade behavior,
not keywords; a hypothetical exercise is not runtime evidence or merge approval.
The existing scripts/skill_eval_cases.py owns the format and closed vocabulary.
Paid model evaluations require their own authorization; no new evaluation
framework or contributor receipt format is introduced.

## Documentation changes

Keep product documentation in README/docs and local plans/private diagnostics
under ignored .local/archive. Describe implemented behavior and actual checks;
mark future or unavailable behavior honestly. Use Archify full-diagram exports,
not cropped screenshots, when a diagram changes. Do not update illustrations
or rerun their native/provider scenarios merely for unrelated contributor prose.
