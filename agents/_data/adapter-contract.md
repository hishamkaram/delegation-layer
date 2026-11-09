# Adapter Contract and Runtime Capability Checks

This document defines the normative requirements for provider adapters
(`antigravity:print`, `codex:exec`, `codex:review`, `claude:print`,
`ccr:print`, `pi:json`, and `opencode:run`).

## 1. Supported Runtime Profiles

| Adapter | Capability Profile | Approval Policy | Command Shape & Flags | Deferred Capabilities |
|---|---|---|---|---|
| `antigravity:print` | `read-only`, `workspace-write` | native `plan` for read-only; native automatic approval (`always-proceed`) for writes | read-only: `agy --sandbox --mode plan --add-dir <canonical-workspace> ...`; write: `agy --sandbox --mode accept-edits --dangerously-skip-permissions --add-dir <canonical-workspace> ...` | extra writable roots |
| `codex:exec` | `read-only`, `workspace-write` | native sandbox | `codex exec ... --sandbox <mode> ...` | unrestricted execution, ephemeral sessions |
| `codex:review` | `read-only` | native review operation | `codex exec review --uncommitted|--base <branch>|--commit <sha> ...` | no continuation or workspace-write |
| `claude:print` | `read-only`, `workspace-write` | native `plan` for read-only; `bypassPermissions` for unattended writes | `claude --print ... --permission-mode <mode> ...` | independent containment |
| `ccr:print` | `read-only`, `workspace-write` | Claude native `plan` for read-only; `bypassPermissions` for unattended writes | `ccr launch --model <alias> --auth-mode provider-only --print --permission-mode <mode> -- ...` | subscription pools, independent containment |
| `pi:json` | `read-only`, `workspace-write` | read-only uses read,grep,find,ls with extensions disabled; workspace-write uses Pi's built-in read,bash,edit,write tools and provider-native approval | `pi --mode json ...`; read-only adds `--tools read,grep,find,ls --no-extensions`; workspace-write adds `--tools read,bash,edit,write` | independent containment |
| `opencode:run` | `read-only`, `workspace-write` | native ACP `plan`/`build` mode; native permission requests answered `reject_once` for read-only and `allow_once` for workspace-write | `opencode acp` started in the workspace directory; mode, model, and effort are acknowledged `session/set_config_option` values | interactive clarification and steering, independent containment |

