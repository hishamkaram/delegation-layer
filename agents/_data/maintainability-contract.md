# Provider maintainability contract

This is the shared engineering guidance for provider redesign work. The
[delegation invariants](delegation-invariants.md), [adapter contract](adapter-contract.md),
and [code-quality floor](code-quality-floor.md) remain authoritative for their
respective requirements; this document adds the maintainability decisions that
apply across the implementation, review, test, acceptance, and documentation
skills.

## Boundary and ownership

- The core owns task admission and claims. The application lifetime state
  machine owns all native-lifetime transition policy; the live guardian and
  recovery paths enter that same state machine under one lifetime-owner lease.
  Pueue owns scheduling and worker-row state. The execution runner owns
  provider `Start`, pipes, capture, and provider `Wait`. The live guardian owns
  its native scope handle and lifetime-owner lease until finalization completes.
  Task or inspection storage owns the durable intent, registration, worker,
  launch, termination, and managed-worker completion records. Only the state
  machine may authorize native scope disposal.
- An adapter owns its immutable identity, supported request options, runtime
  capability profile, native argument/configuration preparation, identity semantics, and
  versioned interpretation of sealed provider evidence. It never owns pipes,
  capture, sealing, process lifetime, stopping, or publication.
- Interfaces belong to the consumer that needs them. Keep each interface
  narrow enough to express the real boundary, and use one explicit catalog at
  startup rather than init registration, mutable global registries, dynamic
  plugins, or a universal provider DSL.
- Reuse existing task, execution, predicate, usage, and lifecycle value types.
  A new abstraction needs a concrete consumer and a lower total maintenance
  cost than the code it replaces. Provider-specific behavior must not leak into
  task, execution, or publication code.
- Give each lifecycle decision one canonical owner. A lower layer may expose a
  narrow primitive or callback, but it must not duplicate application policy
  for another record family or command path.
- Before adding a lifecycle branch, map all callers and both dependency
  directions with the codebase graph, write the affected rows in the canonical
  durable transition matrix below, and compare the proposed owner with existing
  helpers. A new path must call the canonical resolver or policy gate rather
  than copy its predicates.

## Review dimensions

For every substantial change or review, record evidence about:

1. ownership and resource lifetimes, including cleanup and failure paths;
2. dependency direction and whether a new dependency is necessary;
3. the interface owner and whether the boundary is real and narrow;
4. duplicated lifecycle, capability-check, parser, or acceptance-harness logic;
5. error context, compatibility behavior, and accidental secret exposure; and
6. contributor effort, including whether the documented path is discoverable
   and repeatable.

Do not add arbitrary file-size or coverage thresholds. Existing formatting,
lint, race, build, and vulnerability thresholds remain the project floor.

## Lifecycle duplication guard

Supervisor binding, migration, cleanup authorization, and evidence ownership
are lifecycle policy. They must each have one application-level decision point.
Infrastructure packages may report observations and execute an already
authorized primitive; they must not decide which Delegate records are safe to
reap or which successor a continuation should use. Reviewers must reject a
second predicate that answers the same lifecycle question, even when it is
located in a command-specific helper or test-only path.
Acceptance harnesses use `acceptance_supervisor_common.Process` and `Processes`
for child ownership, waiting, and process receipts. Provider-specific stdin or
bounded stdout behavior is supplied through their narrow options; a provider
harness must not define another process class or registry. The ownership
verifier enforces this boundary.

## Durable transition matrix and composed tests

Every lifecycle design, implementation, review, and handoff uses one canonical
matrix. The required column order is: durable facts → owner/lease → permitted
action → commit point → failure/recovery → consumer verdict → named test.

| Durable facts | Owner / lease | Permitted action | Commit point | Failure / recovery | Consumer verdict | Named test |
|---|---|---|---|---|---|---|
| Stable row ID plus the exact records and validated observations that exist before the transition | One policy owner and the named lease or lock it must hold | The single side effect authorized by those facts | The durable write or independently verified native fact after which consumers may advance | The retry entry point and classification for every partial or ambiguous result | The exact verdict each real consumer returns from the committed facts | The test that proves normal execution and recovery for this row |

Rows follow durable state rather than call-stack phases. They name every writer,
validator, and consumer of each record, and normal execution and recovery enter
the same row. An in-memory flag, queue observation, process disappearance, or
successful return is not a durable fact unless an authoritative contract makes
it one. A commit point is the first fact from which all named consumers can
reach the row's verdict after restart.

