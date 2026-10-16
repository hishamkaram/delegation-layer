# Delegation Layer Invariants

This document establishes the normative operational invariants for the delegation layer. All agents, adapters, and tools must comply with these invariants across all executions.

## 1. Terminal Authority and Immutability
- **Sole Authority**: `outcome.json` is the sole terminal authority. `result.txt` or `publish.reject` alone is merely a payload file and does not establish task completion.
- **Sealed Evidence Required**: An outcome can only be published after raw output files (`raw/stdout`, `raw/stderr`, etc.) are closed, synced, and sealed by `provider.exit` containing a canonical manifest and SHA-256 digest.
- **Payload & Outcome EEXIST**:
  - On payload EEXIST: candidate bytes, length, and digest must match the existing file exactly; mismatch is an invariant fault.
  - On outcome EEXIST: candidate decision must match the existing outcome. A valid existing winner is never overwritten and is preserved.
- **Replay & Idempotence**: Collection is purely observational and recovers existing sealed evidence; it may reconcile the saved Pueue binding and enter lifetime recovery only for an already-requested stop. A queue-row end alone cannot publish terminal facts, and collection never launches, retries, or resumes paid provider work.

## 2. Task Identity and Turns
- **One Task = One Turn**: A task represents one initial user-request turn and
  at most one provider launch. Explicit native clarification replies and
  active-turn steering belong to that same task; they do not create another
  initial user message, reset its budget, or grant another launch. Provider-owned
  background-agent completion remains within the original invocation. A new
  user-request turn requires a separate continuation task.
- **Continuation by Reference**: Resuming or continuing a conversation creates a new task with a new unique task ID bound to an explicit layer-recorded session handle.
- **Session Handoff**: A continuation may transfer a session claim after either a validated terminal outcome or a durable Pueue-ended observation for a budget stop backed by a guardian lifetime-termination record with independently proven empty scope. This evidence proves only that the predecessor turn ended for handoff; it never establishes task completion or replaces `outcome.json`.
- **No Shared Mutability**: Original task records, identities, and results remain immutable and sealed. Launch count and execution metadata are preserved per task.

## 3. Supervision, Deadlines, and Forbidden Signals
- **Explicit Lifetime Owners**: Pueue owns scheduling and worker-row state. The
  application lifetime state machine is the sole owner of native-lifetime
  transition policy; the live guardian and recovery paths enter that same
  policy while holding the task's lifetime-owner lease. Task or inspection
  storage owns its create-once intent, registration, worker, launch, and
  termination records. The execution runner owns pipes, capture, and provider
  process `Start`/`Wait`. The live guardian owns its native scope handle and
  lifetime-owner lease through finalization; the worker never destroys or
  releases the native scope.
