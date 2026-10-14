# Troubleshooting

Use `--json` while diagnosing a task. It keeps admission, liveness, and
publication separate and includes a bounded error message without embedding
provider answer text.

## Provider is not found or rejected before admission

Check that the executable is on the dispatch process's `PATH`, is a regular
executable file, and can run in the requested workspace. Delegation Layer
invokes the provider's version and help commands and checks the stable runtime
flags declared by the selected profile. A missing declared flag or an
unreadable executable is a capability failure, not a release-version failure.
Provider-specific launch options remain validated by the actual provider turn;
this avoids rejecting current CLIs whose noninteractive help output omits or
truncates forwarded options.

The observed version is evidence for diagnostics and task binding only. It is
not checked against a hardcoded release, semver range, web lookup, or binary
allowlist. If a probe is rejected, inspect the bounded reason class in the
control response and compare the selected CLI's current help output with the
profile's declared runtime flags.

Use `delegate providers --json` to confirm the profile ID and required runtime
flags. Discovery does not prove that the current host has the executable.

`provider_preflight_failed` can mean the supervised inspection worker failed
before it established provider readiness. That failure does not prove the
provider is unavailable, misconfigured, or unauthenticated. Preserve the
response and follow its `failure.next_action`; do not change provider settings
based only on a failed inspection worker.

For this failure, `failure.inspection` reports a sanitized reason class, the
immutable admission deadline state, and optionally the inspection journal task
ID. An `open` deadline means the failure occurred before that deadline; it does
not authorize a retry or supervisor repair. Follow `failure.next_action` and
report these fields as evidence without inferring a provider configuration or
authentication problem.

Codex and Claude use the same portable runtime admission path on Linux and
Darwin. If either is rejected, inspect the specific executable, required flag,
policy, or authentication reason in the structured response.

## Authentication fails

Authenticate with the provider's own CLI workflow and retry a new task. Do not
copy credential files into the Delegation Layer state root. Native login caches
and account policy can change between preparation and launch; the provider's
reported authentication result is authoritative for that turn.

For a live acceptance command, an unavailable native login is reported as
`BLOCKED` with exit status `2` only when structured Delegate state, a supported
native authentication-status response, or a structured HTTP 401/403 provider
error positively identifies it. An adapter may instead publish a stable,
sanitized authentication reason code from an exact structured provider
envelope; Antigravity uses `authentication-unavailable`. Free-form provider
text remains a normal failure. The receipt is sanitized and the aggregate live gate
treats that status as neutral; a non-authentication rejection remains a
failure.

## Mode or option is unsupported

Compare the request with the catalog entry. `antigravity:print` supports
both permission modes, `continuation`, `model`, `effort`, and `native-timeout`;
Codex, CCR, and Claude support both modes plus `model` and `effort`; Pi and OpenCode
support both modes plus their documented model and effort options. `ccr:print`
requires `--model` to be a configured CCR alias. AGY
read-only uses native `--sandbox --mode plan` without a bypass; authorized
workspace-write retains the native accept-edits mode and bypass. Codex maps
effort to TOML `model_reasoning_effort`, CCR, Claude, and AGY pass native effort
flags, Pi maps effort to `--thinking`, and OpenCode maps it to `--variant`.
An omitted or `default` effort is left out of native argv. Unsupported model,
effort, policy, or timeout combinations are rejected before supervisor
admission.

If an explicit choice is needed, run:

`delegate models --provider PROFILE [--cwd ABS] --json`

The command is implemented for the six profiles that expose model discovery and
returns advisory model facts through the state-rooted supervisor with a
`model-discovery-v1` five-minute inspection bound. The longer bound covers
first-run provider model/cache initialization. `complete` describes model
enumeration; null
efforts mean unknown metadata and `[]` means no choices were explicitly
reported. Do not use discovery as an admission allowlist or infer per-model
compatibility from harness-wide effort values. Provider defaults remain valid.

## Supervisor configuration fails

