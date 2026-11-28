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
| `doctor` | [Optional passive installation/root/provider and advisory scheduler observations](../skills/agent-integration/references/command-reference.md#doctor) |
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
| `result read` | [Bounded committed local answers](../skills/agent-integration/references/command-reference.md#result-read) |
| `result save` | [Complete verified local answer export](../skills/agent-integration/references/command-reference.md#result-save) |
| `context`, `resolve`, guarded `result read`, `origin` | [Guarded local connector contract](connector-contract.md) |
| `gateway connect/status/doctor/serve/disconnect`, `gateway workspace`, `gateway service` | [Gateway login, workspace approvals and per-user service](gateway-connect.md) |
| `filesystem scopes/list/read/inspect/git` | [Bounded filesystem observations](connector-contract.md#read-only-filesystem-observations) |
| `gateway upgrade --preview/--accept-execution-policy-removal` | [Existing-profile upgrade](gateway-connect.md#native-root-and-profile) |
| `gateway repair-context` | [Explicit root context repair](#root-context-repair) |
| `supervisor status/ensure-owner/migrate/restart` | [Operator recovery](#supervisor-recovery) |
| `settle ID [--kind task|inspection] --prepare/--confirm` | [Human maintenance permission](#human-maintenance-permission) |

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

See [model discovery](../skills/agent-integration/references/command-reference.md#choosing-models-and-effort)
and [scoped model guards](connector-contract.md#scoped-model-discovery). Native
owns provider/model/effort/permission/budget validation and defaults. Discovery
is advisory; an explicit model ID need not appear in a catalog. Native budgets
must be chosen explicitly as a positive duration or `unlimited`; there is no
new-task budget default. Gateway connection and
workspace approval save routing, not execution-policy defaults.

## Filesystem and guarded continuation

Use [filesystem observations](connector-contract.md#read-only-filesystem-observations)
for strict request files, bounded pages/bytes and lossless names. Use
[guarded Native continuation](connector-contract.md#guarded-native-continuation)
for successor IDs and fresh origins. Historical project-token SHA guards compare
saved task bindings; they do not authorize fresh admission. Gateway
`prepare_continuation` remains unfinished. These candidate contracts do not
establish cloud or installed-provider acceptance.

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

### Installation upgrades

Agents use the [delegate-upgrade workflow](../skills/delegate-upgrade/references/workflow.md)
for planned updates and interrupted installation handoffs. The CLI contracts
below define supported commands and response semantics.

Use the same state root throughout. Stop its scheduler and Gateway connector
before upgrading; preserve their definitions, profile and credentials. Quiesce
external/custom producers too, including any other configuration or state root
that targets this private Pueue endpoint and commands already in flight. The
private admission fence does not exclude arbitrary Pueue writers.

For a planned update, run this from a persistent user context outside a Delegate
task **before** replacing the installation:

```sh
delegate supervisor ensure-owner --prepare-upgrade --json
```

Check `delegate help supervisor ensure-owner` first. Older releases without
`--prepare-upgrade` use the reactive procedure from the new CLI after updating.
Check pinned connector service paths before preparation; keep an unsupported
source-installed profile stopped until its supported reinstall is available.

Keep the returned `supervisor_control.upgrade.operation_id`. Exit `3` means
draining or another pending prerequisite; follow the exact blocker and resume
the same preparation. `upgrade_prepared` with phase `awaiting_installation`
means preparation finished. Run your normal installer, then execute the returned
`completion_command` argv, using the new installation. For example:

```sh
delegate supervisor ensure-owner --complete-upgrade OP --refresh-owner --json
```

Replace `OP` with the exact returned identity. A proactive operation needs this
explicit completion even when the installer leaves the same paths/version in
place. Completion creates a fresh daemon generation. An unrelated ordinary
`ensure-owner` cannot authorize it. Repeating an already completed OP returns
its retained completion without another daemon launch or repair. Completion
history stays immutable; a historical replay alone is not evidence that the
current generation is ready. Gateway's explicit upgrade separately requests
current readiness from Native. Ordinary `ensure-owner` retains its existing
startup and inspection-routing preparation.

Exact completion can advance that operation's own retained drain when Native's
queue and canonical maintenance proofs permit it. Active work stays pending;
it never authorizes another operation. Replaying completed A while B is current
returns A's history without refreshing B's serving owner. Cancellation,
transport and cleanup faults remain faults even alongside pending progress.

A successfully acknowledged predecessor shutdown is retained separately from
process closure. Reconciliation observes that same predecessor without sending
shutdown again just because its endpoint still responds. Successor launch still
requires Native's canonical closure proof. Older strict readers refuse populated
acknowledgement records, including completed migration history; use a compatible
build and preserve the operation and records.

If the installation was already replaced, ordinary
`delegate supervisor ensure-owner --json` detects a different selected pair,
creates or resumes one reactive operation, and fences new private admissions.
It leaves admitted tasks running until maintenance is safe, then hands serving
to the installed pair. Keep its operation identity across pending responses.
An ordinary current-pair call without a pending operation does not restart a
healthy daemon. Same-path replacement cannot be detected from path metadata;
use proactive preparation for that case.

Finish the current handoff before replacing the installation again. If a
candidate executable disappears before migration accepts its target, the same
authorized operation can select the current compatible installation. After
target acceptance, that target stays fixed. An unavailable accepted target is
a fault: preserve the operation and records, and report it without clearing
the fence or choosing another target.

A retained operation in `draining` can recover a stopped predecessor once,
using its exact saved installed pair. The drain owns the durable Start
acceptance and process-birth receipt. Recovery requires authenticated resident
ownership, bootstrap exclusion, settled current and historical daemon Wait
custody, verified stopped identity, and unchanged restore sources. Offline queue
checks can refuse recovery; they never prove readiness. Advancement still
requires the actual live queue and canonical maintenance proof.

An accepted Start is never replayed. Unknown birth, a changed or unknown boot,
a closed Wait, or an absent/mismatched recorded child reports `stop_and_report`
even with exit `3`. A later healthy endpoint cannot clear that reason. Only the
live invocation that proves Start never occurred can roll its acceptance back;
there is no automatic reset or provider retry. Older strict readers refuse a
drain containing the new `predecessor_start` receipt. Keep those binaries off
that root, and preserve the operation when recovery is exhausted.

`--refresh-owner` alone replaces only the serving helper when needed; it does
not authorize a prepared installation or claim daemon execution readiness.
Upgrade-driven refresh and missing-helper replacement through ordinary ensure
or manual migration refuse with `owner_refresh_successor_start_accepted`
and `stop_and_report` when a successor Start was accepted without validated
canonical completion, including an unknown birth. The current resident may own
successor's only Wait. Keep its operation and installation; do not retire it or
repeat refresh. Exact completion without refresh can proceed only when the
selected runner matches that resident. Otherwise restore the appropriate
installation before following the retained operation's recovery guidance.
Read `state`, `next_action`, `upgrade` and errors together: exit `0` can mean
prepared, helper refreshed, or historical completion. Only a response that
actually reports `owner_ready` confirms current admission preparation. Exit `3`
is pending, `1` is a fault/refusal and `2` is invalid input. Do not repeat an
uncertain mutation or replay a task that was created or may have been sent.
Inspect the saved operation first after a lost acknowledgement. Resume only
that operation through its reported maintenance guidance; an already recorded
completion authorization is never replaced by a new OP. Do not repeatedly
request helper refresh while following acknowledged pending progress.

After verified completion, restart the scheduler and connector from the new
installation. A Gateway profile pinned to a removed candidate executable needs
an explicitly supported service reinstall; do not edit its credential or profile
to hide that mismatch. Installation handoff does not rewrite task history,
schedule definitions, provider configuration or native sessions.

### Root context repair

`delegate gateway repair-context` explicitly reconciles a saved connector root
token after a device-number change. It requires the same original root, canonical
path and inode, validated startup history on its first repair and every canonical
task/inspection request. Later repairs use the immutable binding origin. It does
not relocate or adopt a copied/replaced root. Do not edit the profile, credentials
or task history to make a token match.

Use this order from a persistent user context outside Delegate-owned tasks:

1. Select or install the reviewed compatible build. Run
   `delegate supervisor ensure-owner --refresh-owner --json` to replace an old
   serving helper when needed, then finish the retained handoff with the same
   OP. Reactive mode uses `ensure-owner --json`; proactive mode requires its
   saved `--complete-upgrade OP` command. Require completed handoff and no pending
   generation transition before repair. Keep the compatible daemon running and
   ready for confirmation; its healthy current helper may remain running.
2. Stop the Gateway connector, scheduler and other writers/frontends through
   their supported controls. Preserve their original running/paused state.
3. Prepare and inspect the exact returned challenge, coordinates, digests and
   request counts:

   ```sh
   delegate gateway repair-context --json
   ```

4. After explicitly confirming the intended original root, no other live copy,
   and stopped incompatible writers/frontends, use that challenge:

   ```sh
   delegate gateway repair-context --confirm CHALLENGE --offline-cutover-confirmed --json
   delegate gateway doctor --json
   ```

5. After doctor verifies the root match, restart the saved connector service
   with `delegate gateway service start --json` if it was running, and restore
   only the producers stopped for this repair.

Add the same `--profile ABS` to Gateway commands for an explicitly selected
profile. Omit `--root` for the default store; for a recorded custom/historical
store add only its exact existing `--root ABS` throughout. Prepare is passive
and prints no saved token, wire guard or secret. A matching legacy context returns
`root_context_not_required` without enabling the feature. Confirmation revalidates
under the profile lock, inactive connector identity, bootstrap exclusion, live canonical root-wide
quiescence and nonblocking maintenance exclusion. It atomically adds
`root-context-binding-v1` and `context_binding` to `root.json`; registration,
profile, credentials, task requests and outcomes retain their bytes.

Clean native work blocks confirmation with `root_context_native_work_pending`,
exit `3` and `wait_for_native_work`. Observation, evidence and cleanup faults
report `root_context_evidence_unreadable`, exit `1` and `stop_and_report`.
Maintenance-lock contention also waits with exit `3`. A changed snapshot or
challenge reports `root_context_challenge_stale`; run passive prepare again
before confirming the new challenge. An identity mismatch remains
`native_context_changed` and requires `stop_and_report`.
A pure cancellation or deadline retains `gateway_operation_interrupted`. An
interrupted confirmation may already have committed; do not infer rollback or
repeat confirmation automatically. Run passive `gateway repair-context` on the
same profile, or `gateway doctor`, before another action. For an interrupted
confirmation, passive prepare with `already_reconciled: true` means the binding
is already committed: run `gateway doctor` instead of confirming again. A joined
concrete fault still reports the evidence failure.

This is a one-way compatibility cutover: older strict readers refuse the new
root feature on their next open; already-open old writers require operator
closure. The original token preimage remains immutable, and each later
acquisition must match the accepted canonical path/device/inode exactly. Another
device-number change requires another explicit repair. No automatic stable
Darwin volume-identity mapping is provided. See the
[connector contract](connector-contract.md#root-context-repair) for the binding.

### Execution context and logical migration

For an authorized change of a quiescent daemon's execution context, use
[runtime restart](../skills/agent-integration/references/command-reference.md#supervisor-runtime-restart):
inspect `supervisor status --runtime-restart`, retain its original startup token,
then explicitly request `supervisor restart --from-startup TOKEN`. This does not
perform provider login. `stop-owner` releases serving only; a new caller does
not relocate a daemon that is still running.

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
An unfinished accepted upgrade target also fences this missing-helper
replacement, including manual migration: keep its current custodian and follow
`owner_refresh_successor_start_accepted` / `stop_and_report`. Busy, uncertain or
concurrent handoffs can refuse safely. Inspect that same root before another
explicit attempt; do not replay a created or uncertain task.
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

Outside the retained installation-drain recovery above, an unmarked historical
generation must already be reachable before a fresh migration can inspect its
queue. Delegate cannot safely start it solely for inspection, so a stopped
historical generation has no automatic migration.
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

`supervisor_migration_blocked` means migration was refused. Follow its exact
reason and next action. `wait_for_legacy_work` asks you to let existing work
finish; a refusal with another action is not an instruction to keep waiting.

Read `active_work` and `migration_readiness` separately. An ended queue row can
still lack native closure, so `active_work: "none"` does not make migration safe.
`first_blocker` identifies the bounded reason and exact task when known. Follow
its `next_action`; `human_review_task` requires inspecting that task rather than
waiting for more queue activity or launching replacement work. Unknown queue
state or unreadable evidence cannot establish a clear maintenance assessment.
Pure contention on a work owner reports `owner_busy` with
`wait_for_legacy_work`; let that owner finish before retrying maintenance.
After a resident failure, `diagnostic_snapshot_at` identifies a later local
observation; it does not replace the original error or prove its cause.

`installation_binding` compares the selected runtime client and daemon with
recorded provenance (`scope: "runtime_selected"`,
`verification: "filesystem_metadata"`). Per-role availability is separate from
path relation. Missing files do not prove a process exited, and a matching path
does not prove which executable image a running daemon loaded. A compatible
live endpoint can still advise `run_supervisor_ensure_owner` for an installation
handoff.

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
readiness requires repair. Completed journal entries remain historical evidence.
If an installation handoff has finished but inspection routing still points to
its predecessor, status reports `legacy_present` and
`next_action: "run_supervisor_migrate"`. Gateway's current-readiness step and
ordinary `ensure-owner` complete the supported current-generation routing.
Historical completion replay does not repair it. Follow the returned guidance;
an interrupted or older flow can still require
`delegate supervisor migrate --json` with the same root.
Continuation and some provider
inspections can require this step. Admission may also finish an already prepared
edge; status still advises explicit completion until the edge is recorded as
complete. Follow a more specific maintenance blocker first.
An inspection journal still bound to an external Pueue
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
waits for readiness before recording the completed migration. Inspection routing
completes through its own journal; normal admission uses the recorded successor.
A completed private record alone does not prove that its inspection edge is ready.
The installation-upgrade owner completes its current generation's edge without
starting another daemon during that derived step. Gateway's separate readiness
intent can first use Native's ordinary startup owner if the exact published
installation has stopped. Explicit migration can also prepare that edge after
checking the current queue and
root-wide closure evidence. If the exact published current daemon has stopped,
this path can restart it normally before those checks; it does not repeat the
completed private handoff. A crash between these steps is resumed from the
immutable migration journals. A successful JSON response has
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

### Human maintenance permission

An ended private queue row without native lifetime closure can remain a blocker
on the same boot. Inspect its exact `first_blocker.task_id`; a cancellation
acknowledgement or elapsed time does not establish closure. For a cooperative
task whose recorded worker and every launch are positively absent, an operator
can grant permission for supervisor maintenance:

```sh
delegate settle TASK --prepare --json
delegate settle TASK --confirm CHALLENGE --json
```

Prepare reads existing evidence without repairing it. Review the exact task,
returned challenge and held claims before confirming. An agent must obtain the
human's approval for that task and effect; a challenge is a freshness check,
not proof of human identity. Confirm revalidates the ended row, root, receipts,
same boot, process incarnations and free execution owners before a create-once
write. Missing rows, live owners, unknown identities and conflicting evidence
refuse. Repeating the same confirmed challenge returns the immutable winner.

This records `allow_supervisor_maintenance` only. Native execution remains
unknown; it creates no accepted result, success, termination or continuation
permission. Scheduler, resource, session, workspace, custody and deletion claims
remain held. Status presents the administrative decision separately; collect
stops with exit `1`. Wait stops with exit `1` when `--all` includes a settled
member, or every member of `--any` is settled. `--any` keeps watching other
pending members until genuine readiness or the deadline. Settlement itself never
sets result readiness. A genuine later
outcome takes precedence. Connected completed migration history lets the same
permission remain usable for later maintenance without reconfirming it.
For a scheduled task, an unresolved occurrence can still prevent further runs;
maintenance permission does not free that reservation.

Inspection operations have a separate namespace:

```sh
delegate settle OP --kind inspection --prepare --json
delegate settle OP --kind inspection --confirm CHALLENGE --json
```

A complete legacy inspection can qualify for maintenance from its validated
request, receipt, completion and independent worker observation, even when its
old queue row and executables are gone. Historical numeric IDs are matched only
against their own receipts. Expired unstarted GuardianWait inspections use
canonical recovery; no human permission is needed for these cases.
Valid tracked exact scopes, partial launches and positively previous-boot
lifetimes also use normal recovery. Explicit settlement preparation directs
these cases to `supervisor ensure-owner` rather than offering a challenge.

Manual inspection settlement retains native safeguards: a valid unresolved
cooperative lifetime, an existing free owner, same-boot recorded processes
positively absent, and an exact ended or positively absent queue label under the
canonical generation mapping. Live processes, active/ambiguous labels, malformed
evidence and unknowable legacy execution refuse. Preparation writes nothing;
confirmation creates only maintenance permission. The response identifies the
inspection operation and acknowledged missing scope-closure fact. This does not
make the operation eligible for deletion or admission proof. Canonical closure
supersedes optional permission, including after previous-boot recovery.

Admission-only submitted tasks report `admission_only_unlaunched` with
`cancel_task` guidance. Explicit cancellation uses the existing durable launch
refusal; settlement does not cancel a task. Missing control facts are reported
separately in `control_fact`, naming a record, target kind, executable or queue
observation without publishing its unrestricted native cause.
An inspection ID passed to task `status` gets observational inspection guidance;
only an explicit settlement request points to `settle --kind inspection`.

Return to the blocked maintenance command after confirmation. Settlement does
not execute that command. An unproven task on an external supervisor can still
block root-wide maintenance: this private permission does not authorize changes
to its external queue. Previous-boot evidence uses existing explicit recovery;
rebooting is not a promise of closure. Preserve evidence on refusal.

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
