# Provider guide

Delegation Layer ships five launchable provider profiles. The profile name is
part of the task request and determines the command, environment, policy,
identity observer, input transport, output interpreter, and supported options.

| Profile | Permission mode | Request options | Native command |
| --- | --- | --- | --- |
| `antigravity:print` | `read-only`, `workspace-write` | `continuation`, `model`, `effort`, `native-timeout` | `agy` |
| `codex:exec` | `read-only`, `workspace-write` | `continuation`, `model`, `effort` | `codex exec` |
| `claude:print` | `read-only`, `workspace-write` | `continuation`, `model`, `effort` | `claude` print mode |
| `pi:json` | `read-only`, `workspace-write` | `continuation`, `model`, `effort` | `pi --mode json` |
| `opencode:run` | `read-only`, `workspace-write` | `continuation`, `model`, `effort` | `opencode run --format json` |

The catalog is available locally with `delegate providers --json`. Its runtime
metadata describes the help arguments and flags required by each adapter.
The catalog uses public request option names; the native flag mapping is
documented in each provider section below.

Agents can request one provider's provider-neutral contract with
`delegate capabilities --provider PROFILE --json`. This command is
side-effect-free and reports `status: "unknown"` until dispatch performs the
supervised runtime probe. It does not claim authentication or live acceptance.

Use `delegate models --provider PROFILE [--cwd ABS] --json` when an explicit
model or effort choice is needed. Model discovery is implemented for all five
profiles. It uses the state-rooted supervisor and the `model-discovery-v1`
60-second native inspection; it creates inspection evidence without admitting
a provider task.
The current directory is used when `--cwd` is omitted. Discovery is advisory,
never an admission allowlist, and provider defaults remain valid.

The response distinguishes unknown effort metadata (`null`) from an explicit
empty choice list (`[]`). `complete` refers to model enumeration; it can be
true while harness-wide or per-model effort metadata is null. Harness-wide
effort choices are not assumed to apply to every model, and listing a model
does not prove the account can execute it. `available` and `partial` exit 0,
`blocked` and `unavailable` exit 2, and `failed` exits 1.

The released binaries target Darwin and Linux. Provider CLIs own authentication,
configuration, MCP servers, hooks, plugins, skills, and native permissions.
Delegation Layer passes the requested native permission mode without inspecting
or rejecting personal configuration. `workspace-write` requests unattended native
execution, including command execution; native approval bypass may permit access
beyond the workspace. It does not provide an independent sandbox.
Missing executables or required flags, unsupported request options, and invalid
provider results still prevent successful delegation.

## Shared compatibility behavior

Before a task is admitted, the selected adapter resolves a canonical regular
executable and records its observed identity. A supervised runtime probe then:

1. invokes the executable's version command and accepts any nonempty, valid
   reported version;
2. invokes the provider help command, including any profile-specific subcommand;
3. checks that every complete flag used by the adapter is advertised; and
4. fingerprints the executable again before the profile is finalized.

This detects missing commands, non-executable files, and incompatible flag
changes without pinning a release number or binary hash. A reported version
is valid when it is trimmed nonempty UTF-8 text without control characters; no
semantic-version pattern is required. A newer provider build is accepted when
it preserves the command capabilities the adapter needs.

The observed version is descriptive evidence stored with the task. It does not
authorize a release by itself, and Delegation Layer does not query a web release
list or maintain a semver allowlist. An unusually large version observation is
omitted from the bounded JSON projection while the exact validated value stays
bound in task metadata. The inspection definition and executable digest bind
the capability result to the exact command that was checked; the runner
repeats the check before launch. Probe failures expose only a bounded reason
class, such as a missing required flag or executable identity drift, and never
provider output or process diagnostics.

The probe does not replace provider behavior validation. Each adapter still
requires its expected output shape, task and session identity, authentication
result, and output artifact integrity. The recorded effective configuration binds
the selected native mode and executable, not the contents of provider settings.

When dispatch returns a capability projection, `status: "ready"` means the
shared probe observed the required behavior and executable identity for that
admission boundary. A live acceptance receipt remains separate: authenticated
proof is `passed`, and unavailable authentication is `blocked` and neutral.

## Live acceptance

The shared live gate covers all five providers through the shipped dispatcher
and runner with a private Pueue instance. Each supported mode performs a fresh
turn, an exact continuation, and collection replay. Read-only scenarios verify
that the workspace is unchanged. Write scenarios verify file creation, editing,
and shell-generated output using unpredictable test values:

```sh
make acceptance-agy
make acceptance-codex
make acceptance-claude
make acceptance-pi
make acceptance-opencode
make acceptance-native

# One selected provider/mode:
python3 scripts/acceptance_native.py --provider pi:json --mode workspace-write --scenario write

# AGY read-only mode:
python3 scripts/acceptance_native.py --provider antigravity:print \
  --permission read-only --scenario read-only
```

