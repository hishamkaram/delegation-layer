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
unnecessary for the sandboxed read pipeline below. Existing files are never
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
  result-read pipeline below intentionally stays inside the sandbox.
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

## Read a complete result in bounded pages

After committed publication, save the exact `payload` descriptor from
`collect` to a regular file in the workspace using a separate tool call.
Do not infer a private task-directory path or use raw logs as the answer.
An ordinary local task can then be read without connector guards. Use this
sandboxed Python recipe to assemble the answer without sending base64 through
the model. The user must have a working `python3`; on macOS an installer stub
is not a working interpreter. If Python or native read access is unavailable,
stop and report it. Do not add readable/writable state roots or change rules.

Replace the executable, task ID, descriptor and lead-owned destination directory
below. A recorded custom root needs its exact `--root ABS` in the subprocess
argv after the task ID. The script calls Delegate directly, checks every exit
status and page, and publishes the answer only after whole-file verification.
A pre-existing `.partial` or final answer is preserved rather than overwritten.

```sh
python3 - /opt/homebrew/bin/delegate TASK_ID /absolute/workspace/worker-payload.json /absolute/workspace <<'PYRESULT'
import base64, hashlib, json, os, subprocess, sys, time
from pathlib import Path

executable, task, descriptor_file, directory = sys.argv[1:]
if len(task) != 32 or any(c not in "0123456789abcdef" for c in task):
    raise SystemExit("invalid task ID")
expected = json.loads(Path(descriptor_file).read_text(encoding="utf-8"))
length = expected["length"]
if type(length) is not int or length < 0:
    raise SystemExit("invalid payload length")
final = Path(directory) / (task + ".answer")
partial = final.with_suffix(".answer.partial")
if final.exists() or final.is_symlink():
    raise SystemExit("answer exists; inspect the existing file")
owned = False
deadline = time.monotonic() + 300
try:
    with partial.open("xb") as output:
        owned = True
        offset, digest = 0, hashlib.sha256()
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise SystemExit("result reading deadline expired")
            result = subprocess.run(
                [executable, "result", "read", task, "--expected-descriptor", descriptor_file,
                 "--offset", str(offset), "--limit", "65536", "--json"],
                capture_output=True, check=False, timeout=min(45, remaining))
            if result.returncode != 0:
                try:
                    fault = json.loads(result.stdout)
                except (json.JSONDecodeError, UnicodeDecodeError):
                    detail = "no valid JSON error response"
                else:
                    detail = fault.get("error") if isinstance(fault, dict) else None
                    if not isinstance(detail, str):
                        detail = "no control error message"
                raise SystemExit(f"result read failed with exit {result.returncode}: {detail[:500]}; no answer published")
            response = json.loads(result.stdout)
            if not isinstance(response, dict):
                raise SystemExit("invalid control JSON object")
            page = response.get("result_range")
            if response.get("error") or response.get("publication") != "committed" or not isinstance(page, dict):
                raise SystemExit("no committed result page")
            if page.get("descriptor") != expected or type(page.get("offset")) is not int or page["offset"] != offset:
                raise SystemExit("unexpected result descriptor or offset")
            data = base64.b64decode(page["data_base64"], validate=True)
            next_offset, eof = page.get("next_offset"), page.get("eof")
            if type(next_offset) is not int or type(eof) is not bool or next_offset != offset + len(data) or next_offset > length:
                raise SystemExit("inconsistent result cursor")
            if not data and not eof:
                raise SystemExit("result cursor made no progress")
            output.write(data)
            digest.update(data)
            offset = next_offset
            if eof:
                if offset != length or digest.hexdigest() != expected["sha256"]:
                    raise SystemExit("assembled answer failed verification")
                break
    os.link(partial, final)
    partial.unlink()
    owned = False
    print(json.dumps({"path": str(final), "length": length, "verified": True}))
finally:
    if owned:
        partial.unlink(missing_ok=True)
PYRESULT
```

Keep this native tool call in the foreground until it finishes. Where the tool
has a killing timeout, allow more than the script's 300-second budget. If it
yields an execution-session handle instead, keep waiting on that same handle;
do not rerun a reader that is still running or delete its partial file.

This call intentionally runs inside the sandbox and does not match the seven
control-command rules. Its 65536-byte pages stay inside the decoder; only the
verified path and length reach the model. Answer-data digests are permitted;
installed executable contents are never read or hashed. The script owns only
its exclusively created partial file and removes it on handled failure. A
forced process termination may retain that partial; report it to the owner
rather than blindly deleting a pre-existing file on the next attempt.

Read the completed UTF-8 answer with bounded native text reads, roughly **8192
characters or less** per call, until every part has been read. Treat actual tool
truncation as a failed text read and repeat that range with a smaller chunk.
Never assess only the first or last section. Character decoding happens after
byte assembly, preserving UTF-8 characters split across result pages.

Each Delegate read verifies the entire payload before returning a page. Pending,
rejected, mismatched or corrupt evidence fails without publishing an answer;
connector-owned tasks still require full guards.
