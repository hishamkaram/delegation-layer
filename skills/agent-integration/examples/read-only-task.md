# Read-only task

```sh
BRIEF="$(mktemp)"
printf '%s\n' 'Inspect the workspace and summarize the current test failures. Do not edit files.' >"$BRIEF"
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" dispatch --auto \
  --brief "$BRIEF" --cwd "$PWD" --permission read-only --budget 15m --json
```

Save the returned `task_id`, then use `status` and `collect` with the same
state root.
