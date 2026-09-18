# Integration API

The supported integration surface is the `delegate` command and its versioned
JSON responses. Go packages under `internal/` are implementation details and
are not a public import API.

## Request flow

A client creates a task with `dispatch`:

```sh
delegate --pueue-config /absolute/path/to/pueue.yml dispatch \
  --provider codex:exec \
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

The exact response can also contain `capability`, `supervisor`, `raw`,
`pending`, `stops`, `stop`, `error`, and `error_truncated` fields depending on
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
      "runtime": {
        "help_args": ["exec"],
        "required_flags": [
          "--cd",
          "--color",
          "--ignore-rules",
          "--ignore-user-config",
          "--json",
          "--output-last-message",
          "--sandbox",
          "--strict-config",
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
    "help_args": [],
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
task with `--resume-task PREDECESSOR_ID` and an exact predecessor relationship.

## Collection contract

`collect` and `logs` are observational. They may read existing durable records,
wait for one bounded `--watch` interval, and retry validated reservation
cleanup, but they never launch, retry, or resume provider work. A client can
therefore poll safely until publication is terminal and then replay the same
outcome later.

See the [CLI reference](cli-reference.md) for the full flag and response
reference, and [architecture](architecture.md) for ownership and recovery
boundaries.
