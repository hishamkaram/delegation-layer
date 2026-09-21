# Read-only task

```sh
STATE_ROOT="${DELEGATE_ROOT:-$HOME/.local/state/delegation-layer}"
BRIEF="$(mktemp)"
printf '%s\n' 'Inspect the workspace and summarize the current test failures. Do not edit files.' >"$BRIEF"
delegate --root "$STATE_ROOT" dispatch --auto \
  --brief "$BRIEF" --cwd "$PWD" --permission read-only --budget 15m --json
```

Save the returned `task_id`, then use `status` and `collect` with the same
state root.
