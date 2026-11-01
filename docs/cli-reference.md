# CLI reference

This page indexes task, schedule and Team commands and owns operator supervisor configuration.
The packaged [command reference](../skills/agent-integration/references/command-reference.md)
is the shared syntax/default owner; [result handling](../skills/agent-integration/references/result-handling.md)
owns JSON decisions and exit codes.

## Synopsis

Use `delegate help` for the installed build's command surface.
`delegate-run` is the supervisor-owned worker and is normally invoked for you.

| Command | Shared reference |
| --- | --- |
| `dispatch` | [Flags, defaults, and review scope](../skills/agent-integration/references/command-reference.md#dispatch) |
| `providers`, `capabilities` | [Discovery](../skills/agent-integration/references/command-reference.md#providers-and-capabilities) |
| `models` | [Model and effort discovery](../skills/agent-integration/references/command-reference.md#choosing-models-and-effort) |
| `preflight` | [Static readiness](../skills/agent-integration/references/command-reference.md#checking-readiness) |
| `status`, `collect`, `logs`, `cancel` | [Observation and results](../skills/agent-integration/references/result-handling.md#observe-and-collect) |
| `continue` | [Exact-session continuation](../skills/agent-integration/references/continuation.md) |
| `recheck` | [Offline evidence recovery](../skills/agent-integration/references/result-handling.md#offline-recovery) |
| `reply`, `steer`, `events` | [Interaction](../skills/agent-integration/references/interaction.md) |
| `workspace`, `checkpoint` | [Workspaces and checkpoints](../skills/agent-integration/references/workspaces.md) |
| `teams register/update/delete/list/show`, `teams default show/set/clear` | [Team syntax and responses](../skills/agent-integration/references/command-reference.md#teams) · [Teams guide](teams.md) |
| `schedule create/list/show/pause/resume/delete` | [Saved recurring definitions](../skills/agent-integration/references/command-reference.md#schedules) |
| `schedule history`, `schedule execution` | [Passive execution history and evidence](../skills/agent-integration/references/command-reference.md#schedule-history-and-execution) |
| `scheduler serve/status/stop`, `scheduler run` | [User-started host and compatibility runtime](../skills/agent-integration/references/command-reference.md#scheduler-runtime) |
| `supervisor status`, `supervisor migrate` | [Operator recovery](#supervisor-recovery) |

Global paths must be absolute. The CLI's default root is the user's
configuration directory joined with `delegation-layer`; integration examples
pass an explicit root so each invocation uses the same one.
`--pueue-config ABS` or `DELEGATE_PUEUE_CONFIG` selects an external compatible
supervisor for advanced integrations.

## Scheduler hosting

The [scheduler reference](../skills/agent-integration/references/command-reference.md#scheduler-runtime)
owns command syntax and timing/budget behavior. Check `delegate help` for the
installed build's commands.

The user starts one foreground host per execution root in an independent
terminal or tmux pane and leaves it running:

```text
delegate --root ROOT scheduler serve
```

That host manages every schedule in the root and restarts its scheduler child
after unexpected exits using bounded backoff. If the host or pane ends, or the
machine reboots, the user must run the command again. Independent launch is the
user's responsibility. The guard checks an explicit Delegate task-scope marker
and native Linux cgroup evidence; it does not reconstruct guardian ancestry.
An absence of those positives is not proof of independent placement.
An agent must never start the persistent host inside its own bounded task.
Delegate does not install an OS service or start a detached host automatically.

Create/resume save the definition and its ID, then check for a compatible live
host. Without one, exit `3` reports `schedule.saved: true` and
`scheduler.next_action: start_required`, retaining the same ID and exact
`scheduler.serve_command` JSON argv array. Give that array intact to the user;
do not create the definition again. Host readiness does not prove an occurrence
ran. See [definition and host results](../skills/agent-integration/references/result-handling.md#schedule-definition-and-host-results).
Human output also prints JSON argv after `run independently (argv):`; preserve
argument boundaries, including spaces, instead of joining it into an unquoted
shell command.
Temporary host unreachability after saving returns exit `3` with pending
observation guidance and no top-level `error`. An owner-reported
`scheduler.fault: true` identifies a permanent host fault: create/resume return
exit `1` while retaining `schedule.saved: true` and the ID. A diagnostic string
or degraded state alone does not establish that fault.

`scheduler status` is passive. `scheduler stop` fences stopped intent and joins
the child and accepted control work before completing. The host keeps responding
to status/stop while it waits for its child and any prior execution owner. It
records a closure witness after execution ownership is free and before releasing
host ownership; completed stop requires that witness and exact host absence.
Acceptance alone, timeout or host death before the witness is not completion.
Authenticated `stop_accepted: true` remains true if later readback fails; report
the accompanying fault too. Already admitted tasks retain their own supervisors
and budgets.

`scheduler run` remains the compatibility foreground runtime without automatic
child restart. Its current generation appears as `scheduler.state: foreground`,
even after an older managed host stopped. `scheduler stop` does not terminate
that invocation: it reports `stop_accepted: false` and
`next_action: stop_foreground_runtime`. The user ends it in its original pane
with Ctrl-C; prefer `scheduler serve` for managed recovery. History and execution
inspection work with the host stopped.
Before readiness, every current saved definition is checked for compatibility.
An incompatible definition leaves the host degraded and stops automatic child
restart for that fault; report it while retaining the saved IDs.

Executing schedules requires the automatic managed worker. Use colocated `delegate`
and `delegate-run` binaries from the same build and omit the advanced `--runner`
override; see the [scheduler requirement](../skills/agent-integration/references/command-reference.md#scheduler-runtime).

### Optional legacy OS hosting

The existing manual service setup for `scheduler run` remains an operator option
outside the new user-started host path. It is not installed or enabled by
Delegate. The acceptance driver's optional `--hosting service` mode selects this
legacy path; its default `managed-host` mode uses the foreground host without an
OS service manager. See [native scenarios](contributing.md#native-and-cli-scenarios)
for coverage requirements and evidence limits.

An operator's service host must stop the scheduler process without terminating
its independently supervised tasks. For orderly stop on systemd 255, use `Delegate=yes`,
`ExitType=cgroup`, `KillMode=process`, `SendSIGKILL=no`, `TimeoutStopSec=infinity`
and `Restart=no`. Request `systemctl --user --no-block stop UNIT` while its main
process is live. The scheduler exits, while the unit can stay `deactivating`
until its independent tasks and private supervisor exit; restarting that unit
waits behind the outstanding stop. This is not a guarantee for host crashes or
forced teardown. See [systemd service lifetime](https://www.freedesktop.org/software/systemd/man/255/systemd.service.html#ExitType=).
A macOS LaunchAgent runs in the logged-in user's `gui/UID` domain with
`AbandonProcessGroup=true` so independent process groups survive.
For this legacy path, service stop joins pending admissions and
control work without a shutdown-time guarantee. An already accepted admission
may finish during stop; the service does not wait for every admitted task to finish.
Restart by starting the same command with the same root after its previous
owner exits. The service uses persisted definitions and occurrence identities;
it does not replay missed appointments. See the
[runtime policies](../skills/agent-integration/references/command-reference.md#scheduler-runtime).
History and execution inspection do not require stopping or starting the service.
Provider settings/accounts remain owned by the user and provider CLI.

### Scheduler upgrades

Before creating or using Team-targeted schedules:

1. Request `delegate --root ROOT scheduler stop` and wait for completed host and
   child shutdown, rather than only acceptance of the request. For a legacy
   `scheduler run` service, stop it through its service host and wait for the
   previous owner to exit. Preserve independently admitted tasks; do not force
   their teardown to complete an upgrade.
2. Update the CLI, `delegate-run` and the binary/command used by the scheduler
   host to the same new build.
3. Have the user run `delegate --root ROOT scheduler serve` independently with
   that build, the same execution root and intended user configuration context.
   An operator retaining legacy OS hosting restarts that service instead.
4. Create or use Team-targeted definitions only after that restart.

Old binaries strictly reject the new saved request fields. Mixed-version
rolling readers are unsupported: do not keep an old scheduler reading new
targeted definitions. The upgraded runtime preserves existing definitions;
there is no task-JSON rewrite or automatic service restart. See
[scheduled Members](teams.md#scheduled-members-and-upgrades) for catalog identity
and latest-default behavior.

## Dispatch

See [dispatch](../skills/agent-integration/references/command-reference.md#dispatch)
and the canonical [manual example](../skills/agent-integration/examples/read-only-task.md).

### Resource check

See [admission results](../skills/agent-integration/references/result-handling.md#task-creation)
for caller decisions and [architecture](architecture.md#fresh-admission-resource-check)
for signals and limits.

## Teams

See the [Teams guide](teams.md) for CLI-managed SQLite configuration, scope,
validation versus availability, revisions and optional project defaults.
The packaged reference owns [management syntax](../skills/agent-integration/references/command-reference.md#teams),
[explicit Member targeting](../skills/agent-integration/references/command-reference.md#using-a-member),
and [admitted task inspection](../skills/agent-integration/references/command-reference.md#team-task-context).
No-Team delegation requires no catalog setup.

## Managed Git workspaces

See [workspace commands and capture](../skills/agent-integration/references/workspaces.md).

## Git checkpoints

See [checkpoint guidance](../skills/agent-integration/references/workspaces.md#git-checkpoints)
and [checkpoint responses](../skills/agent-integration/references/result-handling.md#checkpoint-results).

## Preflight and continuation

See [static readiness](../skills/agent-integration/references/command-reference.md#checking-readiness)
and [continuation](../skills/agent-integration/references/continuation.md).

## Providers and runtime compatibility

See [catalog/capability facts](../skills/agent-integration/references/command-reference.md#providers-and-capabilities)
and [provider differences](providers.md).

## Models (implemented)

See [model discovery](../skills/agent-integration/references/command-reference.md#choosing-models-and-effort).

## Capabilities

See [capabilities](../skills/agent-integration/references/command-reference.md#providers-and-capabilities).

## Interactive tasks

See [interaction](../skills/agent-integration/references/interaction.md).

## Observation commands

See [observation](../skills/agent-integration/references/result-handling.md#observe-and-collect).

## Offline evidence recheck

See [offline recovery](../skills/agent-integration/references/result-handling.md#offline-recovery).

## JSON responses and exit codes

See [task control responses](../skills/agent-integration/references/result-handling.md#task-control-response)
and [exit codes](../skills/agent-integration/references/result-handling.md#exit-codes).
Team management uses its own [typed envelope and exits](../skills/agent-integration/references/command-reference.md#teams);
task responses can include [persisted Team context](../skills/agent-integration/references/command-reference.md#team-task-context).

### Required provider options

See [required options](../skills/agent-integration/references/command-reference.md#providers-and-capabilities).

## Supervisor recovery

The default supervisor is private to the state root. Normal dispatch adopts a
reachable compatible generation and recovers the saved generation when its
endpoint is absent.
The explicit commands are for inspecting or repairing a real logical/configuration
migration:

```sh
delegate --root "$HOME/delegation-state" supervisor status --json
delegate --root "$HOME/delegation-state" supervisor migrate --json
```

`supervisor_migration_blocked` or `wait_for_legacy_work` means migration must
wait for existing work; it is not a completed migration.

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
prepares or reuses Delegate's private supervisor and records the
inspection handoff. Existing tasks and inspections keep their saved supervisor
bindings and can finish there. Both daemons and queues remain independent.
The completed journal marker selects the new inspection head; a retry resumes
that recorded private generation rather than choosing an unconnected directory.
Default private dispatch and model discovery perform this same safe journal
handoff automatically under the existing bootstrap lock. Private daemon or
configuration migrations still require the explicit recovery command.

For admission's narrow migration exception and retry precedence, use
[task creation](../skills/agent-integration/references/result-handling.md#task-creation).
General administration does not override an attempt's `stop_and_report`.

## Supervisor binding

The saved configuration and endpoint identify a private supervisor generation.
Later controls validate that binding and the live supervisor's required queue
behavior. An installation upgrade alone does not invalidate a compatible
generation already serving its endpoint or authorize replacing it.

The configuration parser accepts YAML and JSON-form YAML with strict fields
and bounded depth, aliases, nodes, and document size. Before first publication,
bootstrap may choose a compact socket name inside the same private supervisor
directory to fit the host limit. A published binding stays authoritative;
changing its configuration or endpoint uses the explicit migration flow.
An extremely deep root can still exceed the socket limit; see
[troubleshooting](troubleshooting.md#supervisor-configuration-fails).

`delegate-run` normally resolves beside `delegate`. The absolute global
`--runner` option selects a custom integration, even when it names that
bundled worker. Task compatibility and correction decisions are owned by the
[runner option guidance](../skills/agent-integration/references/command-reference.md#dispatch).
`status` and `collect` do not use the override.

A compatible live generation is adopted under its saved binding. If its
endpoint is unavailable, Delegate recovers that same recorded generation when
its ownership and availability are proven. Another compatible installation
may observe a live endpoint; that alone does not authorize starting a different
generation. An unresolved transition, ownership conflict, or unavailable
generation retains the explicit migration requirement.

Recovery preserves task history and recorded generation provenance.
Delegate reconciles a proven metadata mismatch within the same generation;
it does not relabel a running supervisor. Legacy bindings still require a
usable supervisor and valid configuration/endpoint checks.
`collect` may recover the private supervisor only to observe an already
requested budget stop; it never submits provider work.

See [supervisor recovery](#supervisor-recovery) and
[troubleshooting](troubleshooting.md) for operator actions.
