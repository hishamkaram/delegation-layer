# Provider guide

Delegation Layer ships seven launchable provider profiles. The profile name is
part of the task request and determines the command, environment, policy,
identity observer, input transport, output interpreter, and supported options.

| Profile | Permission mode | Read-only mechanism | Request options | Native command |
| --- | --- | --- | --- | --- |
| `antigravity:print` | `read-only`, `workspace-write` | `native-plan` | `continuation`, `model`, `effort`, `native-timeout` | `agy` |
| `codex:exec` | `read-only`, `workspace-write` | `native-sandbox` | `continuation`, `model`, `effort` | `codex exec` |
| `codex:review` | `read-only` | `native-sandbox` | `review-target`, `model`, `effort` | `codex exec review` |
| `ccr:print` | `read-only`, `workspace-write` | `native-plan` | `continuation`, `model` (required alias), `effort` | CCR foreground launch |
| `claude:print` | `read-only`, `workspace-write` | `native-plan` | `continuation`, `model`, `effort` | `claude` print mode |
| `pi:json` | `read-only`, `workspace-write` | `tool-allowlist` plus disabled extensions for read-only | `continuation`, `model`, `effort` | `pi --mode json` |
| `opencode:run` | `read-only`, `workspace-write` | `native-plan` | `continuation`, `model`, `effort` | `opencode run --format json` |

The catalog is available locally with `delegate providers --json`. Its runtime
metadata describes the help arguments and flags required by each adapter.
Its read-only metadata describes the native mechanism selected by the adapter;
the containment value is `provider-owned` for the shipped profiles, so this
mode is not an independent delegate-owned sandbox. Provider-native controls
such as Pi read-only's `--no-extensions` remain part of that provider contract;
they are capability-probed before provider execution rather than treated as delegate
owned policy.
This provider containment metadata is separate from process-lifetime
containment, which is guardian-owned and independently proven.
The catalog uses public request option names; the native flag mapping is
documented in each provider section below.

Agents can request one provider's provider-neutral contract with
`delegate capabilities --provider PROFILE --json`. This command is
side-effect-free and reports `status: "unknown"` until dispatch performs the
supervised runtime probe. It does not claim authentication or live acceptance.

Use `delegate models --provider PROFILE [--cwd ABS] --json` when an explicit
model or effort choice is needed. Model discovery is implemented for all six
profiles. It uses the state-rooted supervisor and the `model-discovery-v1`
five-minute native inspection to allow first-run provider model/cache
initialization; it creates inspection evidence without admitting
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
Missing executables or declared runtime flags, unsupported request options, and
invalid provider results still prevent successful delegation. Provider-specific
launch options are validated by the supervised provider turn so help-output
format changes do not become false admission failures.

## Shared compatibility behavior

Before a task is admitted, the selected adapter resolves a canonical regular
executable and records its static identity and required capability contract.
For managed runtime-only tasks, admission records that runtime verification is
pending. The task's supervised runner performs the runtime probe before the
provider task starts:

1. invokes the provider-declared version command (defaulting to `--version`)
   and accepts any nonempty, valid
   reported version that fits the bounded durable evidence record;
2. invokes the provider help command, including any profile-specific subcommand;
3. checks the stable public flags declared by the adapter; and
4. fingerprints the executable again before the profile is finalized.

This detects missing commands, non-executable files, and incompatible flag
changes without pinning a release number or binary hash. A reported version
is valid when it is trimmed nonempty UTF-8 text without control characters and
fits the bounded durable evidence record; no semantic-version pattern is
required. A newer provider build is accepted when it preserves the command
capabilities the adapter needs.

The runner uses a fresh inspection result to build the launch plan it will
actually execute. A provider can be upgraded or relinked between admission and
launch when the new executable passes the current capability probe. The task's
requested mode, policy, workspace, writable roots, predicate, environment
contract, and input/output declarations remain immutable and are still matched
exactly before the process starts. The final executable, observed version, and
digest are recorded in the started and sealed task evidence.

Native policy projections, custom runners, and historical tasks retain their
admission inspection requirements. Runtime-only tasks do not share capability
observations: every task's fresh preflight is required, even when unchanged
launcher bytes load an updated runtime payload.

The same compatibility rule applies to the private Pueue boundary: delegate
checks the required queue behavior and fields, accepts nonempty observed Pueue
version output and additive status fields, and keeps the bundled artifact pin
only for reproducible packaging.

