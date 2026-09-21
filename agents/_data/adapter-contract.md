# Adapter Contract and Runtime Capability Checks

This document defines the normative requirements for provider adapters
(`antigravity:print`, `codex:exec`, `claude:print`, `pi:json`, and
`opencode:run`).

## 1. Supported Runtime Profiles

| Adapter | Capability Profile | Approval Policy | Command Shape & Flags | Deferred Capabilities |
|---|---|---|---|---|
| `antigravity:print` | `workspace-write` | Accept workspace file edits; deny other unapproved requests; pre-approved sandboxed commands may run | `agy --sandbox --mode accept-edits --add-dir <canonical-workspace> --output-format json --input-format text --print-timeout <duration>` | `read-only`, unrestricted auto-approval, extra writable roots |
| `codex:exec` | `read-only`, `workspace-write` | native sandbox | `codex exec ... --sandbox <mode> ...` | unrestricted execution, ephemeral sessions |
| `claude:print` | `read-only`, `workspace-write` | native `plan` or `acceptEdits` permission mode | `claude --print ... --permission-mode <mode> ...` | unrestricted execution |
| `pi:json` | `read-only` | native read,grep,find,ls tool selection; native extensions remain enabled | `pi --mode json --tools read,grep,find,ls ...` | workspace-write |
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
- **Input Delivery**: Brief text is delivered exclusively via finite regular file passed to child stdin, followed by immediate EOF. Brief text is never passed in argv. Brief size limit is 8 MiB.
- **Argv Construction**: Built strictly as Go string slices (`[]string`), executed directly via `exec.Command` without shell wrapper or reparsing.
- **Working Directory**: Set strictly to the validated canonical workspace directory (`Cmd.Dir = workdir`).
- **Launch Verification**:
  - Records the requested native permission mode, runtime identity, and workspace.
  - Rechecks the admitted launch contract immediately before launch. Personal
    provider configuration is not hashed or rejected for drift by new profiles.
  - Previously admitted tasks retain their original preparation contract.
- **Credential Safety**: Credentials, tokens, and unrelated user environment variables are never included in command arguments, logs, metadata, or committed fixtures.

## 3. Evidence Capture and Publication Predicates
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

## 4. Usage Accounting
- Usage metrics are recorded with explicit scope: `task`, `conversation-cumulative`, `main-agent`, `whole-tree`, or `unknown`.
- Missing or unreported fields remain null; they are never assumed to be zero.
- Cumulative counters from successive turns are never conflated with single-turn usage unless explicitly calculated from verified endpoints.
