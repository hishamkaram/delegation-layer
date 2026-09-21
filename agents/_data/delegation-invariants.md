# Delegation Layer Invariants

This document establishes the normative operational invariants for the delegation layer. All agents, adapters, and tools must comply with these invariants across all executions.

## 1. Terminal Authority and Immutability
- **Sole Authority**: `outcome.json` is the sole terminal authority. `result.txt` or `publish.reject` alone is merely a payload file and does not establish task completion.
- **Sealed Evidence Required**: An outcome can only be published after raw output files (`raw/stdout`, `raw/stderr`, etc.) are closed, synced, and sealed by `provider.exit` containing a canonical manifest and SHA-256 digest.
- **Payload & Outcome EEXIST**:
  - On payload EEXIST: candidate bytes, length, and digest must match the existing file exactly; mismatch is an invariant fault.
  - On outcome EEXIST: candidate decision must match the existing outcome. A valid existing winner is never overwritten and is preserved.
- **Replay & Idempotence**: Collection is purely observational and recovers existing sealed evidence; it may reconcile the saved supervisor binding to record termination for an already-requested budget stop, but it never launches, retries, or resumes paid provider work.

## 2. Task Identity and Turns
- **One Task = One Turn**: A task represents exactly one provider turn.
- **Continuation by Reference**: Resuming or continuing a conversation creates a new task with a new unique task ID bound to an explicit layer-recorded session handle.
- **Session Handoff**: A continuation may transfer a session claim after either a validated terminal outcome or a durable supervisor-ended observation for a budget stop. The budget-stop observation proves only that the predecessor turn ended for handoff; it never establishes task completion or replaces `outcome.json`.
- **No Shared Mutability**: Original task records, identities, and results remain immutable and sealed. Launch count and execution metadata are preserved per task.

## 3. Supervision, Deadlines, and Forbidden Signals
- **Supervisor-Owned Stopping**: Process lifetime and stopping are strictly owned by the supervisor (`pueue`).
- **Forbidden Primitives**:
  - Direct process signals (`os.Process.Signal`, `os.Process.Kill`, `syscall.Kill`) are prohibited.
  - Process group manipulation (`syscall.Setsid`, `setpgid`, PID/group cleanup, `pkill`, `killall`) is prohibited.
  - `exec.CommandContext` is prohibited because its cancellation implementation defaults to `Process.Kill`.
  - Nonzero `exec.Cmd.WaitDelay` is prohibited because its timeout mechanism defaults to process termination.
- **Controlled Subprocess Execution**: Subprocesses are spawned using `exec.Command` with argv slices, explicit working directory, no shell reparsing, and owned standard library pipe handling.
- **Wall-Clock Deadlines**: Tasks enforce a finite positive wall-clock execution deadline. Expiry routes through supervisor stop requests. Stop requested, stop acknowledged, and termination observed are recorded as distinct status fields. Token or dollar ceilings and raw argv are rejected before admission.
- **Bounded Event Loop**: The runner owns a bounded, explicit set of goroutines for stdout/stderr capture, Wait, and supervisor deadline observation. The root runner is decoupled from caller/dispatcher context cancellation.

## 4. Durability, Staging, and Filesystem Placement
- **Durability Algorithm**:
  1. Acquire maintenance shared lock.
  2. Create unique stage file with `O_CREATE|O_EXCL|O_WRONLY` (mode 0600).
  3. Write data, barrier file content (`f.Sync` on Linux, `F_FULLFSYNC` on Darwin), close writable file descriptor before linking.
  4. Hard-link stage file to final destination name (`Root.Link`).
  5. Barrier destination directory to ensure directory entry persistence.
  6. Unlink stage file and barrier directory for cleanup.
- **Filesystem Capability**: Filesystems are not rejected by type name. Admission probes the required file, directory, hard-link, sync/barrier, and lock operations; a root is refused only when one of those operations cannot be proven.
- **State Root Isolation**: Task state root and workspace must never overlap in either direction, and state root must never lie within any provider-writable tree.
- **Advisory Locks**: Stable advisory locks (`.admission.lock`, `.run.lock`, `.maintenance.lock`) use `flock` on dedicated inodes without unlinking or passing file descriptors to child processes.
