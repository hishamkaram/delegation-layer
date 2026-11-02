# JSON decision sketch

Use the created task's exact ID as `TASK_ID` and follow the main skill's
[root rule](../SKILL.md#3-prepare-separate-paths-and-a-finite-brief).

```sh
delegate status TASK_ID --json
delegate collect TASK_ID --watch 5s --json
```

Apply [result handling](../references/result-handling.md) to each response and
follow [Observe and collect](../SKILL.md#5-observe-and-collect). Those sections
own the decision order for task creation, failures, timeout continuation,
terminal outcomes, pending interaction, and bounded collection. Read them
before interpreting these commands; pending publication alone is not a reason
to wait when the task needs an answer or timeout recovery.

## Checkpoint responses

These abbreviated shapes use the delivered field names; `…` marks omitted
values. Apply [checkpoint results](../references/result-handling.md#checkpoint-results)
to the whole response and its exit code.

Accepted creation, exit `3`:

```json
{"schema_version":1,"command":"checkpoint","root_id":"…","admission":"unknown","liveness":"undetermined","publication":"unknown",
 "checkpoint":{"id":"CHECKPOINT_ID","next_action":"watch_checkpoint"}}
```

Busy refusal, exit `3`, with no accepted ID:

```json
{"checkpoint":{"reason_code":"requested_action_not_authorized","next_action":"retry_create"}}
```

Qualified checkpoint from `status`, exit `0`; only this state may be forked:

```json
{"checkpoint":{"id":"CHECKPOINT_ID","current":{"id":"CHECKPOINT_ID","selection":"ordinary","cwd":"/abs/source",
 "state":"checkpointed","snapshot_ref":"refs/delegate/ROOT_ID/checkpoints/CHECKPOINT_ID","commit_oid":"…","usable":true}}}
```

Exact removal attempt from `status CHECKPOINT_ID --operation OPERATION_ID`,
exit `0`. Creation history stays `checkpointed`, but `usable` is now `false`:

```json
{"checkpoint":{"id":"CHECKPOINT_ID","operation_id":"OPERATION_ID",
 "current":{"id":"CHECKPOINT_ID","selection":"ordinary","state":"checkpointed","usable":false,
  "removal":{"operation_id":"OPERATION_ID","attempts":1,"state":"removed"}},
 "operation":{"operation_id":"OPERATION_ID","attempts":1,"state":"removed"}}}
```

An empty `list` page, exit `0`, still has the array:

```json
{"schema_version":1,"command":"checkpoint","root_id":"…","admission":"unknown","liveness":"undetermined","publication":"unknown","checkpoints":[]}
```
