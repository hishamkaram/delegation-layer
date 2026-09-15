# Supervised native metadata inspection

Status: implementation in progress; not integrated or certified. This extends the
Claude integration in EXECUTION-PLAN.md. The first private release still requires
all three providers. No native metadata success is a provider acceptance receipt.

Host qualification update: the user switched back to Team and requested that
work continue with that login. Team qualification remains pending. The earlier
blanket freeze requirement exceeded the agreed threat model: EXECUTION-PLAN.md
excludes hostile administrator replacement between validation and startup and
requires preflight source/drift checks. A fresh remote check does not atomically
bind the native child's later reload; that limitation must be explicit rather
than described as a snapshot guarantee.

Compare a core-owned, supervised first-party policy fetch plus local/cache
validation against a native diagnostic-only probe. The first must preserve
transient credential handling and inspect every fallback source; the second is
acceptable only if source evidence proves it cannot execute unverified policy
and reports enough evidence. Personal-only qualification no longer satisfies the
requested host scope. Do not bypass managed policy or silently accept unknown
sources. Runtime containment and live acceptance remain mandatory. Anthropic's
[delivery and cache documentation](https://code.claude.com/docs/en/server-managed-settings)
confirms independent startup fetch, caching, and session refresh behavior.

The private personal-account receipt is
`metadata-probe/run-20260914T184106Z-0eb4acbfd0d1/receipt.json`, SHA-256
`3aa1bd5a8889488d4e70e957a8ae309c807aa1ce8012912c9c725cb696e74e7a`.
It records native exit zero, clean shutdown, zero provider turns, no direct
signals, no retained native stdout/stderr, and a clean synthetic-sentinel scan.
This is a feasibility receipt, not production crash-ownership or certification
proof. Final admission and launch must independently recheck current metadata.

## Problem and evidence

Claude 2.1.270 reads OAuth through the native macOS `security` executable. An
exact service/account Security-framework query with UI disabled returns
`errSecAuthFailed` on this host. An attributes-only query can establish that the
separate legacy API-key entry is absent, but cannot supply OAuth expiry or plan.
A file fallback, cached settings, or display-account information does not prove
the selected native credential backend. Safe mode retains managed hooks, so
checking emitted init events after startup cannot replace pre-admission policy
verification. See CLAUDE-ACCEPTANCE.md and the private source/probe receipts.

The existing control-command Pending object is in-memory; it cannot own a helper
after the dispatcher exits. A short normal runtime is not a timeout guarantee.
Pueue 4.0.4 owns ordinary inherited process groups; its
[Unix implementation and tests](https://github.com/Nukesor/pueue/blob/0d10739a6431bd853195dc7730056fce79faeee1/pueue/src/process_helper/unix.rs)
cover child/grandchild stopping. A missing daemon handle or escaped descendant
still cannot be treated as observed termination. The implementation must retain
that existing unknown-state limitation.

## Alternatives and decision

| Alternative | Result |
| --- | --- |
| Direct in-process credential read | Smallest boundary, but the exact no-UI query is denied on this host. |
| File/cache/auth-status-only proof | Cannot establish selected credentials and expiry; rejected. |
| Change Keychain ACL, copy credentials, or change login | Violates the native-login and settings constraints; rejected. |
| Extract the one-shot control-command owner | Useful primitive, but loses its owner on dispatcher exit; insufficient alone. |
| Supervised inspection at admission and at final preflight | Durable ownership, but adds nested scheduling, another uncertain add, receipts, and stop paths at the paid-start boundary. |
| Supervised admission worker plus core-owned final inspection inside the existing supervised runner | Selected: durable ownership at admission, existing supervisor lifetime at final preflight, no nested queue. Requires explicit deadline/start arbitration. |

Use a two-stage provider preparation contract. A post-meta inspection flag was
considered and rejected: it would admit a task before policy was known and omit
native facts from the immutable effective-policy digest. An injected resolver
inside the existing preparer would hide supervision and lifecycle effects inside
an operation currently treated as a finite profile lookup. An explicit candidate
and finalizer make the effect boundary reviewable.

## Team remote-policy observation

The selected Team baseline requires fresh empty server-managed settings and a
compatible local/cache policy snapshot. The supervised private experiment on
2026-09-14 at 18:55 UTC returned HTTP 404 from the same first-party endpoint
used by CLI 2.1.270; its native loader treats 204/404 as successful empty
settings. This establishes feasibility, not effective-policy certification.
The receipt is `metadata-probe/run-20260914T185543Z-2156980194ed/receipt.json`
(SHA-256 `8dc9e7aaf30d84463081277396ef99bf75e6cddf4e398b2e81da7562ebd9e850`),
with `remote-policy-observation.json` SHA-256
`6272c81741d0f41a34e4355f603ad0e82b3ab89fa04ee77b1dde78394cf797f1`.
The base probe's `unsupported_plan` remains its old personal-only classification;
the separate remote observation records the actual Team endpoint result.

| Approach | Assessment |
| --- | --- |
| Require personal OAuth | Simpler eligibility proof, but no longer meets the requested Team host scope. |
| Require an atomic remote snapshot freeze | Unsupported by the CLI and stronger than the agreed threat model; do not claim this guarantee. |
| Inspect through `claude doctor` | Rejected as the initial proof: Commander preAction executes managed helper selection before the doctor handler. It also lacks full policy bytes. |
| Read the local cache alone | Rejected: it cannot establish the server's current policy. |
| Core-owned supervised first-party GET, plus all local/cache checks | Selected for the compatible Team baseline; fresh checks at admission and final preflight, with the native reload timing limit explicit. |

The optional network effect belongs to the core. Extend the compiled inspection
description with one fixed HTTPS GET endpoint and fixed nonsecret headers; a
pure callback derives the Authorization value from transient native bytes. Core
owns the HTTP client, credentials-bearing header lifetime, TLS verification,
response limit, deadline, and cleanup. Another pure callback projects native and
remote response bytes into fixed nonsecret facts. Callbacks never perform I/O.
This is one optional effect after the native read, not an arbitrary workflow DSL.
No request can supply a URL, header, redirect target, credential, or command.

For this baseline, use the pinned first-party endpoint only, verified TLS, no
redirects, no ambient proxy/TLS overrides, no automatic refresh or retry, a
bounded response, and the same absolute inspection deadline. Never put
credentials in argv, persistence, hashes, errors, debug output, or receipt fields.
A failure cannot be replaced with a passing cached observation. Before launch,
validate the native cache and every other applicable managed source independently
because the child's own fetch can fail and use its cache. Nonempty or unknown
remote mechanisms remain unsupported until separately implemented and tested.

The native child may fetch again after preflight and can refresh policies during
its run. The layer detects source drift at its checkpoints; it does not freeze
administrator-controlled policy. This is the same documented administrator
replacement limit in EXECUTION-PLAN.md, not permission to bypass organization
policy. Required tests include drift, fallback caches, malformed/oversized
responses, redirects, TLS/fetch failures, expired authentication, projection
failures, late responses after deadline, and zero ordinary admission on refusal.

### Fixed native storage backend

The pinned binary also contains a StorageV5 latch. Without an explicit choice,
cached or remote GrowthBook evaluation can select another settings/cache backend;
reading direct files alone does not prove which backend native startup will use.
Comparing the alternatives, inferring the latch from direct-file absence is
insufficient, and querying `doctor` remains unsafe before managed-policy proof.
The selected profile supplies invocation-local `CLAUDE_CODE_HOVER_REST=0`.

This is a verified code path in this exact binary, not a claim of a stable public
Claude API. The environment schema parses `0` as false. Startup pins the latch
before OAuth priming and managed-policy loading, and the latch accepts only its
first value. Later feature evaluation cannot repin it. OAuth remains Keychain
first, using the same service and account; the control changes the plaintext
fallback/settings backend, not the Keychain identity or policy endpoint. The
profile therefore also requires the direct plaintext credential fallback to be
absent, verified through file metadata without reading or hashing credentials.
Managed files and the direct remote-policy caches remain independently checked.

The provider rejects conflicting ambient values and supplies exactly one fixed
control in its compiled environment. Its definition digest binds that control.
No global feature flag, credential, login, Keychain ACL, or administrator policy
is edited. Source evidence is retained privately in `storage-backend-evidence.json`
(SHA-256 `912b36bcdbde718d49c8167a6e91e0ea3fed49c76767a159cf3fa1ee375c5fca`)
with nine exact byte ranges and per-slice hashes under the same inspected runtime
SHA-256. Backend controls and fallback refusal still require live qualification.

## Credential fallback decision: strict profile retained

The user selected **keep the strict guarantee and pause Claude live qualification**.
The checkpoint-only proposal is not approved and must not be implemented. No
additional Claude live attempt is authorized under the present fallback state.
The current absence requirement remains authoritative. Real qualification found
`~/.claude/.credentials.json` present and correctly refused before helper or
model launch. Its contents have not been read, copied, or hashed.

The pinned composite store (`vn`, around byte 166441000; fallback implementation
166438590–166447000) returns non-null Keychain JSON first and reads the plaintext
fallback on missing/null/error results. The StorageV5 pin does not disable this
OAuth fallback. Native `claude.ai`/`apiKeySource` labels do not distinguish the
selected store.

The alternatives considered were:

| Choice | Guarantee and effect |
|---|---|
| Preserve the current profile | A fallback file cannot become an unchecked credential source. Live qualification stays blocked on this environment; no login or file is changed. |
| Adopt a separately revised checkpoint profile | Require valid unexpired Keychain OAuth and applicable remote-policy proof at admission and immediately before launch. Treat the file as inactive at those checkpoints and never read it. This permits the existing login, but a later native Keychain error can activate unchecked fallback credentials; the layer cannot claim to prevent that path. |

The unselected second choice is a narrower guarantee, not proof that fallback is disabled.
It would require explicit acceptance of that boundary, a new profile revision,
updated certification evidence, and regressions for every failed/null/malformed
Keychain check. No fabricated digest may represent unread credential content.
Production would remain unavailable until both live turns, replay, review, and
CI pass. The existing profile has not been relaxed.

A multi-source credential inspection extension was also considered. It adds
secret-input handling, authorization checks for potentially distinct accounts,
and supervision interfaces, while still not pinning which source the native
child later selects. It is not proposed for this handoff.

## Consumer contract

`internal/provider` owns the candidate value consumed by the app. The same
immutable catalog supplies native inspection definitions to the app and worker.
There is no second mutable registry, provider-specific switch in the core,
request-supplied command, or provider process capability.

A candidate carries:

- the validated static launch inputs and writable roots;
- an optional compiled inspection definition, identified by revision and digest;
- a pure finalizer taking the immutable request and bounded nonsecret facts and
  returning the existing PreparedProfile.

An inspection definition supplies a fixed native command plan, bounded output
limit, and pure projection of transient output into canonical nonsecret facts.
It has no process, pipe, stop, store, or publication authority. The app/execution
core owns those resources. Providers that need no inspection use a ready-candidate
wrapper around their existing PreparedProfile; keep one catalog contract.

Use existing PolicySourceDigest values for normalized fact digests when they
express the binding. Do not add a persisted schema field merely to repeat those
values. The actual nonsecret proof is retained in the inspection journal; its
canonical digest, probe revision, and helper/runtime identity enter the effective
policy. Observation timestamps do not enter the comparable policy identity.

Claude projects only the supported plan enum, expiry, and the required inference
scope predicate. Never return arbitrary scope strings, tokens, credential hashes,
or unrelated stored fields. Legacy API-key absence and managed-source validation
remain independent prerequisites. Unknown policy is unavailable.

## Admission and immutable records

### Implementation interfaces

The implementation uses these consumer-owned boundaries. The value types and
ready-provider wrapper exist in `internal/provider/candidate.go`. The catalog
uses the single candidate hook, preserves certified finalization, and refuses
inspection through its compatibility preparation path. Claude now supplies an
inspection candidate and a pure Team/Pro/Max finalizer. Admission and worker
integration remain pending:

```go
// internal/provider: compiled policy descriptions and pure interpretation.
type PrepareCandidate func(task.TaskRecord) (ProfileCandidate, error)
type ProfileCandidate struct {
    Directory     string
    WritableRoots []string
    Inspection    *InspectionDefinition
    Finalize      func(facts json.RawMessage, now time.Time) (PreparedProfile, error)
}
type InspectionDefinition struct {
    Revision         string
    Executable       string
    ExecutableSHA256 string
    Arguments        []string
    Directory        string
    Environment      []string
    OutputLimit      int64
    Project          func(stdout []byte) (json.RawMessage, error)
    Remote           *HTTPInspectionDefinition
}
type HTTPInspectionDefinition struct {
    URL           string
    Headers       map[string]string // fixed nonsecret protocol fields
    Authorization func(native []byte) (value string, required bool, err error)
    Project       func(native []byte, status int, body []byte) (json.RawMessage, error)
}
```

`Project` is pure and sees stdout only after a successful native exit with no
capture failure or overflow. Stderr is drained and discarded. It returns a
bounded, strictly validated canonical JSON object of nonsecret facts. Its error
and panic values never cross the core boundary. Claude's schema allows only
the supported plan enum, integer expiry, and inference-scope boolean; decoding
or projection errors produce a fixed core reason code. Facts are not arbitrary
native JSON, and native bytes are never digested. A remote definition uses its own projector instead of the native-only one.
Its authorization callback can skip the GET for a proven ineligible personal
plan; its projector receives status zero in that case and must validate that
condition. `InspectionDefinition.Snapshot` validates and clones the command and
remote description, rejects literal credential headers and callback ambiguity,
and hashes the serializable fields and revision, excluding function values.
The bound worker executable digest pins the compiled projector implementation.

The catalog's registration preparation hook becomes `PrepareCandidate`. A
`ReadyCandidate` wrapper adapts the existing agy/Codex profile factories and
rejects nonempty supplied facts. Catalog finalization retains the existing
certified predicate/writer checks and artifact normalization. A legacy
`Catalog.Prepare` compatibility entry point may finalize only candidates without
inspection; it must refuse an inspection candidate, never bypass its proof.
The app uses the explicit candidate path for admission and runner checks.

`ProfileCandidate.ValidateStatePlacement` uses the same canonical overlap
validator as `PreparedProfile`. Core clones definition slices before computing
the digest or running a helper. No adapter receives a journal, supervisor,
context cancellation function, writer, or start capability. Native argv and
environment are reconstructed through the same catalog in the worker and must
match the admission definition digest before execution.

The app owns admission coordination. A shared `internal/inspection` package
owns inspection record validation, projection containment, and the worker's
transient process lifetime. It may depend on provider value descriptions,
execution timing primitives, pueue, and taskdir; those packages must not import
inspection. In particular execution must not import provider or inspection.

### Journal schema and replay key

Use one operation directory, `inspections/<task-id>/`, with the request digest
inside its immutable request record. A digest subdirectory is deliberately not
used: changing a request under the same task ID must not create another helper
operation. Record basenames are fixed constants, never user-supplied paths.

| Record | Required binding and meaning |
| --- | --- |
| `request.json` | schema version, RootID, TaskID, full normalized TaskRecord, request SHA-256, definition revision/digest, helper and worker executable paths/digests, supervisor binding, exact group/label, absolute deadline |
| `submission.json` | request-record digest and create-once add intent; existence consumes submission authority even if no reply is available |
| `receipt.json` | request-record digest and exact observed supervisor numeric target; a client reply alone is not worker completion |
| `start.json` | request-record digest and create-once worker start intent; an existing guard never permits another native invocation |
| `result.json` | request-record digest, fixed reason, validated nonsecret facts when available; no exception/native message strings |
| `completion.json` | request-record digest, result-record digest, helper exit classification, completed timestamp; eligible only with matching successful supervisor completion |
| `worker-observation.json` | request/completion digests and exact numeric target; written by admission only after independent successful supervisor completion |
| `stop-request.json` | request-record digest, deadline cause, exact observed target and supervisor binding |
| `stop-reply.json` | matching stop-request digest and bounded acknowledged/unknown classification |
| `stop-observation.json` | matching request/target and independently observed terminal state |

Canonical record hashing uses the existing task JSON rules. Every read checks
the operation identity, strict schema, size bounds, and referenced hashes.
Admission inspection has a 20-second absolute deadline, including queue time.
Both native streams are capped at one MiB, with overflow drained and rejected;
projected facts are capped at four KiB. The ordinary task budget independently
bounds final preflight. These are shared core limits, not provider defaults or
user-supplied timeout commands.
Immutable create-once records accept an existing identical winner; conflicting
bytes are an evidence fault. A submission claim is different: an existing guard
never grants another add even if its bytes match. A stable operation lock
serializes claim acquisition; it is not held across external commands or waits.
Under that lock, read an existing request before allocating a deadline. A
matching replay retains the winner's original deadline and bindings, rather than
minting a fresh operation timestamp or extending the time budget. Changed
request bytes or static helper/worker/supervisor bindings are conflicts. Only a
missing request permits construction of the initial immutable request record.
The worker also uses a distinct create-once start guard, so duplicate worker
invocation cannot repeat the native read. This guard is consumed before helper
start, including when the helper cannot start.

Group provisioning has a separate root-scoped immutable intent and observation
record, bound to the supervisor configuration. The group name derives from the
full RootID and has parallelism one. The app verifies the exact
group settings after creation or an uncertain reply. An unresolved intent never
authorizes another mutation or any task submission. A conflicting existing
group is refused. Group lifecycle records cannot be reused under a changed
daemon binding.

1. Build and normalize the request, validate provider options and static inputs,
   and check state placement against workspace and provider-writable roots.
2. Compute the canonical request digest. Resolve the selected supervisor binding
   and the root-scoped inspection group only after static validation succeeds.
3. Create inspection records under the state root, outside `tasks/<task-id>`.
   Bind RootID, TaskID, request digest, operation identity, inspection definition,
   relevant nonsecret environment, helper/worker/runtime hashes, supervisor
   binding, exact group, and absolute deadline. No MetaSHA256 exists yet.
4. Persist a one-use submission guard before the external add. Submit only the
   fixed worker with validated root/task/operation identifiers and `--escape`.
   The supervisor receives a minimal nonsecret environment: pueue captures the
   add environment, so forwarding unrelated ambient authentication is forbidden.
5. Reconcile exact label, group, binding, and numeric target. An uncertain add
   never grants another add. Repeated dispatch of the same request reattaches to
   its recorded operation; it does not launch another inspection implicitly.
6. Require actual worker completion, a valid bounded result, and eligible facts.
   Finalize and validate the profile, including its normalized fact digest.
7. Only now create ordinary task, brief, and meta records and permit paid
   submission. Every policy refusal leaves zero ordinary task records and zero
   paid launches. Different request bytes cannot reuse an existing operation.

The inspection journal contains request, submission guard, receipt, result,
completion, and distinct stop-request/reply/observation records. Use a small
root-control-directory wrapper around taskdir's existing rooted access,
maintenance locking, staged create-once writes, barriers, and bounded readers.
Do not copy the durability algorithm or disguise control work as an AI provider.
A result payload without its matching completion is not an eligible proof.

`taskdir.ControlDir` supplies the storage primitive: `OpenInspection(taskID,
create)`, `OpenInspectionGroup(create)`, bounded `WithLock(func(*ControlTransaction) error)`, strict `Read`, and
canonical `Put`. `Put` returns whether this call created the record. Guard
consumers require both `created == true` and `err == nil`; an identical winner
returns false and cannot grant authority. The operation-specific record schemas,
one-use submission/start permits, worker, and reconciliation still require
integration above this primitive. Use the scoped transaction for record access
inside the callback. Closing the outer handle rejects new standalone calls and
waits for active transactions; transaction work must finish before its locks are
released. Retained transaction handles cannot authorize later access.

Provision or verify only an exact root-scoped pueue inspection group through the
bound daemon. Persist mutation intent and reconcile uncertain group creation;
never silently override an existing group's settings. No global auth or provider
configuration changes are permitted. An unavailable group is an unavailable
inspection, not a passing skip.

## Worker and sensitive data lifetime

The pueue-owned worker validates all immutable bindings and checks the absolute
deadline before starting a helper. An operation that expires while queued must
never start that helper when eventually scheduled.

Core uses explicit argv, finite stdin, bounded private stdout/stderr capture,
concurrent draining, and owned Start/Wait. Excess bytes are discarded while the
pipes continue draining. Only the pure projection sees credential-bearing bytes;
pueue-visible streams and all records/errors receive fixed reason codes or
validated nonsecret facts. Clear owned byte buffers after projection and discard
references; do not claim that every temporary Go heap copy can be erased.
Projection failures and panics must not print native payloads or panic values.

The worker owns a finite deadline using the existing budget/stop model: persist
stop intent, reconcile one exact supervisor job, and request its stop through
pueue. No direct signals, process-group manipulation, CommandContext, WaitDelay,
or retry after uncertainty. Requested, acknowledged, and observed termination
remain distinct. Worker/daemon failure or a missing handle leaves unknown state
and blocks paid submission; it does not prove descendant termination.

`execution.PreflightScope` lends the existing budget owner's context and timing
gate to a synchronous inspection callback. Its context exposes that owner's
absolute deadline and is canceled by the same observer, without a second timer.
The scope is revoked when the callback returns. `execution.RunSupervised` uses
the same budget owner for the admission worker's persisted absolute deadline;
queueing never renews it. An expired queued worker refuses before helper startup
when pueue schedules it. The dispatcher does not install a second stop owner for
queued jobs: if the group cannot progress, that queue entry remains pending.
Expiry proves zero helper starts, not removal or process termination. Acceptance
must release its bounded queue blocker and observe the expired worker finish;
it cannot report queue cleanup merely because the dispatch deadline elapsed.
`inspection.Inspect` validates the compiled helper
digest, drains private native output, invokes the optional projection/fetch,
clears owned native bytes, and rechecks the timing gate before returning facts.
The worker still needs its durable journal permit and exact supervisor binding.

Supervisor mutation interfaces distinguish group creation, worker submission,
and worker stopping. Each permit carries its immutable saved root/task/target
and supervisor binding; the consumer checks those fields before observation or
mutation. A permit for one action cannot authorize another action.

Admission observation contexts also bound journal lock waits and record writes.
The durable staging path derives its lock timeout from the supplied context and
refuses already-canceled writes before staging. Once staging starts, barrier and
cleanup work remain joined; cancellation cannot turn a partially persisted guard
into permission to retry. Worker stop acknowledgments use the separate stop
observation lifetime, not the expired native-work context.

The result journal keeps facts as a JSON object, including an empty object for
an unavailable result. This preserves the existing strict record decoder's
required-field validation without treating raw JSON bytes as a protocol array.
Result and completion are separate create-once writes. A result alone remains
ineligible, and a completed local result still requires independently observed
successful completion of the exact supervisor job.

## Existing task and final prelaunch checks

Early runner profile checks read the recorded nonsecret proof and repeat static
validation without spawning a helper. Missing, expired, malformed, or mismatched
proof cannot rewrite immutable metadata or trigger a new admission operation.
Collection, status, logs, and discovery remain observational.

At the last execution preflight, the core performs one fresh local inspection
inside the already pueue-owned runner and its armed deadline. Re-finalize the
profile and compare it with admission metadata. Changed facts or source digests
refuse the paid start. The one-use execution permit prevents repeated final
inspection through a repeated runner invocation.

Add explicit start authorization coordinated with the budget owner. If expiry
or stop intent wins first, a late eligible helper result cannot call provider
Start. If authorization wins first, later expiry uses normal supervisor stopping.
Do not hold the arbitration mutex across a potentially blocking external Start
or a supervisor call: authorize under the lock, then perform the owned operation.
Check the clock at authorization as well as the timer-observed state. The gate
must not claim actual start or termination; those keep their existing receipts.

## File ownership and implementation sequence

1. Root freezes the provider candidate/finalizer and inspection-definition
   signatures, journal record schema, group binding, and start-gate contract.
2. A bounded worker implements the root control journal and inspection records,
   reusing taskdir primitives, with fault and crash-window tests.
3. A bounded worker implements core transient capture, projection isolation,
   worker lifetime, and budget/start arbitration with injected deterministic
   operations. Provider packages own no processes.
4. Root integrates the catalog, admission/existing-task paths, pueue group and
   operation reconciliation, internal worker entry point, and Claude finalizer.
5. Root updates contributor guidance and the live harness, then runs all gates,
   direct review/fix cycles, and the normal private PR/CI/squash lifecycle.

The exact native worker may be an internal mode of the existing shipped runner;
do not add a third shipped binary without a demonstrated need. The ordinary
provider task schema, historical interpreters, and existing certifications must
remain compatible.

## Required proof before acceptance

- Static refusal and every failed/unknown inspection create no ordinary task and
  perform no paid submission; repeated/uncertain add cannot duplicate a helper.
- Wrong group, duplicate label, changed binding, malformed status, and changed
  request/runtime/environment refuse the operation.
- Queued expiry performs zero helper starts. Running expiry routes an exact
  supervisor stop; delayed replies retain unknown rather than inventing exit.
- Reader failures, overflow, malformed native JSON, projection panic, and failed
  result/completion writes never produce eligible proof.
- Sentinel secrets in stdout, stderr, arbitrary fields, and projection errors
  never appear in pueue output, records, public errors, or committed fixtures.
- Crash windows around submission, result, and completion retain one-use intent
  and never promote an incomplete payload.
- Changed final facts refuse paid start. A blocked final helper followed by
  expiry and late success never starts the provider. A blocked Start must not
  prevent budget stop preparation after authorization.
- Existing Codex/agy behavior, historical collection, and zero-launch replay
  remain covered by the normal unit/race, protocol, and supervisor gates.
- Isolated real pueue proves worker lifetime, ordinary inherited helper stopping,
  caller reattachment, natural completion, and daemon shutdown. This fixture is
  not a substitute for the two real Claude provider turns.
- Full quality gate, both Claude live turns, direct Codex review with no open
  actionable findings, private PR CI, squash merge, main CI, and retirement are
  required before declaring this integration complete.
