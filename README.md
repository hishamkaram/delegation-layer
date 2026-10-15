# Delegation Layer

<p align="center">
  <a href="#install"><img src="docs/assets/delegation-layer-hero.jpg" alt="One lead AI sends briefs through Delegate to three specialist agents and collects their findings" width="1400"></a>
</p>

Briefs out. Findings back. One place to track the work.

Give your lead AI agent a reliable way to delegate bounded coding work to the
provider that fits the task.

Delegation Layer is a local CLI and Agent Skill for supervising one finite turn
of a coding agent, checking its capabilities at runtime, and returning a
validated result with durable evidence. It works with the provider CLIs you
already use instead of replacing them.

## Why use it?

- **One simple control layer** — your agent can discover providers and dispatch
  work through one provider-neutral interface.
- **Bounded by design** — every task has an explicit workspace, permission mode,
  and time budget.
- **Recoverable results** — a later `collect` call can read a finished task
  without launching the provider again.
- **Clarify while work runs** — answer native questions through Delegate; steer
  an active Codex turn without restarting it. [Interactive tasks](docs/cli-reference.md#interactive-tasks).
- **Resumable turns** — a durable timeout can continue the exact provider
  session through a linked successor task.
- **Runtime compatibility** — the selected provider's executable and advertised
  capabilities are checked before provider execution.
- **Model choice when needed** — native model and effort discovery is available
  without turning discovery into an admission allowlist.
- **Native authentication** — provider login stays with the provider CLI; task
  state does not contain copied credentials.

## How it works

1. Install the plugin so the skill and self-contained CLI are available together.
2. The lead agent discovers the available provider capabilities.
3. Delegation Layer admits one bounded provider turn through the supervisor.
4. The lead agent collects the sealed result and evidence.

## Ask naturally

Try “Ask Codex to review this change”, “Get Antigravity’s opinion”, or
“Have Claude investigate this failure”. The skill routes external collaboration
through `delegate`, preserves a named destination, and collects task evidence.
Ordinary native subagents remain available for work inside the current harness,
even when you name that harness. Ask for a separate session when you want
Delegate to launch another instance of the same CLI.

Skill installation makes this workflow discoverable; it does not force a model
to choose it over another plugin. Selection depends on the harness, model, and
loaded instructions. The plugin and optional installer routing hooks supply a
shared preference at session and subagent startup; they do not intercept tools
or rewrite your persistent instructions.
If an agent bypasses Delegate, that is a selection failure, not proof the
integration worked. For a persistent preference, see
[configure agent routing](docs/agent-routing.md).

## Install

### Recommended for Codex and Claude Code

Install the plugin from this repository. It installs the provider-neutral skill
and provides collaboration context hooks plus a trusted session-start bootstrap that reuses the checksum-verified
Delegation Layer release installer when `delegate` is missing. The bootstrap
installs the CLI and its bundled supervisor; it does not install provider CLIs,
change provider authentication, or change provider configuration.

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

Start a new harness session after installation. Plugin hooks require the
harness trust review before they run. If the bootstrap reports that the
install directory is not on `PATH`, add the reported directory to `PATH` and
start a new session; the skill never falls back to a provider-native CLI.

### Install the Agent Skill only

The standard Agent Skills installer works across popular coding agents. From
the project where you want the skill available, run:

```sh
npx --yes skills add hishamkaram/delegation-layer --skill agent-integration
```

The `--skill agent-integration` selection matters because this repository also
contains maintainer skills. The installer detects supported agents and lets you
choose the project or global scope. Add `--global` to install for all projects,
or `--agent codex` (and similar agent names) to choose a specific harness.

If you want the Delegation Layer guided installer instead, run:

```sh
npx --yes delegation-layer install
```

It provides interactive scope, harness, and routing choices. Global installation
and routing hooks for Claude/Codex are selected by default; choose skill-only
to omit hooks. Codex requires native hook trust before activation. For scripts or a choice without prompts:

```sh
npx --yes delegation-layer install \
  --scope project \
  --harness universal \
  --yes
```

The commands above do not install the CLI, change provider
logins, or run a task. Noninteractive installs remain skill-only unless you add
`--routing default`. See [routing setup and removal](docs/agent-routing.md).
Use the plugin path above when you want the skill and
CLI bootstrap together.

### Install the CLI

Use the prebuilt installer that fits your machine:

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
`delegate-run`, `pueue`, and `pueued` into `$HOME/.local/bin` by default. The
CLI uses the bundled supervisor automatically. Set
`DELEGATION_LAYER_INSTALL_DIR` to use another directory.

Check the installation:

```sh
delegate --help
delegate providers --json
```

## What you need to run a task

Normal users do not need Go or Node after choosing an installation method.
Dispatching a task requires:

- a released Delegation Layer CLI;
- a compatible provider CLI with an available native login;
- a supported provider CLI such as `agy`, `codex`, `claude`, `pi`, or `opencode`;
- Darwin or Linux on amd64 or arm64 for the supplied release builds.

The release contains the supervisor client and daemon and starts a private
instance under the state root, recovering it when a control command finds it
stopped. A reachable compatible daemon is adopted across CLI upgrades without
stopping it or rewriting its launch record. An explicit supervisor repair
command is available for a real logical/configuration migration; it preserves
active work and the legacy journal. An explicit `--pueue-config` or
`DELEGATE_PUEUE_CONFIG` can still select an existing compatible supervisor for
advanced integrations. Node.js 18 or newer is needed only for the npm-based
installers; it is not a runtime requirement for the released Go CLI. Provider
authentication remains native to each provider. Your provider settings, MCP
servers, plugins, and skills stay with that provider. Permission modes select
native behavior; Delegation Layer is not an independent sandbox. Authorized
`workspace-write` tasks run unattended using native approval behavior, including
Pi file edits and shell commands. Native access can extend beyond the workspace.

## Try one bounded task

Create a brief and a workspace outside the private state root:

```sh
mkdir -p "$HOME/delegation-workspace"
printf '%s\n' 'Inspect the repository and summarize the current build status.' > brief.txt

delegate --root "$HOME/delegation-state" dispatch \
  --auto \
  --brief "$PWD/brief.txt" \
  --cwd "$HOME/delegation-workspace" \
  --permission read-only \
  --budget 30m \
  --json
```

The response contains a task ID and separate admission, liveness, and
publication fields. Observe and collect it with:

```sh
delegate --root "$HOME/delegation-state" status TASK_ID --json
delegate --root "$HOME/delegation-state" collect TASK_ID --watch 5s --json
```

Use `delegate providers --json` for the compiled provider catalog and
`delegate capabilities --provider PROFILE --json` for a provider-neutral
capability response. `delegate preflight --provider PROFILE --cwd ABS --json`
checks static admission without creating a task. Managed runtime-only tasks
perform the fresh runtime probe inside their supervised worker before provider
execution; inspection-backed paths retain admission checks. See
[architecture](docs/architecture.md#ownership-boundaries). Catalog discovery
alone does not prove authentication or supervisor readiness.

When an explicit model or effort choice is needed, query the implemented native
discovery command:

```sh
delegate models --provider codex:exec --cwd "$HOME/delegation-workspace" --json
```

`--cwd` defaults to the current directory. Model discovery uses the
state-rooted supervisor and a bounded `model-discovery-v1` five-minute
inspection to allow a provider's first-run model/cache initialization. It
creates inspection evidence without admitting a provider task, and may contact
the provider CLI.
It is advisory: a discovered model is not an admission guarantee or an
allowlist. `complete` describes model enumeration. A null effort list means
unknown metadata; an empty list means the provider explicitly reported no
choices. Per-model effort metadata is independent of the harness-wide list.
Discovery exits `0` for `available` or `partial`, `2` for `blocked` or
`unavailable`, and `1` for `failed`.

For Codex's native read-only review command, select `codex:review` and make
the Git state explicit. Run this from an existing Git checkout outside the
Delegate state root, with the changes you want reviewed:

```sh
(
REVIEW_WORKSPACE="$(git rev-parse --show-toplevel)" || exit 1
REVIEW_BRIEF="$(mktemp)" || exit 1
REVIEW_RESPONSE="$(mktemp)" || exit 1
printf '%s\n' 'Review the uncommitted changes in this Git checkout.' >"$REVIEW_BRIEF"
if delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" dispatch \
  --provider codex:review \
  --review-target uncommitted \
  --brief "$REVIEW_BRIEF" \
  --cwd "$REVIEW_WORKSPACE" \
  --permission read-only \
  --budget 1h \
  --json >"$REVIEW_RESPONSE"; then
  review_exit=0
else
  review_exit=$?
fi
cat "$REVIEW_RESPONSE"
printf '\nDispatch exit: %s; saved response: %s\n' "$review_exit" "$REVIEW_RESPONSE"
)
```

When that response says `task_record: "created"`, observe its exact `task_id`
with the `status` and `collect` commands above, using the same state root.
Otherwise follow the returned failure; do not dispatch again to recover output.

Use `base:BRANCH` or `commit:SHA` for a different review target. Review tasks
are persisted as read-only tasks and do not support continuation or active-turn
interaction. The brief is retained in the task evidence; Codex's explicit
review target supplies the native review scope because the Codex CLI does not
accept a prompt together with these target flags.

If a task reports `status: "timed_out"` and
`continuation.resumable: true`, continue the exact session:

```sh
delegate --root "$HOME/delegation-state" continue \
  --task TASK_ID --brief "$HOME/follow-up.txt" --budget 30m --json
```

The successor receives a new task ID and is observed with the same `status` and
`collect` commands.

## Documentation

- [Getting started](docs/getting-started.md) — install and run a first task.
- [CLI reference](docs/cli-reference.md) — commands, flags, responses, and
  exit codes.
- [Integration API](docs/api.md) — JSON responses and idempotent client
  behavior.
- [Provider guide](docs/providers.md) — profiles and runtime compatibility.
- [Architecture](docs/architecture.md) — lifecycle and ownership boundaries.
- [System components](diagrams/components.html) — interactive Archify view.
- [Task lifecycle](diagrams/lead-interface.html) — interactive lifecycle view.
- [Security](docs/security.md) — isolation, policy, and result integrity.
- [Troubleshooting](docs/troubleshooting.md) — common setup and runtime
  failures.
- [Agent integration skill](skills/agent-integration/SKILL.md) — the
  provider-neutral instructions installed into an agent harness.
- [Contributing](CONTRIBUTING.md) — development and review guidance.

## License

This project is licensed under the [MIT License](LICENSE).

### Consult models through Claude Code Router

Delegate also supports CCR-configured third-party and local models through
`ccr:print`. Ask your agent to “have CCR review this” and it can discover and
choose a configured model alias. CCR and Claude Code must already be installed
and the provider configured. See [provider guidance](docs/providers.md#claude-code-router).
