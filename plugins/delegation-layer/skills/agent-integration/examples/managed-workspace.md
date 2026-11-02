# Managed workspace example

Use this example for independent write work that should be isolated in a
retained Git worktree. The source must be a Git checkout and the state root
must remain outside provider-writable workspaces.
Commands use the CLI default under the main skill's
[root rule](../SKILL.md#3-prepare-separate-paths-and-a-finite-brief); preserve any
explicit custom or recorded historical root on all related commands.

```sh
SOURCE="$PWD"
BRIEF="$(mktemp)" || exit 1
RESPONSE="$(mktemp)" || exit 1
printf '%s\n' 'Implement the requested change and report the files changed.' >"$BRIEF"
printf 'Saved response: %s\n' "$RESPONSE"

if delegate dispatch --auto \
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
Each later block is a separate shell invocation. Replace `ABS_SAVED_RESPONSE`
with the printed absolute response path and the ID placeholders with the actual
returned IDs; reassign the path in each invocation.

```sh
RESPONSE="ABS_SAVED_RESPONSE"
if delegate status TASK_ID --json >"$RESPONSE.status"; then
  status_exit=0
else
  status_exit=$?
fi
printf '%s\n' "$status_exit" >"$RESPONSE.status.exit"
cat "$RESPONSE.status"
```

Read the saved status JSON and apply
[Observe and collect](../references/result-handling.md#observe-and-collect)
before running another command. Run the collection below only when that
guidance calls for it.

```sh
RESPONSE="ABS_SAVED_RESPONSE"
if delegate collect TASK_ID --watch 5s --json >"$RESPONSE.collect"; then
  collect_exit=0
else
  collect_exit=$?
fi
printf '%s\n' "$collect_exit" >"$RESPONSE.collect.exit"
cat "$RESPONSE.collect"
```

Apply that same decision order to the collection JSON before treating exit `3`
as pending. Cancellation, `failure.code: "task_cancelled"` or
`failure.next_action: "stop_and_report"` ends pending polling; do not continue
the task or dispatch a replacement. Bounded
[failure diagnosis](../references/result-handling.md#failure-diagnosis)
remains available. Keep observing the same task with a finite watch only when
the canonical guidance calls for it.

Before release, wait for the workspace to return to `state: retained` and inspect
its final capture. Only after the Lead explicitly chooses release, run:

```sh
RESPONSE="ABS_SAVED_RESPONSE"
if delegate workspace release WORKSPACE_ID \
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
`status` first and collected in the foreground, following the canonical
observation decisions before each next step. Exit `3` alone is not a reason
to keep polling. The Lead reviews the capture and owns landing any changes.
`workspace release` is explicit; no automatic apply, commit, branch merge,
TTL eviction, or provider-CLI fallback occurs.

## Fork from a checkpoint

Use this sequence when the worker must start from current uncommitted content
instead of a committed base. Run it from the Lead, not from inside a worker
that uses the same workspace. The [checkpoint guidance](../references/workspaces.md#checkpoint-before-fork)
owns the rules; [checkpoint results](../references/result-handling.md#checkpoint-results)
owns every response decision.

```sh
SOURCE="$PWD"
RESPONSE="$(mktemp)" || exit 1
printf 'Saved response: %s\n' "$RESPONSE"
if delegate checkpoint create \
  --cwd "$SOURCE" --budget 5m --json >"$RESPONSE.checkpoint"; then
  checkpoint_exit=0
else
  checkpoint_exit=$?
fi
printf '%s\n' "$checkpoint_exit" >"$RESPONSE.checkpoint.exit"
cat "$RESPONSE.checkpoint"
```

Exit `3` with `checkpoint.id` and `next_action: "watch_checkpoint"` means the
request was accepted, not captured. Use that `checkpoint.id` as
`CHECKPOINT_ID`. Do not run `create` again.
For the later checkpoint blocks, replace `ABS_SAVED_RESPONSE` with the absolute
response path printed by this checkpoint invocation.

```sh
RESPONSE="ABS_SAVED_RESPONSE"
if delegate checkpoint status CHECKPOINT_ID \
  --watch 30s --json >"$RESPONSE.checkpoint.status"; then
  checkpoint_status_exit=0
else
  checkpoint_status_exit=$?
fi
printf '%s\n' "$checkpoint_status_exit" >"$RESPONSE.checkpoint.status.exit"
cat "$RESPONSE.checkpoint.status"
```

Repeat that status command while it exits `3`. Fork only when it exits `0`,
`checkpoint.current.state` is `checkpointed`, and `checkpoint.current.usable`
is `true`. Use `checkpoint.current.commit_oid` as `COMMIT_OID` in the first
dispatch example, replacing `--base HEAD` with `--base COMMIT_OID`. Report a
`stop_and_report` result instead of forking.

When the Lead no longer needs the checkpoint, remove it explicitly:

```sh
RESPONSE="ABS_SAVED_RESPONSE"
if delegate checkpoint remove CHECKPOINT_ID \
  --budget 5m --json >"$RESPONSE.checkpoint.remove"; then
  checkpoint_remove_exit=0
else
  checkpoint_remove_exit=$?
fi
printf '%s\n' "$checkpoint_remove_exit" >"$RESPONSE.checkpoint.remove.exit"
cat "$RESPONSE.checkpoint.remove"
```

Exit `3` with `checkpoint.operation_id` and `watch_checkpoint` means removal
attempt `OPERATION_ID` was accepted. Observe that exact attempt with
`checkpoint status CHECKPOINT_ID --operation OPERATION_ID --watch 30s --json`,
saving its response and exit the same way. A `retry_remove` response has no
`operation_id` and accepted nothing.
