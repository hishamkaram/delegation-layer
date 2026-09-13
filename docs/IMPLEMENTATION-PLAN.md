# Implementation plan and milestones

**Status: agreed plan, nothing built.** Supersedes the *Milestone plan* section of
[`DESIGN.md`](../DESIGN.md), which remains the architecture of record.

## How this plan was produced

Three implementation plans were written **blind** — Claude, Codex (via the codex plugin) and `agy`
(via its CLI) each received one identical brief, none saw another's plan, and none was told the
others existed. All three then read all three and debated across **three rounds** with an explicit
claim ledger. Every contested row was ruled MAINTAIN / RETRACT / REFINE / VERIFY with evidence.

What that process actually bought, since "three models agreed" is not by itself evidence:

- **Three rows were settled by measurement rather than argument**, and each overturned a position
  all three participants held. See *Measured during planning*.
- **Three defects were found in `DESIGN.md`** that agreement alone would not have produced — each
  was found by one participant and then independently verified. See *Design amendments*.
- **Each participant retracted something of its own.** Claude conceded `doc.go`-only packages, a
  misread of the argv rule, and — last — its own measured `link` proposal; Codex retracted its
  Phase 0 exit criterion and its publication lock; `agy` retracted seven rows, including its own
  strongest linter argument.

Two rows are worth singling out as evidence the process did something agreement could not:
`contextcheck`, where **two participants were wrong in opposite directions** and both retracted;
and the publication primitive, where the position that fell last was **the one backed by the
author's own measurement**. Both are in *Disagreements and how they fell*.

Artifacts (blind plans, all three rounds, raw replies) are outside the repo and not published.

---

## Measured during planning

Three facts were established by execution on this machine (macOS 26.6.2, arm64, Go 1.27.1) rather
than by reasoning, and each changed the plan.

### The brief reaches the provider on stdin

`DESIGN.md:673` forbids brief text in argv; `DESIGN.md:576` prescribes `/dev/null` stdin. Codex read
these as a contradiction blocking the first adapter, since nothing would be left to carry the brief.

Measured instead:

```
$ echo "reply with exactly the word PONG and nothing else" \
    | agy --sandbox --dangerously-skip-permissions --print-timeout 60s --output-format json
{"status":"SUCCESS","response":"PONG\n","num_turns":1,...}
```

**`agy` reads the prompt from stdin.** So the contract is: `delegate-run` pipes `brief.md` to the
provider's stdin and closes it. The brief never enters argv, and EOF still arrives — which was the
entire purpose of the `/dev/null` rule, since a CLI that wants interactive input still fails fast.

**The universal `/dev/null` rule is replaced by: no interactive input source; finite input then
EOF.** A provider-supported file argument is the alternative where one exists.

One qualification: EOF guarantees **no further stdin input**, not that a CLI exits promptly — a
provider could retry on EOF or open a terminal directly. **The non-interactive flags remain
required**; stdin discipline does not replace them.

### `link`, not `rename`, is the publication primitive

`DESIGN.md` specifies `result.tmp` → `result.txt` by rename. All three blind plans copied that.
It is wrong, and not for the reason the debate first found: **`rename` is last-writer-wins, so it
will silently overwrite an already-committed terminal record** — violating `DESIGN.md:1324`,
*terminal records are terminal*.

`link(2)` is atomic create-if-absent. Measured with 8 goroutines, each staging to its own
`os.CreateTemp`, released simultaneously against one target, three consecutive runs:

```
winners=1 (must be exactly 1)         ← all three runs
loser:                                 link .../result-57906587.tmp .../result.txt: file exists
late attempt over a committed result:  link .../late-2037241564.tmp .../result.txt: file exists
```

Exactly one winner every time; every loser gets `EEXIST`; a late writer cannot overwrite a committed
result at all.

**But `link` alone is not sufficient, and the first version of this plan said it was.** `link` is
atomic **per pathname**. Publication links `result.txt`; rejection links `publish.reject`. Those are
different names, so **both succeed** — leaving two contradictory terminal records. Measured, 200
iterations of a publisher racing a rejecting collector:

```
SPLIT-BRAIN: both terminal records present in 200/200 runs
```

Not a rare interleaving — a certainty. The fix is a **single create-if-absent linearisation point**:
`outcome.json` is the one terminal record, and it is the only file a reader trusts. Same harness,
same 200 iterations, with each writer claiming `outcome.json` after staging its payload:

```
SINGLE OUTCOME: ambiguous/missing verdict in 0/200 runs
```

This removes the need for the publication lock the debate had otherwise converged on — but only
together with the single-outcome rule, not from `link` by itself.

---

## Phase 0 — foundation

