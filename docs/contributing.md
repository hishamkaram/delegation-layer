# Contributing and extending providers

Contributions should keep the shared task lifecycle predictable while making
provider differences explicit at the adapter boundary.

## Development setup

Install Go 1.27.1 and the repository's pinned verification tools. The unit and
hermetic acceptance suites provide their own supervisor fixtures. Clone the
repository and run the local checks before editing:

```sh
go test ./...
go test -race ./...
python3 -m unittest discover -s scripts -p 'test_*.py'
make check
make skill-package-check
```

The repository's `AGENTS.md`, invariant contracts under `agents/_data/`, and
focused skills under `skills/` are the authoritative contributor instructions.
They cover ownership, path and process invariants, concurrency, testing, and
documentation hygiene.

Agent integrations should follow the provider-agnostic
[agent-integration skill](../skills/agent-integration/SKILL.md), which uses the
same capability, admission, evidence, authentication, and bounded-output
criteria as the runtime and acceptance tests.

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
across all five providers, including AGY read-only, positive unattended writes,
continuation, and replay. `make acceptance-agy`, `make acceptance-codex`,
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
