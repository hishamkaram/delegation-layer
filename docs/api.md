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

A fresh `dispatch` or `continue` runs the automatic
[resource check](cli-reference.md#resource-check) before workspace, provider,
or supervisor preparation. A created request is durable before supervisor
submission. The response includes `task_id`, `task_record`, and the initial
admission, liveness, and publication states.

A `task_id` can be allocated, or supplied with `--id`, without a task being
created. Only `task_record: "created"` names a task: keep that ID as the only
handle for later `status`, `collect`, and `continue` calls. With
`task_record: "not_created"`, no task exists, so there is nothing to poll;
follow `failure.next_action`. A `provider_unavailable` failure whose cause is
a capability the recorded launch requires and the installed provider CLI no
longer offers adds `failure.reason_code: "provider_capability_changed"` and a
fixed detail in `error`; its exit code and `stop_and_report` guidance are
unchanged. With `task_record: "unknown"`, existence could
not be proven; follow the returned stop guidance and do not dispatch again.

### Resource pressure

Confirmed pressure (sustained, or critical memory) refuses only the new
request. The response exits `1` with `task_record: "not_created"`,
`failure.code: "resource_pressure"`, `failure.stage: "check-resources"`, a
`reason_code` naming the metric and basis, `failure.next_action:
"retry_later"`, and `failure.retry_after_seconds`. This is the only response
that carries `retry_later`. The delay is advice, not a reservation: Delegate queues
nothing and never retries for the caller. Report the refusal, or wait and
repeat the exact original admission once; a `continue` stays `continue`.

An unavailable metric never refuses by itself. If no independent pressure is
confirmed, the resource check allows admission to continue with
`resource_admission.status: "unknown"` and a structured warning; later
admission steps may still fail. If the task is created, observe it; do not
dispatch again. Running tasks are untouched, and `status`, `collect`,
`cancel`, and recovery of an existing or uncertain task are never checked. The
[CLI reference](cli-reference.md#resource-check) owns the fields and caller
decisions; [architecture](architecture.md#fresh-admission-resource-check)
owns the signals, placement, and limits.

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

The exact response can also contain `task_record`, `status`, `parent_task_id`,
`capability`, `continuation`, `supervisor`, `raw`, `pending`, `stops`, `stop`,
`failure`, `resource_admission`, `diagnostics`, `recheck`, `error`, and
`error_truncated` fields depending on
the command and observation. Payload and sealed raw output are descriptors, so a client reads
the named file only after validating that its rooted path, length, and digest
match the response.

`outcome` is the terminal authority. A response may contain both a committed
outcome and an operational `error`; the error describes an ancillary failure
such as cleanup or a descriptor read and does not erase the committed result.

`task_id` is the durable Delegate identity. `supervisor.numeric_task_id` and
`stops[].numeric_task_id` are private, ephemeral queue addresses that the
bundled queue may reuse for a later task; they are not task handles.

## Checkpoint response

`checkpoint` commands return the same versioned envelope with
`command: "checkpoint"` and no `task_id` or `outcome`. Three optional fields
carry checkpoint facts:

```text
checkpoint: {
  id?, operation_id?,
  current?: CheckpointDescription,
  operation?: CheckpointRemovalDescription,
  reason_code?, next_action?
}
checkpoints: [CheckpointDescription]   (list only; [] when empty)
checkpoint_cursor?: string             (list only; next --after value)

CheckpointDescription: id, selection ("ordinary" | "managed"), workspace_id?,
  cwd?, state ("pending" | "checkpointed" | "unavailable"), reason_code?,
  snapshot_ref?, commit_oid?, usable, removal?: CheckpointRemovalDescription
CheckpointRemovalDescription: operation_id, attempts, state
  ("pending" | "removed" | "nothing_to_remove" | "unavailable"), reason_code?
```

An `unavailable` creation has `reason_code` `checkpoint_failed`,
`checkpoint_not_launched`, or `checkpoint_result_unavailable`; an unavailable
removal uses the matching `checkpoint_removal_*` reason.

`id` is the acknowledged or authenticated creation; `operation_id` names only
an acknowledged or authenticated registered removal attempt. Both survive a
later read fault, and an unreadable description is omitted rather than shown
as empty availability. `current.removal` is the latest attempt; `operation` is
the attempt selected with `--operation`, and its own state chooses that exit.
`attempts` counts every registered attempt, not the described attempt's
position. Command `reason_code` and `next_action` (`watch_checkpoint`,
`stop_and_report`, `retry_create`, `retry_remove`, or
`requested_action_not_authorized` as a reason) never overwrite a receipt's
reason inside a description.

`state` is the immutable creation history; `usable` is derived current
availability. It is `true` only for a `checkpointed` creation with no
registered removal attempt. It describes Delegate's records, not a fresh Git
ref read. A `snapshot_ref` or `commit_oid` can describe a retained candidate of
an unavailable creation. Exit codes and the full decision table are in the
[CLI reference](cli-reference.md#git-checkpoints).

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

Catalog entries expose `supported_options` and any `required_options` using
Delegate's public request names. `interaction` describes native clarification
and steering support. `runtime` identifies the version/help commands and
required public flags; these are requirements, not observations of this host.
See the [provider guide](providers.md) for the current profile table.

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
`partial` use `0`; `blocked` and `unavailable` use `2`. `failed` is a
discovery failure and uses `1` for an ordinary operational failure or `2` for
an invalid or unsupported request, such as an invalid `--cwd` or a
root/workspace overlap. Status and exit are separate observations; `failed`
with exit `2` is not an authentication block. JSON remains parseable for each
status.

When Delegate cannot safely use or transition the state root's supervisor,
discovery returns `status: "blocked"` with one of the stable supervisor reason
codes: `supervisor_migration_blocked`,
`supervisor_migration_unsupported`, or `supervisor_upgrade_required`. It does
not misreport that condition as a provider or model failure, and it returns no
model facts.

When discovery fails operationally, the response also includes an optional
`failure` object with the same fields as a `not_created` dispatch failure
(`code`, optional `reason_code`, `stage`, `next_action`, optional
`recovery_command`, and optional `inspection`) and a fixed public `error`
message. Keep both fields and
follow `failure.next_action`; a `stop_and_report` response is reported, not
retried.

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
The successor launches in the current caller's environment and supervisor
control, not the predecessor's; the predecessor's records decide compatibility.
The [provider guide](providers.md) owns per-provider profile rules.
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
