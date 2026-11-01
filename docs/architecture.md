# Architecture

Delegate manages the local lifecycle of a bounded task run by an installed
agent CLI. Request storage, scheduling, native execution and result publication
have separate owners. Adapters describe and interpret provider turns; the shared
runtime owns launch, supervision and publication.

| View | Interactive | SVG | PNG |
| --- | --- | --- | --- |
| Components and owners | [Explore](../diagrams/components.html) | [Light](../diagrams/components.light.svg) · [Dark](../diagrams/components.dark.svg) | [Light](../diagrams/components.light.png) · [Dark](../diagrams/components.dark.png) |
| Ordinary task lifecycle | [Explore](../diagrams/lead-interface.html) | [Light](../diagrams/lead-interface.light.svg) · [Dark](../diagrams/lead-interface.dark.svg) | [Light](../diagrams/lead-interface.light.png) · [Dark](../diagrams/lead-interface.dark.png) |

Editable sources: [components](../diagrams/components.architecture.json) and
[lifecycle](../diagrams/lead-interface.sequence.json). This page owns the
implementation overview. The packaged [command reference](../skills/agent-integration/references/command-reference.md)
and [result handling](../skills/agent-integration/references/result-handling.md)
own shared public procedures and response decisions.

## Package boundaries

| Owner | Responsibility |
| --- | --- |
| `internal/app` | Compose commands, capture caller context and project CLI responses. |
| `internal/teams/contract` / `internal/teams` | Pure Team definitions/default merging / the authoritative SQLite catalog, project identity and optional project-default references. |
| `internal/task` / `internal/taskdir` | Task values and validity rules / durable records and terminal publication. |
| `internal/provider` | Prepare provider launches and interpret native evidence through pure contracts. |
| `internal/admission` | Required inspections and their admission evidence. |
| `internal/tasklifecycle` | Create, submit, observe, continue and control ordinary turns. |
| `internal/supervisor` | Select bindings, recover generations and authorize private-queue maintenance. |
| `internal/pueue` | Execute supervisor control operations through its protocol boundary. |
| `internal/guardian` / `internal/lifetime` | Shared lifetime transition policy / native process-scope primitives. |
| `internal/execution` | Provider start, input transport, capture, wait and sealing. |
| `internal/workspaces` | Optional workspace provisioning, reservations, capture and release. |
| `internal/resourceadmission` | Invocation-local host observations and the new-task pressure decision. |
| `internal/scheduler` | Schedule storage and recurrence policy; the application runtime composes due claims with existing task execution. |