**Phase 0 ships the engineering gate and a working executable. It does not ship empty packages.**
Claude's blind plan proposed one `doc.go` per package so the boundaries would exist; Codex and `agy`
both rejected that as exactly the unjustifiable scaffolding the brief forbade, and they were right —
an empty package gives AST-based linters nothing to analyse, so the gate would be unexercised.
Packages are created when behaviour arrives.

### Tracked files at Phase 0 exit

| File | Purpose |
|---|---|
| `go.mod` | module `github.com/hishamkaram/delegation-layer`, `go 1.27.0` declared minimum. No dependencies yet, so no `go.sum`. |
| `Makefile` | The gate. `tools fmt fmt-check vet lint test-race build vuln check`. |
| `.golangci.yml` | `default: none`, then the explicit set defended below. |
| `.github/workflows/ci.yml` | Calls `make check` on **Linux and macOS**, action SHAs pinned. |
| `.goreleaser.yaml` | Written now, validated by `goreleaser check` in the gate, **not released until Phase 5**. |
| `AGENTS.md` | The one authoritative instruction file: invariants, package ownership, Go rules, the verification gate, the prohibition on signalling. |
| `agents/_data/delegation-invariants.md` | Tri-state discipline, at-most-once launch, the publication predicate, the two-bounds rule, fail-closed config. |
| `agents/_data/adapter-contract.md` | The eight operations and what each may **not** do — chiefly that `collect` never executes a turn. |
| `agents/_data/code-quality-floor.md` | The lint categories and the local gate. |
| `.codex/config.toml` | Codex project rules. No unrestricted-sandbox default and no 12-thread setting — neither prevents a failure here. |
| `.codex/agents/go-implementer.toml`, `go-reviewer.toml` | Two roles, scoped to assigned packages. |
| `skills/add-an-adapter/SKILL.md` | The one recurring multi-step procedure this project has. |
| `scripts/verify-gates.sh` | Proves the gate **rejects**. See the exit criterion. |
| `cmd/delegate/main.go` + `main_test.go` | A real entry point: help, version, strict rejection of unknown commands. Exercises the whole gate on day one. |
| `.gitignore`, `LICENSE`, `README.md`, `docs/` | — |

`CONTRIBUTING.md` and `SECURITY.md` wait until the repo is public.

### Linters

`default: none`, then enable — each defended by the failure it prevents *here*:

| Linter | What it prevents in this codebase |
|---|---|
| `errcheck` (`check-type-assertions: true`) | An unchecked `Sync`, `Close`, `Link` or `Remove` on the publish path. The filesystem is the state machine; a dropped error there corrupts the publication contract. |
| `govet` (enable-all − `fieldalignment`) | Ordinary correctness. |
| `staticcheck` | Misuse of the standard library; dead branches. |
| `ineffassign` | A parsed status or effective-config assignment that is never read. |
| `nilerr` | Returning `nil` after a failed operation — **the boolean collapse in error form**. |
| `errorlint` + `errname` | The recovery policy branches on error kind; `%w` chains must survive. |
| `forcetypeassert` | Panics while decoding provider JSON and `pueue status --json`, whose shape the design flags as unstable. |
| `contextcheck` | See the note below — kept, but scoped. |
| `exhaustive`, `default-signifies-exhaustive: false` | A `switch` that forgets `Undetermined`. **The configuration is the whole point**: without it, one `default:` clause defeats the linter completely. |
| `forbidigo` | Makes decision 4 mechanical. Patterns on `\.Signal\(`, `\.Process\.Kill\(`, `syscall\.Kill`, `syscall\.Setsid`. |
| `nolintlint` (`require-explanation`, `require-specific`) | Suppressions that do not name their linter or say why. |
| `gocognit` 20, `gocyclo` 15 | The admission ladder and health ladder becoming unreadable nested decisions. |
| `gofumpt` (formatter) | Formatting reaching review at all. |

**Dropped from the reference:** `bodyclose` — there is no HTTP in v1 by decision
(`DESIGN.md:1305-1310`); re-add with A2A. `prealloc` — speculative allocation tuning is not a
foundation requirement.

**On `forbidigo`.** It matches source text, so deliberate indirection through a variable evades it.
That makes it a **regression guard, not a proof** — and the realistic failure it guards against is
specific: an autonomous coding agent reflexively writing `cmd.Process.Kill()` to implement a
timeout. That is the single most likely way decision 4 gets violated, and it is exactly the shape
`forbidigo` catches. The regexes must be validated against the pinned analyser's matching semantics
in Phase 0, not assumed.

