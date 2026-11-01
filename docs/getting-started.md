# Getting started

1. Follow [installation](installation.md) for the integration and CLI.
2. Try the [agent-first prompt](../README.md#ask-naturally).
3. For direct CLI use, follow the packaged
   [read-only task example](../skills/agent-integration/examples/read-only-task.md)
   or [authorized write example](../skills/agent-integration/examples/workspace-write-task.md).

Read [result handling](../skills/agent-integration/references/result-handling.md)
before observing a response. A returned task ID alone does not prove that a
task record exists.

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

Save the definition first, then start one scheduler for its state root yourself.
Saving, host readiness and a successful provider result are separate facts.

The examples use `/home/alex/delegation-state` as the state root and
`/home/alex/projects/repo` as the workspace. Replace them with your actual
absolute paths, including the **exact root already used by your agent**. Keep
the state root outside provider-writable workspaces. Every command that reads
or changes state must use that same explicit `--root`.

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

### Save the first definition

Ask your agent, substituting your paths and the choices you want:

> Review this repository's open pull requests every weekday at 09:00 in
> Europe/Berlin, using workspace /home/alex/projects/repo and state root
> /home/alex/delegation-state. Use our selected provider, model and effort,
> native read-only permissions and a 30-minute budget per run. Keep findings
> local. Save the schedule, return its ID, exact root, exit status and JSON
> response, and give me the scheduler command if returned. I will start it
> myself in an independent terminal.

The agent uses [`schedule create`](../skills/agent-integration/references/command-reference.md#schedules),
which initializes absent schedule storage without launching a provider.
`serve` requires that storage to exist. Replace `SCHEDULE_ID` below with the
returned `schedule.id`; retain it for every later read or state change.

On builds with host controls, **`schedule.saved: true` retains that ID on any
exit code**. Exit `3` can request `start_required` or `inspect_scheduler`;
exit `1` can report a permanent host fault after saving. Follow the actual
fields and error without creating the definition again. For `start_required`,
give the returned `scheduler.serve_command` JSON argv array intact to the user;
preserve every argument boundary, including spaces, when entering it in a shell.
Do not join the array into an unquoted command. Older builds may omit these
host fields entirely. In either path, confirm the saved definition:

```sh
delegate --root "/home/alex/delegation-state" schedule show SCHEDULE_ID --json
```

If persistence is uncertain, keep the ID and inspect the reported error before
proceeding. The canonical [definition and host results](../skills/agent-integration/references/result-handling.md#schedule-definition-and-host-results)
own the response decisions.

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
delegate --root "/home/alex/delegation-state" scheduler serve
```

Use the exact executable and root from returned `scheduler.serve_command` when
present. Leave this command running: **do not wait for it to return before
checking readiness in a second terminal**. One host manages every saved
schedule in that root. The user starts it independently; do not launch it from
an agent's bounded task, append shell `&`, or install an OS service as part of
this walkthrough.

**Older build with only `run`:** instead run this foreground command in the
same independent terminal or tmux pane:

```sh
delegate --root "/home/alex/delegation-state" scheduler run
```

Keep it running and open a second terminal for history inspection below. Do not
issue `scheduler status` or `scheduler stop` on that build, or expect `start_required`,
`serve_command` or other saved-host fields. This compatibility runtime has no
supervised-child automatic restart.

### Check readiness from a second terminal

**For builds with host controls only**, open another terminal and run:

```sh
delegate --root "/home/alex/delegation-state" scheduler status --json
```

Confirm both `scheduler.state: "ready"` and `scheduler.ready: true`. Exit `0`
alone means observation completed. Starting, backoff or an unreachable host
needs observation of the same root, not a competing start or another create.
Report `scheduler.fault: true` and its available diagnostic as a permanent
owner-reported fault; degraded state or `last_error` alone is insufficient.
See [host result decisions](../skills/agent-integration/references/result-handling.md#schedule-definition-and-host-results).

### Inspect the first occurrence and collect its result

For either build, leave the machine awake and the scheduler running through
the next due appointment. In the second terminal:

```sh
delegate --root "/home/alex/delegation-state" schedule history SCHEDULE_ID --limit 10 --json
```

In `schedule_history.entries`, find `kind: "occurrence"` and copy its
`occurrence.task_id` into `TASK_ID` below. A `kind: "skip"` row records an
interval without an admitted task. An empty history before the next due time
is expected. Host readiness alone does not prove a provider ran.

```sh
delegate --root "/home/alex/delegation-state" schedule execution TASK_ID --json
```

This passively reads validated local task evidence alongside the last cached
observation. Exit `0` or `complete: true` means inspection completed, not that
the task succeeded. Follow [scheduled result handling](../skills/agent-integration/references/result-handling.md#scheduled-execution-results)
for missing, pending or rejected evidence. For an existing task with a result
to collect, use the same task ID and root:

```sh
delegate --root "/home/alex/delegation-state" collect TASK_ID --watch 0s --json
```

Apply [normal result decisions](../skills/agent-integration/references/result-handling.md#observe-and-collect)
to its JSON and exit status before reading the returned validated payload
descriptor. Collection does not repeat provider work. A released reservation
does not prove successful publication, and history can retain an older cached
observation after collection. Results remain inspectable after the agent
session or scheduler ends.

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
delegate --root "/home/alex/delegation-state" scheduler stop --json
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
