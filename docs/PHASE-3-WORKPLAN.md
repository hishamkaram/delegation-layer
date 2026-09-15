# Phase 3 — Antigravity implementation and acceptance

Status: implementation in progress; no native permission runtime acceptance claimed.
Base: accepted Phase 2, main `b52eda2917fb186cdf7a8f21854d978dc5b79145`.
The provider specifications in `EXECUTION-PLAN.md` remain authoritative.

## Responsibilities and order

Root Codex owns integration, final review, the native `codex review` invocation,
commits, private PR, CI observation, squash merge, and verification on main.
The existing Luna agents use maximum effort and have separate ownership:
Antigravity interpretation/identity, shared usage storage, and read-only policy
inventory. No implementer commits or operates the remote repository.

Phase 4 begins only after Phase 3 satisfies every gate below and is merged.
Phase 5 follows the same rule after Phase 4. The first release remains private
and requires all three providers.

## Implementation contracts

1. Add an immutable, digest-bound Antigravity print output interpreter. Read only
   sealed task evidence. Stream arbitrarily long response text with bounded
   parser memory; reject malformed UTF-8/JSON, duplicate keys, trailing records,
   unknown status, error payloads, blank answers, and identity conflicts. Drain
   both streams even after refusal or a payload-writer failure. The exact stderr
   timeout marker always prevents successful publication.
2. Record identity using a bounded observer with the same JSON string semantics
   as final interpretation. The observer authorizes no publication. Resume uses
   only the exact layer-recorded UUID; the existing session claim serializes
   continuation and preserves the predecessor.
3. Add optional scoped usage records to interpretation and outcome. A slice
   supports later Claude main-agent and whole-tree records without merging them.
   Missing fields remain nullable; absent usage preserves existing serialized
   outcomes. Clone all pointer fields and validate durable replay. Antigravity
   counters and duration are conversation-cumulative; incomplete evidence is
   unreliable. No inferred per-task deltas, counter sums, or zero filling.
4. Implement a typed launch profile and policy resolver using the discovered
   CLI's source inventory. Validate applicable CLI, shared,
   project, workspace, customization, hook, plugin, and MCP inputs. Reject
   unknown policy effects, bypass/unsandboxed rules, extra writable workspaces,
   and unreadable applicable sources. Preserve authentication and global files.
   Normalize non-secret decisions and source identities into the admission
   digest; recompute immediately before the one permitted launch.
5. Bind the canonical executable and its observed version/digest to the task
   profile. Use argv slices, canonical cwd, finite task-owned stdin, explicit workspace-write,
   sandbox, JSON output, text input, disabled slash commands, and positive native
   print timeout. Resume adds only the recorded conversation flag. Unsupported
   model/effort combinations and read-only requests fail before admission.
6. Validate state placement outside the workspace and every declared provider
   runtime/temp/cache writable root. Persist enough non-secret normalized
   decisions to explain the permission profile and any drift rejection.
7. Wire the native interpreter into both shipped commands, preserving the
   existing fixture predicate for recovery. Collection remains observational.
   Ordinary dispatch never launches a paid acceptance probe. Runtime preflight
   discovers the executable and required flags before admission; incomplete
   source discovery or failed prerequisites remain an explicit unavailable
   profile. No release status is inferred from a local receipt.

## Verification matrix

| Gate | Required evidence |
| --- | --- |
| Interpreter and identity units | Fresh/resume; every known failure envelope; blank/empty answer; timeout at every chunk split; missing/duplicate/wrong ID; escaped strings; malformed/truncated/extra JSON; large streamed answer; reader and writer faults; complete drain |
| Usage/storage units | Nullable counters, duration spelling, cumulative/reset fixtures without derived task claims; pointer isolation; multiple scopes; invalid metadata; old outcome compatibility; exact replay |
| Policy/profile units | Compatible baseline; bypass, outside writes, symlinks, unsandboxed rules, hooks/MCP/plugins, extra workspace, unreadable/unknown policy; digest drift; exact argv and metacharacter-safe stdin; unsupported permission/model/effort |
| Compiled CLI cases | Pre-admission policy refusal; launch-time drift refusal; sealed collection replay with zero launches; immutable predecessor and active-session refusal |
| Existing regression gates | `make check`, `make -j4 check`, `make acceptance-protocol`, `make acceptance-supervisor` |
| Native gate | `make acceptance-agy`, including all three turns below; a missing prerequisite fails, never skips |
| Review and merge | Root review of all changed files; direct `codex review`; all actionable findings fixed and affected checks rerun; private PR; exact-head Linux/macOS CI green; squash; main tree and postmerge CI verified |

## Native acceptance: three turns

Use the actual shipped `delegate` and `delegate-run` with an isolated real pueue
instance. Store receipts and task state outside all provider-writable roots.
Use the declared normal task budget of 120 seconds and watch bound of 150 seconds.
Record executable/version/digest, effective profile, exact task/session IDs,
argv with no prompt contents, launch counts, raw evidence, outcome, filesystem
oracles, and the actual process/supervisor terminal observation.

- L1: place a random nonce in the workspace. Require reading it, creating the
  specified workspace sentinel, and attempting the specified sibling write.
  Verify real sentinel contents and absence of the outside file. A refused
  positive control is inconclusive; any outside write fails runtime acceptance.
- L2: create a new task resuming L1's exact conversation. Require recall of the
  prior nonce and verify same provider ID, distinct task ID, and byte-identical
  original request, identity, raw evidence, result, and outcome.
- L3: request a deliberately long answer with a short native print timeout.
  Use `--budget 120s --native-timeout 3s` so the supervisor budget cannot normally
  win the race against agy's timeout. The optional native timeout is normalized,
  bound into task identity, positive, no greater than the outer budget, and
  rejected for other providers. Without it, existing budget semantics remain.
  Preserve the observed envelope and raw streams; require no successful result
  publication. Do not require reproduction of one historical envelope shape.

Do not launch an automatic retry or additional exploratory paid turn. Rerun
only a specifically invalidated measurement or changed implementation with an
explicit receipt explaining why. An observation timeout retains the existing
handle for inspection; it does not authorize relaunch, fabricated terminal
state, or direct signals. Shut down the private supervisor only after its jobs
are positively observed finished.

## Acceptance record

Create `PHASE-3-ACCEPTANCE.md` from actual receipts after execution. Include the
observed runtime capability table, config-source rules,
runtime writable-root disclosure, live L1–L3 results, review scope, exact tested
commit/tree, CI URLs, and material limitations. Do not advance implementation
tracking to verified based on unit tests or a draft runtime acceptance record alone.

## Native capability candidate

The first authenticated supervised run reached the model but denied the exact workspace nonce read. The revised candidate passes `--add-dir` with only the canonical requested workspace and keeps the sandbox and headless approval policy. Runtime capability evidence remains subject to the live controls below. A native write-denial outcome from the outside control stays rejected; continuation must independently commit and recall the nonce after both workspace copies have been removed.

The local acceptance environment inherited `SSH_CONNECTION` from a tmux session. Omitting that variable only in the acceptance child cleared authentication, consistent with the documented local keyring versus SSH authentication flows. Production preserves the caller environment; genuine remote sessions must use their configured authentication flow.

The initial candidate reached a native `WriteToFile` denial with the inside sentinel still absent; it failed the positive control. The current candidate adds the documented per-run `--mode accept-edits` to permit file edits while retaining shell permission rules and the sandbox. The effective approval record includes this decision. Outside-write containment is still unverified for this candidate and must pass the live filesystem control. See https://antigravity.google/docs/cli/modes/.
