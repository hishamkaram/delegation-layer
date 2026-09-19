# Security and isolation

Delegation Layer is designed for a local, single-user supervisor. It reduces
accidental cross-task access and ambiguous process outcomes; it does not make a
provider CLI or its account server trustworthy by itself.

## State and workspace boundaries

The state root, workspace, provider runtime directories, and temporary paths
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

## Policy and drift checks

Before admission and again immediately before launch, the system compares the
executable identity, command profile, canonical workspace, effective policy,
input/output bindings, and session expectation. A provider executable replaced
between checks is rejected. Provider version numbers are observed for evidence
but are not hardcoded release constraints.

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