These commands require the selected CLI, a usable native provider login, and
the test supervisor supplied by the development workflow. Released users do
not install a separate supervisor. For an individual gate, exit `0` means it
passed. Exit `2`
means `BLOCKED`: a required executable, supervisor, or authentication
prerequisite was unavailable and a sanitized `failure.json` receipt was
written. Textual login refusals and
structured provider authorization responses such as HTTP 401 or 403 are both
classified as authentication prerequisites. The aggregate
`acceptance-native` target reports passed, blocked, and failed counts. It
keeps authentication blocks neutral for development and still fails on a real
acceptance failure. When no scenario passes, its reported status is `BLOCKED`,
even though the neutral aggregate exit code is zero. Check the reported status
before claiming authenticated live proof. The gate never copies credentials, retries a
provider turn, or reports an authentication refusal as a pass.

## Antigravity

`antigravity:print` supports both permission modes. Read-only uses exactly
`agy --sandbox --mode plan` and does not include a bypass flag. Authorized
workspace-write uses `--sandbox --mode accept-edits` with the existing
`--dangerously-skip-permissions` bypass and records effective approval as
`always-proceed`. It passes the brief through standard input; keeping
`--sandbox` does not establish a delegate-owned boundary for every tool or
sandbox escape.
`--model` maps to agy's native model flag and `--effort` to its native effort
flag. An omitted effort leaves agy's default untouched.
`--native-timeout` sets agy's own print timeout inside the wall-clock `--budget`.
Native workspace trust, MCP configuration, and account settings remain agy's
responsibility. Missing or expired login is reported by the provider.

## Codex

`codex:exec` invokes `exec` with the requested `read-only` or `workspace-write`
native sandbox and noninteractive approval policy. It preserves native user
configuration without injecting feature overrides. `--model` maps to Codex's
native model option. `--effort` is encoded as the TOML setting
`-c model_reasoning_effort="VALUE"`; an omitted or `default` effort is not
added to native argv. Continuation uses the exact recorded provider session.

## Claude

`claude:print` uses print mode and structured streaming output. `read-only` maps
to native `plan` permission mode; `workspace-write` maps to `bypassPermissions`
so authorized edits and commands do not require interactive approval.
Claude loads its own settings, tools, MCP servers, and login. The adapter checks
stream structure, session identity, and result integrity. Continuation selects
the exact recorded provider session.
`--model` and `--effort` pass through to Claude's native flags; an omitted or
`default` effort leaves the provider default untouched.

## Pi

`pi:json` uses JSON event mode with the native `read,grep,find,ls` tool selection
for `read-only`. Native extensions and configuration remain enabled; the tool
selection is not an independent sandbox for extensions. Continuation uses the
exact recorded Pi session in the same workspace. `workspace-write` leaves the
native tool selection to Pi, including its default shell, edit, and write tools;
custom configuration can change that selection. It records `provider-native`
approval and does not add a filesystem sandbox. `model` is passed through, and
`effort` maps to `--thinking` in both modes.

Pi may perform additional internal agent cycles for retries, compaction, or
follow-up work inside the same CLI invocation. New tasks validate their order
and require a successful final result; an unfinished or failed final cycle
cannot publish success. Delegate does not relaunch the provider for these
internal cycles.

## OpenCode

`opencode:run` uses JSON event mode in the requested workspace. `read-only`
selects the native `plan` agent; `workspace-write` selects `build` with native
`--auto`. OpenCode loads its own configuration, plugins, permissions, and Git
settings. The adapter does not require a Git checkout root or inspect workspace
symlinks. Continuation and model use native options; `effort` maps to `--variant`.

Previously admitted tasks retain their original preparation and output
interpretation contracts. Dispatch a new task to use the simplified native
profiles; upgrading does not silently change a queued task's launch settings.
An explicit continuation creates a new task using the current mapping for its
inherited permission mode, while retaining the exact provider session and
leaving its predecessor immutable.

## Adding a provider

An adapter should expose a small, explicit profile rather than leaking raw
provider arguments into the public CLI. Register its ID, permission modes,
options, help arguments, and required flags; implement static preparation and a
finalizer; declare input and output artifacts; provide an identity observer; and
register a strict predicate interpreter for the provider's output.

Keep process lifetime, stopping, capture, sealing, collection, and publication
in the shared core. The provider owns only its command construction, bounded
environment and policy calculation, identity observation, and output
interpretation. Add unit tests for capability drift and predicate failures,
then document the profile here and in the discovery output. The development
workflow is described in [Contributing](contributing.md).
