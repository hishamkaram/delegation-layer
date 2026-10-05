# Result handling

Use the response fields independently. A successful control command is not
the same as a successful provider result.

1. Save `task_id` immediately after dispatch or continue.
2. For a dispatch or continue error, inspect `task_record` first. When it is
   `created`, observe the returned ID with `status`. When it is `not_created`,
   do not poll; correct the request only if `failure.next_action` is
   `correct_request`, otherwise report and stop. When it is `unknown`, stop and
   report without polling or retrying. Status and collect can report
   `admission: "unknown"` alongside valid terminal evidence for an existing task.
3. If `status` is `timed_out`, follow the continuation procedure before treating
   pending publication as a reason to wait. `continuation.resumable: true`
   permits a linked successor even when no outcome has been published.
4. If `publication` is committed and `outcome.verdict` is committed, return
   the result and its payload/evidence descriptors.
5. If `outcome.verdict` is rejected, report a terminal rejected result with its
   refusal and evidence. Do not retry automatically.
6. If `pending.kind` is `interaction`, handle the returned questions using
   [interaction guidance](interaction.md) before waiting again. Otherwise, if
   `liveness` is queued or running or publication is pending, observe the same
   task with a finite watch interval.
7. If authentication is `blocked`, preserve that neutral state and tell the
   caller what prerequisite is unavailable without inventing a login command.
8. Never relaunch an uncertain task by guessing. Observe only a task whose
   record is known to exist; the `task_record` decision above takes precedence.

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
