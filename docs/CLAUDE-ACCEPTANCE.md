# Claude provider runtime safety

Status: **Claude live runtime safety acceptance passed; accepted in PR #9, squash `e688947`; Linux/macOS PR CI and main CI passed.**
The bounded live gate completed on 2026-09-15 with the user's signed-in Claude
account. It ran two real provider turns, verified the restricted read/search
controls and exact continuation, collected both outcomes, and performed an
observational replay with zero provider launches. The harness did not expose,
modify, or copy the host login Keychain, credential files, or global settings.
Because native continuation is tied to the host Claude configuration directory,
Claude wrote its normal host session metadata during the live run; the layer
does not edit or remove that metadata.

The integration passed `make check`, including the full race suite, 140 harness
tests, lint, CLI smoke tests, and vulnerability checks, after the lock and context
corrections. Group reconciliation, process joining,
worker binding, independent source-binding registration, and root-timestamp
pinning have clean bounded direct reviews. A Go overlay removing the production
join makes both process-ownership tests fail with the expected diagnostic.

The intermittent concurrent-dispatch failure is now reproduced independently of
pueue: concurrent `openat` calls using nonexclusive `O_CREAT` returned `ENOENT`
while creating the same lock inode. The syscall-level trace identifies
`.run.lock` after its parent directory opened successfully. A minimal eight-caller
probe failed in each of three runs; exclusive creation followed by reopening the
`EEXIST` winner passed 3,000 cases. These are observed Darwin results, not a claim
about every platform's implementation.

Both rooted and public lock initialization now use exclusive creation and reopen
only an existing winner. Existing-only lock access still refuses missing files;
it never recreates them or retries `ENOENT`. The corrected storage stress test
passes 100 task initializations with eight competing callers. The shipped native
inspection gate now uses eight callers and passes success, reattachment, replay,
and running/queued expiry, with natural daemon shutdown and zero AI turns.

The lock correction has a clean scoped direct Codex review. Context propagation
now reaches existing-store filesystem probes and budget stop receipts; legacy
nonblocking methods retain their behavior. The integrated supervisor gate passed
48 hermetic cases and five native cases with natural shutdown. The final protocol rerun passed all 49 fault cases and the compiled CLI workflow.
Direct context review found one missing cancellation checkpoint after filesystem
policy preflight. The checkpoint is now immediately before probe-directory
creation, and the corrective direct review reports no regression in that scope.
The full quality gate passed again after this final correction. An isolated
policy-cancellation fault test passed three race-enabled runs; removing the
checkpoint makes the test fail because canceled preflight creates the probe
parent. The overlays and logs are retained as private diagnostic evidence. The private investigation record
is `task-initialization-investigation.json`. Phase 5 was reviewed in the private
pull request and squash-merged as `e688947`; both PR CI and main CI are green.
The live run records the plaintext fallback as an opaque
presence marker only and uses the native helper as the authentication authority;
credential bytes and global login/settings remain untouched.

The acceptance driver isolates layer-owned task, evidence, supervisor,
temporary, and scratch-workspace paths. The workspace is outside the host home
tree so the provider's ancestor policy walk cannot discover real project
settings. Before pueue or any native/provider process starts, the driver uses
`lstat` only on the host `.credentials.json` fallback; a present entry is
recorded as an opaque marker and never opened. The native helper confirms the
signed-in account immediately before launch, while Claude keeps its native
host configuration and session store so the real account can continue.

## Runtime capability observation

- Provider: `claude:print`, minimum `read-only` profile with `dontAsk` approval.
- The acceptance host is Darwin ARM64. The discovered executable's path,
  reported version and digest are recorded as task identity observations.
- The actual version and help invocations exited successfully, and every flag
  used by the adapter was present in the help output. A changed reported CLI
  version remains acceptable when these checks pass.
- The ordinary
  macOS first-open confirmation was completed for the signed Anthropic binary;
  authentication and global provider settings were not changed.

## Profile and ownership

The normative profile remains the [execution plan](EXECUTION-PLAN.md): fixed
safe/restricted print arguments, Read/Glob/Grep, optional non-mutating
EndConversation, strict empty MCP, task-owned settings, and finite stdin.
Fresh session IDs use UUIDv5 with decoded root-ID bytes as the namespace and
canonical task-ID ASCII bytes as the name. Resume preserves the profile and
selects only the exact layer-recorded session UUID.

Preparation and interpretation stay inside the provider package. The existing
app/runner owns admission, process lifetime, supervision, capture, sealing,
collection, and publication. The live safety run uses an explicit test
composition of that same app; its passing status does not change production
discovery. Historical interpretation remains registered.

