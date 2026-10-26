# Provider guide

Delegation Layer ships seven launchable provider profiles. The profile name is
part of the task request and determines the command, environment, policy,
identity observer, input transport, output interpreter, and supported options.

| Profile | Permission mode | Read-only mechanism | Request options | Native command |
| --- | --- | --- | --- | --- |
| `antigravity:print` | `read-only`, `workspace-write` | `native-plan` | `continuation`, `model`, `effort`, `native-timeout` | `agy` |
| `codex:exec` | `read-only`, `workspace-write` | `native-sandbox` | `continuation`, `model`, `effort`, `interactive` | `codex exec` |
| `codex:review` | `read-only` | `native-sandbox` | `review-target`, `model`, `effort` | `codex exec review` |
| `ccr:print` | `read-only`, `workspace-write` | `native-plan` | `continuation`, `model` (required alias), `effort` | CCR foreground launch |
| `claude:print` | `read-only`, `workspace-write` | `native-plan` | `continuation`, `model`, `effort`, `interactive` | `claude` print mode |
| `pi:json` | `read-only`, `workspace-write` | `tool-allowlist` plus disabled extensions for read-only | `continuation`, `model`, `effort` | `pi --mode json` |
| `opencode:run` | `read-only`, `workspace-write` | `native-plan` | `continuation`, `model`, `effort` | `opencode acp` |

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
side-effect-free and reports the catalog contract with `status: "unknown"`.
Runtime checks happen inside the supervised task or an admission inspection,
as described in [architecture](architecture.md#ownership-boundaries). Static
discovery does not claim authentication or live acceptance.

Use `delegate models --provider PROFILE [--cwd ABS] --json` when an explicit
model or effort choice is needed. Six of the seven launchable profiles support
model discovery; `codex:review` does not. It uses the state-rooted supervisor and the `model-discovery-v1`
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
Delegation Layer passes the requested native permission mode. It does not
inventory personal configuration; a provider contract can read narrow startup
state needed to make that mode effective. For example, Pi read-only preparation
resolves `sessionDir` from settings when no explicit session directory was
supplied, and rejects malformed settings or an overlapping location. `workspace-write` requests unattended native
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
recorded launch contract when reconstructed after an upgrade. A continuation is
a new task: it keeps the recorded profile, predicate, and eligibility but takes
its filtered launch values, writable roots, and executable lookup from the
caller's current environment.
Native Git-targeted reviews still require a suitable repository.

Codex's native sandbox can deny staging or committing in a linked worktree
because its Git metadata is outside the checkout. Managed capture still records
file changes without a commit. The Lead can explicitly run Git operations with
`workspace exec`; Delegate does not widen Codex's sandbox to shared repository
metadata.

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

Claude can finish background work after the caller's turn and answer each
completion with a synthetic follow-up turn in the same invocation. The
published answer is the first result, which must answer the caller's turn: its
`origin` is absent or `human`. Later results are accepted only when every
result carries consecutive `result_index` values and each later result has
`origin.kind` `task-notification` without a `subkind`. Follow-up answers are
never published. A failed caller result stays rejected. A failed follow-up, a
repeated `system/init` with a different session or permission mode, another
origin such as a scheduled trigger or peer message, a missing or duplicate
counter, or an unfinished follow-up at EOF also rejects. On every result,
`terminal_reason`, when present, must be `completed`, and `queued_turn_count`,
when present, must be `0`; older CLI releases that omit these fields remain
accepted. The documented `max_turns`, `api_error`, `aborted_streaming`, and
`aborted_tools` terminal reasons reject as provider failures. Under the current predicate
revisions, an unrecognized `terminal_reason` or `stop_reason` string of at most 128 bytes rejects as
`unsupported-completion` with diagnostic `unsupported_completion`; it is never
success, a provider failure, or malformed output. An explicit null, mistyped,
or oversized `terminal_reason` is malformed. An empty follow-up is accepted
only as a coalesced batch acknowledgement with `num_turns` `0`, and a later
non-empty follow-up must answer that batch before EOF. Main-agent usage comes
from the caller's result; the cumulative cost comes from the last accepted
result and is never summed.

A resumed session can also answer old background completions before it
reaches the caller's turn, with leading acknowledgements: results with
`origin.kind` `task-notification` and no `subkind`, `subtype` `success`, an
empty string `result`, `num_turns` explicitly `0`, and `result_index` values
that start at `0` and are consecutive. They are accepted after the initial
`system/init` and before any root assistant or user message since that init or
the previous acknowledgement, and one repeated `system/init` with the same
session and permission mode is permitted once after each, before root
messages. When acknowledgements precede
the caller's result, that result must carry the next consecutive
`result_index`. An acknowledgement is never the answer, adds no usage or cost,
and never closes an interactive input. A failed, refused, aborted, or queued
acknowledgement always rejects. It reports its own refusal even if the
caller's result succeeds or never arrives, unless a higher-priority malformed or
incomplete fault takes precedence, such as malformed evidence or an interactive
stream that is unfinished at EOF; the task is rejected either way. A stream with
only acknowledgements is incomplete,
and a nonempty, missing, mistyped, or wrongly counted acknowledgement, or an
unmatched repeated `system/init`, is malformed. Frozen revisions, including
the retained unscoped `native-permissions-v4` (read-only),
`native-permissions-v5` (workspace-write), and `interactive-v4`, keep
rejecting this prefix.

Only root messages take part in the follow-up cycles. A complete assistant or user
message whose `parent_tool_use_id` is absent or `null` is a root message, and
after a result it opens a follow-up cycle that its attributed result must close.
A message with a nonblank string `parent_tool_use_id` (at most 1024 bytes, no
NUL, valid UTF-8) belongs to a nested agent. It stays structurally validated
and bound to the session, but it never opens, advances, or settles a cycle or a
batch, is never an answer, and does not affect accounting. Any other value
(empty, blank, oversized, not a string, or a case-aliased key) is malformed, before
or after the caller's result. The id itself is opaque and is not tracked.

The current `claude:print` revisions are read-only `native-permissions-v5`
and workspace-write `native-permissions-v6`. They use an advisory wire:
`session_id` and the native permission mode are the only `system/init`
authority. Other init metadata, such as the CLI version, `cwd`, model, tools,
and MCP servers, is advisory, is never defaulted, and may change on a repeated
init. Duplicate keys reject among the selected authority and control fields;
opaque tool, content, and extension payloads are validated as JSON syntax only,
and unknown content block types are opaque.

Tasks sealed under earlier predicate revisions keep their recorded
interpretation. The retained scoped revisions are the read-only
`native-permissions-v4` digest `b3c26a3eae4e39d6a68f55e191b7ac2aa7f05476103ee2d111de0fd873880f0d`
and the workspace-write `native-permissions-v5` digest
`27bf2e76be4185559ecaa8cfbd404d81744281298b6a29b6bf99ab4e1f4b1b29`. They keep
the strict wire and the identical repeated-`system/init` rule they were sealed
under. Each is replay-only and is never a fresh default. An offline recheck
reassesses a scoped revision, like each other eligible earlier revision,
directly under the current revision of its mode without changing the original
outcome.

The retained unscoped revisions share those version strings and differ by
digest: the read-only `native-permissions-v4` digest
`f7c1053edce279071b9caa46f727cf196d5998fc05d07afdf9c5378acd9eb25c` and the
workspace-write `native-permissions-v5` digest
`333a689a864dfb03ecd4a6eace97bcf6964fea96768ddbbb86a92d98c4a7a76b`. Both were
admitted before message scope existed and treat every message after the
caller's result as a root message. Each is replay-only, is never a fresh
default, and is not a recheck source.

A new continuation task is distinct from reconstruction of its predecessor.
The predecessor's task record, metadata, and launch plan are never modified and
keep rebuilding under their recorded predicate, including each retained
digest above. In an ordinary noninteractive continuation, only the new
successor task selects the current predicate of the same mode and launch shape,
and only when the predecessor is a native print record sealed under the
pre-attribution revision (read-only `native-permissions-v3`, workspace-write
`native-permissions-v4`) or under the retained scoped or unscoped digest of its
mode. Other records passed through this successor hook retain their recorded
predicate. Interactive continuation instead builds the current interactive
profile in the caller's current environment while preserving the exact recorded
session. A successor takes its filtered launch values, writable roots, and
executable from the caller's environment, not the predecessor's. A legacy native
OAuth record derives a fresh policy proof from the caller's inspection instead
of keeping the predecessor's. Reconstruction of an existing task always retains
its recorded predicate and environment.

With `--interactive`, Claude uses its native stream-json control protocol,
`--permission-prompts host`, the SDK's `--permission-prompt-tool stdio` transport,
and invocation-local `CLAUDE_CODE_SDK_READS_SESSION_STATE=1`. The adapter answers
only root `AskUserQuestion` requests. Other permission requests are denied rather
than treated as clarification. This does not change the selected native permission
mode. There is no active-turn steering for Claude. Native background-agent
completion is observed before closing the input channel, and the input stays
open while a follow-up turn or batch acknowledgement awaits its answer. The
published interactive answer follows the same caller-result and message-scope
rules; a nested agent's `tool_result` never resolves a root question. Input
closure is sticky, but the stream completes only when the session is idle again
with no unsettled agent, question, cycle, or batch. Follow-up turns that arrive
after input closure remain evidence. `interactive-v6` is the current revision
and applies the same advisory wire, repeated-`system/init`, and
`unsupported-completion` rules as the current print revisions.
`interactive-v5` is retained for replay under its scoped attribution and
strict wire. `interactive-v4` is retained for replay under its unscoped
attribution (every message after a result opens a follow-up cycle) and its
original end-of-stream rule (input closure plus the principal result, with no
idle re-check of the session). `interactive-v1` through `interactive-v5`
recheck under `interactive-v6`. Public help checks do
not prove the SDK control protocol; its initialization and structured events are
validated during the task.

## Pi

`pi:json` uses JSON event mode with the native `read,grep,find,ls` tool selection,
`--no-extensions`, and the effective `--session-dir` for `read-only`, so
extension startup and session persistence cannot create project state before a
read-only turn begins. A configured session directory that overlaps the
workspace or Delegate state is rejected before admission. Provider configuration
and authentication remain native. Continuation uses the exact recorded Pi
session in the same workspace and keeps the predecessor's recorded launch
profile and historical flag mapping. The successor derives its session
directory, writable roots, and executable from the caller's current environment
instead of the predecessor's. `workspace-write` explicitly selects Pi's
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

`opencode:run` keeps its profile name and launches `opencode acp`, OpenCode's
[Agent Client Protocol](https://opencode.ai/v2/docs/cli/acp) agent, which
speaks JSON-RPC 2.0 over standard input and output. Fresh tasks record profile
revision `opencode-acp-v1` and predicate `acp-v2`. The adapter passes no other
argument. The process starts in the canonical workspace, and the workspace,
agent, model, effort, and session travel in protocol requests. The runtime probe
validates `opencode --version` and requires `opencode acp --help` to succeed;
no ACP flag allowlist is required. Help proves no protocol
behavior, so every task validates the handshake, each configuration
acknowledgement, and the terminal response.

The execution core owns the standard input and output transport; the adapter
only encodes and interprets protocol frames. It sends one request at a time:
`initialize` with protocol version 1, then `session/new` for a fresh task.
Next, `session/set_config_option` sets the native mode, then an explicit model,
then an explicit non-default effort. Each option must be acknowledged before
the next request. Finally, one `session/prompt` carries the brief. `read-only`
selects the native `plan` mode and `workspace-write` selects `build`. The
adapter answers each native permission request for the task session with
`reject_once` in read-only mode and `allow_once` in workspace-write mode. A
write-mode request that offers no `allow_once` option rejects the task. The
client advertises no filesystem, terminal, elicitation, authentication, or
child-session capability. OpenCode loads its own configuration, plugins, MCP
servers, authentication, permissions, and Git settings. Its permission mode is
provider-owned, not an independent sandbox. The adapter does not require a Git
checkout root or inspect workspace symlinks.

`--model` and `--effort` are sent unchanged as the native `model` and `effort`
options. The model comes first because OpenCode's effort option belongs to the
current model. An omitted or `default` effort sends nothing. OpenCode decides
whether a value is valid: an option it does not acknowledge rejects the task.

Root `agent_message_chunk` updates received during the prompt form message
runs. A run is a sequence of root chunks that name one `messageId`, compared
by its decoded string; the empty string is a valid ID. Thoughts, tool calls,
plans, usage, other update kinds, and updates tagged as OpenCode child sessions
never end a run, and child updates are excluded from the answer. The answer is
the ordered root text of the final run. A nontext chunk counts toward its run,
so a final run without nonwhitespace text rejects as empty; an earlier
preamble is never used instead. When any root chunk has no `messageId` or a
null one, the answer is all root text of the prompt and the
`answer_message_boundary_unavailable` diagnostic is reported. When the final
`messageId` also named an earlier, separated run, the same whole-prompt
answer is used with `answer_message_boundary_ambiguous`. A duplicated
`messageId`, or one that is neither a string nor null, rejects the sealed
answer as invalid output; it never stops the live protocol. Success requires a
`session/prompt` response with `stopReason` `end_turn`, one matching session
identity, and non-whitespace selected answer text. Any other stop reason, a
JSON-RPC error, an unacknowledged option, or a session mismatch rejects. Usage
from the terminal response is reported with unknown scope.
OpenCode does not support `--interactive`. Its protocol channel carries no
user clarification or steering and writes no interaction journal.

Continuation resumes the exact recorded session with `session/resume` in the
task workspace. The agent must advertise the resume capability, and a resume
response that names another session rejects. The adapter never uses
`session/load`, which replays earlier messages. It never falls back to
`session/new` when resume is refused or unavailable.

Previously admitted tasks retain their original preparation and output
interpretation contracts. An ACP record admitted under `acp-v1` keeps that
predicate for reconstruction and replay, its answer remains all root text of
the prompt, and it is never rechecked under `acp-v2`. An exact-session
continuation of an `acp-v1` or `acp-v2` predecessor resumes the recorded
session in the caller's current environment, with writable roots derived there,
and uses `acp-v2`; an ACP record with any other predicate is refused. Records of the historical
`opencode run --format json` profiles keep their recorded argv, runtime flag
probe, and run predicate for reconstruction and replay. This covers the
`--standalone` native profile, the earlier native profile with `--dir` and
`--variant`, and the adapter-owned pure-mode profile. An offline recheck of
run predicate revisions 2 through 4 uses run revision 5; an ACP predicate is
never a recheck target for a run record. An ordinary exact-session
continuation of a historical native run predecessor creates a new ACP task.
That task resumes the same recorded session, using the caller's current
environment and derived writable roots, under the current ACP launch,
capability, and output contracts, when the caller's current profile is
compatible. Pure-mode predecessors keep their recorded profile and predicate
but also launch in the caller's environment. The predecessor
record and its outcome, including a failed outcome, never change.

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
keeps the predecessor's recorded CCR preparation revision and requires a
predecessor that recorded its environment: current route-preserving sessions
use `--resume-route`, while historical modeled sessions retain their recorded
model/provider-only launch shape. The successor launches in the caller's current
environment. A route-preserving continuation therefore rejects a model override
that would change the recorded route. A historical modeled `--model` alias or
provider-only record resolves the alias through the caller's configuration and
does not promise a retained route.

CCR's foreground exit and sealed Claude stream must both indicate success.
Fresh CCR tasks use the current `ccr-foreground-v6` revision, with the same
advisory wire and Claude follow-up attribution rules as `claude:print`. The
retained `ccr-foreground-v5` revision keeps its recorded multi-result
interpretation under the scoped Claude attribution policy and strict wire;
`ccr-foreground-v4` and earlier keep their recorded single-result
interpretation. An ordinary exact-session continuation of a `v4` or `v5`
predecessor selects the current `v6` revision of the same launch shape for
the new task only. An offline recheck of a route-preserving
revision uses the current route-preserving revision; modeled and older
revisions use the current modeled revision.
CCR reserves exit status `77` for unavailable configured provider credentials.
With an empty sealed stdout stream and no capture error, Delegate reports
`authentication-unavailable`; it does not infer an HTTP status or repair the
credential. Other nonzero exits remain provider failures. Earlier CCR builds
that return a generic exit status retain their generic failure classification.
The requested alias is recorded; it is not independent proof of upstream model
identity. CCR detached jobs are not used by this adapter.

Run the shared native acceptance gate with `--provider ccr:print --model ALIAS`
for each permission mode. Missing authentication is BLOCKED rather than passed.
