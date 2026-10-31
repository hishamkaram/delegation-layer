# Authorized workspace write task

Use the captured invocation in the [read-only example](read-only-task.md).
Replace its brief with the authorized change and checks, and select
`--permission workspace-write`. For independent parallel writes, also follow
[managed workspaces](../references/workspaces.md) and its
[worked example](managed-workspace.md).

Workspace-write requests unattended native commands and edits. Native access
can extend beyond the workspace; the provider owns tools and restrictions.
Use it only when the caller authorized edits.

Apply [task creation](../references/result-handling.md#task-creation) before
observing any returned ID, then collect the created task in the foreground.
A denial does not authorize a direct provider call or changed permissions.
