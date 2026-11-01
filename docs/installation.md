# Installation

Choose the plugin for integrated startup, or install the skill and CLI
separately. Provider CLIs and their native logins must already be usable.

These repository docs describe current main. The
[latest published release](https://github.com/hishamkaram/delegation-layer/releases/latest)
can lag main; repository instructions do not prove a change is in your installed build.

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
private supervisor automatically.

Verify the installed CLI:

```sh
delegate --help
delegate providers --json
```

The provider catalog describes compiled support. A task still needs the
selected provider's native runtime and login.

## Recurring-task setup

Use the same explicit state root for schedule management, the scheduler and
result inspection. Create the first definition with
[`schedule create`](../skills/agent-integration/references/command-reference.md#schedules);
it prepares absent schedule storage without launching a provider. Existing
compatible definitions and ordinary task state are retained. Incompatible or
unsafe database storage is refused; see [storage and migration](architecture.md#scheduler-storage).

Before hosting the scheduler, make the selected provider CLI, native login and
saved workspace usable from the service's environment. An interactive terminal's
PATH or login context does not establish service readiness. Follow
[scheduler hosting](cli-reference.md#scheduler-hosting) to start, stop and restart
the foreground command through your chosen host. Delegate does not install or
register a scheduler service for you.

## Updates

Update a plugin through its host plugin manager. For a standard Skills install,
use `npx skills update`. For the guided installer, rerun its original command;
`--force` authorizes replacing existing skill files. Update the CLI with the
same installer you chose (`brew upgrade hishamkaram/tap/delegation-layer`
for Homebrew). Restart the harness and review changed hooks as required.

Keep existing task state. For an explicit migration response, follow
[result handling](../skills/agent-integration/references/result-handling.md#task-creation);
general supervisor administration is in the [CLI reference](cli-reference.md#supervisor-recovery).
