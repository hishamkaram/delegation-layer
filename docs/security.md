# Security and isolation

Delegation Layer is designed for a local, single-user supervisor. It reduces
accidental cross-task access and ambiguous process outcomes; it does not make a
provider CLI or its account server trustworthy by itself.

## State and workspace boundaries

The state root, workspace, declared provider runtime directories, and temporary paths
are canonicalized and checked for overlap. Task records are stored in private
directories with create-once files and bounded names. The provider receives the
workspace selected by the request and the adapter's effective policy, not an
arbitrary path supplied as raw arguments.

## Credentials and environment

Provider authentication is native. The child process can use the provider's
own account home and login cache, while Delegation Layer persists only bounded
control values needed to reproduce the launch. It does not copy, rewrite, or
move provider credentials into task state. Native provider behavior may still
update its own cache or account metadata as part of a normal invocation.

The released CLI creates a private Pueue configuration, socket, keys, and
daemon data below the state root. Keep the state root private and outside any
provider-writable workspace. An explicitly selected external configuration
remains under the caller's supervisor controls.

## Native permissions and launch checks

Before admission and again immediately before launch, the system compares the
executable identity, command profile, canonical workspace, effective policy,
input/output bindings, and session expectation. A provider executable replaced
between checks is rejected. Provider version numbers are observed for evidence
but are not hardcoded release constraints.

Provider CLIs load and enforce their own settings, MCP servers, hooks, plugins,
and skills. New tasks do not inventory these sources or disable them. Permission
flags request native behavior; custom tools and extensions remain governed by
the provider. Delegation Layer does not independently contain their effects.
Workspace-write requests unattended native execution. Antigravity read-only
uses exactly `--sandbox --mode plan` without a bypass; its authorized
workspace-write path retains `--sandbox --mode accept-edits` with the existing
native bypass. Claude uses permission bypass, Pi uses native configured tools,
and OpenCode uses its automatic build mode. These modes can permit access
beyond the selected workspace; configured native restrictions still belong to
each provider. Previously admitted tasks retain their recorded preparation
contract.

Model discovery reports bounded, nonsecret model facts through the same
state-rooted inspection boundary. Its output is advisory and never an
admission allowlist. A null effort list is unknown metadata; an empty list is
an explicit report of no choices, and harness-wide choices are not assumed to
apply to every model.

## Process and output safety

The supervisor owns process lifetime and the runner uses bounded capture. The
brief, control JSON, and raw streams have size limits. Declared output artifacts
must remain inside their approved roots and match the recorded integrity data.
Only a validated predicate interpreter can turn provider output into a
published result.

## Operational limits

This is a local supervisor, not a hostile multi-tenant sandbox. A provider can
modify files that its selected native policy permits, contact its own service,
or write provider-managed caches. Provider CLIs evolve independently; a
changed command flag, output contract, or authentication behavior can cause a
task to be rejected until its adapter is updated. The runtime capability probe
detects command and flag drift early, while the behavioral predicates protect
the result boundary.
