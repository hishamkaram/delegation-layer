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
`continuation`, `supervisor`, `raw`, `pending`, `stops`, `stop`, `failure`,
`diagnostics`, `recheck`, `error`, and
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

For lifetime-managed tasks, `liveness: "ended"` is publishable only after the
application lifetime state machine records termination with independently
proven empty scope and a durable worker completion disposition: the exact
managed-worker `Wait`, or `not-started` when no child was launched. A Pueue row
end or worker exit alone is insufficient. While the live guardian still owns
that finalization, responses use `liveness: "running"` and
`pending.kind: "lifetime-finalization"`; clients keep observing the same task
and do not repair the supervisor or redispatch it.

A committed collection exits `0`; a validated rejected provider outcome exits
`4`. `collect` returns `3` when publication is still pending after its bounded
observation interval. `status` remains observational and returns `0` even when
the supervisor state is uncertain, while reporting that uncertainty in its
bounded error field. Invalid requests use `2`; storage, supervisor, or output
faults use `1`. Clients should inspect both the exit code and the JSON authority
fields.

## Offline answer recovery

For an eligible rejected task, `collect` advertises
`failure.next_action: "recheck"`. Run `delegate --root ABS recheck TASK_ID --json`
once. It validates the original records and sealed evidence without invoking
providers or the supervisor. Source records remain immutable.

Exit `0` with `recheck.status: "recovered"` supplies an `answer` descriptor and
absolute `answer_path`, plus an absolute `report_path`, assessment ID, source
hashes, and target predicate. The original `publication` stays `rejected`;
consume the derived answer, not the original rejection payload. Exit `4` means
the reassessment still rejects the evidence; `1` is an operational/evidence
error and `2` an invalid or unsupported request. Cached assessments are
revalidated. Recovery does not change task lifecycle or continuation rights.

`diagnostics` contain bounded, sanitized interpretation details. Optional usage
warnings can accompany a committed or recovered answer without setting `error`.
Original diagnostics remain in `outcome`; a recheck reports its own diagnostics
separately. Never substitute raw stdout for an accepted answer.

An `inspect-provider` failure can include `failure.inspection` with a bounded
`class`, `deadline_state` (`open`, `expired`, or `unknown`), and optional
`journal_task_id`. These fields identify the failed admission phase without
exposing state paths or provider output. They are diagnostic evidence only;
`failure.next_action` remains the recovery decision.

## Discovery API

`delegate providers --json` returns a separate bounded response. This structural
example is abbreviated: it shows one entry and omits options, interaction, and
runtime metadata. Read the command output for the current complete contract:

```json
{
  "schema_version": 1,
  "providers": [
    {
      "id": "codex:exec",
      "supported_modes": ["read-only", "workspace-write"],
      "continuation": "native",
      "read_only": {
        "supported": true,
        "mechanism": "native-sandbox",
        "containment": "provider-owned"
      }
    }
  ]
}
```

The catalog describes compiled support. It does not claim that the executable
is installed, authenticated, or compatible on the current host. Managed
runtime-only dispatch records the static contract; the supervised worker
checks the current executable before provider execution. Inspection-backed
admission retains its runtime checks. See
[ownership boundaries](architecture.md#ownership-boundaries). The started and
sealed evidence records the final executable identity and observed version.
The version is descriptive evidence; flags, executable identity, and
behavior predicates establish compatibility without a web release lookup or
semver allowlist.

## Model discovery contract

`delegate models --provider PROFILE [--cwd ABS] --json` is the runtime
discovery command for native model IDs and effort facts. `--cwd` defaults to
the current directory and must be absolute when supplied. The command uses the
state-rooted supervisor and the shared `model-discovery-v1` five-minute
inspection bound, which allows first-run provider model/cache initialization. It creates
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

When Delegate cannot safely use or transition the state root's supervisor,
discovery returns `status: "blocked"` with one of the stable supervisor reason
codes: `supervisor_migration_blocked`,
`supervisor_migration_unsupported`, or `supervisor_upgrade_required`. It does
not misreport that condition as a provider or model failure, and it returns no
model facts.

## Capability contract

`delegate capabilities --provider PROFILE --json` returns the selected
provider's compiled capability contract without creating state, contacting the
supervisor, or launching a provider. Its `status: "unknown"` and
`verification: "catalog"` values are deliberate: this response is discovery,
not host readiness or live acceptance.

The response includes supported permission modes, request options, continuation,
and the read-only mechanism. `containment: "provider-owned"` means the adapter
selects the provider's native read-only behavior but does not create an
independent boundary around provider configuration, extensions, MCP servers, or
plugins. This is separate from process-lifetime containment, which is
guardian-owned and independently proven. Model and effort choices remain in the separate runtime `models`
response, and authentication remains unknown until a runtime or live
acceptance boundary observes it.

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
    "supported_modes": ["read-only", "workspace-write"],
    "supported_options": ["continuation", "model", "effort"],
    "read_only": {
      "supported": true,
      "mechanism": "native-plan",
      "containment": "provider-owned"
    },
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

For inspection-backed admission, successful runtime preparation can return a
dispatch `capability` object with `status: "ready"`,
`verification: "runtime"`, the observed version, and the executable digest.
Managed runtime-only admission instead returns `status: "unknown"` with
`verification: "catalog"`; its fresh probe runs in the supervised worker, so
collect the task outcome to establish execution and completion.
The observed version is omitted from this projection if its diagnostic text is
unusually large. Terminal launch evidence has its own bounded storage field;
if a refreshed version cannot fit that field, runtime preparation rejects it
before the provider starts. This is a record-size safeguard, not a release-version
compatibility check.
A ready runtime projection proves only the bounded compatibility probe for
that admission boundary. A catalog projection proves no host verification.
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

For a budget-stopped turn, continue only when `continuation.resumable` is true.
A normal terminal outcome, committed or rejected, can also be continued when
the provider supports it, the exact session was recorded, and the runner/session
reservation has been released. Not every rejection records a usable session.
Normal completion without budget-stop evidence need not include this projection
in `status` or `collect`; the `continue` admission response has its own
continuation fields. `codex:review` does not support continuation. Every
continuation creates a new task and preserves the exact provider session.
Follow the returned failure action when continuation is unavailable; do not
automatically retry a `stop_and_report` response.

## Collection contract

`collect` and `logs` are observational. They may read existing durable records,
wait for one bounded `--watch` interval, reconcile the saved Pueue binding, and
enter lifetime recovery only for an already-requested stop. A queue-row
end alone cannot publish terminal facts. They may
retry validated reservation cleanup, but they never launch, retry, or resume
provider work. A client can
therefore poll safely until publication is terminal and then replay the same
outcome later.

See the [CLI reference](cli-reference.md) for the full flag and response
reference, and [architecture](architecture.md) for ownership and recovery
boundaries.