Each native adapter locates its executable during static preparation and records
the resolved path in the immutable candidate. The
[product boundary](product-boundary.md) owns installed-tool observation and
compatibility policy. Lookup uses the
unfiltered preparation scope that composition supplies to the factory, which
may be a recorded scope that composition selected. An adapter does not replace
that scope with a recorded launch environment; a recorded environment only
selects the launch environment, under the provider's own saved and unrecorded
rules. Managed runtime-only
admission records the required capability digest with no observed version.
The task's already-supervised runner verifies that the file is still executable, runs its
help command, and checks the stable public entrypoint flags that
the profile can observe reliably before finalization and provider launch.
No standalone version command is run.
Native policy projections, custom runners, and historical contracts retain
their required admission inspection. A
provider's complete launch argv remains subject to the provider process and its
result predicate; rapidly changing or pipe-truncated forwarded options are not
turned into a stale admission allowlist.
Documented SDK protocol options that are absent from public help (Claude
`--permission-prompt-tool stdio`) additionally require protocol fixtures and
live handshake acceptance; public help alone does not prove native interaction. A
reported version is an optional diagnostic. The current runtime probe records
only the resolved executable path and leaves optional installation diagnostics
empty; it does not extract version text from help or protocol output. Historical
populated diagnostics remain subject to their recorded-field validation. A
fresh launch may refresh the provider runtime path after the current capability
probe. Provider compatibility is decided from observed behavior (help output,
declared flags, and the adapter's protocol and result checks). Delegate's own
contracts remain real checks: profile, predicate, and protocol revisions, and
the worker, supervisor, and native-helper identities. The
[product boundary](product-boundary.md) owns the rule that provider
installations are mutable and user-owned.

## 2. Launch Planning and Preflight Policy
- **Native Configuration**: Provider CLIs own configuration, authentication,
  MCP servers, hooks, plugins, skills, and native policy. New admission does not
  rewrite these sources. A provider-specific permission contract may inspect
  documented startup state and pass native controls required to make the
  requested mode effective; those controls are capability-probed and recorded
  with the launch plan. Permission modes select native behavior; they do not
  establish independent containment.
  Workspace-write requests unattended execution, including native automatic
  approval where available. Native access may extend beyond the workspace;
  remaining provider restrictions are reported, not repaired by the adapter.
- **Input Delivery**: Ordinary tasks deliver the finite brief through a regular
  file on stdin followed by EOF. A machine-protocol adapter, such as OpenCode
  ACP, instead uses core-owned stdin for its protocol frames. The adapter
  encodes the brief into its prompt request, the core writes every frame, and
  native stdin closes when the protocol reports completion. This machine
  channel has no user clarification or steering and writes no interaction
  journal. Explicit interactive tasks use a core-owned
  pipe carrying one initial brief and bounded native clarification/steering
  frames. Adapters only encode and interpret frames. Brief text is never passed
  in argv; its size limit remains 8 MiB. The original budget includes waiting
  for interaction. Native completion closes stdin; only the per-task guardian
  may stop the provider scope. No PTY or terminal scraping is permitted.
- **Argv Construction**: Built strictly as Go string slices (`[]string`), executed directly via `exec.Command` without shell wrapper or reparsing.
- **Native Review Scope**: `codex:review` persists one explicit Git target
  (`uncommitted`, a base branch, or a commit) with the request and translates
  it into the Codex review subcommand. Its required brief remains task
  evidence; Codex does not accept that prompt alongside the native target
  flag.
- **Model and Effort Selection**: `--model` and `--effort` are explicit bounded
  request fields for Codex, Claude, and AGY, in addition to profiles that
  already advertise them. They pass only through the selected adapter. Codex
  encodes effort as the TOML setting `model_reasoning_effort`; Claude and AGY
  use their native effort flags. An omitted or `default` effort is omitted from
  native argv. No provider default is inferred or recorded as an explicit
  selection.
- **Working Directory**: Set strictly to the validated canonical workspace directory (`Cmd.Dir = workdir`).
- **Launch Verification**:
  - Records the requested native permission mode, runtime path, and workspace.
  - Rechecks the provider capability and rebuilds the current launch plan
    immediately before launch. Current profiles preserve personal provider
    configuration. A changed provider path or reported
    version is accepted when the fresh capability check succeeds; the task
    policy, workspace, predicate, writable roots, and artifact declarations
    remain admission-bound.
  - Resolves the installed CLI in the task's saved authorized lookup
    environment and builds one direct command from the fresh observation. The
    installed tool runs in place under the product boundary. A missing
    executable, operating-system launch error, authentication or protocol error,
    or native resume failure is reported explicitly, with no automatic retry,
    fallback executable, or fresh session.
  - Records the final provider executable path in `provider.started.json` and
    `provider.exit`. Current runtime probes leave optional installation
    diagnostics empty; `meta.json` remains the immutable
    admission record.
  - Previously admitted tasks retain their original preparation contract.
- **Credential Safety**: Credentials, tokens, and unrelated user environment variables are never included in command arguments, logs, metadata, or committed fixtures.

## 3. Model Discovery Contract

The implemented public command is:

```text
delegate models --provider PROFILE [--cwd ABS] --json
```

`--cwd` defaults to the current directory and must be absolute when supplied.
Discovery uses the state-rooted supervisor and the `model-discovery-v1` native
inspection bound, which is five minutes to allow first-run provider model/cache
initialization. It may contact the provider CLI and creates
inspection evidence, but it does not admit a provider task or launch a
delegated turn.
This revision applies the five-minute bound only to model discovery. Required
admission inspections use a one-minute queue-inclusive bound; managed
runtime-only checks use the admitted task's budget. Existing 20-second admission journals and
model-discovery-v1 journals with the former one-minute deadline remain
recoverable with their recorded deadlines.

The JSON response contains `schema_version`, `command`, `provider`,
`observed_at`, `status`, `reason_code`, `complete`, `source`, `efforts`, and
`models`. Each model contains an exact `id`, nullable `efforts`, and optional
`name` and `default_effort` fields. `complete` means model enumeration
completed; it does not assert that effort metadata is known. A null effort list
means unknown metadata, while `[]` means the provider explicitly reported no
choices. Harness-wide efforts are not assumed to apply to every model.

The [shared discovery reference](../../skills/agent-integration/references/command-reference.md#choosing-models-and-effort)
owns public status/exit handling. Discovery is advisory and never an admission
allowlist. Listing a model does not prove account access; dispatch remains the
admission boundary.

## 4. Evidence Capture and Publication Predicates
- **Raw Evidence**: Standard streams are piped to task-owned files (`raw/stdout`, `raw/stderr`). Child output writers must complete and flush before the seal (`provider.exit`) is committed.
- **Success Criteria**:
  - Valid envelope structure (JSON/JSONL) with positive status and non-whitespace final response string.
  - Exact match of task and session identities (expected session matches provider-reported session).
  - Absence of timeout markers (e.g. `[agy] print timeout`), refusal envelopes, or fatal error payloads. A documented native transient retry (such as Codex `willRetry: true`) still requires a later successful root completion; a non-retrying error cannot be erased by later success.
  - Nonzero exit code with a success-shaped body is treated as a conflicting completion and rejected.
- **Continuation**:
  - Continuation tasks reference layer-recorded session handles from a predecessor with either a validated terminal outcome or a durable Pueue-ended observation backed by an application-owned lifetime-termination record with independently proven empty scope.
  - Canonical closed lifetime evidence of any kind with a free runner lease also authorizes handoff when publication is unknown, with no seal or outcome; sealed evidence must be collected first. A queue end or timestamp alone never authorizes handoff.
  - Budget-stop and closure evidence authorize session handoff only; `outcome.json` remains the sole task-completion authority.
  - Only a genuinely absent optional `provider.ref` permits the validated immutable requested prior session as the input reference. It is not evidence the interrupted provider resumed or persisted messages; a malformed, unreadable, or mismatched record refuses.
  - Active sessions are protected by durable session claims under `.session.lock`.
  - Continuation invokes the provider using explicit session resumption flags (e.g. `--conversation <id>`, `resume <uuid> -`, `--resume <uuid>`).
  - Provider continuation validation receives the immutable predecessor plan as
    well as its request. A versioned predicate is authoritative when native
    continuation behavior changed: CCR route-preserving v3 sessions reject a
    model override, while v1/v2 modeled sessions retain their recorded
    historical behavior.

## 5. Usage Accounting
- Usage metrics are recorded with explicit scope: `task`, `conversation-cumulative`, `main-agent`, `whole-tree`, or `unknown`.
- Missing or unreported fields remain null; they are never assumed to be zero.
- Cumulative counters from successive turns are never conflated with single-turn usage unless explicitly calculated from verified endpoints.

## Native interaction

`codex:exec` uses app-server stdio for explicit interactive requests, including
native question replies and `turn/steer` with an exact active-turn precondition.
Its invocation opts into `default_mode_request_user_input`; this changes no
user settings or permission mode. `claude:print` uses stream-json input and
the documented SDK stdio host permission handler for `AskUserQuestion` only.
It retains native permission mode and does not implement steering. Other
adapters reject the interactive option and retain ordinary continuation.

Replies never authorize other tools or change permissions. Unknown native
permission requests are denied rather than converted to clarification.
Delivery reservation is durable before writing; uncertain messages are never
replayed. Native acknowledgement and completion of requested work remain
separate facts. The core seals the closed interaction journal with stdout and
stderr; outcome.json remains terminal authority.

## Optional provider-output silence

The runner owns `output_silence_timeout`, independently of native provider
settings. Positive stdout/stderr reads reset its monotonic activity deadline;
incoming clarification input does not. Waiting for a reply or healthy background
work counts as silence when the caller opts in. The interaction ready deadline
continues to describe wall budget only. Explicit zero disables, omission inherits
on continuation, and each successor has a fresh activity clock. Default behavior
remains unchanged. Provider-native final result events alone do not close a turn.
