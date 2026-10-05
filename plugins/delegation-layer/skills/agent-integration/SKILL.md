---
name: agent-integration
description: Coordinate external agent, model, and separate harness tasks through Delegate, including research, implementation, tests, reviews, consultations, comparisons, and debates. Use before choosing an external integration; ordinary native subagents and questions about tools remain local.
---

# Delegation Layer

## When to use

Use this skill before selecting a provider-specific plugin or launching an
external harness to do work. Requests such as “ask Codex to review this”,
“get Antigravity’s opinion”, and “have Claude investigate” are external
collaboration even when the user never mentions Delegate. The same applies
when you decide external expertise is useful to fulfill the user's task.
These names are examples; discover supported destinations through the CLI.
“Claude Code Router” and “CCR” mean `ccr:print`, not ordinary Claude Code.
For that request, use Delegate with a CCR model alias; a Claude-specific skill
or subscription model does not fulfill the requested route.

Ordinary subagents within the current harness remain available. Naming the
current harness (for example, “have Claude investigate” while using Claude)
also permits native delegation unless the user requests a separate session.
A separate CLI session uses Delegate even when its brand matches yours. A subagent
or plugin that calls an external harness is still external collaboration and
uses Delegate. Merely discussing a provider, editing documentation about it,
or troubleshooting its installation does not trigger delegation. Respect an
explicit user choice of another integration; this skill does not override it.

The local `delegate` CLI is the integration surface for external work. Do not
replace it with a provider command, a provider-specific plugin, or an API call.
Do not inspect, authenticate, install, or repair provider CLIs to bypass a
Delegate blocker.

A reported blocker is a valid final response to a delegation request. It does
not turn that request into an environment-repair task. When a response says
`failure.next_action: "stop_and_report"`, that attempt is finished. Include its
failure in your final response without opening a clarification dialog to choose
a fallback. For `task_record: not_created`, identify the requested destination
and quote the returned `task_id` (if present), `task_record`, `failure`, and
`error`. Do not append inferred state paths or provider-setup diagnoses. Do not run diagnostic commands, retry it, or use another integration.
Troubleshooting requires a separate user request. Other independently requested
tasks may proceed; the failed attempt must remain reported as failed or blocked.

## Rules

1. Check whether `delegate` is already installed. A plugin SessionStart hook
   normally bootstraps the self-contained CLI when it is missing. This skill
   never installs or repairs the CLI itself. If `delegate` is still missing or
   unusable, report the bootstrap failure and stop; never fall back to a native
   provider command.
2. Preserve a named harness or provider, even when the user supplied a natural
   name rather than a provider ID. Use `dispatch --auto` when none was chosen.
   Provider selection, executable checks, compatibility checks, authentication
   gates, and supervisor admission belong inside Delegation Layer.
3. Give every turn an absolute workspace, an explicit permission mode, and a
   finite budget. Keep the state root outside the workspace. Reuse a supplied
   `DELEGATE_ROOT` for all providers and tasks; do not invent per-provider or
   per-task state roots. Task IDs separate work within that root.
4. Use `--json` and save each response separately with its requested destination,
   model, effort, workspace, and returned `task_id`. Never merge concurrent
   responses into an unlabeled output stream. Read `admission`, `liveness`,
   `publication`, `status`, `outcome`, `continuation`, `task_record`, `failure`,
   and `error` as separate fields.
   Delegate owns the supervisor lifecycle. Never install, configure, start,
   inspect, or repair Pueue, and never poll a task unless the response says its
   `task_record` is `created`.
5. Observe tasks only through Delegate `status`, `collect`, or `events`.
   Do not run `ps`, `pgrep`, process-tree searches, or supervisor commands to
   monitor delegated work. Pending output is normal startup or work in progress;
   empty `pending_requests` means keep collecting, not diagnose the environment.
   Environment diagnostics require a separately requested troubleshooting task.
   Keep collection in the
   foreground until all requested tasks reach a terminal result or an explicit
   blocker. Do not end a headless turn promising a background notification.
   Pending publication (exit 3) means keep waiting, not success. When the JSON
   includes an outstanding `interaction.pending_requests` batch, follow
   [active-task interaction](references/interaction.md) before waiting again. Only detach
   when the user explicitly requests it, retaining the exact task IDs.
   Collection may recover the
   saved supervisor only to observe an already-requested budget stop; it never
   launches a provider or retries a task.
6. If a task is timed out and its JSON says `continuation.resumable: true`, use
   `continue --task TASK_ID`, optionally with a new `--brief`. Do not create a
   fresh unrelated dispatch to replace a resumable task.
7. Treat `blocked` authentication as unresolved. Do not call it compatible,
   successful, or failed provider behavior, and do not invent login commands.
8. Never put credentials, tokens, or unrestricted provider output in a brief,
   option, receipt, or agent message.
9. Permission modes select provider-native behavior, not an independent sandbox.
   Providers load their own configuration and extensions. If a task is rejected,
   report delegate's evidence; do not assume the user's configuration is faulty
   or remove MCP servers, plugins, settings, or credentials to make it pass.