The observed version is descriptive evidence stored with the task. It does not
authorize a release by itself, and Delegation Layer does not query a web release
list or maintain a semver allowlist. The version-size bound is a durable-record
safety limit, not a release compatibility check; an observation that exceeds it
is rejected before provider execution. Terminal launch evidence
uses the same bound so publication remains durable. The inspection definition and
executable digest bind each capability result to the checked command. The runner
checks the current runtime before launch. Probe failures expose only a bounded reason
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

The shared live gate covers all six providers through the shipped dispatcher
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
written. Authentication blocks require a structured Delegate admission state,
a supported native authentication-status response, or a structured provider
authorization response such as HTTP 401 or 403. An adapter may instead emit a
stable, sanitized reason code from an exact structured envelope;
Antigravity emits `authentication-unavailable`. CCR also emits this code when
its foreground process reports the reserved credential-unavailable exit status
with no Claude output. Pi's default-model control first uses Delegate model
discovery: an explicit empty native available-model catalog is a blocked
prerequisite; malformed discovery remains a failure. An explicitly selected Pi
model still goes through the ordinary provider turn. Free-form provider output is
never used as an authentication oracle. The aggregate
`acceptance-native` target reports passed, blocked, and failed counts. It
keeps authentication blocks neutral for development and still fails on a real
acceptance failure. When no scenario passes, its reported status is `BLOCKED`,
even though the neutral aggregate exit code is zero. Check the reported status
before claiming authenticated live proof. The gate never copies credentials, retries a
provider turn, or reports an authentication refusal as a pass.

OpenCode live acceptance additionally requires `OPENCODE_LIVE_MODEL` to name a
configured tool-capable route. The gate blocks before dispatch when it is
absent; it never guesses a model or treats a provider's no-tool-endpoint
response as a passing result.

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
configuration. Ordinary exec tasks do not inject feature overrides. `--model` maps to Codex's
native model option. `--effort` is encoded as the TOML setting
`-c model_reasoning_effort="VALUE"`; an omitted or `default` effort is not
added to native argv. Continuation uses the exact recorded provider session.

New ordinary tasks work in Git and non-Git directories. Delegate supplies
`--skip-git-repo-check` for both permission modes and exact-session continuation;
the requested native sandbox remains selected. Existing tasks retain their
recorded launch contract when reconstructed or continued after an upgrade.
Native Git-targeted reviews still require a suitable repository.

If a current ordinary task exits with a positive status before emitting any
structured events, collection reports `provider_startup_failed`. This diagnosis
uses sealed process facts and never copies or interprets stderr as a cause.

With `--interactive`, Codex uses its native app-server over core-owned stdio
instead of exec. Delegate enables `default_mode_request_user_input` only for that
invocation, starts or resumes the exact thread, and starts one turn. Native
question batches are exposed through `collect`/`events`; `reply` answers them and
`steer` targets that same active turn. Model and effort use native turn parameters.
The native sandbox and never-ask approval policy remain selected by the requested
permission. No global settings are changed.

Codex tool and web-search payloads are not completion authority. Their unused
metadata, including flattened duplicate tool IDs, is skipped. Thread identity,
turn completion, and the selected agent answer remain validated. The optional
last-message artifact is compared to the selected answer without buffering the
whole answer.

`codex:review` uses the same supervised executable with a distinct completion
contract for Codex's native `exec review` operation. It is always read-only and requires an
explicit `--review-target`: `uncommitted`, `base:BRANCH`, or `commit:SHA`.
The target is persisted with the immutable task request and is translated only
by the adapter into the corresponding native review flag. Review tasks do not
support continuation or active-turn interaction. The required brief remains
task evidence; Codex's target flag supplies the review scope because its native
review command does not accept a prompt alongside that flag.

## Claude

`claude:print` uses print mode and structured streaming output. `read-only` maps
to native `plan` permission mode; `workspace-write` maps to `bypassPermissions`
so authorized edits and commands do not require interactive approval.
Claude loads its own settings, tools, MCP servers, and login. The adapter checks
stream structure, session identity, and result integrity. Continuation selects
the exact recorded provider session.
`--model` and `--effort` pass through to Claude's native flags; an omitted or
`default` effort leaves the provider default untouched.

With `--interactive`, Claude uses its native stream-json control protocol,
`--permission-prompts host`, the SDK's `--permission-prompt-tool stdio` transport,
and invocation-local `CLAUDE_CODE_SDK_READS_SESSION_STATE=1`. The adapter answers
only root `AskUserQuestion` requests. Other permission requests are denied rather
than treated as clarification. This does not change the selected native permission
mode. There is no active-turn steering for Claude. Native background-agent
completion is observed before closing the input channel. Public help checks do
not prove the SDK control protocol; its initialization and structured events are
validated during the task.

## Pi

