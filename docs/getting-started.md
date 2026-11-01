# Getting started

1. Follow [installation](installation.md) for the integration and CLI.
2. Try the [agent-first prompt](../README.md#ask-naturally).
3. For direct CLI use, follow the packaged
   [read-only task example](../skills/agent-integration/examples/read-only-task.md)
   or [authorized write example](../skills/agent-integration/examples/workspace-write-task.md).

Read [result handling](../skills/agent-integration/references/result-handling.md)
before observing a response. A returned task ID alone does not prove that a
task record exists.

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

Find other topics in the [documentation index](README.md).
