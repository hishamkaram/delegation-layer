# CLI reference

This page indexes task commands and owns operator supervisor configuration.
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
| `supervisor status`, `supervisor migrate` | [Operator recovery](#supervisor-recovery) |

Global paths must be absolute. The CLI's default root is the user's
configuration directory joined with `delegation-layer`; integration examples
pass an explicit root so each invocation uses the same one.
`--pueue-config ABS` or `DELEGATE_PUEUE_CONFIG` selects an external compatible
supervisor for advanced integrations.

## Dispatch

See [dispatch](../skills/agent-integration/references/command-reference.md#dispatch)
and the canonical [manual example](../skills/agent-integration/examples/read-only-task.md).

### Resource check

See [admission results](../skills/agent-integration/references/result-handling.md#task-creation)
for caller decisions and [architecture](architecture.md#fresh-admission-resource-check)
for signals and limits.

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
