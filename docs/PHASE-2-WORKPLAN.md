# Phase 2 implementation ownership and acceptance

Accepted base: Phase1 squash `5f9c581f36ffdbed45c0d42bb78f9511afd24324`, source tree `fb4715f6656e841a7e3f5441fa547ca5984a44ee`, private PR2. All exact-head Linux/macOS push/PR checks passed; root audited all49 IDs and live workflow in native CI logs. This phase remains in implementation and is not accepted until every gate below passes.

## Shared design decisions

Keep task values pure and persistence in taskdir. Add a small exact-reference immutable predicate registry with callback-scoped rooted sealed readers and a write-only unpublished candidate sink. Storage retains all hashing, closure, barriers and publication. Return a valid existing winner before registry resolution. Preserve fixture v1 bytes and semantics. Separate collect/status dependencies from submission, launch and stop authority.

Use pinned `go.yaml.in/yaml/v3 v3.0.5` only for strict pueue configuration parsing, alongside the standard library and pinned x/sys. The reviewed `takeover-readiness/yaml-decision/DECISION.md` supplies schema, defaults, resource limits, aliases, presence-sensitive fields, exact byte digest and runtime corpus. Base sections are selected without a profile flag; no ambient endpoint fallback. This is a deliberate dependency amendment, not a homemade YAML implementation.

Persist the absolute supervisor client path in fresh pueue bindings. New typed supervisor/provider/stop records are identity-bound and create-once. A stop permit belongs only to the newly linked, fully durable request creator under maintenance SH. Existing requests cannot issue another attempt; request, reply and scoped termination observation remain separate. Cancellation does not acquire the active runner's exclusive run lock.

The runner receives stdin from a narrow rooted, validated O_RDONLY *os.File so exec passes a finite descriptor directly and the parent can close it after Start. Do not pass an opaque io.Reader wrapper to Cmd.Stdin and inadvertently introduce an unowned copy goroutine. Raw stdout and stderr have separate owned pipe/capture workers; Wait happens exactly once. Normal nonzero exit is observed evidence, whereas infrastructure capture/Wait/flush failures prevent a seal. A started-receipt write error cannot abandon an already running process.

Budget expiry and completion share a preparation barrier. If expiry wins, its
create-once stop intent is durable before completion can cancel observation.
Supervisor reconciliation and the stop reply run outside the barrier; completion
still disarms observation before publication, and a delayed reply does not delay
sealing. If completion wins, no budget intent is prepared. Persistence failure
prevents the supervisor operation and remains a reported stop error.

A completed `exec.ExitError` with an observed process state remains exit evidence,
including Unix signal status `-1`; capture and infrastructure failures still
prevent sealing. A fresh runner requires a known queued/running supervisor state.
An immediate cancel response carries the same proven termination fact as its
saved observation; acknowledgment alone never implies termination.

The fake Phase2 interpreter uses a distinct fixture:test/read-only/version2 contract. Use bounded JSONL metadata/chunk events, allowing arbitrarily long total answers by concatenating exact decoded answer chunks. This avoids prebuilding a general JSON-string streaming parser for an acceptance-only format. Limit each event frame explicitly in the checked-in contract; do not limit total raw or answer size. Require exact task/session identity, full stream completion, a single final success, a non-whitespace answer and clean observed exit. Define each deterministic refusal reason and precedence in the contract before implementing tests. A late session event has a separate runner-owned observational path; it cannot publish or override a terminal winner.

Dedicated acceptance mains call the same application/runtime with a reviewed harness-only constructor. Their independent runner reconstructs the same pinned fake configuration from an explicit acceptance-only path and verifies its digest before Start. Production mains import no acceptance constructor and expose no arbitrary provider executable/argv escape. Unimplemented native launch profiles are refused before admission. Native agy/Codex/Claude remain mandatory subsequent phases before release.

## Execution responsibilities

The lead assigns each worker a bounded, exclusive source scope, then takes it
back after reviewing the handoff. Current assignments and source hashes are
recorded in the execution evidence so completed ownership does not imply
permission to edit a later worker's changes. All delegated agents use
`gpt-5.6-luna` with max effort. Reuse agents for related work and run broader
checks after a stable handoff.

The lead owns integration across storage, predicates, supervisor operations,
process execution and command output; reviews every implementation and test
oracle; and conducts the actual isolated supervisor lifecycle. Workers use
finite compiled helpers within their assigned test scope. All authors share
the worktree and preserve each other's edits. The lead owns the direct Codex
CLI review, private PR, CI verification and merge cycle.

## Required acceptance

Retain all Phase0 and Phase1 gates. Implement every row of the Phase 2 acceptance matrix in the private execution evidence: A01–A06 admission/binding, P01–P07 execution/evidence, B01–B07 budget/watch, C01–C06 stop observations, I01–I05 actual isolated supervisor lifecycle, plus the12 YAML compatibility cases. Each row retains its paired control and U/H/R evidence requirement. Compiled CLI tests must use the real shared application/runtime, not imitation commands.

Root independently checks source, meaningful race/fault/unit tests, clean sequential and parallel makecheck, inherited49 protocol cases, exact-byte live command behavior, and isolated actual installed pueue/pueued4.0.4 on a supported private filesystem. Capture exact A/S/E/K/M events, argv, input/config/output digests, process ownership/completion and source/binary binding. Missing output is not evidence of zero events.

No signals, process groups, CommandContext, WaitDelay or broad cleanup, including tests. All helpers are finite; directly owned processes are naturally awaited. Intentional dispatcher-exit cases retain the exact in-flight client identity and private paths as unknown, with a separate owner-alive Wait control. Real supervisor acceptance has K=M=0; stop routing tests use harmless fake clients. Private real config places every resolved state/runtime/socket/alias/pid/secret/cert/key path under one short unique /tmp/dl-* base outside the worktree. Every client and daemon receives explicit -c. Shutdown follows positively completed jobs/children; failures preserve paths and PIDs.

After stable authorship: root reviews source and all results; freeze exact source; run direct actual `codex review` plus root independent gates. Resolve every actionable finding, including P2/P3, repeat until no issues. Open a private PR, verify all exact-head Linux/macOS CI checks, squash to main and verify resulting tree. Continue Phase3 onward. First release and repository stay private.
