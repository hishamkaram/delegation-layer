# Security and isolation

Delegation Layer is designed for a local, single-user supervisor. It reduces
accidental cross-task access and ambiguous process outcomes; it does not make a
provider CLI or its account server trustworthy by itself.

The Claude Code and Codex plugin has one bootstrap responsibility: verify that
the Delegation Layer CLI is usable and install the checksum-verified release bundle when
it is missing. It does not install provider CLIs, read provider credentials,
or change provider settings. If a newly installed binary is outside the
current harness `PATH`, the hook stops with a PATH and new-session instruction
instead of allowing direct provider execution.

## State and workspace boundaries

The state root, workspace, declared provider runtime directories, and temporary paths
are canonicalized and checked for overlap. Task records are stored in private
directories with create-once files and bounded names. The provider receives the
workspace selected by the request and the adapter's effective policy, not an
arbitrary path supplied as raw arguments.

Managed Git workspaces are optional cooperative checkout separation, not a
security sandbox or read-only guarantee. They resolve a committed base and
exclude dirty/untracked source changes from the managed checkout, but shared
Git refs, configuration, and other global resources remain shared. Ignored
runtime files, submodule interiors, empty directories, extended attributes, and
LFS payloads are not promised as Git-content backup. A clean Git status is not
proof that no write occurred outside the promised content scope. See the
[managed workspace reference](../skills/agent-integration/references/workspaces.md)
for capture and retention boundaries.

Delegate serializes its own native worktree metadata commands using the
canonical common-directory inode. This advisory lease does not exclude an
external Git process or another same-user process that replaces repository
paths. It does not repair incomplete metadata left by interrupted Git commands.

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
input/output bindings, and session expectation. A provider upgrade observed by
the fresh preflight is accepted when its capability check succeeds; a replacement
during the final binding and start handoff is rejected. Provider version numbers
are observed for evidence but are not hardcoded release constraints.

Provider CLIs load and enforce their own settings, MCP servers, hooks, plugins,
and skills. New tasks do not rewrite these sources. A provider-specific
permission contract may inspect documented startup state and pass native
controls required to make the requested mode effective; capability inspection
records those controls before launch. Permission flags request native behavior;
Delegation Layer does not independently contain provider effects.
Workspace-write requests unattended native execution. Antigravity read-only
uses exactly `--sandbox --mode plan` without a bypass; its authorized
workspace-write path retains `--sandbox --mode accept-edits` with the existing
native bypass. Claude uses permission bypass, Pi read-only uses its native tool
allowlist with extensions disabled, Pi workspace-write explicitly selects its
built-in `read`, `bash`, `edit`, and `write` tools, and OpenCode uses its
automatic build mode. These modes can permit
access beyond the selected workspace; configured native restrictions still
belong to each provider. Previously admitted tasks retain their recorded
preparation contract.

Model discovery reports bounded, nonsecret model facts through the same
state-rooted inspection boundary. Its output is advisory and never an
admission allowlist. A null effort list is unknown metadata; an empty list is
an explicit report of no choices, and harness-wide choices are not assumed to
apply to every model.

## Process and output safety

Pueue owns scheduling and worker-row state. The application lifetime state
machine owns native stopping and empty-scope termination proof for live and
recovery paths. Durable storage owns its intent, registration, exact worker,
launch, termination, and managed-worker completion records. The execution runner
owns bounded capture and provider `Start`/`Wait`. The live guardian retains its
native scope handle and lifetime-owner lease through finalization. Native scope
disposal occurs only after termination and either the exact managed-worker
`Wait` or an explicit `not-started` disposition is durable. The
brief, control JSON, and raw streams have size limits. Declared output artifacts
must remain inside their approved roots and match the recorded integrity data.
Only a validated predicate interpreter can turn provider output into a
published result.

Linux cgroup v2 scopes are identity-bound and provide exact containment when
the current delegated cgroup permits them. Darwin and Linux fallback private
sessions are cooperative. The Linux guardian binds itself as the sole child
subreaper before worker start. Its exact identity and the worker identity are
observer-only topology anchors, never signal targets. It reaps only adopted
task zombies by exact PID, leaving the worker's single `Wait` to the execution
runner. A live Linux guardian proves emptiness through two fresh topology
observations under its exact subreaper identity; it does not inspect unrelated
process environments. Darwin combines live topology with an inherited per-task
marker because it has no subreaper primitive. New Darwin tasks use a private
session and signal each matched process through its current audit-token identity
after checking the stable unique identity; a reused numeric PID is never
sufficient authority. Immutable legacy tasks keep their historical group pin
until they drain. After either cooperative owner is lost, recovery may terminate
durable identities and connected descendants but cannot prove the scope empty.
Delegate reports that uncertainty and never publishes false termination
evidence.

## Operational limits

This is a local supervisor, not a hostile multi-tenant sandbox. A provider can
modify files that its selected native policy permits, contact its own service,
or write provider-managed caches. Provider CLIs evolve independently; a
changed command flag, output contract, or authentication behavior can cause a
task to be rejected until its adapter is updated. The runtime capability probe
detects command and flag drift early, while the behavioral predicates protect
the result boundary.

## Interactive input

Clarification replies are data for a specific pending question; they do not grant
new permissions. Steering changes guidance within the existing task authority.
Neither command changes model, workspace, permission, or deadline. Native tool
approval requests are not exposed as questions or automatically approved by this
protocol. Existing provider-native permission modes still apply.

Question text, answers, and steering are stored in the private task journal and
sealed evidence. Treat them as task content, including any sensitive content the
caller supplies. An uncertain delivery is never automatically repeated. The
interaction transport is bounded stdio, with no PTY or network listener.
