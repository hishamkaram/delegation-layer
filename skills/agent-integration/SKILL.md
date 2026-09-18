---
name: agent-integration
description: Use Delegation Layer from a provider-agnostic agent workflow, including capability discovery, bounded dispatch, terminal collection, and live authentication handling.
---

# Agent integration

This skill is self-contained as an operating guide. It gives an agent enough
information to prepare a safe run, choose a provider from observed
capabilities, and collect a terminal result when the host exposes the
Delegation Layer CLI. It applies to every provider profile and does not encode
provider names, release versions, or native CLI syntax.

## Preconditions

The host is responsible for installing the CLI. This skill must not install,
build, upgrade, or repair it. It may check whether `delegate` is available and
create separate task and state directories that it is authorized to use. A
released CLI carries the compatible `pueue` and `pueued` supervisor binaries
and starts a private instance automatically; an explicit supervisor config is
only needed for an advanced integration.
Authentication is a separate live gate; do not invent a provider-specific
login check or classify a missing login as static incompatibility.

Keep the supervisor configuration, Delegation Layer state root, and task
workspace as separate canonical paths. Do not put credentials in a brief, task
option, environment value recorded by the task, or agent-visible output.

## Start safely

First check that the already-installed CLI is available, then discover compiled
profiles:

```sh
command -v delegate
delegate help
delegate providers --json
```

If `command -v delegate` fails, stop and report that the host prerequisite is
missing. Do not install or build the CLI from this skill.

`providers --json` is static discovery. It lists provider IDs, supported
permission modes and options, and the help arguments and flags required by the
compiled adapters. It does not inspect the host, authenticate a provider, or
prove that a provider is launchable.

Choose a listed `PROFILE`, then inspect its provider-neutral contract:

```sh
delegate capabilities --provider PROFILE --json
```

This command is side-effect-free. It does not create state, contact Pueue, or
launch a provider. A successful catalog response has
`status: "unknown"`, `verification: "catalog"`, and
`live_acceptance.status: "not_run"`. Unknown here means that dispatch has not
run the host probe yet; it is neither a compatibility failure nor live proof.

Choose a listed `PROFILE`, ensure its provider executable is available to the
dispatch environment, and prepare a finite brief and two separate absolute
directories. Then dispatch one bounded task:

```sh
mkdir -p "${HOME}/delegation-workspace" "${HOME}/delegation-state"
printf '%s\n' 'Inspect the workspace and return a short summary.' > "${HOME}/delegation-brief.txt"

delegate --root "${HOME}/delegation-state" \
  dispatch \
  --provider PROFILE \
  --brief "${HOME}/delegation-brief.txt" \
  --cwd "${HOME}/delegation-workspace" \
  --permission read-only \
  --budget 30m \
  --json
```

The default release owns the private supervisor lifecycle under the state root.
If an existing compatible supervisor must be used, pass its absolute
`--pueue-config` path or set `DELEGATE_PUEUE_CONFIG`. If the CLI or provider
executable is unavailable, stop on the CLI's structured error. For live
acceptance, an unavailable authentication prerequisite is `blocked`; preserve
that evidence and never relabel it as
incompatible or passed.

Save the returned `task_id`. Admission records one immutable request and at
most one provider launch attempt; the command does not wait for completion.
Observe and collect with that exact ID:

```sh
delegate --root "${HOME}/delegation-state" status TASK_ID --json
delegate --root "${HOME}/delegation-state" collect TASK_ID --watch 5s --json
```

Repeat `collect` while the task is pending. Collection is observational: it
may wait for the bounded watch interval and clean up a validated reservation,
but it never launches, retries, or resumes provider work. A continuation is a
new task with an exact predecessor ID when the selected profile advertises
continuation support.

## Read the JSON contract

The capability response has this stable shape. The two arrays are
profile-specific and can be non-empty; empty arrays below only keep the example
provider-agnostic:

```json
{
  "schema_version": 1,
  "command": "capabilities",
  "capability": {
    "contract": "runtime-capability-v1",
    "provider": "PROFILE",
    "status": "unknown",
    "verification": "catalog",
    "help_args": [],
    "required_flags": [],
    "reason_code": "runtime_probe_not_run",
    "live_acceptance": {
      "status": "not_run",
      "authentication": "unknown",
      "reason_code": "runtime_probe_not_run"
    }
  }
}
```

Interpret the fields as follows:

- `status=unknown` with `verification=catalog` is a declaration from the
  compiled catalog.
- `status=ready` with `verification=runtime` appears in a dispatch response
  only after the supervised probe observed the required behavior and exact
  executable identity for that admission boundary.
- `status=unsupported` is a machine-readable refusal. Use `reason_code` and
  the bounded error to explain it; do not try another profile without a new
  explicit selection.
- `version` is diagnostic observation. Accept any valid nonempty trimmed UTF-8
  version text. Never use a release allowlist, semantic-version comparison, or
  web lookup as a compatibility decision. Required flags, executable identity,
  and provider behavior decide compatibility. An unusually large version may
  be omitted from the bounded projection.
- `live_acceptance` is separate from compatibility. `not_run` does not claim a
  live test; `passed` requires authenticated provider evidence; `blocked` means
  authentication or another prerequisite was unavailable and is neutral. A
  blocked result must never be reported as a pass or as incompatible behavior.

Dispatch JSON contains the same `capability` projection plus separate task
`admission`, `liveness`, and `publication` fields. Read those fields
independently. Trust a terminal `outcome` only after it has been returned by
the validated collection path and its payload and sealed evidence descriptors
match the task records.

## Integration acceptance contract

Apply these criteria to every provider and every agent integration:

- required behavior is verified at the runtime boundary;
- arbitrary valid provider version text remains acceptable;
- missing flags, executable identity drift, malformed facts, and timeouts fail
  closed;
- authentication is represented separately from compatibility;
- missing authentication is `blocked`, never a passing skip;
- admission is at most once and collection is observational;
- terminal results come from durable validated evidence; and
- JSON output is bounded, deterministic for observational commands, versioned,
  and free of credentials.

Do not expose credentials, tokens, private raw provider output, or unrestricted
diagnostics in prompts, tool results, receipts, or task metadata. After an
uncertain admission or lost response, inspect the original task with
`status`/`collect`; do not relaunch it. Stop and report the durable state when
the task cannot be resolved from validated evidence.

## Completion record

An integration is complete when it can provide:

- the selected profile and original task ID;
- the bounded capability and dispatch JSON responses;
- the terminal committed or rejected outcome and validated evidence
  descriptors; and
- a sanitized live-acceptance receipt marked `passed`, `failed`, or `blocked`.
