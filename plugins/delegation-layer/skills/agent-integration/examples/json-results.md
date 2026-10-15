# JSON decision sketch

```sh
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" status "$TASK_ID" --json
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" collect "$TASK_ID" --watch 5s --json
```

Apply [result handling](../references/result-handling.md) to each response and
follow [Observe and collect](../SKILL.md#5-observe-and-collect). Those sections
own the decision order for task creation, failures, timeout continuation,
terminal outcomes, pending interaction, and bounded collection. Read them
before interpreting these commands; pending publication alone is not a reason
to wait when the task needs an answer or timeout recovery.
