# Optional managed Git workspaces

Managed workspaces are an optional Delegate feature for tasks that need a
retained Git worktree. Ordinary delegation, including ordinary work in a
non-Git directory, is unchanged. Git is required only when a request creates
or attaches to a managed workspace (`--isolation git-worktree` or
`--workspace ID`), or creates or removes a [Git checkpoint](#git-checkpoints).

This reference is self-contained guidance for using managed workspaces.

Provider-native permissions still apply inside an isolated checkout. A provider
may edit files but be unable to stage or commit because linked-worktree Git
metadata lives outside that checkout. Leave edits uncommitted for capture;
do not bypass the provider's permissions. When a commit is needed, the Lead can
explicitly authorize a bounded Git command through `workspace exec`.

Delegate owns durable records, process lifetimes, capture, and recovery; use
the public commands below to observe and manage them.

## Choose the boundary

For independent parallel write work, choose `--isolation git-worktree`
automatically unless the user explicitly chooses shared coordination. The
model chooses the fan-out and task scope; there is no fixed task-count cap and
no TTL eviction. Shared coordination means the caller deliberately accepts a
common checkout and its write conflicts. A separate managed workspace is the
safer default for independent edits.

Automatic provider-agnostic routing still goes through Delegate. A native
subagent in the current harness remains allowed, and an explicit bypass is
respected. This workspace feature does not force external routing or replace a
user's explicit integration choice.

The coordinator owns one workspace usage reservation at a time. A provider
turn, continuation, read-only review, or verification operation waits for the
canonical native closure and terminal capture of the current owner. A queue
row ending or a provider outcome alone is not closure evidence.

## Source and checkout

Create a managed workspace from a committed source base:

```text
delegate --root ROOT dispatch --isolation git-worktree --cwd ABS \
  [--base REV] [--workspace-budget DURATION] ... --json
```

`--base` resolves to a commit; the default is `HEAD`. Dirty and untracked
source changes are observed but excluded from the managed checkout. The
provider runs in the retained detached worktree, with the source-relative
working subdirectory preserved. The source may itself be a managed checkout,
such as a sibling in the same workspace container. After provisioning, the new
workspace depends on the shared repository, not on the source checkout, so
removing that source does not block capture, release, or forget. A request that attaches to an existing
workspace uses its recorded checkout and source identity:

```text
delegate --root ROOT dispatch --workspace WORKSPACE_ID ... --json
```

Public `--workspace` cannot be combined with `--cwd`, `--base`, or
`--isolation`. It is an attachment by durable ID, not a path alias. The Lead
owns landing decisions; Delegate does not automatically apply changes, commit,
create branches, or merge them.

Git-content capture is not a complete filesystem backup, index partition, or
worker commit-history backup. It does not promise ignored runtime files,
submodule interiors, empty directories, extended attributes, or LFS payloads
behind pointers. Read paths from `workspace.artifacts` and verify each
descriptor’s length and SHA-256 before using its bytes. Entries marked
`forgetting`, `forgotten`, or `omitted` have no readable path.

Shared Git refs, config, and other global resources mean that
worktree separation is cooperative checkout isolation, not a security sandbox
or read-only enforcement. A clean Git status does not prove that a provider
made no write outside the promised content scope.

## Public commands

The CLI currently exposes these managed-workspace commands:

```text
delegate --root ROOT workspace list [--after ID] [--limit 1..50] --json
delegate --root ROOT workspace status ID [--operation OP] [--watch DURATION] --json
delegate --root ROOT workspace exec ID --budget DURATION --json -- ARGV...
delegate --root ROOT workspace release ID [--discard] [--budget DURATION] --json
delegate --root ROOT workspace forget ID [--budget DURATION] --json
```

`workspace exec` passes `ARGV...` directly to the supervised helper; the
`--` delimiter makes the arguments literal and no shell is inserted. Its
initial response is asynchronous and uses exit code `3` while the operation
is pending. `status` can watch one operation, while `list` returns summaries
and a cursor for the next page. `--limit` is between 1 and 50.
Follow the [workspace acknowledgement decisions](result-handling.md#managed-workspace-results)
before retrying a mutation. A busy precondition and an accepted pending action
have different next actions; an accepted action can have no helper ID yet.

The workspace helper budget defaults to 5 minutes, is persisted with the
workspace or operation, and can be overridden by the corresponding budget
flag. This auxiliary budget is separate from and does not renew the provider
task budget. The existing `codex:review` default remains 1 hour.

`continue --task PREDECESSOR_ID` inherits the predecessor's managed workspace,
working directory, and provider session, while creating a new task ID. The new
turn runs in the calling environment, so the exact recorded session must be
available there; see [continuation](continuation.md). The predecessor's
admitted records stay unchanged. A changed provider request is a new turn; it
must not mutate the predecessor's immutable records. A retained workspace can be used by a
read-only review by attaching its ID, for example:

```text
delegate --root ROOT dispatch --provider codex:review \
  --workspace WORKSPACE_ID --review-target TARGET --brief ABS_BRIEF \
  --permission read-only --budget 1h --json
```

## Capture and verification

The provider outcome and Git-content `CaptureReceipt` are separate immutable
facts. Normal capture covers the initial committed base through the captured
tree, including worker commits and added untracked files that belong to the
Git-content view. It uses a private temporary Git index and records an owned
base pin plus tree/snapshot reference.

Each provider turn also records its actual starting tree, `HEAD`, and index
identity just before launch, so Lead edits made between turns belong to the
next turn's start. The index identity covers every entry's mode, object, stage
(including conflicts), path, assume-unchanged or skip-worktree flag, and
intent-to-add (`git add -N`) state, but not stat caches or `HEAD`. A task capture's `candidate.turn` compares that start with the
final capture: `content`, `head`, and `index` are `changed` or `unchanged`, and
`baseline_ref` keeps the start tree. This is observed evidence, not
enforcement: a read-only turn that commits or edits is reported, not rejected,
and writes reverted before capture or ignored files are not observed.
`status: "unavailable"` with `reason_code: "baseline_unavailable"` means no
baseline exists, for example for a turn started before this evidence existed;
treat it as unknown, never as unchanged. A capture without `candidate.turn`,
such as one recorded before per-turn evidence existed or a release or exec
recapture, is also unknown, never unchanged. A failed baseline refuses the
provider launch for that task.

An observed turn adds `turn-diff.patch` and `turn-name-status.z`, which diff
that turn's start tree to the final tree with the same bounds as the
cumulative artifacts. Use them to see which files the current turn touched;
they exclude earlier turns and Lead edits made before its launch. Use the
cumulative `diff.patch` and `name-status.z`, from the workspace base, to land
or review the whole delegated result. Report a read-only turn whose `content`,
`head`, or `index` is `changed` as a mutation, even when its turn file list is
empty, and report an unavailable turn as unknown.

`status` and `collect` for a task report that task's own capture, even after a
later turn has used the workspace. The workspace `state` still reflects the
resource now, so a sealed earlier capture does not make a busy, released, or
forgotten workspace usable. `workspace status` reports the latest use.
Optional binary patch and name-status artifacts are bounded to 8 MiB each; an
overflow is omitted, never silently truncated. Verification diagnostics retain
an 8 MiB prefix per stream, mark truncation explicitly, and continue draining
the stream. A verification command's exit is verification evidence, not the
provider task outcome.

A failed helper may retain an `error.txt` diagnostic with a bounded, validated
descriptor. It is operation evidence, not captured Git content or a provider
answer. Diagnostic write failure does not turn a known helper failure into
success. Temporary result-read errors keep capture retryable.

The normal timeout path is ordered: the provider's native scope closes, the
managed worker's exact `Wait` is recorded, then a new finite auxiliary capture
scope runs. Recovery may instead record a validated absence disposition; it
proves closure without inventing a native `Wait` or provider success. The
expired provider scope is never renewed or reused. If capture
is unavailable after closure, the closure evidence can still preserve a valid
continuation decision; it does not rewrite the provider outcome.

## Git checkpoints

A checkpoint records the current Git content of one checkout as a synthetic
commit pinned by the owned ref `refs/delegate/ROOT_ID/checkpoints/CHECKPOINT_ID`.
It does not move `HEAD`, create or switch a branch, or change the real index.
The commit's tree is the full current Git-content view, built through a private
index with the capture algorithm above, so the same exclusions apply. Its single
parent is the `HEAD` the helper observed. Author, committer, and timestamp are
fixed Delegate values; trailers carry the checkpoint ID and request digest.
This is unrelated to a provider continuation `mode: "checkpoint"`.

```text
delegate --root ROOT checkpoint create --cwd ABS [--budget DURATION] --json
delegate --root ROOT checkpoint create --workspace ID [--budget DURATION] --json
delegate --root ROOT checkpoint status CHECKPOINT_ID [--operation OP] [--watch DURATION] --json
delegate --root ROOT checkpoint list [--after CHECKPOINT_ID] [--limit 1..50] --json
delegate --root ROOT checkpoint remove CHECKPOINT_ID [--budget DURATION] --json
```

- `create` names exactly one target: `--cwd` for an absolute ordinary Git
  checkout directory, or `--workspace` for a managed workspace ID. Nothing is
  inferred from the current directory. Select a managed checkout only by
  `--workspace ID`; never pass its path as `--cwd`.
- `status` and `remove` take the checkpoint ID `C` from the create response.
  `list` takes no target and runs no Git command.
- Only `create --cwd` may initialize a new state root. Every other checkpoint
  command needs the existing root and fails with exit `1` without creating it.
  Use one state root for every command about the same checkpoint.
- `--budget` is the finite helper budget (default 5m). It is persisted with the
  operation and does not renew any provider task budget. `--watch` exists only
  on `status`.

A managed checkpoint is a workspace use. It needs the workspace's use owner and
holds that workspace's usage reservation while its helper runs, so a concurrent
turn or `workspace exec` on that workspace gets the normal busy response. It
closes without content capture. Create it while the workspace is retained.

Delegate does not restore, apply, merge, or land a checkpoint into any checkout.
It does not control nested delegation budgets or lifetimes either. A fork from
a checkpoint is an ordinary new managed dispatch with its own task budget.

### Checkpoint before fork

Use this sequence to start an isolated worker from current uncommitted work.
Save every response and exit code once; follow
[checkpoint results](result-handling.md#checkpoint-results) for each one.

1. Run `checkpoint create` with one target. Exit `3` with `checkpoint.id` and
   `next_action: "watch_checkpoint"` means the request was accepted. It does
   not mean the content was captured. Save that exact ID as `C`.
2. Run `checkpoint status C --watch 30s --json` until the exit is not `3`.
   Never repeat `create` to observe it.
3. Fork only when `status` exits `0` and `checkpoint.current.state` is
   `checkpointed` and `checkpoint.current.usable` is `true`. A `commit_oid` or
   `snapshot_ref` alone is not proof: an unavailable creation can still name a
   retained candidate.
4. Dispatch the worker with the qualified `checkpoint.current.commit_oid` as
   the existing `--base` of a new managed workspace. Use an ordinary checkout of
   the same repository as `--cwd`, normally the source the Lead already uses:

   ```text
   delegate --root ROOT dispatch --auto --brief ABS_BRIEF --cwd ABS_SOURCE \
     --isolation git-worktree --base COMMIT_OID \
     --permission workspace-write --budget 30m --json
   ```

5. Remove the checkpoint explicitly when the Lead no longer needs it.

`status` exit `0` means a successful metadata observation, not an available
checkpoint. `usable` is `true` only for a `checkpointed` creation with no
registered removal attempt, including a failed one. It is derived from
Delegate's records; it is not proof that an external Git command left the ref
in place.

If `checkpoint create --workspace ID` returns exit `3` with
`reason_code: "requested_action_not_authorized"` and `retry_create`, no
checkpoint was accepted and no ID was assigned. When that workspace is in use
by the turn you are running in, do not wait for yourself, retry in a loop, or
retry through `--cwd`. Finish and report; the Lead creates the checkpoint after
your turn closes. When another use holds it, the Lead observes that workspace
until it is retained again, then retries the same create once.

Checkpoint refs outlive `workspace release` and `workspace forget`: the forget
inventory covers only the workspace's own base, capture, and baseline refs.
`checkpoint remove C` is the only command that removes a checkpoint ref.
Removal needs only the shared Git common directory, so it works after the
source checkout or managed checkout is gone. A missing or replaced common
directory makes that removal attempt `unavailable`, never `removed`. Creation
history stays observable after removal: `current.state` keeps the creation
disposition, while `current.usable` becomes `false` and `current.removal`
shows the latest attempt.

## Retention and forgetting

A created managed checkout is retained for every outcome, including no-op and
provider-failure outcomes, until the Lead explicitly releases it. A failed
admission before checkout creation has no checkout to retain. Normal release
captures final content before exclusive durable cleanup. A failed capture or
identity check retains the checkout and evidence for retry. `--discard` accepts
loss of uncaptured checkout content; it does not waive native closure,
ownership, or identity proof.

Release leaves the pinned refs/blobs, turn-baseline refs, and capture receipts
as evidence. After a proven release, `workspace forget ID` writes an exact
inventory intent before removing only matching owned refs and optional
artifacts by identity/CAS. It
retains capture receipts, tombstones, all provider outcomes, raw evidence, and
payload descriptors. Readers should label an intentionally forgotten item as
`forgotten`, not as corruption.

Release and forget keep a compact container anchor, ownership marker, and durable
journal metadata. They remove the checkout and the exact evidence authorized by
the forget inventory. A failed forget attempt can retain a diagnostic written
after its inventory was committed; do not delete these internal paths manually.

Unknown or busy workspace state is retained and receives no cleanup. There is
no TTL cleanup, workspace renaming, global Git prune, or unowned goroutine
unlink path.

## Recovery and review scope

When provision is pending and Delegate proves the provider task was not created,
the response contains `task_record: "not_created"`,
`failure.code: "workspace_provision_pending"`, and
`failure.next_action: "watch_workspace"`. Run the returned recovery command
with the same state root and workspace ID. Do not poll a nonexistent task or
start the provider again. An explicit task ID can have unknown creation state;
follow the returned failure instead of inferring absence.

`state: "preparation_failed"` with `reason_code: "admission_not_launched"`
means the admission closed without a registered checkout or provider launch.
Report its `stop_and_report` failure; do not keep watching it as provisioning.
A short observation window does not cancel accepted scheduling or renew the
helper's deadline. Continue observing an accepted pending action as directed.

After provision, keep the returned task ID and the same admission inputs when
retrying a request proven not created. Delegate gives a safely closed attempt
a new helper identity and deadline; it never reopens the earlier worker or
renews its deadline. Changed provider input needs a new task ID. Partial setup keeps its
durable source and ownership records. Explicit release removes only an exactly
verified owned checkout; changed paths or unverifiable Git metadata remain
retained with an error. Recovery never creates a checkout just to delete it.

Review the whole delegated turn against its recorded initial base and captured
tree, including worker commits. For that scope, use an ordinary read-only task
with explicit base/snapshot refs and a `git diff BASE TREE` instruction. Native
Codex review targets have their own scope: `uncommitted` covers current staged,
unstaged, and untracked changes; `base:BRANCH` compares a branch; `commit:SHA`
reviews a commit. An uncommitted review does not cover changes already committed
by the worker. Do not pass a tree reference as a branch or assume a native
target reviews the complete delegated turn. Preserve an explicitly requested
native review target and state its scope.

## Checkpoint response

Checkpoint commands use the versioned envelope with `command: "checkpoint"`
and no `task_id` or `outcome`. The response fields are:

```text
checkpoint: {
  id?, operation_id?,
  current?: CheckpointDescription,
  operation?: CheckpointRemovalDescription,
  reason_code?, next_action?
}
checkpoints: [CheckpointDescription]   (list only; [] when empty)
checkpoint_cursor?: string             (list only; next --after value)

CheckpointDescription: id, selection ("ordinary" | "managed"), workspace_id?,
  cwd?, state ("pending" | "checkpointed" | "unavailable"), reason_code?,
  snapshot_ref?, commit_oid?, usable, removal?: CheckpointRemovalDescription
CheckpointRemovalDescription: operation_id, attempts, state
  ("pending" | "removed" | "nothing_to_remove" | "unavailable"), reason_code?
```

Creation `state` records immutable history; `usable` is derived availability,
true only for a checkpointed creation without a registered removal attempt.
A retained candidate can have commit/ref fields even when unavailable; those
fields alone never authorize a fork. `current.removal` is the latest attempt;
`operation` is the selected attempt, whose own result controls its exit.
`attempts` counts all registered removal attempts.

Acknowledged IDs survive later read faults; unreadable descriptions are omitted.
Apply [checkpoint results](result-handling.md#checkpoint-results) to the whole
response and exit, preserving nested and command-level reasons separately.