Normal dispatch starts the bundled Pueue client and daemon with a private Unix
socket below the state root. `dispatch`, `status`, `cancel`, and continuation
checks restart that private daemon after a restart when the saved binding still
matches. For this private supervisor, the canonical config path, config
digests, and endpoint define the durable binding. Executable paths, hashes,
resolver details, and observed versions are provenance, so upgrading the CLI
does not invalidate a compatible daemon that is already serving the private
queue. Bootstrap and recovery verify it through the resolved Pueue client's
protocol response. Warm admission validates the completed active-generation
records under their shared lease and probes the private Unix endpoint directly.
It leaves that daemon and its queued work running. A different verified client
may inspect a compatible live endpoint without changing its generation. If the
endpoint is unavailable, Delegate restarts the saved private executable pair
when available. If that active pair is unavailable, Delegate returns the
migration-required failure; the explicit `supervisor migrate` transaction then
transfers authority to the staged pair. Launch provenance is updated only after
the authorized generation becomes ready.

Before first publication, Delegate checks the generated socket path against the
host limit. If the ordinary state-rooted name is too long, it uses a compact
name in the same private supervisor directory. A release that encounters the
exact older over-limit config left by an interrupted pre-identity bootstrap
repairs that config atomically and continues. Any changed config, active
identity, migration/recovery artifact, endpoint, PID, runtime data, or
scheduling record still fails closed; users should not edit those files.

On an upgraded installation, a different verified state-root executable pair
is only a staged successor. The completed supervisor identity remains the
active generation, and its ordinary-scheduling receipt must match that exact
generation. Delegate repairs a stale scheduling receipt against the active
generation once; it does not repeatedly select the staged pair or migrate a
live queue during dispatch. A malformed identity or scheduling receipt fails
closed and is not overwritten as an automatic repair.

If a reachable endpoint returns malformed or incompatible status, delegate
fails closed and does not start a second daemon. Preserve the state root and
report the bounded supervisor error. For an advanced integration, pass an
existing absolute `--pueue-config` path or set `DELEGATE_PUEUE_CONFIG` to that
path and keep its configuration and credentials outside the task state root
and workspace. The runtime accepts a nonempty observed version when readiness
and queue behavior match the supported schema, including additive fields that
do not change the required lifecycle data. A Pueue release upgrade alone does
not require changing the state root or installing a matching version.

Provider executable upgrades are handled by the runner's fresh preflight. If
the current CLI still passes its version/help and declared runtime checks, the
runner refreshes the launch plan and starts that current executable. A changed
task policy, workspace, writable root, predicate, or artifact contract remains
a refusal because those fields are admission-bound.

A legacy private supervisor binding without daemon identity can be recovered
when delegate resolves a matching executable pair and verifies the saved
private config and endpoint. If no pair can be resolved, delegate fails closed.
Preserve the state root and report the setup failure; do not edit or remove its
supervisor records.

An older private daemon path or digest is provenance drift. If its endpoint is
reachable and its status schema is compatible, normal dispatch adopts it and
leaves the old launch record unchanged. A private supervisor config or
endpoint change is a real migration and is handled only by the explicit
`supervisor migrate` command under the state-root bootstrap lock. Delegate
first reopens the exact saved binding and checks the old supervisor. If the old
endpoint is stopped, it starts the recorded daemon only long enough to inspect
the queue. When no queued or running work remains, Delegate shuts that endpoint
down through its own client and watches for the endpoint to become unavailable
after graceful shutdown, then starts the current bundled daemon when the old
and current bindings share that endpoint.
The legacy journal remains unchanged for audit. A prepare/complete migration
journal makes a crash during this sequence safe to resume.
Only Delegate-owned private supervisors are eligible for this transition;
external Pueue configurations are never stopped by migration. A journal bound
to an external supervisor reports `supervisor_migration_unsupported` and
`stop_and_report`; it does not suggest the private migration command.

