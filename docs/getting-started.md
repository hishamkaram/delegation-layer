# Getting started

1. Run the [recommended standalone setup](installation.md#recommended-standalone-setup),
   or install the integrated plugin. Complete printed PATH, restart and native
   hook-trust steps; passive checks do not prove account readiness or activation.
2. Review the first-task prompt/example printed by setup, or try the
   [agent-first prompt](../README.md#ask-naturally). Setup launches no task.
3. For direct CLI use, follow the packaged
   [read-only task example](../skills/agent-integration/examples/read-only-task.md)
   or [authorized write example](../skills/agent-integration/examples/workspace-write-task.md).

Read [result handling](../skills/agent-integration/references/result-handling.md)
before observing a response. A returned task ID alone does not prove that a
task record exists.

## Terminal interface

Run `delegate tui` in a terminal for Tasks, Teams, Schedules and System views.
Use `a` for actions and `?` for keys. Opening the workbench starts no provider
task or scheduler; effects use the existing CLI owners. Read the
[workbench guide](terminal-workbench.md) for dated history, confirmations and
shutdown. For scripts, keep using ordinary commands with `--json`.

## Remote tasks

Use the [remote workflow](remote-delegation.md) to prepare another installation,
verify its SSH identity and dispatch to a prepared workspace or an explicit
file snapshot. Keep the returned `remote:ID` handle for observation and controls.
The destination owns execution and results; reconnecting does not restart work.
The workflow also documents the remote command limits and retained file data.

## Optional Teams

Use [Teams](teams.md) when you want reusable Roles, Members and provider/model
defaults. Ask the agent to register a scoped Team, inspect the saved definition
and revision, then choose an explicit Member for a task. It manages the catalog
through Delegate CLI; registration validates configuration without proving
provider/model availability. Direct delegation works without Team setup.

The guide owns [scope and linked-worktree rules](teams.md#global-and-local-scope),
[optional project defaults](teams.md#optional-project-defaults), and
[latest defaults versus saved tasks](teams.md#latest-defaults-and-saved-task-identity).

## Recurring tasks

Start `delegate scheduler serve` yourself in an independent terminal or tmux
pane. It can become ready before you save the first definition. Agents and
manual commands share the CLI's OS-default store under the same native
user/configuration context, from any project directory. Each definition keeps
its own project `--cwd`. Saving, host readiness and a successful provider result
are separate facts.

Ordinary commands below omit `--root`. For intentional custom stores and
historical tasks, use the
[explicit-root and upgrade guidance](cli-reference.md#advanced-explicit-and-historical-roots).
The example workspace `/home/alex/projects/repo` must be replaced with your
actual absolute project path.

### Check your installed commands

```sh
delegate help
```

Use `scheduler serve` when help lists it. If help lists only `scheduler run`,
use the older-build path below; it has no host status/stop commands or automatic
child restart. These docs describe main, and
[published installations can lag](installation.md). Choose by installed help
and actual response fields, not a release number. For features missing from a
published build, use a release containing them or the existing
[source-build setup](contributing.md#development-setup).

### Start in an independent terminal or tmux pane

Use your intended native user context: the selected provider CLI, login,
configuration and workspace must be usable there. Use the same Delegate
installation throughout, with colocated `delegate` and `delegate-run` from the
same build, and omit `--runner`. A different terminal's PATH or login does not
prove this launch context is ready.

Open a terminal independently of the agent's bounded task. You can run the
foreground command there directly, or first create a tmux session:

```sh
tmux new-session -s delegate-scheduler
```

Inside that terminal or tmux pane, run the command supported by your build.
For a build with host controls:

```sh
delegate scheduler serve
```

This explicit start initializes absent base and scheduler storage while
preserving existing root metadata. It becomes ready with zero definitions.
If initialization reports busy, rerun after the other initializer finishes;
existing invalid or incompatible storage is an error, not replaced. When a
saved definition returns `scheduler.serve_command`, preserve its exact
executable and root, including any intentional custom root.
Leave this command running: **do not wait for it to return before
checking readiness in a second terminal**. One host manages every saved
schedule in that root. The user starts it independently; do not launch it from
an agent's bounded task, append shell `&`, or install an OS service as part of
this walkthrough.

**Compatibility: `run` or an older build without fresh-store `serve`:**
save the first definition using [the section below](#save-the-first-definition)
before starting the runtime.
`scheduler run` opens existing storage only. If installed help offers only
`run`, use this foreground command in the same independent terminal or tmux pane:

```sh
delegate scheduler run
```

Keep it running and open a second terminal for history inspection below. On a
run-only build, do not issue `scheduler status` or `scheduler stop`, or expect `start_required`,
`serve_command` or other saved-host fields. This compatibility runtime has no
supervised-child automatic restart.

### Check readiness from a second terminal

**For builds with host controls only**, open another terminal and run:

```sh
delegate scheduler status --json
```

Confirm both `scheduler.state: "ready"` and `scheduler.ready: true`. Exit `0`
alone means observation completed. Starting, backoff or an unreachable host
needs observation of the same root, not a competing start or another create.
Report `scheduler.fault: true` and its available diagnostic as a permanent
owner-reported fault; degraded state or `last_error` alone is insufficient.
See [host result decisions](../skills/agent-integration/references/result-handling.md#schedule-definition-and-host-results).

### Save the first definition

Ask your agent, substituting your paths and the choices you want:

> Review this repository's open pull requests every weekday at 09:00 in
> Europe/Berlin, using workspace /home/alex/projects/repo. Use our selected
> provider, model and effort, native read-only permissions and a 30-minute
> budget per run. Keep findings
> local. Save the schedule, return its ID, exact root, exit status and JSON
> response, and give me the scheduler command if returned. I manage the host
> myself in an independent terminal.

The agent uses [`schedule create`](../skills/agent-integration/references/command-reference.md#schedules),
which initializes absent schedule storage without launching a provider.
On the current build, this may happen before or after `serve`; saving never
starts the host. For direct CLI use, replace `ABSOLUTE_PROJECT`,
`ABSOLUTE_BRIEF` and `PROVIDER_ID` with your actual project, regular brief file
and selected provider. Preserve any selected `--model` and `--effort` too:

```sh
delegate schedule create --cwd "ABSOLUTE_PROJECT" --brief "ABSOLUTE_BRIEF" \
  --provider PROVIDER_ID --permission read-only --budget 30m \
  --cron '0 9 * * MON-FRI' --timezone Europe/Berlin --json
```

Run this from another project/session under the same native configuration
context: it reaches the same store as plain `scheduler serve`. Replace
`SCHEDULE_ID` below with the returned `schedule.id`; retain it for every later
read or state change.

On builds with host controls, **`schedule.saved: true` retains that ID on any
exit code**. Exit `3` can request `start_required` or `inspect_scheduler`;
exit `1` can report a permanent host fault after saving. Follow the actual
fields and error without creating the definition again. For `start_required`,
give the returned `scheduler.serve_command` JSON argv array intact to the user;
preserve every argument boundary, including spaces, when entering it in a shell.
Do not join the array into an unquoted command. Older builds may omit these
host fields entirely. In either path, confirm the saved definition:

```sh
delegate schedule show SCHEDULE_ID --json
```

If persistence is uncertain, keep the ID and inspect the reported error before
proceeding. The canonical [definition and host results](../skills/agent-integration/references/result-handling.md#schedule-definition-and-host-results)
own the response decisions.

### Inspect the first occurrence and collect its result

For either build, leave the machine awake and the scheduler running through
the next due appointment. In the second terminal:

```sh
delegate schedule history SCHEDULE_ID --limit 10 --json
```

In `schedule_history.entries`, find `kind: "occurrence"` and copy its
`occurrence.task_id` into `TASK_ID` below. A `kind: "skip"` row records an
interval without an admitted task. An empty history before the next due time
is expected. Host readiness alone does not prove a provider ran.

```sh
delegate schedule execution TASK_ID --json
```

This passively reads validated local task evidence alongside the last cached
observation. Exit `0` or `complete: true` means inspection completed, not that
the task succeeded. Follow [scheduled result handling](../skills/agent-integration/references/result-handling.md#scheduled-execution-results)
for missing, pending or rejected evidence. For an existing task with a result
to collect, use the same task ID and root:

```sh
delegate collect TASK_ID --watch 0s --json
```

Apply [normal result decisions](../skills/agent-integration/references/result-handling.md#observe-and-collect)
to its JSON and exit status before reading the returned validated payload
descriptor. Collection does not repeat provider work. A released reservation
does not prove successful publication, and history can retain an older cached
observation after collection. Results remain inspectable after the agent
session or scheduler ends.

### Clean up a test schedule

For a one-off experiment, retain its exact schedule ID and every admitted task
ID, including independently admitted workers. Follow the project
[cleanup rule](../AGENTS.md#test-and-experiment-cleanup) and
[live-test operating steps](../skills/live-e2e/SKILL.md). For those exact owned
IDs, first prevent additional launches:

```sh
delegate schedule delete SCHEDULE_ID --json
delegate schedule show SCHEDULE_ID --json
delegate schedule history SCHEDULE_ID --limit 100 --json
```

Confirm the definition is deleted. Page history if it returns a continuation
cursor, and account for each admitted occurrence: deletion does not cancel it.
For each owned active task, request cancellation and observe its closure:

```sh
delegate cancel TASK_ID --json
delegate schedule execution TASK_ID --json
delegate collect TASK_ID --watch 0s --json
```

Apply the linked result-handling rules; a cancellation acknowledgement,
committed answer or released reservation alone does not prove complete closure.
Stop the host using the next section only if this experiment started and owns
it. After closure, release any owned managed workspace through
[`workspace release`](../skills/agent-integration/references/workspaces.md#public-commands)
and remove owned scratch files/binaries. Retain shared/default storage and
historical rows. If closure is unresolved, keep the evidence and report the
exact resource. A test that fails during setup still follows these steps.

### Detach and reattach tmux

With tmux's default bindings, press **Ctrl-b, then d** to detach while the
foreground scheduler keeps running in its pane. Reattach from a terminal with:

```sh
tmux attach-session -t delegate-scheduler
```

These are the [official tmux session controls](https://github.com/tmux/tmux/wiki/Getting-Started).
Detaching does not add reboot persistence. Keep the scheduler pane and tmux
server alive.

### Stop, recover and upgrade

**With host controls**, request stop from the second terminal:

```sh
delegate scheduler stop --json
```

Completed stop reports exit `0` and `scheduler.state: "stopped"`; wait for the
foreground command to exit too. `scheduler.stop_accepted: true` alone is
acceptance; exit `3` remains pending, and a later readback fault does not revoke
acceptance or prove shutdown completed. Observe the same generation and follow the
[stop decisions](../skills/agent-integration/references/result-handling.md#schedule-definition-and-host-results).
Independently admitted tasks retain their own supervisors and budgets.

**With foreground `run`**, reattach to its original pane, press Ctrl-C and wait
for the command to exit. Use this for older builds as well as the current
compatibility command; public host stop does not terminate raw `run`.

While a `serve` host lives, unexpected child exits recover with backoff. If
the host/pane is lost or the machine reboots, start the supported foreground
command again with the same root and user context. A still-live faulty host
must be stopped before starting afresh after addressing the cause. There is
no catch-up backlog: history records skipped downtime.

After forced host loss, such as SIGKILL or a killed tmux server, stop can remain
accepted but pending because no closed receipt exists. That is not completed
shutdown. When passive status reports `no_host_observed`, the user may run
`serve` again. If a prior runtime still owns execution, the new host waits with
`state: "starting"` and `last_error: "waiting_for_prior_runtime"`; let ownership
settle rather than forcing takeover. An unreachable endpoint or missing
heartbeat alone does not prove the owner is gone.

Before **any CLI upgrade**, follow [stop-update-restart](cli-reference.md#scheduler-upgrades),
including when moving from foreground `run` to `serve`. Stop and await the
scheduler before a package manager replaces its binaries; keep saved state and
independently admitted tasks.

For recurring [Team Members](teams.md#scheduled-members-and-upgrades), the
schedule saves the explicit target and caller choices; each new occurrence
resolves latest Member defaults. Restart on a compatible build before using
definitions with fields an older reader cannot support.

Find other topics in the [documentation index](README.md).
