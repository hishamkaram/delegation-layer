# Teams

Teams are optional, reusable task-selection configuration. A Team describes its
purpose, Roles and Members; a Member may supply provider, model and effort
defaults. The lead chooses an explicit scoped Team and Member for each request.
Role descriptions provide context, not permissions or automatic routing rules.
Ordinary delegation needs no Team setup or catalog access.

For example, ask your agent:

> Register a project Team named Review team with ID review-team. Give it a
> reviewer Role and a primary Member using Codex with its native defaults.
> Show the saved definition and revision. Then ask that Member to review the
> current changes, read-only, with a 30-minute budget.

The agent uses Delegate CLI for both management and execution. The CLI stores
definitions in SQLite; a JSON definition is input transport, not an editable
catalog. [Command syntax and responses](../skills/agent-integration/references/command-reference.md#teams)
are shared by installed agents and manual users.

## Register and choose a Member

Set `PROJECT_CWD` to an absolute existing repository directory and `BRIEF` to
an absolute regular brief file. This example saves a project Team, inspects it,
then requests one task:

```sh
delegate teams register --scope project --cwd "$PROJECT_CWD" \
  --definition - --json <<'JSON'
{
  "schema_version": 1,
  "id": "review-team",
  "name": "Review team",
  "purpose": "Review changes and report confirmed problems.",
  "roles": [
    {"id": "reviewer", "name": "Reviewer", "responsibility": "Check correctness and regressions."}
  ],
  "members": [
    {"id": "primary", "name": "Primary reviewer", "role_ids": ["reviewer"],
     "execution_defaults": {"provider": "codex:exec"}}
  ]
}
JSON
delegate teams show --scope project --cwd "$PROJECT_CWD" --team review-team --json
delegate dispatch \
  --cwd "$PROJECT_CWD" --brief "$BRIEF" \
  --team-scope project --team review-team --member primary \
  --permission read-only --budget 30m --json
```

Capture the dispatch response and exit status separately and apply
[task creation decisions](../skills/agent-integration/references/result-handling.md#task-creation)
before observing or retrying it. Saving a Team does not launch a provider.
Ordinary execution omits `--root` and shares the CLI's default state across
projects. `--cwd` selects the workspace and Local Team context. Preserve an
intentional or recorded nondefault root explicitly; see
[state-root selection](cli-reference.md#advanced-explicit-and-historical-roots).

Definitions use schema version 1 and strict decoding. IDs contain 1–64 lowercase
ASCII letters/digits with internal single hyphens. Team, Role and Member IDs
are stable identifiers; names are separate descriptive text. Role IDs and
Member IDs are unique within their respective arrays. Every Member references
one or more unique declared Roles; Members can share Roles or have several.
Explicit empty `roles` and `members` arrays are valid for a Team with no Members.

`execution_defaults` is optional. If present, it requires `provider` and permits
exact `model` and `effort` selectors. It cannot grant permissions, set budgets
or workspaces, carry credentials, or inject shell arguments. Those task choices
remain explicit request options. Unknown fields and unsupported schema versions
are rejected. The [definition contract](../internal/teams/contract/definition.go)
owns validation and encoding.

## Global and Local scope

| CLI scope | Project context | Availability |
| --- | --- | --- |
| `user` (Global) | None for management | The current user's catalog, across projects. |
| `project` (Local) | Explicit `--cwd ABS` | That local project in the current user's catalog. |

Scopes are explicit; a same-named Local Team never shadows a Global Team.
`teams list` without `--cwd` lists Global Teams only. With `--cwd`, it includes
Global and current-project Teams. If project resolution fails, a safe partial
listing can retain Global Teams and report a diagnostic.

For Git projects, the canonical common Git metadata directory identifies the
project. Nested directories and linked worktrees share Local Teams and the
project default, including dirty repositories and repositories with no commits.
Independent clones remain separate even when their remote URLs match. Symlink
aliases resolve to the same native paths. Moving a repository changes this
path-based identity; old definitions remain stored and the new location needs
explicit registration. There is no automatic reassociation by remote.

Outside Git, Local management and targeting require both an explicit
`--project-root ABS` and an explicit `--cwd ABS` contained in that root. Delegate
does not guess a project by scanning ancestor markers. `--project-root` is
rejected in a Git context. An unavailable or failing Git executable is an error,
not evidence that the directory is outside Git. Global management and ordinary
no-Team tasks do not require Git.

For managed workspaces, a Local target belongs to the canonical source project,
including the recorded source of an attached retained workspace. Creating a
linked worktree does not choose a new project. Invalid or missing targets are
rejected before provisioning; the prepared workspace must still match the
admitted project before launch.

## Storage and safe edits

The CLI owns one SQLite catalog at
`<user configuration directory>/delegation-layer-catalog/teams.sqlite`, resolved
from the command's captured user configuration context. It is separate from
execution state: changing `--root` does not select another Team catalog.
Team-targeted execution rejects catalog/execution-root overlap.

Manage definitions through `teams register`, `update`, and `delete`. Definition
files or stdin are transport only; editing an input file changes nothing until
an explicit mutation commits. Do not edit the database directly.

Registration conflicts with an already active identity. Updates replace the
complete definition and require its ID to match `--team`. Both update and
delete require the positive revision returned by `teams show` as
`--expected-revision`. This compare-and-swap check prevents overwriting a
concurrent edit. A conflict requires reading the current definition and revision
before deciding on a new edit; editing another Team does not conflict.

Deletion retains an identity/revision tombstone while removing executable
availability. Re-registering that same scoped ID advances its revision rather
than resetting it, so an old revision cannot edit the replacement. Deletion
does not change existing task evidence or saved schedule definitions. There is
no historical-definition browser.

`teams list`, `show`, and `default show` are passive reads: they do not create
directories or a database, migrate or repair it, change preferences, inspect
models/readiness, authenticate, or start execution infrastructure. A confirmed
missing catalog lists as empty. A present unreadable, corrupt or unsupported
catalog is an error. A hot rollback journal requires writer recovery; passive
inspection does not repair it. Safe diagnostics can accompany unrelated usable
Teams without turning database-wide corruption into an empty catalog.

Only explicit mutations initialize catalog storage. If commit, close or response
output reports uncertainty, inspect the same Team or project default before
deciding what to do. Do not automatically repeat a mutation whose durable result
is unknown.

## Validation and model availability

Register/update validate the definition and known static provider capabilities.
Their mutation response records `definition: "validated"`,
`provider_capabilities: "validated"`, and `readiness: "not_checked"` under
`mutation.validation`. A configured provider/model is not proof of installation,
authentication, account access or successful execution.

Use [model discovery](../skills/agent-integration/references/command-reference.md#choosing-models-and-effort)
when resolving a natural-language model name, or
[preflight](../skills/agent-integration/references/command-reference.md#checking-readiness)
when readiness was requested. These are separate observations. Optional
discovery being blocked does not prevent saving an otherwise valid supported
definition. Preserve exact explicit model IDs even when discovery omits them;
never guess an alias or install/authenticate a provider to complete registration.
Normal task admission and provider execution determine actual usability.

Explicit caller provider/model/effort choices win over Member defaults. Keeping
the same provider permits omitted model/effort to inherit. Changing provider
discards the old provider's inherited model/effort; explicitly supplied
replacement values survive. A Member without defaults needs an explicit
`--provider` or `--auto`. A provider-bound Member cannot be combined with
`--auto`; explicit provider and auto remain mutually exclusive.

## Optional project defaults

A project default is only a scoped Team reference. It can refer to a Global
Team or a Team in the current Local project; it copies no Member settings.
Only `teams default set` or `clear` writes this preference. Show, selection,
dispatch and continuation never assign it.

`teams default show` reports `unset`, `resolved` or `stale`. Deleting its Team
leaves a diagnosable stale reference. Set another reference or clear it
explicitly; clearing is idempotent. A missing or stale default does not block
ordinary direct delegation. The lead still chooses explicit Team/Member flags;
there is no implicit assignment, Role-only target or automatic Member fan-out.

## Latest defaults and saved task identity

| Request | Selection/settings used |
| --- | --- |
| New direct task ID | Latest committed Member defaults, merged once with explicit caller choices. |
| Separately authorized new attempt after failure | A new task ID and latest defaults; no automatic retry/backoff is added. |
| Replay/recovery of an existing task ID | Its immutable saved selection and execution settings; no catalog lookup or second execution. |
| Exact-session continuation | Recorded Team context and existing continuation rules, including supported explicit model/effort/budget overrides; no retargeting from current configuration. |
| New scheduled occurrence | Latest defaults with the schedule's saved raw choices and omissions. An already persisted occurrence task ID keeps its saved request. |

Dispatch, status and collect expose admitted context as the optional top-level
`team` field. It contains the selected scope/project, catalog ID, Team revision,
names/purpose, Member, Roles and original caller intent. The canonical task
request still owns execution settings. This is persisted evidence, not a current
catalog view; inspection works after the catalog is edited, deleted or unavailable.
Use [task inspection](../skills/agent-integration/references/command-reference.md#team-task-context)
to see it. A configured Member alone does not establish a task result.

## Scheduled Members and upgrades

`schedule create` accepts the same explicit target flags as dispatch. It saves
the scoped target, original caller choices/omissions and immutable catalog ID,
not a frozen Team revision or resolved Member defaults. Changing that saved
target requires a new schedule. Each new occurrence uses the normal admission
path to resolve its Member once. Catalog edits leave running tasks and previous
results intact.

Run the scheduler with the intended user configuration context. If it resolves
a different catalog instance, admission fails even when matching Team/Member
names exist there. A missing Member, project mismatch or inadmissible effective
configuration fails explicitly without a substitute launch. Existing scheduler
closure, release and non-overlap rules still apply; a failed occurrence does
not automatically disable the schedule.

Before creating or using Team-targeted schedules, follow the
[stop–update–restart procedure](cli-reference.md#scheduler-upgrades).
Old binaries strictly reject the new saved request fields. Mixed-version
rolling readers are unsupported; do not run old and new scheduler readers
against new targeted definitions together.

Teams add no TUI, automatic retries, provider-account management or catalog
relocation/synchronization. Provider settings, accounts and native permissions
remain user/provider-owned; see the [product boundary](../agents/_data/product-boundary.md).
