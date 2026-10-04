# Parallel reviews and debates

## Keep each request identifiable

Choose the fan-out needed for the user's task. Each participant needs its own
brief and response association, even when two participants use the same provider.
Record the request label, provider, exact requested model/effort, workspace,
permission, state root, and response together. The label identifies the attempt
before admission; the returned task ID identifies it afterward.

Prefer independent tool calls: each tool result belongs to its invocation.
When a shell must launch requests concurrently, redirect each invocation to its
own files. Never use `dispatch ... & dispatch ... & wait` with shared stdout.

After preparing two independent briefs, a portable capture pattern is:

```sh
RECEIPTS="$(mktemp -d)" || exit 1
# Set these absolute paths and catalog IDs in this same tool invocation.
# BRIEF_A and BRIEF_B contain the real independent assignments.
(
  delegate --root "$STATE_ROOT" dispatch --provider "$PROVIDER_A" \
    --brief "$BRIEF_A" --cwd "$WORKSPACE_A" \
    --permission read-only --budget 30m --json \
    >"$RECEIPTS/review-a.json" 2>"$RECEIPTS/review-a.stderr"
  printf '%s\n' "$?" >"$RECEIPTS/review-a.exit"
) &
(
  delegate --root "$STATE_ROOT" dispatch --provider "$PROVIDER_B" \
    --brief "$BRIEF_B" --cwd "$WORKSPACE_B" \
    --permission read-only --budget 30m --json \
    >"$RECEIPTS/review-b.json" 2>"$RECEIPTS/review-b.stderr"
  printf '%s\n' "$?" >"$RECEIPTS/review-b.exit"
) &
wait
printf 'Read each response separately in %s\n' "$RECEIPTS"
```

Keep the request-to-label mapping alongside those files. Include any exact
requested model/effort in its invocation. Read both exit codes, stderr, and JSON
responses independently; shell `wait` is not proof that either dispatch succeeded.
Poll only tasks with `task_record: created`. A returned task ID with
`task_record: not_created` does not identify an observable task.

## Wait within the current turn

Concurrent dispatch does not require background collection. Collect each task
in the foreground with a bounded watch; for example, `collect TASK_ID --watch
30s --json`. Exit 3 means publication is still pending: keep observing that task,
handling its budget-stop and continuation fields as described in the main skill.
Do not end a headless turn while a shell background job is meant to finish the
requested work. The host may close its session at that point and no later
notification is guaranteed. A user-requested detach is different: report the
pending task IDs and state root explicitly without claiming completion.

## Compare actual results

Collect each admitted task to its authoritative terminal outcome. Present each
participant's findings with its task ID and evidence. Keep a blocked or failed
participant visible; one successful review does not satisfy a requested pair.
Do not attribute your own analysis to an unavailable participant.

## Challenge and follow up

Independent opening reviews may run concurrently. A challenge depends on the
actual earlier result: collect it first, then include its relevant findings and
evidence in the other participant's next brief. Never claim a cross-check merely
because two initial reviews were requested.

For an existing participant, use `continue --task TASK_ID` with that participant's
own predecessor and the follow-up brief. This creates a new task for the same
provider conversation; save the new ID separately. Use the continuation result
and [continuation guidance](continuation.md), including advertised resumability
after a budget stop. Never use another participant's task ID or restart a
conversation to imitate a follow-up. If the first turn is blocked, report that
the dependent challenge or follow-up did not run. Other independent assignments
may still proceed.
