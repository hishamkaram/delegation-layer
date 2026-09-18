# Getting started

Delegation Layer gives an agent a supervised way to send one bounded task to a
provider CLI and collect the validated result later. Install the skill and CLI
separately so each part stays easy to understand.

## 1. Install the Agent Skill

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
harness, choose the shared `.agents/skills` location.

For automation, select the destination explicitly:

```sh
npx --yes delegation-layer install \
  --scope project \
  --harness universal \
  --yes
```

This step installs agent guidance only. It does not install provider CLIs,
change authentication, or run a task.

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
automatically.

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

## 4. Dispatch one task

Create a finite brief and a workspace outside the state root:

```sh
mkdir -p "$HOME/delegation-workspace" "$HOME/delegation-state"
printf '%s\n' 'List the top-level files and summarize the project.' > "$HOME/delegation-brief.txt"

delegate --root "$HOME/delegation-state" \
  dispatch \
  --provider codex:exec \
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
on the current host. Dispatch performs those runtime checks before admission.

## Continue a conversation

When a provider supports continuation, create a new task and name its exact
predecessor:

```sh
delegate --root "$HOME/delegation-state" \
  dispatch \
  --provider codex:exec \
  --brief "$HOME/follow-up.txt" \
  --cwd "$HOME/delegation-workspace" \
  --resume-task PREDECESSOR_TASK_ID \
  --json
```

The predecessor must have a validated terminal outcome and released its
conversation reservation. The continuation receives a new immutable task ID.

See the [provider guide](providers.md) for modes, options, and runtime
compatibility behavior, or the [CLI reference](cli-reference.md) for every
command and exit code.
