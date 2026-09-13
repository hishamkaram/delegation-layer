# Delegation layer — design notes

> Working name only; the directory is trivially renamable. Written 2026-09-12.
>
> **Status: design complete, not built.** No code exists yet. The supervisor, runtime, first adapter,
> config model and status payload are settled, and the load-bearing ones were verified by live
> testing on this machine, not taken from docs.
>
> **Revised 2026-09-13 after an adversarial review against Codex** (`/tmp/codex-debate/
> delegation-layer/design-ready-for-m1-20260913-115839/DEBATE.md`). The thesis survived unchallenged
> — resumption-by-reference over replay, a supervisor we do not write, file-based state, intent-based
> config. Three specification defects did not, and are corrected here: the document contained two
> incompatible publication rules; the at-most-once admission rule could duplicate paid work; and the
> first adapter's `permission: read-only` mapping was marked verified when it does not deliver
> read-only. See *The publisher*, *At-most-once launch*, and the config table's footnote.
>
> **Two decisions are still open and both block milestone 1**: *is a Task one turn or a
> conversation?* and *what happens to `permission: read-only` on `agy`?* (open questions 1 and 6).
>
> `agy --sandbox` was then **measured** rather than argued about (2026-09-13, `agy` 1.2.2, with a
> control run): it is a real filesystem sandbox after all, but a `workspace-write` one, not
> read-only. See *`--sandbox` measured*.
>
> Diagrams: `diagrams/components.html` (system components) and `diagrams/lead-interface.html`
> (how the lead AI talks to the layer) — **both regenerated 2026-09-13** against this revision.

## Goal

A frontier model leads; cheaper agents from any provider do the donkey work. The lead
dispatches bounded tasks to worker CLIs, collects their results, and integrates them.

Two properties that nothing off the shelf gives us together:

1. **A worker turn survives its launcher.** If the lead hits a context limit, crashes, or the
   terminal closes, the worker's finished answer is still collectable instead of lost.
2. **One task model across providers.** Claude Code, Codex, Gemini and others behave differently;
   the lead should not care which one served a task.

Separate from `claude-codex-duo`. That plugin becomes a *consumer* later — which is also the test
of whether the seam is in the right place.

---

## Verified facts (2026-09-12)

Two tiers of confidence, and the difference matters:

- **Checked against primary docs** — most of what follows. CLI flags move fast; re-verify before
  implementing, and pin the versions each adapter was tested against.
- **Tested live on this machine** (macOS 26.6.2, arm64) — marked as such where it appears. Three
  things were established this way, and all three contradicted or sharpened what the docs said:
  **pueue 4.0.4 and task-spooler 1.0.4** (kill reach, JSON, output retrieval),
  **`agy` 1.2.1** (the `--print-timeout` failure shape, the result JSON, transcript storage,
  `remote-control`'s actual purpose), and **the macOS process-tree ceiling** (a `setsid`-escaped
  descendant survives every supervisor tested). A fourth was added on 2026-09-13:
  **`agy` 1.2.2's `--sandbox`** (a real filesystem sandbox, `workspace-write` not read-only,
  established with a control run).

The tested tier is why several claims here changed after first being written. Where a doc and a test
disagree, the test wins and the correction is recorded rather than quietly edited.

### Claude Code

- `--bg` / `--background`: "Start the session as a background agent and return immediately.
  Prints the session ID and management commands." **"Can't be combined with `-p`/`--print`."**
- A **supervisor daemon** hosts background sessions: keeps them running after the terminal closes,
  survives machine sleep and auto-updates, "automatically restarts stopped sessions if they exit
  unexpectedly", stops idle unattached sessions after ~1 hour.
  - Ceiling: sessions stop on an explicit machine restart, and after 48h offline.
- Shell surface: `claude agents --json` (`--all`, `--cwd`), `claude attach <id>`, `claude logs <id>`,
  `claude stop <id>`, `claude respawn <id>`, `claude rm <id>`, `claude daemon status`.
- State per session: `working | blocked | done | failed | stopped`; `status`: `busy | waiting | idle`;
  `waitingFor` (e.g. `permission prompt`); `id` (short) and `sessionId` (UUID).
- Docs say: read state via `claude agents --json`, **do not parse** `~/.claude/jobs/<id>/state.json`.
- `--session-id <uuid>` — **client-supplied session id** (must be a valid UUID).
- Print-mode-only flags: `--json-schema`, `--max-budget-usd`, `--max-turns`, `--permission-prompts`.
- `-p` sessions persist and are resumable (`--resume`), unless `--no-session-persistence`.
- Background sessions move into an isolated git worktree under `.claude/worktrees/` before editing.

### Codex CLI

- `codex exec` (alias `codex e`): non-interactive. `--json` (NDJSON events),
  `-o` / `--output-last-message <file>` (writes the final message to a file),
  `--sandbox read-only|workspace-write|danger-full-access`, `--cd`, `--ephemeral`.
- `codex exec resume --last | <SESSION_ID>`, `--all`; `codex resume`, `archive`, `unarchive`, `delete`.
- Sessions persist as JSONL under `~/.codex/sessions/`.
- **No background mode, no detach, no job registry.**

### Gemini CLI

- Headless with `-p`/`--prompt` or a non-TTY; `--output-format json`.
- `--resume` / `-r <session-id>`, `--list-sessions`; sessions recorded to disk.
- Caveat to verify per version: gemini-cli issue #14435 reports headless JSON not surfacing the
  session id needed for resume.

### Kimi Code CLI (Moonshot)

- TypeScript, npm, Apache-2.0. Claude-Code-shaped, so it drops into the print-mode adapter.
- `--print` ("implicitly enables `--afk`"), `-p/--prompt TEXT`, `--quiet`.
- `--output-format text|stream-json`, `--input-format text|stream-json`, `--final-message-only`
  — the last one gives `result.txt` directly, the same gift as Codex's `-o`.
- `-C/--continue` (previous session in this cwd), `-S/-r/--session|--resume [ID]`, `-w/--work-dir`.
- `--yolo` (auto-approve tool calls, user still reachable for `AskUserQuestion`);
  `--afk` (auto-approve **and** auto-dismiss `AskUserQuestion`) — their version of deleting the
  blocked state.
- `kimi acp` (ACP server mode; the `--acp` flag is deprecated), `--wire` (experimental).
- **No background or detach mode.** Note: the `kimi-cli` repo is being wound down in favour of
  "Kimi Code" — re-verify the package before writing the adapter.

### Pi (earendil-works)

- TypeScript, MIT, `@earendil-works/pi-coding-agent`. Minimal by design: four default tools
  (read, write, edit, bash), 15+ providers incl. local via Ollama/LM Studio. v0.84.2, 2026-08-14.
- `-p "prompt"` one-shot; `-c` continue most recent; `-r` browse past sessions;
  `--no-session` ephemeral.
- `--mode rpc` — JSON over stdin/stdout for non-Node hosts.
- **`--tools read,grep,find,ls`** — tool restriction at launch. This is a real answer to open
  question 2 (who owns isolation): a read-only worker enforced by the CLI, not by us.
- Sessions persist as JSONL. **No background or detach mode.**

### OpenCode

**The only provider besides Claude `--bg` whose session outlives its launcher** — and here the
server is one *we* run, so its lifetime is ours to reason about rather than a vendor daemon's.

- `opencode serve` — "Start a headless OpenCode server for API access." Flags: `--port`,
  `--hostname`, `--mdns`, `--mdns-domain`, `--cors`.
- `opencode run [message..]` — "Run opencode in non-interactive mode by passing a prompt directly."
- `--attach` — "Attach to a running opencode server" (e.g. `--attach http://localhost:4096`;
  also avoids cold-boot cost across many runs).
