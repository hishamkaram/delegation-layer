# Adapter Contract and Runtime Capability Checks

This document defines the normative requirements for provider adapters
(`antigravity:print`, `codex:exec`, `claude:print`, `pi:json`, and
`opencode:run`).

## 1. Supported Runtime Profiles

| Adapter | Capability Profile | Approval Policy | Command Shape & Flags | Deferred Capabilities |
|---|---|---|---|---|
| `antigravity:print` | `read-only`, `workspace-write` | native `plan` for read-only; native automatic approval (`always-proceed`) for writes | read-only: `agy --sandbox --mode plan --add-dir <canonical-workspace> ...`; write: `agy --sandbox --mode accept-edits --dangerously-skip-permissions --add-dir <canonical-workspace> ...` | extra writable roots |
| `codex:exec` | `read-only`, `workspace-write` | native sandbox | `codex exec ... --sandbox <mode> ...` | unrestricted execution, ephemeral sessions |
| `claude:print` | `read-only`, `workspace-write` | native `plan` for read-only; `bypassPermissions` for unattended writes | `claude --print ... --permission-mode <mode> ...` | independent containment |
| `pi:json` | `read-only`, `workspace-write` | read-only uses read,grep,find,ls; write uses provider-native tools and approval | `pi --mode json ...`; read-only adds `--tools read,grep,find,ls` | independent containment |
| `opencode:run` | `read-only`, `workspace-write` | native plan/build agent; native `--auto` for workspace-write | `opencode run --format json --dir <workspace> ...` | unrestricted execution |

Each native adapter locates its executable during static preparation and records
the executable identity in the immutable candidate. The already-supervised
inspection worker then verifies that the file is still executable, runs its
version and help commands, and checks that every flag used in its command shape
is advertised in help output before finalization and provider launch. Any
reported version and binary digest are observations bound to that task's
admission and start identity; no release version, operating system, architecture,
profile revision, or digest is compared with a checked-in value.

## 2. Launch Planning and Preflight Policy
- **Native Configuration**: Provider CLIs own configuration, authentication,
  MCP servers, hooks, plugins, skills, and native policy. New admission does not
  inventory these sources or inject settings to disable them. Permission modes
  select native behavior; they do not establish independent containment.
  Workspace-write requests unattended execution, including native automatic
  approval where available. Native access may extend beyond the workspace;
  remaining provider restrictions are reported, not repaired by the adapter.
- **Input Delivery**: Brief text is delivered exclusively via finite regular file passed to child stdin, followed by immediate EOF. Brief text is never passed in argv. Brief size limit is 8 MiB.
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
  - Rechecks the admitted launch contract immediately before launch. Personal
    provider configuration is not hashed or rejected for drift by new profiles.
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
  - Absolute absence of timeout markers (e.g. `[agy] print timeout`), refusal envelopes, or error payloads.
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