Every affected row requires a composed lifecycle test through each affected
production record store and each real consumer that turns the records into an
action or verdict. When the same record family has task and inspection stores,
or another production variant, exercise every affected implementation. Fakes
and in-memory stores may test isolated protocol ordering only; they cannot prove
durable commit, restart recovery, create-once classification, or a consumer
verdict. Fault injection may wrap a production store, but the test must retain
its real serialization, create-once behavior, reader, and consumer.

## Recovery and verification

Apply the canonical matrix to normal execution and journal resumption together.
Every durable row must have a retry path that preserves recorded progress;
create-once publication must not reconstruct an earlier phase. Once a
predecessor is durably stopped, finish its recorded successor before choosing
a later installation. Publication and cleanup use the same implementation in
normal execution and recovery.

For provider lifetime, the required order is: acquire the owner lease; persist
the deterministic scope intent; create that exact native scope; persist its
registration; start the managed worker; if it starts, persist its exact identity
before acknowledging its hello; authorize and persist each observed provider
launch; fence the worker and prove the scope empty; persist termination; persist
one completion disposition (`not-started` or the started child's exact `Wait`);
dispose the native scope; then release the guardian's scope handle and owner
lease.
Recovery resumes from the last durable record and uses the same finalization
policy. An intent-only scope without registration may be disposed because
commit order proves that worker start was not authorized. A registered
cooperative scope without its exact live owner may clean durable process
identities but cannot publish empty-scope termination. A registered scope is
never disposed before termination and the worker completion disposition is
durable,
and a worker never disposes a scope merely because its own process is ending. A
deterministic scope collision enters this same recovery policy while the owner
lease remains held. Apply the invariants' [ambiguous create-once
rule](delegation-invariants.md#ambiguous-create-once-writes) to every durable
record family. Registration is not a special case: each affected transition row
must cover absent, matching, conflicting, and unreadable results under its
canonical owner and lease.

Before removing operational state, identify every consumer that still needs
it. Persist independently observed terminal evidence first, then make those
consumers able to finish from that evidence without the queue row. A worker's
own completion is not proof of its process exit. For every fresh inspection, the
guardian may publish that evidence only after durably proving the native scope
empty and recording either `not-started` or the exact child `Wait` through the
shared finalization policy. Pueue terminal events and exact status remain
fallback observations. Both
outcomes validate the immutable submission receipt. Success also requires its
bound completion; exact failure may be terminal without a completion because
the worker can exit before publishing one. Validate uniqueness over the whole
observed queue before publishing per-task authority;
rejecting a later
duplicate must not leave an earlier ambiguous row promoted to trusted proof.
Test cleanup racing an active observer, including failed worker exits.
Treat supervisor numeric IDs as reusable operational handles. Exact identity
observation and numeric mutation (removal, stopping, or runner replacement)
must share an exclusive supervisor lock that excludes submissions and other
mutations; a stale snapshot must never authorize
an action on a reused ID. Retain that lock until any in-flight mutation settles.

Supervisor observation must classify pending transitions and apply the
application's final gate within the same generation lease. Shared leases may
observe published state; directory repair, identity publication, inspection
group creation, and recovery-alias retirement require exclusive ownership.
Test concurrent callers and unfinished commands as well as stored queue size;
a queue containing 200 rows does not prove 200 concurrent admissions work.
Measure running-task capacity and dispatch-burst throughput separately. A
capacity test must hold the requested number of tasks in Running together;
record the number of concurrent dispatch callers without treating it as a
product concurrency limit. Give gated fixture tasks enough budget for the
whole scenario, and reject timed-out tasks even if a stale queue row still
says Running.
Healthy no-change paths must not acquire exclusive transition ownership merely
to discover there is nothing to change. Reuse the same validation and planning
functions for a shared probe and the fresh exclusive mutation check. Optional
maintenance skips a busy lock; required transitions still wait within their
control deadline. Preserve failed load receipts and distinguish host contention
from demonstrated application defects rather than assuming either cause.
For a published private generation, warm admission validates the binding,
executable bytes, resolved config, migration completion, and exact-generation
scheduling receipt under the shared generation lease, then probes the Unix
endpoint directly. It must not launch a supervisor control process. Missing,
refused, changed, or stale state falls through to the elected exclusive path,
which performs the protocol-level group check before repair or startup. Treat
the scheduling receipt as Delegate's authority for its own default-group policy;
manual supervisor mutation is outside the public lifecycle boundary.

Keep supervisor lifecycle work out of an already-running task's data path.
Dispatch and explicit observation own bootstrap, migration, repair, and queue
reconciliation. Add ordinary tasks and managed inspections with
`add --stashed --print-task-id`, persist that exact numeric receipt against the
immutable task, then enqueue only that ID. A managed runner can start only
after this commit; it validates the durable receipt and starts from its saved binding
without bootstrapping the supervisor or scanning the queue. Keep a conservative
reconciliation path only for legacy, custom, or direct runner invocations that
lack the internal supervised launch marker. Test that concurrent supervised
runners and inspection workers issue zero startup control commands, including
when receipt publication fails or enqueue becomes uncertain. Those failures
leave the exact row stashed for status recovery and must never submit again.

The process that calls `exec.Cmd.Start` owns exactly one `Wait`. Detachment
from the command caller does not remove the obligation to reap a child while
its parent remains alive. Review the waiter, pipes, parent exit, and failure
paths together; do not infer process exit solely from socket disappearance.
Acceptance cleanup owns only directories created or immediately adopted by the
current invocation. That owner records the directory and parent identities at
creation, removes through the verified parent descriptor, and rejects a
replaced path. Shared/default parents such as `/tmp`, `$HOME`, and
`/Users/Shared` are never disposable merely because a child was created there.
Raw process captures require the same identity token; a boolean or pathname is
not cleanup authority.
Deferred cleanup must capture the acquired resource independently of named
return values, which an error return can overwrite. Preserve pending-command
handles until ownership cleanup is arranged; redact diagnostics at the outer
boundary afterward.
Persist one managed-worker completion disposition for every registered scope.
If `Start` succeeded, bind the exact child-`Wait` result to the durable worker
and registration identities. If `Start` failed, record `not-started` with no
process identity. Scope disposal and lifetime-owner release require both
empty-scope termination and that durable disposition. Queue terminal state
remains operational evidence and cannot replace this proof.
Distinguish watcher timeouts from the deadline of the worker that owns startup.
An admitted runner must not abandon startup merely because a shorter watcher
window elapsed. Scope any longer observation window to that lifecycle phase;
retain the original client for stop control and cleanup. Prove delayed success
and deadline expiry with the same owned command, without redispatching.
Task-directory persistence owns the local fallback for control and journal I/O.
A finite caller deadline remains authoritative through every nested lock and
record write; an inner layer must not replace it with a shorter convenience
timeout. A caller without a deadline receives the bounded local fallback.
Cancellation may stop lock acquisition or work before staging starts, while a
durable write that has started retains ownership through its barriers and stage
cleanup. Tests must cover caller deadlines longer and shorter than the fallback,
direct background calls, and replay after a result commits without its bound
completion.
An unfinished control probe is uncertainty, not proof of binding drift or
incompatibility. Preserve its pending-command owner through error classification,
without discarding independently established binding or storage failures. Test
pending and completed invalid probes separately at the shared policy boundary.
An inspection worker's failure is not evidence that a provider is unavailable
or misconfigured: it may precede native inspection. Preserve that distinction
in public failure classification, including replay of recorded worker failures.
A supervisor shutdown acknowledgement proves receipt of the request, not process
exit. Restart acceptance must establish that the owned predecessor and endpoint
have stopped before claiming to test recovery of a stopped supervisor. Keep
shutdown-in-progress races as a separate scenario rather than conflating them
with completed shutdown.

Before selecting a new lifecycle design, consult current primary documentation
and Context7 where available, then reproduce the relevant behavior with the
pinned toolchain. Record what the sources establish and what local experiments
prove separately. An unavailable source is a disclosed limitation.

### Native portability proof

Cross-platform compilation and a dependency's platform support prove only that
the binaries can run on those platforms. They do not prove Delegate's process
identity, descendant discovery, signaling, waiting, endpoint, descriptor, or
crash-recovery behavior. Pueue owns queueing and worker-row state; its
cross-platform process handling does not replace Delegate's lifetime owner,
durable evidence, or empty-scope proof.

Before changing any native lifecycle boundary, name the owner and invariant for
each supported OS and verify the exact primitive against current primary
documentation. Keep the state machine and policy shared. Put only the primitive
behind an OS-specific implementation when semantics differ. Never infer Darwin
behavior from Linux, infer Linux behavior from Darwin, or use a cross-compiled
binary, container, fixture on another kernel, or dependency claim as native
runtime evidence.

Private supervisor launch is a native boundary owned by the Pueue client. The
Pueue client owns the launch decision and waiter; `internal/lifetime` owns the
shared Unix-session primitive. A new daemon inherits the descriptor limit
already available to Delegate and starts in an independent Unix session. Native
Linux and macOS tests must prove descriptor inheritance and caller-process-group
interruption. Cross-compilation proves only that the platform implementation
builds.

#### Native backend and assurance matrix

| Native host | Backend | Assurance | Claim boundary |
|---|---|---|---|
| Linux | cgroup v2 | exact | Kernel cgroup identity and emptiness may establish exact scope containment when the native receipt proves the required controls and operations. |
| Linux | process group | cooperative | Emptiness is publishable only while the exact live guardian owns and observes the scope; ownerless recovery remains unverifiable except for previous-boot proof. |
| macOS | process group | cooperative | Emptiness is publishable only while the identity-pinned live guardian owns and observes the scope; ownerless recovery remains unverifiable except for previous-boot proof. |

No backend may inherit another row's assurance. Every production scope creation
uses `lifetime.Prepare` to select and validate one row, persists the exact
returned plan as durable scope intent, and only then calls
`lifetime.CreatePrepared` with that persisted plan. This
`Prepare` → persisted intent → `CreatePrepared` path is the sole production
creation path. Creation must not select a new backend, recompute an identity, or
fall back after the intent commit.

Native evidence contains two bound receipts. The primitive receipt records the
native host kernel and architecture, selected backend, persisted-plan digest,
scope identity, invoked primitive and observed result, and executable
fingerprint. The assurance receipt references the primitive-receipt digest and
records `exact` or `cooperative`, the lifetime owner and lease, the proof used,
and every unresolved limit. A generic success flag, backend name, or queue row
cannot substitute for either receipt.

Required evidence for an affected supported OS is a native test or acceptance
run covering the changed success and failure paths. Process-lifetime changes
must exercise start, exact child `Wait`, deadline termination, descendants that
create another group or session, identity reuse protection, interruption and
recovery, and cleanup. Supervisor changes must also exercise an existing state
root across an executable-path or byte-identity change. Capacity claims require
the requested tasks to be concurrently running on that OS while every counted
task holds its live lifetime-owner lease and native scope. Cross-compilation,
race tests, and hermetic fixtures remain additional gates, not substitutes.

The research basis is the official [Go `os/exec` contract](https://pkg.go.dev/os/exec),
which assigns one `Wait` and documents process-only default cancellation;
[Pueue's platform statement](https://github.com/Nukesor/pueue#features), which
describes its queue and process-group support; and Apple's
[XNU process identity implementation](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/kern_proc.c),
which validates audit-token signals with Darwin-specific process identity.

Distinguish seeded journal-state tests, actual interrupted transitions, fixture
processes, and real supervisor/provider acceptance. A native fixture test proves
that fixture on that OS, not live provider acceptance. Give fixture setup its
own bounded deadline; preserve the short fault deadline and all assertions for
the operation under test. A focused retry passing does not erase a failed full
gate. Exercise large retained history and the required concurrency explicitly.
Acceptance clients must use each command's actual JSON and exit-code contract;
dispatch failure envelopes are not observation schemas. After an uncertain
submission creates a task, observe that same task without resubmitting it.
Retain each response and exit code, and never treat an operational observation
error as success merely because its JSON also contains an admitted task.

### Live acceptance proof

Live acceptance must satisfy all applicable requirements below in addition to
the transition-row tests:

- **Lifetime closure before cleanup**: commit evidence that every admitted or
  started task reached the shared lifetime finalizer, the native scope is
  independently proven empty at its declared assurance, and the managed worker
  has an exact `Wait` or `not-started` disposition before deleting an owned
  state root or raw process capture. Supervisor shutdown acknowledgement alone
  is not closure.
- **Exact packaged/default-path proof**: a claim about packaged or default use
  invokes the packaged executable through the documented default path
  resolution. Its receipt records the resolved absolute path, SHA-256 binary
  fingerprint, package/build identity, and default-path observation. An
  explicitly supplied development binary proves only that binary's scenario.
- **Live lease capacity proof**: a capacity claim holds the requested number of
  real tasks concurrently after each has a durable owner-lease record, live
  lease, and created native scope. The receipt identifies every counted task
  and the overlap interval; queue depth, `Running` labels, dispatch callers, or
  fixture gates alone are insufficient.
- **Always-finalized bounded failure receipts**: the harness installs its receipt
  finalizer before risky setup and gives finalization its own finite deadline.
  Every success, failure, timeout, blocked prerequisite, and cleanup error
  commits a sanitized terminal receipt that names the last completed stage,
  lifetime-closure result, cleanup result, retained evidence, and unresolved
  facts. A missing or incomplete receipt is failed evidence, never a passing or
  silently skipped run.

Fixture protocol, live provider, and native lifecycle/supervisor results are
separate claims with separate verdicts and bound receipts. A pass in one class
does not satisfy or upgrade another class.

## Compatibility and evidence

Preserve existing provider IDs, persisted request normalization, native login
flows, historical predicate revisions, and nullable usage scope. Explicitly
unsupported options fail before admission; they are never silently discarded.
Collection is observational and must not launch, retry, or resume paid work.
Missing prerequisites or inconclusive native evidence are `BLOCKED`, never a
passing skip. A rejected task may have a rejection payload; rejection is
established by the authoritative outcome record and its evidence, not by an
assumption that the payload is empty.

Use meaningful tests for observable behavior, error branches, writer failures,
boundaries, and compatibility. Do not copy lifecycle or capability-check tests
into every provider. Keep provider interpretation pure over sealed evidence and
keep acceptance orchestration in the shipped dispatcher/runner and shared
harness.

### Handoff evidence

Every design, implementation, test, and review handoff identifies the affected
canonical transition row IDs and, for each row, its policy owner, lease, record
writer, validator, and every action- or verdict-producing consumer. It includes:

- an exact source snapshot: repository baseline and reviewed revision when
  available, plus a path and SHA-256 manifest for every changed or untracked
  file in the build, test, or review input; untracked inputs must never be
  hidden behind a generic working-tree description;
- every executed binary's resolved path, SHA-256 fingerprint, version/build
  identity, and whether it came from the package/default path or an explicit
  development path;
- commands, exit codes, named tests, and immutable receipt paths/digests,
  separated into fixture, provider, and native claim classes; and
- unresolved findings, missing or inconclusive evidence, retained resources,
  and the exact claim each limitation prevents.

The handoff is incomplete when its source or binary snapshot cannot reproduce
the evidence, a named consumer lacks a verdict, or an unresolved item is
presented as passed.

Root owns integration, independent review, commits, pull requests, CI, and
merges. Contributors may prepare bounded changes and
receipts, but must not transfer those responsibilities or install global
skills/configuration. A review record is incomplete when its output is
inconclusive or when actionable findings remain unresolved.

## Repository skill format

The repository skills under `skills/` intentionally use a small, documented
metadata format so the verifier is deterministic without becoming a YAML
parser. Each `skills/<name>/SKILL.md` starts with exactly this frontmatter:

```text
---
name: lower-case-hyphenated-name
description: one non-empty single-line description
---
```

Only `name` and `description` are allowed, each exactly once. The name must
match its directory and use lowercase letters, digits, and single hyphens
between words and be at most 64 characters. Unquoted metadata values are plain strings that begin with an
ASCII letter and do not use YAML-special scalar forms; quote values that begin
with punctuation or digits or contain `: `. Each skill body should make its trigger, inputs, workflow,
deliverables, and exit criteria easy to find; the verifier deliberately does
not require particular Markdown headings.

Repository links in a skill use relative Markdown paths in an inline
parenthesized destination. Parentheses in a destination are balanced, and an
angle-bracket destination may contain spaces; a fragment after `#` is allowed
and ignored for file existence checks. Link titles are unsupported; put explanatory text in the label or surrounding prose.
Reference-style links/definitions and
other unsupported Markdown shapes are rejected explicitly. External URLs may
be used when genuinely useful, but they are not a substitute for links to the
applicable repository contracts.

The verifier recognizes a one-target inline `make <target>` command when it is
written as inline code or as a command-shaped line. The target must exist in
the Makefile. Options, variable assignments, extra targets, shell operators,
and other trailing command text are unsupported and fail explicitly. A command
that is intentionally future work must carry the explicit marker `[planned]`
or `(planned)` on the same line; that marker is accepted only when the line has
one unambiguous command. Unfinished scaffolding uses explicit uppercase tokens
such as `TODO`, `FIXME`, `TBD`, `PLACEHOLDER`, `<YOUR_NAME>`, or
`{{REPLACE_ME}}`; ordinary prose such as “todo list” is not a marker.