**On `contextcheck` — the row where two reviewers were wrong in opposite directions.** Codex dropped
it, reasoning that watcher and worker lifetimes intentionally differ. `agy` kept it and called it
"load-bearing for enforcing the two-bounds rule." Both retracted. The correct reading:

- The watcher's cancellation must **never** propagate into provider execution — propagating it would
  defeat detachment, which is the bug the two-bounds rule exists to prevent.
- `delegate-run` establishes an **independent execution lifetime** at its entry point.
- Contexts propagate consistently **within each lifetime**.

Different roots do not justify abandoning context checking inside either program. Keep the linter,
scoped, with a documented boundary exception if the pinned analyser demands one. And note what a
linter cannot do: the property that actually matters is proven by the acceptance test — expire the
watcher, observe **zero** stop requests, then collect the naturally completed task.

### Tool versions

`golangci-lint`, `gofumpt` and `govulncheck` are **not installed on this machine**; `go`, `goreleaser`,
`gh` and `jq` are. `make tools` installs the three at pinned versions into `./bin` via `go install`,
so CI and laptop run identical binaries. Start from the reference's pins (`golangci-lint` 2.12.2,
`govulncheck` 1.1.4) and **verify them against Go 1.27.1 during Phase 0** — that compatibility is
unverified. Never `@latest`; record the version that actually worked.

### `make check`, in order

```
check: tool-versions fmt-check vet lint test-race build vuln
```

Cheapest and most deterministic first, so a failure returns in seconds rather than after the race
detector. Implemented as **sequential recipe invocations, not prerequisites**, so `make -j check`
cannot reorder them. Missing tools fail loudly rather than being skipped. CI calls this same target
rather than re-listing the steps, so the two cannot drift.

### Exit criterion

```bash
make tools && make check && bash scripts/verify-gates.sh
```

`make check` exiting zero proves only that the gate **accepts** compliant code. `verify-gates.sh`
proves it **rejects**: each known-bad fixture must compile, fail with the **expected diagnostic**,
and have a corrected counterpart that passes — otherwise a missing tool or an unrelated error could
masquerade as enforcement. Fixtures: a `.Signal(` call (`forbidigo`), a tri-state `switch` missing
the unknown case **and carrying a `default:` branch** (`exhaustive`), an ignored error (`errcheck`).
No fixture executes signalling behaviour. It runs in the recurring gate, not only at setup.

---

## Design patterns

1. **The task directory is a type.** `task.Open(root, id) (*Dir, error)`. No package outside
   `internal/taskdir` holds a task path. Prevents two packages disagreeing about a filename.

2. **Terminal records are created, never replaced — and there is exactly one of them.** The only
   commit primitive is create-if-absent: stage to a **unique** `os.CreateTemp(dir, "*.tmp")`, then
   `os.Link(tmp, name)`, then unlink the temp. There is no general `Write` on the type.

   **`outcome.json` is the single terminal record and the only linearisation point**, carrying the
   verdict and the payload's digest. The order matters: the payload (`result.txt` or
   `publish.reject`) is staged, synced, **closed**, and linked **first**; only then is
   `outcome.json` claimed. So the claim is never followed by a vulnerable write — **`outcome.json`
   existing implies its payload is already committed and immutable.** A reader trusts
   `outcome.json` and nothing else: **a payload present without `outcome.json` is
   publication-pending, not committed** — which is exactly the health ladder's publication-pending
   row, now given a mechanism. `result.txt` stays plain text the lead can read; it is a projection
   of the terminal record, not the record.

   This gives up `DESIGN.md:373`'s "no separate done flag" simplicity. That is a real cost, paid
   deliberately: without a single claim, `link` is not enough. It is atomic per *pathname*, so a
   publisher linking `result.txt` and a collector linking `publish.reject` **both succeed** —
   measured at 200/200, not a rare interleaving.

   **`EEXIST` means "a destination exists", not "my operation succeeded."** The loser reads the
   authoritative record and compares **decision content** — task identity, sealed-input digest,
   predicate version, verdict, canonical answer — **excluding incidental fields** like timestamps
   and writer identity. Matching → idempotent completion, stand down. Diverging, malformed, or
   unrelated → **preserve the winner, record an invariant fault separately, surface the conflict.**
   Never modify the terminal record, and never swallow it as a lost race: with `raw/` sealed and the
   predicate pure, two writers *must* agree, so disagreement is a broken invariant.

   *Requires* a filesystem supporting hard links, with staging and destination on the same
   filesystem. Verified on APFS; fine on ext4, XFS, btrfs, tmpfs; fails on FAT/exFAT and is
   unreliable over NFSv3/SMB. The support statement is **"tested filesystems supporting hard links
   and the required sync operations"** — *not* "local", which is neither sufficient (a local FAT
   volume fails) nor necessary (locality is not intrinsic to hard links). **Check compatibility
   before dispatch and fail closed; never silently fall back to an overwriting rename.**

   *Also requires* a scavenger for orphaned `*.tmp`: unlike `rename`, `link` does not consume the
   source, so a crash between `CreateTemp` and `Link`, or between `Link` and `Remove`, leaves one
   behind. And because `link` shares the inode, **any surviving writable handle would mutate the
   committed file** — writers are closed before commitment, without exception.