- `-c/--continue`, `-s/--session <ID>`, `--fork` ("Fork the session when continuing"),
  `--format default|json` ("raw JSON events"), `--auto` ("Auto-approve permissions that are not
  explicitly denied"), `-m/--model provider/model`, `--agent`, `--dir`, `--title`.
- `session list` (`-n/--max-count`, `--format`), `session delete <ID>`.
- Server exposes an OpenAPI 3.1 spec at `/doc`; SDK has `session.abort`.
- Gap to solve in the adapter: the docs do not say how to get the session id back out of a
  non-interactive `run`. Likely `--format json` events or `session list -n 1`; verify.

**Two open issues that temper the durability case — verify before betting on the server:**
- **#29894** — `session.abort` silently no-ops when OpenCode runs as a server driven over the SDK.
  That is exactly the mode we would use, so cancel may not work there.
- **#19023** — sessions permanently stuck after a server restart or stream interruption; no startup
  recovery for orphaned messages/tool parts.

Net: OpenCode stays a target, but it is **not** a safe harbour that lets us skip owning the durable
path ourselves. That path is pueue, which was tested here and passed.

### Aider — **excluded from scope** (decided 2026-09-12)

Aider is an editor, not an answerer: its product is a git commit, no session id, no headless resume,
and it still prompts interactively under `--yes-always` (issue #426). Supporting it would force a
second `result_shape` (`mutation`) and a second collect path for one tool.

**Dropping it means every worker returns a message and `result.txt` is the universal contract.**
`result_shape` is removed from the capability table; there is one collect path, not two.

### Models that are not CLIs — DeepSeek, GLM, MiniMax, Kimi-as-a-model

- Reached by pointing `ANTHROPIC_BASE_URL` + `ANTHROPIC_MODEL` at a compatible endpoint, or via an
  OpenAI-compatible gateway (Atlas Cloud, Tencent TokenHub, SiliconFlow, ofox …).
- MiniMax publishes an Anthropic-SDK-compatible endpoint directly.
- No first-party *terminal agent* for GLM, DeepSeek or MiniMax. They appear as models inside other
  people's multi-provider harnesses.
- Consequence: **zero new adapters.** These are `ccr:<alias>` entries through the router already
  maintained for `claude-codex-duo`, running the existing print-mode path with different env.
  Four provider names for the cost of a config file.

#### GLM / Z.ai specifically — checked, because "GLM has a CLI" is half true

Z.ai ships two first-party tools, and **neither is a headless coding-agent CLI**:

1. **ZCode** — the real first-party agent, launched 2026-07-02. An **Electron desktop app**
   (macOS/Windows/Linux), an "Agentic Development Environment" with the agent conversation at the
   centre and file manager, terminal, git panel and browser preview around it. BYOK for Anthropic,
   OpenAI, DeepSeek, Moonshot, MiniMax and any compatible endpoint; MCP support. **No official CLI.**
2. **`@z_ai/coding-helper`** (`chelper`, Node ≥18) — first-party, but an **installer/configurator**,
   not an agent. It "quickly loads GLM Coding Plan into your favorite Coding Tools", configuring
   **Claude Code, Codex, OpenCode, Crush, Factory Droid**. Commands: `init`, `auth`, `doctor`,
   `lang set`.

Tool 2 **strengthens** the `ccr:<alias>` conclusion rather than undermining it: Z.ai's own supported
route to a terminal *is* the base-URL swap into someone else's CLI. It also independently confirms
OpenCode as a mainstream target.

Third-party GLM CLIs exist and should be **avoided for this layer**:
- `kingsword09/zcode-cli` (npm `zcode-app-cli`) — extracts ZCode Desktop's bundled runtime and
  launches it as a child process. Has `--prompt`, `--print`, `--json`, `--target`. But its README
  states it is "not affiliated with or endorsed by Z.ai", and it depends on a desktop app's
  internals, which can change on any update. A poor foundation for a durability layer.
- `glm-acp-agent` — third-party ACP agent with GLM as the reasoning core.

### The orchestrator field (competitive check)

Orca (Stably), Multica, Paseo, Agent Orchestrator, herdr — all do parallel worktree fan-out across
Claude Code / Codex / OpenCode / Cursor. **None of them claim durability.** The gap this project
aims at is still open.

### Antigravity CLI (`agy`) — Google

**Verified on this machine 2026-09-12, `agy` 1.2.1 (Homebrew).** The best-equipped print-mode
provider in the set: the only one with all three config intents natively.

- `-p` / `--print` / `--prompt` — "Run a single prompt non-interactively and print the response".
- `--output-format text|json|stream-json`, `--input-format text|stream-json`.
- `--json-schema <string|path>` — enforce structured output. Optional per task; does not change
  decision 1 (no layer-wide output contract).
- `-c/--continue` (most recent), `--conversation <ID>` (resume by id).
- **`--model`** and **`--effort low|medium|high`** — the effort vocabulary matches our intent
  exactly, no translation needed.
- `--dangerously-skip-permissions` (auto-approve all tool permissions), `--sandbox` — whose help text
  says "terminal restrictions" and **undersells what it actually does**; measured below.
- `--mode accept-edits|plan` — agent execution mode; `plan` is useful for a plan-only worker.
- `--add-dir` (repeatable), `--project`, `--agent`, `--new-project`, `--disable-slash-commands`.
- Subcommands: `models`, `agents`, `mcp`, `plugin`, `remote-control`, `update`.
- Session id: `conversation_id` in the JSON output.
- **No documented background/detach mode**, so `owns_job: false` — supervised by pueue like the rest.

**`--print-timeout` defaults to `5m0s`, and that is a landmine.** It is the *provider's* watch bound;
left unset it silently becomes the *job's* bound and long tasks are cut at five minutes. **The
adapter must always pass it explicitly, tied to the task budget.** This is the two-bounds rule
appearing inside a provider flag — exactly the confusion this design exists to prevent.

#### Tested live, 2026-09-12 — both open questions answered

**The result JSON is the richest of any provider tested.** A successful run returns:

```json
{"conversation_id":"5988734b-…","status":"SUCCESS","response":"OK\n",
 "duration_seconds":2.4,"num_turns":1,
 "usage":{"input_tokens":13108,"output_tokens":25,"thinking_tokens":24,
          "cache_read_tokens":0,"total_tokens":13133}}
```

Two consequences: `response` is **inline**, so `result.txt` is written from the JSON with no `-o`
flag needed; and `usage` gives **real token counts in the result** — the only provider so far that
does. That is the natural data source for the task budget.

**`--print-timeout` elapsing ABANDONS the turn, and reports success.** Measured with a 10s timeout
on a long generation:

| Signal | Value on timeout |
|---|---|
| exit code | **0** |
| `status` | **`"SUCCESS"`** |
| `response` | **`""`** — empty |
| `duration_seconds`, all `usage` counters | **0**, despite tokens actually being spent |
| stderr | `[agy] print timeout after 10s with turn in progress; returning partial output` |

And the work is genuinely gone, not still running: the conversation DB's mtime froze at the instant
the CLI exited, and resuming the conversation and asking how much of the essay had been produced
returned **"NONE"**. The conversation itself survives and resumes fine (`num_turns` went to 2).

**This is the exact failure shape this design exists to prevent** — a success-looking result with
silently missing content, the same family as the `--escape` truncation. It is also the two-bounds
bug shipped as a default: a *watch* bound (5 minutes) silently acting as the *job's* bound.

**Adapter rules this forces (non-negotiable):**
1. Always pass `--print-timeout` explicitly, tied to the task budget. Never inherit the 5m default.
2. **Never trust exit code or `status`.** An empty `response` with `SUCCESS` means timed out.
3. **Do not write `result.txt` when `response` is empty** — that would make the completion signal lie.
4. Parse stderr for the `[agy]` marker and record it in `meta.json`.
5. Treat `usage` as unreliable on a timed-out run; it reports zeros for tokens that were spent.

#### `--sandbox` measured, 2026-09-13, `agy` **1.2.2** — it contains writes, but it is not read-only

The adversarial review found the config table claiming `permission: read-only` → `--sandbox` as
verified, on the strength of a flag name. Neither side had executed anything. So this was measured:
a probe attempting one write at four locations, run by `agy -p` from a workspace directory, **with
`--dangerously-skip-permissions` also passed** so that approval policy could not be confused with
containment. Then the identical run without `--sandbox` as the control.

| write target | `--sandbox` | control (no `--sandbox`) |
|---|---|---|
| inside the workspace | **WROTE_OK** | WROTE_OK |
| a sibling directory outside the workspace | **DENIED, `EPERM`** | WROTE_OK |
| `$TMPDIR` | **WROTE_OK** | WROTE_OK |
| a dotfile in `$HOME` | **DENIED, `EPERM`** | WROTE_OK |

**Three conclusions, in order of importance:**

1. **`--sandbox` is a real filesystem sandbox**, not merely terminal restrictions. The help text is
   wrong-by-omission; the vendor's sandbox page is accurate ("commands can write to your workspace,
   temp directories, and common build caches"). The control proves the denials are attributable to
   the flag and to nothing else.
2. **It is `workspace-write`, not `read-only`.** The withdrawn ✓ stays withdrawn — mapping it to
   `read-only` would promise containment the flag does not provide.
3. **`agy` separates containment from approval; Codex fuses them.** `--sandbox` still denied writes
   while `--dangerously-skip-permissions` was auto-approving every tool call. Codex spells both on
   one flag (`--sandbox read-only|workspace-write`). **That is a modelling gap in this design's
   `permission` intent, not a provider quirk** — see the config table's *two axes* note.

**Version churn worth recording:** this document's live testing was on 1.2.1 on 2026-09-12; the
installed binary was **1.2.2** one day later. That is the C-12 instability concern showing up
on schedule, and it is why a capability is re-probed per adapter version rather than trusted from a
table.

**Transcripts: `~/.gemini/antigravity-cli/conversations/<conversation_id>.db`** — one **SQLite** file
per conversation, mode 600, tables `trajectory_meta, steps, gen_metadata, executor_metadata,
parent_references, trajectory_metadata_blob, battle_mode_infos`. The step content is a binary
protobuf-ish blob, not readable text. So `transcript` is `{kind: "file", path}`, but **the
recovery story is much weaker here than Codex's JSONL** — salvaging a lost answer means decoding
blobs. Treat antigravity transcripts as low-value for recovery and prefer resume-by-id instead.

**`agy remote-control` is NOT job supervision** — so no third `owns_job: true` mode. Its flags give
it away: `--name` sets "this machine's instance name shown in Remote Control", and it registers to
**start at boot** unless `--session` scopes it to the login session. It registers the *machine* for
remote operation (pairing with `mic-serve`), not a task. Not started here: a boot-time registration
is a global system change, and it would not serve this purpose anyway.

**Correction:** an earlier note here said `agy` was unauthenticated because `agy models` errored with
"Please sign in". That was wrong — `agy -p` ran fine. The failure is specific to the `models`
subcommand, not the auth state. A probe must therefore never infer "unavailable" from one
subcommand's error; that is the plugin's probe lesson exactly.

### A2A protocol

- v1.0 in 2026, governed under the Linux Foundation (hosted since June 2025). JSON-RPC 2.0.
- Task lifecycle, eight states: `submitted, working, input_required, auth_required, completed,
  failed, canceled, rejected`.
- SSE streaming plus webhook push notifications for long-running / disconnected work.
- No durability guarantees — it standardizes *talking*, not *surviving*.

### Durable execution engines

- DBOS: embeddable library, state in Postgres. Restate: single-binary sidecar. Temporal: server.
- Consistent verdict across comparisons: **SQLite covers ~90% of durable-workflow needs**; engines
  earn their complexity only across services or regions. We are single-machine.
- None of them help with the actual problem: their recovery primitive is **retry**, and an agent
  turn is expensive, side-effecting and non-deterministic. Replay is permanently unavailable to us.

### Prior art: `amElnagdy/delegate-skills`

- 18 per-CLI relays (`relay.mjs`), dependency-free Node. 1.9k stars, MIT, created 2026-06-14,
  last push 2026-08-31, 28 open issues, CI smoke tests.
- Genuinely well built: writes `result.json` on every outcome including when the relay itself is
  killed; no network/telemetry; injection-aware token regexes; spawns `detached: true` so
  `process.kill(-pid)` targets only the child's *own* process group (the safe form).
- **Opposite architecture to ours**: synchronous polling, no detach/attach, `--timeout` kills the
  child (SIGTERM→SIGKILL), and the relay forwards its own death to the CLI. Lead dies → worker dies.
- Best use: its per-CLI knowledge is a catalog worth borrowing (MIT), not a foundation to build on.

---

## The design

### Core insight

**A worker's finished answer should be a file, and the file's existence should be the completion
signal.** Everything else follows from that.

```
tasks/<task_id>/
  brief.md        # what the worker was asked
  task.json       # requested config: model, permission, effort, raw
  meta.json       # provider, launch mode, session id, EFFECTIVE config, created_at
  submit.json     # SUBMISSION INTENT — written and fsynced BEFORE the supervisor is called
  raw/            # the provider's own stdout and stderr, preserved BEFORE any decision
  result.txt      # written ATOMICALLY (tmp + rename); its EXISTENCE means "finished"
  publish.reject  # why publication was REFUSED, when it was — a terminal record too
  provider.exit   # the worker CLI's own exit code
  publish.exit    # the publisher's own exit code — written LAST, after the rename or the reject
```

A task is fully reconstructible from its own directory — that is why config is a file, not argv.

Rename is atomic, so there is no partial read and no separate "done" flag to keep in sync. Some CLIs
help: Codex's `-o/--output-last-message <file>` writes the final message to a file you name. But a
redirection is not a publication rule, and the next section is why.

### The publisher — `delegate-run`

An earlier draft of this document said *"the wrapper runs `cmd > result.tmp && mv result.tmp
result.txt`"*. That was wrong twice. It named a component decision 4 abolished, and redirection is
not a decision about whether there is an answer.

For `agy` the answer arrives **inline in a JSON envelope** (`response`, above), so nothing writes
`result.txt` unless something reads that envelope and judges it. `dispatch` returns a task id and
exits. pueue captures stdout into *its own* store, not into the task directory. With no named owner,
nobody is left to normalize the result — a publisher-shaped hole, and applied literally the old rule
would have published the `agy` timeout envelope as a completed answer.

So the publisher is a first-class component:

```
pueue add --escape --label <task_id> -- delegate-run <task_id>
```

`delegate-run` is our own binary. It (a) invokes the provider CLI, (b) preserves raw stdout and
stderr under `raw/` **before deciding anything**, (c) applies that adapter's publication predicate,
(d) renames `result.tmp` → `result.txt` if the predicate passes or writes `publish.reject` if it does
not, (e) writes `publish.exit` last. It supervises nothing, holds no pid it is not still waiting on,
and sends no signal — **decision 4 survives intact.** "No supervision of our own" was never "no
process of our own".

**The predicate requires adapter-confirmed successful completion of this task's turn.** Not
"terminal" — terminal is too weak. A provider that returns a well-formed ERROR result carrying
non-empty diagnostic text and no abort marker is terminal, non-empty and unmarked, and publishing it
would file an error message as the answer. Three conditions, all required:

1. the adapter confirms **success in the provider's own vocabulary** — never the exit code;
2. the extracted answer is non-empty;
3. no adapter-specific abort marker is present (for `agy`: the `[agy] print timeout` stderr line).

**Publication is recoverable, because `delegate-run` can die between (a) and (d).** That is why
`raw/` exists and why it is written first: the evidence is bound to the task and outlives the
publisher, so a later `collect` re-applies the same predicate to the same bytes and reaches the same
verdict. Reconstruction does not weaken the atomic rename — readers still only ever see a committed
result, and **a committed result is never reopened**, whatever is missing beside it.

### The one genuinely hard question: running, stuck, or broken?

A missing `result.txt` is ambiguous. Liveness alone does not resolve it — *alive* and *healthy* are
different questions, and a wedged worker answers yes to the first.

**Channels, consulted in order of authority.** The first that returns an authoritative answer wins;
no lower channel overrides a higher one.

| # | Channel | Answers | Available on |
|---|---|---|---|
| 1 | `result.txt` exists | **done** — committed, never reopened | all |
| 2 | `publish.reject` exists, no result | **failed**, with the refusal reason | all |
| 3 | `provider.exit` exists, no `publish.exit`, no result | **publication pending or recoverable** — *not* failed | all |
| 4 | `publish.exit` exists, no result and no reject | **undetermined** — the publisher died mid-decision; re-run the predicate over `raw/` | all |
| 5 | provider registry | **working / blocked / failed**, *with a reason* | Claude `--bg` only — `claude agents --json` → `state`, `status`, `waitingFor` |
| 6 | log growth since last poll | **working** — bytes added, last line | all — Codex `--json` NDJSON, Claude `-p --output-format stream-json`, else raw stdout |
| 7 | pid + start-time | **alive / dead / undetermined** | all (never pid alone — pid reuse is real) |

**Rows 2–4 are why there are two exit files.** An earlier draft had one `exit` and the rule "`exit`
exists, no result → failed", which misreads the commonest recovery case: the provider succeeded, the
publisher had not yet renamed, and a reader arriving in that window would mark a live result failed.
Separate filenames establish *provenance* — whose exit code this is — and the four rows above
establish *publication state*. Both are needed; neither alone is enough.

A reader that races the rename sees row 3, rechecks, and gets row 1. A reader that sees row 1 stops
there: a missing `publish.exit` beside a committed result invalidates nothing.

Only Claude `--bg` can say *why* a worker is waiting. Everywhere else, channel 6 is the whole story.

### Stuck is an observation, never a verdict

Channel 4 going quiet is **not** proof of a wedge. A single long model turn, a slow tool call and a
genuinely hung process are indistinguishable from outside the CLI.

So the layer records `idle_seconds` and the last log line, and **never emits the word "stuck."** The
lead decides what idle means for the task at hand — 3 minutes of silence is alarming for a codemod
and unremarkable for a research task. Thresholds belong to the caller, never to the layer.

This is the plugin's most expensive lesson restated: **a bound elapsing is the watcher's fact, not
the job's.** No idle threshold may kill a worker or mark it failed. Silence is evidence; the lead
weighs it.

### "Has an issue" is four different failures

Conflating them is costly — the first is free to retry, the last already burned tokens.

| Shape | Evidence | Handling |
|---|---|---|
| **Launch error** | **positive proof of non-submission** — `submit.json` absent, so the supervisor was never called | nothing was spent; retry freely |
| **Admission unknown** | `submit.json` exists but no supervisor record bound to this task | **never auto-resubmit.** Surface to the lead with the evidence; a second launch is authorized, never inferred |
| **Mid-flight error** | rate limit / auth expiry / failed tool call in the stream | keep the last error line; the worker may recover unaided — do not intervene on the first one |
| **Terminal failure** | `publish.reject`, or `provider.exit` non-zero with a completed publish | done; read the session transcript for partial work before re-dispatching |

The second row is the one an earlier draft got wrong. It said *"no session id was ever recorded →
nothing was spent; retry freely"* — but **absence of a saved identity is not absence of execution**.
See *At-most-once launch*.

### `status.json` — the poll snapshot

Written by the poller on each pass, so the lead never re-derives state from raw channels:

```
state              # done | failed | working | blocked | idle | died | undetermined
source             # WHICH channel answered — makes every verdict auditable
last_activity_at   # newest of: log mtime, registry timestamp
idle_seconds       # reported, never acted on
last_line          # last log line, truncated
provider_state     # verbatim from the registry, unmapped, where there is one
waiting_for        # verbatim, where the provider says
session_id         # the provider ref, for resume
result_path        # present only when done
exit_code          # when terminal
transcript         # WHERE the provider's own session record is  (see below)
logs               # how to get the worker's stdout/stderr       (see below)
```

`source` matters as much as `state`: "working, per the registry" and "working, because the log grew"
carry different confidence, and a lead deciding whether to give up should see which one it got.

#### Why `transcript` and `logs` are load-bearing, not extras

The recovery policy says: when a worker died with no result, **the CLI's own session transcript may
still hold the answer — look before re-dispatching.** A lead that is not told *where* to look cannot
follow that policy, and will re-dispatch and pay twice. So `status` must be the single call that
answers all three recovery questions: is it done, what did it say, and where do I look if it isn't.

**`logs` is a command, not a path.** pueue owns the captured output (`pueue log <id> --json` returns
it), and the file lives in pueue's own store. Rather than leak pueue's layout to the lead, add a
fifth command — `delegate logs <id>` — that proxies it. The lead never learns pueue exists.

This is forced, not chosen: **`--escape` and shell redirection are mutually exclusive.** pueue
documents that `--escape` "implicitly disables nearly all shell specific syntax", so the launch
cannot both use `--escape` and redirect output to `tasks/<id>/stdout.log`. `--escape` is
non-negotiable (it is what stops prompt text being mangled — see the adapter rule above), so the log
stays pueue's and we proxy it.

**`transcript` is a descriptor, not always a path** — it differs per provider and is sometimes not a
file at all:

| Provider | Transcript |
|---|---|
| Codex | file — `~/.codex/sessions/*.jsonl` |
| Claude Code | file — `~/.claude/projects/<slug>/<session-id>.jsonl` |
| Pi | file — JSONL sessions |
| OpenCode | **API only** — `session list` / the server; no path to hand over |
| Gemini | file, but the session id may not surface in headless JSON (issue #14435) |

So the shape is `{kind: "file", path}` or `{kind: "command", command}`, and `null` where genuinely
unavailable. **`null` means "not available", never "no transcript exists"** — the same discipline as
`undetermined`. The adapter supplies it; the layer never guesses a path.

### Shrink the ambiguity instead of detecting it

Two moves that cost nothing and beat any amount of monitoring:

1. **Ask for progress lines in the brief.** Not a schema — one sentence of prose asking the worker to
   print a line as it begins each step. Turns silence into evidence for free. Advisory: a worker that
   ignores it leaves you exactly where you already were.
2. **Default to print mode with `--permission-prompts none` and a read-only sandbox.** A worker then
   *cannot* block on a permission prompt — it fails fast instead. This deletes the blocked state
   rather than monitoring it, and is the strongest argument for print-mode-by-default (see the fork
   in the road, below).

### The invariant under all of it

**Never collapse "could not determine" into "no."** An unreachable registry, or a pid whose
start-time no longer matches, is `undetermined` — not dead. Re-dispatching or signalling on an
undetermined reading is how you get two workers on one task, or a healthy job destroyed. This is
exactly the defect `claude-codex-duo`'s probe had, one level up.

### Diagnosing silence — three read-only channels

Only `claude:bg` *reports* why a worker waits. Everywhere else we **derive** it, with no provider
cooperation and nothing signalled:

1. **The last structured EVENT, not the last byte.** Codex `--json`, Kimi and Claude
   `stream-json`, OpenCode `--format json` all emit event streams. A worker mid-tool-call emits
   `tool_use` and then goes quiet until the tool returns. That is not silence — it is *"waiting on
   bash for 90s"*. This is the `waitingFor` equivalent, derived rather than reported.
2. **The descendant process tree.** `ps -o pid,ppid,stat,etime,command -g <pgid>`. If the CLI
   spawned `npm test` and it is still running, the worker is not stuck — its tool is.
3. **CPU-time delta across polls.** `ps -o time`. Rising means something computes. Flat means
   blocked on I/O.

Together they discriminate the cases that matter:

| descendants | CPU | last event | diagnosis |
|---|---|---|---|
| yes | rising | `tool_use` | tool running — normal, however long |
| no | rising | any | model streaming, or local compute |
| no | flat | `tool_use` | tool wedged |
| no | flat | assistant text | waiting on the model, or on input |

**Prevention on top:** launch with **stdin from `/dev/null`**. A CLI that tries to prompt gets EOF
and fails fast instead of hanging forever. With the auto-approve flags, blocking-on-input largely
stops existing rather than needing to be detected.

This upgrades `idle` from an unexplained number to an explained one. The rule from the previous
section still holds — the layer reports the diagnosis, it does not act on it.

### Do not write the wrapper — adopt a supervisor (decided 2026-09-12, choice pending one test)

The "wrapper" is a process supervisor, and that is a thoroughly solved problem. Candidates surveyed
2026-09-12:

| Tool | Lang / License | Daemon | Parallel | Kill reaches tree | Machine status | macOS |
|---|---|---|---|---|---|---|
| **pueue** | Rust, MIT/Apache-2.0 | yes | yes (groups + slots) | **unproven** — see #188 | `status --json` | full |
| **task-spooler** (`tsp`) | C, GPL | auto-spawned | yes (`-S n` slots) | **documented** — kills the job's process group | `-M json` | brew core |
| **nq** | C, public domain | **none** (flock chain) | **no — sequential by design** | no — you signal the pid | `ls -F` | 10.3+ |
| **systemd-run --user** | — | PID 1 | yes | **guaranteed** (cgroups) | journal | **Linux only** |
| **jobd** | Python, MIT, v0.5 beta | broker + workers | yes | Linux only | HTTP + SSE | **degraded** |
| pm2 / supervisord / circus | various | yes | yes | partial | yes | yes |
| launchd | — | native | yes | per-service | plists | native only |

**Ruled out and why:**
- **pm2, supervisord, circus** — wrong shape, not merely worse. They supervise *long-running
  services* and **restart a process when it exits**. For one-shot tasks that is actively harmful.
- **nq** — the most elegant option here (no daemon; `flock(2)` chain; log file whose extension is
  the PID; public domain; philosophically identical to our file-based design) but **sequential by
  construction**, and cancel means we signal a pid ourselves. Wrong shape for parallel fan-out.
- **jobd** — GPU-aware multi-machine scheduling, over-scoped, v0.5 beta, macOS worker degraded.

#### The fact that reframes the choice: cgroups, and macOS's lack of them

jobd's own docs explain its macOS degradation: robust supervision — "memory caps, process reaping,
and preemption" — uses **`systemd-run --user` scopes and cgroups**, and on non-systemd platforms
"these guarantees are dropped."

**Killing a process tree reliably is a cgroups feature, and macOS has no equivalent.** Not pueue,
not task-spooler, not anything we could write: on macOS a child can call `setsid()` and escape its
process group, with no container to kill instead.

So *"does cancel reach everything"* is **an OS ceiling, not a tool property**:
- **On Linux**, `systemd-run --user --scope` is simply the correct answer and beats every tool here.
  If this ever targets Linux, that should be a first-class mode, not an afterthought.
- **On macOS**, every option degrades to process groups. The question becomes which tool does
  process-group kill *properly* and gives us the rest.

#### The two real candidates

**task-spooler** — 20+ years old, in Homebrew core, explicitly documents killing the **job's process
group**, `-M json`, parallel slots. It documents the exact behaviour pueue leaves open. Against:
GPL (matters if this ever ships), sparser JSON, several competing maintained forks, no `wait`-by-id,
no pause/resume.

**pueue** — far better maintained, richer JSON, groups and slot control, `wait`, pause/resume (useful
for rate limits), permissive dual license, 6.3k stars, self-described feature-complete, **macOS fully
supported**.

#### DECIDED: pueue — tested on this machine 2026-09-12

macOS 26.6.2, arm64. `pueue 4.0.4` and `task-spooler 1.0.4`, both from Homebrew.

**Fixture:** one task spawning a four-process tree — itself, an in-group child, an in-group
**grandchild (depth 3)**, and one escaping via `os.setsid()` then `exec sleep`. All dummies were
plain `sleep`s with unique durations; PGIDs were confirmed before each kill (the first three shared
a group, the escapee had its own).

| Descendant | `pueue kill` | `ts -k` |
|---|---|---|
| depth-2, in group | dead | dead |
| **depth-3 grandchild** | **dead** | **dead** |
| setsid-escaped | **ALIVE** | **ALIVE** |
| the task itself | dead | dead |

**Issue #188 is fixed in v4** — pueue reaches grandchildren. And the escaped process survived *both*
tools, which **measures** the macOS ceiling instead of assuming it: the cgroups conclusion is now
confirmed empirically, not just from two projects' docs.

**What decided it** (kill reach was a tie):

| | pueue 4.0.4 | task-spooler 1.0.4 (brew) |
|---|---|---|
| JSON status | `status --json`, `log --json`; `{"Done":{"result":{"Failed":7}}}` with enqueued/start/end | **none** — `-M json` → *"Wrong option M."* |
| exit code | structured | `E-Level 7`, human text only |
| output retrieval | `log --full` / `--json`, correct stdout+stderr | `ts -c <id>` returned **nothing** |
| argument safety | `-e/--escape`, verified working | no equivalent |

The `-M json` flag found in research exists only in **justanhduc's fork**, not the Homebrew formula.
No machine-readable output is disqualifying on its own.

#### Adapter rule forced by the test: never put prompt text in argv

Both tools execute commands **through the system shell**, rejoining argv and re-parsing it. Without
`--escape`, the test command `sh -c 'echo out-line; echo err-line >&2; exit 7'` lost its quoting:
`out-line` vanished silently while the exit code still arrived correctly as 7. **Silent partial
output loss with a correct-looking exit code** is the worst failure shape available — nobody notices
until a worker's answer comes back truncated.

This design already avoids it (the brief lives in `brief.md`, a file, never in argv), but make it
explicit:
1. Always pass `--escape`.
2. Never interpolate prompt or brief text into the command string. The command references files;
   the files hold the content.

#### Rust crates — the fallback if both CLI tools fail the test

| Crate | Version / License | What it gives |
|---|---|---|
| **`processkit`** | 3.3.4 (2026-08-22), MIT | The wrapper's whole job description: `host_containment()` (spawn-free query of the host's mechanism), cgroup v2 / Job Object / `procctl(2)` / POSIX pgroup tiers, timeouts + cancellation tokens, supervision with exponential backoff, output capture and line streaming, readiness probes, shell-free pipelines with per-stage containment. Production-ready, 100% documented. |
| **`process-wrap`** | 10.0.0, MIT/Apache, **watchexec** | Lower level; official successor to `command-group`. Process group and session wrappers, Job Objects, kill-on-drop (Tokio), signal-mask reset. **No cgroups.** |
| `shared_child` | — | Kill a child safely from multiple threads. |
| `std::process::Command::process_group(0)` | stdlib | The primitive, now built in. |
| `CaoE` | — | The **inverse** of what we want: kills grandchildren when the parent dies. |

**processkit independently confirms the macOS ceiling**, and more precisely than jobd: its
containment depends on a `Drop` handler running, so "a SIGKILL of the owner or a crash… skips it",
and macOS has "no equivalent" to Linux's parent-death signal for reaching indirect descendants.
Two unrelated projects, same conclusion — this is an OS fact, not a tooling gap.

**But processkit's default guarantee is the inverse of ours.** It is *kill-on-drop*: when the owner
dies, the tree dies. We want workers to **survive** the lead. It offers `spawn_detached()` →
`DetachedChild` with "no public lifetime, kill, or wait operations", which escapes containment by
design — but that trades away kill and wait, i.e. our cancel.

**This exposes the real structure: two levels are required.**

1. A supervisor **detached from the lead**, so it survives the lead dying.
2. Inside it, the worker held with **kill-on-drop containment**, so cancel works.

`pueued` **is** level 1. `processkit` is how you would build level 2 inside a level 1 you wrote
yourself. So the choice is not library-versus-tool:

| | Gives you |
|---|---|
| pueue / task-spooler | both levels, built and debugged, one binary to install |
| processkit | level 2 only — you still write and daemonize level 1 |

**Decision: settled by the test above — pueue.** `processkit` is no longer needed as a fallback, but
stays recorded as the foundation to use if pueue is ever abandoned upstream — never raw `setsid` +
`kill(-pgid)` of our own.

Two notes:
- **`host_containment()` is this design's own principle as an API** — query what the OS can
  guarantee, declare it honestly, never assume. Same discipline as the `undetermined` state.
- **It would commit the project to Rust**, or at least a Rust sidecar binary. That collides with
  open question 6 (runtime choice) and is a real cost to weigh.

| What the wrapper had to do | pueue |
|---|---|
| survive the launcher dying | `pueued` owns the task — "no need to be logged in" |
| survive reboot | "the queue is always saved to disk and restored on kill/system crash" |
| capture stdout/stderr | "logs are persisted onto the disk and survive a crash"; `log`, `follow` |
| machine-readable status | `status --json`, `log --json` |
| cancel | `pueue kill` (task or group) |
| block until done | `pueue wait` |
| pause / resume | free — useful for rate limits |

**The architectural payoff: our code signals nothing, ever.** No `setsid`, no pgid, no pid handles,
no SIGTERM/SIGKILL escalation anywhere in this project. The component flagged as the riskiest thing
we would build becomes a dependency that has already been through this exact bug.

**The one thing to test before adopting.** Issue **#188**: `pueue kill -c` signalled only *direct*
children, leaving nested processes dangling. The `--children` flag was deprecated and removed in
**v3.0.0 as part of redesigning process termination** — which suggests a proper process-group fix.
Suggests, not proves. Verify empirically, and it is safe to do so: launch a task that spawns a
sleeping grandchild, `pueue kill`, check whether the grandchild survives. Dummy processes in
isolation — never against a live GUI session.

**What stays ours** (small, and the interesting part):
- the brief, the task directory, `result.txt` written atomically
- session-id capture and resume bookkeeping — pueue knows nothing about agent sessions
- the health ladder: mapping pueue's task state plus the event stream into the four lead outcomes
- the **task budget**, since pueue has no per-task timeout or resource limit. Now trivial: a poller
  that calls `pueue kill <id>` when a ceiling is crossed. Still no signalling.
- the two-bounds distinction (watch bound detaches; task budget terminates)

For the silence-diagnosis channels, use `psutil` (Python) or `systeminformation` (Node) rather than
hand-rolled `ps` parsing.

**Honest trade-off:** a new runtime dependency — a Rust binary plus a daemon the user installs and
keeps running. And pueue states it "is not designed to be a heavy-duty programmable task
scheduler/executor"; it is built for human interaction. Right size for single-machine, human-scale
fan-out; wrong tool at hundreds of concurrent tasks.

### Stopping a runaway — safe by construction

Earlier drafts declared `can_cancel: false` to avoid repeating the macOS incident. That was too
blunt: it treated cancel as one thing when it is two.

**What was dangerous is signalling a number recorded earlier** — a pgid that had come to name
something else by the time it was used. Signalling a process you **forked yourself and have not
reaped** is a different act entirely: that pid cannot be reused while you hold it.

So move the authority. **The lead never signals anything. The supervisor does.**

With pueue adopted, we do not even hold the handle: `pueue kill <task_id>` addresses a task by an id
the daemon issued, and the daemon owns the process. The incident's failure mode becomes
structurally unreachable, because nothing in our path ever turns a stored number into a signal
target.

A ladder, preferring mechanisms that do not signal at all:

| Mode | Mechanism | Signalling by us |
|---|---|---|
| `claude:bg` | `claude stop <id>` — ask the daemon that owns it | none |
| `opencode:attach` | `session.abort` over the server API (⚠ #29894 may no-op) | none |
| every other mode | `pueue kill <task_id>` | none |

With pueue as the supervisor, **there is no row left where we signal anything.** The orphan case
that previously needed recorded-identity signalling is pueue's problem, not ours — which is the
entire reason to adopt it. If pueue's kill turns out not to reach nested children (issue #188, see
above), that is a bug to report upstream or a reason to reject pueue, **never** a reason to add
pgid-signalling code of our own.

**This also supplies the missing cost ceiling.** Codex, Gemini, Kimi and Pi have no
`--max-budget-usd`; `delegate-run` can enforce a wall-clock or token bound on the child it is still
waiting on, and a lead-requested stop goes through the same safe handle.

### Two bounds that must never share a field name

Same elapsed timer, opposite correct action. Conflating them is how the plugin killed healthy jobs.

| | Meaning | On expiry |
|---|---|---|
| **watch bound** | the lead stopped watching — a fact about the *watcher* | **detach**, never kill |
| **task budget** | the caller declared in advance that this task may not exceed X — a determination about the *job* | **terminate**; that is the entire point |

A caller-declared ceiling is a decision made before the fact, with the job's own cost in view. A
watcher timeout is the observer giving up. Both are legitimate; they get separate names, separate
fields, and separate code paths.

### Tier is a property of the LAUNCH MODE, not the provider

This is the refinement that matters. An adapter does not declare a tier; it exposes launch modes,
each with its own capabilities:

| launch mode | owns_job | native budget cap | cancel via | auto-approve flag |
|---|---|---|---|---|
| `claude:bg` | **true** (vendor daemon) | false | `claude stop <id>` | — |
| `claude:print` | false | **true** (`--max-budget-usd`, `--max-turns`) | `pueue kill` | `--permission-prompts none` |
| `codex:exec` | false | false | `pueue kill` | `--sandbox read-only` |
| `gemini:print` | false | false | `pueue kill` | — |
| `kimi:print` | false | false | `pueue kill` | `--afk` |
| `pi:print` | false | false | `pueue kill` | (`--tools` restricts) |
| `antigravity:print` | false | false | `pueue kill` | `--dangerously-skip-permissions` |
| `opencode:attach` | **true** (our server) | false | `session.abort` ⚠ #29894 | `--auto` |

Every worker returns a **message**; `result.txt` is the universal contract and there is exactly one
collect path. (This is what excluding Aider bought.)

The lead picks a mode from requirements, not a provider by name. The layer supervises every
`owns_job: false` mode **identically** and merely translates for modes that own themselves — so v1
ships with one supervision path, not two.

**Two `owns_job: true` modes, and they are not equivalent.** Claude's `--bg` is owned by a vendor
daemon whose policy we inherit (~1h idle stop, 48h offline, restart-on-crash). OpenCode's `serve` is
a server *we* run — but see its two open recovery bugs above. Neither removes the need to own the
`setsid` path ourselves.

**Deleting the blocked state generalizes.** `--permission-prompts none`, `--afk`, `--auto`,
`--yes-always`, `--dangerously-skip-permissions`, `--tools read,grep,…` are each a provider's way of
removing the blocked state rather than monitoring it. Every adapter should expose one, and the
default should use it.

**Do not put a containment flag on that list.** `codex --sandbox read-only` and `agy --sandbox`
restrict what a tool call may *do*; they are not answers to "who approves it". Codex fuses the two on
one flag and `agy` does not, which is exactly how a containment flag ended up filed as a permission
intent nobody had checked. See the config table's *two axes* note.

### Claude Code's fork in the road

`--bg` and `-p` are mutually exclusive, and they trade durability against control:

| | `--bg` | `-p` |
|---|---|---|
| Supervisor keeps it alive / auto-restarts | ✅ | ❌ |
| `--max-budget-usd`, `--max-turns` | ❌ | ✅ |
| `--permission-prompts none` (deny instead of hang) | ❌ | ✅ |
| Can sit `blocked` on a permission prompt | ✅ | forced to deny |
| Resume by id | ✅ | ✅ |

Leaning: **default workers to print mode** (bounded and priced, same code path as every other
provider); `--bg` is opt-in for long or interactive tasks. Note this case rests on *cost bounds and
not blocking* — not on structured output, which we dropped (below).

### Adapter interface — eight functions

```
capabilities()                     -> launch modes, their flags, and the CONFIG KEYS each honors
start(brief, task_id, mode, cfg)   -> provider_ref (session id) + the EFFECTIVE config it applied
poll(provider_ref)                 -> working | done | failed | blocked | unknown
publishable(raw)                   -> {publish: true} | {publish: false, reason}  — THE PREDICATE
collect(provider_ref)              -> path to the result text for work ALREADY DONE. Never executes a turn
cancel(provider_ref)               -> native stop where one exists, else `pueue kill`
resume_handle(provider_ref)        -> {available, kind, ref} — see below; `available: false` is a valid answer
transcript(provider_ref)           -> {kind: "file", path} | {kind: "command", command} | null
```

**`collect` and `resume_handle` are different acts and must never be confused.** `collect` gathers
work that already happened and spends nothing. A resume handle starts a *new turn* on an existing
conversation and costs money. The `agy` timeout measurement is the proof they are not
interchangeable: after the turn was abandoned, the conversation resumed perfectly well and
`num_turns` went to 2 — but asking it for the abandoned essay returned **"NONE"**. Resume recovered
the *conversation*, not the *work*.

So `resume_handle()` **declares availability rather than assuming it**, per provider and per
situation: `{available: false}` is a correct answer, and the layer must never present a resume handle
as a way to recover a lost turn. Recovery of an abandoned turn is `raw/` and the transcript, or it is
nothing.

### Per-task config — three intents, never flags

The lead sets *intent*; the adapter owns the spelling. That is the whole reason adapters exist.

```
model       # provider-namespaced string or alias — also selects the ccr route
permission  # read-only | workspace-write | auto-approve | ask
effort      # low | medium | high | default
raw         # { <launch mode>: [extra argv] }  — escape hatch, per mode
```

**An unsupported key FAILS THE DISPATCH. It is never silently dropped.**

This is the `--escape` lesson generalized: silent partial application with a success-looking result
is the worst failure shape available. Running a task at default effort after being asked for `high`,
and reporting success, is a lie about what was paid for. `capabilities()` declares which keys a mode
honors; dispatch fails closed. `--allow-unsupported` downgrades it to a warning recorded in
`meta.json` for leads that genuinely want best-effort.

`meta.json` records **requested and effective** config — "why did this task cost so much" is only
answerable if both were kept.

**Unsettled: is uniform fail-closed right, or should it be per-key?** Only 3 of 7 launch modes have
a verified `effort` knob, so a uniform rule makes refusal the common case — and tightening the ✓ bar
(decision 16) makes that worse, not better. The argument for a **per-key** policy is real: `model`
and `permission` are safety-and-cost keys where silently running something other than what was asked
is a lie about what was paid for, while `effort` is a quality key where a recorded warning may serve
the lead better than a refused dispatch. This design keeps the uniform rule for now because it is the
conservative one and `--allow-unsupported` exists as the escape hatch — but the adversarial review
flagged it as the strongest surviving argument against its own ruling, and neither side argued it.
Revisit after milestone 2, when there is real refusal data rather than a table.

| intent | Codex | Claude `-p` | OpenCode | **Antigravity** | Kimi | Pi |
|---|---|---|---|---|---|---|
| `permission: read-only` | `--sandbox read-only` ✓ | `--permission-prompts none` ✓ | — | **✗ — no such mode** | — | `--tools read,grep,find,ls` ✓ |
| `permission: workspace-write` | `--sandbox workspace-write` ✓ | — | — | **`--sandbox` ✓✓ measured** | — | — |
| `permission: auto-approve` | unverified | — | `--auto` ✓ | **`--dangerously-skip-permissions` ✓** | `--afk` ✓ | — |
| `model` | `-m` / `-c model=` | `--model` ✓ | `-m provider/model` ✓ | **`--model` ✓** | unverified | unverified |
| `effort` | `-c model_reasoning_effort=` ✓ | unverified | `--variant` ✓ | **`--effort low\|medium\|high` ✓** | unverified | unverified |

✓ = verified against docs 2026-09-12. **✓✓ = verified by execution**, with a control run — so far
only `agy --sandbox`, on 2026-09-13. Codex's effort values are
`minimal | low | medium | high | xhigh`, default `medium`.

**`permission` is really two axes, and this table flattened them.** *Containment* is what the
filesystem allows; *approval* is whether a tool call waits for a human. Codex fuses both onto
`--sandbox`, so flattening cost nothing there. `agy` does not: `--sandbox` contained writes while
`--dangerously-skip-permissions` was approving every call, in the same measured run. The flattening
is what let a containment flag be filed under a permission intent nobody checked. Until the intent
is modelled as two fields, **`workspace-write` is the honest middle value** and the per-mode
capability declaration carries which axis each flag actually moves.

**A ✓ means the mapping ENFORCES the intent, not that a flag with a plausible name exists.** That
distinction was not being drawn, and it produced a wrong row: `agy --sandbox` was marked ✓ for
`permission: read-only` when `agy --help` describes it as *"Run in a sandbox with terminal
restrictions enabled"* — which is what this document itself says correctly under *Antigravity CLI*.
Restricting the terminal is not read-only; the vendor's own sandbox documentation says commands can
still write to the workspace and to build caches. The ✓ was unearned and is withdrawn. **Before any
mapping carries a ✓, the adapter must demonstrate that the flag enforces the declared intent; an
unverified mapping is treated exactly like an unsupported key, and the dispatch fails.**

Neither this document nor the review that found it had executed anything; both reasoned from help
text and vendor docs. **That gap is now closed** — a write test with a control run was performed on
1.2.2 the following day (*`--sandbox` measured*), and it says the flag contains writes to the
workspace and `$TMPDIR` while denying everything else with `EPERM`. So `--sandbox` is real, and it
is `workspace-write`. The read-only ✓ stays withdrawn; the flag earns a ✓✓ on the row it belongs to.

**Effort is the least portable of the three** — Codex, OpenCode and Antigravity have a confirmed
knob; the rest do not. Antigravity's `--effort low|medium|high` matches our intent vocabulary
exactly. Expect effort unsupported on several modes, which is why fail-closed matters more than the
table.

Two consequences:
- **`permission: ask` is only meaningful on `claude:bg`**, the one mode that can block and be
  answered. Everywhere else it fails closed — consistent with open question 3.
- **`model` can select the route, not just a flag.** `model: "glm-4.6"` on `claude:print` means the
  ccr base-URL swap, not a `--model` argument.

Defaults are per launch mode and overridable per task; the layer's own default is
`permission: read-only`, matching the read-only-by-default lean in open question 2.

**Which collides with the withdrawn ✓ above, and the collision is real.** The layer defaults to
`permission: read-only`; `antigravity:print` is the chosen first adapter; `agy` now has no verified
read-only mapping; and an unsupported key fails the dispatch. Taken together, **the first adapter
cannot run at the layer's own default.** That is the rules working correctly, not a rule to bend —
but it needs a decision before milestone 1, and it is open question 6.

### State

One SQLite file (or one JSON per task dir — decide when building). Fields: `task_id`, `provider`,
`mode`, `provider_ref`, `state`, `brief_path`, `result_path`, `lease`, timestamps.

**At-most-once launch — and admission is a tri-state.** An `O_EXCL` create of the task record is the
lease, written *before* the launch, so a crash can never leave an **untracked** job. It does not
prevent an **unobserved** one: between the supervisor accepting the job and our recording the id it
returned, the dispatcher can die. The record exists, the identity does not, and the worker may be
running and spending money right then.

So **retry requires positive proof of non-submission**, never the mere absence of an identity:

| state | what proves it | action |
|---|---|---|
| **not-admitted** | `submit.json` is absent. It is written and fsynced *before* the supervisor is called, so its absence proves the call was never made | retry is free |
| **admitted** | a supervisor task carries `--label <task_id>` | never relaunch; attach and collect, whatever we failed to save |
| **admission-unknown** | `submit.json` exists but no labelled task is found — including when the supervisor is perfectly reachable | **never auto-resubmit**; surface to the lead |

**The label proves admission; it cannot disprove it.** `pueue add --label <task_id>` makes the
positive half observable — a task carrying our label is proof the job was accepted, independent of
anything we managed to persist. The negative half does not follow: `pueue clean` *"removes all
finished tasks from the list"*, so label absence is equally consistent with "never ran" and with
"ran, finished, was cleaned". A reachable supervisor showing nothing is `admission-unknown`, not
`not-admitted`. `submit.json` carries the negative half instead, because it is *our* record and
nothing else prunes it.

This is not a new invention — it restores what the prior art already knew and this design had
dropped (`codex-run.sh:1142`: *ADMISSION-UNKNOWN: retain claim; no terminal outcome; never
automatically resubmit*). It is the tri-state liveness rule applied one level up: **never collapse
"could not determine" into "no".**

*Unresolved:* whether pueue's label is observable in the window between daemon acceptance and `add`
returning could not be established from its documentation. It does not change the rule — that window
lands in `admission-unknown` either way — but it does mean `admitted` may be under-reported there.

Use A2A's eight state names even though we are not adopting its transport — it costs nothing now
and makes an HTTP surface a serialization layer later rather than a re-architecture.

---

## Decisions taken

1. **No answer-content schema at any layer — but a per-adapter completion contract is mandatory.**
   Two different things, and an earlier draft of this decision said "no output contract at all",
   which contradicted the adapter rules this same document states at *Adapter rules this forces*.
   What we do not constrain is **what the answer says**: a frontier lead reads prose fine, and a
   schema may be a per-task option later, never a layer requirement. What we must constrain is
   **whether there is an answer at all** — a decision a program makes with no model in the loop, and
   one the CLIs do *not* give us for free: `agy` returns exit 0 and `status: SUCCESS` with an empty
   `response` after a timeout. So every adapter supplies a publication predicate (see *The
   publisher*) and the layer refuses to publish without one. The validators in `claude-codex-duo`
   are a third thing again — a *gate script* deciding whether a phase may advance, which is that
   plugin's audit requirement, not delegation's. Do not inherit those.
2. **No A2A transport in v1** — adopt the state vocabulary only.
3. **No durable-execution engine.** Replay is unavailable to us; retry of an agent turn is not
   idempotent and costs money.
4. **No custom pty, no tmux dependency, and no process supervision of our own.** `pueue` is the
   supervisor; we never call `setsid`, never hold a pid we are not still waiting on, never send a
   signal. See *Do not write the wrapper*. This is **not** "no process of our own": `delegate-run`
   is ours, it is what pueue supervises, and it supervises nothing itself. See *The publisher*.
5. **Results are text.** The lead reads them.
6. **Cancel is supported, and nothing we write ever sends a signal.** The lead requests; `pueue kill`
   (or a provider-native stop) acts. This supersedes both the earlier `can_cancel: false` and the
   interim "the wrapper signals its own forked child" — `delegate-run` forks the provider and waits
   on it, and cancellation reaches it through pueue. See *Stopping a runaway*.
7. **Single machine, no server, and no daemon we wrote.** `pueued` is a daemon, but it is a
   dependency we install, not code we maintain — that is the entire point of adopting it.
8. **Aider is out of scope**, so every worker returns a message and there is one collect path.
9. **pueue is the supervisor** — chosen over task-spooler after both were installed and tested here;
   the deciding margin was JSON status and output retrieval, not kill reach, which tied.
10. **Node / TypeScript**, distributed on npm.
11. **Config is intent, and an unsupported key fails the dispatch.** Never silently dropped; the
    `--allow-unsupported` escape hatch records a warning instead.
12. **`status` returns locations, not just state** — result path, transcript descriptor, session id —
    because the recovery policy is unusable if the lead is not told where to look.
13. **`antigravity:print` is the first adapter**, Codex the second — **provisional**, pending open
    question 6. One of the four reasons for it was withdrawn on 2026-09-13.
14. **`delegate-run` is a first-class component** — our process, supervised by pueue, supervising
    nothing. It owns the publication decision. See *The publisher*.
15. **Admission is tri-state and retry requires proof of non-submission.** Nothing is ever
    automatically resubmitted on `admission-unknown`. See *At-most-once launch*.
16. **A ✓ in the config mapping table means the flag ENFORCES the intent.** An unverified mapping is
    treated as an unsupported key and the dispatch fails. `✓✓` marks a mapping verified by
    *execution with a control run*, which is the bar for anything safety-shaped.
17. **`permission` gains `workspace-write`** — measured on `agy --sandbox` 2026-09-13. Containment
    and approval are separate axes; the table flattened them and that is how a wrong ✓ survived.

## Explicitly not building in v1

HTTP server · A2A over the wire · Temporal/DBOS/Restate · streaming (poll instead) ·
cross-machine tasks · **answer-content** schemas · a second supervision path · a second
`result_shape` · automatic signalling of any process we did not fork and are not still waiting on ·
automatic resubmission of a task whose admission is unknown.

---

## Invariants carried over from `claude-codex-duo`

Hard-won over thirteen review cycles. Marked by whether the file-as-signal design still needs them.

| Invariant | Still needed? |
|---|---|
| Tri-state liveness (`running / ended / undetermined`) — never collapse to a boolean | **Yes** — the health ladder above |
| Never signal an identity you cannot prove | **Obsolete by construction** — with pueue we never signal at all. Kept as the reason we chose a supervisor rather than writing one |
| Bind recovery to a job identity, never to a path — a prefix outlives its attempts | **Yes**, as `task_id` + `provider_ref` + the pueue task id |
| A watch bound is the watcher's, not the job's — timeout must detach, not kill | **Yes** — now stated as the two-bounds rule above; it is also what `delegate-skills` gets wrong for our purpose |
| Terminal records are terminal — never invent one for a job still in flight | **Yes** — `result.txt` is written once, by rename, and `publish.reject` is equally terminal. Note the first draft of the health ladder violated this: it called a pending publication "failed" |
| At-most-once launch | **Yes** — the `O_EXCL` lease, *plus* tri-state admission. The lease alone was not enough, and this document had to relearn it |
| **Admission is tri-state, and retry needs proof of non-submission** (`codex-run.sh:1142`) | **Yes** — this design dropped it and the drop was the most expensive defect the review found. `submit.json` restores the provable negative |
| Receipts for exactly-once collection (submission id → job id) | **Mostly dropped** — that was for a job living inside another daemon's registry; with a file as the completion signal it evaporates |
| Claims, budgets, phase gates, accept ledgers | **Dropped** — review-protocol concurrency, not delegation |
| Session resume is lossy and per-provider; model it, never assume | **Yes** — e.g. Codex's `resume --last` is scoped to the working directory; `--all` widens it |

---

## Open questions

**Two must be settled before code: #1 and #7.** The rest are answered more cheaply by building than
by more discussion.

1. **What is a Task — one turn, or a conversation?** A2A allows a Task to span messages. If a Task
   is one turn, resume is trivial and multi-turn work is the lead's problem. *Leaning: one turn* —
   and the adversarial review found this coherent, with one condition now written into the adapter
   interface: a completed task may expose a conversation handle for a *new* task to continue,
   without that changing the original task's identity or result. **Decide before milestone 1.**
2. **Who owns isolation?** `--bg` auto-creates a git worktree; Codex and Gemini do not. Either the
   layer makes a worktree per task, or workers are **read-only by default** with writes opt-in.
   *Leaning: read-only default — it collapses most of the safety surface.* Note the mechanism
   already exists per-provider: `pi --tools read,grep,find,ls` and `codex --sandbox read-only` both
   enforce it inside the CLI, which is better than us policing it from outside.
3. **What happens to a blocked worker?** *Largely closed.* Auto-approve flags plus stdin from
   `/dev/null` mean most modes cannot block on input at all, and the event stream tells us what a
   silent worker is waiting on. What remains open is only `claude:bg`, where a worker can sit
   `blocked` indefinitely while the daemon keeps it alive: does the lead get to answer the prompt,
   or is blocked-past-N simply a failure?
4. **Does the lead ever re-dispatch?** If a result is unusable, is that a new Task or a resumed one?
   Cost and idempotency both hang on this. *Answer by building — milestone 1's timed-out run is the
   first real instance of it.*
5. **What does the lead branch on?** Proposed: exactly four outcomes — *usable result*, *no result
   but recoverable*, *no result and terminal*, *undetermined*. Anything richer is the adapter's
   business.
6. **`permission: read-only` on `agy` — what happens?** *(New 2026-09-13. Blocks milestone 1.)* The
   layer defaults to `read-only`, `agy` has no read-only mode, and an unsupported key fails the
   dispatch — so the first adapter cannot run at the layer's default.

   **The cheap option was taken first and it changed the answer.** `agy --sandbox` was measured
   rather than argued about (see *`--sandbox` measured*), and it turns out to be a genuine
   filesystem sandbox: writes outside the workspace and into `$HOME` return `EPERM`, with a control
   run proving the flag causes it. It is `workspace-write`, not `read-only` — which both confirms
   the withdrawn ✓ and supplies a real containment tier the design did not have a name for.

   That leaves two options, and the choice is a product decision, not a technical one:
   - **(a)** demote `agy` behind **Codex** for milestone 1. `codex --sandbox read-only` is a
     verified read-only mode, so the layer's default works untouched. Cost: the reasons `agy` was
     chosen first — inline `response`, real token `usage`, and the nastiest timeout trap in the set
     to build the guards against — all move to milestone 2.
   - **(b) *(leaning)*** declare `read-only` **unsupported on `agy`**, and have the `agy` adapter
     declare `workspace-write` instead. A task that wants `agy` at the layer default is refused,
     explicitly and on the record; a task that asks for `workspace-write` runs contained. Fail-closed
     survives intact, milestone 1 keeps its first adapter, and workers are **not** unrestricted —
     which was the only real cost of this option before the measurement.

   *A leaning is now recorded where none was before, because measurement removed the reason there
   was none. The call is still the user's.*
7. ~~Runtime and distribution~~ — **DECIDED 2026-09-12: Node / TypeScript.** It matches everything
   already maintained around this: claude-code-router is Node, Claude Code plugins are Node/shell,
   OpenCode ships a JS SDK client, and both Kimi CLI and Pi are TypeScript — so the eventual
   integration is the cheapest of the options. Use `systeminformation` for the descendant-tree and
   CPU-delta channels rather than hand-rolled `ps` parsing. Distribution is npm.

## Milestone plan

Each step is falsifiable — it either works or teaches us the design is wrong, cheaply.

**Two things block milestone 1**, both decisions rather than unknowns: open question 1 (*Task = one
turn?*) and open question 6 (*`read-only` on `agy`*). Everything else it depends on is settled and
tested: the supervisor (pueue, verified on this machine), the runtime (Node/TypeScript), the config
model, the status payload, and the publication and admission protocols above.

### 1. Task record + the first adapter — `antigravity:print` *(pending open question 6)*

Commands in scope: `dispatch`, `status`, `collect`, `cancel`. `logs` can follow.

**Three specifications must exist in code before the first task is dispatched**, and each is a
correction the review of 2026-09-13 forced:
- the **publication predicate** per adapter, plus the publication/recovery states in the health
  ladder — a reader must never call a pending publication "failed";
- **tri-state admission** with `submit.json`, where retry requires proof of non-submission and
  unknown never auto-resubmits;
- `resume_handle()` **declaring availability**, and `collect` never executing a turn.

**Why `agy` first rather than Codex** — all four reasons came out of live testing on 2026-09-12:
- `response` is inline in the result JSON, so `result.txt` needs no `-o` flag
- `usage` returns real token counts — the task budget's data source, and no other tested provider
  gives it
- **amended 2026-09-13:** `--model` and `--effort` hold. `--sandbox` does **not** implement
  `permission: read-only` — but it was measured and *does* implement `workspace-write`, with
  containment independent of approval. The intent abstraction is still exercised; it is exercised
  by a provider that splits an axis Codex fuses, which is a better test than the original claim.
  See open question 6
- it is already installed and authenticated here, so milestone 1 can be run end to end immediately

And a fifth that reads as a drawback but is not: **its timeout trap is the nastiest failure shape in
the set** (exit 0 + `status: SUCCESS` + empty `response`). Building against it first forces the
"never trust exit code, never write an empty `result.txt`" guards to be correct from the start,
rather than retrofitted after a quieter provider lulled us.

Exit criterion: the health ladder and the four lead outcomes demonstrated on real runs, including a
deliberately timed-out one that is correctly reported as **not** complete.

### 2. Second adapter, different provider, same code path

Proves the interface is not shaped around one CLI. Cheapest honest second: **Codex** — its
`-o/--output-last-message`, `--sandbox` and `-c model_reasoning_effort=` are spelled nothing like
`agy`'s, and its JSONL transcript is the strong recovery case that antigravity lacks. **Kimi or Pi**
are the alternative, being Claude-Code-shaped. The `ccr:<alias>` models (DeepSeek/GLM/MiniMax) do
*not* count as a second adapter; they are the same one with new env.

Verify when reaching each: how OpenCode returns a session id from a non-interactive `run`; whether
`kimi-cli` or its successor "Kimi Code" is the package to target; whether Gemini's headless JSON
surfaces the session id (#14435).

### 2b. OpenCode via `serve` + `run --attach`

The second `owns_job: true` mode. Treat it as an experiment, not a shortcut: issues #29894 and
#19023 mean its abort and its restart recovery may both be broken. Worth measuring; not worth
depending on until measured.

### 2c. ~~Verify pueue's kill reaches nested children~~ — done 2026-09-12, passed

See *DECIDED: pueue*. The remaining known gap (setsid-escaped descendants) is an OS ceiling, not a
tool defect, and belongs in the capability declaration rather than in a fix.

### 3. Port `claude-codex-duo`'s Phase 2 fan-out onto the layer

If the review skill runs unchanged, the seam is right. If it does not, we learned it in weeks
instead of months.

### 4. The lead loop

Only then. And only after that, and only if something remote needs to call in: A2A over HTTP.

## Sources

- Claude Code CLI reference — https://code.claude.com/docs/en/cli-reference
- Claude Code background agents / agent view — https://code.claude.com/docs/en/agent-view
- Codex CLI developer commands — https://learn.chatgpt.com/docs/developer-commands?surface=cli
- Gemini CLI session management — https://github.com/google-gemini/gemini-cli/blob/main/docs/cli/session-management.md
- Gemini CLI headless reference — https://geminicli.com/docs/cli/headless/
- OpenCode CLI reference — https://opencode.ai/docs/cli/
- Kimi Code CLI command reference — https://moonshotai.github.io/kimi-cli/en/reference/kimi-command.html
- Kimi CLI repo — https://github.com/MoonshotAI/kimi-cli
- Pi repo — https://github.com/earendil-works/pi
- Pi review, v0.84.2 — https://www.glukhov.org/ai-devtools/pi/pi-coding-agent-review/
- Aider headless scripting, issue #426 — https://github.com/Aider-AI/aider/issues/426
- Alternative models with Claude Code — https://github.com/Alorse/cc-compatible-models
- MiniMax Anthropic-SDK endpoint — https://platform.minimax.io/docs/api-reference/text-anthropic-api
- CLI coding agent directory — https://github.com/bradagi/awesome-cli-coding-agents
- Antigravity CLI headless mode — https://antigravity.google/docs/cli/headless/
- antigravity-cli repo — https://github.com/google-antigravity/antigravity-cli
- OpenCode server / OpenAPI — https://opencode.ai/docs/server/
- OpenCode issue #29894, session.abort no-op over SDK — https://github.com/anomalyco/opencode/issues/29894
- OpenCode issue #19023, sessions stuck after server restart — https://github.com/anomalyco/opencode/issues/19023
- pueue — https://github.com/Nukesor/pueue
- pueue issue #188, signalling descendants — https://github.com/nukesor/pueue/issues/188
- task-spooler (`tsp`) — https://github.com/justanhduc/task-spooler
- nq, daemon-free unix job queue — https://github.com/leahneukirchen/nq
- jobd (cgroups/systemd-run rationale, macOS degradation) — https://pypi.org/project/jobd/
- processkit — https://docs.rs/processkit
- process-wrap (successor to command-group) — https://github.com/watchexec/process-wrap
- Rust issue #115241, Child::kill does not terminate grandchildren — https://github.com/rust-lang/rust/issues/115241
- Z.AI Coding Tool Helper — https://docs.z.ai/devpack/extension/coding-tool-helper
- ZCode overview — https://www.aimadetools.com/blog/what-is-zcode-z-ai/
- Unofficial ZCode terminal client — https://github.com/kingsword09/zcode-cli
- A2A specification — https://a2a-protocol.org/v0.1.0/specification/
- A2A adoption 2026 — https://www.glukhov.org/ai-systems/comparisons/a2a-protocol-2026-adoption/
- Durable execution comparison — https://devstarsj.github.io/2026/04/03/durable-execution-temporal-restate-dbos-distributed-workflows-2026/
- SQLite durable workflows — https://byteiota.com/sqlite-durable-workflows-skip-temporal/
- delegate-skills — https://github.com/amElnagdy/delegate-skills
- Antigravity CLI sandbox docs — https://antigravity.google/docs/cli/sandbox/
- Adversarial review of this design, 2026-09-13 (Claude vs Codex, 2 rounds, ruling OVERTURNED with an
  amended motion upheld) — `/tmp/codex-debate/delegation-layer/design-ready-for-m1-20260913-115839/DEBATE.md`
