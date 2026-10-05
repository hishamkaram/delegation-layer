# Adapter Contract and Runtime Capability Checks

This document defines the normative requirements for provider adapters
(`antigravity:print`, `codex:exec`, `claude:print`, `ccr:print`, `pi:json`, and
`opencode:run`).

## 1. Supported Runtime Profiles

| Adapter | Capability Profile | Approval Policy | Command Shape & Flags | Deferred Capabilities |
|---|---|---|---|---|
| `antigravity:print` | `read-only`, `workspace-write` | native `plan` for read-only; native automatic approval (`always-proceed`) for writes | read-only: `agy --sandbox --mode plan --add-dir <canonical-workspace> ...`; write: `agy --sandbox --mode accept-edits --dangerously-skip-permissions --add-dir <canonical-workspace> ...` | extra writable roots |
| `codex:exec` | `read-only`, `workspace-write` | native sandbox | `codex exec ... --sandbox <mode> ...` | unrestricted execution, ephemeral sessions |
| `claude:print` | `read-only`, `workspace-write` | native `plan` for read-only; `bypassPermissions` for unattended writes | `claude --print ... --permission-mode <mode> ...` | independent containment |
| `ccr:print` | `read-only`, `workspace-write` | Claude native `plan` for read-only; `bypassPermissions` for unattended writes | `ccr launch --model <alias> --auth-mode provider-only --print --permission-mode <mode> -- ...` | subscription pools, independent containment |
| `pi:json` | `read-only`, `workspace-write` | read-only uses read,grep,find,ls; write uses provider-native tools and approval | `pi --mode json ...`; read-only adds `--tools read,grep,find,ls` | independent containment |
| `opencode:run` | `read-only`, `workspace-write` | native plan/build agent; native `--auto` for workspace-write | `opencode run --format json --dir <workspace> ...` | unrestricted execution |

Each native adapter locates its executable during static preparation and records
the executable identity in the immutable candidate. The already-supervised
inspection worker then verifies that the file is still executable, runs its
version and help commands, and checks that public flags used in its command shape
are advertised in help output before finalization and provider launch.
Documented SDK protocol options that are absent from public help (Claude
`--permission-prompt-tool stdio`) additionally require protocol fixtures and
live handshake acceptance; public help alone does not prove native interaction. Any
reported version that fits the bounded durable evidence record and its binary
digest are recorded in the task evidence; a fresh
launch may refresh provider runtime identity after the current capability
probe. No release version, operating system, architecture, profile revision, or
digest is compared with a checked-in value.

## 2. Launch Planning and Preflight Policy
- **Native Configuration**: Provider CLIs own configuration, authentication,
  MCP servers, hooks, plugins, skills, and native policy. New admission does not
  inventory these sources or inject settings to disable them. Permission modes
  select native behavior; they do not establish independent containment.
  Workspace-write requests unattended execution, including native automatic
  approval where available. Native access may extend beyond the workspace;
  remaining provider restrictions are reported, not repaired by the adapter.
- **Input Delivery**: Ordinary tasks deliver the finite brief through a regular
  file on stdin followed by EOF. Explicit interactive tasks use a core-owned
  pipe carrying one initial brief and bounded native clarification/steering
  frames. Adapters only encode and interpret frames. Brief text is never passed
  in argv; its size limit remains 8 MiB. The original budget includes waiting
  for interaction. Native completion closes stdin; only the supervisor stops
  processes. No PTY or terminal scraping is permitted.
- **Argv Construction**: Built strictly as Go string slices (`[]string`), executed directly via `exec.Command` without shell wrapper or reparsing.
- **Model and Effort Selection**: `--model` and `--effort` are explicit bounded
  request fields for Codex, Claude, and AGY, in addition to profiles that
  already advertise them. They pass only through the selected adapter. Codex
  encodes effort as the TOML setting `model_reasoning_effort`; Claude and AGY
  use their native effort flags. An omitted or `default` effort is omitted from
  native argv. No provider default is inferred or recorded as an explicit
  selection.
- **Working Directory**: Set strictly to the validated canonical workspace directory (`Cmd.Dir = workdir`).
- **Launch Verification**:
  - Records the requested native permission mode, runtime identity, and workspace.
  - Rechecks the provider capability and rebuilds the current launch plan
    immediately before launch. Personal provider configuration is not hashed or
    rejected for drift by new profiles. A changed provider path, reported
    version, or executable digest is accepted only when the fresh capability
    check succeeds; the task policy, workspace, predicate, writable roots, and
    artifact declarations remain admission-bound.
  - Records the final provider executable, observed version, and digest in
    `provider.started.json` and `provider.exit`; `meta.json` remains the
    immutable admission record.
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
This revision applies the five-minute bound only to model discovery; historical
admission probes retain their 20-second bounds. Existing model-discovery-v1
journals with the former one-minute deadline remain recoverable.

The JSON response contains `schema_version`, `command`, `provider`,
`observed_at`, `status`, `reason_code`, `complete`, `source`, `efforts`, and
`models`. Each model contains an exact `id`, nullable `efforts`, and optional
`name` and `default_effort` fields. `complete` means model enumeration
completed; it does not assert that effort metadata is known. A null effort list
means unknown metadata, while `[]` means the provider explicitly reported no
choices. Harness-wide efforts are not assumed to apply to every model.

The status exit mapping is `available`/`partial` → `0`,
`blocked`/`unavailable` → `2`, and `failed` → `1`. Discovery is advisory and
never an admission allowlist. Listing a model does not prove that the account
can execute it; dispatch remains the admission boundary.

## 4. Evidence Capture and Publication Predicates
- **Raw Evidence**: Standard streams are piped to task-owned files (`raw/stdout`, `raw/stderr`). Child output writers must complete and flush before the seal (`provider.exit`) is committed.
- **Success Criteria**:
  - Valid envelope structure (JSON/JSONL) with positive status and non-whitespace final response string.
  - Exact match of task and session identities (expected session matches provider-reported session).
  - Absence of timeout markers (e.g. `[agy] print timeout`), refusal envelopes, or fatal error payloads. A documented native transient retry (such as Codex `willRetry: true`) still requires a later successful root completion; a non-retrying error cannot be erased by later success.
  - Nonzero exit code with a success-shaped body is treated as a conflicting completion and rejected.
- **Continuation**:
  - Continuation tasks reference layer-recorded session handles from a predecessor with either a validated terminal outcome or durable supervisor-ended budget-stop evidence.
  - Budget-stop evidence authorizes session handoff only; `outcome.json` remains the sole task-completion authority.
  - Active sessions are protected by durable session claims under `.session.lock`.
  - Continuation invokes the provider using explicit session resumption flags (e.g. `--conversation <id>`, `resume <uuid> -`, `--resume <uuid>`).

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