3. **Seal before signalling completion — and sealing is not syncing.** Flushing bytes is easy to
   mistake for proving no writer can append more. To seal `raw/`: drain the provider's output,
   finish every write, **close every writer**, then prohibit further mutation — and only then commit
   `provider.exit`. Recovery waits for the committed seal and **never finalises from unsealed
   bytes**. Raw output stays readable while growing, but a readable file is not sealed evidence.

4. **Three distinct numeric enums, zero = Unknown.**
   ```go
   type Liveness uint8    // Undetermined=0, Running, Ended
   type Admission uint8   // Unknown=0, NotAdmitted, Admitted
   type Publication uint8 // Unknown=0, Pending, Committed, Rejected
   ```
   A struct nobody populated reads "I don't know", never "ended". Separate types, so one cannot be
   assigned to another. Human-readable strings stay the *wire* representation; **deserialisation
   validates and rejects unknown values**, and omitted evidence stays Unknown.
   Honest limit: Go has no closed sum types. Numeric enums still admit undeclared values, and
   `admission != Admitted` still compiles. `exhaustive` plus transition tests cover what the type
   system cannot. **Do not claim Go makes the collapse impossible — it makes the obvious spelling of
   it fail to compile, and the rest is tested.**

5. **Submission authority is granted, not assumed.** `Store.PrepareSubmission` runs under the
   admission lock and returns a private-construction permit only after `submit.json` is committed
   and synced. The supervisor path requires that permit. On retry, under the same lock: no
   `submit.json` → **proved** not-admitted; matching label → admitted, attach; anything else →
   unknown, never resubmit. **Submission intent is never deleted to unstick a task.**

6. **`provider.start`, the second guard.** `submit.json` proves at-most-once *submission to pueue*;
   it does not cover the publisher itself running twice. `delegate-run` commits a `provider.start`
   intent before invoking the provider, so a second invocation may recover evidence but **cannot
   launch again**. No claim that pueue restarts the command is needed — two invocations are the
   counterexample.

7. **The publication predicate cannot refuse without a reason.** Constructors only:
   `adapter.Publishable(answer)` and `adapter.Refuse(reason)`, where `Refuse("")` is an error. The
   validated answer checks provider-specific successful completion, non-empty extracted answer, and
   absence of an abort marker. **Only that value can reach `CommitResult`.** An exit code or a
   non-empty diagnostic string cannot authorise publication. Publisher and recovery collector run
   the **same compiled predicate** over the same sealed bytes.

8. **A collector must never receive a starter.** The eight operations are implemented, but no
   consumer receives an eight-method interface. The publisher needs execution and completion
   interpretation; collection needs evidence extraction and descriptors. *Prevents an ordinary
   collection path from starting paid work through its injected dependencies.* No registry with
   `init()`, no service locator, no base adapter — `cmd/*/main.go` builds an explicit map.

9. **Two bounds, two owners, two field names.** `WatchBound` is the watcher's; `TaskBudget` is
   `delegate-run`'s. The watcher's context never enters provider execution. Budget expiry records
   the stop *request* before asking the supervisor to stop a **validated** task identity, and an
   unsuccessful stop stays visible and uncertain — it never manufactures a terminal record.
   **"Stop requested" and "termination observed" are reported separately.**

10. **Fail-closed config is a data check.** `capabilities()` returns
    `map[ConfigKey]Mapping{Flag, Verified}` with `Verified ∈ {Unverified, Documented, Executed}`.
    Absent, or `Unverified` → the dispatch fails. Decision 16's `✓`/`✓✓` become enum values nobody
    can forget to check, and the same table generates the doc. **Containment and approval are
    separate fields inside the effective config**, even though `permission` stays the public
    vocabulary — that flattening is how a wrong `✓` survived once already.

11. **`delegate-run` contains no goroutines.** It forks one child and waits. Stated as a rule so
    "every goroutine has an owner" never needs enforcing. A goroutine appearing there is a design
    change requiring a decision entry.

12. No package-level mutable state except sentinel errors. `context.Context` on everything that
    shells out. Wrap with `%w`. Never log and return the same error. **`delegate-run` never invokes
    a shell** — argv slice only; the supervisor launch line is the only shell-reparsed surface and
    it carries `--escape`.

