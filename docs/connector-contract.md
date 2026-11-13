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

Tokens identify the actual root/project directory, including physical identity.
The CLI project token encodes a local canonical path. Keep raw CLI tokens local
to the connector; do not put them in remote messages, logs or a registry. An
external gateway can use `ctx1_` plus SHA256 of the local token as its wire
identity, while its connector checks that hash and supplies the pinned actual
CLI token locally. This encoding is an external connector policy, not another
Delegate context identity. Replacing a root invalidates every control guard.
Replacing or removing a project invalidates new admission and existing-task
resume. Observation, result retrieval, cancellation and fencing retain the live
root guard and compare the exact saved project token when a request exists;
they do not require its workspace to remain present. For an absent request,
the echoed project token is validated input, not a verified project receipt.
No directory is automatically rebound to a replacement.

Guarded execution retains the selected project directory through the existing
runner lease. Preflight commands for that project and the provider
child borrow it through the authenticated launch gate; after authorization the
child checks the directory identity, changes directory through its descriptor
and closes the inherited descriptor before provider execution. A detected
parent-side mismatch remains a refusal even if the pathname is restored before
sealing. A child gate refusal prevents provider execution and accepted output.
These checks protect the initial working directory and observed mismatches;
they do not make later third-party absolute path access atomic or provide an OS
sandbox. Ordinary unguarded tasks retain their existing launch behavior.

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
Dispatch rejects this flag before preparation. Guarded dispatch uses an explicit provider and project; alternate
routing, continuation, managed workspace selection and Team selection are
rejected. Ordinary commands without guards preserve existing behavior.

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

`execution_options` projects `provider`, `permission` (canonical request mode),
exact `budget_nanoseconds` (signed 64-bit positive nanoseconds), and requested
`model` and `effort` selections. Each selection is `{kind: "explicit", value:
"..."}` or `{kind: "native_default"}`. These report canonical requested
configuration, not a resolved native default. There are no briefs, project
paths or environment values in this projection. Before resuming, a connector
must compare the spec identity and these options against its current local
provider/permission/budget allowance.

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

## Execution closure and quota release

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
