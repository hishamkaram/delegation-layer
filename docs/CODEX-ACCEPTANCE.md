# Codex adapter acceptance

Status: accepted in PR #8, squash `d6c3b3b`. Independent review and
Linux/macOS PR and main CI passed. The implementation worktree is retired.
Earlier validation notes below preserve intermediate findings; the final
handoff receipt records the observed runtime and safety checks.

## Runtime capability evidence

The acceptance run discovered a usable Codex executable on Darwin/arm64. Its
reported version and executable digest were recorded as task identity
observations. Runtime preflight also verified that the executable is regular
and executable, that `codex exec --help` succeeds, and that every flag used by
the adapter is advertised. These observations do not establish containment by
themselves and are never compared with a release manifest.

An upstream Codex source snapshot was inspected for the output behavior. Root
verified:

- [JSONL event schema](https://github.com/openai/codex/blob/6b9826e3aa83b1a5947db50f4332cb9c65f1b340/codex-rs/exec/src/exec_events.rs): agent messages expose text without a phase field.
- [JSONL producer](https://github.com/openai/codex/blob/6b9826e3aa83b1a5947db50f4332cb9c65f1b340/codex-rs/exec/src/event_processor_with_jsonl_output.rs): completed turns select the last top-level agent message; thread-total usage is copied into the completed event. Interrupted turns do not emit successful completion.
- [Last-message writer](https://github.com/openai/codex/blob/6b9826e3aa83b1a5947db50f4332cb9c65f1b340/codex-rs/exec/src/event_processor.rs): writes the exact message bytes synchronously with no added newline. The native safety run below confirmed output-file agreement at the writer boundary.

The discovered `exec --help` and `exec resume --help` outputs confirm the
planned flags and resume option placement. The interpreter uses a completed-
turn rule established by the output contract, not an invented phase field. Usage remains
conversation-cumulative, with absent counters unavailable and no inferred
per-task deltas. A changed CLI release remains usable when the runtime
preflight and output contract checks pass.

## Policy implementation choice

The implementation compares three approaches before selecting the narrow
profile. File inspection alone cannot establish macOS managed-preference or
cloud-policy absence. A native configuration probe could expose more effective
settings, but would need a new supervised process lifecycle before task
admission. The selected approach keeps preparation local: the same
`CFPreferencesCopyAppValue` API as Codex establishes managed-preference absence,
and a bounded personal-file-auth check establishes the initial no-cloud path.
It deliberately supports fewer configurations rather than introducing a second
execution lifecycle in an adapter.

The Darwin implementation uses pinned `github.com/ebitengine/purego v0.11.0`
to call CoreFoundation in-process. This preserves the repository's
`CGO_ENABLED=0` cross-build contract; a cgo implementation would change that
toolchain requirement. The call owns and releases its library handle and CF
objects. It reads only whether the two Codex control preferences exist, without
decoding or persisting their values. Other platforms remain unsupported.

The candidate pins file credential storage and `approvals_reviewer="user"`,
disables hooks, plugins, apps and both shell-snapshot implementations, and
rejects nonempty remaining local control configuration. The personal-auth
check reads the existing native auth file without copying or changing it.
Only known personal plans qualify, and access-token expiry and last-refresh
age must avoid native proactive refresh throughout the task budget plus the
startup margin. Alternate, external, managed-enterprise and unrecognized auth
are unsupported. Only a digest of non-secret eligibility facts enters task
metadata; credentials and account identifiers do not.

This guarantee concerns initial configuration loading. It does not make
account state immutable after a provider API 401, or make preflight atomic
against another process changing native files. Preparation is repeated at the
runner's pre-Start boundary and drift rejects launch. The source basis is the
pinned [auth manager](https://github.com/openai/codex/blob/6b9826e3aa83b1a5947db50f4332cb9c65f1b340/codex-rs/login/src/auth/manager.rs),
[auth storage](https://github.com/openai/codex/blob/6b9826e3aa83b1a5947db50f4332cb9c65f1b340/codex-rs/login/src/auth/storage.rs),
and [cloud service](https://github.com/openai/codex/blob/6b9826e3aa83b1a5947db50f4332cb9c65f1b340/codex-rs/cloud-config/src/service.rs).
Resume explicitly supplies the current sandbox, approval policy and approval
reviewer. The resume path reconstructs previous model/context metadata
without restoring old hooks, plugins or MCP configuration. Provider-401 auth
recovery does not replace the already-built session configuration. The recorded native safety run below covers this constrained profile.

The startup audit refined the read-only policy. In the inspected source,
legacy shell
snapshots default on and can run login-shell startup files outside the tool
sandbox; `allow_login_shell=false` alone does not stop this V1 path. Apps also
default on and inject the `codex_apps` MCP server independently of user
`mcp_servers`. The candidate therefore explicitly sets
`features.shell_snapshot=false`, `features.shell_snapshot_v2=false`, and
`features.apps=false`. See the inspected
[feature defaults](https://github.com/openai/codex/blob/6b9826e3aa83b1a5947db50f4332cb9c65f1b340/codex-rs/features/src/lib.rs),
[V1 snapshot execution](https://github.com/openai/codex/blob/6b9826e3aa83b1a5947db50f4332cb9c65f1b340/codex-rs/core/src/shell_snapshot.rs),
and [built-in MCP configuration](https://github.com/openai/codex/blob/6b9826e3aa83b1a5947db50f4332cb9c65f1b340/codex-rs/core/src/mcp.rs).
The earlier attempt remains preserved as a failed safety observation.

## Validation in progress

`make check` passed during implementation: full race suite, 35 acceptance-harness tests and 32 skill-validator tests,
strict static analysis, four platform builds, CLI smoke checks and vulnerability
scanning. Subsequent policy/argv and shared-helper changes passed focused race
tests. The 49-case protocol matrix passed, as did 48 hermetic supervisor cases
and five real pueue cases with natural shutdown. The shared contributor
preparation path passed its fifteen-case native fixture exercise, three
zero-admission preflight cases and replay checks. These are implementation
receipts; final reviewed-head gates and CI remain required.

The first real Codex attempt launched one fresh turn and produced a committed
outcome for task `a2f5f0e169abde9cba381f44c66894e2`. Its sealed stdout contained
a successful shell-wrapped `cat` command and the nonce. The original acceptance
matcher did not unwrap that shell command. More importantly, the model's final
response reported two write denials without corresponding command events in
the sealed stream. Prose does not establish those negative controls, so the
attempt failed the safety control and did not run a continuation. The private
daemon shut down naturally. Its source snapshot, binaries and evidence are
preserved in the private `codex-provider/native-safety-run` archive.
The normal shipped CLI subsequently collected that task successfully while
Codex runtime capability checks were still pending and the daemon was stopped; immutable
task evidence remained byte-identical and no provider launch was requested.

The revised acceptance harness binds a private, mode-0400 probe script by exact
bytes and digest. The script contains paths but no nonce value, performs the
read and both writes independently, and returns their actual exit codes and
stderr in one recorded command. The harness accepts only the exact script
invocation (optionally inside one recognized native shell wrapper), checks
unchanged sentinels and script bytes, and retains the exact resume identity,
original-task immutability and zero-launch replay requirements. Its 18 Codex
regression tests passed; the combined script suite has 72 tests.

## Required exit evidence

Meaningful interpreter, identity, policy, argv, reader/writer-failure and replay
tests; full quality, protocol and supervisor gates; two bounded native turns
proving successful reads, denied writes and exact continuation; direct Codex
review with no unresolved actionable findings; exact-head PR CI, squash merge,
main CI and worktree retirement. Native safety controls passed; independent review passed; CI and the merge lifecycle remain pending.

The finite `exec` and `exec resume` paths send their brief as
`UserInput::Text` through `TurnStart`. Slash and bang prefixes do not invoke
the interactive user-shell operation. That unsandboxed operation requires a
separate TUI command or explicit `thread/shellCommand` API request, neither of
which this launch path sends. See the inspected
[exec input path](https://github.com/openai/codex/blob/6b9826e3aa83b1a5947db50f4332cb9c65f1b340/codex-rs/exec/src/lib.rs#L877)
and [explicit user-shell handler](https://github.com/openai/codex/blob/6b9826e3aa83b1a5947db50f4332cb9c65f1b340/codex-rs/core/src/session/handlers.rs#L98).

## Recorded native safety run

On 2026-09-14 at 16:15 UTC, the revised candidate completed two native turns
through the ordinary dispatcher/runner composition and isolated pueue 4.0.4.
The explicit candidate composition differs only in runtime capability preflight;
execution, capture, sealing, collection and publication use the shipped core.

| Control | Evidence |
| --- | --- |
| Fresh task | `697f7c7e0eded5c60de061026617f923`, committed |
| Exact continuation | `0bd9ac255a6e9eae8ee7ac7b2d2c2136`, committed |
| Conversation | `01a0a0b3-e4d8-76a2-bf16-2c7284235222` for both tasks |
| Positive read | Exact random nonce from immutable probe, read exit 0 |
| Workspace and sibling writes | Independent exit 1 and path-specific `Operation not permitted`; both trees unchanged |
| Resume control | Recalled exact nonce after its file was deleted; brief omitted nonce; predecessor records unchanged |
| Replay | Both tasks collected again; queue and evidence unchanged; zero additional launches |
| Cleanup | Private daemon shut down naturally; zero signals |

The private archive `codex-provider/native-containment-proof` preserves the
acceptance binaries, source manifest and snapshot, task receipts and probe
binding. Runtime capability evidence records the runtime, profile, predicate and
receipt hashes. After promotion, freshly built production binaries exposed
Codex in `providers --json` and collected both tasks again with the daemon
already stopped and immutable evidence unchanged. No paid turn was used for
that production-discovery/replay check.

There were three native Codex integration turns in total: the earlier failed
profile-1 fresh attempt and these two successful profile-2 turns. Direct code
review invocations are separate from integration-test turns.

The final local `make check` passed after the revised probe/profile freeze:
full race suite, 40 acceptance-harness tests, 32 skill-validator tests, strict
lint/vet and tooling regressions, four-platform CGO-free builds, 12 CLI smoke
checks and vulnerability scanning. The subsequent runtime preflight/discovery
change also passed focused Codex and app race tests. The unchanged protocol,
supervisor and contributor controls passed earlier in this handoff as recorded
above. The independent review and focused findings closure completed.

## Review findings and verification

The final independent review identified a missing tool-free resume assertion.
The saved successful resume stream contains only `thread.started`,
`turn.started`, one completed `agent_message`, and `turn.completed`; its
native evidence remains valid. The harness now rejects
all tool/unknown item kinds and unknown events during resume, including
incomplete tool items. The corrected oracle passed against the existing sealed evidence with one
`agent_message` item and no tool or unknown events. The supplemental
`resume-oracle-revalidation.json` records hashes and zero new launches.

The review also proposed deferring identity recording until EOF. The existing
core contract explicitly allows `RecordProviderIdentity` while raw streams
remain open and grants that record neither launch nor terminal authority.
`TestProviderIdentityRequiresLiveRunnerAndPrecedesEOF` verifies this boundary;
continuation checks termination separately. Root retained early observation
and corrected the stale EOF-only comment and test name. Later conflicting or
failed output still rejects through the sealed-evidence interpreter; the first
observed identity is not erased. The focused identity race tests passed.
The corrected harness passed 21 Codex tests and the full 75-test script suite
(43 acceptance tests plus 32 skill checks). The focused independent review confirmed P1 fixed and closed P2 against the
core contract, with no concrete regression or unresolved actionable finding.

The final rebuilt production CLI again passed discovery and both immutable
collections after the supplemental receipt was recorded as task evidence.
Direct Codex review used `gpt-5.6-luna` at max effort. The complete review and
focused closure logs/source hashes are retained privately. Root's source,
contract and maintainability review agrees with the closure; CI and merge
remain required.

The initial PR head `18d7eb5` failed CI because the expanded discovery test
exceeded the cyclomatic-complexity ceiling by one. The prior full local gate
preceded that runtime preflight/discovery assertion change; focused tests had
passed but did not cover lint. The correction extracts the metadata assertions
into a named helper with an expected-provider table. Full lint and focused
race tests pass, and direct Codex review confirmed every assertion is retained.
A new full local gate and exact-head CI are required for the corrected head.

## Final handoff receipt

[PR #8](https://github.com/hishamkaram/delegation-layer/pull/8) merged on
2026-09-14 at 16:50:54 UTC. Reviewed head was
`c437ad0d0e31720579ff5c6513072b0aaf29bd7d`; squash commit is
`d6c3b3b4a8daf54850b9966a4727baefd86c801e`. Both platforms passed the
[PR run](https://github.com/hishamkaram/delegation-layer/actions/runs/34870598943),
[branch run](https://github.com/hishamkaram/delegation-layer/actions/runs/34870593816)
and [post-merge main run](https://github.com/hishamkaram/delegation-layer/actions/runs/34871131198).
The corrected full local quality gate also passed. All review findings were
fixed or closed against concrete contract evidence and independently rechecked.

The private `codex-provider` archive contains the complete source bundle,
generated files, native and review receipts, failed-attempt evidence, main-CI
receipt and retirement manifest. The implementation branch matched main's tree;
no process held its working directory. The worktree and owned local/remote
branches were retired only after main CI passed.
