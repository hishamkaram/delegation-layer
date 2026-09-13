# Adapter Contract and Certified Profiles

This document defines the normative requirements for provider adapters (`antigravity:print`, `codex:exec`, and `claude:print`). All three adapters are required dependencies for the v1 private release.

## 1. Minimal Certified Profiles

| Adapter | Capability Profile | Approval Policy | Command Shape & Flags | Deferred Capabilities |
|---|---|---|---|---|
| `antigravity:print` | `workspace-write` | Deny unapproved; pre-approved sandboxed commands may run | `agy --sandbox --output-format json --input-format text --disable-slash-commands --print-timeout <duration>` | `read-only`, auto-approve (`--dangerously-skip-permissions`), extra writable roots |
| `codex:exec` | `read-only` | `never` | `codex exec --json --color never --ignore-user-config --ignore-rules --strict-config --sandbox read-only -c approval_policy="never" -c allow_login_shell=false --cd <workspace> --output-last-message <path> -` | `workspace-write`, unrestricted execution, ephemeral sessions |
| `claude:print` | `read-only` | `dontAsk` | `claude --print --input-format text --output-format stream-json --verbose --safe-mode --restricted --tools Read,Glob,Grep --disallowedTools mcp__* --strict-mcp-config --mcp-config <empty-mcp.json> --settings <profile.json> --permission-mode dontAsk --permission-prompts none --disable-slash-commands --no-chrome --session-id <uuid>` | `workspace-write`, shell execution, Agent/subagents, custom tools, background mode |

## 2. Launch Planning and Preflight Policy
- **Input Delivery**: Brief text is delivered exclusively via finite regular file passed to child stdin, followed by immediate EOF. Brief text is never passed in argv. Brief size limit is 8 MiB.
- **Argv Construction**: Built strictly as Go string slices (`[]string`), executed directly via `exec.Command` without shell wrapper or reparsing.
- **Working Directory**: Set strictly to the validated canonical workspace directory (`Cmd.Dir = workdir`).
- **Policy Verification**:
  - Resolves non-secret effective configuration across applicable project, user, system, and managed sources.
  - Computes a normalized policy digest at task admission and re-verifies it immediately before launch. Any configuration drift or unrecognized policy fails closed with `unsupported-effective-config`.
- **Credential Safety**: Credentials, tokens, and unrelated user environment variables are never included in command arguments, logs, metadata, or committed fixtures.

## 3. Evidence Capture and Publication Predicates
- **Raw Evidence**: Standard streams are piped to task-owned files (`raw/stdout`, `raw/stderr`). Child output writers must complete and flush before the seal (`provider.exit`) is committed.
- **Success Criteria**:
  - Valid envelope structure (JSON/JSONL) with positive status and non-whitespace final response string.
  - Exact match of task and session identities (expected session matches provider-reported session).
  - Absolute absence of timeout markers (e.g. `[agy] print timeout`), refusal envelopes, or error payloads.
  - Nonzero exit code with a success-shaped body is treated as a conflicting completion and rejected.
- **Continuation**:
  - Continuation tasks reference layer-recorded session handles from completed tasks.
  - Active sessions are protected by durable session claims under `.session.lock`.
  - Continuation invokes the provider using explicit session resumption flags (e.g. `--conversation <id>`, `resume <uuid> -`, `--resume <uuid>`).

## 4. Usage Accounting
- Usage metrics are recorded with explicit scope: `task`, `conversation-cumulative`, `main-agent`, `whole-tree`, or `unknown`.
- Missing or unreported fields remain null; they are never assumed to be zero.
- Cumulative counters from successive turns are never conflated with single-turn usage unless explicitly calculated from verified endpoints.
