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

## Gateway upgrade and scope errors

The prepared Native 0.9.0 / Gateway 0.2.0 candidate is not a published release.
Check installed help before using its new surface. Native owns execution choices
and defaults; connection/workspace commands no longer accept provider/model/
effort/permission/budget policy flags. Do not reconstruct an old ceiling as a
client-side execution rule. Native budgets remain finite.

A saved old-policy profile or unavailable saved executable uses the supported
same-profile upgrade flow:

```sh
delegate gateway upgrade --preview --json
delegate gateway upgrade --accept-execution-policy-removal --json
```

Preview reports source/count observations; it does not run Native/service checks.
Acceptance needs authorization to remove obsolete execution policy. It stops the
owned connector and uses existing SDK profile, Native supervisor and OS service
owners, with a compare against the saved source before selecting the installed
binary. Registration, credentials/generation, original root, disabled approvals,
revisions, pins and history stay preserved. Follow `upgrade.phase`, `failure`,
`next_action` and pending supervisor evidence. Do not edit journals/source paths,
create replacement state, copy an executable, use tmux or repeat login to bypass
a blocked prerequisite. A stopped connector is not provider-task closure.

On macOS, same-user SSH can manage the existing `gui/UID` service domain; the
calling shell need not run in Aqua. `gateway_gui_session_required` with
`check_user_gui_session` means that domain is unavailable. Check that the intended
user has a graphical login; do not create another registration or tmux session.
Registration still does not prove provider authentication or execution readiness.

Approve an existing ancestor once. New project selection and browsing use a
scope ID and relative path; manual project/temp aliases are not needed. Whole
Linux `/tmp` and macOS resolved user temp plus shared `/tmp`/`/private/tmp` are
automatic. Preserve `scope_discovery_error` instead of substituting a guessed
path. Missing paths, changed versions, special-file reads and escaping symlinks
are explicit failures, not evidence that content was filtered. Git structural
`candidate`/`partial` is not repository validation.

