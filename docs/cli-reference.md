# CLI reference

`delegate` manages durable tasks. `delegate-run` is the supervisor-owned
worker and is normally invoked for you; run it directly only when developing
or integrating a custom supervisor.

## Synopsis

```text
delegate [--root ABS] [--pueue-config ABS] [--runner ABS] dispatch \
  --provider PROFILE --brief FILE --cwd ABS [--id TASK_ID] \
  [--permission MODE] [--budget DURATION] [--model MODEL] \
  [--effort EFFORT] [--native-timeout DURATION] \
  [--resume-task PREDECESSOR_ID] [--json]

delegate [--root ABS] providers [--json]
delegate [--root ABS] status TASK_ID [--json]
delegate [--root ABS] collect TASK_ID [--watch DURATION] [--json]
delegate [--root ABS] logs TASK_ID [--json]
delegate [--root ABS] cancel TASK_ID [--json]
delegate help
delegate version
```

Global paths must be absolute. `--root` defaults to the user's configuration
directory joined with `delegation-layer`. Dispatch requires `--pueue-config`,
unless `DELEGATE_PUEUE_CONFIG` supplies the same absolute path.

## Dispatch

`dispatch` validates and persists the request before it contacts Pueue. The
brief must be a regular file no larger than 8 MiB. It is supplied to the
provider through standard input and is never appended to provider arguments.
`--cwd` must name an existing absolute workspace directory. State and workspace
paths must remain disjoint.

| Flag | Meaning |
| --- | --- |
| `--provider PROFILE` | Provider profile from `delegate providers`, such as `codex:exec`. |
| `--brief FILE` | Finite task brief, no larger than 8 MiB. |
| `--cwd ABS` | Canonical workspace directory. |
| `--id TASK_ID` | Optional 32-character lowercase hexadecimal ID. Omit to allocate one. |
| `--permission MODE` | `read-only` (default) or `workspace-write`. |
| `--budget DURATION` | Positive Go duration; default `30m`. Queue time is excluded. |
| `--model MODEL` | Model selector when the chosen profile advertises `model` (currently Pi and OpenCode). |
| `--effort EFFORT` | Provider effort/variant selector when the chosen profile advertises it (Pi maps this to `--thinking`; OpenCode maps it to `--variant`). `default` keeps the provider default. |
| `--native-timeout DURATION` | Antigravity's native print timeout, no greater than `--budget`. |
| `--resume-task TASK_ID` | Exact predecessor for a continuation; creates a new task. |
| `--json` | Emit the versioned control response. |

Raw provider arguments, token ceilings, and dollar ceilings are deliberately not
part of this interface. Unsupported options fail before supervisor admission.

Every task ID is single-use for the requested turn. Repeating a dispatch with
the same ID requires the same immutable request and cannot grant another
provider launch. A continuation is a separate task that names an exact
predecessor and reserves the provider conversation until the predecessor has a
validated terminal outcome and its runner has released ownership.

## Providers and runtime compatibility

`providers --json` returns a bounded object with `schema_version: 1` and a
sorted `providers` array. Each entry contains its ID, supported permission
modes, supported request options, and runtime metadata: optional help-command
arguments plus the flags required by the adapter.

Discovery is metadata only. It does not inspect the host, authenticate a
provider, contact Pueue, create state, or launch a process. During dispatch,
the selected adapter locates its executable, verifies that it is a regular
executable file, runs its version command, then runs its help command and
checks that every required flag is advertised. Any valid nonempty version is
accepted; valid means trimmed UTF-8 text without control characters, not a
particular semantic-version pattern. A missing executable or required flag
rejects the request. The
executable is fingerprinted again before launch so a replacement cannot be
silently substituted.

The observed version is descriptive task evidence, not a release authorization.
The compatibility check does not use a web release lookup, semver allowlist, or
hardcoded provider version. Probe diagnostics expose bounded reason classes;
provider output and process diagnostics are not copied into control fields.

The compatibility probe is only a startup check. Provider output shape,
identity, containment, policy, authentication, and result integrity remain
strict runtime contracts; a CLI that starts successfully can still produce a
rejected task outcome if it violates those contracts.

## Observation commands

`status` reports the saved admission, current liveness, and publication state.
`collect` reads a committed outcome or waits for one observation interval when
`--watch` is positive; `--watch` defaults to `0s` and never changes the task
budget. Collection never launches, retries, or resumes provider work. `logs`
returns validated descriptors for the raw stdout and stderr streams. `cancel`
records one explicit stop request and observes its effect; it does not turn an
uncertain process result into a successful outcome.

When a task has a committed outcome, that outcome is the terminal authority.
Payload and sealed raw streams are returned as descriptors with rooted paths,
byte sizes, and SHA-256 digests rather than copied into the control response.

## JSON responses and exit codes

With `--json`, task-command responses contain `schema_version: 1`, the command
name, task identity when applicable, and separate `admission`, `liveness`, and
`publication` fields. The `providers` response is a separate catalog schema.
An `outcome` is present only after it has passed the recorded provider
predicate. Operational diagnostics appear in `error` and do not erase an
independently valid published outcome.

| Exit code | Meaning |
| --- | --- |
| `0` | Command completed; collection returned a committed outcome when applicable. |
| `1` | Operational, storage, supervisor-binding, or output error. |
| `2` | Invalid request or unsupported profile/configuration. |
| `3` | `collect` reached the end of its bounded watch interval without a terminal publication. |
| `4` | Collection returned a valid rejected provider outcome. |

All complete JSON responses are bounded. Oversized diagnostic text is shortened
while authority fields and descriptors are retained.

## Supervisor binding

The initial dispatch binds Pueue 4.0.4, the resolved executable, its absolute
path, and the exact configuration and resolved settings used for that task.
Later operations use that saved binding and reject a mismatched fresh
authority. The configuration parser accepts YAML and JSON-form YAML with
strict fields and bounded depth, aliases, nodes, and document size.

`delegate-run` normally resolves beside `delegate`. Use the absolute global
`--runner` option for a source-build layout or a custom integration.

For setup failures, see [Troubleshooting](troubleshooting.md). For provider
specific behavior, see [Providers](providers.md).
