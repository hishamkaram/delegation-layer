# Result handling

Use the response fields independently. A successful control command is not
the same as a successful provider result.

1. Save `task_id` immediately after dispatch or continue.
2. If a dispatch or continue response does not confirm admission, preserve any
   task ID and report its bounded error. An uncertain admission must be
   inspected, never automatically dispatched again. Status and collect can
   report `admission: "unknown"` alongside valid terminal evidence.
3. If `status` is `timed_out`, follow the continuation procedure before treating
   pending publication as a reason to wait. `continuation.resumable: true`
   permits a linked successor even when no outcome has been published.
4. If `publication` is committed and `outcome.verdict` is committed, return
   the result and its payload/evidence descriptors.
5. If `outcome.verdict` is rejected, report a terminal rejected result with its
   refusal and evidence. Do not retry automatically.
6. Otherwise, if `liveness` is queued or running or publication is pending,
   observe the same task again with a finite watch interval.
7. If authentication is `blocked`, preserve that neutral state and tell the
   caller what prerequisite is unavailable without inventing a login command.
8. If a command reports an operational uncertainty, inspect the original task
   with `status` and `collect` before considering any action. Never relaunch an
   uncertain task by guessing.

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
