# Result handling

Use the response fields independently. A successful control command is not
the same as a successful provider result.

1. Save `task_id` immediately after dispatch or continue.
2. For a dispatch or continue error, inspect `task_record` first. When it is
   `created`, observe the returned ID with `status`. Before applying the generic
   `not_created` stop rule, handle the sole migration exception: when
   `failure.code` is `supervisor_upgrade_required` and `failure.next_action` is
   `run_supervisor_migrate`, run `supervisor migrate` once with the same root.
   Only after it reports `supervisor_control.state: "migrated"` and
   `supervisor_control.next_action: "retry_dispatch"`, retry the exact original
   admission argv once. The retry is command-neutral: `dispatch` remains
   `dispatch`, and `continue` remains `continue`; never create a fresh dispatch
   or call a provider directly to replace a failed continuation. Also handle
   `workspace_provision_pending` or `workspace_busy` with `watch_workspace`
   using [managed workspace recovery](#managed-workspace-results) before the
   generic stop rule. For every other `not_created` response, do not poll;
   correct the request only if
   `failure.next_action` is `correct_request`, otherwise report and stop. When
   `task_record` is `unknown`, stop and report without polling or retrying.
   Status and collect can report `admission: "unknown"` alongside valid terminal
   evidence for an existing task.
3. If `status` is `timed_out`, follow the continuation procedure before treating
   pending publication as a reason to wait. `continuation.resumable: true`
   permits a linked successor even when no outcome has been published.
4. If `publication` is committed and `outcome.verdict` is committed, return
   the result and its payload/evidence descriptors.
5. If `outcome.verdict` is rejected and `failure.next_action` is `recheck`,
   follow [offline recovery](#offline-recovery) once. Otherwise report the
   terminal rejection with its diagnostic and evidence. Do not redispatch.
6. If `pending.kind` is `interaction`, handle the returned questions using
   [interaction guidance](interaction.md) before waiting again. If
   `pending.kind` is `lifetime-finalization`, the resident guardian still owns
   cleanup; observe the same task with a finite watch interval and do not run
   supervisor or process commands, redispatch, or call the provider directly.
   Otherwise, if `liveness` is queued or running or publication is pending,
   observe the same task with a finite watch interval.
7. If authentication is `blocked`, preserve that neutral state and tell the
   caller what prerequisite is unavailable without inventing a login command.
8. Never relaunch an uncertain task by guessing. Observe only a task whose
   record is known to exist; the `task_record` decision above takes precedence.

For an `inspect-provider` failure, preserve the optional
`failure.inspection.class`, `deadline_state`, and `journal_task_id` fields in
the report. They are bounded diagnostics, not a new recovery instruction.
Follow `failure.next_action`; do not infer provider authentication or settings
from an inspection-worker or supervisor-observation failure.

For `delegate models`, validate the complete JSON envelope before using any
fact. Treat `available` and `partial` with exit `0` as observations, preserve
`blocked` and `unavailable` with exit `2` as unresolved prerequisites, and
preserve `failed` with exit `1` as a discovery failure. `complete` refers to
model enumeration. Keep `efforts: null` distinct from `efforts: []`, and do not
apply harness-wide effort choices to every model. Discovery is advisory and
never an admission allowlist; a provider default remains valid and a listed
model is not an execution guarantee.

Exit codes are bounded control signals: `0` is a completed command, `1` is an
operational error, `2` is an invalid or unsupported request, `3` is a pending
collection watch, and `4` is a validated rejected outcome.

Catalog facts (`verification: "catalog"`) are not runtime proof. Preflight
checks static readiness; runtime verification requires the supervised probe.
For managed runtime-only dispatches, admission accepts the static contract and
the task's runner performs that probe before provider execution. Always collect
the created task's outcome; `admission: "admitted"` alone does not prove that
the provider started or completed the work.
Versions and help are observations, not semantic-version allowlists.
Authentication is a separate live gate (`not_run`, `passed`, or `blocked`).
Report a blocked prerequisite without inventing a login command or retrying.

## Managed workspace results

Treat `response.workspace` as a separate resource and Git-content evidence
projection. It does not replace `outcome`, `publication`, or `continuation`.
The provider outcome is immutable and the workspace `CaptureReceipt` is a
separate immutable fact; a failed or unavailable capture must not be reported
as a provider failure when native closure is already proven.

Before a task record exists, a pending workspace provision returns
`task_record: "not_created"` with
`failure.code: "workspace_provision_pending"` and
`failure.next_action: "watch_workspace"`. Run the returned workspace-status
command with the same root and workspace ID. Once ready, retry the same
admission with the returned task ID in `--id` and unchanged provider, brief,
permission, model, effort, and source options. A proven never-launched attempt
gets a fresh helper deadline; a created task is only observed.

An occupied managed workspace can return `workspace_busy` with
`task_record: "not_created"` and `watch_workspace` before authorizing a task.
Observe the owning workspace, then retry the exact original admission command.
For `continue`, keep `continue --task PREDECESSOR_ID`; never replace it with a
dispatch. These two workspace responses are narrow exceptions to the generic
stop rule. Do not invent another recovery code or provider retry.

Read the acknowledgement before observing or repeating a workspace mutation:

| Response | Required action |
| --- | --- |
| Exit `3`, reason `requested_action_not_authorized`, next action `retry_exec`, `retry_release`, or `retry_forget` | The requested mutation was not accepted. Observe the workspace without `--operation` until the prior use ends, then retry that exact mutation. `exec` and `release` need `state: retained`; `forget` needs `state: released`. |
| Exit `3`, next action `watch_workspace` | The action is accepted. Observe the same workspace; add `--operation OP` only when its response supplies `operation_id`. Do not repeat the mutation or invent an operation ID. |
| A concrete failure or `stop_and_report` | Preserve the response and report it; do not turn it into a pending watch or provider fallback. |

Use `workspace status ID [--operation OP] --watch DURATION --json` for these
observations. An accepted action may not have a helper ID yet. An `exec`
result of `succeeded` proves the command finished; wait for `state: retained`
before another use, because recapture must finish too. Verification evidence
does not replace the provider outcome. Keep the checkout retained until the
Lead has terminal task/capture evidence and explicitly releases it. Read
[managed workspace guidance](workspaces.md) for the exact command surface and
review/capture/forgetting boundaries.

## Offline recovery

When Delegate returns `failure.next_action: "recheck"`, use the same root and
exact task ID:

```sh
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" recheck TASK_ID --json
```

This command checks the already sealed evidence with an explicitly compatible
new interpreter. It does not run the provider, contact a service, spend model
tokens, or change the original outcome.

- Exit `0` with `recheck.status: "recovered"`: use only the answer descriptor
  returned for that recheck. Report that the answer was recovered from saved
  evidence. The original `publication` remains `rejected`; do not call the
  original task committed or use recovery to infer continuation permission.
- Exit `4` with `recheck.status: "rejected"`: report the diagnostic and stop.
- Exit `1` or `2`, or any response without a verified recovered answer:
  report the error and stop. Do not read raw streams as a substitute answer,
  invent a result path, run a provider directly, or launch a replacement task.

Keep the assessment ID and source/target descriptors with the result. Do not
loop on the same recheck. A separate user request is required for further
troubleshooting or a new provider task.
