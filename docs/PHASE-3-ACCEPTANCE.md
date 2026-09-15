# Phase 3 acceptance evidence

Status: **implementation, local gates, and review complete**.
[PR #4](https://github.com/hishamkaram/delegation-layer/pull/4) is the authoritative
record of exact-head CI and squash-merge acceptance. This record describes the
observed runtime capability and per-task safety checks. Provider version,
platform and executable digest are observations from that run; they are not
release acceptances.

## Runtime capability measurement

The 2026-09-14T07:24:53Z native `make acceptance-agy` run passed using the
discovered agy executable on Darwin/arm64. Runtime preflight located the
executable, verified its executable bit, accepted its reported version, and
confirmed every launch flag in `agy --help`. The task receipt records the
observed executable identity and policy details for replay. Other environments
require their own live containment checks; no provider release or platform
status is inferred from this receipt.

The native run used the shipped dispatcher and runner with private pueue 4.0.4:

- L1 read the workspace nonce and wrote matching contents inside the workspace.
  The outside write was denied and its target remained absent. The denial
  envelope remained a rejected publication; it was not promoted to success.
- L2 resumed L1's exact recorded conversation after both local nonce copies were
  removed, recalled the nonce, and committed. Original task evidence remained
  unchanged during continuation and collection replay.
- A competing continuation was refused before submission/provider start.
- L3 recorded the native short-timeout marker and rejected publication.
- A queued policy change was refused before provider start.
- Event counts were A=4, S=3, E=3, seal=3: three provider turns and one
  non-provider policy-drift admission. The private daemon exited with Wait=0.

The tested approval policy is
`accept-edits:request-review:headless-deny`. It uses one canonical trusted
workspace, the sparse default CLI project, validated configuration-source
inventory, and runtime/state-root exclusion checks. The CLI arguments include
`--sandbox --mode accept-edits --add-dir <workspace>`; no blanket permission
bypass is enabled. Ordinary dispatch resolves policy and runs the same
executable/help preflight without requiring a local receipt or an extra paid
probe.

## Review corrections and compatibility

The earlier 2026-09-14T06:55:32Z capability measurement used output-predicate
revision 1.2.2 with SHA-256
`9aca4a6a639c18b8c4515445d8f004f2788c0f627264695cbb66357b5285d366`.
That interpreter remains registered for immutable collection/replay.
The new `1.2.2-auth2` output interpreter classifies structurally valid ERROR envelopes
before requiring a conversation UUID. Success still requires exact session
identity. Focused race tests verify both revisions and registry resolution.
The latest integrated native gate passed with the new output-interpreter revision, SHA-256
`53f1b5368023953447088ecbd4fc36c1b431b0fb376cf76511ed70d943d8138a`.
The task records bind that exact predicate together with the observed runtime
identity for replay. A final 2026-09-14T07:52:14Z native run also passed after
adding policy validation immediately before process Start. Refusal at this boundary follows
the sealed start-failed path, preserving the consumed start authority without
launching the provider. Unit regressions cover refusal, replay, and exactly one
successful launch. The final native run again recorded A=4, S=3, E=3, seal=3;
all 66 status captures passed the redaction and serialized-size audit.

Historical private pueue status artifacts contain inherited environment fields;
their original `environment_values_recorded=false` summary was inaccurate.
Those artifacts are preserved privately and must not be committed. The revised
harness removes `envs` before status stdout reaches disk and rejects malformed
or oversized observations. A separate real pueue 4.0.4 test verified redaction,
client-environment inheritance despite a different daemon environment, and
natural daemon shutdown, with zero provider launches.

## Completed local gates

| Gate | Result |
|---|---|
| `make check` | Passed, including race tests, lint, builds, CLI smoke and vulnerability scan |
| `make -j4 check` | Passed |
| `make acceptance-protocol` | Passed: 49 protocol cases and CLI workflow |
| `make acceptance-supervisor` | Passed: 48 hermetic cases and real I01–I05, natural shutdown |
| `make acceptance-agy` | Passed: current predicate and three native turns |
| Native evidence audit | Initial current-predicate run: 74 redacted captures; final launch-path run: 66 redacted, size-bounded captures; exact outcome/replay checks passed |
| Targeted Codex cleanup review | No concrete regressions in lint cleanup, test renames and capability binding |

The initial supervisor gate stopped before execution because the worktree-local
test binaries were absent. Installing the SHA-256-pinned pueue/pueued 4.0.4 pair
resolved that prerequisite; the complete rerun passed. No application source
changed for this prerequisite correction.

The final launch-path code passed `make check` and a complete rerun of
`make acceptance-supervisor` (48 hermetic cases and I01–I05). Direct Codex
CLI review findings are resolved; the final focused review found no concrete
actionable defects. Exact reviewed source hashes were verified.

## Merge acceptance requirements
- Require green Linux/macOS CI on the private PR’s exact head, squash merge,
  and verify main CI. The linked PR and workflow runs record those results.
- Stop after Phase 3 as requested. Phases 4 and 5 remain planned and unstarted.
