# Optional managed Git workspaces

Managed workspaces are an optional Delegate feature for tasks that need a
retained Git worktree. Ordinary delegation, including ordinary work in a
non-Git directory, is unchanged. Git is required only when a request creates
or attaches to a managed workspace (`--isolation git-worktree` or
`--workspace ID`).

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
working subdirectory preserved. A request that attaches to an existing
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
working directory, provider session, and recorded environment, while creating
a new task ID. A changed provider request is a new turn; it must not mutate the
predecessor's immutable records. A retained workspace can be used by a
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

## Retention and forgetting

A created managed checkout is retained for every outcome, including no-op and
provider-failure outcomes, until the Lead explicitly releases it. A failed
admission before checkout creation has no checkout to retain. Normal release
captures final content before exclusive durable cleanup. A failed capture or
identity check retains the checkout and evidence for retry. `--discard` accepts
loss of uncaptured checkout content; it does not waive native closure,
ownership, or identity proof.

Release leaves the pinned refs/blobs and capture receipts as evidence. After a
proven release, `workspace forget ID` writes an exact inventory intent before
removing only matching owned refs and optional artifacts by identity/CAS. It
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