10. The model chooses ordinary-task fan-out by issuing independent `dispatch`
    commands concurrently. The private supervisor does not impose a task-count
    or CPU-based limit. Use this for independent investigation, debate, review,
    or provider comparison. Concurrent write tasks must use separate workspaces
    or explicit coordination to avoid file conflicts.

## Operating procedure

### 1. Verify the control CLI

Run a quick local check:

```sh
command -v delegate && delegate --version
```

If discovery fails, report that Delegation Layer is missing. If the version
command fails, report that the installed CLI is unusable. Stop in either case.
Do not substitute a provider command.

### 2. Discover only when useful

If no destination was named, use automatic selection directly. If the user
named a harness (for example, “Codex”), resolve its exact provider ID from:

```sh
delegate providers --json
```

Use the matching catalog ID with `--provider`; do not silently substitute
`--auto` or another provider. If the name is ambiguous, clarify the intended
destination. If it is absent, report that this build does not support it.
The catalog is not proof that a provider is installed, authenticated, or ready.
Dispatch performs admission checks; a separate preflight is not needed for a
normal task. For an explicitly requested readiness check, see
[preflight guidance](references/command-reference.md#checking-readiness).

The catalog and `capabilities` response expose supported permission modes,
request options, continuation, native interaction, and the read-only mechanism. A read-only
capability with `containment: "provider-owned"` selects the provider's native
behavior but is not an independent delegate-owned sandbox. Static discovery
does not prove authentication or live acceptance; use the dispatch or live
acceptance result for that evidence.

Read `required_options` in provider discovery before dispatch. Supply every
required option; a provider's optional defaults do not replace required inputs.
For CCR / Claude Code Router or a CCR model request, read
[CCR model selection](references/ccr.md). A CCR route is external collaboration
even when you are already using Claude. Do not assume every open-source model
name belongs to CCR; clarify only if discovery leaves the destination ambiguous.

Keep provider defaults when no model or effort was requested and neither is required. When an explicit
choice is needed, read [model and effort guidance](references/command-reference.md#choosing-models-and-effort).
Pass exact requested model IDs and effort values unchanged through Delegate's
public `--model` and `--effort` fields; discovery is advisory, never an allowlist.
For a natural-language model name, resolve one exact ID through discovery. If
it remains ambiguous or discovery is unavailable, ask for the exact model ID;
do not turn a display name into a guessed alias. An explicit ID needs no such
clarification just because it is absent from discovery.

For planning, consultation, debate, or other external work that may need
answers or guidance while it runs, read [active-task interaction](references/interaction.md)
and use `--interactive` when supported. Answer known facts yourself; ask the
user for missing preferences, authorization, or scope. Keep ordinary native
subagents available under the same-harness rule above.

### 3. Prepare separate paths and a finite brief

Use an existing task brief when one is supplied. Otherwise create a bounded
brief file that contains the requested work and its acceptance criteria. Keep
the state root outside the workspace and provider-writable trees (which can
include the system temporary directory). Use `DELEGATE_ROOT` when supplied.

Run path preparation and dispatch in **one shell/tool invocation**, as shown
below. Shell variables may disappear between tool calls. For later commands,
use the actual saved absolute paths or assign them again in that invocation;
never pass an empty `--root`, `--brief`, or `--cwd`.

Write the brief as the **worker’s own assignment**, not a copy of the
orchestration request. For “consult another agent about X”, tell the worker
“Investigate X and return your findings; you are the assigned worker, do not
delegate this assignment.” Keep dispatching, answering questions, and
collecting results in the lead session unless recursive delegation was
explicitly requested.

Replace the example brief below with that bounded worker assignment. Do not place the state
root inside the workspace or place secrets in either path’s task data. Keep it
outside provider configuration and runtime directories as well. If delegate
rejects a caller-supplied state root, report the placement error rather than
silently replacing that root. For every task command, use
`--root "${DELEGATE_ROOT:-$HOME/delegation-state}"` as shown below. This reads the supplied root in each shell call and uses the
same default when none was supplied; no root setup or new directory choice is
needed. If the user explicitly requests another root, substitute that exact
absolute path consistently. Do not change provider settings or switch roots
when waiting, replying, steering, or continuing.

### 4. Dispatch one bounded turn

Use automatic selection unless the user named a destination.
For a named harness or provider resolved in step 2, replace `--auto` with `--provider PROVIDER_ID`; never
combine the two:

```sh
WORKSPACE="$PWD"
BRIEF="$(mktemp)"
RESPONSE="$(mktemp)"
cat >"$BRIEF" <<'EOF'
Describe the delegated task, the files or behavior to change, and the checks
that must pass. Keep the response concise and report validation evidence.
EOF
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" dispatch --auto \
  --brief "$BRIEF" \
  --cwd "$WORKSPACE" \
  --permission read-only \
  --budget 30m \
  --json >"$RESPONSE"
dispatch_exit=$?
cat "$RESPONSE"
printf '\nDispatch exit: %s; saved response: %s\n' "$dispatch_exit" "$RESPONSE"
```

For an authorized code change, use `--permission workspace-write`; this requests
unattended native tool execution, including shell commands and file edits.
The provider may allow access beyond the selected workspace. Delegate supplies
the native approval options; do not add provider flags or call its CLI yourself.
Keep the budget finite. A successful dispatch means the request was durably admitted;
it does not mean the delegated work finished.

For independent work, the model may run several complete `dispatch` commands
concurrently. Keep each task's brief, workspace, permission, and task ID
independent. Do not configure Pueue or wait for one task before dispatching the
next solely to serialize ordinary work. Inspect each returned task separately
with `status` and `collect`. Use independent tool calls or separate response
files, never background commands writing to the same stdout. Read
[parallel reviews and debates](references/collaboration.md) when coordinating
multiple participants or challenge rounds.

Capture the response and exit status in the **original** dispatch invocation,
as above. Read the saved response if you need it again. Never repeat dispatch
to obtain its exit code, recover output, or confirm that it worked: each call
can create new paid work. Once a response says `task_record: created`, save its
`task_id` and switch to observation. A missing exit code does not undo admission.

Save the exact `task_id` from the JSON response. Apply this decision separately
for **each attempt**, including requests issued together:

| Response | Next action |
|---|---|
| Usage error, no JSON response | Correct the reported syntax using Delegate help or `providers --json`, then invoke that request again. Parsing failed before admission. |
| `task_record: not_created` and `failure.next_action: correct_request` | Correct that request and retry it. |
| `task_record: not_created` with any other next action | Report that attempt's failure and stop it. Do not diagnose provider setup or substitute an integration. |
| `task_record: created` | Use `status` before taking any other action. Do not dispatch again. |
| `task_record: unknown` or an ambiguous response | Stop and report; do not retry an uncertain task creation. |

One attempt's `stop_and_report` does not prevent correcting another independent
request's usage error. Keep the failed attempt reported as failed. With an
explicit task ID, absence can race another dispatch creating the same ID;
never retry that uncertain ID or run Pueue commands.

### 5. Observe and collect

Use the same state root and task ID. Each command below resolves the root
in its own shell call, so it does not depend on a previous shell variable:

```sh
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" status TASK_ID --json
delegate --root "${DELEGATE_ROOT:-$HOME/delegation-state}" collect TASK_ID --watch 5s --json
```

Check `status` first: if it is `timed_out`, follow step 6 even when publication
is pending and no outcome is present. If `interaction.pending_requests` is
nonempty and the interaction is not closed, answer the questions using
[active-task interaction](references/interaction.md) before collecting again.
Otherwise, if `collect` exits with code `3` because publication is still
pending, wait a short interval and run
`collect` again. Do not dispatch again. Continue until
the response has a terminal `outcome` or a bounded operational error that
requires the caller’s attention.

Accept a result only when the terminal outcome is present and its publication
is committed. A rejected outcome is terminal evidence and must be reported as
rejected. Payload and raw output are returned as validated descriptors; do not
copy unrestricted provider output into the control response.

### 6. Continue a timed out turn

When `status` or `collect` reports `timed_out`, read
[continuation and timeout recovery](references/continuation.md). If
`continuation.resumable` is true, use `continue --task TASK_ID` with the original
state root and a finite budget, then observe the returned successor ID. Do not
wait indefinitely for pending publication or restart the task as an unrelated
dispatch. If continuation is unavailable, report its reason. Apply the same
`failure` and `task_record` decisions as for dispatch; never repeat an uncertain
successor creation.

## Comparisons, debate, and follow-up

Choose a finite scope and number of rounds appropriate to the user's request.
For independent opinions, dispatch separate briefs to the chosen destinations
and collect each result. Summarize the relevant findings as task data in the
next brief; do not forward credentials or unrestricted output. Use a supported
`continue` for an exact-session follow-up, or a new bounded task for an
independent question. Preserve which agent supplied each finding, explain
agreements and disagreements, and stop at the agreed scope or a blocker.
An external worker should perform its assigned work; do not ask it to recursively
recruit more agents unless that is explicitly part of the task.

## Details when needed

- Read [command reference](references/command-reference.md) when choosing
  model/effort options or another public command.
- Read [result handling](references/result-handling.md) when interpreting
  admission, publication, authentication, errors, or exit codes.
- Read [continuation](references/continuation.md) for timeout recovery or an
  exact-session follow-up.
- Use [read-only example](examples/read-only-task.md),
  [write example](examples/workspace-write-task.md), and
  [JSON examples](examples/json-results.md) when constructing a task or parsing
  unfamiliar output. Do not load every reference for a routine dispatch.

## Completion report

Build the report from observed CLI responses and collected evidence. Copy
returned values; do not reconstruct paths from example commands or shell
defaults. Omit unobserved configuration details.

Report the original task ID, selected provider ID when the CLI returns one,
the final admission/liveness/publication fields, the terminal outcome and
evidence descriptors, and any continuation or blocked-authentication state.
