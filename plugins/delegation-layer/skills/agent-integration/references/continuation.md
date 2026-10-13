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
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" continue \
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
and links it to the predecessor. Observe and collect the new ID separately. Always retain the original state
root, even if a returned command hint omits it. Permission is inherited; do not
add `--permission` to `continue`. If this exact `continue` returns the
migration-required `not_created` response, run the same-root supervisor
migration once and then repeat that argv-equivalent `continue` once after the
successful migration response. The response field is named `retry_dispatch`,
but it does not authorize a fresh dispatch. Apply the remaining `task_record`
error rules in [result handling](result-handling.md) before retrying or
observing.

`mode` describes the provider contract:

- `native` uses the provider’s exact persisted conversation/session ID;
- `checkpoint` requires a provider checkpoint and is resumable only when the
  response explicitly says so;
- `unsupported` cannot be continued safely.

Do not use a guessed session ID, a provider-specific resume command, or a new
independent dispatch to recover a timeout. The CLI accepts either a validated
terminal outcome or a durable Pueue-ended observation backed by a guardian
lifetime-termination record with independently proven empty scope for the
session handoff; without one of those proofs it refuses continuation. Inspect
the predecessor again rather than bypassing that boundary.
