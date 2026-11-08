# Command reference

Paths passed to `--root`, `--cwd`, and `--brief` must be absolute.
Use `--json` and preserve the response and process exit separately.
The main skill owns [root selection](../SKILL.md#3-prepare-separate-paths-and-a-finite-brief).

| Command | Purpose | Provider execution |
| --- | --- | --- |
| `delegate --version` | Check the control CLI | None |
| `providers --json` | List compiled profiles | None |
| `capabilities --provider ID --json` | Read a compiled contract | None |
| `models --provider ID [--cwd ABS] --json` | Discover model/effort facts | Native inspection, no admitted task |
| `preflight --provider ID --cwd ABS --json` | Check static readiness | None |
| `dispatch [--auto \| --provider ID] ... --json` | Admit a bounded turn; a Member may supply the destination | At most one launch attempt |
| `status TASK --json` | Observe task state | No launch |
| `collect TASK [--watch DURATION] --json` | Read or wait for publication | No launch |
| `logs TASK --json` | Read validated stream descriptors | No launch |
| `cancel TASK --json` | Record an explicit stop | No launch |
| `continue --task TASK ... --json` | Create an exact-session successor | At most one successor launch attempt |
| `recheck TASK --json` | Reassess eligible evidence offline | None |
| `reply`, `steer`, `events` | [Active interaction](interaction.md) | No new launch |
| `workspace`, `checkpoint` | [Git resources](workspaces.md) | Helpers, not provider turns |
| `teams` | [Optional configuration and Member selection](#teams) | None |
| `schedule` | [Recurring definitions](#schedules), [history and execution evidence](#schedule-history-and-execution) | None |
| `scheduler serve/status/stop` (when supported), `scheduler run` | [User-started host and compatibility runtime](#scheduler-runtime) | Due occurrences use normal dispatch; status/stop do not launch providers |
| `devices list/probe/trust` | [Remote destination discovery and trust](#remote-tasks) | List is passive; probe/trust contact the selected destination, without launching a provider |
| `remote list/reconcile` | [Saved remote task references](#remote-tasks) | List is passive; reconcile recovers existing matching work, with no new admission |

The commands below use the CLI default and omit `--root`. The advanced override
syntax is `delegate --root ABS COMMAND ...`; use it only for an explicitly
supplied or already recorded custom or historical root, preserved exactly under
the main skill's [root rule](../SKILL.md#3-prepare-separate-paths-and-a-finite-brief).

## Dispatch

```text
delegate dispatch [--auto | --provider ID] --brief FILE \
  --cwd ABS [--id TASK_ID] [--permission MODE] [--budget DURATION] \
  [--model MODEL] [--effort EFFORT] [--native-timeout DURATION] \
  [--interactive] [--review-target TARGET] [--resume-task TASK_ID] \
  [--team-scope user|project --team TEAM_ID --member MEMBER_ID] \
  [--project-root ABS] --json
```

| Option | Contract |
| --- | --- |
| `--brief FILE` | Regular file, at most 8 MiB; task input, not a raw provider argument. |
| `--cwd ABS` | Existing workspace, disjoint from the private state root. Ordinary tasks support non-Git directories. |
| `--auto` | Try static admission in deterministic provider-ID order; choose the first statically ready candidate. |
| `--provider ID` | Named catalog profile; mutually exclusive with `--auto`. |
| `--id TASK_ID` | Optional 32-character lowercase hexadecimal ID; omit to allocate. |
| `--permission MODE` | Default `read-only`; authorized `workspace-write` requests unattended commands and edits. Native access can extend beyond the workspace. |
| `--budget DURATION` | Positive Go duration; default `30m`, or `1h` for `codex:review`. Queue time is excluded. |
| `--model MODEL` | Exact selector for a profile advertising `model`: Codex, CCR, Claude, AGY, Pi, or OpenCode. CCR requires a configured alias. |
| `--effort VALUE` | Advertised effort selector; omitted or `default` preserves native defaults. |
| `--native-timeout DURATION` | Antigravity print timeout, no greater than `--budget`. |
| `--interactive` | Enable the profile's advertised native interaction; steering is a separate capability. See [interaction](interaction.md). |
| `--review-target TARGET` | Required only for native [Codex review](#codex-review). |
| `--resume-task TASK_ID` | Compatibility form for exact-predecessor continuation; prefer [continue](continuation.md). |
| `--cpu-guard`, `--memory-guard` | Independent fresh-admission checks, both default `true`. Set `=false` to opt out for this dispatch or continuation only. |
| `--team-scope`, `--team`, `--member` | Supply all three to choose one explicit [Member](#using-a-member); no implicit Team or Role-only target. |
| `--project-root ABS` | Explicit containing root for a non-Git project-scoped Member; invalid without that target or in a Git context. |

Without a Team target, exactly one of `--provider` and `--auto` is required.
A bound Member may supply an omitted destination; an unbound Member still
requires an explicit provider or auto. See [Teams](#teams) for saved defaults.

Codex maps effort to TOML `model_reasoning_effort` for ordinary turns or a
native parameter for interactive turns. CCR, Claude, and AGY pass their native
effort flag. Pi maps it to `--thinking`; OpenCode sends acknowledged ACP
session options, with model before effort. CCR treats model as a CCR alias.

Raw provider arguments, token ceilings, and dollar ceilings are not public
options. Preserve the selected workspace; do not initialize Git or change
trust settings as a workaround.

Every task ID identifies one immutable request and at most one provider launch
attempt. Changing its request is rejected; repeating it cannot grant another
launch. A selected provider's failure does not select a fallback.
Read [task creation](result-handling.md#task-creation) before any retry.

Guard overrides are not saved, inherited by nested tasks, or applied to scheduled
occurrences. Existing-task recovery does not run a fresh check. Remote overrides
require destination capability support before upload or admission; they do not
change a previously sent or durably rejected reference. A rejected reference
needs a new task ID to request different admission guards. Overrides are explicit
caller choices, not automatic recovery guidance after resource pressure.

Omit `--runner`: Delegate selects its managed worker. A custom override
conflicting with managed workspaces or a predecessor returns `invalid_request`
with `managed_workspace_runner_conflict` or `continuation_runner_conflict`.
Correct it only under the returned task-creation guidance.

## Teams

Teams are optional configuration managed through the CLI and stored in SQLite.
The [Teams guide](https://github.com/hishamkaram/delegation-layer/blob/main/docs/teams.md)
owns definition examples, scopes/worktrees, safe edits, optional project defaults,
availability, saved-task semantics and limitations. Do not edit the catalog as
JSON or introduce another catalog file. `--root` remains an execution-state
option; it does not select a Team catalog or initialize execution state for
management commands.

```text
delegate teams register --scope user|project --definition FILE_OR_DASH \
  [--cwd ABS] [--project-root ABS] --json
delegate teams update --scope user|project --team TEAM_ID --expected-revision N \
  --definition FILE_OR_DASH [--cwd ABS] [--project-root ABS] --json
delegate teams delete --scope user|project --team TEAM_ID --expected-revision N \
  [--cwd ABS] [--project-root ABS] --json
delegate teams list [--cwd ABS] [--project-root ABS] [--limit N] [--after CURSOR] --json
delegate teams show --scope user|project --team TEAM_ID \
  [--cwd ABS] [--project-root ABS] --json
delegate teams default show --cwd ABS [--project-root ABS] --json
delegate teams default set --cwd ABS [--project-root ABS] \
  --team-scope user|project --team TEAM_ID --json
delegate teams default clear --cwd ABS [--project-root ABS] --json
```

- `--definition` reads a strict bounded definition from a file, or stdin when
  its value is `-`. Register takes the Team ID from that definition; update
  requires it to match `--team`. Update/delete require a positive current
  `--expected-revision`; read it with show before deciding on a mutation.
- `--scope user` management takes no project context. `--scope project` requires
  explicit `--cwd`; non-Git context also requires `--project-root`. List without
  `--cwd` returns user Teams only; with context it also includes project Teams.
  List defaults to 50 rows, at most 100. Pass returned `next_cursor` unchanged
  as the exclusive `--after` cursor, retaining the same project context.
- Default commands always require project context. Set references a user Team
  or a Team in that project. Show reports `unset`, `resolved` or `stale`;
  clear is idempotent. The preference never selects a Member automatically.
- List, show and default show are passive; they neither initialize/migrate/repair
  storage nor inspect provider readiness. A missing catalog lists as empty;
  unreadable/corrupt storage remains an error. Safe partial listings carry
  diagnostics, including unresolved project context or invalid individual rows.

Management JSON uses `schema_version: 1`, `command: "teams"`, `action`,
optional `default_action`, and `status` (`ok`, `empty`, `partial`, or `failed`).
Optional typed fields are `catalog_id`, `team`, `teams`, `next_cursor`,
`mutation`, `default`, `diagnostics`, `failure`, and `error` with
`error_truncated` when needed. Show's `team` has `reference`, `revision`,
and `definition`; list returns summaries, not complete definitions.
Register/update return `mutation.validation` with `definition: "validated"`,
`provider_capabilities: "validated"`, and `readiness: "not_checked"`.
Management never claims that a provider/model can execute.

Handler exit `0` means a successful read/mutation, including empty/no-default
and safe partial results. Exit `2` means invalid input/definition, an unknown
explicit target, revision conflict or project/catalog identity mismatch.
Exit `1` means operational/storage/schema failure. These do not replace task
or scheduler exit semantics. Argument errors may retain normal stderr usage
without JSON. Responses are bounded to 2 MiB; a successful mutation with output
failure is not permission to repeat it. Inspect the same target/default first.
For a revision conflict, inspect the current revision; for uncertain storage
or close errors, inspect durable state before deciding on another mutation.

### Using a Member

The lead inspects list/show and optional project-default guidance, then passes
all three target flags on dispatch or schedule creation. Match a named provider
through the existing provider catalog; preserve exact caller model/effort IDs.
Models are advisory, not an admission allowlist. Blocked optional discovery
does not prevent registration of a valid supported definition.

Caller options override Member defaults. A provider change discards model/effort
inherited from the previous provider; explicit replacement values survive.
`--auto` conflicts with a provider-bound Member. Roles grant no permission,
budget or routing authority. Keep the requested workspace, root and permissions.
For a Local target, the canonical source project owns identity across linked
worktrees; reject a mismatch rather than retargeting.

New task IDs and new scheduled occurrences resolve latest defaults once.
Same-ID replay/recovery and exact-session continuation retain saved selection
and settings without a catalog read. Continuation accepts its existing supported
overrides but rejects new Team/Member targeting flags. Preserve explicit target
failures; no substitution or automatic retry is added. If no Team target was
requested, ordinary provider/auto delegation stays available without catalog
setup, even when there is no matching Team or project default.

### Team task context

After a task record is created, inspect the same ID and root:

```sh
delegate status TASK_ID --json
delegate collect TASK_ID --watch 5s --json
```

Dispatch/status/collect optionally expose top-level `team`, the admitted
immutable context from the task request. It includes the scoped target and
catalog ID, Team revision, names/purpose, Roles, Member and original caller
intent. Human output includes scope, Team ID, Member ID and revision. These
reads do not consult the catalog; an edit or deletion cannot change that saved
view. This differs from the live catalog record returned by `teams show`.
The task's canonical execution fields remain authoritative. Apply normal
[result decisions](result-handling.md#task-control-response); Team context does
not prove admission, provider readiness or successful publication.

## Schedules

The management commands below save and inspect definitions; they do not start a
scheduler, launch a provider themselves, or prove that a task ran. On builds with
[host controls](#scheduler-runtime), create/resume check for a compatible live
host after saving. When a response includes `scheduler.serve_command`, retain
the saved ID and give that argv array intact to the user for an independent
terminal or tmux pane. For run-only builds, use the
[compatibility instructions](#scheduler-runtime); do not expect those host fields.
Never start a persistent scheduler inside the agent's bounded task or submit
another create to obtain a running scheduler.
See [definition and host results](result-handling.md#schedule-definition-and-host-results).

```text
delegate schedule create --brief FILE --cwd ABS \
  [--auto | --provider ID] --cron 'MIN HOUR DOM MONTH DOW' \
  [--timezone ZONE] [--id SCHEDULE_ID] [--model MODEL] [--effort EFFORT] \
  [--permission MODE] [--budget DURATION] [--native-timeout DURATION] \
  [--review-target TARGET] \
  [--team-scope user|project --team TEAM_ID --member MEMBER_ID] \
  [--project-root ABS] --json
delegate schedule list [--after SCHEDULE_ID] [--limit N] --json
delegate schedule show SCHEDULE_ID --json
delegate schedule pause SCHEDULE_ID --json
delegate schedule resume SCHEDULE_ID --json
delegate schedule delete SCHEDULE_ID --json
```

- Requested provider options remain as supplied, including omissions. Creation
  validates inputs without checking provider installation, authentication or
  readiness. It does not create a managed workspace. Interactive requests,
  continuation/session options and supervisor/runner overrides are unsupported.
- Optional [Member targeting](#using-a-member) follows dispatch's destination
  requirements. Creation validates the target and pins its catalog instance ID,
  while saving raw caller overrides/omissions. Each new occurrence resolves
  latest defaults; a different catalog instance or missing target fails without
  fallback. A saved target change needs a new schedule. Follow the operator
  [scheduler upgrade procedure](https://github.com/hishamkaram/delegation-layer/blob/main/docs/cli-reference.md#scheduler-upgrades)
  before using Team-targeted definitions; mixed-version readers are unsupported.
- Cron has five fields and minute precision. Lists, ranges, steps and month/day
  names are supported. Sunday is `0` or `SUN`, not `7`. `N/step` runs from `N`
  through that field's maximum. Descriptors such as `@daily`, seconds, years,
  embedded timezones, `?` and Quartz extensions are unsupported.
- Day-of-month and weekday restrictions follow the cron parser: when both fields
  are restricted, either may match. `*` or `*/1` makes both fields necessary;
  patterned steps such as `*/2` and full restricted ranges retain the either-field
  behavior. Choose explicit fields for the intended recurrence.
- `--timezone` accepts a named zone such as `Europe/Berlin` or `UTC`. Omission
  captures the host/process zone at creation. Later host changes do not change
  it. An empty value or `Local` is invalid; if automatic resolution fails,
  supply the zone explicitly.
- Missing dates/local times are skipped without moving the appointment or
  disabling the definition. February 29 runs in leap years. A repeated local
  minute runs only at its first chronological occurrence. Warnings are advisory;
  they do not predict every future timezone change.
- Definitions are immutable. Pause blocks future admissions; resume skips the
  interval from the stored cursor through now without catch-up. A clock behind
  the cursor leaves that cursor unchanged. Delete is terminal and retains
  history. Pause/delete do not cancel an active occurrence or require a live
  host. Create/resume preserve the saved definition even when the host is
  absent, starting, in backoff or otherwise not confirmed ready.
- Among schedule management commands, only create initializes absent storage.
  An absent store lists as empty; show/state commands report not found. List
  defaults to 50 rows, at most 100, including paused/deleted definitions. Use the
  returned cursor to page.
- Create/state responses are compact. Show returns saved requested choices and
  brief metadata, not brief contents, with a 2 MiB response limit. Keep the ID
  when output or storage reports uncertainty; inspect it with show rather than
  submitting another create. Reusing an existing ID conflicts.

Handler results use exit `0` for confirmed completion/advisory warnings, `2` for
invalid input/state transitions and `1` for not-found, conflicts or operational
failures. On builds with host controls, create/resume also use exit `3` for saved
definitions needing user host start or pending host readiness; keep the same ID
and follow [result handling](result-handling.md#schedule-definition-and-host-results).
`--json` applies to handler responses; argument/usage errors retain the CLI's
normal stderr behavior. A valid rule with no future occurrence in the supported
timestamp range has a distinct range error, not a claim of permanent invalidity.

### Schedule history and execution

```text
delegate schedule history SCHEDULE_ID [--before RFC3339Nano] [--limit N] --json
delegate schedule execution TASK_ID --json
```

`history` returns occurrence summaries and compact skipped intervals together,
newest first by occurrence due time or interval end. It defaults to 50 entries,
at most 100. The returned continuation timestamp is the exclusive `--before`
cursor; use it unchanged for the next page. Newer entries appear when you start
a fresh first page, not as a frozen snapshot across requests. A skip is one
stored `(from, to]` interval with a reason, not an invented task for each minute.

`execution` accepts the task ID of one scheduled occurrence, including a released
one. It joins validated local task evidence with the occurrence and its cached
observation. It does not observe live OS/provider state or infer unrecorded
events. Returned result and log descriptors refer to existing evidence; contents
are not inlined. An ordinary task without an occurrence is not a scheduled
execution.

Both commands are passive: no provider/supervisor call, task control, filesystem
capability probe, initialization, migration or repair. They work with the
scheduler running or stopped, subject to the database busy timeout. Paused and
deleted schedules retain history. An existing schedule with no history returns
an empty page; missing root, database, schedule or occurrence returns not-found.
Missing task files and corrupt evidence are distinct; see
[scheduled result handling](result-handling.md#scheduled-execution-results)
for freshness, no-result states, response bounds, exit codes and recovery.

## Supervisor owner control

```sh
delegate supervisor stop-owner --json
```

Use only when the installed build advertises this command. Preserve the explicit
root for work bound to a custom or historical root. This command contacts one
existing infrastructure owner; it does not initialize, repair or start one.
It leaves Pueue and admitted tasks running. Startup, accepted operations and
unresolved cleanup can make the owner busy.

The `supervisor_owner` response separates `state` (`serving_released`,
`not_initialized`, `busy` or `unknown`) from `process_closure` (`closed`,
`pending` or `unknown`). A released owner can remain alive as the sole waiter for
its daemon; serving release is not proof of process exit or root quiescence.
The optional `process` value (`pid`, `start_token`) identifies the validated
selected `startup`; it does not establish liveness. It is omitted when no owner
was selected or validated. Observe that exact incarnation after daemon shutdown;
a new root-selected stop can address a newer owner.
An absent root reports `not_initialized` with unknown process closure.
Exit `0` reports the observed release or uninitialized state, `3` reports busy,
and `1` reports a control or observation fault. Preserve an uncertain result;
the command does not resend an uncertain stop request. Human output exposes
the same separate observations.

## Scheduler runtime

Check `delegate help` for the installed build's command surface. Use `serve`,
`status` and `stop` only when that help advertises them; prefer `serve` for managed
recovery. The signatures below describe the current command surface. They use
the same CLI default as ordinary schedule commands; preserve an explicit custom
or historical root when the schedule is bound to one.

```text
delegate scheduler serve
delegate scheduler status
delegate scheduler stop
delegate scheduler run [--pueue-config ABS] [--json]
```

If installed help offers only `scheduler run`, the user runs
`delegate scheduler run` in an independent terminal or tmux pane and
leaves it in the foreground. Ctrl-C in its original pane stops it; it has no
automatic child restart. Keep the saved schedule ID and inspect
[history and execution](#schedule-history-and-execution) for actual occurrences.
Do not issue unsupported `scheduler status` or `scheduler stop` commands, or
expect `start_required` or `scheduler.serve_command` fields from that build.
The managed-host controls and response fields below apply only when installed
help supports them.

The user starts one foreground `serve` host per canonical execution root in an
independent terminal or tmux pane and leaves it running. It manages every saved
schedule in that root. When returned, agents give the exact
`scheduler.serve_command` argv array to the user. Agents must not start either
persistent scheduler command inside their own bounded task. The guard recognizes
an explicit Delegate task-scope marker and, on Linux, native Delegate
cgroup membership. It does not reconstruct guardian ownership or detect every
enclosing ancestor. A successful check or missing marker does not prove
independent placement; independent user launch remains required. Delegate
installs no OS service, enables no reboot/login startup and
does not automatically launch a detached host.

When present, `scheduler.serve_command` preserves spaces and shell metacharacters
as separate arguments. Human output also prints JSON argv, not an unquoted shell string.
Preserve those boundaries; see the [scheduler response fields](result-handling.md#schedule-definition-and-host-results).

Explicit `serve` may initialize absent scheduler storage through the same
scheduler owner lease used by `schedule create`. It can become ready with no
definitions; later creates reach that running host without a restart. Existing
root identity and data stay in place; invalid existing storage fails explicitly.
Passive reads and low-level `run` do not initialize absent storage. A second host
or competing `run` cannot acquire the same root's ownership. The host uses its
retained launch context and the saved requested provider choices. For
Team-targeted schedules, new occurrences read the pinned catalog's latest Member
defaults through normal admission; existing task IDs retain saved settings. Use
the intended user configuration context, or a different catalog instance fails
admission even when Team/Member IDs match. Context changes require completed stop
and a fresh user start; full launch environments are not persisted.

While the host lives, it restarts its scheduler child after an unexpected exit,
including a clean exit while running is still desired. A successful Start needs
its exact child Wait before replacement; a definitively failed Start that
created no child may retry without Wait. Retry delays are 1, 2, 4, 8, 16, 30, 60,
120 and 300 seconds, capped at five minutes indefinitely. Five continuous healthy
minutes reset the policy;
elapsed healthy time and delays use monotonic timing. Create/resume/status do not
reset cooldown. A live but unready child or uncertain launch is not replaced.
Before the child reports Ready or scans for due work, the application's
before-Ready check validates every current saved definition. An incompatible
definition leaves the host degraded and stops automatic child restart for that
incompatibility; it does not enter an endless crash/retry loop. Preserve the
saved definitions and report the compatibility fault.
If the host, terminal/tmux pane or machine stops, automatic recovery ends; the
user must run serve again.

`scheduler status` is passive: it does not start, migrate, repair or clean
anything. It observes starting, ready, backoff, degraded, stopping or stopped
state, or a compatibility runtime's `foreground` generation, and available
attempt, bounded diagnostic and retry information. Optional
`scheduler.fault: true` is the host owner's permanent fault decision; degraded state or
`last_error` alone is not that decision. After a definition is saved,
transport-only unreachability returns pending exit `3` without a top-level
`error`; a permanent host fault returns exit `1`, retaining
`schedule.saved: true` and the same ID.

`scheduler stop` persists a stopped fence, interrupts backoff, ends admission and
joins accepted controls, GoCron shutdown and child Wait. The host remains
responsive to authenticated status/stop while joining its child or waiting for a
prior execution owner. Under host ownership, it writes a closure witness once
execution ownership is free. Completed stop requires that witness and exact
host absence; acceptance, timeout or host death alone is not completion.
Authenticated acceptance remains visible even if later readback fails; concrete
faults remain visible too. SIGINT, SIGTERM and SIGHUP use the same fenced shutdown
path. Independently admitted tasks retain their own supervisors and budgets.
See [host result decisions](result-handling.md#schedule-definition-and-host-results).

`scheduler run` runs the foreground runtime directly and remains a compatibility
path without automatic child restart. On builds with host controls, it shares
root ownership with serve and announces its current foreground generation,
which an older managed-host stop
receipt cannot hide. Status reports `scheduler.state: foreground`.
`scheduler stop` does not terminate the raw invocation: it returns
`stop_accepted: false` and `next_action: stop_foreground_runtime`. The user ends
it with Ctrl-C in its original pane; prefer `scheduler serve` for managed
recovery. The existing manual OS-service hosting option is separate from the new
foreground host path; see [operator hosting](https://github.com/hishamkaram/delegation-layer/blob/main/docs/cli-reference.md#scheduler-hosting).

Serve and run require the automatic managed worker. Use colocated `delegate`
and `delegate-run` binaries from the same build and omit the advanced `--runner`
override. An override selects custom ownership even when it names the bundled
worker. `scheduler run` rejects it with exit `2` before opening storage or
acquiring a lease. `scheduler serve` checks existing state and schedule storage
before rejecting the override with exit `2`, so a storage error can take
precedence. Ordinary explicit custom-runner dispatch and continuation retain their
existing compatibility rules. Historical custom-runner claims are not
automatically repaired or released.

Startup skips past appointments. Expired, overlapping or capacity-limited
appointments are skipped with a recorded reason; no catch-up queue or automatic
provider retry/continuation is added. Downtime is recorded in compact skipped
intervals, never replayed minute by minute. Pause/delete prevent new claims,
without canceling already claimed tasks. Use [history and execution](#schedule-history-and-execution)
to inspect recorded work, and [result handling](result-handling.md#scheduled-execution-results)
to collect its answer. Schedule reservation release is separate from result
publication.

Each appointment starts a fresh leader with its saved budget or the normal
provider default. Overlap protection covers that leader. Independently admitted
Delegate workers keep their own budgets; provider-native in-session subagents do
not acquire independent Delegate lifetimes. The workflow owns saved progress,
worker IDs and deduplication; unknown acknowledgment is not permission to resend.

Interrupting the compatibility runtime joins its admission/control work before
releasing its owner lease. Accepted lifecycle calls settle independently of the stop signal;
there is no shutdown-time guarantee. Already admitted tasks continue under their
own supervisors. The compatibility command reports bounded identifiers/codes on
stderr; `--json` selects the final command response, not an event stream or
daemon-control protocol. Uncertain task
evidence blocks only its schedule until the existing owners can prove closure.

## Codex review

For explicit native code review, select `codex:review` and one persisted
target: `uncommitted`, `base:BRANCH`, or `commit:SHA`. It is read-only
and supports neither `--interactive` nor continuation. Use `codex:exec`
for general investigation or edits. The brief is retained as task evidence;
Codex's native target flag supplies review scope because it cannot be combined
with a native prompt.

An `uncommitted` review includes current staged, unstaged, and untracked
changes, but excludes worker commits. For a complete managed turn use an
ordinary read-only task with sealed base/tree refs and `git diff BASE TREE`.
Preserve an explicitly requested narrower native target. See
[workspace review scope](workspaces.md#recovery-and-review-scope).

## Managed workspace commands

[Workspaces](workspaces.md#public-commands) owns creation, attachment, helper
budgets, capture, review, release, and forgetting. Use managed worktrees for
independent parallel writes unless the user explicitly chooses shared coordination.

## Checkpoint commands

[Git checkpoints](workspaces.md#git-checkpoints) owns commands and limits;
[checkpoint results](result-handling.md#checkpoint-results) owns decisions.
A checkpoint is Git-content evidence, not a provider outcome.

## Providers and capabilities

```sh
delegate providers --json
delegate capabilities --provider PROVIDER_ID --json
```

`providers` returns `schema_version: 1` and a sorted `providers` array.
Each entry exposes `id`, `supported_modes`, `supported_options`, any
`required_options`, `continuation`, `read_only`, `interaction`, and
`runtime` metadata. Supply every required option:
`ccr:print` requires a configured CCR model alias; `codex:review` requires
a review target. See [CCR](ccr.md).

These commands create no task or state, contact no supervisor, and launch no
provider. The capability envelope has `schema_version: 1`,
`command: "capabilities"`, and `capability`. Its catalog projection uses
`contract: "runtime-capability-v1"`, `status: "unknown"`,
`verification: "catalog"`, and `live_acceptance.status: "not_run"`.
It describes compiled support, not installation, authentication, or host readiness.

`read_only.containment: "provider-owned"` describes native permissions,
separately from guardian-owned process lifetime. Runtime metadata lists required
help/flag behavior. Required native behavior determines compatibility;
provider versions are optional diagnostics.

Inspection-backed admission can return `status: "ready"` with
`verification: "runtime"` after its probe. Managed runtime-only admission
keeps the catalog projection; its supervised worker checks compatibility before
provider execution. Collect the created task to establish the result.
Provider versions are optional diagnostics. Historical executable-digest members
remain readable as recorded data. Do not depend on these fields or calculate new
executable fingerprints for diagnostics or admission. Required behavior and the
selected installed executable govern compatibility. Live acceptance remains separate (`not_run`,
`passed`, or `blocked`); a blocked prerequisite is unresolved.

## Choosing models and effort

Pass exact requested model IDs and effort unchanged, even if discovery omits
them. Keep defaults when no explicit choice is needed. Resolve a natural-language
name through native discovery:

```sh
delegate models \
  --provider PROVIDER_ID --cwd ABSOLUTE_WORKSPACE --json
```

If discovery cannot resolve a name to one exact ID, ask for that ID; never
manufacture an alias. An explicit ID needs no clarification just because it
is absent from discovery.

`--cwd` defaults to the current directory. Discovery uses the state-rooted
supervisor and a `model-discovery-v1` five-minute native inspection, allowing
first-run model/cache initialization. It may contact the provider, creates
inspection evidence, and admits no provider task. `codex:review` has no
model-discovery command.

The JSON envelope has `schema_version`, `command: "models"`, `provider`,
`observed_at`, `status`, `reason_code`, `complete`, `source`, nullable
`efforts`, and `models`. Rows have exact `id`, optional `name`, nullable
`efforts`, and optional `default_effort`.

`complete` describes enumeration, not effort knowledge. `efforts: null`
means unknown; `[]` means explicitly no choices. Do not apply harness-wide
efforts to every model. Omit effort for unknown/empty per-model metadata unless
explicitly requested. A listed model does not prove execution access;
discovery is advisory, never an admission allowlist.

| Status | Exit | Interpretation |
| --- | --- | --- |
| `available` or `partial` | `0` | Observed model facts; partial enumeration is incomplete. |
| `blocked` or `unavailable` | `2` | Unresolved prerequisite. |
| `failed` | `1` | Operational discovery failure. |
| `failed` | `2` | Invalid or unsupported request, such as invalid cwd or root/workspace overlap; not an authentication block. |

Validate the whole envelope. Keep optional typed `failure` and fixed `error`
with the result; follow `failure.next_action`, stopping on `stop_and_report`.
Supervisor refusals retain `supervisor_migration_blocked`,
`supervisor_migration_unsupported`, or `supervisor_upgrade_required` as
reason codes and return no model facts.

## Checking readiness

Use static preflight when readiness was requested, not before every task:

```sh
delegate preflight \
  --provider PROVIDER_ID --cwd ABSOLUTE_WORKSPACE \
  --permission REQUESTED_MODE --json
```

Supply required options; the supported request options also include budget,
model, effort, native timeout, and review target where advertised.
Preflight creates no task, starts no supervisor, and launches no provider.
A ready `verification: "preflight"` result is not live authentication or
execution proof. Report blocked/unsupported results and follow the returned
guidance; readiness checks do not authorize provider configuration changes.

## Remote tasks

See the [remote workflow](https://github.com/hishamkaram/delegation-layer/blob/main/docs/remote-delegation.md) for device
configuration, explicit trust, file manifests and supported SSH settings.
`--ssh-config ABS` explicitly selects one supported configuration file; the
choice is saved for later controls. Default discovery reads user then system
configuration. `dispatch --device ALIAS` requires `--remote-cwd ABS` or `--input-manifest FILE`
instead of local workspace options. Preserve the returned `remote:ID` on status,
collect, cancel, events, reply and steer. `collect --out ABS` downloads selected
files; it does not apply them. `remote reconcile remote:ID` retains the original
intent after uncertain admission. Do not strip the prefix or use a new ID.
