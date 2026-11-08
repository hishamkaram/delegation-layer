# Maintainability and architecture

This document owns architecture, ownership and review guidance.
[AGENTS](../../AGENTS.md) owns collaboration/check reporting; the
[product boundary](product-boundary.md) owns product scope. Read only relevant
sections. No contributor ledger, handoff index, private implementation baseline
or universal full-contract read is required.

## Boundary and ownership

Name the authoritative owner, writers, readers and invariant before changing
state or an interface. Mutable resources stay behind their owner; other modules
consume a narrow public boundary or pure values. Interfaces belong to consumers.

Separate data, pure decisions and I/O. Small cohesive domains may keep values
and validity rules in one package. Do not create empty layers, packages per
helper, forwarding caches or global model/store/service dumping grounds.
An abstraction needs a concrete consumer, invariant or demonstrated reuse.

The [delegation invariants](delegation-invariants.md#3-supervision-deadlines-and-forbidden-signals)
own runtime lifecycle. The [adapter contract](adapter-contract.md) owns provider
preparation/interpretation, not shared process/capture/publication mechanics.
Use one startup catalog, not hidden singleton state or init registration.

## Architecture guidance

Keep modules cohesive and dependencies directed toward data and pure decisions.
Applications compose runtime owners through their public boundaries. A wrapper
holding another owner is not a new boundary. Preserve real supported consumers,
persisted formats and error identities, not every private export/signature/test ID.

Review changed dependencies against the affected owners and real consumers.
Graphs describe relationships; they do not establish whether a design is sound.
The [code-quality floor](code-quality-floor.md) owns compiler, linter, formatter
and test requirements.

### Module organization

Review repeated filename families for a shared capability or responsibility.
Use a cohesive package when it makes that owner and its dependencies clearer;
import defining owners instead of reaching through broad compatibility facades.
Review the complete capability: command entrypoints, implementation, fixtures,
tests and actual consumers. Keep entrypoints thin and put feature tests with
their owner; shared test setup must not become a production dependency. A root
entrypoint or governance tool can remain there when its real consumer or
repository-wide responsibility justifies it.

When moving a capability, migrate its maintained imports, command paths, test
discovery, source-evidence closure and documentation together. Verify the
selected cases still execute; a passing empty filter is not preserved coverage.
Do not leave a second implementation or an unused forwarding facade behind.

Prefixes, flat files, file length and one consumer are not defects by themselves.
Keep related Go files in one package when extraction would create cycles,
forwarding layers or artificial callback interfaces. A move needs a concrete
ownership, contract, dependency or test-selection benefit; remove obsolete
facades after their consumers migrate instead of replacing them with new ones.

## Lifecycle tests

The [delegation invariants](delegation-invariants.md) own terminal authority,
create-once recovery, process ownership and launch ordering. Normal execution
and recovery reuse the same policy, resolver and finalizer. No command,
adapter or record family gets a second lifecycle implementation.

Trace indirect consumers before tightening a shared gate. Separate observation,
optional maintenance and mutation authority; pair allowed reads and refused
mutations in the same durable state. Preserve error identities, exit codes and
JSON guidance through the actual public consumer, not just helper tests.

Durable-write/crash/recovery tests use production stores and real consumers,
covering absent/matching/conflicting/unreadable create-once results. Fakes prove
isolated ordering, not native containment or durable recovery. No mandatory
transition IDs, matrix, handoff schema or source-symbol inventory is needed.

Concurrency tests synchronize at the owner's boundary. Sleeps and launch
barriers do not prove internal overlap. Separate hang watchdogs from latency
requirements and join owned children/goroutines on failure. Running capacity
requires live tasks/leases/scopes together; queue depth and dispatch-burst
throughput are different measurements.

## Whole-error pending classification

Classify pending/busy with the owning domain's canonical policy over the whole
error. A marker errors.Is check can hide a joined storage or unlock fault.
Workspace operations use workspaces.PendingError; other domains keep their
own owners and marker semantics, not a new global framework.

The workspace policy requires its marker and only known pending leaf causes.
A bare deadline is not pending; a concrete fault joined to a marker remains
a fault. Preserve Close/unlock errors and the consumer's pending-only or
pending-or-busy meaning. Test pending/deadline, busy and joined faults through
the actual public consumer.

## Recovery and verification

Normal execution and journal resumption share one policy. Preserve committed
progress; never reconstruct an earlier launch after an ambiguous write. Keep
durable barriers and cleanup owned after staging starts despite cancellation.
Propagate phase contexts through locks/storage; a watcher timeout cannot replace
the worker's deadline or authorize another launch.

Only absence of the requested record establishes absence. A missing validation
dependency after reading it is unreadable evidence. Preserve underlying errors
and use the store's canonical absence classifier.

Numeric supervisor IDs are reusable. Identity validation and numeric mutation
share the exclusive supervisor lock until in-flight operations settle. Observers
revalidate under their read lease; cached readiness grants no mutation authority.
Keep bootstrap, migration and queue scans out of supervised-worker startup.

Cleanup triggers/queue rows are observations, not removal authority. Preserve
consumers that need operational state until they can finish from durable facts.
Admission does not wait for background maintenance. Shutdown acknowledgement
is not independently observed process exit.

One Start owner owns one Wait. Retain pending-command handles, original errors
and resource cleanup before outer-boundary sanitization. Directory/FIFO removal
requires pinned invocation-owned object and parent identities; replacement,
unknown closure or a pathname alone cannot authorize deletion. Never remove
a shared/default parent. Failed maintenance needs a later request/event, not a
busy loop. Current runtime details live in the [architecture guide](../../docs/architecture.md#failure-and-recovery).

## Managed workspace changes

The application coordinator owns workspace lifecycle; Git only executes
authorized operations. Preserve common-directory resolution, topology leases,
immutable bindings, checkpoint lineage, capture/finalization and retained
recovery. Test fresh/continuation paths in affected permission modes, including
Git/non-Git controls. Never initialize Git in every generic fixture. See
[workspace architecture](../../docs/architecture.md#managed-git-workspace-lifecycle)
and [agent guidance](../../skills/agent-integration/references/workspaces.md).

## Portable paths and temporary files

Use Go t.TempDir/os.MkdirTemp/os.CreateTemp or Python tempfile and native joins.
Do not hard-code shared temp roots, host separators or null devices. An explicit
state root or observed absolute artifact path is valid; absolute paths are not
inherently wrong, and shared parents are never disposable.

Canonicalize filesystem identity at its owner and carry that form through
comparisons. Test macOS aliases using a test-owned symlink and canonical control,
not fixed /tmp or /var spellings. Preserve intentional no-symlink boundaries.
Git/manifest POSIX strings are wire paths: validate them and convert at the I/O
edge. Isolate OS primitives. Missing optional flags must preserve the guarantee
or return a stable unsupported failure, never AttributeError or silent downgrade.

## Native platform checks

Linux/macOS are supported native hosts. Verify new primitives on the actual
affected host before dependent implementation. Cross-builds, another kernel's
containers and upstream portability claims do not prove native process,
endpoint, descriptor or recovery behavior. Windows is not currently claimed.

Keep policy shared and only primitives OS-specific. Preserve the backend's
assurance: cgroup identity can provide exact containment; cooperative sessions
need their exact live owner to prove emptiness. Persist prepared intent before
creation; do not reselect a backend afterward.

Changed native paths need appropriate start/Wait, deadline/descendant,
identity-reuse, interruption/recovery and cleanup cases. Endpoint limits and
descriptor/session behavior need native checks. Fixture, native-lifecycle,
package-path and live-provider results establish different claims. Preserve
functional bounds and disclose unavailable hosts.

## Provider checks

Use the requested provider/model/effort/native permissions and existing auth.
Do not alter settings, install a substitute or retry uncertain paid work for
a pass. Follow [agent integration](../../skills/agent-integration/SKILL.md) for
external work and observation.

Run affected scenarios when behavior needs them, not a full paid matrix for
unchanged runtime. Missing prerequisites are blocked/unverified. A development
binary does not prove the default installation. Preserve test-driver closure,
bounded finalization and identity-owned cleanup; keep private output out of
public artifacts and report actual request options. No contributor ledger or
custom proof receipt is required.

## Review dimensions

Review integrated behavior, ownership, dependencies, real consumer compatibility,
errors and secret handling. Small files alone do not prove cohesion. Fix
in-scope findings and recheck affected consumers; report unrelated findings
separately instead of expanding the ticket. Skill authoring/evaluation format
belongs in the [contributor guide](../../docs/contributing.md#repository-skills).
