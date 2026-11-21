# Guarded local connector contract

Delegate owns task admission, execution lifetime and accepted result evidence.
A connector calls the public CLI; it does not read private task files, submit
queue commands or reconstruct lifecycle state. The optional `connector` JSON
object adds context and correlation observations to the existing response.
Existing admission, liveness, publication and outcome fields keep their meanings.

## Context and explicit cutover

```sh
delegate context inspect --root ABS --cwd ABS --gateway-id G --json
delegate context enable-connector --root ABS --offline-cutover-confirmed --json
```

Inspection is passive: an absent root fails without initialization. The response
contains `connector.contract_version: 1`, `features`, `root_token` and, when a
project was supplied, `project_token`. Guarded connector operations require both
`namespace-admission-v1` and `namespace-mutation-lease-v1`; version 1 alone is
insufficient. With `--gateway-id`, `origin_registered` is false for
an unregistered origin; otherwise `origin_barrier` reports its gateway, recovery,
generation and `admission_open`. Inspection never chooses a generation.

Enablement is a one-way operator cutover on an existing root. Before attesting,
stop old frontends/writers and drain their tasks. The flag records that explicit
operator assertion; it cannot prove old processes have stopped. Enablement
preserves RootID and physical root identity and adds the required root features.
It can also upgrade an admission-only candidate root. The root-wide namespace
mutation lock is made durable before its required feature is published; an
enabled root with a missing or unsafe lock fails instead of recreating it.
An older strict reader refuses the upgraded root on its next open. A process
that already opened the old root still requires operator closure.

Dynamic workspace selection requires `workspace-constrained-connector-v1`.
Canonical-path approvals additionally require `connector-path-project-v1`, which
depends on the workspace and namespace features. Existing connector-enabled
roots need an explicit offline cutover before using these capabilities:

```sh
delegate context enable-connector --root ABS --workspace-constrained --offline-cutover-confirmed --json
```

For an absent root, `delegate context initialize --root ABS
--workspace-constrained --json` publishes the required features and durable
namespace lock through a create-only operation. It starts no supervisor. An
existing or concurrently created root fails with `root_exists`; it is never
silently adopted or upgraded by initialization. Workspace inspection and the new
Gateway client report `root_cutover_required` when the selected existing root
lacks the feature needed by that operation.

The root token identifies the actual Native root, including physical identity.
Project tokens have two explicit representations. Legacy v1 binds canonical
path/device/inode; `context inspect --cwd` retains that representation. New
Gateway workspace approvals use v2, whose decoded JSON is
`{"version":2,"project":{"path":"CANONICAL_PATH"}}`. V2 binds only the canonical
path, accepts same-path replacement and requires the path feature on the root.
Mixed or malformed representations fail; old v1 records are never reinterpreted.
Keep raw CLI tokens local
to the connector; do not put them in remote messages, logs or a registry. An
external gateway can use `ctx1_` plus SHA256 of the local token as its wire
identity, while its connector checks that hash and supplies the pinned actual
CLI token locally. This encoding is an external connector policy, not another
Delegate context identity. Replacing a root invalidates every control guard.
For v1, replacing or removing a project invalidates new admission and existing-task
resume. For v2, replacement or recloning at the same canonical path is allowed;
a missing directory refuses the next acquisition. Observation, result retrieval,
cancellation and fencing retain the live
root guard and compare the exact saved project token when a request exists;
they do not require its workspace to remain present. For an absent request,
the echoed project token is validated input, not a verified project receipt.
No binding changes its saved canonical path.