**Rejected as ceremony:** a repository/service layering, a DI container, an `internal/errors`
package, an event bus, interfaces with one implementation and one consumer, and — after the `link`
measurement — the publication lock.

---

## Phases

Every exit command is **proposed future work**, not a command claimed to pass today. Each phase also
re-runs `make check`.

### Phase 1 — the durable protocol, with no external CLI

- **Goal:** make duplicate launch and false publication hard to *express*, before any paid execution
  exists.
- **Built:** `internal/task` (values and pure rules, no I/O), `internal/taskdir` (the file protocol,
  leases, create-if-absent commit, sealing), `internal/config` (strict validation), the tri-state
  types, submission permits, `provider.start`, outcome reduction, identity-bound recovery.
- **Exit:** `go test -race -count=1 -v ./internal/task ./internal/taskdir ./internal/config`
  — concurrent dispatch, every submission crash boundary, concurrent publisher/collector, torn
  records, the health-ladder precedence table. **Every ambiguous admission case must produce zero
  additional submissions.** Fault injection at named boundaries *plus* helper processes that exit at
  checkpoints, so locks release naturally rather than by an injected error.
- **Not yet:** adapters, pueue, budgets, live tests.

### Phase 2 — a supervised task completing, against fakes

- **Goal:** prove the executable and the subprocess boundaries before provider quirks arrive.
- **Built:** `cmd/delegate-run`; `internal/pueue` (subprocess client, versioned JSON decoding);
  `dispatch`, `status`, `collect`, `cancel`, `logs`; the wall budget; watch detachment; the fake
  provider and fake supervisor; publication recovery.
- **Exit:** `make acceptance-supervisor` — hermetic subprocess tests plus an **isolated** pueue
  suite (dedicated config, state directory and endpoint; acceptance setup **fails** if isolation
  cannot be established). Proves `--escape` handling, label reconciliation, dispatcher exit followed
  by successful collection, repeated collection executing **no** provider turn, and watch expiry
  producing **zero** stop requests. Stop routing is checked against the fake supervisor; the live
  portion never tests signal behaviour.
- **Not yet:** paid providers, token caps, release automation.

`logs` lands here, earlier than `DESIGN.md` suggests: its descriptor is part of the recovery
interface, so it is cheap to establish alongside the supervisor seam.

### Phase 3 — the first adapter, `antigravity:print`

- **Goal:** survive `agy`'s success-shaped timeout failure without publishing an answer.
- **Built:** all eight operations for `agy`; version-qualified capabilities; `--print-timeout` tied
  to the budget; `permission: workspace-write` → `--sandbox`; `read-only` declared **unsupported**;
  brief delivered on **stdin, closed** (measured above); inline-response parsing; usage accounting;
  conversation and transcript descriptors.
- **Exit:** `make acceptance-agy` — fixtures plus required live cases: a successful answer; a
  deliberate print timeout; `read-only` refused **before launch**; workspace-write containment; and
  collection performing no new turn. The timeout case asserts **no `result.txt` despite exit 0 and
  `SUCCESS`**, preserves the stderr marker, and labels timed-out usage unreliable. Brief delivery is
  demonstrated with quotes, newlines and shell metacharacters. Missing prerequisites **fail** this
  target rather than skipping.
- **Not yet:** Codex, automatic resume, transcript-database decoding.

### Phase 4 — the second adapter, Codex: the falsification test

- **Goal:** prove the interface is not shaped around `agy`.
- **Built:** `internal/provider/codex` — `codex exec`, JSONL/session parsing, output-file staging
  (under `raw/`, never straight to `result.txt`), `read-only` mapping, resume availability,
  transcript discovery bound to the actual session.
- **Exit:** `make acceptance-codex`, **plus a structural assertion**: the phase's `git diff --stat`
  touches only `internal/provider/codex/**`, one line of `cmd/*/main.go`, and tests. A required
  change in `internal/taskdir` or `internal/pueue` means the interface was wrong — found for the
  price of one adapter. Continuation creates a **new task**; the original result stays
  byte-identical.
- **Not yet:** Claude background mode, OpenCode, Gemini, Kimi, Pi, router aliases.

### Phase 5 — consumption and release

- **Goal:** prove a lead can consume the public surface, then ship both binaries.
- **Built:** a subprocess-only fan-out/collection acceptance test; stable machine-readable CLI
  payloads; recovery and operator docs; the goreleaser run; archives containing **both** binaries
  plus checksums; Homebrew formula with a `pueue` dependency; documented `go install` for both
  commands.
