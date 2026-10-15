# CLI reference

`delegate` manages durable tasks. `delegate-run` is the supervisor-owned
worker and is normally invoked for you; run it directly only when developing
or integrating a custom supervisor.

## Synopsis

```text
delegate [--root ABS] [--pueue-config ABS] [--runner ABS] dispatch \
  (--auto | --provider PROFILE) --brief FILE --cwd ABS [--id TASK_ID] \
  [--permission MODE] [--budget DURATION] [--model MODEL] \
  [--effort EFFORT] [--native-timeout DURATION] [--interactive] \
  [--resume-task PREDECESSOR_ID] [--review-target TARGET] [--json]

delegate [--root ABS] providers [--json]
delegate [--root ABS] capabilities --provider PROFILE [--json]
delegate [--root ABS] models --provider PROFILE [--cwd ABS] [--json]
delegate [--root ABS] preflight --provider PROFILE --cwd ABS \
  [--review-target TARGET] [--json]
delegate [--root ABS] continue --task PREDECESSOR_ID [--brief FILE] \
  [--budget DURATION] [--model MODEL] [--effort EFFORT] [--interactive] [--json]
delegate [--root ABS] status TASK_ID [--json]
delegate [--root ABS] collect TASK_ID [--watch DURATION] [--json]
delegate [--root ABS] recheck TASK_ID [--json]
delegate [--root ABS] reply TASK_ID --request REQUEST_ID --answers FILE --id MESSAGE_ID [--json]
delegate [--root ABS] steer TASK_ID --brief FILE --id MESSAGE_ID [--json]
delegate [--root ABS] events TASK_ID [--after SEQUENCE] [--watch DURATION] [--json]
delegate [--root ABS] logs TASK_ID [--json]
delegate [--root ABS] cancel TASK_ID [--json]
delegate [--root ABS] supervisor status [--json]
delegate [--root ABS] supervisor migrate [--json]
delegate help
delegate version
```

Global paths must be absolute. `--root` defaults to the user's configuration
directory joined with `delegation-layer`. Dispatch starts the bundled Pueue
client and daemon in a private state-rooted configuration. `--pueue-config` or
`DELEGATE_PUEUE_CONFIG` is optional and selects an existing compatible
supervisor for advanced integrations.

Private ordinary-task supervision uses Pueue's unlimited default group. The
orchestrating model chooses fan-out by issuing multiple dispatches concurrently;
tasks are still durably recorded and observable through `status` and `collect`.
Managed runtime-only tasks use static admission and check the provider inside
their own supervised execution budget. An admitted task still needs that fresh
check before provider execution; `collect` reports its terminal result. Required
native policy inspections, custom runners, and saved inspections retain their
recorded path. Model discovery retains its bounded inspection group. Dispatch
prepares unlimited scheduling before admission and may release previously
admitted queued work even if the new request fails.
Use separate workspaces or read-only permissions when concurrent tasks could
write the same files.

## Supervisor recovery

The default supervisor is private to the state root. Normal dispatch adopts a
reachable compatible endpoint and starts one only when the endpoint is absent.
The explicit commands are for inspecting or repairing a real logical/configuration
migration:

```sh
delegate --root "$HOME/delegation-state" supervisor status --json
delegate --root "$HOME/delegation-state" supervisor migrate --json
```

`supervisor status` is read-only. It reports whether the state root has an
older inspection binding, whether that exact legacy endpoint is reachable, and
whether any queued or running work prevents migration. It does not start or
stop a supervisor. A completed migration reports `state: "migrated"` with
`migration_phase: "complete"`; compatible live adoption reports `state: "ready"`
with `migration_phase: "adopted"`. Both report `next_action: "none"`. The retained
legacy journal is historical evidence, not pending work. An inspection journal still bound to an external Pueue
configuration reports `state: "external"` and
`next_action: "run_supervisor_migrate"`. The repair changes Delegate's
inspection routing; it does not operate on that external supervisor.