- **Lifetime Commit Order**: Persist the deterministic scope intent before
  creating a native scope, registration before starting the managed worker,
  and the exact worker identity before acknowledging its hello. Persist each
  observed provider launch before acknowledging that launch. Persist
  independently proven empty-scope termination and one durable worker
  completion disposition before native-scope disposal or lifetime-owner
  release: the started worker's exact child-`Wait`, or `not-started` when
  `Start` failed and no process identity exists.
  A crash recovery resumes this order; it never infers termination from a
  queue row, a worker exit, or a missing control connection. A native-scope
  collision enters that same recovery transition under the existing owner
  lease. After any ambiguous create-once write, the transition owner applies
  the [canonical create-once classification](#ambiguous-create-once-writes)
  before advancing; unreadable evidence retains the scope and fails closed.
- **Cooperative Owner Loss**: A cooperative scope may prove emptiness only
  while its exact live guardian owns the scope. After that owner is lost,
  recovery may terminate durable launch identities and connected descendants,
  but it reports unverifiable state and never publishes termination. A
  previous-boot record is the exception because the boot identity proves that
  no process from that scope can still exist.

### Forbidden-signal policy

- Direct process signals and process-group operations are prohibited outside
  `internal/lifetime`. Only the verified guardian lifetime implementation may
  use them; adapters, runners, tests, and external tools use the guardian
  boundary.
- The sole exception permits a repository acceptance driver to force-reap only
  a control or fixture subprocess it started directly, after its cooperative
  release and bounded wait have failed. It must report the acceptance failure
  and retain diagnostic state. This narrow process-ownership cleanup never
  applies to a provider, worker, supervisor daemon, or native task scope and is
  never terminal task evidence.
- Acceptance artifact deletion requires an invocation-created directory token
  containing the exact directory and parent identities. Cleanup uses that
  verified parent boundary, rejects replacement, and never treats a
  shared/default parent or an unpinned pathname as disposable authority.
- `exec.CommandContext` is prohibited because its cancellation implementation
  defaults to `Process.Kill`.
- Nonzero `exec.Cmd.WaitDelay` is prohibited because its timeout mechanism
  defaults to process termination.
- **Controlled Subprocess Execution**: Subprocesses are spawned using `exec.Command` with argv slices, explicit working directory, no shell reparsing, and owned standard library pipe handling.
- **Wall-Clock Deadlines and Cancellation**: Tasks enforce a finite positive wall-clock execution deadline. Expiry and a durable user cancellation route through the guardian, which terminates its registered scope and records empty-scope proof before the task may be reported ended. A canceller may mutate the Pueue row directly only while it holds the task's lifetime-owner lease, which proves that no guardian can own or launch the provider concurrently. Pueue stop requests remain scheduling evidence, not native termination authority. Token or dollar ceilings and raw argv are rejected before admission.
- **Bounded Event Loop**: The runner owns a bounded, explicit set of goroutines for stdout/stderr capture, Wait, and supervisor deadline observation. The root runner is decoupled from caller/dispatcher context cancellation.

## 4. Supervisor Admission Ordering
- **Stage Before Launch**: Ordinary tasks and fresh inspections enter Pueue
  as exact `Stashed` rows. The application publishes the numeric supervisor
  receipt before enqueueing that row. Pueue cannot start the worker before this
  commit.
- **Exact Recovery**: A failed receipt commit or uncertain enqueue keeps the
  same row and consumed submission permit. Status or replay may revalidate and
  enqueue only that numeric row; no recovery path issues another add. A row
  already queued or running is accepted as a recovery race only when the
  application already has its matching durable receipt.

## 5. Durability, Staging, and Filesystem Placement
- **Durability Algorithm**:
  1. Acquire maintenance shared lock.
  2. Create unique stage file with `O_CREATE|O_EXCL|O_WRONLY` (mode 0600).
  3. Write data, barrier file content (`f.Sync` on Linux, `F_FULLFSYNC` on Darwin), close writable file descriptor before linking.
  4. Hard-link stage file to final destination name (`Root.Link`).
  5. Barrier destination directory to ensure directory entry persistence.
  6. Unlink stage file and barrier directory for cleanup.

### Ambiguous create-once writes

This rule applies to every create-once durable record, including intent,
registration, worker, launch, termination, completion, supervisor receipt, and
terminal evidence. When a write can have reached storage but returns an error,
the canonical transition owner holds or reacquires that transition's named
owner lease and reads through the same production store before taking another
action. The read has exactly four classifications:

- **Absent**: replay the exact original candidate through the create-once path,
  then read and verify the committed winner before advancing. Another ambiguous
  result re-enters this classification; it never authorizes a new candidate.
- **Matching**: verify that the durable winner exactly matches the original
  candidate under the record's canonical equality contract, including every
  required identity and content field, then continue from the post-commit
  transition.
- **Conflicting**: preserve the existing winner, report an invariant fault, and
  do not overwrite or substitute it.
- **Unreadable**: fail closed, retain owned resources and recovery evidence, and
  report the unresolved record without replaying or advancing.

Only an authoritative read from that production store may establish absence.
A write error, cache miss, missing queue row, process exit, or failed secondary
observation must never be interpreted as absence.

- **Filesystem Capability**: Filesystems are not rejected by type name. Admission probes the required file, directory, hard-link, sync/barrier, and lock operations; a root is refused only when one of those operations cannot be proven.
- **State Root Isolation**: Task state root and workspace must never overlap in either direction. Admission also rejects overlap with declared provider runtime roots. Native provider configuration and extensions may grant wider access; these placement checks do not establish an independent sandbox.
- **Advisory Locks**: Stable advisory locks (`.admission.lock`, `.run.lock`, `.maintenance.lock`) use `flock` on dedicated inodes without unlinking or passing file descriptors to child processes.
- **Git Topology Lease**: Managed worktree add, list, unlock, and remove each
  hold one exclusive lease on the canonical Git common-directory inode. This
  leaf lease is separate from private state locks and uses their shared lock
  acquisition machinery. Its descriptor is transferred to the native Git
  command so helper exit cannot release the lease before Git exits. The
  execution runner remains the sole subprocess owner. Provider turns and
  content capture do not hold this topology lease.
