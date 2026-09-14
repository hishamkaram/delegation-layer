# Delegation layer

A durable single-machine agent delegation layer: a frontier model leads, cheaper agents from provider CLIs do the donkey work, and **a worker's turn survives its launcher.**

**Status: Phases 0 and 1 accepted (PRs #1 and #2 merged); Phase 2 supervisor integration undergoing acceptance.**
The durable protocol and its compiled `protocolfixture` harness passed the required 49-case matrix, independent review, and Linux/macOS CI. Phase 2 adds the shared `delegate` commands and `delegate-run` process owner. Its compiled acceptance programs supply a finite test provider; production dispatch refuses native profiles until their adapter phases are implemented and verified. agy, Codex, and Claude are all required before the private first release.

- [`docs/EXECUTION-PLAN.md`](docs/EXECUTION-PLAN.md) — Normative, authoritative execution plan for all phases.
- [`docs/IMPLEMENTATION-PLAN.md`](docs/IMPLEMENTATION-PLAN.md) — Phase-by-phase implementation tracking and progress.
- [`docs/PROTOCOL-FIXTURE.md`](docs/PROTOCOL-FIXTURE.md) — Finite protocol test commands, ownership and fault observations.
- [`docs/CLI.md`](docs/CLI.md) — Task commands, result codes, and supervisor configuration.
- [`DESIGN.md`](DESIGN.md) — Architectural design, verified provider facts, decisions taken, and historical measurements.
- `diagrams/components.html` — System components architecture.
- `diagrams/lead-interface.html` — Interaction model between the calling lead and the delegation layer.

## Core Invariants and Decisions

- **Resumption-by-Reference & At-Most-Once Launch**: An agent turn is non-deterministic and costly to repeat. A task is strictly **one provider turn**. Resuming a conversation allocates a **new task** bound to the recorded session handle, preserving original identity, result, and launch count.
- **Sole Terminal Authority (`outcome.json`)**: A worker's finished answer is a payload file (`result.txt`), but `outcome.json` is the sole terminal authority. The existence of a payload alone is pending; only an `outcome.json` referencing the payload digest seals completion. Terminal payloads and outcomes cannot be overwritten, and collection never launches, retries, or resumes paid work.
- **Durable Filesystem Commit**: Staging uses cryptographically unique temporary files in the destination directory, closed and synced before hard-linking into place, followed by a directory sync barrier. Remote, tmpfs, or unsupported filesystems lacking reliable barrier primitives are refused.
- **Supervisor-Owned Stopping**: `pueue` owns process supervision. Direct process signals (`os.Process.Signal`, `os.Process.Kill`, `syscall.Kill`), process group manipulation (`setsid`, `setpgid`, `pkill`, `killall`), `exec.CommandContext`, and nonzero `WaitDelay` are strictly prohibited. Budget supervision uses a bounded runner event loop with explicit ownership.
- **Execution Budgets**: A monotonic runtime timer observes the execution budget and requests a validated supervisor stop on expiry. Stop-request, stop-acknowledgment, and termination-observed are separate facts; expiry does not prove termination. Token/dollar ceilings and raw argv are rejected before admission.
- **Input Delivery**: The prompt brief is delivered via finite regular file passed to child stdin, followed by immediate EOF. Brief text is never passed in argv, and provider commands are never shell-reparsed.
- **Provider Scope (v1 Private Release)**:
  - `antigravity:print`: Required candidate profile for `workspace-write` within validated roots, pending adapter-phase live acceptance receipts. Read-only is unsupported; `--sandbox` does not imply read-only.
  - `codex:exec`: Required candidate profile for `read-only`, pending adapter-phase live acceptance receipts.
  - `claude:print`: Required candidate profile for `read-only` via restricted built-in tools, pending adapter-phase live acceptance receipts.
  All three provider profiles are required before the v1 release.
- **Private Delivery**: The repository and release assets remain private. Authenticated GitHub tooling is required for installation; public repository distribution, public Homebrew taps, and unauthenticated `go install` are out of scope.
- **Declared Workflow**: Multi-task coordination consumes an explicit declared plan DAG; it never autonomously infers or invents new tasks.

## Historical Measurements Preserved

Marked as such throughout documentation and tests:
- **Supervisor comparison**: `pueue 4.0.4` vs `task-spooler 1.0.4` — kill reach, JSON status, output retrieval.
- **macOS process-tree ceiling**: A `setsid`-escaped descendant survives every supervisor tested; this is an OS limit, not a tool defect.
- **`agy` 1.2.1**: The `--print-timeout` failure shape (exit 0 + `SUCCESS` + empty response), result JSON, transcript storage.
- **`agy` 1.2.2 `--sandbox`**: Filesystem sandbox behavior, `workspace-write` rather than read-only.
- **Go toolchain & cross-compilation**: Go 1.27.1 cross-compiles all 4 target platforms (`darwin/amd64`, `darwin/arm64`, `linux/amd64`, `linux/arm64`) with CGO_ENABLED=0 in seconds without external linkers.
