# Getting started

Delegation Layer gives an agent a supervised way to send one bounded task to a
provider CLI and collect the validated result later.

For Codex or Claude Code, install the plugin. It installs the skill and
bootstraps the self-contained Delegation Layer CLI when needed. Its context
hooks supply the shared external-collaboration preference.

For Codex:

```sh
codex plugin marketplace add hishamkaram/delegation-layer
codex plugin add delegation-layer@delegation-layer
```

For Claude Code:

```sh
claude plugin marketplace add hishamkaram/delegation-layer
claude plugin install delegation-layer@delegation-layer
```

Start a new harness session after installation and trust the plugin hook when
the harness asks. The plugin does not install provider CLIs or change provider
authentication. If it reports a PATH repair, apply the reported PATH change
and start a new session.

In Codex, enabling the plugin alone does not trust its hooks. Review and trust
its hooks in the startup prompt or `/hooks`; otherwise the skill is available
but the shared startup guidance is skipped. A trusted routing hook supplies
the exact installed skill path, so the agent does not need to guess cache paths.

## 1. Install the Agent Skill only

Use the common Agent Skills installer from the project where your agent works:

```sh
npx --yes skills add hishamkaram/delegation-layer --skill agent-integration
```

The `--skill agent-integration` selection avoids installing the repository's
maintainer skills. The installer detects supported agents and lets you choose
the project or global scope. Use `--global` for all projects or `--agent codex`
(and similar agent names) for a specific harness.

The package also includes a guided installer with an interactive scope and
harness picker:

```sh
npx --yes delegation-layer install
```

It selects global installation by default. If it cannot identify a specific
harness, choose the shared `.agents/skills` location. The interactive installer
also offers routing hooks for Claude/Codex by default, or skill-only guidance.
Other hosts currently receive skill-only guidance.

For automation, select the destination explicitly:

```sh
npx --yes delegation-layer install \
  --scope project \
  --harness universal \
  --yes
```

Noninteractive commands install agent guidance only unless `--routing default`
is requested. They do not install the Delegation Layer
CLI, provider CLIs, change authentication, or run a task. Use the plugin path
above when the skill and CLI should be installed together.

