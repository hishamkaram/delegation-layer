# Result handling

Read response fields independently. A successful control command is not a
successful provider result.

## Task control response

Task-command JSON uses `schema_version: 1`, `command`, root/task identity
when applicable, and three separate dimensions:

| Field | Values |
| --- | --- |
| `admission` | `unknown`, `not-admitted`, `admitted` |
| `liveness` | `undetermined`, `running`, `ended` |
| `publication` | `unknown`, `pending`, `committed`, `rejected` |

Queue state is not a liveness value. Optional `status` describes
`timed_out`, `interrupted`, or `blocked`. Responses can also include
`task_record`, `parent_task_id`, `capability`, `continuation`,
`supervisor`, `raw`, `pending`, `stops`, `stop`, `failure`,
`resource_admission`, `diagnostics`, `recheck`, `workspace`, and `error`
with `error_truncated` where applicable.

`outcome` is terminal authority after validation. Its verdict, evidence
digest, and payload descriptor describe the accepted or rejected evidence.
A payload descriptor has `basename`, `length`, and `sha256`; raw stream
descriptors include rooted paths. Validate the returned path, byte length,
and digest before reading; never invent paths or use raw logs as an answer.
A committed outcome may coexist with an ancillary operational `error`;
keep both observations.

`task_id` is Delegate's durable identity. `supervisor.numeric_task_id` and
stop `numeric_task_id` values are ephemeral queue addresses and may be reused;
never use them as task handles.

## Task creation

Capture the original response and exit status once. A returned `task_id`
can be allocated without creation. Before any observation or retry, apply
this table to each dispatch or continuation attempt separately, in order:

| Response | Next action |
|---|---|
| Usage error, no JSON response | Correct the reported syntax using Delegate help or `providers --json`, then invoke that request again. Parsing failed before admission. |
| `task_record: not_created`, `failure.code: supervisor_upgrade_required`, and `failure.next_action: run_supervisor_migrate` | Run `delegate --root "<the exact absolute state root used by the failed command>" supervisor migrate --json` once. Retry the exact original admission argv once only after `supervisor_control.state: migrated` and `supervisor_control.next_action: retry_dispatch`; otherwise stop and report the migration response. Preserve an explicit custom root exactly. A failed `continue` must be retried as the same `continue`; never replace it with a fresh `dispatch` or a direct provider call. |
| `task_record: not_created` and `failure.next_action: correct_request` | Correct that request and retry it. |
| `task_record: not_created`, `failure.code: workspace_provision_pending` or `workspace_busy`, and `failure.next_action: watch_workspace` | Follow [managed workspace recovery](#managed-workspace-results) using the same root and workspace. Once ready, retry the exact admission command and inputs. A failed `continue` remains that same `continue`, never a new dispatch. |
| `task_record: not_created`, `failure.code: resource_pressure`, `failure.next_action: retry_later`, and `failure.retry_after_seconds: 30` | Delegate checked the host and confirmed resource pressure, so no ordinary task record was created and no provider task is running for this attempt. Do not poll or observe a task. Choose one: report the refusal and stop, or wait at least 30 seconds in the foreground and retry the exact original admission argv once. A `continue` stays that same `continue`. If the retry is refused again, report and stop. Never loop, retry in the background, schedule a retry, or run Pueue or process commands. The 30 seconds is advice, not a guarantee that the retry will pass. |
| `task_record: not_created` with any other next action | Report that attempt's failure and stop it. Do not diagnose provider setup or substitute an integration. |
| `task_record: created` | Use `status` before taking any other action. Do not dispatch again. An optional `resource_admission` object with `status: unknown` and `warnings` only means a resource metric could not be read; the task was admitted. Keep the warning with your report, observe that same task with `status` and `collect`, and do not redispatch. |
| `task_record: unknown` or an ambiguous response | Stop and report; do not retry an uncertain task creation. |

A `stop_and_report` ends that attempt. Do not diagnose provider setup, ask
for a fallback, run Pueue/process commands, or use a direct integration.
Other independent requests may proceed. With an explicit task ID, absence
can race another creator; never retry an uncertain ID.

`resource_admission` is optional: `status` is `clear`, `pressure`, or
`unknown`, and `warnings` contain bounded `code`, optional `metric`,
`reason_code`, and `message`. An unknown metric never refuses by itself;
independent confirmed pressure still can. Only new dispatch/continue admission
runs this check. Existing-task observation, cancellation, and recovery remain
available.

## Observe and collect

After `task_record: "created"`, run `status` first. Keep the same root and
task ID; observation never launches or retries provider work.

1. Preserve a concrete failure's guidance. Cancellation and `stop_and_report`
   prevent automatic continuation. An ancillary error does not erase a valid
   terminal outcome.
2. Return a committed `outcome` only with committed publication. For a
   validated rejected outcome, use [offline recovery](#offline-recovery) once
   only when `failure.next_action` is `recheck`; otherwise report rejection.
3. With no terminal outcome, handle `timed_out` before waiting on publication:
   follow [continuation](continuation.md), including its busy-workspace exception.
   For `interrupted`, all three of `failure.code: "task_interrupted"`,
   `failure.next_action: "continue_task"`, and
   `continuation.resumable: true` are required for automatic continuation.
   A cancelled timeout is `task_cancelled` / `stop_and_report`, even if
   collect exits `3`; stop instead of waiting.
4. If `interaction.pending_requests` is nonempty and interaction is open,
   answer using [interaction](interaction.md) before collecting again.
5. With `pending.kind: "lifetime-finalization"`, the guardian still owns
   cleanup. Keep observing the same task with a finite watch, without
   supervisor repair or process commands. A queue-row end alone cannot prove
   termination. Other pending publication or running/undetermined liveness
   is observed through the same task; it is never permission to redispatch.
6. Preserve blocked authentication as unresolved. Report the unavailable
   prerequisite without inventing login commands or interpreting free-form logs.

Keep collection in the foreground until terminal evidence or a reported
blocker. Detach only when requested. `collect --watch` defaults to `0s`;
a positive finite interval bounds observation and never extends the task budget.

Committed publication certifies that the provider/evidence predicate passed;
it does not certify fulfillment of the brief. The Lead must read the complete
validated payload, including all attributed follow-up sections when present,
and check requested artifacts, tests, and acceptance criteria before declaring
the work done. Never select only the first, last, or largest answer section
and discard the others.

A progress-only answer is not completion. First inspect all validated answers
and artifacts; if work remains, use a supported exact-session continuation
under [continuation guidance](continuation.md). Existing permissions and
`stop_and_report` rules still apply. Workers must keep owned commands in the
foreground or await native background completion before their final handoff.
A promise to monitor detached work is not a finished result.

For an `inspect-provider` failure, preserve the optional
`failure.inspection.class`, `deadline_state`, and `journal_task_id` fields in
the report. They are bounded diagnostics, not a new recovery instruction.
Follow `failure.next_action`; do not infer provider authentication or settings
from an inspection-worker or supervisor-observation failure.

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

## Exit codes

| Exit | Control meaning |
| --- | --- |
| `0` | Command completed; collect returned a committed outcome, or recheck recovered an answer, when applicable. A successful observation alone is not provider completion. |
| `1` | Operational error; interrupted collection also uses this exit. Read the response. |
| `2` | Invalid or unsupported request; discovery can also report an unresolved prerequisite. |
| `3` | Pending bounded observation/action, including collection, workspace, or checkpoint responses. Not success. |
| `4` | Validated rejected outcome or still-rejected recheck. |

For model-discovery status/exit pairs use the
[model contract](command-reference.md#choosing-models-and-effort).
