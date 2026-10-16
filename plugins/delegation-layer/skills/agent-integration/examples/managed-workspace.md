# Managed workspace example

Use this example for independent write work that should be isolated in a
retained Git worktree. The source must be a Git checkout and the state root
must remain outside provider-writable workspaces.

```sh
SOURCE="$PWD"
BRIEF="$(mktemp)"
RESPONSE="$(mktemp)"
printf '%s\n' 'Implement the requested change and report the files changed.' >"$BRIEF"

if delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" dispatch --auto \
  --brief "$BRIEF" --cwd "$SOURCE" \
  --permission workspace-write --isolation git-worktree \
  --base HEAD --workspace-budget 5m --budget 30m --json >"$RESPONSE"; then
  dispatch_exit=0
else
  dispatch_exit=$?
fi
printf '%s\n' "$dispatch_exit" >"$RESPONSE.exit"
cat "$RESPONSE"
```

Read the saved JSON before running another command. When
`task_record: "created"`, use its `task_id` as `TASK_ID` below. When no task
was created, follow the [result decisions](../references/result-handling.md#managed-workspace-results);
do not run task status or collection against that nonexistent record.

```sh
if delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" status TASK_ID --json >"$RESPONSE.status"; then
  status_exit=0
else
  status_exit=$?
fi
printf '%s\n' "$status_exit" >"$RESPONSE.status.exit"
if delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" collect TASK_ID --watch 5s --json >"$RESPONSE.collect"; then
  collect_exit=0
else
  collect_exit=$?
fi
printf '%s\n' "$collect_exit" >"$RESPONSE.collect.exit"
```

Continue observing that created task when collection exits `3`. Wait for the
workspace to return to `state: retained` and inspect its final capture. Only
after the Lead explicitly chooses release, run:

```sh
if delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" workspace release WORKSPACE_ID \
  --budget 5m --json >"$RESPONSE.release"; then
  release_exit=0
else
  release_exit=$?
fi
printf '%s\n' "$release_exit" >"$RESPONSE.release.exit"
```

Read the release acknowledgement. `watch_workspace` means observe this
accepted release; a `retry_release` acknowledgement means it was not accepted.
Follow the same result decisions rather than assuming either response is
completed cleanup.

Save each response and exit code once. A created task is observed with
`status` first and collected in the foreground. Exit `3` means pending, not
success; continue observing the same task. The Lead reviews the capture and
owns landing any changes. `workspace release` is explicit; no automatic
apply, commit, branch merge, TTL eviction, or provider-CLI fallback occurs.
