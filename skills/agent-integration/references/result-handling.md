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
   or call a provider directly to replace a failed continuation. For every
   other `not_created` response, do not poll; correct the request only if
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
Versions and help are observations, not semantic-version allowlists.
Authentication is a separate live gate (`not_run`, `passed`, or `blocked`).
Report a blocked prerequisite without inventing a login command or retrying.

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