- **Exit:** `make acceptance-release && make acceptance-install VERSION=...` — consumer acceptance,
  both provider targets, `goreleaser check`, snapshot packaging, checksum validation, and an
  **executable smoke test on each supported OS family**. *Cross-compilation alone is not runtime
  acceptance, and snapshot packaging is not shipment* — completion requires a published version
  installed cleanly through the advertised channels.

### Phase 6 — port `claude-codex-duo`'s Phase 2 fan-out

`DESIGN.md` milestone 3. Exit: the review skill runs against the layer **with no edit to the skill**.
Note Phase 5's consumer-shaped test does **not** prove this port works; it proves the surface is
consumable.

### Phase 7 — the lead loop

`DESIGN.md` milestone 4. Only then, and only after that, A2A over HTTP if something remote needs to
call in.

### Not a phase — the OpenCode spike

`DESIGN.md` 2b carries two possibly-broken upstream issues. A timeboxed spike whose only output is a
note in the design. Never on the critical path.

---

## Ordering and blockers

`0 → 1 → 2 → 3 → 4 → 5 → 6 → 7`. The spike runs in parallel.

**`DESIGN.md` says open questions 1 and 6 both block milestone 1. This plan disagrees on scope**, and
the disagreement is the main structural change: the design fuses the task record, the publisher and
the first real adapter into one milestone, so nothing is falsifiable until a live `agy` run — which
puts a product decision on the critical path from day one. Split them and the blockers move.

**Open question 1 — Task = one turn?** Blocks **Phase 1** (earlier than the design says): it decides
whether the persistent schema carries a conversation field. **Resolved: one turn.** A continuation is
a new task referencing an existing conversation handle; the original identity and result stay
immutable. This is not merely the recorded leaning — it is **forced by measured provider behaviour**:
after a timeout, resuming the `agy` conversation incremented `num_turns` to 2 and returned `"NONE"`
for the lost work (`DESIGN.md:256-260`). Resumption recovers the session, never the lost turn.
*Coherence check, not a substitute for the decision:* complete A, create B from A's handle, collect A
again, assert A's bytes and provider-launch count are unchanged.

**Open question 6 — `read-only` on `agy`?** Blocks **Phase 3 only**. **Resolved: option (b)** — the
adapter declares `read-only` unsupported and `workspace-write` supported, backed by the verified
`--sandbox` measurement. A dispatch at the layer default is refused explicitly and on the record. The
default is **not** silently changed. Cheapest proof is a fake invocation counter showing **zero**
launches for a default request; the containment claim then needs a controlled live workspace fixture,
not a repeat of the broad sandbox investigation.

**Additional blockers found during planning:**

| Uncertainty | Blocks | Cheapest resolution |
|---|---|---|
| `link`/rename/sync/lock semantics on supported filesystems | Phase 1 exit | Temp-directory tests on macOS and Linux. **Advertise local filesystems only**; `link` is not guaranteed across all network filesystems, and the design does not claim them. |
| Supervisor identity visible inside `delegate-run` | Phase 2 | A finite fake task with a unique label on the isolated pueue instance; observe label visibility and identity reconciliation without signalling. Missing identity means **no guessed cancellation target**. |
| Budget semantics | Phase 2 | v1 is an explicit finite **wall-clock** deadline starting at provider launch. Test with a fake clock. |
| Pinned linter versions against Go 1.27.1 | Phase 0 exit | The diagnostic-specific fixtures in `verify-gates.sh`. |

---

## Testing strategy

| Tier | Needs | Covers |
|---|---|---|
| **Unit** | nothing | strict config, enums and transition tables, provider-envelope decoding, publication predicates, requested/effective config, health precedence, descriptors |
| **Hermetic integration** | nothing (fakes built by the test) | `delegate-run` end to end, create-if-absent commit, `publish.reject`, recovery from sealed `raw/`, supervisor JSON decoding |
| **Isolated acceptance** | a real `pueued` on a **dedicated** config/state/endpoint | escaping, labels, logs, launcher-independent collection |
| **Live** | `agy` / `codex`, `-tags=live`, opt-in only | authentication, model/effort behaviour, permission enforcement, session identity, real completion envelopes, the timeout shape |

**Fakes are real executables, not fake HTTP servers.** `internal/testutil` holds fixture helpers;
subprocess tests use a helper mode of the test binary itself, so nothing needs installing and it
cross-builds. **Executable paths are injected directly — never by shadowing the developer's real
provider through `PATH`.**

The fake provider takes a scenario, records argv, stdin and invocation count, writes scripted output,
optionally waits on a test-controlled barrier, and exits itself. Scenarios: valid completion;
**exit 0 + `SUCCESS` + empty response** (the `agy` trap); non-empty failure; timeout markers;
malformed and truncated output; delayed session identity. The fake supervisor additionally
impersonates pueue accepting a task but losing the response, returning no label, returning duplicate
labels, changing version, and emitting an unknown status shape.