Root guards remain physical and exact. A device/inode change fails closed;
it is never silently mapped to saved approvals or tasks. The explicit
[root context repair](#root-context-repair) below can accept a device-number
change for the same original canonical path and inode while preserving its
saved token. It provides no automatic stable Darwin volume-identity mapping.

Guarded execution retains the selected project directory through the existing
runner lease. Preflight commands for that project and the provider
child borrow it through the authenticated launch gate; after authorization the
child selects and checks its working directory, changes directory through its
descriptor and closes the descriptor before provider execution. V1 retains its
physical checks. V2 reacquires the current approved ancestor and selected path
after gate authorization; physical observations only check that acquisition's
consistency. A running command keeps its original cwd. Later path replacement
or removal does not poison its result; root, process, capture and storage checks
remain in force. A child gate refusal prevents provider execution and accepted output.
These checks protect the initial working directory and observed mismatches;
they do not make later third-party absolute path access atomic or provide an OS
sandbox. Ordinary unguarded tasks retain their existing launch behavior.
Because each command acquires the current path, a replacement between preflight
and provider launch can select different directories for those commands.

## Root context repair

```text
delegate gateway repair-context [--profile ABS] [--json]
delegate gateway repair-context [--profile ABS] --confirm CHALLENGE --offline-cutover-confirmed [--json]
```

Native's task store owns the binding; the Gateway command coordinates the saved
SDK profile and runtime exclusions. Prepare is passive. It validates the saved
SDK token against historical infrastructure startup coordinates on the first
repair, then uses the immutable origin in `root.json` for later repairs. It scans
all canonical task and inspection requests, including requests without connector
context. Mixed root tokens, incomplete evidence and unresolved staging refuse
repair. `root_context` contains origin/accepted coordinates, request counts,
evidence digests and a challenge; it never returns the saved token, its wire guard
or secrets. A matching legacy context returns `root_context_not_required` without
enabling the feature.

Complete the retained installation handoff and all generation transitions
before confirmation. Stop the connector and nonparticipating writers/frontends.
`--offline-cutover-confirmed` attests that this is the intended original root,
no other live copy exists, and incompatible writers/frontends are stopped.
Confirmation takes the profile lock, checks inactive connector identity, retains
bootstrap exclusion, verifies live canonical root-wide quiescence and takes the
store maintenance exclusion nonblocking. The compatible daemon must be running
and ready for confirmation; its healthy current helper may remain running.
Follow the exact order and commands in
[operator recovery](cli-reference.md#root-context-repair).

Confirmation atomically publishes the optional `context_binding` and required
`root-context-binding-v1` feature in `root.json`. Older strict readers refuse
that feature; already-open old writers still require operator closure.
Registration, profile, credentials, canonical requests and outcome bytes are
preserved. The binding keeps its immutable origin token preimage and accepts
one exact current canonical path/device/inode. It refuses moved, replaced or
copied roots; a later device-number change needs another explicit repair with
the same path and inode. The attestation supplies the no-other-live-copy fact;
local evidence cannot establish it. After confirmation, run Gateway doctor and
restore the saved service only after it verifies the root match.

## Workspace-constrained selection

A connector explicitly approves an existing ancestor using its ordinary opaque
project token. It then asks Delegate to resolve an existing relative project:

```sh
delegate context inspect --root ABS --expected-workspace WORKSPACE_TOKEN --relative-project team/service --json
```

The returned `project_token` binds the same selected leaf used for workspace
validation. `workspace_constraint` is `{"token":"WORKSPACE_TOKEN",
"relative_project":"team/service"}`. V1 tokens retain physical identity; v2 tokens
retain the approved canonical path. An ancestor and leaf must use the same
representation. Changing files or creating another child does not change the
approval. Tokens remain
local data, not remote routing handles.

Fresh guarded dispatch and explicit admission recovery must supply the complete
constraint with `--expected-workspace` and `--relative-project`, in addition to
the existing task guards. Delegate freezes it in the canonical request and
receipt. Admission, recovery and the retained launch gate validate the approved
ancestor and selected project. Traversal and escaping symlinks fail. V1 rejects
ancestor replacement. V2 permits same-path replacement of ancestor and leaf,
but refuses any change to the frozen canonical project path.
These are validation and launch checks, not continuous filesystem ancestry
enforcement or an OS sandbox.

Passive observation, accepted result retrieval, cancellation and fencing compare
frozen guards without requiring the live workspace directory. Supplying the
workspace constraint requires an exact saved match. Legacy passive callers can
omit that optional constraint while still matching root, project, origin and
any supplied spec digest. Omission never authorizes a fresh dispatch or recovery
of a workspace-constrained task.

Native continuation inherits a path-bound predecessor's root, canonical project
and workspace constraint before reserving the successor. Supplied constraints
must match exactly. Local continuation does not copy Gateway-origin correlation
and is not included in Gateway origin inventory. A fresh Gateway caller keeps
its own origin; no continuation can strip or retarget the inherited constraint.
Local continuation uses Native's frozen constraint. Removing an SDK workspace
approval narrows Gateway routing; it does not revoke the local user's authority
to continue through Native directly.

Legacy JSON projections omit the additive workspace field even when history
contains dynamic tasks. New consumers opt in with
`--include-workspace-constraint`; supplying expected workspace flags also opts
in. This affects only the output clone, never canonical validation. Origin
inventory accepts smaller pages with `--limit 1` (valid range 1..200); callers
must follow `next_cursor` until `complete`. Pagination bounds the emitted entry
count, not the full canonical membership scan needed for completeness.

## Read-only filesystem observations

`workspace-filesystem-v1` is an executable capability reported by successful
public context inspection. It is not a new persisted root-required feature.
The connector maps public scope IDs to saved local approvals; raw local paths
and tokens stay local. New selection uses scope ID plus relative path, without
creating a permanent project pin. Linux automatically exposes whole `/tmp`;
macOS resolves user temp with trusted `/usr/bin/getconf DARWIN_USER_TEMP_DIR`
and includes shared `/tmp` (canonical `/private/tmp`), deduplicating canonical
paths. Discovery errors remain explicit.

```sh
printf '%s\n' '{}' | delegate filesystem scopes --request - --json
delegate filesystem list --root ABS --request /absolute/private/list.json --json
delegate filesystem read --root ABS --request /absolute/private/read.json --json
delegate filesystem inspect --root ABS --request /absolute/private/inspect.json --json
delegate filesystem git --root ABS --request /absolute/private/git.json --json
```

`--request` requires a strict JSON regular file or `-` for stdin, at most 1 MiB.
`--control-timeout` is positive and at most `30s` (default `30s`). Empty scope
discovery `{}` needs no Native root. Other operations require an existing root
and matching local `expected_root`. A workspace list request is:

```json
{"expected_root":"ROOT_TOKEN","scope":{"kind":"workspace","expected_workspace":"WORKSPACE_TOKEN"},"relative_path":".","limit":100}
```

Use a separate read request with `offset` and a decoded-byte `limit` up to 65536.
Lists return at most 100 entries per page. Follow the opaque `next_cursor` only
for the same selection and observed version; mutation can invalidate it. No
cursor state, root, task, approval or pin is created. Pass `expected_version`
when a later range must match an earlier observation.

Names and requested bytes are unfiltered, including hidden files, hard links,
non-UTF-8 names and binary files. Use standard padded base64
`relative_path_base64` instead of `relative_path` to round-trip arbitrary Unix
names. Never reconstruct a path from lossy display text. `read` returns
`data_base64` and optional UTF-8 `text`; special entries are inspectable but not
read as regular content. Symlink containment stays inside the same selected
root, not the union of approved scopes.

Git observations are rooted structural evidence: marker, linked-worktree file,
bare layout and HEAD shape. `candidate` and `partial` do not mean validated;
external metadata remains partial. This operation runs no Git command and makes
no definitive repository-validity claim. See [observation results](../skills/agent-integration/references/result-handling.md#filesystem-and-model-observations).

## Scoped model discovery

`scoped-model-discovery-v1` is a separate executable capability. The public form
uses an existing approved **path** workspace token supplied exactly as saved:

```sh
delegate models --root ABS --provider PROFILE --cwd CANONICAL_PROJECT --expected-root ROOT_TOKEN --expected-workspace-file /absolute/private/workspace-token --relative-project team/service --json
```

The three selection guards are required together. The token file is a regular
file containing the exact opaque token bytes, at most 1 MiB; it is not JSON and
has no added newline. Do not decode, regenerate or synthesize tokens. A supplied
`--cwd` must match the derived canonical project; omission derives it from the
selection. This new route rejects legacy physical approvals and accepts no raw
project token, origin or historical project digest.

Native saves the derived originless path `ConnectorContext` in the canonical
inspection request. The worker reacquires the selected path and starts each
provider command through the existing project-aware lifetime and child gate;
a conflicting cwd, missing project-aware lifetime or custody/finish failure
cannot yield accepted model facts. Same-path replacement remains supported.
Setup and process work use the same persisted absolute inspection deadline;
there is no renewed budget or second process owner. Ordinary unguarded `models`
keeps its existing behavior. Model discovery remains explicit, finite and
advisory; it may probe a provider and record inspection evidence without admitting
a provider task.

## Guarded task operations

```sh
delegate dispatch --id TASK --provider PROFILE --brief FILE --cwd ABS \
  --expected-root ROOT_TOKEN --expected-project PROJECT_TOKEN --origin ORIGIN_JSON --json
delegate resolve TASK --expected-root ROOT_TOKEN --expected-project PROJECT_TOKEN \
  --expected-origin ORIGIN_JSON --json
delegate resolve TASK --expected-root ROOT_TOKEN --expected-project PROJECT_TOKEN \
  --expected-origin ORIGIN_JSON --resume-admission --expected-spec-sha256 SHA --json
delegate resolve TASK --expected-root ROOT_TOKEN --expected-project PROJECT_TOKEN \
  --expected-origin ORIGIN_JSON --fence-if-absent --json
delegate cancel TASK --id CONTROL_ID --expected-root ROOT_TOKEN \
  --expected-project PROJECT_TOKEN --expected-origin ORIGIN_JSON --json
```

`status` and `collect` also accept the three task guards. A guard set is all or
nothing. Existing-task `resolve`, `status`, `collect`, `cancel`, and `result read`
also accept `--expected-spec-sha256`; it is checked against the canonical request
before task control, publication, or result bytes. Existing recovery requires it.
Dispatch rejects this flag before preparation. Guarded dispatch uses an explicit
provider and project; alternate routing, managed workspace selection and Team
selection are rejected. Guarded continuation has its own successor contract below.
Ordinary commands without guards preserve existing behavior.

`ORIGIN_JSON` is a strict JSON file, or `-` for stdin:

```json
{"gateway_id":"gateway","admission_generation":"generation","operation_id":"operation","principal_ref":"principal","gateway_request_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
```

The first four values contain 1..128 ASCII letters, digits, `.`, `_`, `:` or
`-`. Digests use 64 lowercase hex digits. Task and cancellation control IDs use
32 lowercase hex digits. Origin and context are frozen in the canonical request.
`connector.receipt` projects those values with `task_id`, `spec_sha256`,
`brief_sha256`, `root_token`, `project_token` and `execution_options`.
The spec digest identifies the actual canonical request bytes.

Origin additionally accepts optional `binding`: immutable opaque UTF-8 text,
without NUL, bounded by the existing 1 MiB control-record limit. The encoded
Origin input must also fit that limit. Native advertises executable capability
`origin-binding-v1`; it does not interpret Gateway's binding format or add a
root-required feature. Absent/empty binding preserves historical canonical bytes.
Once binding-bearing records exist, older strict readers cannot read them;
root-wide inventory and upgrade recovery can fail. Keep a binding-capable
installation for that root and use the supported upgrade handoff before replacing
writers. This is not a new root migration or permission to mix incompatible writers.

`execution_options` projects `provider`, `permission` (canonical request mode),
exact `budget_nanoseconds` (signed 64-bit positive nanoseconds), and requested
`model` and `effort` selections. Each selection is `{kind: "explicit", value:
"..."}` or `{kind: "native_default"}`. These report canonical requested
configuration, not a resolved native default. There are no briefs, project
paths or environment values in this projection. Before resuming, a connector
must compare the spec identity. Legacy correlations also compare the retained
requested options. A Gateway compact binding instead commits original caller
intent without reconstructing it from this receipt; bodyless existing-only
recovery still validates the exact canonical Native spec. Native owns execution
validity and defaults. Gateway and the
connector retain routing/authorization and resource limits; they add no provider,
model, effort, permission or budget policy ceiling.

Passive resolve never submits or repairs. `connector.resolution` is `existing`,
`absent`, `unresolved` or `permanently_refused`. Absence is not permission to
recreate an uncertain request. Explicit `--resume-admission` recovers only a
complete matching existing prepared request through the existing lifecycle
owner; it cannot fall back to new-task creation. A cancelled or refused task
still cannot start.

`--fence-if-absent` permanently refuses an absent ID under the same namespace
exclusion used by all task producers. A matching attributed preparation-only
partial task can also be fenced after proving no launch evidence. Unknown
partial state remains unresolved. The local operator-only action
`resolve TASK --fence-unattributed-partial --expected-root ROOT_TOKEN
--expected-project PROJECT_TOKEN` requires an explicit selected preparation-only
partial task and no guessed origin. Fencing preserves evidence and creates no
task outcome. Completeness and attribution are checked under the same exclusions
as the refusal commit: a complete prepared task stays `existing`, and explicit
unattributed recovery cannot erase recorded attribution. A supplied spec digest
must match the actual immutable request bytes, including on complete tasks.
Operator-only partial recovery with a supplied digest fails if those bytes
cannot be read, including an absent task or missing request; it creates no
refusal in that case. Ordinary `--fence-if-absent` can refuse a wholly absent ID
without claiming a request digest match: that refusal prohibits every later
request for the ID. Passive reads and identical retries verify the refusal file and
containing directories with sync and matching readback before reporting durable
refusal. Busy owners, missing locks and persistence failures provide no refusal
proof.

Cancellation uses the caller's stable control ID. Retrying the same ID does not
create another stop request; a lost first acknowledgment can be recovered by
passive closure observation.

A complete running task can hold the exclusions required by
`--fence-if-absent`. A busy refusal fence is therefore not a prerequisite for
guarded cancellation. Resolve the matching complete receipt passively, then
cancel with its original guards and stable control ID. Absent or incomplete
work still requires authoritative refusal before claiming it cannot launch.
When combining a native cancel response with a separate canonical observation,
match their nonempty root IDs, the requested task ID and the cancellation ID
before accepting stop evidence. Ordinary cancellation uses the common response
fields; it does not supply a connector receipt itself.

## Historical project guards

Existing-task observation, cancellation, result access and recovery can use
`--expected-project-sha256 SHA` instead of raw `--expected-project TOKEN`.
The SHA is over the exact saved opaque token bytes. Supply one representation,
not both; do not regenerate a token from its decoded path. This lets historical
controls compare the saved binding after the project disappears. It does not
select a fresh project or authorize fresh dispatch/scoped model discovery.
The live Native root and the original origin/spec guards still apply.

## Guarded Native continuation

The candidate supports an explicit successor ID and fresh successor origin:

```sh
delegate continue --root ABS --task PREDECESSOR --id SUCCESSOR --brief /absolute/brief.txt --expected-root ROOT_TOKEN --expected-project-sha256 PROJECT_TOKEN_SHA --expected-origin /absolute/private/predecessor-origin.json --expected-spec-sha256 PREDECESSOR_SPEC_SHA --origin /absolute/private/successor-origin.json --json
```

IDs are distinct 32-character lowercase hex values. Expected origin/spec/project
identify the predecessor; `--origin` identifies the fresh successor. Native
validates the predecessor and inherits its frozen project/workspace constraint
before reserving the successor. A predecessor spec digest is never reused as
the successor spec. Keep the same successor ID and both origin files across
uncertainty; observe/recover that operation instead of creating another task.
Normal local continuation keeps its existing behavior.

The Gateway candidate implements `prepare_continuation`, followed by its existing
`dispatch_task` confirmation. These Native flags and local tests do not prove
deployed cloud continuation parity. **Unverified:** paired process acceptance,
full installed-provider continuation and cloud-client acceptance.

Binding-present control output keeps full Outcome, receipts, descriptors/ranges,
closure, fresh stop and structured failure data in a 4 MiB response envelope.
Auxiliary fields and the human `failure.recovery_command` hint are omitted.
Overflow reports a fixed diagnostic with nonzero exit and no partial JSON.
Ordinary and schedule limits stay unchanged. Native retains top-level
`execution_assurance` and `result_range` for its own CLI consumers; the current
Gateway strict DTO does not accept them. Gateway does not request the assurance
flag and uses guarded `connector.result_range` instead. Inventory retains its
existing canonical bytes and membership hashing; it does not infer this output
projection from returned entries.

## Execution closure and quota release

For an opt-in diagnostic of the actual per-task lifetime backend, inspect the
capability and then the task:

```sh
delegate context inspect --root ABS --include-execution-assurance --json
delegate status TASK --root ABS --include-execution-assurance --json
```

Context reports `connector.execution_assurance_feature` as
`task-lifetime-assurance-v1`. Task status adds `execution_assurance` only when
requested. A published guardian registration reports `state: "registered"`,
`backend: "cgroup-v2"` with `assurance: "exact"`, or `backend: "process-group"`
with `assurance: "cooperative"`. `registration_sha256` identifies the canonical
registration data. It is not an executable fingerprint. A clean missing
registration reports `state: "unregistered"` without an inferred backend;
malformed, foreign or unreadable evidence fails explicitly. Default JSON remains
unchanged. This diagnostic uses the actual immutable guardian registration,
not service defaults, and proves neither task termination nor execution closure.

An accepted committed or rejected outcome proves publication, not complete
execution lifetime closure. A worker can publish before its guardian records the
exact worker exit. Generic `liveness: ended`, queue-row completion, a stop
request or an acknowledgment is also insufficient to release a reservation.
Keep a result descriptor even while execution closure remains pending.

Passive task responses can add `connector.execution_closure`:

```json
{"kind":"registered-native","reference_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
```

Kinds are `registered-native`, `registered-recovered`, `legacy-previous-boot`,
`unregistered-recovered` and `never-started`. The first four use the existing
canonical lifetime-closure resolver. Never-started proof requires complete
unlaunched evidence and a persisted stop/refusal that prohibits a later start.
The digest references the actual immutable owner record bytes selected by that
resolver, after owner exclusion, file/ancestor sync and byte-identical readback.
It is not a digest of a summary or a new persisted task state.

Without proof, `execution_closure_pending` can be `owner_busy`, `missing_owner`,
`partial_evidence` or `closure_unproven`. These reasons are observations, not
closure. Read/sync/cleanup errors remain errors; they do not become pending
success. A failed connector operation removes closure, permanent-refusal,
result-range and origin readiness/completeness proofs from its response.
Release quota only after this canonical closure proof, a matching fresh
successful `stop.terminated`, or verified permanent namespace refusal. Never
release from an accepted outcome alone. Bound passive polling and retry busy
observations; observation must not prevent an active local cancellation.
Canonical closure verification briefly borrows existing task exclusions. A
competing collection or publication can therefore report retryable busy; a
busy observation supplies no closure proof.

## Verified result ranges

Ordinary local tasks also support [unguarded bounded reads](../skills/agent-integration/references/command-reference.md#result-read).
That route refuses any task with connector origin or context, including
context-only successors, with exit `2`; the guarded form and receipt below
remain required for connector tasks.

```sh
delegate result read TASK --expected-root ROOT_TOKEN --expected-project PROJECT_TOKEN \
  --expected-origin ORIGIN_JSON --expected-descriptor DESCRIPTOR_JSON \
  --offset 0 --limit 65536 --json
```

The descriptor is the original accepted committed payload descriptor. JSON
files may use `-` for stdin, but origin and descriptor cannot both use stdin.
`result_range` includes the unchanged `descriptor`, `offset`, `data_base64`,
`next_offset` and `eof`. Offset may equal the payload length for an empty EOF
range; otherwise it must be within the payload. The limit is 1..65536
bytes. The reader validates canonical outcome/seal binding and all sealed raw
streams, then scans the complete payload length and digest before releasing
any selected bytes. Hidden-tail corruption and cancellation return no range.
Memory for payload selection is bounded; verification still reads the whole
result on every request.
Each range also revalidates the sealed raw evidence. Total retrieval work grows
with both raw evidence and payload size for every requested page.

Canonical validators can also scan the whole accepted payload or sealed raw
streams during guarded dispatch, resolve/status, collect, cancel and existing
resume. One CLI invocation can perform several serial scans. An integration's
full-scan concurrency limit must count these task-control invocations too;
limiting only explicit result ranges does not bound all verification scans.

New connector commands and guarded controls use a finite `--control-timeout`
(default `30s`). JSON paths must be regular files; pipe/socket stdin waits are
cancellable without closing borrowed stdin. `--expected-spec-sha256` belongs to
existing-task controls and is rejected on dispatch before preparation. A whole-result verification timeout reports
`failure.code: result_verification_timeout`, never partial verified bytes.
For a large result, retrieve locally or explicitly configure compatible finite
CLI/client/HTTP deadlines; an outer timeout does not prove the operation failed.

`connector.creation_attempt: not_attempted` describes only this invocation's
canonical task-creation commit. It does not classify a task left by an earlier
attempt. Busy creation can return exit `3` with
`failure.code: creation_not_attempted` and `next_action: retry_later`.
Preparation remains outside the commit exclusion. A refusal permanently blocks
ordinary task admission and launch for its ID. A closed origin generation blocks
new canonical creation; existing tasks retain Delegate's execution authority.
Neither proves no helper activity, cost or outstanding cleanup. An advisory
early rejection is optional and cannot replace the canonical commit checks.

## Origin recovery barrier

```sh
delegate origin barrier --gateway-id G --recovery-id R --generation NEXT \
  --expected-generation PREVIOUS --expected-root ROOT_TOKEN --json
delegate origin inventory --gateway-id G --generation NEXT --expected-root ROOT_TOKEN \
  --limit 100 --json
delegate origin reopen --gateway-id G --recovery-id R --generation NEXT \
  --expected-root ROOT_TOKEN --completeness-receipt RECEIPT --json
```

First registration requires the operator's explicit generation and recovery ID;
omit `--expected-generation` only when no origin is registered. A barrier closes
admission and advances to a fresh generation under a short root-wide namespace
mutation exclusion. Creation, absent-ID fencing and partial refusal hold its shared side through
their membership changes; barrier and reopen hold its exclusive side. Each
operation retains its existing maintenance reference and inner ownership order.
Contention returns busy without waiting indefinitely. A live task's longer
maintenance ownership does not by itself block this transition. All producers
check current origin generation before creating fragments.
The barrier excludes new canonical task creation; it does not revoke execution authority
for an already canonical prepared task. Existing-task recovery still requires
its immutable request guards and the ordinary stop/refusal and admission owners.
Retry the same explicit transition after contention or an uncertain reply.

Inventory derives membership from canonical tasks and refusals. Existing-task
entries include an optional `receipt`, with the same fields as the task receipt.
Its project token and execution options derive from the frozen canonical request,
even if the project directory has since disappeared or been replaced. Refusal
entries omit request receipts and spec/brief identity; their permanent refusal
is the membership fact. When `task.json` is initially absent, classification
borrows the existing namespace exclusion and rereads the evidence. An active
creator holding that exclusion returns busy (exit 3), rather than a misleading
permanent evidence fault. An already readable request retains the direct
classification path.
Missing or unmapped project attribution remains ownerless for administrative
recovery; consumers must never substitute the project used to query inventory.
Raw CLI receipt tokens remain local to the connector and require the same wire
hashing described above. Pages are sorted
by task ID, with limit 1..200 and `--cursor` from `next_cursor`. Existing-task entries retain
immutable origin/spec/brief identity; they do not copy mutable lifecycle state.
Require the same `membership_sha256` across every accumulated page; discard and
restart the scan if it changes. A concurrent local fence can add a refusal,
including an ID before the current cursor, while admission is closed. Only
`complete: true` has a `completeness_receipt`. Reopen verifies that receipt
and unchanged complete membership under namespace mutation exclusion. Changed or unknown
evidence fails closed. Page output and control duration are bounded; scanning
large histories still consumes work proportional to canonical history size.
Reopen holds the mutation exclusion through that scan, so its retryable busy
window for new creation and refusals also grows with history.
