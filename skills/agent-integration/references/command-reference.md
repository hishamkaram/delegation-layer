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
| `scheduler run` | [Independent scheduler](#scheduler-runtime) | Admits due occurrences through normal dispatch |

Prefix every task command with `delegate --root ROOT`, using the same
explicit root across invocations.

## Dispatch

```text
delegate --root ROOT dispatch [--auto | --provider ID] --brief FILE \
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
| `--interactive` | Enable advertised native clarification/steering; see [interaction](interaction.md). |
| `--review-target TARGET` | Required only for native [Codex review](#codex-review). |
| `--resume-task TASK_ID` | Compatibility form for exact-predecessor continuation; prefer [continue](continuation.md). |
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
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" status TASK_ID --json
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" collect TASK_ID --watch 5s --json
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

The management commands below save and inspect definitions. They do not start
a scheduler, launch a provider, or prove that a task ran. Start the
[scheduler](#scheduler-runtime) separately to execute due occurrences.

```text
delegate --root ROOT schedule create --brief FILE --cwd ABS \
  [--auto | --provider ID] --cron 'MIN HOUR DOM MONTH DOW' \
  [--timezone ZONE] [--id SCHEDULE_ID] [--model MODEL] [--effort EFFORT] \
  [--permission MODE] [--budget DURATION] [--native-timeout DURATION] \
  [--review-target TARGET] \
  [--team-scope user|project --team TEAM_ID --member MEMBER_ID] \
  [--project-root ABS] --json
delegate --root ROOT schedule list [--after SCHEDULE_ID] [--limit N] --json
delegate --root ROOT schedule show SCHEDULE_ID --json
delegate --root ROOT schedule pause SCHEDULE_ID --json
delegate --root ROOT schedule resume SCHEDULE_ID --json
delegate --root ROOT schedule delete SCHEDULE_ID --json
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
  history. Pause/delete do not cancel an active occurrence.
- Only create initializes absent schedule storage. An absent store lists as
  empty; show/state commands report not found. List defaults to 50 rows, at most
  100, including paused/deleted definitions. Use the returned cursor to page.
- Create/state responses are compact. Show returns saved requested choices and
  brief metadata, not brief contents, with a 2 MiB response limit. Keep the ID
  when output or storage reports uncertainty; inspect it with show rather than
  submitting another create. Reusing an existing ID conflicts.

Handler results use exit `0` for success/advisory warnings, `2` for invalid
input/state transitions and `1` for not-found, conflicts or operational failures.
`--json` applies to handler responses; argument/usage errors retain the CLI's
normal stderr behavior. A valid rule with no future occurrence in the supported
timestamp range has a distinct range error, not a claim of permanent invalidity.

### Schedule history and execution

```text
delegate --root ROOT schedule history SCHEDULE_ID [--before RFC3339Nano] [--limit N] --json
delegate --root ROOT schedule execution TASK_ID --json
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

## Scheduler runtime

```text
delegate --root ROOT scheduler run [--pueue-config ABS] [--runner ABS] [--json]
```

This foreground command requires existing schedule storage. One process owns
each root; a second fails busy. It uses the service launch environment and the
saved requested provider choices. Saving a definition never starts the service.
For Team-targeted schedules, new occurrences read the pinned catalog's latest
Member defaults through normal admission; existing task IDs retain saved settings.
Use the same intended user configuration context as creation, or a different
catalog instance fails admission even when Team/Member IDs match.
See [operator hosting](https://github.com/hishamkaram/delegation-layer/blob/main/docs/cli-reference.md#scheduler-hosting).

Startup skips past appointments. Expired, overlapping or capacity-limited
appointments are skipped with a recorded reason; no catch-up queue or automatic
retry/continuation is added. Pause/delete prevent new claims, without canceling
already claimed tasks. Use [history and execution](#schedule-history-and-execution)
to inspect recorded work, and [result handling](result-handling.md#scheduled-execution-results)
to collect its answer. Schedule reservation release is separate from result
publication.

Each appointment starts a fresh leader with its saved budget or the normal
provider default. Overlap protection covers that leader. Independently admitted
Delegate workers keep their own budgets; provider-native in-session subagents do
not acquire independent Delegate lifetimes. The workflow owns saved progress,
worker IDs and deduplication; unknown acknowledgment is not permission to resend.

Interrupting the service joins its admission/control work before releasing its
owner lease. Accepted lifecycle calls settle independently of the stop signal;
there is no shutdown-time guarantee. Already admitted tasks continue under their
own supervisors. The
service reports bounded identifiers/codes on stderr; `--json` selects the final
command response, not an event stream or daemon-control protocol. Uncertain task
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
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" providers --json
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" capabilities --provider PROVIDER_ID --json
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
Optional protocol members `version` and `executable_sha256` are diagnostics,
not launch authority. Live acceptance remains separate (`not_run`, `passed`,
or `blocked`); a blocked prerequisite is unresolved.

## Choosing models and effort

Pass exact requested model IDs and effort unchanged, even if discovery omits
them. Keep defaults when no explicit choice is needed. Resolve a natural-language
name through native discovery:

```sh
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" models \
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
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" preflight \
  --provider PROVIDER_ID --cwd ABSOLUTE_WORKSPACE \
  --permission REQUESTED_MODE --json
```

Supply required options; the supported request options also include budget,
model, effort, native timeout, and review target where advertised.
Preflight creates no task, starts no supervisor, and launches no provider.
A ready `verification: "preflight"` result is not live authentication or
execution proof. Report blocked/unsupported results and follow the returned
guidance; readiness checks do not authorize provider configuration changes.
