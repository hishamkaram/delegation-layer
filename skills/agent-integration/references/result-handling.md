# Result handling

Use the response fields independently. A successful control command is not
the same as a successful provider result.

1. Save `task_id` immediately after dispatch or continue. It is the durable
   Delegate identity. `supervisor.numeric_task_id` and a stop's
   `numeric_task_id` are private, ephemeral queue addresses. The queue can
   reuse a number for a later task, so a repeated number is not a binding
   error; never use it as a task handle.
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
   generic stop rule. A `resource_pressure` refusal (`failure.next_action:
   "retry_later"`, `failure.retry_after_seconds: 30`, `task_record: "not_created"`) is
   a bounded caller choice: report and stop, or wait at least 30 seconds in the
   foreground and retry the exact original admission once; `continue` remains
   `continue`. Never loop, retry in the background, or poll. For every other
   `not_created` response, do not poll;
   correct the request only if
   `failure.next_action` is `correct_request`, otherwise report and stop. When
   `task_record` is `unknown`, stop and report without polling or retrying.
   Status and collect can report `admission: "unknown"` alongside valid terminal
   evidence for an existing task. An admitted task can carry
   `resource_admission` with `status: "unknown"` and bounded `warnings`
   (for example `resource_observation_unavailable`). That is a warning about
   an unreadable host metric, not a failure: observe that same task with
   `status` and `collect`, and do not redispatch.
3. If `status` is `timed_out`, follow the continuation procedure before treating
   pending publication as a reason to wait. `continuation.resumable: true`
   permits a linked successor even when no outcome has been published. An
   `interrupted` task (`failure.code: "task_interrupted"`) follows the table in
   [interrupted turn](continuation.md#interrupted-turn): continue only when
   `failure.next_action` is `continue_task` and `continuation.resumable` is
   `true`, otherwise report it. A `timed_out` task whose `failure.code` is
   `task_cancelled` is `stop_and_report`: report it and stop, even though
   `collect` exits `3`; that exit is not success.
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
preserve `failed` as a discovery failure. Exit `1` with `failed` is an
operational failure; exit `2` with `failed` is an invalid or unsupported
request, not an authentication block. Status and exit are separate
observations. When the response
includes the optional `failure` object and `error` message, keep both in the
report and follow `failure.next_action`; report and stop on
`stop_and_report`. `complete` refers to
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

In task `status` or `collect`, a workspace with `state: "undetermined"`,
`evidence_state: "unknown"`, and `next_action: "watch_workspace"` means that
observer's bounded view expired before it read the workspace. It is an
unobserved view only. It does not imply corruption, change the task fields, or
revoke an eligible continuation, and it does not turn a committed task into a
failure. Follow the task fields above: return a committed result, handle a
rejection using its reported guidance, and keep observing only while the task
fields are pending. When useful, watch the same workspace with the same root.
Do not dispatch again, retry the provider, fall back to another integration,
or repair the environment because of it. A workspace view with `stop_and_report` remains a
concrete failure to report.

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

## Checkpoint results

A `checkpoint` response has `command: "checkpoint"` and no `task_id` or
`outcome`. A checkpoint receipt is Git-content evidence; it never replaces a
provider outcome, and a provider outcome never proves a checkpoint. Read
`checkpoint.id`, `checkpoint.operation_id`, `checkpoint.current`,
`checkpoint.operation`, `checkpoint.reason_code`, `checkpoint.next_action`, and
`error` separately, together with the exit code. Exit `0` alone is not enough.

| Command and response | Meaning and next action |
| --- | --- |
| `create`: exit `3`, `id` C, `next_action: "watch_checkpoint"` | Accepted, not yet captured. Watch `status C`; never repeat `create`. |
| `create`: exit `3`, `requested_action_not_authorized`, `retry_create`, no `id` | Busy refusal; nothing was accepted. Apply the [busy rule](workspaces.md#checkpoint-before-fork). Never wait for your own turn or switch to `--cwd`. |
| `remove C`: exit `3`, `id` C, `operation_id` R, `watch_checkpoint` | Removal attempt R is accepted. Watch `status C --operation R`; never repeat `remove` to observe it. |
| `remove C`: exit `3`, `requested_action_not_authorized`, `retry_remove`, no `operation_id` | Not accepted, usually because creation is still pending. The `id` in your command is not a removal ID. Watch `status C` until it settles, then retry `remove C` once. |
| `remove C`: exit `0` with `operation_id` R | R already succeeded. No new attempt was registered. |
| `status C`: exit `0` | Successful observation. Fork only when `current.state` is `checkpointed` and `current.usable` is `true`. With `next_action: "stop_and_report"`, report the unavailable creation or latest removal; do not fork. |
| `status C`: exit `3`, `watch_checkpoint` | Creation or latest removal is still pending. Keep watching the same C. |
| `status C --operation R`: exit `0`, `1`, or `3` | R's own result: `removed` or `nothing_to_remove` is `0`; `unavailable` is `1` with `stop_and_report`; pending is `3`. A newer attempt in `current.removal` never replaces R's outcome. Keep watching R, not the newer head. |
| `list`: exit `0` | One metadata page. Entries may be pending, unavailable, or removed. Run `status C` for a reconciled verdict. |
| Exit `1` with `stop_and_report` | Concrete failure. Report `error` and every ID already returned; do not repeat the mutation. |
| Exit `2` | Invalid request. Parse errors print usage on stderr without guaranteed JSON; correct the request. |

Take `C` only from `checkpoint.id` and `R` only from `checkpoint.operation_id`
of a response that returned them. Never invent an operation ID or reuse a
creation ID as `--operation`. Returned IDs stay valid after a later read
fault; an unreadable `current` or `operation` is omitted rather than shown as
empty availability.

`status --watch` repeats the same observation of the same IDs until a settled
or failed result or the bound ends. It never begins another action, renews a
helper deadline, or cancels accepted work. The latest observation chooses the
exit. A watch that ends at exit `3` means keep observing; it is not a failure.
If writing the response itself fails (exit `1`), accepted work stays accepted;
observe it with `status` instead of repeating the mutation.

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
