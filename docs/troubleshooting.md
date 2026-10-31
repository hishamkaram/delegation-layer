# Troubleshooting

Use the saved JSON response and exit status. For task creation, observation,
retry, rejection, or recovery decisions, follow
[result handling](../skills/agent-integration/references/result-handling.md).
A delegation blocker does not authorize environment repair; diagnose setup
only when troubleshooting was requested.

| Symptom | Guidance |
| --- | --- |
| CLI missing, unusable, or outside harness PATH | [Installation](installation.md); apply the reported PATH change and start a new session. |
| Provider not found or runtime check fails | Compare the exact process PATH and profile's declared capabilities. An inspection-worker failure alone does not prove provider misconfiguration or missing login. [Discovery](../skills/agent-integration/references/command-reference.md#providers-and-capabilities). |
| Authentication blocked | Use the provider's native setup guidance in [providers](providers.md). Preserve the blocked attempt; keep credentials out of task state. |
| Mode, model, effort, or review target unsupported | Check the [provider table](providers.md) and [request options](../skills/agent-integration/references/command-reference.md#dispatch). |
| Model discovery failed | Read both status and exit; use the [model contract](../skills/agent-integration/references/command-reference.md#choosing-models-and-effort). |
| No task was created or creation is uncertain | Follow [task creation](../skills/agent-integration/references/result-handling.md#task-creation); a returned ID alone is insufficient. |
| Resource-pressure refusal | Follow the bounded choice in [task creation](../skills/agent-integration/references/result-handling.md#task-creation). |
| Task still pending, or lifetime finalization pending | Follow [observation](../skills/agent-integration/references/result-handling.md#observe-and-collect) on the same task. |
| Schedule has no runs | Check [recurring-task setup](installation.md#recurring-task-setup), the saved definition and [scheduler hosting](cli-reference.md#scheduler-hosting). Saving a schedule does not start its service. |
| Schedule history looks stale, a reservation remains active, or a released run has no answer | Inspect the same task using [scheduled result handling](../skills/agent-integration/references/result-handling.md#scheduled-execution-results). |
| Schedule query reports database busy, missing storage or schema/evidence errors | Keep the root and IDs. See [history/read behavior](../skills/agent-integration/references/command-reference.md#schedule-history-and-execution) and [storage rules](architecture.md#scheduler-storage); do not delete state or create another task as repair. |
| Timed out, interrupted, cancelled, or continuation busy | [Continuation](../skills/agent-integration/references/continuation.md) owns precedence and exact-session recovery. |
| Workspace or checkpoint pending/busy | [Workspace results](../skills/agent-integration/references/result-handling.md#managed-workspace-results) and [checkpoint results](../skills/agent-integration/references/result-handling.md#checkpoint-results). |
| Rejected answer | Follow [result handling](../skills/agent-integration/references/result-handling.md#observe-and-collect); use [offline recheck](../skills/agent-integration/references/result-handling.md#offline-recovery) only when offered. |
| Valid outcome accompanied by an operational error | Keep the outcome and report the ancillary error; follow [result handling](../skills/agent-integration/references/result-handling.md#observe-and-collect). |

## Supervisor configuration fails

Use [supervisor recovery](cli-reference.md#supervisor-recovery) for explicit
Delegate administration. Preserve the root and recorded bindings. Report
conflicts, unreadable state, or unknown endpoint ownership without editing
records, deleting state, running process searches, or operating Pueue directly.

An extremely deep root can exceed the host's Unix socket path limit even with
Delegate's compact socket name. Choose a shorter root for a new installation;
do not move an existing task's state as a recovery shortcut.

On macOS, a login available in a GUI terminal or tmux does not prove an existing
supervisor started from SSH has the same access. Compare the exact execution
contexts under a separately requested troubleshooting task. A later command
does not relocate the running supervisor.

## Provider starts but the result is rejected

`provider_startup_failed` means a process exited with a positive status
before structured events. It does not identify authentication or configuration
as the cause. Preserve the task and reported diagnostics.

A provider may reject a model, deny a command under its native restrictions,
or change its output contract. Compare the reported evidence with the
[provider guide](providers.md). Keep global settings and credentials intact;
an adapter error is also possible. An upgrade preserves earlier task outcomes
and recorded contracts.

## Deferred maintenance warnings

`delegate: lifetime custody release deferred` reports deferred maintenance.
The task's authoritative outcome, publication, and exit status remain unchanged.
Observe the same task/root using
[result handling](../skills/agent-integration/references/result-handling.md#observe-and-collect).

`delegate: task maintenance observation deferred` reports a maintenance
observation fault. Preserve the warning and follow the same task-observation
guidance. Neither message authorizes manual state deletion, inferred recovery,
or a new provider launch.