Scoped models requires all three selection guards and an exact opaque path
workspace token in a bounded regular file, not JSON or a regenerated token.
A supplied cwd must match the derived canonical path. Historical project-token
SHA guards are for saved tasks, not fresh admission. See the
[connector contract](connector-contract.md#scoped-model-discovery).

Gateway `prepare_continuation` remains unfinished even though guarded Native
`continue` supports explicit successor IDs/fresh origins. Preserve uncertainty;
do not fall back to another dispatch. **Unverified:** new origin-binding/recovery,
full installed-provider coverage, native macOS and actual cloud acceptance.

If a cloud login expires, follow the Gateway
[cloud login guide](https://github.com/hishamkaram/delegation-layer-gateway/blob/main/docs/cloud-login.md).
The IdP must issue refresh grants and the real client must use them. Increasing
device certificate or WSS timeouts cannot establish a seven-day cloud session.
No tenant change or seven-day soak is proven by local tests. After refresh,
resolve the original operation; authentication renewal does not authorize a retry.

## Supervisor configuration fails

Use [supervisor recovery](cli-reference.md#supervisor-recovery) for explicit
Delegate administration. Preserve the root and recorded bindings. Report
conflicts, unreadable state, or unknown endpoint ownership without editing
records, deleting state, running process searches, or operating Pueue directly.

If status reports `active_work: "none"` but migration is blocked, inspect
`migration_readiness` and `first_blocker`. Queue completion alone does not prove
native execution closed. `human_review_task` identifies a task whose evidence
needs review; waiting for queue activity cannot supply missing closure proof.
Keep the exact task ID and reported reason. A later `diagnostic_snapshot_at`
describes the local diagnostic observation, separately from a resident error.

After a completed installation handoff, `legacy_present` with
`run_supervisor_migrate` means inspection routing still needs completion. Use
the same root's explicit migration command; existing tasks retain their
identities. Follow [supervisor recovery](cli-reference.md#supervisor-recovery)
for blocker priority and prepared-edge behavior.

For an eligible same-boot private ghost, see
[human maintenance permission](cli-reference.md#human-maintenance-permission).
It can unblock maintenance after explicit human confirmation; it cannot turn
unknown execution into proven closure or release task/resource/schedule claims.
An unproven external task remains a conservative maintenance blocker.

A prepared installation upgrade remains fenced until its exact OP is completed
explicitly. Use the saved `completion_command` from the new installation;
ordinary `ensure-owner` does not authorize proactive completion. For an update
already applied, reactive `ensure-owner` drains and resumes the same operation.
Follow [installation upgrades](cli-reference.md#installation-upgrades), including
quiescing writers outside Delegate's private admission path. Never clear the
bootstrap journal or edit saved executable paths to bypass a blocker.
Finish the handoff before another installation replacement. A missing candidate
can be reselected before target acceptance; an unavailable accepted target
remains a fault. Preserve that operation and report it. See the same upgrade
procedure for the exact boundary.

A clean absent or refused predecessor during a retained drain can make passive
`supervisor status` exit `0` with `active_work: "unknown"`,
`migration_readiness: "unknown"` and a `predecessor_unavailable` observation
blocker. This is a completed observation, not readiness or an empty queue.
`ensure-owner` pending remains exit `3`; real evidence/cleanup faults exit `1`.

A retained drain can perform one proven cold predecessor Start. If its reason
reports unknown birth, changed/unknown boot, a closed Wait, or an absent/mismatched
recorded child, follow `stop_and_report` even with exit `3`. A healthy endpoint
or later clear queue cannot authorize another Start. Preserve the OP and records;
there is no automatic reset or provider retry. Older binaries refuse a populated
`predecessor_start` receipt. See
[installation upgrades](cli-reference.md#installation-upgrades).

For an installation mismatch, compare both client and daemon observations in
`installation_binding`. File availability and path equality are metadata only.
Neither a missing old keg nor a reachable endpoint establishes which executable
image is running. Follow the explicit recovery action without editing bindings.

An extremely deep root can exceed the host's Unix socket path limit even with
Delegate's compact socket name. Cold owner routing checks this byte budget
before accepting a startup or launching its child, and the child checks it again.
The owner endpoint adds 14 bytes to the canonical root path; the pathname
validator permits 107 bytes on Linux and 103 on macOS. Choose a shorter root for
a new installation; do not move an existing task's state as a recovery shortcut.

On macOS, a login available in a GUI terminal or tmux does not prove an existing
supervisor started from SSH has the same access. Compare the exact execution
contexts within [bounded failure diagnosis](../skills/agent-integration/references/result-handling.md#failure-diagnosis),
without exposing credentials or inherited environments. A later command does
not relocate the running supervisor; changing its context needs covering
authorization.

When changing a quiescent daemon's execution context is authorized, use the
[explicit runtime-restart contract](../skills/agent-integration/references/command-reference.md#supervisor-runtime-restart)
from that intended context. Keep the original startup token across retries.
Passive custody and completed restart observations do not prove provider login;
preserve rejected tasks and verify any separately authorized new task normally.
If the old daemon is alive but unreachable, repeating restart cannot progress;
investigate that daemon's availability without clearing the saved fence. A
pending shutdown may persist until its underlying command fault is fixed.

## Gateway root context no longer matches

A changed device number does not automatically change the saved connector token.
For the same original root, canonical path and inode, follow
[root context repair](cli-reference.md#root-context-repair): select the compatible
build, refresh the serving helper, finish the same upgrade OP, stop producers,
prepare, then explicitly confirm the returned challenge and offline attestation.
The compatible daemon must remain running and ready for confirmation; its
healthy helper may remain running. Run Gateway doctor before starting
the saved service. Do not re-enroll, edit credentials, delete history or move the
root to bypass this mismatch. A copied, moved or replaced root is refused.
Another device-number change needs explicit repair again; no automatic stable
Darwin volume identity is used.

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
