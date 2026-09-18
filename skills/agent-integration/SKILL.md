---
name: agent-integration
description: Install and use Delegation Layer from a provider-agnostic agent workflow, including capability discovery, bounded dispatch, terminal collection, and live authentication handling.
---

# Agent integration

This skill is self-contained. It gives an agent enough information to install
the `delegate` CLI and this skill, prepare a safe first run, choose a provider
from observed capabilities, and collect a terminal result. It applies to every
provider profile and does not encode provider names, release versions, or
native CLI syntax.

## Install the CLI and skill

Install `delegate` and its companion `delegate-run` from a project release
archive when one is available, or build them from a source checkout:

```sh
git clone https://github.com/hishamkaram/delegation-layer.git
cd delegation-layer
mkdir -p "${HOME}/.local/bin"
CGO_ENABLED=0 go build -buildvcs=false -o "${HOME}/.local/bin/delegate" ./cmd/delegate
CGO_ENABLED=0 go build -buildvcs=false -o "${HOME}/.local/bin/delegate-run" ./cmd/delegate-run
```

Put that directory on `PATH`. A source build needs the project's pinned Go
toolchain. A release install does not need Go.

Install this file into the target agent's skill directory. For Codex, a source
checkout can be installed with:

```sh
skill_root="${CODEX_HOME:-${HOME}/.codex}/skills/agent-integration"
mkdir -p "${skill_root}"
cp skills/agent-integration/SKILL.md "${skill_root}/SKILL.md"
```

For another agent, copy the same `SKILL.md` into that agent's documented skill
directory or use its skill installer. The file has no required repository
references or supporting files.

Before the first dispatch, the host also needs:

- `pueue` and `pueued` from the version supported by the installed Delegation
  Layer release, with a private configuration and socket;
- at least one provider CLI discoverable by the selected compiled profile; and
- an authenticated provider session created through that provider's normal
  login flow.

Keep the Pueue configuration, Delegation Layer state root, and task workspace
as separate canonical paths. Do not put credentials in a brief, task option,
environment value recorded by the task, or agent-visible output.

## Start safely

Check the installation and discover compiled profiles:

```sh
delegate --help
delegate providers --json
```

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

Prepare a finite brief and two separate absolute directories, then dispatch
one bounded task:

```sh
mkdir -p "${HOME}/delegation-workspace" "${HOME}/delegation-state"
printf '%s\n' 'Inspect the workspace and return a short summary.' > "${HOME}/delegation-brief.txt"

delegate --root "${HOME}/delegation-state" \
  --pueue-config /absolute/path/to/private-pueue.yml \
  dispatch \
  --provider PROFILE \
  --brief "${HOME}/delegation-brief.txt" \
  --cwd "${HOME}/delegation-workspace" \
  --permission read-only \
  --budget 30m \
  --json
```

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

The capability response has this stable shape:

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

Run the repository's skill verification after changing this file when working
from the source checkout.
