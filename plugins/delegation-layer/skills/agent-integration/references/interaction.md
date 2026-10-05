# Questions and guidance during an active task

Use this reference when an external task may need clarification or guidance
before it finishes, including planning, consultation, and debate. Use only
Delegate commands; do not open a terminal session with the provider.

## Choose the transport

Read the selected provider's `interaction` capability and
`interaction.supported_modes`. `clarification` and `steering` are independent.
Add `--interactive` to dispatch when the chosen provider and permission mode
support the interaction you need. Keep the user's exact provider, model,
effort, workspace, permission, and budget.

Without a named provider, `dispatch --auto --interactive` selects among
providers that support interaction. If a named provider does not support it,
use ordinary bounded turns and `continue` for discussion when native
continuation is available. Explain that questions cannot be answered during
that provider's active turn. Never replace the requested provider silently.

In the worker brief, explicitly request **native structured clarification**:
"Use your native question tool to request clarification and await its answer.
Do not ask only in final text or a progress message and then sleep: the lead
can answer structured requests, not an unstructured prompt. If the question
tool is unavailable, report that blocker and finish instead of waiting."
Assign the actual work directly; the worker should not start another delegate.

No change is needed for self-contained tasks that do not need mid-turn input.
Continuation inherits interaction from its predecessor. `continue --task ID
--interactive` can enable it for a previously ordinary task.

## Respond to questions

1. Save the admitted task ID and state root. Run `collect TASK_ID --watch 30s
   --json` with that same root. Exit 3 is pending, not completion. Collection
   returns early when there is an outstanding native question.
2. Read `interaction.pending_requests`. Use the returned request ID, question
   IDs, options, `multiple`, and `free_text`; do not invent or guess IDs.
3. Answer facts already supplied by the user or verified during the task.
   Ask the user for missing preferences, business decisions, new scope, or
   authorization. Do not guess an answer to keep the worker moving. A question
   reply does not approve tool access or change the task's permission mode.
4. Write one JSON object mapping **every question ID in that request** to an
   array of answer strings. A single-choice question takes one value. Use exact
   option labels unless `free_text` permits another answer. Example:

   ```json
   {"color": ["Blue"], "sections": ["Summary", "Evidence"]}
   ```

5. Allocate a new 32-character lowercase hexadecimal message ID and save it
   together with the exact answers. Submit once:

   ```sh
   delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" reply "$task_id" \
     --request "$request_id" --answers "$answers_file" \
     --id "$message_id" --json
   ```

6. Keep collecting. A receipt is not task completion. Only the task's
   authoritative `outcome` establishes the final result.

Use the same root expression for dispatch, collection, reply, and steering.
If the user explicitly chose another root, use that exact path for all of
them. The other variables stand for actual values saved from this task;
reassign them in each shell invocation if your tool does not preserve variables.
A pending response without questions means keep collecting, not inspect OS
processes or dispatch a replacement task.

## Guide active work

When `interaction.capabilities.steering` is true and the task is active, put
additional guidance in a brief file and submit:

```sh
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" steer "$task_id" \
  --brief "$guidance_file" --id "$message_id" --json
```

Steering is guidance to the existing turn. It does not change provider, model,
effort, workspace, permission, or budget. Once the turn ends, use `continue`
for another authorized turn instead. Do not cancel and redispatch simply to
send guidance.

## Interpret delivery precisely

| State | What it establishes |
| --- | --- |
| `queued` | Delegate durably accepted the message; native delivery is pending. |
| `unknown` | Delivery was reserved or attempted, but its result is uncertain. |
| `sent` | The native input write completed; no native acknowledgement is established. |
| `acknowledged` | A correlated native response acknowledged the message. This does not prove the model followed guidance. |
| `rejected` | The message could not be delivered or the native protocol rejected it. Read the reason. |

If a submission response was lost, repeat the **same message ID and exact
content** to recover its receipt. Never allocate a fresh ID to retry an
`unknown` delivery. Delegate will not replay an ambiguous native write.
Changed content with an existing ID is a conflict. Observe the task instead
of guessing whether it consumed the message.

`events TASK_ID --after SEQUENCE --watch 30s --json` returns a bounded event
page. Save the returned `cursor` for the next page. Events and interaction
state are progress evidence, not a replacement for `collect` and `outcome`.

Waiting for an answer consumes the original execution budget; it does not
pause or extend it. `interaction.deadline` shows the recorded wall-clock
boundary when execution has started. If the task ends while waiting, do not
reply to the old request. Follow the ordinary continuation guidance only when
Delegate reports the task resumable. Preserve any verified answers in the new
brief; uncertain delivery must not become an automatic replay.
