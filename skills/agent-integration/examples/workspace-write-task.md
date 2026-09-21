# Authorized workspace write task

```sh
STATE_ROOT="${DELEGATE_ROOT:-$HOME/delegation-state}"
delegate --root "$STATE_ROOT" dispatch --auto \
  --brief "$PWD/delegated-task.md" --cwd "$PWD" \
  --permission workspace-write --budget 30m --json
```

Use `workspace-write` only when the caller authorized edits in the selected
workspace. Delegate requests unattended native approval for commands and edits.
The provider owns its tools and access restrictions; the workspace is not an
independent sandbox. Save the returned task ID, then use the same state root
with `status` and `collect`; do not run the provider directly after a denial.
