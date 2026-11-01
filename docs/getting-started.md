# Getting started

1. Follow [installation](installation.md) for the integration and CLI.
2. Try the [agent-first prompt](../README.md#ask-naturally).
3. For direct CLI use, follow the packaged
   [read-only task example](../skills/agent-integration/examples/read-only-task.md)
   or [authorized write example](../skills/agent-integration/examples/workspace-write-task.md).

Read [result handling](../skills/agent-integration/references/result-handling.md)
before observing a response. A returned task ID alone does not prove that a
task record exists.

## Optional Teams

Use [Teams](teams.md) when you want reusable Roles, Members and provider/model
defaults. Ask the agent to register a scoped Team, inspect the saved definition
and revision, then choose an explicit Member for a task. It manages the catalog
through Delegate CLI; registration validates configuration without proving
provider/model availability. Direct delegation works without Team setup.

The guide owns [scope and linked-worktree rules](teams.md#global-and-local-scope),
[optional project defaults](teams.md#optional-project-defaults), and
[latest defaults versus saved tasks](teams.md#latest-defaults-and-saved-task-identity).

## Recurring tasks

After [recurring-task setup](installation.md#recurring-task-setup), ask your agent:

> Review this repository's open pull requests every weekday at 09:00 in
> Europe/Berlin. Use Codex, read-only permissions and a 30-minute budget per
> run. Keep findings local. Return the schedule ID and show me how to inspect
> its runs.

The agent saves the definition; saving it does not start the scheduler.
Run the scheduler separately using [scheduler hosting](cli-reference.md#scheduler-hosting).
The host must stay awake and the service must remain running for due work.
Each occurrence gets its own task ID. Published results remain inspectable after
the agent session ends. Use the [history and execution commands](../skills/agent-integration/references/command-reference.md#schedule-history-and-execution)
to find those results, following [scheduled result handling](../skills/agent-integration/references/result-handling.md#scheduled-execution-results).

For recurring [Team Members](teams.md#scheduled-members-and-upgrades), the
schedule saves the explicit target and caller choices; each new occurrence
resolves latest Member defaults. Follow the
[scheduler upgrade procedure](cli-reference.md#scheduler-upgrades) before using
targeted definitions with a resident service.

Find other topics in the [documentation index](README.md).