The common runtime capability checks are shared with Codex. Provider-independent
field-binding tests live with that matcher, while provider tests bind each
output interpreter and its production registration. The shared matcher and Codex
race tests passed after extraction; the integrated quality, protocol,
supervisor, and inspection gates are also recorded below.

## Native policy evidence and supervised access

The inspected executable's remote-policy eligibility and loading functions were
inspected at byte offsets `167605700–167609000` and
`178184976–178213882`. Native Pro/Max OAuth without alternate authentication is
ineligible for remote managed settings; the loader returns before cache/network
loading. Team, enterprise, unknown/null subscription metadata, API keys, profile
authentication, and certain remote/evaluation contexts cannot use that proof.

Eligibility comes from the native `claudeAiOauth` credential object, not the
separate `oauthAccount` display metadata. On macOS the native composite storage
prefers Keychain and falls back to `.credentials.json`. A file-only inspection
cannot establish which native credentials will be used.

A historical no-UI Security-framework probe found the
`Claude Code-credentials` entry's attributes but could not read its data
(`errSecAuthFailed`, -25293). It returned no credentials and changed no Keychain
permissions or authentication settings. The exact native service/account query returned the same denial. This blocks
the planned in-process native-login inspection on this host. The [native
inspection handoff](NATIVE-METADATA-INSPECTION.md) therefore selects supervised
admission inspection with a core-owned final recheck. That path is implemented
and was exercised successfully by the live gate; an observation timeout still
cannot stand in for process termination.

Pinned macOS managed-policy loading reads the entire dictionaries at
`/Library/Managed Preferences/<OS username>/com.anthropic.claudecode.plist` and
`/Library/Managed Preferences/com.anthropic.claudecode.plist` through `plutil`;
it does not use CFPreferences. These source facts supersede generic documentation
assumptions about the host policy API.

