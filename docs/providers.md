# Provider guide

Choose a profile for its native behavior and supported options.
Install and authenticate each provider through its own setup guide.

| Profile | Permission mode | Read-only mechanism | Request options | Native command |
| --- | --- | --- | --- | --- |
| `antigravity:print` | `read-only`, `workspace-write` | `native-plan` | `continuation`, `model`, `effort`, `native-timeout` | `agy` |
| `codex:exec` | `read-only`, `workspace-write` | `native-sandbox` | `continuation`, `model`, `effort`, `interactive` | `codex exec` |
| `codex:review` | `read-only` | `native-sandbox` | `review-target`, `model`, `effort` | `codex exec review` |
| `ccr:print` | `read-only`, `workspace-write` | `native-plan` | `continuation`, `model` (required alias), `effort` | CCR foreground launch |
| `claude:print` | `read-only`, `workspace-write` | `native-plan` | `continuation`, `model`, `effort`, `interactive` | `claude` print mode |
| `pi:json` | `read-only`, `workspace-write` | `tool-allowlist` plus disabled extensions for read-only | `continuation`, `model`, `effort` | `pi --mode json` |
| `opencode:run` | `read-only`, `workspace-write` | `native-plan` | `continuation`, `model`, `effort` | `opencode acp` |

For catalog/capability fields and readiness limits, use
[discovery](../skills/agent-integration/references/command-reference.md#providers-and-capabilities).
For model IDs, effort metadata, and discovery exits, use
[model selection](../skills/agent-integration/references/command-reference.md#choosing-models-and-effort).
A catalog entry is not proof of installation, authentication, or completion.

## Shared compatibility behavior

Delegate runs the installed provider under its supervision and checks required
native behavior. Providers and users own installations, updates, accounts,
settings, extensions, and native sessions. A reported release number is an
optional diagnostic.

Permissions select native controls. Workspace-write requests unattended
commands and edits, and native access may extend beyond the workspace.
The [security guide](security.md) describes assurance limits.
Previously admitted tasks keep their recorded contracts; a new continuation
uses the calling environment and must preserve the exact session.
Use the [continuation guide](../skills/agent-integration/references/continuation.md)
for eligibility and recovery.

## Antigravity

Setup: [official CLI guide](https://antigravity.google/docs/getting-started?tab=cli).

Read-only selects native `--sandbox --mode plan` without a bypass.
Workspace-write uses accept-edits with native permission bypass.
The native print timeout fits inside the task budget. Trust, account settings,
and MCP configuration remain native.

## Codex

Setup: [official quickstart](https://developers.openai.com/codex/quickstart).

`codex:exec` uses the requested native sandbox and noninteractive approval
policy. Ordinary tasks support non-Git directories. With `--interactive`,
Codex uses its app-server and supports clarification and steering.

`codex:review` is native read-only Git review with an explicit target.
See [review scope](../skills/agent-integration/references/command-reference.md#codex-review).
A linked-worktree sandbox can deny Git staging/committing when metadata lives
outside the checkout; managed capture can still record file changes. The Lead
can explicitly authorize Git through `workspace exec`.

## Claude

Setup: [official quickstart](https://code.claude.com/docs/en/quickstart).

Read-only selects native plan mode; workspace-write selects
`bypassPermissions`. Claude loads its own settings, tools, extensions,
MCP servers, and login. Interactive tasks support root clarification questions,
but not active-turn steering.

Validated root follow-up answers are published with the caller's answer in
native order. Read the complete payload, including every attributed section;
a progress update is not completion. Native background-agent follow-ups are
part of that invocation, separate from recurring background-task scheduling.
Earlier tasks retain their recorded interpretation.

## Pi

Setup: [official coding-agent guide](https://github.com/badlogic/pi-mono/tree/main/packages/coding-agent).

Read-only selects native `read,grep,find,ls` tools, disables extensions, and
uses a session directory outside the workspace and Delegate state.
Malformed or overlapping session configuration refuses preparation.
Workspace-write selects built-in `read,bash,edit,write` tools.
Pi owns extensions, authentication, and permissions; no independent filesystem
sandbox is added. Continuation requires the exact recorded session and workspace.

## OpenCode

Setup: [official docs](https://opencode.ai/docs/).

The `opencode:run` profile launches native `opencode acp` over stdio.
Read-only selects plan mode and rejects native permission requests;
workspace-write selects build mode and answers with an offered `allow_once`.
Model and effort must be acknowledged before the prompt. OpenCode owns its
configuration, plugins, MCP servers, and authentication.

Continuation uses `session/resume` for the exact session, never a fresh
session fallback. Root answer selection excludes child-session output.
OpenCode does not support `--interactive`.

## Claude Code Router

Setup: [CCR project](https://github.com/hishamkaram/claude-code-router).
Both CCR and Claude Code must be installed with a configured provider route.

`ccr:print` uses CCR's foreground provider-only route with a required model
alias. Read [CCR model selection](../skills/agent-integration/references/ccr.md).
Read-only uses plan mode; workspace-write uses `bypassPermissions`.
This adapter uses foreground work, not CCR detached jobs.

A route-preserving continuation retains its recorded route and refuses an
override that changes it. Historical modeled sessions resolve their alias
through the caller's current configuration. If the installed CLI no longer
supports the recorded route contract, report the returned refusal.
CCR's foreground exit and validated Claude stream must both indicate success.

## Live acceptance

Contributor-only commands and the meaning of pass/blocked/failure are owned by
[contributing](contributing.md#native-and-cli-scenarios).

## Adding a provider

Use the [adapter contributor guidance](contributing.md#provider-changes).
