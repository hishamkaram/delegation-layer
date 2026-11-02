# Continuation and timeout recovery

Each task is one provider turn. A time limit ends that turn; it does not grant
permission to launch the same task again. The durable task record keeps the
exact provider session identity and the stop evidence.

Apply [result handling](result-handling.md#observe-and-collect) first.
A terminal outcome remains authoritative. Cancellation, `stop_and_report`,
and failed or uncertain admission take precedence over automatic continuation,
even when a response includes `resumable: true`.

When `status` or `collect` reports a timed-out turn with:

```json
"continuation": {
  "resumable": true,
  "mode": "native",
  "predecessor_task_id": "..."
}
```

run:

```sh
delegate continue \
  --task PREDECESSOR_TASK_ID \
  --brief FOLLOW_UP_BRIEF \
  --budget 30m \
  --json
```

A timed-out turn can have `publication: "unknown"` and no `outcome` while
`continuation.resumable` is true. Use that continuation decision; do not wait
indefinitely for a publication that the interrupted turn cannot complete.

The follow-up brief is optional. If it is omitted, Delegation Layer reuses the
validated predecessor brief. A successful continuation returns a new task ID
and links it to the predecessor. Observe and collect the new ID separately.
Follow the main skill's [root rule](../SKILL.md#3-prepare-separate-paths-and-a-finite-brief):
ordinary continuation omits `--root` in the same user/configuration context;
an explicit custom or recorded historical root must be retained exactly, even
if a returned command hint omits it. Permission is inherited; do not
add `--permission` to `continue`. Apply
[task creation](result-handling.md#task-creation) before retrying or observing.
Any authorized admission retry remains the argv-equivalent `continue`;
never replace it with a fresh dispatch.

Run `continue` with the same state root as the predecessor. The new turn uses
the environment of the process that calls `continue`, not an environment saved
by the predecessor, so the exact recorded session must be available there. If
`continue` fails because it is not, report the failure and stop. Do not copy
credentials or settings from another environment, choose a fresh session or
another provider, or dispatch again to repair the failure.

A Team-targeted predecessor carries its recorded selection context forward
without reading the catalog. Current Member defaults, deletion or a stale
project default cannot retarget it. Do not add `--team-scope`, `--team`,
`--member` or `--project-root` to continuation; supported explicit model,
effort and budget overrides keep their existing meaning. A separately
authorized new dispatch with a new ID instead resolves latest Member defaults;
see [Member selection](command-reference.md#using-a-member).

When the predecessor is bound to a managed workspace, `continue --task` carries
the recorded workspace, working directory, and provider session to the new task
ID. The new turn runs in the calling environment. It does not mutate the predecessor's immutable request or
reservation. A changed provider request is a new turn and must pass the normal
workspace-use reservation. Read [managed workspace guidance](workspaces.md)
before attaching another provider, review, or verification operation.

Resource and pre-admission workspace refusals follow the same
[creation decisions](result-handling.md#task-creation). Keep the original
predecessor, brief, budget, and root. A created successor is only observed.

**Busy managed workspace after a timeout.** This is the only case where an
unavailable continuation may be waited for instead of reported. It applies
only when a `timed_out` task reports all of `continuation.resumable: false`,
`continuation.reason` exactly `"managed workspace is busy"`,
`workspace.state: "busy"`, and `workspace.next_action: "watch_workspace"`.
Another reservation may still hold the workspace; do not infer which use. Wait
only when further work needs to continue this task, and return a committed
result normally. Do not wait when a cancellation (`failure.code:
"task_cancelled"`), a concrete fault, or `stop_and_report` applies to the task
or the workspace. If a successor from this predecessor was already created,
observe that successor instead; if its creation is uncertain, follow the
`task_record` stop rule in [result handling](result-handling.md).

1. Watch the workspace with the same root and a finite duration, using
   `workspace.id` from the task response; never guess an ID or path:

   ```sh
   delegate workspace status \
     WORKSPACE_ID --watch 30s --json
   ```

   Repeat the watch only while it reports `state: "busy"`.
2. When it reports `state: "retained"`, run a fresh `status` for the same
   predecessor task ID.
3. Only if that status now reports `continuation.resumable: true` and the task
   rules above still permit it, run the exact `continue --task PREDECESSOR_ID`
   once. Otherwise report its reason and stop.

Never run `continue` while `resumable` is `false`. Report any other reason,
workspace state, or fault under the existing guidance; do not reactivate a
`released` or `forgotten` workspace. An `undetermined` workspace view is
unobserved and follows [managed workspace results](result-handling.md#managed-workspace-results);
it is neither a terminal failure nor permission to continue. Do not dispatch,
fall back to another provider, repair the environment, or mutate the workspace
to free it. Availability is not guaranteed; do not wait indefinitely.

`mode` describes the provider contract:

- `native` uses the provider’s exact persisted conversation/session ID;
- `checkpoint` requires a provider checkpoint and is resumable only when the
  response explicitly says so;
- `unsupported` cannot be continued safely.

Do not use a guessed session ID, a provider-specific resume command, or a new
independent dispatch to recover a timeout. The CLI accepts a validated
terminal outcome, a durable Pueue-ended budget stop backed by a guardian
lifetime-termination record with independently proven empty scope, or canonical
closed lifetime evidence of any closure kind together with a free runner lease
for a turn with no seal or outcome. A sealed predecessor must be collected
first. A queue-row end or a timestamp alone never qualifies; without one of those
proofs it refuses continuation. Inspect the predecessor again rather than
bypassing that boundary.

For a managed workspace timeout, capture is a new finite auxiliary operation
after canonical closure. The normal path proves the provider scope empty and
records the managed worker's exact `Wait`; recovery can instead use a validated
absence disposition or previous-boot termination without inventing a native
`Wait`. Capture never renews the expired provider deadline or reuses its scope. Closure with an
unavailable capture can still preserve continuation evidence; it does not
change the provider outcome.


## Interrupted turn

A host restart can end a provider turn before it publishes anything. Delegate
reports an interruption only when the task's stored publication state is still
unknown, with no provider exit or outcome, its canonical lifetime closure is
proven, and its runner lease is released. A terminated budget stop is checked
first and reports `timed_out` instead, as does a closure whose recorded
termination cause is the budget. Closure proves only that the provider
scope is gone; it is not provider success, and no answer or output from that
turn is accepted. Do not check these facts yourself or infer eligibility from
missing files: obey the returned `status`, `failure`, `continuation`, and
`pending` fields. For an eligible task, after real outcomes and concrete errors
are checked, `status` and `collect` report `status: "interrupted"` with
`publication: "unknown"`, no `outcome`, and
`failure.code: "task_interrupted"`. `status` exits `0`; `collect` exits `1`.
An interruption may also carry `pending.kind: "supervisor-command"`; that
independent observation means only that a supervisor command has not
finished, so follow the interruption's `failure` and `continuation` as usual.
Do not infer an interruption from `publication: "unknown"` alone: a response
with `pending.kind: "publication"` is still pending publication and carries no
interrupted resume offer, so handle it as pending under
[result handling](result-handling.md). Outcomes, `timed_out`, interaction,
`lifetime-finalization`, and ordinary pending exit `3` keep their meaning.

| Response | Action |
|---|---|
| `failure.code: "task_interrupted"`, `failure.next_action: "continue_task"`, and `continuation.resumable: true` (all three) | Run the `continue` above for that predecessor with the original root and a finite budget, then observe the successor. |
| `interrupted` with any other combination, including `failure.next_action: "stop_and_report"` or `continuation.resumable: false` | Report the reason and stop. |
| `timed_out` with `failure.code: "task_cancelled"` (user stop and budget stop, no outcome) | `stop_and_report`; `continuation.resumable` is `false` and no `continue_command` is returned. Report it. `collect` exit `3` here is a finite watch, not success and not a reason to keep waiting. |
| Any other operational error | It is not an interruption; apply [result handling](result-handling.md). |

`continuation.resumable: true` alone never revives an attempt that returned
`stop_and_report`, and a failed admission keeps its `task_record` rules.
Never continue a cancelled task automatically or start a fresh provider
fallback. A user stop keeps `interrupted` / `task_interrupted` for a closed
interrupted task, now with `stop_and_report` and `resumable: false`. A
committed or rejected outcome stays authoritative and is never replaced by
`task_cancelled`. A later, explicit user request to `continue` is a separate
action and still validated against real closure and the recorded session.

The successor uses the predecessor's observed provider session from
`provider.ref`. Only when `provider.ref` is genuinely absent, for any eligible
predecessor and not only an interrupted one, does `continue` fall back to the
validated immutable session that turn was asked to resume. The response reports
the source in `continuation.session_source`: `observed` means `provider.ref`
supplied the identity, and `recorded_input` means the requested session supplied
it. Neither value is evidence the provider saved any message or resumed
successfully. A missing task reference, or an unreadable, malformed, or mismatched
record, refuses. Never search ancestors or rerun the original task.

## Follow up after normal completion

A validated terminal outcome, committed or rejected, can also support a new
turn when the provider supports continuation, its exact session was recorded,
and the runner/session reservation has been released. Not every rejection has
a usable session. A normal completion without a budget stop need not expose
`continuation.resumable` in `status` or `collect`. Use `continue` with the exact
predecessor and a finite budget, then apply its task-creation and failure
response as described in [result handling](result-handling.md).

If the response says `stop_and_report`, stop that attempt; do not automatically
retry it. A caller may make a later request after observing the predecessor.
Native `codex:review` tasks cannot be continued.
