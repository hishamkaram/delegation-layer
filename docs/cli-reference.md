# CLI reference

This page indexes local/remote task, device, schedule and Team commands and
owns operator supervisor configuration.
The packaged [command reference](../skills/agent-integration/references/command-reference.md)
is the shared syntax/default owner; [result handling](../skills/agent-integration/references/result-handling.md)
owns JSON decisions and exit codes.

## Synopsis

Use `delegate help` for the installed build's command surface. Use
`delegate help schedule create` or `delegate schedule create --help` for scoped
flags, types, defaults, required inputs, examples and relevant exits. Group help
lists its actions; leaf help shows their flags. Help writes stdout and exits `0`
without opening state or probing providers. An option-looking value remains a
value, and `--` ends option recognition; help does not bypass malformed lexical
input. Invalid requests keep exit `2` with scoped usage on the error stream.
`delegate-run` is the supervisor-owned worker and is normally invoked for you.

| Command | Shared reference |
| --- | --- |
| `tui` | [Terminal workbench, keys and TUI-specific exits](terminal-workbench.md) |
| `help [COMMAND [ACTION...]]` | Installed command tree and registered flags |
| `devices`, remote dispatch and `remote:ID` controls | [Remote workflow](remote-delegation.md) |
| `remote usage/retire`, remote `logs` ranges | [Storage and logs](remote-delegation.md#storage-and-explicit-retirement) |
| `dispatch` | [Flags, defaults, and review scope](../skills/agent-integration/references/command-reference.md#dispatch) |
| `providers`, `capabilities` | [Discovery](../skills/agent-integration/references/command-reference.md#providers-and-capabilities) |
| `models` | [Model and effort discovery](../skills/agent-integration/references/command-reference.md#choosing-models-and-effort) |
| `doctor` | [Optional passive installation/root/provider observations](../skills/agent-integration/references/command-reference.md#doctor) |
| `preflight` | [Static readiness](../skills/agent-integration/references/command-reference.md#checking-readiness) |
| `status`, `collect`, `logs`, `cancel` | [Observation and results](../skills/agent-integration/references/result-handling.md#observe-and-collect) |
| `continue` | [Exact-session continuation](../skills/agent-integration/references/continuation.md) |
| `recheck` | [Offline evidence recovery](../skills/agent-integration/references/result-handling.md#offline-recovery) |
| `reply`, `steer`, `events` | [Interaction](../skills/agent-integration/references/interaction.md) |
| `workspace`, `checkpoint` | [Workspaces and checkpoints](../skills/agent-integration/references/workspaces.md) |
| `teams register/update/delete/list/show`, `teams default show/set/clear` | [Team syntax and responses](../skills/agent-integration/references/command-reference.md#teams) · [Teams guide](teams.md) |
| `schedule create/list/show/pause/resume/delete` | [Saved recurring definitions](../skills/agent-integration/references/command-reference.md#schedules) |
| `schedule history`, `schedule execution` | [Passive execution history and evidence](../skills/agent-integration/references/command-reference.md#schedule-history-and-execution) |
| `schedule health`, `schedule policy` | [Schedule reliability and quota policy](../skills/agent-integration/references/command-reference.md#schedule-reliability) |
| `scheduler serve/status/stop/migrate`, `scheduler run` | [User-started host and compatibility runtime](../skills/agent-integration/references/command-reference.md#scheduler-runtime) |
| `context`, `resolve`, `result read`, `origin` | [Guarded local connector contract](connector-contract.md) |
| `supervisor status`, `supervisor migrate` | [Operator recovery](#supervisor-recovery) |

Remote handles use the supported controls in the [remote workflow](remote-delegation.md#observe-reconnect-and-collect).
A local command in this index does not imply support for `remote:ID`.

Global paths must be absolute. Ordinary commands omit `--root`; the CLI owns
the default state location. Agents and manual commands under the same native
user/configuration context share it from any project directory:

| Host | Default state root |
| --- | --- |
| macOS | `~/Library/Application Support/delegation-layer` |
| Linux | `$XDG_CONFIG_HOME/delegation-layer`, or `~/.config/delegation-layer` when unset |

`--cwd` selects a task's project workspace, not its state root. Changing the
native user/configuration context can change the default.
`--pueue-config ABS` or `DELEGATE_PUEUE_CONFIG` selects an external compatible
supervisor for advanced integrations.

### Advanced: explicit and historical roots

Use `--root ABS` only for an intentional separate store or an existing task's
recorded nondefault root. Keep that exact root for its schedules, task controls,
collection and continuation. A custom root must stay outside provider-writable
workspaces. Existing roots and RootIDs remain in place; Delegate does not search
for, adopt or move historical state.

**Upgrading old integrations:** update the installed skill/plugin through the
[normal installation path](installation.md) and check the active
[routing guidance](agent-routing.md#activation-and-status), including any older
personal skill that takes precedence. Corrected ordinary skill commands omit
`--root`; exporting `DELEGATE_ROOT` no longer changes those commands. The CLI
does not use that variable to select its default. If you intentionally used a
nondefault root, supply its old absolute path explicitly for every operation on
that work. For example, replace `OLD_ROOT` with that recorded path:

```text
delegate --root "OLD_ROOT" schedule history SCHEDULE_ID --json
delegate --root "OLD_ROOT" collect TASK_ID --watch 0s --json
delegate --root "OLD_ROOT" scheduler serve
```

## Scheduler hosting

The [scheduler reference](../skills/agent-integration/references/command-reference.md#scheduler-runtime)
owns command syntax and timing/budget behavior. Check `delegate help` for the
installed build's commands.

For the complete save/start/readiness/result/stop walkthrough, use
[getting started](getting-started.md#recurring-tasks), including the older-build
path when help lists only `scheduler run`. The host controls below require a
build that provides them.

The user starts one foreground host per execution root in an independent
terminal or tmux pane and leaves it running:

```text
delegate scheduler serve
```

Explicit user-started `serve` initializes an absent base store and scheduler
database, preserving any existing root identity. It can become ready with zero
definitions, before the first `schedule create`. A later create from another
project directory uses that same default store without restarting the host;
each definition keeps its own `--cwd`. An intentional explicit `--root` has the
same startup behavior. Invalid, unreadable, corrupt or incompatible existing
storage fails explicitly rather than being replaced. If another initializer
holds the scheduler owner lease, startup reports busy; rerun after it finishes.

That host manages every schedule in the root and restarts its scheduler child
after unexpected exits using bounded backoff. If the host or pane ends, or the
machine reboots, the user must run the command again. Independent launch is the
user's responsibility. The guard checks an explicit Delegate task-scope marker
and native Linux cgroup evidence; it does not reconstruct guardian ancestry.
An absence of those positives is not proof of independent placement.
An agent must never start the persistent host inside its own bounded task.
Delegate does not install an OS service or start a detached host automatically.

Create/resume save the definition and its ID, then check for a compatible live
host. Whenever `schedule.saved: true` is returned, retain that ID on any exit
code. Without a host, exit `3` reports `schedule.saved: true` and
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

`scheduler status`, `schedule list`, `schedule history` and `schedule execution`
are passive: they do not initialize missing storage or start a host. Missing
schedule storage lists as empty; history/execution report not-found.
`scheduler stop` fences stopped intent and joins
the child and accepted control work before completing. The host keeps responding
to status/stop while it waits for its child and any prior execution owner. It
records a closure witness after execution ownership is free and before releasing
host ownership; completed stop requires that witness and exact host absence.
Acceptance alone, timeout or host death before the witness is not completion.
Authenticated `stop_accepted: true` remains true if later readback fails; report
the accompanying fault too. Already admitted tasks retain their own supervisors
and budgets.

`scheduler run` requires existing base and scheduler storage and does not
initialize either. It remains the compatibility foreground runtime without
automatic child restart. Its current generation appears as `scheduler.state: foreground`,
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

Use stop-update-restart for **every CLI upgrade**, including foreground legacy
`run` to `serve`, before any package manager or installer replaces the binaries:

1. Keep the exact execution root and saved IDs. For a managed `serve` host,
   request `delegate scheduler stop --json` and wait for confirmed
   completed host/child shutdown and the foreground command to exit. Acceptance
   alone or exit `3` is pending. For a foreground `scheduler run`, press Ctrl-C
   in its original terminal/tmux pane and wait for exit; older run-only builds
   have no host stop command. For optional legacy OS hosting, stop `run` through
   that host and wait for its previous scheduler owner to exit. Preserve
   independently admitted tasks and their supervisors/budgets. For an
   intentional nondefault root, pass its exact `--root ABS` throughout.
2. Only then update the CLI and colocated `delegate-run` to the same new build,
   including the executable/command used by any legacy service host. This order
   applies to `brew upgrade hishamkaram/tap/delegation-layer` and every other
   installer. Replacing a live managed host's executable can make its next
   child restart fail permanently with `incompatible_build`.
3. Check the upgraded build's `delegate help`. Have the user start
   `delegate scheduler serve` independently when available, or
   foreground `scheduler run` if that remains the supported surface, with the
   same root and intended native user context. An operator retaining legacy OS
   hosting restarts that service instead. Use the
   [walkthrough](getting-started.md#recurring-tasks) to check readiness where
   supported and actual occurrence evidence.
4. Create definitions using newly supported fields, including Team targets,
   only after the compatible scheduler restart.

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
endpoint is absent and its recorded installed pair remains available.
When an upgrade removes the old installed Pueue pair but its private daemon is
still alive, admission selects a compatible current installed transport and the
resident refreshes scheduling/capability evidence for that transport. This does
not restart the daemon or rewrite its launch witness or historical task records.
The saved executable paths remain provenance, not a request to execute deleted
files. Repeated admission reuses the refreshed evidence. If the serving owner's
installed helper was also removed, ordinary admission and `ensure-owner` use the
existing serving-release/replacement lifecycle from an outside invocation. The
old owner retains its daemon Wait custody; this does not cancel active work.
Busy, uncertain or concurrent handoffs can refuse safely. Inspect that same
root before another explicit attempt; do not replay a created or uncertain task.
A nested invocation cannot replace this owner: perform recovery from a prepared
persistent user context outside task-owned scope.

Passive `supervisor status` can report `state: "unavailable"`,
`reason_code: "scheduling_refresh_required"` and
`next_action: "run_supervisor_ensure_owner"`. Run the current installation from
a prepared persistent user context outside a task-owned scope:

```sh
delegate supervisor ensure-owner --json
```

Keep the same execution root (and its explicit `--root` if one was selected).
Do not edit saved executable paths or downgrade the scheduler database to repair
this condition. If the endpoint is stopped or cannot be verified, follow the
reported migration/refusal guidance instead. A replacement transport cannot
start a daemon while publishing the old daemon's launch witness.

An interrupted historical restart keeps its recorded target. If that target
cannot be verified or is not ready, recovery and explicit migration refuse
while preserving the record. This ambiguous legacy state has no automatic
repair.

An unmarked historical generation must already be reachable before a fresh
migration can inspect its queue. Delegate cannot safely start it solely for
inspection, so a stopped historical generation has no automatic migration.
The refusal reports `reason_code: "historical_target_not_ready"` and
`next_action: "stop_and_report"`; dispatch keeps `supervisor_upgrade_required`
without a recovery command. Restoring readiness through the original
installation allows migration to inspect its queue. A migration that already
recorded a safe predecessor shutdown can still resume its installed successor.

Historical state-rooted executable copies are not used as the recovery client.
Recovery selects an installed client for those bindings and still validates the
recorded configuration and endpoint; an unavailable compatible installation
fails explicitly. See the [installed-executable rule](../agents/_data/product-boundary.md#installed-executables).

The explicit commands are for inspecting or repairing a real logical/configuration
migration:

```sh
delegate supervisor status --json
delegate supervisor migrate --json
```

`supervisor_migration_blocked` or `wait_for_legacy_work` means migration must
wait for existing work; it is not a completed migration.

`supervisor status` is read-only. It reports whether the state root has an
older inspection binding, whether that exact legacy endpoint is reachable, and
whether any queued or running work prevents migration. It does not start or
stop a supervisor. A completed migration reports `state: "migrated"` with
`migration_phase: "complete"`; compatible live adoption reports `state: "ready"`
with `migration_phase: "adopted"`. Both report `next_action: "none"` only when
current admission readiness is also verified. Explicit migration completes
ordinary scheduling preparation before reporting readiness. If that preparation
fails after migration has committed, the response retains `state: "migrated"`
and `migration_phase: "complete"` alongside an error and `stop_and_report`.
A completed `migration_phase` remains historical evidence even when current
readiness requires repair. The retained
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

If the selected resident helper is missing, run `supervisor migrate` from
outside a Delegate task; it replaces that serving resident through the current
installation before migrating the supervisor.

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

## Scheduler reliability and multi-task wait

See the canonical [wait](../skills/agent-integration/references/command-reference.md#wait)
and [schedule reliability](../skills/agent-integration/references/command-reference.md#schedule-reliability)
contracts for finite any/all observation, immutable failure hooks and mutable
opt-in quota policy. Host readiness does not certify schedule success.

Stop the scheduler before explicit `delegate scheduler migrate --json` when
`migration_required` is reported. Passive reads never perform that
migration. It preserves old definitions, occurrences and task evidence.

Dispatch, schedule creation and continuation support
`--output-silence-timeout DURATION`; default off, explicit `0` disables, and
continuation inherits otherwise. Waiting for background work or a reply counts
as silence. The normal wall budget remains in force.