Assertions worth naming: existing submission intent plus a reachable empty queue never produces a
second submission; unsupported configuration produces **zero** external calls; error text and
aborted partial answers never become results; result existence outranks missing exit metadata;
collection racing publication cannot replace a terminal decision; watch expiry produces zero stop
requests; repeated publisher invocation produces **at most one** provider start.

**Recorded fixtures** under `testdata/`: real captures of `agy --output-format json` and
`pueue status --json`, versioned. A decode test over these turns the pueue-instability risk into a
red CI run instead of a production surprise.

CI stays green without providers or `pueued` because the mandatory suite is **genuinely hermetic,
not because it skips its assertions**. Required acceptance targets fail on missing prerequisites
rather than converting skipped live verification into a pass.

---

## Cut list

1. **`tools/maintainability`** — the reference's 317-line bespoke AST scanner with per-prefix
   exemptions. `gocyclo`/`gocognit` cover function complexity; the rest is policy machinery.
   **Re-add trigger:** any non-generated `.go` file over 600 lines.
2. **Most of the instruction scaffolding.** The reference carries 36 files across `skills/`,
   `agents/`, `agents/_data/` and `.codex/agents/`; ten of its eleven skills are 10 lines that say
   "read these three files." Keep one `AGENTS.md`, three `_data` contracts, two codex agents, one
   skill. Also drop the reference's unrestricted-sandbox default and 12-thread setting.
3. **SQLite and any storage abstraction.** The task directory is the only state — that is the
   thesis. A second store adds migrations, a driver, and dual-state synchronisation for nothing.
4. **`gopsutil`, CPU sampling and process-tree inspection in v1.** The design makes silence
   observational and forbids it from deciding anything (`DESIGN.md:446`, `:580`). Diagnostics that
   decide nothing do not justify a dependency and a phase. Keep log activity and structured events,
   with uncertainty explicit — **and do not replace CPU heuristics with the equally unjustified
   claim that log timestamps fully diagnose a quiet task.**
5. **Arbitrary `raw` argv in v1** — a deliberate narrowing of `DESIGN.md:925`. Uninterpreted flags
   can defeat `--sandbox`, `-o` or `--print-timeout`, which voids the effective-config guarantee the
   capability table exists to make. Supporting it honestly needs per-provider conflict and
   precedence analysis. Reject non-empty `raw`; `--allow-unsupported` does **not** bypass this.
6. **Token and dollar ceilings.** See the design amendment below — there is no enforcement source.
7. **`bodyclose` and anything HTTP-shaped**; `CONTRIBUTING.md` / `SECURITY.md` until public;
   release-note duplication and the reference's browser-image pipeline.
8. **The publication lock** — removed by the `link` measurement, not by argument.

---

## Design amendments this plan forces

Three defects in `DESIGN.md`, each found by one participant and independently verified.

1. **`DESIGN.md:1335` is stale.** It says the two blockers are *"#1 and #7"*. Question 7 is the
   runtime question, struck through and redecided as Go at `:1382`; `:1393` correctly names 1 and 6.
   The correction should mark **both** now-resolved rather than swapping "7" for "6".

2. **The token budget has no enforcement source.** `:1412-13` names `agy`'s `usage` as *"the task
   budget's data source"*; `:~242` says usage arrives **in the result**; `:~776` proposes enforcing
   the budget with *"a poller that calls `pueue kill`"*. A poller can only poll a quantity
   observable **while the turn runs** — token usage arrives with the answer, after the only moment
   killing would have saved anything. Timed-out runs can even report zero having spent tokens
   (`:253`). **v1 advertises:** post-run usage accounting where available, with timed-out usage
   marked unreliable; token/dollar ceilings **rejected before admission**; a **wall-clock** deadline
   as the only enforced bound. And not even that unconditionally — a deadline observer or the
   supervisor can fail, so *stop requested* and *termination observed* are reported as separate
   facts. This belongs in the public contract, not only the risk register.

3. **The publication primitive is wrong, and the completion contract needs one more file.**
   `result.tmp` → `result.txt` by rename is last-writer-wins and can overwrite a committed terminal
   record, contradicting `:1324`. Replace with unique staging plus `link`, **and add `outcome.json`
   as the single authoritative terminal record** — which knowingly gives up `:373`'s "no separate
   done flag". Both are required: `link` alone leaves `result.txt` and `publish.reject` racing on
   different pathnames, which both win, 200/200. See *Measured during planning*.

   Also add the input contract the design never states: the brief is delivered on **finite stdin
   closed at EOF** (or a provider-supported file argument), never argv; and `delegate-run` never
   invokes a shell.

