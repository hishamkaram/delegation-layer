# Delegation Layer

Delegation Layer runs one bounded turn of a supported AI command line tool as a
durable, supervised local task. It records the request before submission,
checks the executable and its capabilities at runtime, and publishes a
validated result that can be collected later.

> **Private preview**
>
> The project is under active development. Keep the repository and any release
> artifacts private while evaluating it.

## What it does

The `delegate` command accepts a finite brief, a canonical workspace, a
permission mode, and a wall clock budget. It prepares a provider-specific
launch profile, submits exactly one provider turn through Pueue, and stores
immutable task records and output descriptors under a private state root.

The companion `delegate-run` executable is started by the supervisor. It
reconstructs the recorded profile, verifies that admission and launch still
refer to the same executable, policy, workspace, and request, then captures and
seals the provider result. A later `collect` call can recover a committed
outcome without launching the provider again.

## Requirements

- Go 1.27.1, the repository's pinned toolchain.
- Pueue 4.0.4 and a running `pueued` daemon using a private configuration.
- A signed-in installation of one or more supported provider CLIs: `agy`,
  `codex`, `claude`, `pi`, or `opencode`.
- A Darwin or Linux host on amd64 or arm64 for the supplied build targets.
  Provider availability is profile-specific; see the provider guide for native
  platform prerequisites.

The provider executable must be available to the process that dispatches the
task. Delegation Layer runs the provider's version and help commands before
admission and accepts any reported version that is valid and advertises every
flag required by the adapter. It does not pin a provider release or binary
hash. A valid version here means nonempty trimmed UTF-8 output without control
characters; it is not required to match a semantic-version pattern.

## Install

### Private release artifacts

When a private release is published, download the archive for your operating
system and architecture with an authenticated GitHub client, verify its
checksum, and place `delegate` and `delegate-run` on your `PATH`.

### Build from source

```sh
git clone https://github.com/hishamkaram/delegation-layer.git
cd delegation-layer
mkdir -p "$HOME/.local/bin"
CGO_ENABLED=0 go build -buildvcs=false -o "$HOME/.local/bin/delegate" ./cmd/delegate
CGO_ENABLED=0 go build -buildvcs=false -o "$HOME/.local/bin/delegate-run" ./cmd/delegate-run
```

Ensure `$HOME/.local/bin` is on `PATH`, or install the binaries in another
directory already on `PATH`.

## Quick start

Create a brief and a workspace outside the state root. Use an absolute path to
the Pueue configuration that controls the private daemon:

```sh
mkdir -p "$HOME/delegation-workspace"
printf '%s\n' 'Inspect the repository and summarize the current build status.' > brief.txt

delegate --pueue-config /absolute/path/to/pueue.yml dispatch \
  --provider codex:exec \
  --brief "$PWD/brief.txt" \
  --cwd "$HOME/delegation-workspace" \
  --permission read-only \
  --budget 30m \
  --json
```

The response contains a task ID and separate admission, liveness, and
publication fields. Observe it with `status`, wait for a terminal publication,
then read the validated result with `collect`:

```sh
delegate status TASK_ID --json
delegate collect TASK_ID --watch 5s --json
```

Use the exact task ID printed by `dispatch`. A continuation is a new task that
names its predecessor with `--resume-task`; it never silently reuses the most
recent conversation.

## Providers

| Profile | Modes | Options |
| --- | --- | --- |
| `antigravity:print` | `workspace-write` | `continuation`, `native-timeout` |
| `codex:exec` | `read-only`, `workspace-write` | `continuation` |
| `claude:print` | `read-only`, `workspace-write` | `continuation` |
| `pi:json` | `read-only` | `continuation`, `model`, `thinking` |
| `opencode:run` | `read-only`, `workspace-write` | `continuation`, `model`, `variant` |

Run `delegate providers --json` for the compiled capability catalog. See
[providers](docs/providers.md) for launch behavior, policy boundaries, and
runtime compatibility checks.

## Safety model

- Every task has one immutable request and one provider turn.
- State, workspace, provider runtime, and temporary directories are canonical
  and kept disjoint where the selected policy requires it.
- The supervisor owns process lifetime; the runner owns capture and sealing.
- Admission and launch re-check executable identity, effective policy, and
  session identity.
- Briefs and control responses are bounded. Sealed payloads and raw streams are
  described by validated paths, sizes, and SHA-256 digests.
- Provider authentication remains native to the provider CLI. Delegation Layer
  does not copy credentials into task state or rewrite account settings.

Read [security](docs/security.md) for the full boundary and its operational
limitations.

## Documentation

- [Getting started](docs/getting-started.md) — install, configure, and run a
  first task.
- [CLI reference](docs/cli-reference.md) — commands, flags, responses, and
  exit codes.
- [Integration API](docs/api.md) — JSON responses, state semantics, and
  idempotent client behavior.
- [Provider guide](docs/providers.md) — supported profiles and compatibility
  behavior.
- [Architecture](docs/architecture.md) — task lifecycle and ownership.
- [Security](docs/security.md) — isolation, policy, and result integrity.
- [Troubleshooting](docs/troubleshooting.md) — common setup and runtime
  failures.
- [Contributing](docs/contributing.md) — development setup and adapter
  extension guidance.

## License

No public license has been declared yet. Treat this private preview as
proprietary and do not redistribute it without the project owner's permission.
