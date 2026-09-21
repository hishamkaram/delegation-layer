# Provider guide

Delegation Layer ships five launchable provider profiles. The profile name is
part of the task request and determines the command, environment, policy,
identity observer, input transport, output interpreter, and supported options.

| Profile | Permission mode | Request options | Native command |
| --- | --- | --- | --- |
| `antigravity:print` | `workspace-write` | `continuation`, `native-timeout` | `agy` |
| `codex:exec` | `read-only`, `workspace-write` | `continuation` | `codex exec` |
| `claude:print` | `read-only`, `workspace-write` | `continuation` | `claude` print mode |
| `pi:json` | `read-only` | `continuation`, `model`, `effort` | `pi --mode json` |
| `opencode:run` | `read-only`, `workspace-write` | `continuation`, `model`, `effort` | `opencode run --format json` |

The catalog is available locally with `delegate providers --json`. Its runtime
metadata describes the help arguments and flags required by each adapter.
The catalog uses public request option names; the native flag mapping is
documented in each provider section below.

Agents can request one provider's provider-neutral contract with
`delegate capabilities --provider PROFILE --json`. This command is
side-effect-free and reports `status: "unknown"` until dispatch performs the
supervised runtime probe. It does not claim authentication or live acceptance.

The released binaries target Darwin and Linux. Provider CLIs own authentication,
configuration, MCP servers, hooks, plugins, skills, and native permissions.
Delegation Layer passes the requested native permission mode without inspecting
or rejecting personal configuration. It does not provide an independent sandbox.
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

The Pi and OpenCode live gates use the shipped dispatcher and runner with a
private Pueue instance. They perform a fresh turn, an exact continuation, and
collection replay, then verify provider evidence, session identity, and that
the read-only test leaves the workspace unchanged:

```sh
make acceptance-pi
make acceptance-opencode
make acceptance-native
```

These commands require the selected CLI, a usable native provider login, and
the test supervisor supplied by the development workflow. Released users do
not install a separate supervisor. Exit `0` means the gate passed. Exit `2`
means `BLOCKED`: a required executable, supervisor, or authentication
prerequisite was unavailable and a sanitized `failure.json` receipt was
written. Textual login refusals and
structured provider authorization responses such as HTTP 401 or 403 are both
classified as authentication prerequisites. The aggregate
`acceptance-native` target treats blocked profiles as neutral and still fails
on a real acceptance failure. The gate never copies credentials, retries a
provider turn, or reports an authentication refusal as a pass.

## Antigravity

`antigravity:print` runs `agy` with `--sandbox --mode accept-edits` and the
selected workspace. It passes the brief through standard input.
`--native-timeout` sets agy's own print timeout inside the wall-clock `--budget`.
Native workspace trust, MCP configuration, and account settings remain agy's
responsibility. Missing or expired login is reported by the provider.

## Codex

`codex:exec` invokes `exec` with the requested `read-only` or `workspace-write`
native sandbox and noninteractive approval policy. It preserves native user
configuration without injecting feature overrides. Continuation uses the exact
recorded provider session. Model, effort, and native timeout overrides are not
currently advertised by this adapter.

## Claude

`claude:print` uses print mode and structured streaming output. `read-only` maps
to native `plan` permission mode; `workspace-write` maps to `acceptEdits`.
Claude loads its own settings, tools, MCP servers, and login. The adapter checks
stream structure, session identity, and result integrity. Continuation selects
the exact recorded provider session.

## Pi

`pi:json` uses JSON event mode with the native `read,grep,find,ls` tool selection
for `read-only`. Native extensions and configuration remain enabled; the tool
selection is not an independent sandbox for extensions. Continuation uses the
exact recorded Pi session. `model` is passed through, and `effort` maps to
`--thinking`. This adapter does not advertise `workspace-write`.

## OpenCode

`opencode:run` uses JSON event mode in the requested workspace. `read-only`
selects the native `plan` agent; `workspace-write` selects `build` with native
`--auto`. OpenCode loads its own configuration, plugins, permissions, and Git
settings. The adapter does not require a Git checkout root or inspect workspace
symlinks. Continuation and model use native options; `effort` maps to `--variant`.

Previously admitted tasks retain their original preparation and output
interpretation contracts. Dispatch a new task to use the simplified native
profiles; upgrading does not silently change a queued task's launch settings.

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