---

## Risks

1. **Provider behaviour does not support the contract we advertise.** Permission flags may not
   enforce what they appear to; completion shapes change; `agy` churns versions fast. The design
   already records one withdrawn `✓`. **Early signal:** a controlled adapter probe cannot
   demonstrate its advertised intent, or existing fixtures stop matching a newly observed version.
   Response is to disable that mapping and refuse the dispatch — never to approximate it.

2. **A recovery path duplicates paid work or publishes incomplete evidence.** Most likely at the
   seam between filesystem persistence and supervisor acceptance, or between raw output and result
   commit. **Early signal:** any fault-injection case increments the provider invocation count
   twice, permits two terminal writers, or maps missing evidence to failure or non-admission. Phase
   1 must catch this before any provider exists.

3. **The supervisor cannot deliver the durability the design assumes.** pueue's own changelog says a
   4.0 upgrade *"completely breaks pre-v4.0 states"* and *"the state will be wiped clean"* —
   in-flight tasks vanish from the supervisor while their directories still read `running`, i.e.
   `admission-unknown` for every one. **Early signal:** the versioned decode test fails after a
   `brew upgrade`, or identity cannot be reconciled. The mitigation is that the test exists before
   we depend on the shape, and that unknown stays unknown rather than becoming "not admitted".

4. **The adapter interface is shaped around `agy` without anyone noticing.** **Early signal:** Phase
   4's structural `git diff --stat` assertion, written precisely so this fails loudly instead of
   being absorbed into "small refactors along the way".

5. **Recovery finalises evidence that is not actually sealed.** Syncing bytes is easy to mistake for
   proving that no writer can append more — and a wrong seal turns the pure-predicate guarantee,
   which the whole lock-free publication protocol rests on, into an assumption. **Early signal:** a
   deterministic recovery test produces a terminal verdict before the seal exists, or observes
   different evidence digests before and after sealing. Exercise delayed stderr and interrupted
   publication through fixtures and injected failures; no live signal experiments are needed.

---

## Disagreements and how they fell

Recorded because agreement is not evidence, and because the process is only worth repeating if its
failures are visible.

| Row | Positions | Outcome |
|---|---|---|
| Phase 0 ships empty packages? | Claude yes; Codex and `agy` no | **Claude conceded.** Empty packages give AST linters nothing to analyse. |
| Phase 0 exit criterion | Claude: prove rejection; Codex and `agy`: `make check` | **Claude's held**, strengthened by Codex: expected diagnostic + passing counterpart + in the recurring gate. |
| `contextcheck` | Codex drop; `agy` keep, "load-bearing"; Claude keep | **Both retracted, from opposite directions.** Kept, scoped per-lifetime. The strongest evidence in the set that the debate did work. |
| `forbidigo` | Claude only; both others used prose | **Adopted.** Guards the realistic failure (an agent writing `Process.Kill()`), not the adversarial one. |
| `exhaustive` | Claude and Codex; `agy` omitted it | **Adopted**, with Codex's `default-signifies-exhaustive: false`, without which it does nothing. |
| String vs numeric enum | `agy` string; Claude and Codex numeric | **`agy` retracted** — `""` is a fourth invalid state, which is the collapse its own plan forbade. |
| `raw` passthrough | Codex cut it; Claude and `agy` kept it unexamined | **Codex's held.** |
| Token budget | Codex alone saw it; Claude and `agy` missed it | **Confirmed by independent read**, and the defect is larger than stated. |
| Brief transport | Codex "impossible"; `agy` "contradiction"; Claude "over-read, supervisor-scoped" | **All three wrong.** Claude's scoping was refuted by `:673`; the contradiction dissolved on measurement — `agy` reads stdin. |
| Publication lock | Codex for; `agy` against; Claude against | **Codex produced a real interleaving**; the `link` measurement then removed the need for the lock. All three had specified the wrong primitive (`rename`). |
| Does `link` alone suffice? | **Claude claimed yes, and was wrong** | `agy` and Codex independently found the two-pathname split-brain; Claude measured it at **200/200**. Fixed by a single `outcome.json` claim, 0/200. The last position to fall in the debate, and it was the one backed by the author's own measurement. |
| Live `pueued` | `agy` the developer's own; Codex isolated | **Codex's held.** Claude conceded before it was raised. |
| Release phase | `agy` had none | **`agy` retracted.** |
| CI platforms | `agy` Linux-only | **`agy` retracted.** macOS is the platform whose supervision semantics the design measured. |
