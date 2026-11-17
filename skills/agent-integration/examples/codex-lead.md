# Codex leads using the Delegate CLI

A recurring `codex:exec` workspace-write lead can use the same local
`dispatch`, `status`, `wait` and `collect` commands as other leads after the
user approves the native execution rule below. No Gateway or extra schedule
option is needed. Provider configuration remains user-owned; Delegate neither
installs this rule nor changes trust, accounts or sandbox settings.

## One-time user setup

Choose the absolute path of your installed Delegate, outside provider-writable
directories. Use that exact path in the rule and lead commands. The examples
use `/opt/homebrew/bin/delegate`; replace it with your verified installation
path. Do not assume this path exists on another machine. Run installed files
in place; do not copy them into a workspace or state directory.

The user places this in a separate `delegate.rules` file in the user-owned
`$CODEX_HOME/rules/` directory (normally `~/.codex/rules/`), outside every
provider-writable workspace. Do not put this machine-specific grant in a
project's `.codex/rules/`. Preserve existing rules. A managed `forbidden`
rule takes precedence. If the installation path changes, the user must update
this file and the lead's commands.

```python
prefix_rule(
    pattern = ["/opt/homebrew/bin/delegate", ["dispatch", "status", "wait", "collect", "logs", "continue", "cancel"]],
    decision = "allow",
)
```

### Create the rule file

Run this yourself on each machine where Codex will lead, passing the installed
executable you chose as an explicit absolute argument. Review both the chosen
alias and its printed resolved target; do not select a workspace/temp executable
or a shim controlled by project settings. The script checks file metadata,
writes only the rule above, and refuses to overwrite an existing file. Delegate
does not run this setup automatically.

```sh
python3 - /opt/homebrew/bin/delegate <<'PYRULE'
import json
import os
import sys
from pathlib import Path

if len(sys.argv) != 2:
    raise SystemExit("pass one explicit absolute installed Delegate path")
executable = Path(sys.argv[1])
if not executable.is_absolute() or not executable.is_file() or not os.access(executable, os.X_OK):
    raise SystemExit("selected Delegate must be an absolute executable file")
print(f"Selected executable: {executable}")
print(f"Resolved target: {executable.resolve(strict=True)}")

home = Path(os.environ.get("CODEX_HOME") or Path.home() / ".codex")
if not home.is_absolute():
    raise SystemExit("CODEX_HOME must be an absolute user-owned configuration directory")
rules = home / "rules" / "delegate.rules"
rules.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
pattern = [str(executable), ["dispatch", "status", "wait", "collect",
                             "logs", "continue", "cancel"]]
with rules.open("x", encoding="utf-8") as output:
    output.write(f'prefix_rule(pattern = {json.dumps(pattern)}, decision = "allow")\n')
print(f"Created: {rules}")
PYRULE
```

Start a new Codex session afterward so its configuration loads the rule.
Use the selected alias and printed rule path in the policy check below; the
rule deliberately preserves that alias rather than its versioned target.
A successful parser check does not override a managed denial or establish
that live nested dispatch works in your native execution context.

If an older setup also granted `[ABS, "result", "read"]`, that extra grant is
unnecessary for the sandboxed result commands below. Existing files are never
edited automatically; the user may review and remove just that extra rule.

This is an explicit grant to run those commands outside Codex's sandbox.
It does **not** restrict trailing flags, roots, runners, worker providers or
permissions. It applies to any session loading the rule, including read-only
sessions. Codex workers load the same user rules, so even a read-only Codex
worker can dispatch a write-capable worker through this grant. Unsandboxed
worker providers retain their native access to user files and settings. The parent sandbox is not a write boundary for approved
Delegate operations or their descendants. Omit commands the workflow does
not need; never replace the absolute path with a basename or allow a shell.