`supervisor migrate` runs Delegate-owned repair under the state-root
bootstrap lock. For a private daemon migration, it reopens the recorded legacy
binding, inspects its exact
queue, refuses to stop it while queued or running work exists, and
otherwise shuts it down through its own client and watches for the endpoint to
become unavailable after graceful shutdown. If it was the same private endpoint
used by the current bundled supervisor, Delegate starts that bundled daemon and
waits for readiness before recording the completed migration. Normal admission
then creates the successor inspection group. A crash between these steps is
resumed from the immutable migration journal. A successful JSON response has
`supervisor_control.state: "migrated"` and
`supervisor_control.next_action: "retry_dispatch"`. The command never
migrates or stops an external Pueue configuration.

When the inspection journal points to an external supervisor, the same command
prepares or reuses Delegate's verified private supervisor and records the
inspection handoff. Existing tasks and inspections keep their saved supervisor
bindings and can finish there. Both daemons and queues remain independent.
The completed journal marker selects the new inspection head; a retry resumes
that recorded private generation rather than choosing an unconnected directory.
Default private dispatch and model discovery perform this same safe journal
handoff automatically under the existing bootstrap lock. Private daemon or
configuration migrations still require the explicit recovery command.

When an admission command (`dispatch` or `continue`) returns
`failure.code: "supervisor_upgrade_required"` with
`failure.next_action: "run_supervisor_migrate"`, run the command once with the
same `--root`, then retry the exact original command and argv once only after
`supervisor_control.state: "migrated"` and
`supervisor_control.next_action: "retry_dispatch"`. That response field is
command-neutral: a failed `continue` remains `continue` and must not become a
fresh dispatch. Do not run `pueue`, `pueued`, process searches, or a provider
CLI to repair this state.

## Dispatch

