# Delegation Layer

Delegation Layer runs one bounded turn of a supported AI command line tool as a
durable, supervised local task. It records the request before submission,
checks the executable and its capabilities at runtime, and publishes a
validated result that can be collected later.

> **Preview release**
>
> The project publishes checksum-verified CLI releases and an npm package with
> installers for the CLI and integration skill. The provider CLI and Pueue
> remain host prerequisites.

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

- Go 1.27.1 is needed only for source builds and the Homebrew formula.
- Pueue 4.0.4 and a running `pueued` daemon using a private configuration.
- A signed-in installation of one or more supported provider CLIs: `agy`,
  `codex`, `claude`, `pi`, or `opencode`.
- A Darwin or Linux host on amd64 or arm64 for the supplied build targets.
  Provider availability is profile-specific; see the provider guide for native
  platform prerequisites.
- Node.js 18 or newer is needed for the npm and pnpm installers; Bun is needed
  for the Bun installer.

The provider executable must be available to the process that dispatches the
task. Delegation Layer runs the provider's version and help commands before
admission and accepts any reported version that is valid and advertises every
flag required by the adapter. It does not pin a provider release or predeclared
binary hash. A valid version here means nonempty trimmed UTF-8 output without control
characters; it is not required to match a semantic-version pattern. The
observed version is diagnostic evidence, while capability flags, executable
identity, and provider behavior decide compatibility. No web release lookup or
semver allowlist is used.

## Install

### Install the CLI

The shell installer downloads the latest published release, verifies its
checksum, and installs both `delegate` and `delegate-run` in
`$HOME/.local/bin`:

```sh
curl -fsSL https://raw.githubusercontent.com/hishamkaram/delegation-layer/main/install.sh | sh
```

Set `DELEGATION_LAYER_INSTALL_DIR` if you want to install somewhere other than
`$HOME/.local/bin`.

Homebrew installs the same two executables:

```sh
brew install hishamkaram/tap/delegation-layer
```

The npm package also provides a checksum-verified CLI installer through npm,
pnpm, and Bun:

```sh
npx --yes delegation-layer install-cli
pnpm dlx delegation-layer install-cli
bunx --bun delegation-layer install-cli
```

For a source build, install both Go commands from the latest version:

```sh
go install github.com/hishamkaram/delegation-layer/cmd/delegate@latest
go install github.com/hishamkaram/delegation-layer/cmd/delegate-run@latest
```

Ensure the directory used by the installer or Go is on `PATH`.

Confirm the installation:

```sh
delegate --help
delegate providers --json
```

### Install the agent integration skill

The provider-agnostic skill is separate from the CLI installation. Hermes can
install the current skill directly:

```sh
hermes skills install \
  https://raw.githubusercontent.com/hishamkaram/delegation-layer/main/skills/agent-integration/SKILL.md
```

Any harness with a writable skill directory can use the dependency-free npm,
pnpm, or Bun installer:

```sh
npx --yes delegation-layer \
  --target "$HOME/.hermes/skills/agent-integration"
pnpm dlx delegation-layer \
  --target "$HOME/.hermes/skills/agent-integration"
bunx --bun delegation-layer \
  --target "$HOME/.hermes/skills/agent-integration"
```

The default package command copies only `SKILL.md` into the target directory.
The package's explicit `install-cli` command installs the CLI separately; it
does not change the skill or configure Pueue or a provider CLI. Release tags
and `DELEGATION_LAYER_VERSION` remain available when a deployment needs a
reproducible version.

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
| `pi:json` | `read-only` | `continuation`, `model`, `effort` |
| `opencode:run` | `read-only`, `workspace-write` | `continuation`, `model`, `effort` |

Run `delegate providers --json` for the compiled capability catalog. See
[providers](docs/providers.md) for launch behavior, policy boundaries, and
runtime compatibility checks.
Use `delegate capabilities --provider PROFILE --json` for one provider's
provider-neutral contract. Its catalog response is descriptive; dispatch is
where the supervised runtime probe establishes host compatibility.

When native logins are available, `make acceptance-native` runs the Pi and
OpenCode live gates through a private supervisor. A pass exits `0`; an
unavailable executable, supervisor, or login emits a sanitized `BLOCKED`
receipt and exits `2`, which the aggregate target treats as neutral. Real
behavior and evidence failures still fail the target.

The catalog uses public request option names. The public `effort` option maps
to Pi's native `--thinking` flag and OpenCode's native `--variant` flag.

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
- [System components](diagrams/components.html) — current Archify component map.
- [Task lifecycle](diagrams/lead-interface.html) — current Archify sequence view.
- [Security](docs/security.md) — isolation, policy, and result integrity.
- [Troubleshooting](docs/troubleshooting.md) — common setup and runtime
  failures.
- [Contributing](docs/contributing.md) — development setup and adapter
  extension guidance.
- [Agent integration skill](skills/agent-integration/SKILL.md) — provider-neutral
  JSON and evidence handling guidance for agents.

## License

No public license has been declared yet. Treat the source and release artifacts
as proprietary and do not redistribute them without the project owner's
permission.
