# Security and permissions

Delegate supervises tasks in a native single-user context. Provider CLIs own
accounts, configuration, extensions, and native permissions. Delegate owns task
lifecycle, its installation and supervisor, and validated result evidence.
For remote tasks, those execution owners run on the destination.

## Native permissions

`read-only` and `workspace-write` select provider-native behavior.
Authorized workspace-write requests unattended commands and edits; native
access can extend beyond the selected workspace. The
[provider guide](providers.md) describes the mechanisms for each profile.

Providers load their own settings, MCP servers, plugins, and skills. Delegate
uses the native controls needed for the requested mode. It reports incompatible
commands, authentication failures, and invalid results explicitly.
A startup compatibility check does not prove successful completion.

## State and credentials

Keep the state root private and outside provider-writable workspaces and
provider runtime directories. Task records and evidence contain task content,
including questions and replies. Delegate uses native provider authentication;
credentials stay in the provider's own environment.

The plugin's trusted startup hook can install Delegate's verified release
bundle. Review it through the host's trust mechanism; see
[installation](installation.md#plugin-for-codex-or-claude-code).

## Terminal workbench

The [TUI history cache](terminal-workbench.md#cache-ownership-and-privacy) stores
metadata and dated status codes outside the execution root, with private files.
It excludes prompts, results, logs, credentials and environment contents. Explicit
result/log viewers still display private task content; terminal text is sanitized
and never interpreted as a shell command. Cached observations grant no mutation
authority. Device trust, live remote operations and destructive controls require
explicit actions. Quitting the UI does not cancel admitted tasks.

## Process and result assurance

An update between preflight and launch can change the provider CLI that runs
or cause a launch error; Delegate reports that error without automatically
retrying the launch.

Task identity, bounded capture, native closure evidence, and result validation
have separate owners. A queue row ending is insufficient to prove termination.
Cooperative process scopes can lose the ability to prove emptiness after their
owner is lost; Delegate reports uncertainty instead of publishing false closure.
See [architecture](architecture.md#ownership-boundaries).

A committed outcome proves the recorded provider/evidence checks passed.
The lead must still evaluate whether the answer fulfills the brief.
Read validated descriptors using the
[result contract](../skills/agent-integration/references/result-handling.md#task-control-response);
raw logs are task evidence, not a substitute accepted answer.

## Remote trust and data

Remote execution uses an embedded SSH transport and a bounded supported SSH
configuration subset. Unknown host keys require explicit verified enrollment;
changed or revoked keys are refused. Delegate does not forward an SSH agent or
synchronize provider credentials. See [SSH support](remote-delegation.md#ssh-support).

The destination task and outcome remain authoritative. Origin connectivity,
cached observations and downloaded files do not prove execution stopped or
authorize another launch. Input snapshots and output transfers include only
explicitly selected files; retained bytes have no automatic garbage collection.
See [file selection](remote-delegation.md#dispatch-and-files) and
[retention](remote-delegation.md#retention) before selecting sensitive data.

## Workspace and interaction limits

Managed Git worktrees separate cooperative writes while sharing repository
metadata and other global resources. Their capture is Git-content evidence,
not a full filesystem backup or security sandbox. See
[workspace limits](../skills/agent-integration/references/workspaces.md).

Replies answer specific native questions; steering supplies guidance within
the existing task. Neither changes permission, model, workspace, or deadline.
An uncertain delivery is never automatically repeated. The
[interaction contract](../skills/agent-integration/references/interaction.md)
owns delivery and size limits.
