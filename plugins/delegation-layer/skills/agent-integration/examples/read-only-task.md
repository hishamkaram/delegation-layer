# Read-only task

Run from the intended workspace, outside Delegate's private state root.
Use a catalog provider ID in place of `--auto` when a destination was named.
Prepare paths and dispatch in this same shell invocation:

```sh
WORKSPACE="$PWD"
BRIEF="$(mktemp)" || exit 1
RESPONSE="$(mktemp)" || exit 1
cat >"$BRIEF" <<'EOF'
Inspect the workspace and summarize the current test failures. Do not edit files.
You are the assigned worker; perform this task without delegating it.
EOF
printf 'Saved response: %s\n' "$RESPONSE"
if delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" dispatch --auto \
  --brief "$BRIEF" --cwd "$WORKSPACE" \
  --permission read-only --budget 30m --json >"$RESPONSE"; then
  dispatch_exit=0
else
  dispatch_exit=$?
fi
cat "$RESPONSE"
printf '\nDispatch exit: %s; saved response: %s\n' "$dispatch_exit" "$RESPONSE"
```

Read [task creation](../references/result-handling.md#task-creation) before
running another command. Observe only `task_record: "created"`, using its
exact `task_id` with the same root. Follow
[observation](../references/result-handling.md#observe-and-collect) and the
[main skill](../SKILL.md#5-observe-and-collect) for status and collection.
An allocated ID with `not_created` or `unknown` is not permission to poll
or redispatch. Re-read the saved response when needed; never dispatch again
to recover output or its exit status.