Delegate reads supervisor status one bounded record at a time, so a large
queue does not inherit the 1 MiB limit for an individual control record.
The private supervisor's terminal callback requests best-effort cleanup after a
row reaches `Done`. Current ordinary and inspection callbacks first publish an
exact candidate event, then request a candidate pass. The event remains a
durable inbox entry if the generation write or process launch fails. An exact
terminal inspection observation also revalidates the candidate and starts an
independent maintenance owner without delaying admission. Callback bursts
coalesce in the per-root generation journal. One nonblocking election winner
waits through a bounded quiet window and streams the queue snapshot; losing
callback processes return immediately. The winner rechecks the journal after
releasing ownership, and every later pass gets the same quiet window. An
interrupted claim or in-flight control command remains recoverable; a finished
failed attempt waits for a later terminal trigger and does not spin. A
validated later root activation resumes retained inbox or journal work.
Candidate-event publication failure records a conservative full-recovery wake.
Normal passes expose only event-backed ordinary and inspection rows, while the
recovery scope retains exact historical ordinary-row reconciliation. Delegate
validates its own receipts and durable outcomes, rechecks each unchanged
terminal row under the queue-identity mutation, and removes only those exact
numeric IDs. Dispatch does not wait for this maintenance. Active, incomplete,
ambiguous, unrelated, or not-yet-observed inspection rows remain available
without blocking cleanup of validated rows. Failed removal retains the
candidate for a later terminal trigger to retry, so no Pueue command or
state-file editing is required.
`status` and `collect` also request removal of their task's exact recorded row
after a terminal outcome exists. A runner can publish that outcome shortly
before Pueue marks its row ended; Delegate leaves that still-running row intact
and defers removal to later private maintenance. Cleanup errors do not change
that outcome.
An explicit external `--pueue-config` is never cleaned by these paths.

If an admission command (`dispatch` or `continue`) cannot use the current
logical supervisor binding, its JSON response uses
`failure.code: "supervisor_upgrade_required"`,
`failure.next_action: "run_supervisor_migrate"`, and a sanitized
`failure.recovery_command`. Run the command with the exact same state root:

```sh
delegate --root "$HOME/delegation-state" supervisor migrate --json
```

Retry the exact original admission argv once only when the response reports
`supervisor_control.state: "migrated"` and
`supervisor_control.next_action: "retry_dispatch"`. The latter is a protocol
field, not an instruction to change command type: retry `dispatch` as
`dispatch`, and retry `continue` as `continue`. A continuation retry must not be
replaced by a fresh task or a direct provider call. If active queued or running
work is present anywhere in the legacy supervisor, the response uses
`supervisor_migration_blocked` and `wait_for_legacy_work`; leave that legacy
supervisor running until the work reaches a terminal state. Do not run `pueue`,
`pueued`, process searches, delete the state root, or switch to a provider CLI.

New queued model inspections record whether their worker is managed or custom.
An older inspection request without that ownership field is ambiguous, so an
upgrade leaves its queued command pinned to the original executable. Keep that
runner available until the inspection finishes; then rerun model discovery if
needed.

Queued ordinary tasks with legacy metadata receive a task-bound ownership
receipt before the CLI rewrites their managed runner command. That receipt lets
continuation and retry use the current managed runner after the old installation
is removed. If the receipt is absent and the recorded executable is unavailable,
delegate cannot safely infer its ownership and leaves the task unchanged.

## A task remains pending

`status` reports what is known and may recover the private Pueue binding.
`collect --watch 5s` waits for one bounded observation interval and may recover
the guardian lifetime for a requested budget stop. A queue-row end alone cannot
publish terminal facts. Queue time
does not consume the provider budget. Do not delete the task directory or reuse
its ID while the state is uncertain.

`pending.kind: "lifetime-finalization"` means the live guardian still owns
native cleanup or its durable finalization records. This is active task work,
so liveness remains `running`; `collect` returns exit `3` for a bounded watch.
Keep observing the same task. The guardian retries the current phase with
bounded attempts and capped backoff. Do not start or migrate Pueue, signal
processes, delete state, redispatch the brief, or call the provider directly.