Consumer-owned interfaces keep decisions independent of storage and OS details.
The [maintainability contract](../agents/_data/maintainability-contract.md#subsystem-boundaries-and-maintainability-gates)
and [module registry](../agents/_data/subsystems.json) own enforced boundaries.

## Ownership boundaries

A normal task passes through these boundaries:

1. Validate the request and canonical workspace; check new-task resource pressure.
2. Resolve the provider profile and prepare the private or explicitly selected
   external supervisor. Run admission inspection where the profile requires it.
3. Persist immutable request, brief and metadata. Stage one supervisor row,
   persist its exact receipt, then enqueue that row.
4. The guardian registers the task's native scope and starts its managed worker.
5. The worker reconstructs the recorded contract and performs fresh runtime
   checks before starting the provider once.
6. Observe provider completion, finish captured streams and declared artifacts,
   seal evidence and evaluate the recorded result predicate.
7. Publish the terminal outcome. Later observation reads or recovers that
   publication; another conversational turn is a separate linked task.

The plugin's SessionStart bootstrap precedes this lifecycle. It makes the
control CLI available; it does not own task admission or execution.

### Optional Team selection

The [Teams guide](teams.md) owns public configuration and selection rules.
`internal/teams` owns mutable definitions, per-Team revisions/tombstones,
immutable catalog identity and project-default references. Only explicit CLI
mutations write them. Passive reads do not create, migrate or repair the catalog.
Its user-configuration location is independent of execution `--root` and provider
launch environments.

`internal/task` owns selection intent and admitted context as pure task evidence.
`internal/tasklifecycle` consumes a narrow Member resolver supplied by app
composition. For a definitely new task ID, it reads a committed Member once
before workspace provisioning and merges caller choices through the pure
contract. Project identity comes from the canonical source workspace through
the existing Git owner; a prepared linked worktree is revalidated before launch.
Existing-ID recovery adopts the saved request, including a concurrent creation
winner. No-Team requests and exact-session continuation do not read the catalog.

Passive project Git probes use `execution.RunIsolatedCommand` in a dedicated
one-shot helper, so the CLI/scheduler process neither calls `BindOwnerProcess`
nor becomes a subreaper. The helper owns native process-group identity and
cleanup on deadline or caller disconnect; the parent joins stream draining and
`Wait`, without creating a task, supervisor, execution root or journal. This
read-only ephemeral probe explicitly selects the cooperative backend to avoid
unrecoverable cgroup allocation; task/guardian backend selection is unchanged.

The existing task create-once boundary remains execution authority. Admitted
context records the catalog ID, scoped target, revision and explanatory Role
information; it does not become a second settings store. Dispatch/status/collect
project that context from saved task evidence, without refreshing the catalog.
Schedule definitions pin a catalog instance and preserve raw caller choices;
each new occurrence uses this same resolver. Existing task IDs keep immutable
settings. See [scheduler upgrades](cli-reference.md#scheduler-upgrades) before
introducing new targeted definitions to a resident scheduler.

### Caller context and native settings

Composition captures the caller's environment without changing process-global
settings. Preparation resolves the provider through that selected context and
filters its launch environment according to the adapter. Native settings,
extensions, authentication and session storage remain provider/user-owned.

An admitted task reconstructs from its own recorded context. A historical record
that never saved context retains its documented fallback; an explicitly saved
empty context stays empty. A new continuation instead uses its current caller's
context while retaining the predecessor's permission, workspace and exact native
session. It does not copy the predecessor's settings or credentials.

The saved supervisor binding remains authoritative. Caller environment cannot
silently retarget a private predecessor; an explicit supervisor option retains
its documented precedence. Supervisor control has its own bounded environment,
separate from provider launch variables.

### Runtime-only transition ownership

Managed runtime-only admission records the required capability contract, not a
claim that a provider has already passed a live probe. The supervised worker
checks required native behavior inside the task's budget. Other profiles,
custom runners and historical records retain their required admission inspection.

The fresh launch must preserve the admitted permission, workspace, policy,
writable roots, predicate and artifact declarations. A runtime observation from
another task never grants permission to launch this one. Metadata stays immutable.

### Managed Git workspace lifecycle

Git isolation is optional. Ordinary tasks, including tasks in non-Git directories,
use the normal path without creating a repository. The workspaces coordinator
owns provisioning, use reservations, capture, release and recovery; adapters do
not manage Git subprocesses. The Lead owns verification and landing changes.

The shared Git common directory identifies the repository, including when the
source is already a linked worktree. Delegate serializes its worktree-topology
commands under that common-directory lease; provider turns and content capture
can proceed concurrently. External Git commands do not participate in this lease.

Each managed turn captures a baseline immediately before provider launch. After
canonical provider/worker closure, a separate finite operation captures the final
tree. Evidence distinguishes the cumulative starting-base-to-final change from
the current turn's baseline-to-final change, and separately observes HEAD and
the real index. Intermediate commits do not replace the original cumulative base.

Private-index capture preserves the real index. Flag-only and intent-to-add edits
are observable; a stat-cache refresh or HEAD-only movement is not a content edit.
Task status describes its own use reservation; workspace status describes the
latest use. Capture receipts and retained recovery state do not rewrite the
provider outcome or extend its expired deadline.

Checkpoints reuse the same supervised helper runtime. Their owner publishes a
full-tree candidate and owned ref without moving HEAD, a branch or the real
index. Managed checkpoints are workspace uses; ordinary checkpoints own no
workspace reservation. Current availability is derived from recorded removal
attempts, and forgetting a workspace does not remove checkpoint refs.

See the packaged [workspace and checkpoint guidance](../skills/agent-integration/references/workspaces.md)
for commands, capture limits, continuation and cleanup decisions.

## Private supervisor

Normal installation includes a private supervisor; callers do not configure
Pueue for ordinary tasks. Delegate creates state-rooted configuration and recovers
its saved binding as needed. A separately configured external supervisor remains
an explicit operator choice.

One completed-generation record owns the active private supervisor. Startup and
migration take its exclusive lock; ordinary controls hold a shared generation
lease until their in-flight commands settle. A healthy endpoint can be reused
only after the owning binding and protocol checks pass. Installation changes
alone do not authorize terminating a live generation or replacing its records.

Migration is journaled and preserves task data. Existing-task observation can
remain permitted while new mutations are refused. Queued managed-runner repair
requires its recorded authorization; custom or ambiguous workers are not silently
rewritten. Public recovery instructions live in
[result handling](../skills/agent-integration/references/result-handling.md).

The ordinary private group has no fixed task-count cap: the caller chooses
fan-out, subject to new-task pressure admission. Required provider inspections
and model discovery have their own single-slot inspection group and finite
recorded deadlines.

The queue is operational state, not task history. Maintenance removes only exact
terminal rows with validated Delegate evidence. Active, incomplete, malformed
and unrelated rows remain. External queues are never automatically cleaned.
Queue observation is streamed by record; individual control records stay bounded.

Private-task observation only requests elected maintenance when necessary;
maintenance warnings do not change the task's outcome, payload or exit code.
Recovery and cleanup share their existing owners rather than a second policy.

## Execution and native lifetime

The execution runner owns one provider `Start` and its corresponding `Wait`.
The guardian owns stop authority and the native scope through finalization.
It records intent before scope creation, records the worker before acknowledging
startup, and retains its owner lease while closure is unresolved.

Cancellation first records a durable request. The live guardian observes it and
uses the same finalizer as budget expiry and recovery. A direct supervisor stop
requires fencing provider startup under the lifetime-owner lease. A queue-row
end or shutdown acknowledgement alone does not prove native closure.

Linux uses a delegated cgroup v2 when available. Otherwise Linux and macOS use
cooperative private sessions with platform-specific process identities. The live
owner tracks descendants; ownerless cooperative recovery cannot simply assume
the scope is empty. Native portability claims require native-host checks.

Provider work retains its immutable deadline. Already-started stop/finalization
uses a separate bounded cleanup phase; it cannot renew the provider budget.
Each durable transition is recovered by its existing owner after interruption.

## Installed provider execution

Adapters launch the installed tool directly using argument arrays and a canonical
working directory. Native permissions and settings remain effective. Read-only
selects a provider's native mechanism; it is not an independent Delegate sandbox.

Ordinary brief transport closes after the finite input. Machine-protocol adapters
use a core-owned channel even without user interaction; explicit clarification
and steering remain separate advertised capabilities.

Required help/entrypoint checks establish the observable launch capability;
native protocol and completion checks establish the result contract. Compatible
provider updates are accepted through behavior checks. Optional reported release
numbers are diagnostics, not admission criteria.

The [product boundary](../agents/_data/product-boundary.md) owns provider-installation
policy; the [adapter contract](../agents/_data/adapter-contract.md) owns launch and
interpretation requirements. Older admitted tasks retain their recorded contracts.

## Evidence and publication

Task files remain authoritative. A validated `outcome.json` is the terminal
winner; `result.txt`, raw streams and declared artifacts are evidence referenced
by validated descriptors. A queue result, provider exit or stop request cannot
stand in for that outcome. Committed evidence can be collected without a live
provider or supervisor.

Preparation rereads and validates the recorded request, metadata and brief.
Execution independently validates the exact brief descriptor it supplies.
Large brief validation streams bytes rather than retaining extra full copies.
Raw writers must finish before sealing and publication.

### Native output and derived recovery

Adapters consume bounded views of native records and stream accepted answer text
into core-owned staging. Unused metadata is skipped; consumed identity and
completion fields remain validated. Optional accounting is separate from success.

Saved interpretation revisions remain available for reproducible collection.
Explicit offline recheck reads the original sealed input and publishes a derived
answer/report separately. It does not relaunch the provider, replace the original
outcome, or change continuation authority. See
[offline recovery](../skills/agent-integration/references/result-handling.md#offline-recovery).

## One turn and continuation

A task has one immutable request and at most one provider launch attempt.
Continuation creates a separate task linked to one predecessor and its exact
native session. The session reservation must be released through validated
terminal or canonical lifetime-closure evidence; a queue end or timestamp alone
does not authorize another launch. Sealed predecessors are collected first.

A closed, unsealed turn can report interruption without inventing provider
success. Cancellation and explicit stop guidance take precedence over automatic
continuation. The successor uses its caller's context, preserves the required
recorded compatibility, and never silently selects a fresh session. Adapter-owned
successor rules may advance eligible historical contracts; predecessor records
remain unchanged. Native Codex review does not support continuation.

Exact eligibility and caller actions belong to the
[continuation reference](../skills/agent-integration/references/continuation.md).

## Fresh admission resource check

Only new dispatches and continuations check host resource pressure. Resource
observations belong to that invocation, not a background controller or shared
capacity reservation. The lifecycle owns the admission decision.

The policy distinguishes sustained CPU/memory pressure from transient readings.
Confirmed critical pressure can refuse immediately; an unavailable metric adds
a warning instead of refusing by itself. Confirmed pressure on another independent
metric can still refuse. Linux target-cgroup observations supplement host
observations where available; hidden ancestors and unsupported measurements limit
what can be established.

A refusal creates no ordinary task record. Running tasks, status, collect,
cancel and existing recovery are untouched. No work is paused or offloaded;
admission cannot guarantee that an accepted task avoids a later OOM.
See [resource response handling](../skills/agent-integration/references/result-handling.md)
for the caller's bounded retry or stop decision.

## Runtime discovery

Provider registration is static; runtime compatibility comes from required native
behavior. Providers, capabilities and advisory model discovery expose distinct
facts. Listing a model does not prove account access or successful execution.

Model discovery runs a bounded native inspection without admitting a provider
turn. Enumeration completeness and known effort metadata remain separate.
Negotiated machine-protocol versions and Delegate's recorded interpretation
revisions serve different purposes from a provider's optional release diagnostic.
See [discovery and model selection](../skills/agent-integration/references/command-reference.md).

## Scheduler storage

The
[schedule commands](../skills/agent-integration/references/command-reference.md#schedules)
manage definitions without launching providers. The separate foreground
[scheduler runtime](../skills/agent-integration/references/command-reference.md#scheduler-runtime)
drives due work under one execution owner per root.
Its application loop composes short claims, bounded admission and reconciliation
through consumer-owned ports. Task records remain execution/closure authority;
the database stores occurrence identity, reservations and cached observations.
Recurrence calculation is isolated in
`internal/scheduler/recurrence`; the application composes it with the storage
interface and keeps provider preparation in the existing execution path.
Ordinary dispatch, status and collect do not depend on the scheduler database.

### User-started scheduler host

The host design is under implementation; the following ownership contract is
not a claim of completed integrated or native acceptance.

The user starts `delegate --root ROOT scheduler serve` in an independent
terminal or tmux pane. The foreground host owns one exclusive host lease H per
canonical root, its desired-state writes, generation/attempt identity, child
channel, Start/Wait and restart timing. The scheduler child owns execution lease
E throughout joined runtime shutdown. Acquire H before E; compatibility
`scheduler run` uses the same arbitration and runs without automatic restart.

It announces its own foreground generation under H before acquiring E. Passive
status reports `foreground`; an older managed-host stop receipt cannot mask the
current invocation. Its joined-finish receipt is distinct from managed stop
acceptance. Public stop does not terminate it and instead returns
`stop_accepted: false` with `next_action: stop_foreground_runtime`; the user ends
the raw invocation in its original pane with Ctrl-C. Managed `serve` is preferred.

SQLite remains the sole durable calendar authority. The host does not keep a
second schedule list, create a Pueue infrastructure row, or gain provider retry
authority; admitted tasks keep their existing Pueue and guardian owners.

The host retains the user's launch context in memory and passes it explicitly
to each child, without persisting a full environment. The child uses an
independent session and an owned pollable control/readiness channel. Exact
root/generation/attempt and live host confirmation fence readiness and the first
scan. One successful Start owns one Wait; a readiness timeout, busy E or missing
heartbeat cannot authorize a replacement. A settled unexpected child exit or a
definitively failed Start that created no child can enter the host's
[restart policy](../skills/agent-integration/references/command-reference.md#scheduler-runtime).
Application composition supplies the child's before-Ready callback, which reads
and validates every current saved definition before readiness or the first scan.
An incompatible definition reports degraded state and disables automatic child
restart for that incompatibility, preserving definitions rather than retrying
the same unreadable input indefinitely.

Create/resume preserve the saved definition and ID, then check a compatible live
host. An absent host produces exit `3`, `schedule.saved: true`,
`scheduler.next_action: start_required` and exact `scheduler.serve_command` argv.
Application composition owns this response projection; JSON and human output
preserve argument boundaries. The [response contract](../skills/agent-integration/references/result-handling.md#schedule-definition-and-host-results)
keeps host readiness and optional stop acceptance separate from saved definitions
and completed shutdown. Pure transport unreachability after saving is pending
exit `3` without a top-level error; an owner-derived permanent
`scheduler.fault: true` yields exit `1` while preserving `schedule.saved` and its
ID. The host owns that fault decision; CLI projections do not infer it from
diagnostic strings or degraded state. These commands do not start a detached
host or reset an existing cooldown. Passive `scheduler status` observes the host
without spawning, migrating, repairing or cleaning anything.

Stop durably fences stopped intent and ends admission. H remains held and the
host keeps authenticated status/stop responsive while joining its child and
waiting for any predecessor that still holds E. After E is free, the H owner
acquires E and publishes a closure witness bound to the exact stopped intent.
It then finishes accepted controls and releases E before H. Completed stop
requires the validated witness and exact host absence; host death before the
witness remains accepted but pending. A validated authenticated acceptance
survives a later readback error, alongside that error. Timeout is not completed
shutdown. Independent tasks keep their owners.

Child crashes can recover while the host lives. Loss of the host/pane or reboot
requires a new explicit user start. The lifetime guard recognizes an explicit
Delegate task-scope marker and native Linux Delegate cgroup membership. It does
not reconstruct guardian ownership or historical ancestry, and cannot detect
every enclosing ancestor. Missing positives or a successful check do not attest
independent placement; the independent user-start contract remains explicit.
There is no OS service
installation, automatic detached bootstrap, new daemon dependency or negative
ancestry proof. Native
Linux/macOS candidate runs must verify this composed path. The acceptance driver
defaults to `managed-host` and keeps `service` as an optional legacy mode; adding
that mode does not establish completed native or real-provider verification.

### Calendar storage and evidence

History reads occurrence rows and compact skipped intervals from that same
store. Each occurrence keeps at most its last cached observation, explicitly
dated; it is not a second task lifecycle or a journal of every status poll.
Execution detail joins the occurrence with validated local task records through
a passive, rooted reader. Neither query calls task recovery, probes filesystem
capabilities or changes the cache. Task files remain authoritative even when a
historic cache is stale or its referenced files are unavailable.

At proven native closure, the application adapter can finish sealed-result
publication through the existing collection owner before recording its final
observation. Release and publication remain separate facts. A never-launched
disposition needs the existing permanent refusal plus complete lifecycle proof;
a missing queue row or elapsed time is not enough. See
[scheduled result handling](../skills/agent-integration/references/result-handling.md#scheduled-execution-results)
for inspection and same-task recovery decisions.

The scheduler owner lease alone may initialize or migrate
`<root>/.scheduler/schedules.sqlite`. Opening does not initialize or migrate it.
Schedule create owns initialization; serve requires existing schedule storage.
The database and directory must be private, user-owned and not symlinks.
Task files and artifacts are preserved; there is no task-history backfill.

Schema v1 has root metadata, schedules, occurrences and skip spans in strict
tables. Transactions verify schema/root identity and required SQLite settings.
One unreleased occurrence per schedule is database-enforced. Short immediate
transactions use a reserved connection; uncertain commits require readback.
Only a newly committed claim authorizes its invocation, not a rediscovered row.
Startup after downtime skips past appointments in compact spans; host/child
recovery never builds a catch-up queue or redispatches an uncertain task.

The adapter requires rollback `DELETE`, foreign keys, `synchronous=EXTRA` and
its busy timeout, with `fullfsync` on macOS. It refuses incompatible schemas or
modes rather than converting them. Durability remains SQLite's documented
semantics; a database transaction alone cannot promise exactly-once external
execution. The scheduler's contract and sealed adapter retain this boundary.
Existing compatible schema/data are reopened unchanged. Unsupported or corrupt
schemas fail explicitly; inspection does not repair them. Scheduler storage does
not migrate ordinary task JSON, results or logs into SQLite.

## Failure and recovery

Admission, liveness and publication are separate observations. Recovery rereads
durable evidence and preserves a terminal winner. Ambiguous submission or creation
never authorizes replay merely because a task ID or process is absent.

Response guidance distinguishes request correction, existing-task observation,
bounded resource retry, workspace observation, supervisor migration and stop/report.
The [canonical response procedure](../skills/agent-integration/references/result-handling.md)
owns their precedence. Migration preserves the original command intent: a failed
continuation cannot become a fresh dispatch. Optional maintenance is not permission
to repair provider settings or substitute another provider.

## Structured interaction within a turn

Explicit interaction uses a core-owned bounded stdio channel. Adapters translate
native messages; they do not own process control or terminal sessions.
The runner journals questions, while reply/steer commands append messages under
the journal's lock. Delivery is reserved durably before writing a native frame;
uncertain delivery is not automatically replayed.

Native completion closes input. The closed interaction journal is sealed with
the streams, while the guardian retains cancellation and the original deadline.
Delivery acknowledgement is not task success. A later conversational turn still
uses a separate linked task. See the
[interaction reference](../skills/agent-integration/references/interaction.md).
