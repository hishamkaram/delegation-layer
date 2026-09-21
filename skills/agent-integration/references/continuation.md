# Continuation and timeout recovery

Each task is one provider turn. A time limit ends that turn; it does not grant
permission to launch the same task again. The durable task record keeps the
exact provider session identity and the stop evidence.

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
delegate --root "$STATE_ROOT" continue \
  --task PREDECESSOR_TASK_ID \
  --brief FOLLOW_UP_BRIEF \
  --budget 30m \
  --json
```

The follow-up brief is optional. If it is omitted, Delegation Layer reuses the
validated predecessor brief. A successful continuation returns a new task ID
and links it to the predecessor. Observe and collect the new ID separately.

`mode` describes the provider contract:

- `native` uses the provider’s exact persisted conversation/session ID;
- `checkpoint` requires a provider checkpoint and is resumable only when the
  response explicitly says so;
- `unsupported` cannot be continued safely.

Do not use a guessed session ID, a provider-specific resume command, or a new
independent dispatch to recover a timeout. The CLI accepts either a validated
terminal outcome or durable supervisor-ended budget-stop evidence for the
session handoff; without one of those proofs it refuses continuation. Inspect
the predecessor again rather than bypassing that boundary.