`dispatch` validates the request and prepares its supervisor, then persists
the immutable task before enqueueing its exact supervisor row. Use `--auto`
to try static provider admission in deterministic provider-ID order. The first
statically ready candidate is selected. Managed runtime-only tasks run the
fresh probe inside their guardian-owned worker before provider execution;
inspection-backed paths retain their admission checks. See the
[ownership boundaries](architecture.md#ownership-boundaries). If the selected
provider fails after dispatch starts, the CLI returns that result and does not
silently fall back to another provider. `--provider` and `--auto` are
mutually exclusive.

The brief must be a regular file no larger than 8 MiB. It is supplied to the
provider through standard input and is never appended to provider arguments.
`--cwd` must name an existing absolute workspace directory. State and workspace
paths must remain disjoint.

| Flag | Meaning |
| --- | --- |
| `--provider PROFILE` | Provider profile from `delegate providers`, such as `codex:exec` or `codex:review`. |
| `--brief FILE` | Finite task brief, no larger than 8 MiB. |
| `--cwd ABS` | Canonical workspace directory. |
| `--id TASK_ID` | Optional 32-character lowercase hexadecimal ID. Omit to allocate one. |
| `--permission MODE` | `read-only` (default) or authorized unattended `workspace-write`, including commands and edits. Native access can extend beyond the workspace. |
| `--budget DURATION` | Positive Go duration; default `30m` for ordinary tasks and `1h` for `codex:review`. Queue time is excluded. |
| `--model MODEL` | Model selector when the chosen profile advertises `model` (Codex, CCR, Claude, AGY, Pi, or OpenCode). `ccr:print` requires a configured CCR alias; see [CCR](providers.md#claude-code-router). |
| `--effort EFFORT` | Provider effort selector when the chosen profile advertises it. Codex writes TOML `model_reasoning_effort`; CCR, Claude, and AGY pass their native effort flag; Pi maps it to `--thinking`; OpenCode maps it to `--variant`. `default` keeps the provider default and is omitted from native argv. |
| `--native-timeout DURATION` | Antigravity's native print timeout, no greater than `--budget`. |
| `--resume-task TASK_ID` | Exact predecessor for a continuation; creates a new task. |
| `--interactive` | Enable native clarification and supported active-turn steering. Unsupported providers reject this option before admission. |
| `--review-target TARGET` | Codex review target: `uncommitted`, `base:BRANCH`, or `commit:SHA`. Required by `codex:review`; rejected by other profiles. |
| `--json` | Emit the versioned control response. |

Raw provider arguments, token ceilings, and dollar ceilings are deliberately not
part of this interface. Unsupported options fail before supervisor admission.

For `codex:review`, `--brief` remains required and is persisted as task
evidence. The native review scope comes from `--review-target`; Codex does not
accept a prompt together with its native target flags.

Every task ID is single-use for the requested turn. Repeating a dispatch with
the same ID requires the same immutable request and cannot grant another
provider launch. A continuation is a separate task that names an exact
predecessor and reserves the provider conversation until the predecessor has a
validated terminal outcome or a durable Pueue-ended observation backed by a
lifetime-termination record with independently proven empty scope.

## Preflight and continuation

`preflight --provider PROFILE --cwd ABS [--permission MODE] [--budget DURATION]
[--model MODEL] [--effort EFFORT] [--native-timeout DURATION]
[--review-target TARGET] --json` runs the selected adapter's
static preparation and state-placement checks without creating a task,
contacting Pueue, or launching a provider. It reports `verification:
"preflight"`; a ready result still does not prove live authentication.

`continue --task PREDECESSOR_ID` reads the predecessor's validated brief and
exact provider session, then creates a new linked task through the normal
admission path. `--brief FILE` replaces the predecessor brief with a new
follow-up instruction. `--budget`, `--model`, and `--effort` can override the
corresponding requested values when the provider advertises them. The command
refuses a missing, still-running, mismatched, or unsupported predecessor. A
committed outcome or durable Pueue-ended budget stop backed by application-owned
empty-scope termination proof makes the
predecessor eligible for handoff; the budget stop does not establish task
completion.

Fresh dispatches compile the current provider mapping. Ordinary exact-session
continuations reconstruct the predecessor's recorded preparation contract;
this preserves historical OpenCode, Pi, and CCR mappings while creating a new
turn around the recorded session. Provider-specific continuation validation
still applies, such as Pi's same-workspace rule and CCR's route-preserving model
rule. Interactive continuation is the current exception: Codex and Claude use
the current interactive profile, retain the exact recorded session, and restore
the predecessor environment needed for session discovery instead of reusing a
noninteractive predecessor contract.

The
resolved `delegate-run` executable is saved with each new task, so generated
continuation commands preserve an explicit custom `--runner` integration.

When a budget stop has durable termination evidence, `status` and `collect`
include `status: "timed_out"` and a `continuation` object. `resumable: true`
means the exact session can be continued; `mode` is `native`, `checkpoint`, or
`unsupported`. A timeout never authorizes relaunching the original task.

## Providers and runtime compatibility

`providers --json` returns a bounded object with `schema_version: 1` and a
sorted `providers` array. Each entry contains its ID, supported permission
modes, supported request options, continuation mode, read-only mechanism and
containment, and runtime metadata: optional help-command arguments plus the
flags required by the adapter. The shipped profiles report
`containment: "provider-owned"`, meaning the adapter selects provider-native
behavior without creating an independent delegate-owned sandbox. This metadata
is separate from guardian-owned process-lifetime containment.

Discovery is metadata only. It does not inspect the host, authenticate a
provider, contact Pueue, create state, or launch a process. Static preparation
locates a regular executable and records its identity and required capability
contract. The supervised runtime check runs the version and help commands and
checks the required public flags before provider execution. Interactive
protocols additionally validate their native handshake and event contract.
Any valid nonempty version is accepted; valid means trimmed UTF-8 text without
control characters, not a
particular semantic-version pattern. A missing executable rejects static
preparation. A missing required flag rejects inspection-backed admission; for
managed runtime-only tasks it seals a rejected outcome before provider start.
The executable is fingerprinted again before launch so a replacement cannot be
silently substituted.

The observed version is descriptive task evidence, not a release authorization.
The compatibility check does not use a web release lookup, semver allowlist, or
hardcoded provider version. Probe diagnostics expose bounded reason classes;
provider output and process diagnostics are not copied into control fields.

The runner performs a fresh capability check immediately before launch and
uses the returned current launch plan. This permits a provider relink or
upgrade after admission when the new CLI preserves the adapter contract, while
the task's
permission, policy, workspace, predicate, and artifact contract remain fixed.

The compatibility probe is only a startup check. Provider output shape,
identity, containment, policy, authentication, and result integrity remain
strict runtime contracts; a CLI that starts successfully can still produce a
rejected task outcome if it violates those contracts.

## Models (implemented)

`models` performs advisory native model discovery for one provider. This
command is currently implemented. It uses the state-rooted supervisor and the
shared bounded inspection path, whose `model-discovery-v1` native inspection
window is five minutes to allow first-run provider model/cache initialization.
Required admission inspections use a one-minute queue-inclusive bound. Managed
runtime-only tasks perform their checks within their own task budget. Existing
20-second admission journals retain their recorded deadline.
It creates inspection evidence but does not admit a provider task or launch a
delegated turn. It may contact the selected provider CLI, so use it when an
explicit model or effort choice is needed; provider defaults remain valid.

```sh
delegate models --provider codex:exec --cwd /absolute/workspace --json
```

`--cwd` is optional and defaults to the current directory. When supplied it
must be absolute. A JSON response has this shape:

```json
{
  "schema_version": 1,
  "command": "models",
  "provider": "codex:exec",
  "observed_at": "2026-09-21T10:20:30.123Z",
  "status": "available",
  "reason_code": "ok",
  "complete": true,
  "source": "codex app-server",
  "efforts": null,
  "models": [
    {"id": "gpt-5.6", "name": "GPT-5.6", "efforts": null}
  ]
}
```

`complete` describes whether model enumeration completed. It does not mean
that effort metadata is known. A null `efforts` value means the provider did
not report harness-wide effort choices; an empty array means it explicitly
reported no choices. The per-model `efforts` value has the same distinction.
`name` and `default_effort` are optional. Harness-wide choices are not assumed
to apply to every model, and a listed model does not prove that the account can
execute it.

The status and process exit mapping is:

| Status | Exit | Meaning |
| --- | ---: | --- |
| `available` or `partial` | `0` | Model facts were returned; `partial` means enumeration is incomplete. |
| `blocked` or `unavailable` | `2` | Discovery could not establish usable model facts. |
| `failed` | `1` | Discovery or response processing failed. |

Discovery is never an admission allowlist. Dispatch still performs the
provider's runtime checks and the provider remains authoritative for whether a
requested model or effort can run.

## Capabilities

`capabilities --provider PROFILE --json` returns the selected provider's
compiled runtime contract without creating a task, contacting Pueue, or
starting the provider. The response uses the same `runtime-capability-v1`
contract that dispatch projects after its supervised probe. A catalog response
has `status: "unknown"`, `verification: "catalog"`, and
`live_acceptance.status: "not_run"`; it must not be treated as host readiness.

The dispatch response reports `status: "ready"` and
`verification: "runtime"` only after the required behavior and executable
identity have passed the shared probe. Version text is descriptive. Live
acceptance is a separate gate: authenticated evidence is `passed`, while an
unavailable authentication prerequisite is `blocked` and neutral.

## Interactive tasks

Use `dispatch --interactive` for a task that may need clarification while it is
running. Discover `interaction.clarification`, `interaction.steering`, and
`interaction.supported_modes` with `providers` or `capabilities`. These describe
adapter support, not authentication or live readiness. Codex supports clarification
and steering; Claude supports clarification. Other providers use ordinary
`continue` turns and reject `--interactive`.

`collect --watch` returns early with exit `3` and `pending.kind: "interaction"`
when a native question is pending. Read `interaction.pending_requests`; each
batch has a request ID and questions with IDs, text, options, and `multiple` and
`free_text` flags. Answer every question in that batch in one JSON file:

```json
{"color": ["Blue"]}
```

Use the returned question IDs, not this example's ID. Send the answer with:

```sh
delegate reply TASK_ID --request REQUEST_ID --answers /absolute/answers.json \
  --id 0123456789abcdef0123456789abcdef --json
```

Allocate a new 32-character lowercase hexadecimal message ID for each distinct
message. Keep that ID and the file unchanged for a retry after a lost CLI
response. Repeating the same ID and content returns its existing receipt;
changing the content under that ID is a conflict. Do not resend an uncertain
message with a new ID.

`steer TASK_ID --brief FILE --id MESSAGE_ID --json` sends guidance to the current
Codex turn. It cannot change the task's model, permission, workspace, or budget.
For a new user turn, wait for completion and use `continue --task TASK_ID`.
Continuation inherits interactive mode; `--interactive` can also enable it on a
continuation of an ordinary task. Such an interactive successor uses the
current interactive provider profile while retaining the predecessor's exact
session and recorded session-discovery environment.

Delivery states are `queued`, `unknown`, `sent`, `acknowledged`, and `rejected`.
`unknown` means delivery was reserved but may or may not have reached the provider;
it is never replayed. `sent` means the local pipe accepted the frame. Only an
explicit native receipt establishes `acknowledged`; neither state proves the
model followed the instruction. Codex question resolution alone is not an
acknowledgement. The collected outcome remains the completion authority.

`events --after SEQUENCE --watch 30s --json` returns a bounded event page and the
next `cursor`. Persist that cursor before the next call. A positive watch only
waits for observation; it never extends the task budget. Pages contain up to 100
events and at most 512 KiB; the private journal is bounded to 8192 records and
16 MiB, reserving room for closure. Each event, answer, or steering message must fit a 512 KiB record including its
envelope. The full interaction state, including pending questions and delivery
receipts, is limited to 256 KiB, with closure accounted for before admission.
These bounds keep event pages and task observations inside the 1 MiB JSON
response limit. Larger inputs fail explicitly before they enter the journal.

Waiting for answers consumes the original execution budget. `interaction.deadline`
reports the execution deadline once the native channel is ready. Cancellation,
timeout, provider completion, or loss of the runner closes interaction. A crash
never restarts the original provider. Use the reported continuation path when
available. Questions and replies are private task evidence; the runner seals the
closed interaction journal with the raw streams. This is a structured asynchronous
protocol, not a terminal or arbitrary provider command interface.

## Observation commands

`status` reports the saved admission, current liveness, and publication state.
`collect` reads a committed outcome or waits for one observation interval when
`--watch` is positive; `--watch` defaults to `0s` and never changes the task
budget. For an existing budget stop, collection may reconcile the saved Pueue
binding and enter lifetime recovery. A queue-row end alone cannot publish
terminal facts. It never
launches, retries, or resumes provider work. `logs` returns validated
descriptors for the raw stdout and stderr streams. `cancel` records one
explicit stop request and observes its effect; it does not turn an uncertain
process result into a successful outcome.

When `pending.kind` is `lifetime-finalization`, the resident guardian still
owns native cleanup or durable finalization. Liveness remains `running` and
`collect` exits `3`. Continue observing the same task. Do not run supervisor or
process commands, redispatch the brief, or switch to a provider CLI.

When a task has a committed outcome, that outcome is the terminal authority.
Payload and sealed raw streams are returned as descriptors with rooted paths,
byte sizes, and SHA-256 digests rather than copied into the control response.

## Offline evidence recheck

`recheck TASK_ID --json` reassesses an eligible rejected task using a newer,
explicitly compatible provider interpreter. It reads the original validated
outcome, seal, and complete evidence; it does not probe authentication, contact
providers, start a supervisor, or launch another turn.

The original outcome and evidence stay unchanged. A successful recheck returns
`recheck.status: "recovered"`, a verified `answer` descriptor, and absolute
`answer_path` and `report_path` values while the original
`publication` remains `rejected`. A failed semantic reassessment returns
`recheck.status: "rejected"`. Derived reports live under
`rechecks/<assessment-id>/report.json`; a recovered answer is published before
its report. The assessment identity binds the original outcome and seal hashes
to the target predicate. Repeated requests reuse and validate that assessment.

A rejected `collect` response offers `failure.next_action: "recheck"` only when
an eligible interpretation is available and has not already been assessed.
Otherwise report the rejection. Recovery does not change continuation eligibility.
Do not treat a raw stdout file as a substitute answer.

## JSON responses and exit codes

With `--json`, task-command responses contain `schema_version: 1`, the command
name, task identity when applicable, and separate `admission`, `liveness`, and
`publication` fields. A `status` value is present for timeout or blocked
states; `parent_task_id` identifies a continuation successor and
`continuation` carries resumability metadata. The `providers` response is a
separate catalog schema.
An `outcome` is present only after its publication and evidence have been
validated; its verdict may be committed or rejected. Bounded `diagnostics`
explain new interpretation failures without copying raw provider content. Old
outcomes remain readable and do not acquire an invented rejection cause. Operational diagnostics appear in `error` and do not erase an
independently valid published outcome.

| Exit code | Meaning |
| --- | --- |
| `0` | Command completed; collection returned a committed outcome, or recheck recovered an answer, when applicable. |
| `1` | Operational, storage, supervisor-binding, or output error. |
| `2` | Invalid request or unsupported profile/configuration. |
| `3` | `collect` reached the end of its bounded watch interval without a terminal publication. |
| `4` | Collection returned a valid rejected provider outcome, or recheck still rejected the evidence. |

All complete JSON responses are bounded. Oversized diagnostic text is shortened
while authority fields and descriptors are retained.

## Supervisor binding

For the private state-rooted supervisor, the initial dispatch records the
canonical config path, config digests, endpoint, observed version, and the
resolved client/daemon executable details as provenance. Its verified
content-addressed client/daemon pair is stored below the state root. Later
operations bind to the same private config and endpoint, then verify a live
daemon by running the resolved Pueue client's readiness command and parsing its
queue status. Changing install paths, executable hashes, or observed version
does not by itself invalidate a compatible private supervisor. The
configuration parser accepts YAML and JSON-form YAML with strict fields and
bounded depth, aliases, nodes, and document size.
Fresh bootstrap may choose a shorter socket filename inside the same private
supervisor directory when the ordinary name exceeds the host limit. This
pre-publication choice does not alter a published generation; changing a
published config or endpoint still requires the migration flow below.

`delegate-run` normally resolves beside `delegate`. Use the absolute global
`--runner` option for a source-build layout or a custom integration.

When a saved binding points to the private state-rooted supervisor, `status`,
`cancel`, and continuation checks reuse it when a compatible daemon is already
serving its endpoint. If that endpoint is unavailable, they restart the saved
private executable pair when it is still available. A different verified
client may inspect a compatible live endpoint, but ordinary recovery cannot
start or publish that client's daemon over a stopped active generation. If the
active installation paths are missing but the same verified pair remains under
the state root, a stopped supervisor restarts from those exact bytes without
operator setup when neither identity has a migration-history reference.
A durable bootstrap intent makes interruption recoverable.
A proven binding/daemon path split with identical hashes is repaired without
rewriting daemon provenance. Conflicts, unresolved transitions, and references
in migration history retain the explicit `supervisor migrate` transaction
described above. Dispatch uses the same adoption-first path; a changed daemon
path or hash is provenance drift while the compatible endpoint remains live,
and never grants authority to start a replacement. A real logical binding
change uses the explicit repair flow described above. Legacy bindings without
daemon identity require a resolved executable pair and the normal config and
endpoint checks; see
[Troubleshooting](troubleshooting.md#supervisor-configuration-fails).
`collect` may recover that private daemon only to observe an already-requested
budget stop; it never starts a provider or submits work.

For setup failures, see [Troubleshooting](troubleshooting.md). For provider
specific behavior, see [Providers](providers.md).

### Required provider options

Provider catalog and capability JSON can include `required_options`. Supply
those fields before dispatch or preflight. `ccr:print` requires `model` to be a
configured CCR alias; other existing profiles retain their optional defaults.
Use `delegate models --provider ccr:print --json` to discover aliases when needed.

```sh
delegate --root "$HOME/delegation-state" dispatch --provider ccr:print \
  --model local-review --brief /absolute/task.txt --cwd /absolute/workspace \
  --permission read-only --budget 30m --json
```
