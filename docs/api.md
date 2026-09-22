# Integration API

The supported integration surface is the `delegate` command and its versioned
JSON responses. Go packages under `internal/` are implementation details and
are not a public import API.

## Request flow

A client creates a task with `dispatch`:

```sh
delegate --root /absolute/path/to/state dispatch \
  --auto \
  --brief /absolute/path/to/brief.txt \
  --cwd /absolute/path/to/workspace \
  --permission read-only \
  --budget 30m \
  --json
```

The request is durable before supervisor submission. The response includes the
allocated `task_id` and the initial admission, liveness, and publication
states. Keep that ID as the only handle for later calls.

## Task control response

Every task-command JSON response has `schema_version: 1`, a `command`, and
bounded admission, liveness, and publication fields:

```json
{
  "schema_version": 1,
  "command": "collect",
  "root_id": "…",
  "task_id": "…",
  "admission": "admitted",
  "liveness": "ended",
  "publication": "committed",
  "outcome": {
    "verdict": "committed",
    "evidence_sha256": "…",
    "payload": {
      "basename": "result.txt",
      "length": 123,
      "sha256": "…"
    }
  }
}
```

The exact response can also contain `status`, `parent_task_id`, `capability`,
`continuation`, `supervisor`, `raw`, `pending`, `stops`, `stop`, `error`, and
`error_truncated` fields depending on
the command and observation. Payload and sealed raw output are descriptors, so a client reads
the named file only after validating that its rooted path, length, and digest
match the response.

`outcome` is the terminal authority. A response may contain both a committed
outcome and an operational `error`; the error describes an ancillary failure
such as cleanup or a descriptor read and does not erase the committed result.

## State model

The three status dimensions are independent:

- `admission`: `unknown`, `not-admitted`, or `admitted`;
- `liveness`: `undetermined`, `running`, or `ended`; and
- `publication`: `unknown`, `pending`, `committed`, or `rejected`.

A committed collection exits `0`; a validated rejected provider outcome exits
`4`. `collect` returns `3` when publication is still pending after its bounded
observation interval. `status` remains observational and returns `0` even when
the supervisor state is uncertain, while reporting that uncertainty in its
bounded error field. Invalid requests use `2`; storage, supervisor, or output
faults use `1`. Clients should inspect both the exit code and the JSON authority
fields.

## Discovery API

`delegate providers --json` returns a separate bounded response (the example
shows one catalog entry):

```json
{
  "schema_version": 1,
  "providers": [
    {
      "id": "codex:exec",
      "supported_modes": ["read-only", "workspace-write"],
      "supported_options": ["continuation"],
      "continuation": "native",
      "runtime": {
        "help_args": ["exec"],
        "required_flags": [
          "--cd",
          "--color",
          "--json",
          "--output-last-message",
          "--sandbox",
          "-c"
        ]
      }
    }
  ]
}
```

The catalog describes compiled support. It does not claim that the executable
is installed, authenticated, or compatible on the current host. Dispatch does
that runtime capability check and records the observed executable identity and
version. The version is descriptive evidence; flags, executable identity, and
behavior predicates establish compatibility without a web release lookup or
semver allowlist.

## Model discovery contract

`delegate models --provider PROFILE [--cwd ABS] --json` is the runtime
discovery command for native model IDs and effort facts. `--cwd` defaults to
the current directory and must be absolute when supplied. The command uses the
state-rooted supervisor and the shared `model-discovery-v1` 60-second
inspection bound. It creates
inspection evidence without admitting a provider task or launching a delegated
turn.

```json
{
  "schema_version": 1,
  "command": "models",
  "provider": "claude:print",
  "observed_at": "2026-09-21T10:20:30.123Z",
  "status": "partial",
  "reason_code": "native_metadata_incomplete",
  "complete": false,
  "source": "claude stream-json initialize",
  "efforts": ["low", "high"],
  "models": [
    {"id": "claude-sonnet", "name": "Claude Sonnet", "efforts": null}
  ]
}
```

The top-level `efforts` list is harness-wide metadata. `null` means unknown;
`[]` means the provider explicitly reported no choices. Per-model `efforts`
has the same meaning. `name` and `default_effort` are optional. `complete`
describes model enumeration, so it can be true while effort metadata is null.
Do not infer per-model compatibility from the harness-wide list or from a model
name. A listed model does not prove that the account can execute it.

Discovery is advisory and never an admission allowlist. Dispatch validates the
requested model and effort through the selected adapter and its runtime probe.
The result status maps to process exit codes as follows: `available` and
`partial` use `0`; `blocked` and `unavailable` use `2`; `failed` uses `1`.
JSON remains parseable for each status.

## Capability contract

`delegate capabilities --provider PROFILE --json` returns the selected
provider's compiled capability contract without creating state, contacting the
supervisor, or launching a provider. Its `status: "unknown"` and
`verification: "catalog"` values are deliberate: this response is discovery,
not host readiness or live acceptance.

```json
{
  "schema_version": 1,
  "command": "capabilities",
  "capability": {
    "contract": "runtime-capability-v1",
    "provider": "PROFILE",
    "status": "unknown",
    "verification": "catalog",
    "continuation": "native",
    "required_flags": ["…"],
    "reason_code": "runtime_probe_not_run",
    "live_acceptance": {
      "status": "not_run",
      "authentication": "unknown",
      "reason_code": "runtime_probe_not_run"
    }
  }
}
```

After successful runtime preparation, a dispatch JSON response contains the
same `capability` object with `status: "ready"`,
`verification: "runtime"`, the observed version, and the executable digest.
The observed version is omitted from this projection if its diagnostic text is
unusually large; the exact validated value remains bound in task metadata.
This proves only the bounded compatibility probe for that admission boundary.
Live acceptance remains separate: `passed` requires authenticated provider
evidence, while missing authentication is `blocked` and neutral.

## Idempotency and continuation

A task ID identifies one immutable request and at most one provider launch
attempt. Reusing the same ID with different request bytes is rejected; reusing
it with the same request cannot launch a second turn. A continuation is a new
task with `continue --task PREDECESSOR_ID` and an exact predecessor
relationship. The older `dispatch --resume-task` form remains available for
compatibility.

After durable budget termination, task responses add:

```json
{
  "status": "timed_out",
  "continuation": {
    "resumable": true,
    "mode": "native",
    "predecessor_task_id": "…",
    "continue_command": "delegate --root '/absolute/state' continue --task … --json"
  }
}
```

Only a response with `resumable: true` may be passed to `continue`. The
continuation creates a new task and preserves the exact provider session.

## Collection contract

`collect` and `logs` are observational. They may read existing durable records,
wait for one bounded `--watch` interval, and reconcile the saved supervisor
binding to record termination for an already-requested budget stop. They may
retry validated reservation cleanup, but they never launch, retry, or resume
provider work. A client can
therefore poll safely until publication is terminal and then replay the same
outcome later.

See the [CLI reference](cli-reference.md) for the full flag and response
reference, and [architecture](architecture.md) for ownership and recovery
boundaries.
