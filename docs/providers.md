# Provider guide

Delegation Layer ships five launchable provider profiles. The profile name is
part of the task request and determines the command, environment, policy,
identity observer, input transport, output interpreter, and supported options.

| Profile | Permission mode | Request options | Native command |
| --- | --- | --- | --- |
| `antigravity:print` | `workspace-write` | `continuation`, `native-timeout` | `agy` |
| `codex:exec` | `read-only`, `workspace-write` | `continuation` | `codex exec` |
| `claude:print` | `read-only`, `workspace-write` | `continuation` | `claude` print mode |
| `pi:json` | `read-only` | `continuation`, `model`, `effort` | `pi --mode json` |
| `opencode:run` | `read-only`, `workspace-write` | `continuation`, `model`, `effort` | `opencode run --format json` |

The catalog is available locally with `delegate providers --json`. Its runtime
metadata describes the help arguments and flags required by each adapter.
The catalog uses public request option names; the native flag mapping is
documented in each provider section below.

The binaries build for Darwin and Linux targets, but native policy checks can
be more restrictive. The Codex profile currently requires Darwin to inspect
managed preferences, and the Claude profile currently requires Darwin to use
the system Keychain policy check. Antigravity has no corresponding macOS-only
helper gate; its native CLI and workspace policy still determine whether a task
can run on a given host.

## Shared compatibility behavior

Before a task is admitted, the selected adapter resolves a canonical regular
executable and records its observed identity. A supervised runtime probe then:

1. invokes the executable's version command and accepts any nonempty, valid
   reported version;
2. invokes the provider help command, including any profile-specific subcommand;
3. checks that every complete flag used by the adapter is advertised; and
4. fingerprints the executable again before the profile is finalized.

This detects missing commands, non-executable files, and incompatible flag
changes without pinning a release number or binary hash. A reported version
is valid when it is trimmed nonempty UTF-8 text without control characters; no
semantic-version pattern is required. A newer provider build is accepted when
it preserves the command capabilities the adapter needs.

The probe does not replace provider behavior validation. Each adapter still
requires its expected output shape, task and session identity, containment,
effective policy, authentication result, and output artifact integrity.

## Antigravity

`antigravity:print` runs `agy` with the `workspace-write` policy. The adapter
declares the workspace and provider runtime directories it may write, validates
the provider's configured workspace trust and sparse default project, and
passes the brief through standard input. `--native-timeout` sets agy's own print
timeout inside the task's wall-clock `--budget`.

The CLI uses its native signed-in account and home. A missing or expired login
is reported as the provider's authentication result and does not cause
Delegation Layer to copy credentials into task state.

## Codex

`codex:exec` invokes the `exec` subcommand with strict native launch settings.
The caller chooses `read-only` or `workspace-write`, which is passed to Codex's
native sandbox flag. Continuation uses the exact predecessor task ID.

The adapter deliberately rejects unsupported model, effort, policy, and native
timeout combinations. Codex's own authentication remains in the native Codex
home and is evaluated by the CLI at launch.

## Claude

`claude:print` launches Claude in print mode with the adapter's declared tool
and policy settings. The caller chooses `read-only` or `workspace-write`; that
choice is passed through Claude's native permission mode. It sends the brief via
the recorded input binding, and validates the structured stream, identity, and
result artifact. Continuation names an exact predecessor task.

The adapter accepts any valid version reported by the current Claude CLI after
the shared capability probe. It still rejects changed output shape, missing
required flags, policy drift, identity mismatch, or an invalid result.

On Darwin, the strict profile may use the system credential helper for a bounded
native policy check; only non-secret policy facts are persisted. Other hosts
report the native policy prerequisite as unavailable until an equivalent
platform contract is implemented.

## Pi

`pi:json` runs Pi's JSON event mode directly in read-only mode. Its native
allowlist is limited to `read`, `grep`, `find`, and `ls`; `--no-extensions` and
`--offline` also prevent extension execution and startup package or network
work. Continuation uses the exact Pi session identifier. The public `model`
option is passed through, and public `effort` maps to Pi's native
`--thinking` flag. Pi's built-in `write` and `edit`
tools accept arbitrary absolute paths and do not expose a native workspace
boundary, so `workspace-write` is not advertised until Pi provides one.

## OpenCode

`opencode:run` runs OpenCode's JSON event mode directly in the requested
workspace. The caller chooses the permission mode. Read-only binds the run to
an adapter-owned agent, uses OpenCode's pure mode, and supplies an inline native
permission configuration with deny rules at both global and agent scope for
mutation, shell, subagents, network, and unknown tools while allowing inspection
tools, and disables automatic compaction. Workspace-write binds a separate
adapter-owned agent that allows file inspection and edits while denying shell,
subagents, network, and unknown tools; it disables project configuration and
automatic compaction, keeps OpenCode's native
`external_directory: deny` boundary, and uses the caller-selected `--auto`
approval. The launch also disables system and global Git configuration so an
ambient `core.worktree` setting cannot change the native checkout boundary.
OpenCode's native patch-move handling checks the source path but treats
destinations anywhere in the enclosing Git checkout as internal, so
workspace-write is admitted only when the selected workspace is that checkout's
root. Symlinked workspace trees and permission glob metacharacters are refused.
Continuation and model are passed through as native options. Public `effort`
maps to OpenCode's native `--variant` option.

## Adding a provider

An adapter should expose a small, explicit profile rather than leaking raw
provider arguments into the public CLI. Register its ID, permission modes,
options, help arguments, and required flags; implement static preparation and a
finalizer; declare input and output artifacts; provide an identity observer; and
register a strict predicate interpreter for the provider's output.

Keep process lifetime, stopping, capture, sealing, collection, and publication
in the shared core. The provider owns only its command construction, bounded
environment and policy calculation, identity observation, and output
interpretation. Add unit tests for capability drift and predicate failures,
then document the profile here and in the discovery output. The development
workflow is described in [Contributing](contributing.md).
