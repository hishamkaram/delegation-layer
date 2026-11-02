# Questions and guidance during an active task

Use this reference when an external task may need clarification or guidance
before it finishes, including planning, consultation, and debate. Use only
Delegate commands; do not open a terminal session with the provider.

## Choose the transport

Read the selected provider's `interaction` capability and
`interaction.supported_modes`. `clarification` and `steering` are independent.
Clarification answers a provider's native structured question; steering sends
new guidance to an open turn. The current `claude:print` profile advertises
clarification only; `codex:exec` advertises both. Use the returned capabilities
for the selected provider and mode rather than assuming the two are paired.
Add `--interactive` to dispatch when the chosen provider and permission mode
support the interaction you need. Keep the user's exact provider, model,
effort, workspace, permission, and budget.

Without a named provider, `dispatch --auto --interactive` selects among
providers that support interaction; it does not require steering. When steering
is needed, explicitly select a provider advertising `steering: true`. If a
named provider lacks the needed capability, explain that limit without denying
any supported clarification. When native continuation is available, offer a
later bounded turn through `continue` as an alternative. Never replace the
requested provider or change the interaction workflow silently.

In the worker brief, explicitly request **native structured clarification**:
"Use your native question tool to request clarification and await its answer.
Do not ask only in final text or a progress message and then sleep: the lead
can answer structured requests, not an unstructured prompt. If the question
tool is unavailable, report that blocker and finish instead of waiting."
Assign the actual work directly; the worker should not start another delegate.

No change is needed for self-contained tasks that do not need mid-turn input.
Continuation inherits interaction from its predecessor. `continue --task ID
--interactive` can enable it for a previously ordinary task.
That creates a new eligible turn; it does not enable a channel on an already
running task. Distinguish an unsupported provider capability from a task that
was not admitted with `--interactive`, a closed turn, or the absence of a native
question. A printed question alone is not a pending reply request.

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
   together with the exact answers. Submit the reply:

   ```sh
   delegate reply "$task_id" \
     --request "$request_id" --answers "$answers_file" \
     --id "$message_id" --json
   ```

6. Keep collecting. A receipt is not task completion. Only the task's
   authoritative `outcome` establishes the final result.

Follow the main skill's [root rule](../SKILL.md#3-prepare-separate-paths-and-a-finite-brief)
for dispatch, collection, reply, and steering. The examples use the CLI default;
preserve an explicit custom or recorded historical root for all of them.
The variables stand for actual values saved from this task;
reassign them in each shell invocation if your tool does not preserve variables.
A pending response without questions means keep collecting, not inspect OS
processes or dispatch a replacement task.

## Guide active work

When `interaction.capabilities.steering` is true and the task is active, put
additional guidance in a brief file. Allocate and save a new 32-character
lowercase hexadecimal `steering_message_id` for this new message, distinct from
reply IDs and earlier steering IDs. Submit it; for a lost response, follow the
[delivery rules](#interpret-delivery-precisely) with the same ID and content:

```sh
delegate steer "$task_id" \
  --brief "$guidance_file" --id "$steering_message_id" --json
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
