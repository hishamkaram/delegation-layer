# Contributing and extending providers

Contributions should keep the shared task lifecycle predictable while making
provider differences explicit at the adapter boundary.

## Development setup

Install Go 1.27.1, Node.js 22 or newer, and the repository's pinned verification
tools. Node.js is required by `make acceptance-private-cli` because that gate
executes the current build against a Pi-shaped executable fixture and a real
private supervisor on macOS and Linux. This gate checks startup, dispatch,
status, collection, model discovery, and ordinary-task parallelism; it does not
build historical releases. The unit and hermetic acceptance suites provide their own
supervisor fixtures. Clone the repository and run the local checks before
editing:

```sh
go test ./...
go test -race ./...
python3 -m unittest discover -s scripts -p 'test_*.py'
make check
make skill-package-check
make plugin-package-check
```

The repository's `AGENTS.md`, invariant contracts under `agents/_data/`, and
focused skills under `skills/` are the authoritative contributor instructions.
They cover ownership, path and process invariants, concurrency, testing, and
documentation hygiene.

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
Delegate work is also observed and stopped before cleanup is declared complete. Pueue and pueued
must be on the test PATH. Delegate state roots are separate private directories
under `~/delegation-routing-acceptance`, outside provider-writable `/tmp` and
cache directories. A shell that bypasses the PATH fixture cannot establish
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
runtime capability inspection, supervisor admission, process lifetime,
capture, artifact sealing, publication, cancellation, and collection. Do not
add provider-specific process control or raw argument passthrough to the core.

## Adding or changing a provider

Start with the public contract in `internal/provider/contract.go` and the
existing adapters under `internal/provider/`. Keep preparation free of
provider-process launches and writes. It may perform bounded executable
discovery and policy metadata reads; provider version/help probes and other
native inspection work must run through the shared core. Define the help
command and every complete flag the adapter puts in argv so future CLI
releases can be checked dynamically.

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
when required flags exist, and that missing flags, missing executables, and
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
a disposable workspace. `make acceptance-native` covers each supported mode
across all six providers, including AGY read-only, positive unattended writes,
continuation, and replay. Set `CCR_LIVE_MODEL` to a configured CCR alias for
the aggregate; without it, CCR is explicitly blocked. `make acceptance-agy`, `make acceptance-codex`,
`make acceptance-claude`, `make acceptance-pi`, and `make acceptance-opencode`
select individual profiles. A live
gate returns `0` for a pass, `1` for a real behavior or evidence failure, and
`2` for a sanitized `BLOCKED` prerequisite receipt. The aggregate target keeps
blocked profiles neutral so an unavailable login does not block unrelated
development. Report its status and counts alongside the exit code; an aggregate
`BLOCKED` result is not authenticated live proof.

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

The tag workflow creates and verifies a draft release. Promotion is a separate
manual workflow dispatch from protected `main` so npm, GitHub Releases, and the Homebrew tap are
published only after the archive smoke checks pass. Keep `publish_homebrew`
enabled for normal releases and configure the `HOMEBREW_TAP_TOKEN` repository
secret with write access to `hishamkaram/homebrew-tap`. The promotion script
revalidates every platform archive, checksum, and GitHub attestation before it
updates the tap idempotently; the formula remains pinned to the exact promoted
tag for reproducible Homebrew installs. If a tap update fails after the
release becomes public, rerun the same promotion for the current latest release
after fixing the token or transient failure; the npm and Homebrew steps are
idempotent.

## Documentation changes

Product documentation belongs in the README or focused files under `docs/`.
Keep internal planning notes, acceptance evidence, and local experiment output
outside the public documentation tree. Describe behavior that is implemented
and verified today; avoid promising commands, release artifacts, or provider
features that are not present in the current CLI.
