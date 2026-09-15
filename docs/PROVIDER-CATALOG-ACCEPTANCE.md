# Provider catalog acceptance

Runtime snapshot: `d2444c4` on `codex/provider-catalog`, based on `84a1a4a`.
Accepted in [PR #6](https://github.com/hishamkaram/delegation-layer/pull/6),
squash `38e3da4`. Direct review was clean at `93c30c9`; all Linux/macOS
PR checks and [main CI](https://github.com/hishamkaram/delegation-layer/actions/runs/34840061876) passed.

## Local gates and runtime capability checks

- Full `make check`: passed, including strict lint, race tests, native-harness
  tests, builds, 12 CLI smoke cases and vulnerability scanning.
- `make acceptance-protocol`: all 49 fault cases and compiled workflow passed.
- `make acceptance-supervisor`: all 48 hermetic cases and native I01–I05 passed
  with private pueue/pueued 4.0.4 and natural shutdown.
- `make acceptance-agy`: passed on the discovered agy executable on Darwin arm64.
  Runtime preflight verified the executable and every launch flag advertised by
  `agy --help`; the task receipt records the observed version/digest and output
  predicate revision for replay. No provider release version or binary hash is
  required by catalog discovery.

The native receipt verifies workspace write and outside denial, exact-session
continuation, predecessor immutability, native timeout rejection, repeated
collection, active-session refusal and prelaunch policy drift. Counts were
A=4, S=3, E=3, seals=3; the drift task had no provider start.

| Native case | Task ID | Sealed manifest SHA-256 |
|---|---|---|
| Write/denial | `a5042b45bc36333ee1a7e15fb961be53` | `fc529fa09b1132e89e1308524b165a2059498c8b56ebdad6bdca1caf05f00f84` |
| Exact continuation | `b85e7abee470bc25c439343554d2a163` | `5f1aed028ef3338f95a08136c94b17094be68923db0c6b255440788598d2ef15` |
| Native timeout | `63635cda2a4e81131a3446e24db8d9f5` | `b100530655df66848e44add2c98c9929fad0e688a6f48a421d56651589d4eb71` |

Native evidence run: `agy-20260914T112008Z-64dbae64`. Raw private logs and
binary digests remain outside git in the orchestration evidence manifest.
The documented process-local SSH environment workaround selected the existing
local login; no global credential or configuration changes were made.

## Review and retest provenance

Direct Codex review of `a1c57c9` identified five P2 findings: explicit default
effort compatibility, ignored fixture timeout, runtime capability identity
validation, profile/predicate mode consistency, and prepared-predicate binding.
`60bcd12` fixes them with regression tests, including multiple supported modes
for one mode. `d2444c4` removes a duplicate assertion to satisfy complexity lint;
the dedicated timeout regression remains. Runtime re-review found no additional
runtime/catalog issue, and the final bounded review of `93c30c9` found no
actionable findings.

Two earlier observation timeouts remain recorded as failures: the fake pueue
version probe in P03-diagnostic, and the unchanged compiled child-rendezvous
fixture. Both passed isolated retests and subsequent complete gates without
loosening assertions. Their underlying timing cause was not established.

Codex and Claude integration, shared infrastructure extraction, and the synthetic
contributor exercise remain subsequent handoffs; this receipt claims only the
catalog and agy proof.
