# Troubleshooting

Use the saved JSON response and exit status. For task creation, observation,
retry, rejection, or recovery decisions, follow
[result handling](../skills/agent-integration/references/result-handling.md).
Use [bounded failure diagnosis](../skills/agent-integration/references/result-handling.md#failure-diagnosis)
to inspect available evidence and relevant local read-only metadata without
another generic troubleshooting request. Environment changes, retries and
destination replacement still need authorization covering that action.

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
| Remote connection, trust, admission or transfer failure | Preserve `remote:ID`, the `remote` object when present, connectivity and concrete errors. Interpret [remote response fields](api.md#remote-responses), then follow [remote observation and uncertainty](remote-delegation.md#observe-reconnect-and-collect) and [SSH support limits](remote-delegation.md#ssh-support); connection loss does not authorize redispatch. |
| Schedule has no runs | Follow the [scheduler walkthrough](getting-started.md#recurring-tasks) to check the saved definition, installed command surface and independently started foreground scheduler. Saving a schedule does not start it; readiness does not prove an occurrence ran. |
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
contexts within [bounded failure diagnosis](../skills/agent-integration/references/result-handling.md#failure-diagnosis),
without exposing credentials or inherited environments. A later command does
not relocate the running supervisor; changing its context needs covering
authorization.

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

## Scheduler installation and upgrade observations

After an upgrade, inspect `delegate scheduler status --json`. `host_ready` is
only lifecycle readiness; `ready` additionally requires available installation
health. Follow the reported action to inspect or restore the intended installed
Delegate/runner pair, then restart the foreground scheduler. Observation never
repairs files, restarts the host or replays missed occurrences. Per-schedule
outcomes remain in `schedule health` and history.

A pending metadata probe returns unknown health. If kernel filesystem I/O is
permanently blocked, stopping can remain pending indefinitely while the scheduler host keeps its
ownership lock. The observation timeout does not cancel kernel I/O. Do not delete state
or launch a competing host to bypass that ownership.

`delegate doctor --json` reports advisory scheduler compatibility separately
from top-level issues/exits. `newer_schema` requires a compatible newer build:
downgrading the executable does not reverse a database migration. Doctor never
migrates, repairs or creates scheduler storage. Preserve the existing state.

## Unattended Claude authentication

A validated structured `provider_authentication_failed` diagnostic asks for
native provider login; the task still has `provider_output_rejected`. Follow
`run_native_provider_login` in the intended native context. Delegate does not
perform login or automatically retry. Unknown output remains generic rather
than guessing authentication from arbitrary text.

On macOS, launchd/SSH processes can have different Keychain access from a GUI
terminal. Successful terminal login does not prove unattended access or which
account a provider fallback selected. Delegate does not copy/inject credentials,
change Keychain access, or offer a token-login workaround. Establish the native
provider login/access in the host's intended context; preserve task evidence if
it remains unavailable. Authentication diagnostics do not identify an account,
credential source or the cause of a particular Keychain denial.
