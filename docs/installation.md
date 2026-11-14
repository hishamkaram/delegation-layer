# Installation

Use guided setup for a standalone installation, or the plugin for integrated
startup. Provider CLIs and their native logins remain yours to install and manage.

These repository docs describe current main. The
[latest published release](https://github.com/hishamkaram/delegation-layer/releases/latest)
can lag main; repository instructions do not prove a change is in your installed build.

## Recommended standalone setup

Requires Node.js 18 or later on Linux or macOS:

```sh
npx --yes delegation-layer setup
```

Choose global scope for use across projects. Interactive defaults are global
scope and routing hooks for Claude/Codex. For an explicit noninteractive setup:

```sh
npx --yes delegation-layer setup --scope global --harness claude,codex --routing default --yes
```

`--yes` accepts defaults; noninteractive routing defaults to `skill-only` unless
`--routing default` is explicit. `--force` replaces skill content only. Neither
option overwrites an existing CLI or grants native hook trust. The plugin is
not required for setup.

Setup validates skill/routing targets before installation, selects an installed
Delegate, installs an absent bundle at the exact npm package version, runs
[passive doctor observations](../skills/agent-integration/references/command-reference.md#doctor),
then refreshes and writes skill/routing state through the existing installers.
The matching CLI release must exist; setup never falls back to latest. Current
main support does not prove that the published npm package includes setup or
that its matching CLI release is available.

CLI selection uses `--install-dir ABS`, then `DELEGATION_LAYER_INSTALL_DIR`, then
`delegate` on PATH, then `$HOME/.local/bin/delegate`. An explicit directory wins
even when PATH selects another installation. Follow the printed PATH correction
and use the printed absolute command meanwhile. Broken selected installations,
partial bundles, or an unsupported/malformed doctor response stop setup; repair
or upgrade using the original installation method, or select an empty directory.
Setup does not replace existing CLI files.

Optional `--provider ID --cwd ABS` adds static provider checks. `--model`,
`--effort` and `--review-target` require that pair. Harness selection does not
select a provider. Provider authentication remains unknown; setup does not log
in, repair accounts or execute a provider turn.

Doctor runs in the foreground until it closes; setup has no hard wall-clock
termination deadline. Setup can install CLI, skill and routing files; doctor
itself writes no state and launches no provider, worker or supervisor. A stopped
setup reports completed and remaining steps without global rollback. Rerun to
reuse matching state or inspect reported conflicts. Cancellation exits 0 without
completion. Invalid arguments exit 2. An empty doctor response with exit 2 also
returns setup exit 2: check provider options, or upgrade through the original
installer if doctor is unsupported. Malformed/incompatible reports and valid
blocked doctor reports return setup exit 1. No skill/routing writes start in
these cases; any already completed CLI installation is reported and retained.
Other blocked/failed setup exits 1; completed setup exits 0 with manual actions
still explicit.

Restart or resume the harness and review native hook trust where applicable;
activation remains unknown. With provider options, setup prints a read-only
first-task recipe using the selected absolute CLI and workspace, including brief
preparation, dispatch, status, collect, cancellation and evidence checks. Otherwise
it prints a lead-agent prompt requiring provider/workspace selection. Review and
execute it yourself. See [getting started](getting-started.md).

## Plugin for Codex or Claude Code

The plugin supplies the skill, routing context, and a trusted startup hook
that installs the Delegate release bundle when the CLI is
missing.

Codex:

```sh
codex plugin marketplace add hishamkaram/delegation-layer
codex plugin add delegation-layer@delegation-layer
```

Claude Code:

```sh
claude plugin marketplace add hishamkaram/delegation-layer
claude plugin install delegation-layer@delegation-layer
```

Start a new harness session. Review and trust the hooks when prompted.
In Codex, use the startup review or `/hooks`; enabling a plugin alone does
not trust its hooks. If startup reports a directory missing from `PATH`,
add the reported directory and start a new session.

The bootstrap installs Delegate and its bundled supervisor. Provider setup
remains native; use the [provider guide](providers.md) for setup links.

## Agent Skill only

From the project where the skill should be available:

```sh
npx --yes skills add hishamkaram/delegation-layer --skill agent-integration
```

Select only `agent-integration`; this repository also has maintainer skills.
Add `--global` for all projects or `--agent codex` to select a harness.
The current upstream [Skills CLI](https://github.com/vercel-labs/skills#readme)
requires Node.js 22.20 or later.

The Delegate package also offers a guided installer (Node.js 18 or later):

```sh
npx --yes delegation-layer install
```

Its interactive defaults are global scope and routing hooks for Claude/Codex.
Choose skill-only to omit hooks. For explicit noninteractive installation:

```sh
npx --yes delegation-layer install --scope project --harness universal --yes
```

Noninteractive installation remains skill-only unless `--routing default`
is supplied. For installer-managed global Claude/Codex hooks:

```sh
npx --yes delegation-layer install --scope global --harness claude,codex --routing default --yes
```

Skill installation installs guidance. Install the CLI below separately.
See [routing activation, status, and removal](agent-routing.md).

## CLI installation

Supplied release builds support Darwin and Linux on amd64 and arm64.
Choose one method:

```sh
curl -fsSL https://raw.githubusercontent.com/hishamkaram/delegation-layer/main/install.sh | sh
```

```sh
brew install hishamkaram/tap/delegation-layer
```

```sh
npx --yes delegation-layer install-cli
```

The npm installer also runs through `pnpm dlx delegation-layer install-cli`
or `bunx --bun delegation-layer install-cli`. npm-based installation needs
Node.js 18 or later; Homebrew needs Homebrew, and the shell installer needs
its standard download/archive utilities. Go is only needed for source builds.

The release installer installs the Delegate CLI and bundled supervisor into
`$HOME/.local/bin` by default. Set `DELEGATION_LAYER_INSTALL_DIR` for a
different destination.
Keep that directory on the harness process's `PATH`. Delegate starts its
private supervisor automatically. If interrupted by HUP, INT or TERM, the shell
installer exits with status 129, 130 or 143 and cleans its own temporary and
staging directories. Interruption does not roll back files already installed.

Verify the installed CLI:

```sh
delegate --help
delegate providers --json
```

The provider catalog describes compiled support. A task still needs the
selected provider's native runtime and login.

For a source build of current main, follow
[Development setup](contributing.md#development-setup). It owns the compiler,
repository tools, colocated binaries and supervisor prerequisites.

## Remote-task setup

Install Delegate and the selected provider on the destination too. The origin
plugin does not prepare that machine, copy provider accounts or configure host
services. Follow [destination preparation and SSH trust](remote-delegation.md#prepare-a-destination)
before dispatch; local installation alone does not establish remote readiness.

## Recurring-task setup

Follow the [scheduler walkthrough](getting-started.md#recurring-tasks) to start
`delegate scheduler serve` yourself in an independent user terminal or tmux
pane, save a definition and inspect a real result. Ordinary agent and manual
commands omit `--root` and share the CLI's OS-default store across project
directories. Explicit user-started `serve` initializes absent base and scheduler
storage and can become ready before the first definition; `schedule create`
can also initialize absent storage. Compatibility `scheduler run` requires
existing storage. For intentional custom or historical roots, preserve the
[exact explicit root](cli-reference.md#advanced-explicit-and-historical-roots).
Existing compatible definitions and ordinary task state are retained;
incompatible or unsafe storage is refused. See [storage rules](architecture.md#scheduler-storage).

Check installed help: prefer `scheduler serve` when available, or use the
walkthrough's foreground `scheduler run` path for older builds. Use colocated
`delegate`/`delegate-run` from the same build, without `--runner`, in the intended
native user context with the selected provider CLI, login and saved workspace
available. This scheduler workflow does not install or register an OS service. Advanced
[legacy OS hosting](cli-reference.md#optional-legacy-os-hosting) remains optional.

## Gateway connection

To make this device available through a trusted self-hosted Gateway, follow
[Gateway connection](gateway-connect.md). `delegate gateway connect` enrolls the
device and installs/starts its per-user connector service by default;
`--foreground` instead runs in the current terminal. This explicit Gateway
feature does not install a scheduler service or change ordinary SSH/TUI use.
Approve existing workspace roots separately before remote task admission.

## Updates

Update a plugin through its host plugin manager. For a standard Skills install,
use `npx skills update`. For the guided installer, rerun its original command;
`--force` authorizes replacing existing skill files. Before any CLI replacement,
follow the [scheduler stop-update-restart procedure](cli-reference.md#scheduler-upgrades),
then update with the same installer you chose
(`brew upgrade hishamkaram/tap/delegation-layer` for Homebrew).
Restart the harness and review changed hooks as required.

Keep existing task state. Removing an old Homebrew keg must not require retaining
its executables: the current installation can refresh a compatible live private
supervisor through `delegate supervisor ensure-owner --json` when status requests
it. Follow [supervisor recovery](cli-reference.md#supervisor-recovery); keeping an
old keg or downgrading a migrated scheduler database is not the recovery path.
For an explicit migration response, follow
[result handling](../skills/agent-integration/references/result-handling.md#task-creation);
general supervisor administration is in the [CLI reference](cli-reference.md#supervisor-recovery).
