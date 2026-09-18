# Getting started

This guide takes a local installation from zero to one supervised provider
turn. It assumes the provider CLI itself is already installed and can be
authenticated using its own supported login flow.

## Prepare the host

Install Pueue 4.0.4 (`pueue` and `pueued`) and at least one
of `agy`, `codex`, `claude`, `pi`, or `opencode`. Start a Pueue daemon with a
private configuration and Unix socket. Keep that configuration outside the
Delegation Layer state root and workspace. Node.js 18 or newer is needed for
the optional npm or pnpm installer; Bun is needed for the Bun installer.

Authenticate each provider with its normal CLI workflow. Delegation Layer
passes the provider's native home and bounded control environment through to
the child process. It does not copy account secrets into task state; Claude's
strict profile may use its native credential helper for policy admission.

## Install the CLI

Use the checksum-verified shell installer:

```sh
curl -fsSL https://raw.githubusercontent.com/hishamkaram/delegation-layer/main/install.sh | sh
```

Or use Homebrew, npm, pnpm, or Bun:

```sh
brew install hishamkaram/tap/delegation-layer
npx --yes delegation-layer install-cli
pnpm dlx delegation-layer install-cli
bunx --bun delegation-layer install-cli
```

For a source build, install Go 1.27.1 and run:

```sh
go install github.com/hishamkaram/delegation-layer/cmd/delegate@latest
go install github.com/hishamkaram/delegation-layer/cmd/delegate-run@latest
```

Confirm the installation:

```sh
delegate --help
delegate providers --json
```

The discovery command lists compiled profiles only. Dispatch performs the
host-specific executable, version, and help checks. See the [README install
section](../README.md#install-the-cli) for version pinning and install
directory options.

## Install the agent skill

The agent integration skill is installed separately from the CLI. Hermes can
install the current skill directly:

```sh
hermes skills install \
  https://raw.githubusercontent.com/hishamkaram/delegation-layer/main/skills/agent-integration/SKILL.md
```

For a harness with a writable skill directory, use any of the dependency-free
package installers:

```sh
npx --yes delegation-layer \
  --target "$HOME/.hermes/skills/agent-integration"
pnpm dlx delegation-layer \
  --target "$HOME/.hermes/skills/agent-integration"
bunx --bun delegation-layer \
  --target "$HOME/.hermes/skills/agent-integration"
```

The default package command installs only `SKILL.md`. Use the explicit
`install-cli` command when installing the CLI through pnpm or Bun.

## Run a first task

Choose two separate absolute directories: a workspace the provider may inspect
and a state root where task records will live. Write a finite brief outside the
workspace if you do not want the provider to edit it.

```sh
mkdir -p "$HOME/delegation-workspace" "$HOME/delegation-state"
printf '%s\n' 'List the top-level files and summarize the project.' > "$HOME/delegation-brief.txt"

delegate --root "$HOME/delegation-state" \
  --pueue-config /absolute/path/to/pueue.yml \
  dispatch \
  --provider codex:exec \
  --brief "$HOME/delegation-brief.txt" \
  --cwd "$HOME/delegation-workspace" \
  --permission read-only \
  --budget 30m \
  --json
```

Save the returned `task_id`. The command records the request and submits one
bounded provider turn. It does not wait for the provider's answer.

## Observe and collect

Use the task ID to inspect progress:

```sh
delegate --root "$HOME/delegation-state" status TASK_ID --json
delegate --root "$HOME/delegation-state" collect TASK_ID --watch 5s --json
```

Repeat `collect` while the response is pending. Once publication is terminal,
the JSON response includes an outcome and validated descriptors for the result
payload and raw streams. A rejected outcome is still a terminal, inspectable
result and exits with code 4.

## Continue a conversation

When the provider profile supports continuation, create a new task and name the
exact predecessor:

```sh
delegate --root "$HOME/delegation-state" \
  --pueue-config /absolute/path/to/pueue.yml \
  dispatch \
  --provider codex:exec \
  --brief "$HOME/follow-up.txt" \
  --cwd "$HOME/delegation-workspace" \
  --resume-task PREDECESSOR_TASK_ID \
  --json
```

The predecessor must have a validated terminal outcome and released its
conversation reservation. The new task receives its own ID and immutable
request.

## Choose another provider

Run `delegate providers --json` and compare the advertised modes and options
with [the provider guide](providers.md). Every listed profile reports its
caller-selected permission modes and supported native options; unsupported
combinations are rejected before admission. `workspace-write` gives the
provider's native write capability for the selected workspace when the catalog
advertises that mode. Pi currently advertises only read-only because its
built-in write and edit tools do not provide a native workspace boundary;
`read-only` keeps the adapter's read boundary.

For an agent-facing contract, run
`delegate capabilities --provider PROFILE --json`. This is side-effect-free
catalog discovery and reports `status: "unknown"` until dispatch performs the
supervised runtime probe; it does not prove host readiness or authentication.

## Keep state recoverable

Back up the state root if task history matters. Do not move task directories,
replace the bound supervisor configuration, or reuse a task ID by hand. The
state root, provider runtime directories, and workspace are validated against
canonical paths on every launch boundary.