Apple documents that [disallowing authentication UI](https://developer.apple.com/documentation/security/ksecuseauthenticationuifail)
causes protected queries to fail rather than prompt. Any unavailable or ambiguous
native policy source must prevent admission. The user approved the shared,
supervised inspection amendment, preserving login and system settings. A private
isolated pueue experiment completed the native helper without retaining stdout
or stderr. Its diagnostic follow-up returned native exit zero, Team subscription,
valid unexpired expiry, and inference scope. The fixed reason `unsupported_plan`
comes from the current personal Pro/Max-only verifier. The user subsequently
switched to a personal account. A fresh supervised read at 18:41 UTC on
2026-09-14 returned Max, valid unexpired inference access, and native exit zero.
The receipt SHA-256 is
`3aa1bd5a8889488d4e70e957a8ae309c807aa1ce8012912c9c725cb696e74e7a`;
see the native inspection handoff for its location and scope. The host-account
blocker was cleared for that observation. The user subsequently switched back
to Team. This is supervision evidence only, not provider live acceptance. No
adapter receives process ownership. Team preflight now combines a fresh fixed-endpoint
settings observation with local and cache source validation. The native reload
timing limitation is documented in the inspection handoff; an atomic remote
snapshot freeze is not an agreed guarantee.

A supervised Team-only first-party settings GET returned HTTP 404 at 18:55 UTC
on 2026-09-14, the inspected loader's successful-empty case. Its remote receipt is
recorded in the native inspection contract. Credentials and remote response
bytes were not retained or hashed; the supervisor shut down naturally and used
zero provider turns. This earlier supervision-only observation is supplemented
by the successful two-turn live acceptance recorded below.

## Focused validation

The shared JSONL framer and its Codex adoption passed focused race tests and
lint. Root review found and fixed strict-boolean/null handling, blank error-array
entries, interruption-before-result, unknown stop reasons, and substantive output
after completion. The resulting Claude pure-file race suite passed, along with
the earlier Python validation snapshot, including Unicode SimpleFold corrections.
After the evidence-review corrections, root reran all 60 native-harness tests
and all 32 skill-verifier tests successfully.
The generated Unicode table also passed its Go race test. The live acceptance
harness binds the
updated predicate digest
`b92db06be971c081c653322a6cc9d12ff6996066019df05e0dd6989ab62b3021`.

The shared direct Codex review found a Python/Go Unicode folding mismatch; the
correction passed focused direct re-review with no actionable findings. The
Claude evidence review found stream identity/tool validation and
acceptance-oracle gaps. Those corrections and their focused regressions now
have clean direct follow-up reviews. The candidate and pure finalizer replace
the missing effective-policy implementation. Focused full-package race tests
passed for Claude (4.140 seconds), app (41.614 seconds), and execution
(30.609 seconds). These historical package timings do not establish native
runtime safety; the live acceptance receipt below is the current evidence.

The direct deadline follow-up review returned no actionable findings for its
bounded changes. Subsequent work adds a borrowed preflight scope and a supervised
inspection entry point using the same budget owner. Its scoped direct Codex
review completed with no actionable ownership, race, expiry, or compatibility
findings (`supervised-scope-review.log` in the private evidence directory). Its tests cover queued expiry, late success, scope reuse,
and cancellation while retaining owned work. Private capture tests cover both
stream caps, draining after overflow, masked errors, and retained Wait ownership.
The scoped private-capture direct review also completed with no actionable
findings (`native-capture-review.log`). Neither review covers the later app
admission and worker integration.

The latest affected package race run passed app (53.923 seconds), inspection
(15.540 seconds), execution (34.422 seconds), and Claude (6.487 seconds).
It includes the new worker and admission implementation. Subsequent permission
binding and caller-context corrections require another affected run. The task-directory race suite subsequently passed (124.335 seconds), and its
focused lint returned zero issues after caller-context propagation. A full `make check` subsequently passed, including the vulnerability scan
(`quality-gate.log`). That snapshot predates later deterministic pueue command
and live-inspection changes; the current full quality result is recorded in the
summary above. The Claude policy review found blank-token and pre-network expiry
validation gaps. Both were corrected with targeted tests; focused direct
re-review returned no actionable findings (`claude-auth-review.log`). The live
runtime safety gate then passed with two real turns and zero replay launches.

The acceptance brief explicitly requests the workspace/sibling write, shell,
and subagent controls required by the execution plan. An unavailable route is
skipped without substituting tools or changing permissions; its exact omission
from the pinned init registry remains separate from an attempted native denial.
The harness's 16 deterministic tests passed after this correction.

Bearer token validation follows the syntax in
[RFC 6750 section 2.1](https://www.rfc-editor.org/rfc/rfc6750#section-2.1),
including nonempty token bodies and trailing-only padding. Network authorization
also checks the five-minute native refresh boundary before exposing a Team
credential to the fixed first-party GET. Finalization still enforces the larger
requested-task horizon.

The updated all-package `make test-race` passed, including app (85.708 seconds),
execution (49.478 seconds), inspection (24.424 seconds), Claude (8.066 seconds),
and the new inspection fixture (2.491 seconds). Formatting, configuration checks,
vet, lint, gate-verifier regressions, and the 32 skill-verifier tests also passed.
The compiled protocol acceptance passed all 49 fault cases and its live CLI
workflow (`integrated-protocol.log`). This was an earlier snapshot; the final
Python failure-ownership, inspection-oracle, lock-initialization, and context
corrections are covered by the current evidence summary above.

## Supervised inspection fixture status

The no-AI fixture uses the normal app and runner with an isolated real pueue.
The first execution failed an assertion when one concurrent dispatcher returned
lock-busy with unknown admission while the other admitted the same identity.
The corrected harness resolves that identity through reattachment and verifies
one admission helper, one final helper, and unchanged collection evidence.
The first failed run lacks a natural daemon completion receipt; later connection
refusal and an absent exact PID do not replace owned Wait evidence.

A subsequent execution completed the success/replay, running-expiry, and
queued-expiry assertions. The running helper's exact supervisor job ended as
Killed; queued expiry started no helper and created no ordinary task. Private
sentinel scanning and natural daemon shutdown also completed. The run still
failed overall because the final summary filename collided with a collection
receipt. That filename and concurrent-admission receipt handling are corrected. The
subsequent full execution passed, as recorded below.
Neither failed execution is reported as acceptance success.

Root also corrected inspection deadline classification to operational exit 1,
matching the public CLI contract. The dispatch JSON regression proves that a
wrapped deadline preserves unknown admission, creates no ordinary task, and
publishes no outcome; focused race and lint passed.

The existing supervisor regression gate passed all 48 hermetic cases. Its
native runner-event oracle initially rejected newly introduced preflight and
Start authorization events; exact count/order assertions and retained failure
cleanup were corrected, and the current five-case native rerun passed. Claude
provider turns remain at zero.

The corrected no-AI inspection gate passed all three cases with natural daemon
shutdown and no sentinel leakage. Its receipt SHA-256 is
`2ffd4a6cd457cbcef30a85cabca3270dfcbf08868a0d669c4aa022146fac1d4a`.
An initial real-Claude safety dispatch then correctly refused inherited
`CLAUDE_CODE_COZY_TEAPOT=relaxed` before creating any inspection or ordinary task.
The inspected binary reads this selector at offset 168050856 for Bash-first prompt
steering, used at 175036860. The safety invocation removes that experimental selector
from its invocation environment only; production rejection and the user's
global environment/login remain unchanged. The empty failed-run queue was
independently verified and shut down through pueue while its original daemon
owner retained Wait. A second static refusal rejected non-empty global MCP
configuration. Its empty queue was also verified and shut down. Both original
daemon owners recorded natural exit zero; neither attempt ran a model turn.
The strict MCP classifier was subsequently validated by the harness and its
direct follow-up review; user MCP configuration remains unchanged.

## Strict MCP and global history classification

The inspected binary's strict loader at byte offset 179294970 selects an empty
ordinary-server map instead of `cP` (171463595), which loads ordinary user,
project, and local MCP sources. Explicit MCP files remain active inputs, so
preparation verifies containment flags and the task-owned empty MCP file.
The enterprise guard (`Km`, 171474945) and independently active auth/helper
paths remain conservatively checked. Ordinary MCP commands, environment values,
and headers are excluded from executable/auth classification and value hashes.

Inspected-source offsets 173476776–173477472 establish that root `skillUsage`
is a map of skill names to usage counts and timestamps. Active `statusLine`
commands instead come from the settings schema and policy resolver
(165344214 and 170451820). The classifier excludes that root history map while
retaining checks on active or similarly named nested settings. Tests verify
that history values do not affect the shape projection and that actual helpers,
status-line commands, managed MCP, and other active controls still refuse.
No user settings or credentials were changed.

## Review corrections and current verification

The direct Claude result review identified terminal-system subtype and malformed
optional-array handling gaps. The parser now rejects timeout/refusal lifecycle
markers and explicit null `errors`/`permission_denials`. An empty `errors` array
remains valid because it contains no error entry; this is covered by a success
regression. The contract digest is unchanged.

The live oracle now binds Read to `file_path`, Glob/Grep to `path`, and each
positive result to a unique preceding assistant tool call and subsequent user
result. The shared publication reader requires the protocol's fixed regular,
non-symlink payload file and verifies length and digest. Regression fixtures
cover auxiliary-path spoofing, reordered/wrong-role results, duplicate call IDs,
traversal, absolute paths, symlinks, and malformed lengths.

The prior stderr-marker finding did not establish a concrete Claude-specific
stderr-only failure marker with a successful native exit and result. Pinned-source
inspection and the direct follow-up review did not substantiate that proposed
change. The implementation keeps structured terminal events and sealed exit
evidence authoritative; it does not infer failure from generic diagnostic prose.

All-package race tests, formatting/configuration/vet/lint, skill validation,
cross-builds, CLI smoke tests, and vulnerability checks passed. Protocol
acceptance passed its 49 fault cases and compiled CLI workflow. The supervisor
native gate passed all five cases after its authorization-event oracle was
updated; it observed natural daemon shutdown. Its evidence is
`bin/supervisor-acceptance/native-supervision-1789418201026` in the active worktree.
These results are an earlier snapshot; the later bounded-result and policy
corrections are covered by the current quality and review records above.

The static-refusal cleanup path also passed a real no-model supervisor test;
the private receipt is identified as
`claude-static-refusal-1789418639861010000`.
Dispatch exited 2 before metadata or model launch; its response retained unknown
admission/publication. The final cleanup receipt records all owned processes
joined, natural daemon shutdown, and zero signals; the original daemon Wait
receipt independently records exit 0. This is failure-cleanup proof, not Claude
provider live acceptance.

The latest complete `make check` passed (`current-quality-gate.log`). A subsequent
Python-only review correction requires an exact integer static-refusal schema
version; its direct follow-up accepted the fix. The policy follow-up also accepted
the single root-history projection before all active checks. Later result-review
corrections preserve absent usage for empty model maps, reject padded model
names, and strictly validate every error-array member. All 112 harness tests and
the affected Claude race/lint checks pass. The direct result-edge follow-up
accepted all three corrections without an actionable defect; the broader
inspection review's findings were subsequently resolved and its final context
follow-up was clean.

## Live acceptance result

The successful receipt is retained outside the repository at
`$HOME/Library/Application Support/delegation-layer-evidence/claude-720cf0231bc1728c30475c7a`.
It records `acceptance-passed`, two planned and two executed native turns,
`replay_launches: 0`, natural daemon shutdown, and zero signals. The fresh task
`399da8b345b2a2fa01d9e9d462d65f9d` and its continuation
`036a071a8f1bd43ee2435f874b71ea61` share session
`77a5691a-4e1a-53cb-bedd-11e7bc121652`; both collected payloads are byte-bound
and the predecessor outcome is unchanged.

## Remaining merge gates

- Complete the direct review of the current exact head and resolve any findings.
- Commit the reviewed branch, open the private pull request, wait for green
  exact-head CI, squash merge, verify main CI, and retire the provider worktree.

Private review and diagnostic evidence is retained outside the repository under
the run-specific evidence root. Native acceptance evidence is outside
provider-writable temporary roots. Credentials and raw provider output remain
outside the repository.