Syntax and grant behavior were checked against Codex 0.169.0's
[execution-policy format](https://github.com/openai/codex/blob/rust-v0.169.0/codex-rs/execpolicy/README.md)
and [native rule loading and approval decisions](https://github.com/openai/codex/blob/rust-v0.169.0/codex-rs/core/src/exec_policy.rs).
Check your installed version and effective policy; parser acceptance alone
does not prove a native lead can complete work. For example, the user can
inspect this rule without executing the command:

```sh
codex execpolicy check --rules /absolute/path/to/delegate.rules --resolve-host-executables -- /opt/homebrew/bin/delegate status TASK_ID --json
```

## Lead instructions

- For the seven granted control commands, issue one plain literal Delegate
  command per native shell-tool call. Do not wrap it in variables, command
  substitutions, conditionals, pipes, redirections or heredocs. Prepare brief
  and descriptor files in separate steps. Capture that call's stdout and exit
  status once through the tool; do not redispatch to recover output. The passive
  result-saving command below intentionally stays inside the sandbox.
- Use the approved absolute executable path before the subcommand. Ordinarily
  omit `--root`; let the CLI resolve its default. For an explicitly supplied or
  recorded custom root, put its exact `--root ABS` after the subcommand on
  every call. Never derive a root from `DELEGATE_ROOT` or a sample path.
- Keep Delegate state outside every provider-writable workspace, runtime and
  temporary directory. Do not add it to Codex writable roots to fix a denial.
- Request the exact provider, model, effort and permission intended for each
  worker. Both read-only and workspace-write workers are supported. Shared
  workspace workers need disjoint write ownership assigned by the lead;
  [managed workspaces](../references/workspaces.md) remain optional isolation.
- An external supervisor queue must allow the lead and its workers to run
  concurrently; a one-slot queue leaves workers waiting behind the lead.
- Apply [task creation](../references/result-handling.md#task-creation) before
  observing a returned task ID. Use finite passive `wait` and foreground
  `collect`, not shell polling loops. No automatic provider retry is added.
- The rule does not grant administration (`supervisor`, `scheduler`, `settle`),
  task tools (`workspace`, `checkpoint`, `schedule`, `recheck`), interaction
  (`reply`, `steer`, `events`) or connector tools (`gateway`, `context`, `resolve`,
  `origin`). Global-flag-first forms also miss the prefix. If normal guidance
  requires one and native policy blocks it, stop and report the operator action.
  For `supervisor_upgrade_required`, report `run_supervisor_migrate`; after the
  user migrates outside the lead, the creation table's single exact retry still
  applies. Do not broaden rules or change permissions automatically.
- Native environment filtering is unchanged. An environment-only worker login
  may be unavailable in the lead's shell. Diagnose the returned failure;
  do not inject credentials or change the native environment policy.

## Save and read a complete result

`result save` requires a build newer than v0.7.0. Check `delegate help result save`
for your installed build. Users of v0.7.0 can use its
[historical decoding recipe](https://github.com/hishamkaram/delegation-layer/blob/v0.7.0/skills/agent-integration/examples/codex-lead.md#read-a-complete-result-in-bounded-pages).

After committed publication, save the exact `payload` descriptor from `collect`
to a regular file using a separate native tool call. Then run the Go command:

```sh
/opt/homebrew/bin/delegate result save TASK_ID --expected-descriptor /absolute/workspace/worker-payload.json --output /absolute/workspace/TASK_ID.answer --json
```

Replace the executable, task ID and workspace paths. A recorded custom root
needs its exact `--root ABS` after the task ID. Keep this native call in the
foreground; when it yields a session handle, await that same handle. Saving
runs inside the sandbox and does not need an outside-sandbox execution grant.
Do not add `result save` to the seven-command rule or broaden writable roots.
The command itself does not require Python.

Success returns `publication: committed` and `saved` containing `path`, `length`
and `sha256`, without answer bytes. The Go reader verifies the complete payload
in one pass before publishing the caller-owned file. The parent must already
exist, the destination must be outside the selected state root, and an existing
file, directory or symlink is never overwritten. Task state remains read-only;
saving never launches, repairs or retries provider work.

If `result_saved_cleanup_incomplete` is returned with `saved`, the complete,
verified output already exists. Preserve it and report the cleanup error;
do not retry or delete the output. A forced process termination can retain an
owned partial file; report it rather than blindly removing other files.

Read the saved UTF-8 answer with bounded native text reads, roughly **8192
characters or less** per call, until every part has been read. Treat tool-output
truncation as an incomplete read and repeat that text range with a smaller
chunk. Never assess only the first or last section. UTF-8 interpretation happens
after complete byte verification, preserving characters across buffer boundaries.

[`result read`](../references/command-reference.md#result-read) remains available
for consumers needing bounded byte ranges. Connector tasks retain their full
guards and cannot use the ordinary save route. Missing, pending, rejected,
mismatched, corrupt or timed-out evidence fails without publishing an answer.
