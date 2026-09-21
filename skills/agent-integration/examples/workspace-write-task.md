# Authorized workspace write task

```sh
STATE_ROOT="${DELEGATE_ROOT:-$HOME/.local/state/delegation-layer}"
delegate --root "$STATE_ROOT" dispatch --auto \
  --brief "$PWD/delegated-task.md" --cwd "$PWD" \
  --permission workspace-write --budget 30m --json
```

Use `workspace-write` only when the caller authorized edits in the selected
workspace. The CLI still owns provider selection, policy, admission, and
publication.