For natural collaboration requests and selection limitations, see
[Ask naturally](../README.md#ask-naturally). To make Delegate your persistent
external-collaboration preference, see [agent routing](agent-routing.md).

## 2. Install the CLI

Choose one released CLI installer:

```sh
curl -fsSL https://raw.githubusercontent.com/hishamkaram/delegation-layer/main/install.sh | sh
```

```sh
brew install hishamkaram/tap/delegation-layer
```

```sh
npx --yes delegation-layer install-cli
pnpm dlx delegation-layer install-cli
bunx --bun delegation-layer install-cli
```

The release installer verifies checksums and installs `delegate`,
`delegate-run`, `pueue`, and `pueued` into `$HOME/.local/bin` unless
`DELEGATION_LAYER_INSTALL_DIR` is set. The CLI starts its private supervisor
automatically, recovers it when a control command finds it stopped, and adopts
a reachable compatible supervisor after an installation upgrade.

```sh
delegate --help
delegate providers --json
```

Go is needed only for source builds and contributor work. Node.js 18 or newer
is needed for the npm-based installers, not for the released Go CLI at runtime.

## 3. Prepare the runtime

Dispatching a task requires:

- at least one provider CLI with an available native login, such as `agy`,
  `codex`, `claude`, `pi`, or `opencode`;
- a Darwin or Linux host on amd64 or arm64 for the supplied builds.

Authenticate providers with their own login flows. Delegation Layer keeps
provider credentials in the provider's native environment and does not copy
them into task state.

Keep the state root, workspace, and provider runtime directories separate. The
private supervisor configuration and data live below the state root. The
workspace is the directory the provider may inspect or edit; the state root
contains task records and sealed evidence. An explicit `--pueue-config` or
`DELEGATE_PUEUE_CONFIG` remains available when an existing compatible
supervisor must be used.

If the state root reports a real logical supervisor migration, inspect it and
run the explicit repair command:

```sh
delegate --root "$HOME/delegation-state" supervisor status --json
delegate --root "$HOME/delegation-state" supervisor migrate --json
```

Normal dispatch adopts a reachable compatible endpoint automatically. The
repair command is only for a logical/configuration migration; it preserves the
old inspection journal, refuses to stop queued or running inspection work, and
resumes safely from its migration journal after a crash. Do not start or
configure Pueue yourself.

## 4. Dispatch one task

Create a finite brief and a workspace outside the state root:

```sh
mkdir -p "$HOME/delegation-workspace" "$HOME/delegation-state"
printf '%s\n' 'List the top-level files and summarize the project.' > "$HOME/delegation-brief.txt"

delegate --root "$HOME/delegation-state" \
  dispatch --auto \
  --brief "$HOME/delegation-brief.txt" \
  --cwd "$HOME/delegation-workspace" \
  --permission read-only \
  --budget 30m \
  --json
```

The command records the request and submits exactly one bounded provider turn.
It returns a `task_id`; it does not wait for the provider's final response.

## 5. Observe and collect

Use the task ID to inspect progress and collect the terminal result:

```sh
delegate --root "$HOME/delegation-state" status TASK_ID --json
delegate --root "$HOME/delegation-state" collect TASK_ID --watch 5s --json
```

Collection is observational. It can recover sealed evidence from an existing
task but never launches, retries, or resumes provider work. A rejected outcome
is still a terminal, inspectable result and exits with code 4.

## Optional managed Git workspace

For independent parallel writes, use a retained Git worktree unless the user
explicitly chooses shared coordination. Ordinary non-Git delegation does not
change. From a Git checkout, dispatch with a committed base:

```sh
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" dispatch --auto \
  --brief /absolute/brief.txt --cwd /absolute/git/source \
  --permission workspace-write --isolation git-worktree \
  --base HEAD --workspace-budget 5m --budget 30m --json
```

The source-relative subdirectory is preserved in the managed checkout. Dirty
and untracked source changes are excluded. The checkout remains retained until
the Lead explicitly releases it after reviewing the captured Git-content
evidence; Delegate does not automatically apply, commit, branch, or merge
worker changes.

Use `workspace status ID --watch DURATION --json` to observe a workspace,
`workspace exec ID --budget DURATION -- ARGV...` for a bounded literal-argv
verification command, and `workspace release ID --budget DURATION --json` for
explicit final capture and cleanup. `--workspace ID` attaches a later task to
the retained checkout and cannot be combined with public `--cwd`, `--base`, or
`--isolation`. `continue --task ID` inherits the predecessor's workspace and
session while creating a new task ID. The full retention, capture, forgetting,
and review-scope policy is in the [managed workspace reference](../skills/agent-integration/references/workspaces.md).

## 6. Discover capabilities

List the compiled provider catalog:

```sh
delegate providers --json
```

Inspect one provider's provider-neutral contract:

```sh
delegate capabilities --provider codex:exec --json
```

Capability discovery is side-effect-free and reports the catalog projection. It
does not prove that the executable, supervisor, or authentication is available
on the current host. Dispatch checks static readiness and prepares supervision.
Each managed runtime-only task then checks the current provider before its turn
starts. Collect the task's terminal outcome to prove completion.
The response also reports supported permission modes, request options,
continuation, and the provider-owned read-only mechanism. Model and effort
choices remain in the separate `models` discovery response.
When a provider ID is explicitly required, `preflight --provider ID --cwd ABS
--json` checks static admission without creating a task.

If the task needs an explicit model or effort, use the implemented native
discovery command first:

```sh
delegate models --provider codex:exec --cwd "$HOME/delegation-workspace" --json
```

Omit `--cwd` to use the current directory. Discovery uses the state-rooted
supervisor with a bounded `model-discovery-v1` five-minute inspection to allow
first-run provider model/cache initialization and records
inspection evidence
without admitting a provider task. It is advisory and never an admission
allowlist. A null effort list means unknown metadata, while `[]` means the
provider explicitly reported no choices; per-model choices may still differ.
Defaults are valid when discovery is unnecessary. Its exits are `0` for
`available` or `partial`, `2` for `blocked` or `unavailable`, and `1` for
`failed`.

## Continue a conversation

When a task times out, inspect its `continuation` object. If it says
`resumable: true`, create a linked successor with the exact predecessor:

```sh
delegate --root "$HOME/delegation-state" \
  continue --task PREDECESSOR_TASK_ID \
  --brief "$HOME/follow-up.txt" \
  --budget 30m \
  --json
```

The predecessor must have a committed outcome or durable budget-stop
termination evidence and a released conversation reservation. A budget stop
authorizes session handoff but does not establish task completion. The
continuation receives a new immutable task ID.

See the [provider guide](providers.md) for modes, options, and runtime
compatibility behavior, or the [CLI reference](cli-reference.md) for every
command and exit code.