`pi:json` uses JSON event mode with the native `read,grep,find,ls` tool selection,
`--no-extensions`, and the effective `--session-dir` for `read-only`, so
extension startup and session persistence cannot create project state before a
read-only turn begins. A configured session directory that overlaps the
workspace or Delegate state is rejected before admission. Provider configuration
and authentication remain native. Continuation uses the exact recorded Pi
session in the same workspace and reconstructs the predecessor's recorded
preparation contract, including its effective session directory and historical
flag mapping. `workspace-write` explicitly selects Pi's
built-in `read`, `bash`, `edit`, and `write` tools, so an ambient custom tool
selection cannot remove the capabilities needed for the requested work. Pi
still owns extensions, authentication, and permission behavior. It records `provider-native`
approval and does not add a filesystem sandbox. `model` is passed through, and
`effort` maps to `--thinking` in both modes.

Pi may perform additional internal agent cycles for retries, compaction, or
follow-up work inside the same CLI invocation. New tasks validate their order
and require a successful final result; an unfinished or failed final cycle
cannot publish success. Delegate does not relaunch the provider for these
internal cycles.

For model discovery, Pi lists models available under its native authentication
and provider filters. Its documented empty-list response maps to
`unavailable` / `models_unavailable`, rather than malformed output. A nonempty
listing does not prove remote authorization or quota.

## OpenCode

`opencode:run` uses JSON event mode in the requested workspace. `read-only`
selects the native `plan` agent; `workspace-write` selects `build` with native
`--auto`. OpenCode loads its own configuration, plugins, permissions, and Git
settings. The adapter does not require a Git checkout root or inspect workspace
symlinks. Continuation and model use native options; `effort` maps to `--variant`.

Previously admitted tasks retain their original preparation and output
interpretation contracts. Dispatch a new task to use the simplified native
profiles; upgrading does not silently change a queued task's launch settings.
An ordinary exact-session continuation creates a new task by reconstructing the
predecessor's recorded OpenCode preparation contract, then applying the exact
session continuation. It does not adopt a newer flag or policy mapping merely
because the installed Delegate version changed. Fresh dispatches use the
current mapping, and the predecessor remains immutable.

## Adding a provider

An adapter should expose a small, explicit profile rather than leaking raw
provider arguments into the public CLI. Register its ID, permission modes,
options, help arguments, and stable runtime flags; implement static preparation
and a finalizer; declare input and output artifacts; provide an identity observer; and
register a strict predicate interpreter for the provider's output.

Keep guardian-owned process lifetime and stopping, runner-owned capture, and
core-owned sealing, collection, and publication outside adapters. The provider
owns only its command construction, bounded
environment and policy calculation, identity observation, and output
interpretation. Add unit tests for capability drift and predicate failures,
then document the profile here and in the discovery output. The development
workflow is described in [Contributing](contributing.md).

## Claude Code Router

`ccr:print` launches Claude Code through CCR's foreground provider-only route.
Both `ccr` and `claude` must be installed. Configure providers and credentials in
CCR beforehand. Delegate manages task execution; it does not configure CCR.

Supply `--model` with a configured CCR alias. To choose one, use
`delegate models --provider ccr:print --json`; Delegate reads the tab-separated
output of CCR's `model list` command. Listing does not prove authentication or
model availability. Explicit aliases remain valid inputs even if discovery is
unavailable.
Unknown effort choices remain null; an explicit `--effort` is forwarded to Claude.

Read-only selects `plan`; workspace-write selects `bypassPermissions`. These are
provider-owned permission modes, not an independent sandbox. CCR loads its own
configuration and extensions. Fresh sessions and exact continuations use Claude
session UUIDs, while Delegate records the provider as `ccr:print`. A fresh
dispatch uses the current CCR mapping. An ordinary exact-session continuation
reconstructs the predecessor's recorded CCR preparation revision: current
route-preserving sessions use `--resume-route`, while historical modeled
sessions retain their recorded model/provider-only launch shape. A
route-preserving continuation therefore rejects a model override that would
change the recorded route.

CCR's foreground exit and sealed Claude stream must both indicate success.
CCR reserves exit status `77` for unavailable configured provider credentials.
With an empty sealed stdout stream and no capture error, Delegate reports
`authentication-unavailable`; it does not infer an HTTP status or repair the
credential. Other nonzero exits remain provider failures. Earlier CCR builds
that return a generic exit status retain their generic failure classification.
The requested alias is recorded; it is not independent proof of upstream model
identity. CCR detached jobs are not used by this adapter.

Run the shared native acceptance gate with `--provider ccr:print --model ALIAS`
for each permission mode. Missing authentication is BLOCKED rather than passed.