On an exact Linux cgroup scope, recovery can terminate and recheck the recorded
scope after the managed worker exits. A cooperative scope fails closed if the
guardian was interrupted after acknowledging the worker but before a provider
launch identity became durable. In that narrow state, Delegate keeps the task
pending because an unverified or reused process identity could belong to an
unrelated process. Preserve the state root and report the task evidence; do not
run provider or process-control commands as a repair.

On Linux fallback scopes, protected or unreadable `/proc` environments from
unrelated processes do not block a live guardian. Its exact subreaper identity
and two fresh topology observations are authoritative for that running task. If
the guardian is lost, Delegate may clean processes whose identities remain
durable, but it cannot prove that an unrecorded launch is absent and therefore
does not publish terminal evidence. This is recovery evidence loss, not a
provider failure.

## A continuation is busy

The predecessor still owns its conversation reservation, or its runner has not
released it after a completed outcome or durable budget-stop termination. Use
`status` or `collect` until the response exposes `continuation.resumable: true`,
then retry the continuation with the exact predecessor ID.

## A result is rejected

A rejected outcome is a terminal result with exit code 4. Inspect the outcome
and sealed raw descriptors from `collect` and `logs`. Common causes are changed
provider output shape, session identity mismatch, an invalid
declared artifact, or a provider-reported failure. The task is not retried
automatically. A native configuration error does not by itself prove that the
user's settings are wrong: an obsolete adapter-supplied command override can
also cause it. Preserve the evidence and check the installed delegate version;
do not delete provider settings or MCP configuration as a generic repair.

### Completed provider output rejected after a provider update

Native providers can add large tool payloads, metadata, or duplicate fields in
opaque data without changing their final-answer contract. Current interpreters
skip unconsumed payloads while validating JSON syntax; duplicate fields that
control identity, completion, or the selected answer still reject the result.
Native event and answer text do not inherit the 1 MiB control-response limit.
Invalid optional usage metadata is reported separately from answer validity.

Current Codex output may publish `item.completed` for an `agent_message`
without a preceding `item.started`. The current interpreter accepts that
completion-only form while retaining strict lifecycle checks for updates,
duplicates, identities, and terminal events. Tasks sealed by the previous
Codex runtime-reported-v2 interpreter remain replayable; if their collection
response offers `failure.next_action: "recheck"`, use the offline recheck below
before spending another provider turn.

For an older rejected task, run `collect TASK_ID --json` with its original root.
If it offers `failure.next_action: "recheck"`, run:

```sh
delegate --root "$HOME/delegation-state" recheck TASK_ID --json
```

Use your original root if it differs from the example. Exit `0` and
`recheck.status: "recovered"` identify a verified derived answer. The original
rejected outcome is retained for audit. Exit `4` means the newer interpreter
still rejects the evidence; exit `1` or `2` means an operational error or an
ineligible request. Preserve those diagnostics rather than relaunching paid work,
editing task records, or extracting a plausible answer from raw logs.

## Collection fails after a successful run

If the response contains a valid outcome alongside an operational error, keep
the outcome: it is independently committed. Re-run `collect` or `logs` to
retry descriptor validation and reservation cleanup. Preserve the state root
for inspection when the error remains unresolved.

## A provider denied a command in a write task

Current workspace-write tasks request unattended native approval. Check the
installed delegate version and the task's recorded effective approval before
attributing a denial to provider settings. Tasks admitted before an upgrade
retain their original launch contract. Native managed policy, tool settings,
and provider failures can still prevent execution; report the sealed evidence
without deleting settings or switching to a direct provider command.

If the provider reports that its selected model has no tool-capable endpoint,
that is a model/backend failure, not a delegate permission denial. For adapters
that advertise `model`, an authorized model selection can be passed through
the `dispatch --model` option on a new request without editing global provider
settings.

A runner that reports that its supervisor task ended before provider start is
a separate lifecycle failure. Preserve that task's evidence and supervisor
binding; an admitted task must not be silently dispatched again.
