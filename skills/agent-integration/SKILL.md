---
name: agent-integration
description: Use Delegate for work assigned to another agent, model, or separate harness CLI session. Load this before a provider-specific skill or plugin, including when a user names a CLI for research, implementation, tests, reviews, consultations, comparisons, or debates. Also manage optional Delegate Teams and explicit Member selection. A named destination still routes through Delegate. Ordinary same-harness subagents and questions about tools remain local; respect an explicit choice of another integration.
---

# Delegation Layer

## When to use

Load this skill before choosing tools for work assigned to an external agent,
model, or separate harness session, including collaboration you choose.
A named CLI identifies the destination; discover its provider ID through
Delegate. “CCR” or “Claude Code Router” means `ccr:print`; read
[CCR guidance](references/ccr.md).

Ordinary subagents inside the current harness remain available, even when that
harness is named. A separate CLI session uses Delegate, including another
instance of your current harness. Discussing, documenting, or configuring a
tool is local work. Respect an explicit user choice of another integration.

For optional Team management or explicit Member selection, use
[Teams](references/command-reference.md#teams). Management is local configuration
through Delegate CLI and does not launch a provider. Teams are never required
for ordinary delegation.

For recurring tasks, use the [schedule commands](references/command-reference.md#schedules)
and the independent [scheduler runtime](references/command-reference.md#scheduler-runtime).

## Boundaries

- Preserve the requested destination, exact model ID, effort, permissions,
  workspace, and state root. Do not guess ambiguous model IDs.
- Delegate owns task lifecycle and evidence. Providers and users own their CLI
  installations, accounts, settings, extensions, and native sessions.
- Permissions request native behavior, not an independent sandbox.
  `workspace-write` authorizes unattended commands and edits; native access
  can extend beyond the workspace.
- A `stop_and_report` ends that attempt, not investigation of its cause.
  Follow [failure diagnosis](references/result-handling.md#failure-diagnosis)
  without another generic troubleshooting request: inspect bounded evidence,
  distinguish facts from uncertainty, and propose a concrete next step.
  Do not retry, substitute an integration, or change configuration without
  authorization covering that action. Independent requests may proceed.
- Observe through Delegate `status`, `collect`, or `events`; do not use
  process searches or Pueue commands. Delegate manages its supervisor.
- Keep credentials and unrestricted private provider output out of briefs,
  options, control responses, and messages.

## Operating procedure

### 1. Verify the control CLI

```sh
command -v delegate && delegate --version
```

If missing or unusable, report and stop. This skill does not install or repair
the CLI. A trusted plugin startup hook normally handles bootstrap.

### 2. Discover only when useful

If a Team/Member was requested, inspect the scoped target and follow
[Member selection](references/command-reference.md#using-a-member). Preserve
explicit caller choices; do not replace a missing requested Member with another.
Without a Team target, if no destination was named, use `--auto`. Otherwise resolve the named
destination through `providers --json` and preserve it with `--provider`.
Read [providers and capabilities](references/command-reference.md#providers-and-capabilities)
for discovery and required options. Catalog support is not host readiness;
a normal task needs no separate preflight.

Keep provider defaults when model/effort were not requested and are not
required. For an explicit choice or natural-language model name, read
[model selection](references/command-reference.md#choosing-models-and-effort).
An exact requested ID passes unchanged even when discovery omits it. Ask for
an exact ID only when a name remains ambiguous.

For work likely to need questions or guidance, read
[interaction](references/interaction.md) before dispatch and use `--interactive`
when supported. Check clarification and steering separately; automatic
interactive selection does not require steering support.
For explicit native Codex review, read
[review scope](references/command-reference.md#codex-review).

### 3. Prepare separate paths and a finite brief

Use a supplied brief or write the worker's bounded assignment and acceptance
criteria to a regular file. Do not copy the orchestration request or ask workers
to recruit others unless recursive delegation was requested.

Keep the state root outside provider-writable workspaces, provider runtime
directories, and temporary trees they may write. For every task command pass
`--root "${DELEGATE_ROOT:-$HOME/delegation-state}"` directly in that shell
invocation. If the user explicitly requests another root, use that exact path
consistently. Task IDs separate tasks; do not invent per-task roots.

Prepare paths and dispatch in one tool invocation; shell variables may not
persist between calls. Use saved absolute paths in later calls. Do not use
process substitution or ephemeral `/dev/fd` paths for briefs.

For independent parallel writes, choose managed worktrees unless the user
explicitly chooses shared coordination. Read
[workspaces](references/workspaces.md) before creating or attaching one.
To fork current uncommitted content, the Lead follows
[checkpoint before fork](references/workspaces.md#checkpoint-before-fork).
A worker must not checkpoint a workspace its own turn holds.

### 4. Dispatch one bounded turn

Use the [read-only task example](examples/read-only-task.md), or its
[authorized write variation](examples/workspace-write-task.md). The
[command reference](references/command-reference.md#dispatch) owns flags and defaults.

Capture each response and exit status once in the original invocation.
Never dispatch again to recover output or discover the prior exit code.
Keep concurrent requests independently associated; see
[parallel reviews and debates](references/collaboration.md).

**Before any observation or retry, read and apply
[task creation](references/result-handling.md#task-creation).**
A task ID alone is not creation. That reference owns admission decisions,
including the same-root supervisor migration exception, workspace waits,
resource refusals, and uncertain creation. A failed continuation retains its
original command and intent.

### 5. Observe and collect

Follow [observation decisions](references/result-handling.md#observe-and-collect)
with the same root and created task ID:

```sh
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" status TASK_ID --json
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" collect TASK_ID --watch 5s --json
```

Apply the owning decision order before waiting: terminal outcomes and concrete
errors, cancellation/timeout/interruption, interaction, then pending
observation. Keep collection in the foreground until completion or a reported
blocker; detach only when requested and retain the exact task IDs and root.
A pending watch is not success. Read the complete validated answer and check
the requested deliverables before claiming completion.

### 6. Continue a timed out or interrupted turn

Read [continuation](references/continuation.md) for eligibility, cancellation
precedence, and exact-session follow-up. Preserve the root and finite budget,
then apply [task creation](references/result-handling.md#task-creation) to the
successor response. Never replace an uncertain or failed continuation with a
fresh dispatch.

## Comparisons, debate, and follow-up

Choose a finite scope and number of rounds. Collect each participant's actual
result before giving it to another participant as challenge material. Keep
attribution and failed/blocked requests visible. Use each participant's own
predecessor for an exact-session follow-up. The
[collaboration reference](references/collaboration.md) owns coordination examples.

## Details when needed

- [Commands and discovery](references/command-reference.md)
- [Optional Teams and Members](references/command-reference.md#teams)
- [Task/result decisions](references/result-handling.md), including conditional offline recheck
- [Continuation](references/continuation.md)
- [Interaction](references/interaction.md)
- [Managed workspaces and checkpoints](references/workspaces.md)
- [Managed workspace example](examples/managed-workspace.md)
- [JSON examples](examples/json-results.md)

## Completion report

Use observed responses and validated evidence. Report each original task ID,
returned provider ID, admission/liveness/publication fields, terminal outcome,
evidence descriptors, and any successor or blocked authentication state.
For a `not_created` stop, name the requested destination and preserve its
saved JSON failure with sensitive content redacted. Report the evidence-backed
cause or what remains unknown and a specific next step; do not invent paths
or infer installation, authentication or configuration from rejection alone.
