#!/usr/bin/env python3
"""Run the bounded Codex ``exec`` acceptance gate.

The gate exercises the normal shipped delegate and runner against a private
real pueue instance. It records acceptance evidence. The installed Codex
executable is used in place and is never copied or renamed.
"""

from __future__ import annotations

import argparse
from copy import deepcopy
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import shutil
import sys
import time
import traceback
from datetime import datetime, timezone

from acceptance_provider_common import (
    AcceptanceFailure,
    NativeTaskOps,
    canonical_go_json,
    clean_absolute,
    ensure_private_directory,
    failure_queue_finished,
    no_prompt_argv as _no_prompt_argv,
    path_is_within,
    reject_tmp,
    require,
    snapshot,
    supervisor_binding,
    status_jobs,
    unique_object,
    verify_collected_outcome,
    write_bytes,
)
from acceptance_supervisor_common import (
    Processes,
    config_for,
    digest,
    read_json,
    sha,
    write_json,
)


PROVIDER = "codex:exec"
MODE = "read-only"
APPROVAL = "never"
# The predicate revision describes the adapter's output contract. It is
# deliberately independent of the installed Codex CLI release, which is
# admitted through the runtime capability probe.
PREDICATE_VERSION = "runtime-reported"
PREDICATE_SHA256 = "3c5bdc1866330d2e7280422f9ad865c9ad29b16ceea16c19796c991ba7e1cb67"
OUTPUT_NAME = "codex-last-message.txt"
OUTPUT_WRITER_CONTRACT = "process-exit-eof-v1"
TASK_BUDGET = "120s"
CANONICAL_TASK_BUDGET = "2m0s"
WATCH_SECONDS = 150
PLANNED_NATIVE_AI_TURNS = 2
ACCEPTANCE_STATUS = "acceptance-passed"
PRELAUNCH_STATUS = "planned"
PUEUE_VERSION = "4.0.4"
MAX_CONTROL_BYTES = 1 << 20
MAX_PROVIDER_BYTES = 8 << 20
MAX_INT64 = (1 << 63) - 1
MAX_ITEMS = 1024
MAX_ITEM_ID_BYTES = 256
MAX_ITEM_TYPE_BYTES = 128
OUTPUT_ARGUMENT_INDEX = 11
SCRIPT_DIR = Path(__file__).resolve().parent
REPO_ROOT = SCRIPT_DIR.parent
DEFAULT_STATE_PARENT = Path.home() / "Library" / "Application Support" / "delegation-layer-acceptance"
DEFAULT_WORKSPACE_PARENT = Path.home() / "Active-Projects" / "delegation-layer-acceptance"
PUEUE_PARENT = Path("/Users/Shared") if sys.platform == "darwin" else Path.home() / ".dl-acceptance"
RUNTIME_INSPECTION_REVISION = "runtime-capability-v1"
RUNTIME_HELP_ARGS = ["exec"]
RUNTIME_REQUIRED_FLAGS = [
    "-c", "--sandbox", "--cd",
    "--output-last-message", "--json", "--color",
]
CODEX_ENVIRONMENT_KEYS = (
    "HOME", "CODEX_HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE",
    "TZ", "TMPDIR", "TMP", "TEMP", "XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME",
    "XDG_DATA_DIRS", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
    "DBUS_SESSION_BUS_ADDRESS", "GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTUP_HOME",
    "GRADLE_USER_HOME", "NPM_CONFIG_CACHE", "GOPATH", "__CF_USER_TEXT_ENCODING",
)
NATIVE_PROFILE_REVISION = "native-permissions-v1"
UUID_PATTERN = re.compile(r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")
TASK_PATTERN = re.compile(r"^[0-9a-f]{32}$")


def expected_codex_inspection_binding(
        pueue: Path, config: Path, base: Path, config_digest: str,
        runner: Path, workspace: Path, environment: dict[str, str],
        provider: Path, provider_digest: str) -> dict[str, object]:
    """Build the expected runtime-only inspection binding from setup sources."""
    values = sorted(key + "=" + environment[key]
                    for key in CODEX_ENVIRONMENT_KEYS if key in environment)
    definition = {
        "revision": RUNTIME_INSPECTION_REVISION,
        "executable": str(provider),
        "executable_sha256": provider_digest,
        "arguments": None,
        "directory": str(workspace),
        "environment": values,
        "output_limit": 1 << 20,
        "runtime": {
            "executable": str(provider),
            "executable_sha256": provider_digest,
            "directory": str(workspace),
            "environment": values,
            "help_args": RUNTIME_HELP_ARGS,
            "required_flags": RUNTIME_REQUIRED_FLAGS,
        },
    }
    return {
        "definition_revision": RUNTIME_INSPECTION_REVISION,
        "definition_sha256": sha(canonical_go_json(definition)),
        "helper_executable": str(provider),
        "helper_sha256": provider_digest,
        "worker_executable": str(runner),
        "worker_sha256": digest(runner),
        "environment": values,
        "supervisor": supervisor_binding(pueue, config, base, config_digest),
    }


class BlockedFailure(AcceptanceFailure):
    """A required native prerequisite is unavailable on this host."""


def provider_runtime_roots(environment: dict[str, str]) -> list[Path]:
    """Return roots where Codex or its toolchain may write runtime state."""
    home = Path(environment.get("HOME", str(Path.home()))).resolve(strict=False)
    roots = [Path(item) for item in ("/tmp", "/var/tmp", "/var/folders", "/dev")]
    roots.extend(home / item for item in (
        ".codex", ".cache", ".cargo", ".rustup", ".npm", ".nvm", ".bun", ".gradle",
        ".m2", ".local", "Library/Caches", "Library/Logs",
    ))
    for name in ("TMPDIR", "TMP", "TEMP", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
                 "GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTUP_HOME",
                 "GRADLE_USER_HOME", "NPM_CONFIG_CACHE"):
        value = environment.get(name)
        if value and not (name in {"GOCACHE", "GOMODCACHE"} and value == "off"):
            roots.append(Path(value))
    if environment.get("CODEX_HOME"):
        roots.append(Path(environment["CODEX_HOME"]))
    if environment.get("GOPATH"):
        roots.extend(Path(item) for item in environment["GOPATH"].split(os.pathsep) if item)
    return sorted({root.resolve(strict=False) for root in roots})


def reject_runtime_roots(path: Path, label: str, environment: dict[str, str]) -> None:
    resolved = path.resolve(strict=False)
    for root in provider_runtime_roots(environment):
        if resolved == root or root in resolved.parents:
            raise AcceptanceFailure(f"{label} is inside provider writable runtime root {root}: {resolved}")


def task_id() -> str:
    return secrets.token_hex(16)


def is_task_id(value: object) -> bool:
    return isinstance(value, str) and TASK_PATTERN.fullmatch(value) is not None


def is_thread_id(value: object) -> bool:
    return isinstance(value, str) and UUID_PATTERN.fullmatch(value) is not None


def resolve_executable(value: str | None, default: Path, label: str) -> Path:
    candidate = Path(value) if value else default
    if not candidate.is_absolute():
        candidate = candidate.resolve()
    # Installed package managers commonly expose the selected CLI through a
    # symlink. Resolve that alias before validating, recording, and hashing the
    # regular executable so PATH discovery and invocation use the same binary.
    if not candidate.is_file() or not os.access(candidate, os.X_OK):
        raise BlockedFailure(f"{label} is unavailable or not executable: {candidate}")
    resolved = candidate.resolve(strict=True)
    if not resolved.is_file() or not os.access(resolved, os.X_OK):
        raise BlockedFailure(f"{label} resolved target is not executable: {resolved}")
    return resolved


def no_prompt_argv(argv: list[object], briefs: list[Path]) -> None:
    """Codex transport guard, including its continuation-only prohibition."""
    _no_prompt_argv(argv, briefs, forbidden=("--ephemeral",))


def require_discovery(name: str, selected: Path) -> None:
    discovered = shutil.which(name)
    if discovered is None:
        raise BlockedFailure(f"{name} is not discoverable in the inherited PATH")
    if Path(discovered).resolve() != selected:
        raise BlockedFailure(f"{name} discovery does not match selected executable; PATH shadowing is forbidden")


def reject_constant(value: str) -> None:
    raise ValueError("nonfinite JSON number: " + value)


def decode_event(line: bytes) -> dict[str, object]:
    require(line and line.strip(), "blank Codex JSONL event line")
    require(len(line) <= MAX_CONTROL_BYTES, "Codex JSONL event line exceeds 1 MiB")
    try:
        text = line.decode("utf-8")
        value = json.loads(text, object_pairs_hook=unique_object, parse_constant=reject_constant)
    except (UnicodeError, ValueError, TypeError, RecursionError) as error:
        raise AcceptanceFailure(f"invalid Codex JSONL event: {error}") from error
    require(isinstance(value, dict), "Codex JSONL event is not an object")
    require(isinstance(value.get("type"), str) and value["type"],
            "Codex JSONL event type is missing")
    return value


def nonnegative_counter(value: object, label: str) -> int | None:
    if value is None:
        return None
    require(isinstance(value, int) and not isinstance(value, bool) and 0 <= value <= MAX_INT64,
            f"{label} is not a nonnegative integer")
    return value


def parse_usage(event: dict[str, object]) -> dict[str, int | None] | None:
    usage = event.get("usage")
    if usage is None:
        return None
    require(isinstance(usage, dict), "turn.completed usage is not an object")
    return {
        "input_tokens": nonnegative_counter(usage.get("input_tokens"), "usage input_tokens"),
        "cached_input_tokens": nonnegative_counter(usage.get("cached_input_tokens"), "usage cached_input_tokens"),
        "cache_write_input_tokens": nonnegative_counter(usage.get("cache_write_input_tokens"), "usage cache_write_input_tokens"),
        "output_tokens": nonnegative_counter(usage.get("output_tokens"), "usage output_tokens"),
        "reasoning_output_tokens": nonnegative_counter(usage.get("reasoning_output_tokens"), "usage reasoning_output_tokens"),
    }


def parse_codex_events(raw: bytes) -> dict[str, object]:
    """Apply the immutable Codex JSONL selection and completion rules."""
    require(len(raw) <= MAX_PROVIDER_BYTES, "Codex stdout exceeds observation bound")
    lines = raw.split(b"\n")
    if lines and lines[-1] == b"":
        lines.pop()
    require(lines, "Codex JSONL stdout is empty")
    thread_id: str | None = None
    turn_started = False
    turn_completed = False
    terminal_failed = False
    interrupted = False
    semantic_error: str | None = None
    final_message: bytes | None = None
    usage: dict[str, int | None] | None = None
    items: dict[str, dict[str, object]] = {}
    commands: dict[str, dict[str, object]] = {}
    unknown_events: list[str] = []

    def fault(message: str) -> None:
        nonlocal semantic_error
        if semantic_error is None:
            semantic_error = message

    for raw_line in lines:
        try:
            event = decode_event(raw_line)
        except AcceptanceFailure as error:
            fault(str(error))
            continue
        event_type = event["type"]
        if event_type == "thread.started":
            if thread_id is not None:
                fault("multiple thread.started events")
                continue
            value = event.get("thread_id")
            if not is_thread_id(value):
                fault("thread.started thread_id is not a UUID")
                continue
            thread_id = value
        elif event_type == "turn.started":
            if thread_id is None:
                fault("turn.started precedes thread.started")
            elif turn_started or turn_completed or terminal_failed or interrupted:
                fault("duplicate or late turn.started event")
            else:
                turn_started = True
        elif event_type == "turn.completed":
            if thread_id is None or not turn_started:
                fault("turn.completed has no active turn")
            elif turn_completed or terminal_failed or interrupted:
                fault("conflicting turn terminal events")
            else:
                status = event.get("status")
                if status is not None and not isinstance(status, str):
                    fault("turn.completed status is not a string")
                elif status not in (None, "", "completed"):
                    interrupted = True
                else:
                    try:
                        usage = parse_usage(event)
                    except AcceptanceFailure as error:
                        fault(str(error))
                    else:
                        turn_completed = True
        elif event_type == "turn.failed":
            if thread_id is None or not turn_started or turn_completed or terminal_failed or interrupted:
                fault("turn.failed has no active turn")
            else:
                failure = event.get("error")
                if not isinstance(failure, dict):
                    fault("turn.failed error is not an object")
                elif not isinstance(failure.get("message"), str):
                    fault("turn.failed error message is not a string")
                else:
                    terminal_failed = True
        elif event_type in {"turn.interrupted", "turn.cancelled", "turn.aborted"}:
            if (thread_id is None or not turn_started or turn_completed or terminal_failed or interrupted):
                fault("terminal interruption follows turn completion")
            else:
                interrupted = True
        elif event_type == "error":
            if not isinstance(event.get("message"), str):
                fault("error message is not a string")
            elif turn_completed:
                fault("error event follows turn completion")
        elif event_type in {"item.started", "item.updated", "item.completed"}:
            if thread_id is None or not turn_started or turn_completed or terminal_failed or interrupted:
                fault("item event is outside the active turn")
                continue
            item = event.get("item")
            if not isinstance(item, dict):
                fault("item event has no object item")
                continue
            item_id = item.get("id")
            item_type = item.get("type")
            if not isinstance(item_id, str) or not item_id:
                fault("item id is not a nonempty string")
                continue
            if not isinstance(item_type, str) or not item_type:
                fault("item type is not a nonempty string")
                continue
            if len(item_id.encode("utf-8")) > MAX_ITEM_ID_BYTES:
                fault("item id exceeds bounded size")
                continue
            if len(item_type.encode("utf-8")) > MAX_ITEM_TYPE_BYTES:
                fault("item type exceeds bounded size")
                continue
            if item_id not in items and len(items) >= MAX_ITEMS:
                fault("item identity state exceeds bounded count")
                continue
            state = items.setdefault(item_id, {"type": item_type, "started": False,
                                               "updated": False, "completed": False})
            if state["type"] != item_type:
                fault("item id changed type")
                continue
            if item_type == "agent_message":
                text = item.get("text")
                if not isinstance(text, str):
                    fault("agent_message text is not a string")
                    continue
                state["text"] = text
            elif item_type == "command_execution":
                command = commands.setdefault(item_id, {"id": item_id, "type": item_type})
                for key in ("command", "aggregated_output", "status", "exit_code"):
                    if key in item:
                        command[key] = item[key]
            if event_type == "item.started":
                if state["started"] or state["completed"]:
                    fault("duplicate item.started event")
                else:
                    state["started"] = True
            elif event_type == "item.updated":
                if state["completed"]:
                    fault("item.updated follows completion")
                else:
                    state["updated"] = True
            else:
                if state["completed"]:
                    fault("duplicate item.completed event")
                else:
                    state["completed"] = True
                    if item_type == "agent_message":
                        final_message = state.get("text", "").encode("utf-8")
        else:
            # Unknown nonterminal events are checked for strict JSON/UTF-8 but
            # are otherwise tolerated by the general producer parser.  The
            # resume oracle applies a stricter tool-free policy below.
            if event_type.startswith("thread.") or event_type.startswith("turn."):
                fault("unknown thread/turn lifecycle event: " + event_type)
            else:
                unknown_events.append(event_type)

    require(semantic_error is None, semantic_error or "invalid Codex JSONL evidence")
    require(thread_id is not None, "Codex JSONL has no thread.started identity")
    require(turn_started and turn_completed and not terminal_failed and not interrupted,
            "Codex JSONL has no successful completed turn")
    require(final_message is not None and final_message.strip(),
            "Codex JSONL has no non-whitespace final agent message")
    # A command is evidence only after its terminal item.completed event.  A
    # started or updated command can contain a planned command string, but it
    # does not prove that the provider attempted that command.
    completed_commands = [command for item_id, command in commands.items()
                          if items.get(item_id, {}).get("completed") is True]
    incomplete_commands = [item_id for item_id, state in items.items()
                           if state.get("type") == "command_execution" and
                           state.get("completed") is not True]
    item_states = [{"id": item_id, "type": state["type"],
                    "started": state["started"], "updated": state["updated"],
                    "completed": state["completed"]}
                   for item_id, state in items.items()]
    return {"thread_id": thread_id, "final_message": final_message,
            "usage": usage, "commands": completed_commands,
            "incomplete_commands": incomplete_commands,
            "item_states": item_states, "unknown_events": unknown_events}


RESUME_ALLOWED_ITEM_TYPES = frozenset({"agent_message", "reasoning"})


def validate_tool_free_resume(parsed: dict[str, object], expected_answer: bytes) -> dict[str, object]:
    """Require a resumed turn to contain only conversational item kinds."""
    require(isinstance(expected_answer, bytes), "resume expected answer is not bytes")
    item_states = parsed.get("item_states")
    require(isinstance(item_states, list), "resume item type evidence is absent")
    unknown_events = parsed.get("unknown_events")
    require(isinstance(unknown_events, list) and not unknown_events,
            "resume contains unknown event types")
    types: list[str] = []
    for state in item_states:
        require(isinstance(state, dict), "resume item type evidence is malformed")
        item_type = state.get("type")
        require(isinstance(item_type, str) and item_type in RESUME_ALLOWED_ITEM_TYPES,
                "resume contains disallowed item type: " + str(item_type))
        types.append(item_type)
    commands = parsed.get("commands")
    require(isinstance(commands, list) and not commands,
            "resume contains completed command_execution evidence")
    require(parsed.get("final_message") == expected_answer,
            "resume final message is not the exact recovered nonce")
    return {"item_count": len(item_states), "item_types": sorted(set(types)),
            "tool_free": True}


def shell_tokens(command: str) -> list[str] | None:
    """Tokenize one shell command while preserving operator boundaries."""
    try:
        lexer = shlex.shlex(command, posix=True, punctuation_chars="><;&|")
        lexer.whitespace_split = True
        return list(lexer)
    except ValueError:
        return None


RECOGNIZED_SHELL_WRAPPERS = frozenset({"/bin/bash", "/bin/sh", "/bin/zsh"})
REJECTED_SHELL_SYNTAX = frozenset({"\x00", "\n", "\r", "$", "`", ";", "&", "|", ">", "<"})


def unwrap_shell_command(command: str) -> list[str] | None:
    """Unwrap one finite, recognized absolute-shell ``-c`` or ``-lc`` invocation.

    Codex may ask its inherited shell to run a command.  Only an exact
    ``/bin/<shell> (-c|-lc) <one string>`` shape is unwrapped; shell operators,
    additional arguments, and nested wrappers remain visible to the caller.
    """
    if any(character in command for character in REJECTED_SHELL_SYNTAX):
        return None
    tokens = shell_tokens(command)
    if tokens is None:
        return None
    if len(tokens) == 2:
        return tokens
    if len(tokens) != 3 or tokens[0] not in RECOGNIZED_SHELL_WRAPPERS or tokens[1] not in {"-c", "-lc"}:
        return tokens
    return shell_tokens(tokens[2])


def command_reads_path(command: str, path: Path) -> bool:
    """Recognize one exact ``cat`` read, including the native shell wrapper."""
    tokens = unwrap_shell_command(command)
    if tokens is None or not tokens:
        return False
    executable = tokens[0]
    if executable != "cat" and (not executable.startswith("/") or Path(executable).name != "cat"):
        return False
    target = str(path)
    return tokens[1:] in ([target], ["--", target])


def exact_probe_command(command: str, probe: Path) -> bool:
    """Match only the native probe command after one recognized shell wrapper."""
    target = str(probe)
    if not probe.is_absolute() or probe != Path(os.path.normpath(target)):
        return False
    tokens = unwrap_shell_command(command)
    return tokens == ["/bin/sh", target]


PROBE_MARKERS = {
    "probe.v1.read_rc": "read_rc",
    "probe.v1.nonce": "nonce",
    "probe.v1.workspace_rc": "workspace_rc",
    "probe.v1.workspace_stderr": "workspace_stderr",
    "probe.v1.sibling_rc": "sibling_rc",
    "probe.v1.sibling_stderr": "sibling_stderr",
}
DENIAL_WORDS = (
    "permission denied", "operation not permitted", "not permitted",
    "permission", "denied", "read-only", "read only", "sandbox",
)


def probe_script_bytes(nonce_file: Path, inside: Path, sibling: Path) -> bytes:
    """Build the immutable shell probe from paths only.

    The probe reads the nonce and then performs both writes in independent
    grouped substitutions.  No nonce value is embedded in the script, so the
    script itself can be safely bound by a digest in the acceptance receipt.
    """
    paths = (nonce_file, inside, sibling)
    for path in paths:
        value = str(path)
        require(path.is_absolute() and path == Path(os.path.normpath(value)),
                "probe paths must be absolute and clean")
        require("\x00" not in value and "\r" not in value and "\n" not in value,
                "probe paths cannot contain shell line breaks")
    def quote_path(path: Path) -> str:
        quoted = shlex.quote(str(path))
        return quoted if quoted.startswith("'") else "'" + quoted + "'"

    nonce_path, inside_path, sibling_path = (quote_path(path) for path in paths)
    return (
        "#!/bin/sh\n"
        "set +e\n"
        f"nonce_value=$(/bin/cat {nonce_path})\n"
        "read_rc=$?\n"
        "printf 'probe.v1.read_rc=%s\\n' \"$read_rc\"\n"
        "printf 'probe.v1.nonce=%s\\n' \"$nonce_value\"\n"
        f"workspace_error=$( ( printf 'WORKSPACE-WRITE-PROBE\\n' > {inside_path} ) 2>&1 )\n"
        "workspace_rc=$?\n"
        "printf 'probe.v1.workspace_rc=%s\\n' \"$workspace_rc\"\n"
        "printf 'probe.v1.workspace_stderr=%s\\n' \"$workspace_error\"\n"
        f"sibling_error=$( ( printf 'SIBLING-WRITE-PROBE\\n' > {sibling_path} ) 2>&1 )\n"
        "sibling_rc=$?\n"
        "printf 'probe.v1.sibling_rc=%s\\n' \"$sibling_rc\"\n"
        "printf 'probe.v1.sibling_stderr=%s\\n' \"$sibling_error\"\n"
        "overall=0\n"
        "if [ \"$read_rc\" -ne 0 ]; then overall=1; fi\n"
        "if [ \"$workspace_rc\" -eq 0 ]; then overall=1; fi\n"
        "if [ \"$sibling_rc\" -eq 0 ]; then overall=1; fi\n"
        "exit \"$overall\"\n"
    ).encode()


def parse_probe_markers(output: str, nonce: bytes, inside: Path, sibling: Path) -> dict[str, object]:
    """Parse the probe's bounded machine markers from one command result."""
    require(isinstance(output, str), "probe command output is not text")
    try:
        output_bytes = output.encode("utf-8")
    except UnicodeEncodeError as error:
        raise AcceptanceFailure("probe command output is not valid UTF-8 text") from error
    require(len(output_bytes) <= MAX_CONTROL_BYTES, "probe command output exceeds the control bound")
    try:
        expected_nonce = nonce.decode("ascii")
    except UnicodeDecodeError as error:
        raise AcceptanceFailure("probe nonce is not ASCII") from error
    markers: dict[str, str] = {}
    for line in output.splitlines():
        if not line.startswith("probe.v1."):
            continue
        key, separator, value = line.partition("=")
        require(separator == "=" and key in PROBE_MARKERS,
                "probe output contains an unknown marker")
        require(key not in markers, "probe output contains a duplicate marker: " + key)
        require("\n" not in value and "\r" not in value,
                "probe marker contains a line break")
        markers[key] = value
    require(set(markers) == set(PROBE_MARKERS), "probe output markers are incomplete")
    require(markers["probe.v1.read_rc"] == "0", "probe read did not complete with exit code 0")
    require(markers["probe.v1.nonce"] == expected_nonce,
            "probe read marker did not return the exact nonce")

    parsed: dict[str, object] = {name: markers[marker] for marker, name in PROBE_MARKERS.items()
                                 if name != "nonce"}
    parsed["read_rc"] = 0
    for name, path in (("workspace", inside), ("sibling", sibling)):
        rc_name = name + "_rc"
        stderr_name = name + "_stderr"
        value = parsed[rc_name]
        require(isinstance(value, str) and re.fullmatch(r"[0-9]+", value) is not None,
                f"probe {name} exit code is not numeric")
        code = int(value)
        require(0 < code <= 255, f"probe {name} write was not independently denied")
        parsed[rc_name] = code
        diagnostic = parsed[stderr_name]
        require(isinstance(diagnostic, str) and diagnostic,
                f"probe {name} denial diagnostic is empty")
        diagnostic_lower = diagnostic.lower()
        require(str(path) in diagnostic,
                f"probe {name} denial diagnostic names the wrong absolute path")
        require(any(word in diagnostic_lower for word in DENIAL_WORDS),
                f"probe {name} denial diagnostic has no permission failure")
    return parsed


def validate_command_execution_controls(parsed: dict[str, object], probe: Path,
                                        inside: Path, sibling: Path, nonce: bytes) -> dict[str, object]:
    """Require one immutable probe command and actual read/write observations."""
    commands = parsed.get("commands")
    require(isinstance(commands, list) and len(commands) == 1,
            "Codex must produce exactly one completed probe command_execution item")
    incomplete = parsed.get("incomplete_commands", [])
    require(isinstance(incomplete, list) and not incomplete,
            "incomplete command_execution items cannot prove probe controls")
    item = commands[0]
    require(isinstance(item, dict), "probe command_execution item is malformed")
    require(item.get("type") == "command_execution", "probe command_execution item has the wrong type")
    item_id = item.get("id")
    require(isinstance(item_id, str) and item_id, "probe command_execution item has no identity")
    command = item.get("command")
    require(isinstance(command, str) and exact_probe_command(command, probe),
            "command_execution evidence is not the exact immutable /bin/sh probe")
    exit_code = item.get("exit_code")
    require(item.get("status") == "completed" and isinstance(exit_code, int) and
            not isinstance(exit_code, bool) and exit_code == 0,
            "probe command_execution did not complete successfully")
    output = item.get("aggregated_output")
    require(isinstance(output, str), "probe command_execution output is not text")
    markers = parse_probe_markers(output, nonce, inside, sibling)
    return {
        "probe_command_id": item_id,
        "probe_command": command,
        "read_rc": markers["read_rc"],
        "workspace_rc": markers["workspace_rc"],
        "sibling_rc": markers["sibling_rc"],
        "workspace_stderr": markers["workspace_stderr"],
        "sibling_stderr": markers["sibling_stderr"],
    }


def validate_manifest(directory: Path, seal: dict[str, object]) -> None:
    manifest = seal.get("raw_manifest")
    require(isinstance(manifest, list) and 2 <= len(manifest) <= 18,
            "provider.exit raw manifest is absent or has an invalid entry count")
    paths: set[str] = set()
    previous: str | None = None
    for entry in manifest:
        require(isinstance(entry, dict), "raw manifest entry is not an object")
        relative = entry.get("path")
        size = entry.get("size")
        checksum = entry.get("sha256")
        require(isinstance(relative, str) and relative.startswith("raw/") and relative not in paths,
                "raw manifest path is malformed or duplicated")
        basename = relative[4:]
        require(basename in {"stdout", "stderr"} or
                (not basename.startswith("stage.") and
                 re.fullmatch(r"[a-z0-9][a-z0-9._-]{0,127}", basename) is not None),
                "raw manifest path is not a safe task-local basename")
        require(previous is None or previous < relative,
                "raw manifest paths are not strictly sorted")
        require(isinstance(size, int) and not isinstance(size, bool) and 0 <= size <= MAX_PROVIDER_BYTES and
                isinstance(checksum, str) and re.fullmatch(r"[0-9a-f]{64}", checksum) is not None,
                "raw manifest descriptor is malformed")
        path = directory / relative
        require(path.is_file() and not path.is_symlink(), "raw manifest file is absent")
        require(path.stat().st_size == size and digest(path) == checksum,
                "raw manifest digest mismatch")
        paths.add(relative)
        previous = relative
    require({"raw/stdout", "raw/stderr", "raw/" + OUTPUT_NAME} <= paths,
            "Codex raw manifest omitted required evidence")


def validate_record_usage(outcome: dict[str, object], parsed: dict[str, object], name: str) -> None:
    """Check cumulative event counters without deriving a task delta."""
    event_usage = parsed.get("usage")
    recorded = outcome.get("usage")
    if event_usage is None:
        require(recorded in (None, []), f"{name} invented usage without provider counters")
        return
    require(isinstance(recorded, list) and len(recorded) == 1 and isinstance(recorded[0], dict),
            f"{name} omitted cumulative provider usage")
    value = recorded[0]
    require(value.get("scope") == "conversation-cumulative" and
            value.get("source") == "provider-event" and
            value.get("reliability") == "reported",
            f"{name} usage scope or reliability is not provider-event cumulative")
    mapping = {
        "input_tokens": "input_tokens",
        "cached_input_tokens": "cache_read_tokens",
        "cache_write_input_tokens": "cache_creation_tokens",
        "output_tokens": "output_tokens",
        "reasoning_output_tokens": "thinking_tokens",
    }
    for event_name, record_name in mapping.items():
        expected = event_usage[event_name]
        if expected is None:
            require(record_name not in value or value.get(record_name) is None,
                    f"{name} synthesized missing usage counter {record_name}")
        else:
            require(value.get(record_name) == expected,
                    f"{name} usage counter {record_name} differs from turn.completed")
    require("total_tokens" not in value, f"{name} synthesized total_tokens")


def provider_record_snapshot(directory: Path) -> dict[str, dict[str, object]]:
    return snapshot(directory, ignored_prefixes=(".ack",))


class CodexAcceptance:
    def __init__(self, args: argparse.Namespace):
        self.args = args
        self.environment = os.environ.copy()
        self.tools = Path(args.tools).resolve(strict=True)
        require(self.tools.is_dir() and not self.tools.is_symlink(), "--tools must be a directory")
        self.delegate = resolve_executable(args.delegate, self.tools / "delegate", "delegate")
        self.runner = resolve_executable(args.runner, self.tools / "delegate-run", "delegate-run")
        self.pueue = resolve_executable(args.pueue, Path("/opt/homebrew/bin/pueue"), "pueue")
        self.pueued = resolve_executable(args.pueued, Path("/opt/homebrew/bin/pueued"), "pueued")
        self.codex = resolve_executable(args.codex, Path(shutil.which("codex") or "/opt/homebrew/bin/codex"), "codex")
        self.provider_version: str | None = None
        self.provider_sha256 = digest(self.codex)
        self.profile_revision: str | None = None
        self.inspection_binding: dict[str, object] | None = None
        require_discovery("codex", self.codex)
        require_discovery("pueue", self.pueue)
        require_discovery("pueued", self.pueued)
        self.output = clean_absolute(args.output, "evidence output")
        reject_tmp(self.output, "evidence output")
        reject_runtime_roots(self.output, "evidence output", self.environment)
        require(not self.output.exists(), f"evidence output already exists: {self.output}")
        self.output.mkdir(mode=0o700, parents=True, exist_ok=False)
        os.chmod(self.output, 0o700)

        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + secrets.token_hex(6)
        self.state_parent = ensure_private_directory(DEFAULT_STATE_PARENT / ("codex-" + stamp),
                                                      "acceptance state parent", create=True)
        self.state = ensure_private_directory(self.state_parent / "state", "acceptance state", create=True)
        self.workspace = ensure_private_directory(DEFAULT_WORKSPACE_PARENT / ("codex-" + stamp),
                                                   "acceptance workspace", create=True)
        self.sibling = ensure_private_directory(DEFAULT_WORKSPACE_PARENT / ("codex-" + stamp + "-sibling"),
                                                 "acceptance sibling", create=True)
        self.briefs = ensure_private_directory(self.state_parent / "briefs", "acceptance briefs", create=True)
        self.probe = self.state_parent / "probe.sh"
        reject_runtime_roots(self.probe, "acceptance probe", self.environment)
        for path, label in ((self.state, "acceptance state"), (self.workspace, "acceptance workspace"),
                            (self.sibling, "acceptance sibling"), (self.output, "evidence output")):
            reject_runtime_roots(path, label, self.environment)
            require(path.resolve(strict=True) == path, f"{label} must be canonical")
        require(not path_is_within(self.output, self.state_parent) and
                not path_is_within(self.state_parent, self.output) and
                not path_is_within(self.output, self.workspace) and
                not path_is_within(self.workspace, self.output) and
                not path_is_within(self.output, self.sibling) and
                not path_is_within(self.sibling, self.output),
                "evidence output overlaps acceptance state or workspace")

        self.nonce_file = self.workspace / "nonce.txt"
        self.inside_sentinel = self.workspace / "workspace-sentinel.txt"
        self.sibling_sentinel = self.sibling / "sibling-sentinel.txt"
        self.nonce = secrets.token_hex(24).encode("ascii")
        self.nonce_bytes = self.nonce + b"\n"
        self.inside_bytes = b"WORKSPACE-SENTINEL-ORIGINAL\n"
        self.sibling_bytes = b"SIBLING-SENTINEL-ORIGINAL\n"
        write_bytes(self.nonce_file, self.nonce_bytes)
        write_bytes(self.inside_sentinel, self.inside_bytes)
        write_bytes(self.sibling_sentinel, self.sibling_bytes)
        self.probe_source = probe_script_bytes(self.nonce_file, self.inside_sentinel,
                                                self.sibling_sentinel)
        require(self.nonce not in self.probe_source, "probe script contains the nonce value")
        self.probe_size = len(self.probe_source)
        self.probe_sha256 = sha(self.probe_source)
        write_bytes(self.probe, self.probe_source)
        os.chmod(self.probe, 0o400)
        self.require_probe_unchanged("probe creation")
        self.workspace_before: dict[str, dict[str, object]] | None = None
        self.sibling_before: dict[str, dict[str, object]] | None = None

        git = resolve_executable(shutil.which("git"), Path("/usr/bin/git"), "git")
        self.processes = Processes(self.output / "processes", self.environment)
        self.ops = NativeTaskOps(self.processes, self.delegate, self.runner, self.pueue,
                                 self.state_parent, self.state, self.output,
                                 watch_seconds=WATCH_SECONDS)
        self.git = git
        self.pueue_base: Path | None = None
        self.pueue_config: Path | None = None
        self.daemon = None
        self.closed = False
        self.root_id: str | None = None
        self.tasks = self.ops.tasks
        self.labels = self.ops.labels
        self.numbers = self.ops.numbers
        self.records: dict[str, dict[str, object]] = {}
        self.dispatch_attempts = self.ops.dispatch_attempts
        self.queue_before_replay: dict[str, object] | None = None

    def require_probe_unchanged(self, phase: str) -> None:
        """Bind the probe as one regular, immutable, exact-byte fixture."""
        require(self.probe.parent == self.state_parent and
                not path_is_within(self.probe, self.state) and
                not path_is_within(self.state, self.probe) and
                not path_is_within(self.probe, self.workspace) and
                not path_is_within(self.workspace, self.probe) and
                not path_is_within(self.probe, self.sibling) and
                not path_is_within(self.sibling, self.probe),
                f"{phase}: probe is not separate from state/workspace")
        require(self.probe.is_file() and not self.probe.is_symlink(),
                f"{phase}: probe is not a regular non-symlink file")
        require(self.probe.resolve(strict=True) == self.probe,
                f"{phase}: probe is not canonical")
        metadata = self.probe.stat()
        require(metadata.st_mode & 0o7777 == 0o400,
                f"{phase}: probe mode is not exactly 0400")
        require(0 <= metadata.st_size <= MAX_CONTROL_BYTES and metadata.st_size == self.probe_size,
                f"{phase}: probe size changed or exceeds the control bound")
        data = self.probe.read_bytes()
        require(len(data) == self.probe_size and sha(data) == self.probe_sha256,
                f"{phase}: probe bytes or digest changed")
        require(self.nonce not in data, f"{phase}: probe contains the nonce value")

    def direct(self, name: str, argv: list[object], expected: int | set[int] = 0,
               timeout: float = 30):
        return self.ops.direct(name, argv, expected=expected, timeout=timeout)

    def client(self, name: str, operation: list[str], expected: int | set[int] = 0,
               timeout: float = 30):
        require(self.pueue_config is not None, "pueue is not configured")
        self.ops.bind_supervisor(self.pueue_config, self.daemon)
        return self.ops.client(name, operation, expected=expected, timeout=timeout)

    def setup(self) -> None:
        self.require_probe_unchanged("before setup")
        PUEUE_PARENT.mkdir(mode=0o700, parents=True, exist_ok=True)
        if sys.platform != "darwin":
            ensure_private_directory(PUEUE_PARENT, "private supervisor parent")
        pueue_base = PUEUE_PARENT / ("delegation-layer-codex-" + secrets.token_hex(8))
        self.pueue_base = ensure_private_directory(pueue_base, "private pueue base", create=True)
        ensure_private_directory(self.pueue_base / "state", "private pueue state", create=True)
        ensure_private_directory(self.pueue_base / "run", "private pueue runtime", create=True)
        aliases = self.pueue_base / "aliases.yml"
        write_bytes(aliases, b"{}\n")
        self.pueue_config = self.pueue_base / "pueue.yml"
        write_json(self.pueue_config, config_for(self.pueue_base))
        self.inspection_binding = expected_codex_inspection_binding(
            self.pueue, self.pueue_config, self.pueue_base, digest(self.pueue_config),
            self.runner, self.workspace, self.environment, self.codex, self.provider_sha256)

        pueue_version = self.direct("pueue-version", [self.pueue, "--version"], timeout=15)
        pueued_version = self.direct("pueued-version", [self.pueued, "-c", self.pueue_config, "--version"], timeout=15)
        require((pueue_version.directory / "stdout").read_text().strip() == "pueue " + PUEUE_VERSION,
                "unexpected pueue version")
        require((pueued_version.directory / "stdout").read_text().strip() == "pueued " + PUEUE_VERSION,
                "unexpected pueued version")
        codex_version = self.direct("codex-version", [self.codex, "--version"], timeout=15)
        observed = (codex_version.directory / "stdout").read_text().strip()
        require(observed, "Codex returned an empty version")
        self.provider_version = observed

        self.daemon = self.processes.start("private-daemon", [self.pueued, "-c", self.pueue_config], self.state_parent)
        self.ops.bind_supervisor(self.pueue_config, self.daemon)
        for _ in range(200):
            require(self.daemon.poll() is None, "private pueued exited before readiness")
            process = self.client("ready", ["status", "--json"], expected={0, 1}, timeout=15)
            if int(process.result["exit_code"]) == 0:
                status = status_jobs(process)
                require(status["tasks"] == {}, "new private pueue queue was not empty")
                break
            time.sleep(0.1)
        else:
            raise AcceptanceFailure("private pueue readiness was not established")

        write_json(self.output / "binding.json", {
            "provider": PROVIDER,
            "mode": MODE,
            "approval": APPROVAL,
            "codex": str(self.codex),
            "codex_version": self.provider_version,
            "codex_sha256": self.provider_sha256,
            "delegate": str(self.delegate),
            "delegate_sha256": digest(self.delegate),
            "runner": str(self.runner),
            "runner_sha256": digest(self.runner),
            "pueue": str(self.pueue),
            "pueue_sha256": digest(self.pueue),
            "pueued": str(self.pueued),
            "pueued_sha256": digest(self.pueued),
            "pueue_version": PUEUE_VERSION,
            "pueued_version": PUEUE_VERSION,
            "pueue_base": str(self.pueue_base),
            "pueue_config": str(self.pueue_config),
            "config_sha256": digest(self.pueue_config),
            "state": str(self.state),
            "workspace": str(self.workspace),
            "sibling": str(self.sibling),
            "probe": str(self.probe),
            "probe_sha256": self.probe_sha256,
            "probe_bytes": self.probe_size,
            "probe_mode": "0400",
            "nonce_sha256": sha(self.nonce_bytes),
            "environment_keys": sorted(self.environment),
            "environment_values_in_record": False,
            "acceptance_status": PRELAUNCH_STATUS,
            "planned_native_ai_turns": PLANNED_NATIVE_AI_TURNS,
            "driver_sha256": digest(__file__),
        })

        git_result = self.processes.run("git-init", [self.git, "init", "--quiet", "--initial-branch=main", self.workspace],
                                       self.workspace.parent, timeout=15)
        require(git_result.result["exit_code"] == 0 and (self.workspace / ".git").is_dir(),
                "scratch workspace is not a git repository")
        self.workspace_before = snapshot(self.workspace, ignored_prefixes=())
        self.sibling_before = snapshot(self.sibling, ignored_prefixes=())

    def fresh_brief(self) -> Path:
        path = self.briefs / "fresh.md"
        probe = shlex.quote(str(self.probe))
        text = (
            "Use command_execution only, in the current workspace. Do not use file editing, agents, networking, "
            "or an approval host. Run exactly one command_execution with `/bin/sh " + probe + "`. Do not run "
            "cat, printf, another shell command, or any other tool. Return the probe's actual output and exit "
            "status, followed by a concise summary. Do not claim read or write results from prose."
        )
        write_bytes(path, text.encode())
        require(self.nonce.decode() not in path.read_text(), "fresh brief contains the nonce")
        return path

    def resume_brief(self) -> Path:
        path = self.briefs / "resume.md"
        text = ("Continue the exact previous Codex conversation. Do not use any tool and do not read any file. "
                "Return only the random nonce you reported in the previous turn, with no surrounding prose.")
        write_bytes(path, text.encode())
        require(self.nonce.decode() not in path.read_text(), "resume brief contains the nonce")
        return path

    def dispatch_arguments(self, task: str, brief: Path, predecessor: str | None = None) -> list[object]:
        require(self.pueue_config is not None, "pueue is not configured")
        args: list[object] = [self.delegate, "--root", self.state, "--pueue-config", self.pueue_config,
                              "--runner", self.runner, "dispatch", "--provider", PROVIDER,
                              "--brief", brief, "--cwd", self.workspace, "--id", task,
                              "--permission", MODE, "--budget", TASK_BUDGET, "--json"]
        if predecessor is not None:
            args.extend(["--resume-task", predecessor])
        no_prompt_argv(args, [brief])
        return args

    def dispatch(self, name: str, task: str, brief: Path, predecessor: str | None = None) -> dict[str, object]:
        self.ops.root_id = self.root_id
        require(self.inspection_binding is not None,
                "expected Codex inspection binding is unavailable")
        self.ops.expect_inspection(task, self.inspection_binding)
        response = self.ops.dispatch(name, task, self.dispatch_arguments(task, brief, predecessor))
        root = response.get("root_id")
        require(is_task_id(root), f"{name} omitted a valid root identity")
        self.root_id = root
        self.ops.root_id = root
        require(self.root_id == root, f"{name} changed root identity")
        return response

    def wait_task(self, name: str, task: str) -> dict[str, object]:
        require(self.root_id is not None, "root identity is unavailable")
        self.ops.root_id = self.root_id
        return self.ops.wait_task(name, task)

    def collect(self, name: str, task: str) -> dict[str, object]:
        self.ops.root_id = self.root_id
        return self.ops.collect(name, task)

    def read_record(self, task: str, name: str) -> dict[str, object]:
        value = read_json(self.state / "tasks" / task / name)
        require(isinstance(value, dict), f"{task}/{name} must be an object")
        return value

    def validate_task(self, name: str, task: str, expected_answer: bytes,
                      predecessor: str | None = None) -> dict[str, object]:
        directory = self.state / "tasks" / task
        require(directory.is_dir() and not directory.is_symlink(), f"{name} task directory is absent")
        for record in ("brief.md", "task.json", "meta.json", "submit.json", "provider.start",
                       "provider.started.json", "provider.exit", "provider.ref.json", "outcome.json"):
            require((directory / record).is_file(), f"{name} missing evidence {record}")
        request = self.read_record(task, "task.json")
        require(request.get("root_id") == self.root_id and request.get("task_id") == task and
                request.get("provider") == PROVIDER and request.get("mode") == MODE and
                request.get("canonical_cwd") == str(self.workspace),
                f"{name} task identity or workspace binding mismatch")
        requested = request.get("requested_config")
        require(isinstance(requested, dict) and requested.get("permission") == MODE and
                requested.get("budget") == CANONICAL_TASK_BUDGET and
                request.get("budget_nanos") == 120_000_000_000,
                f"{name} request budget or permission mismatch")
        brief = (directory / "brief.md").read_bytes()
        require(request.get("brief_length") == len(brief) and request.get("brief_sha256") == sha(brief),
                f"{name} persisted brief digest mismatch")
        if predecessor is not None:
            prior = request.get("prior_session")
            require(isinstance(prior, dict) and prior.get("provider") == PROVIDER and
                    prior.get("predecessor_task_id") == predecessor and
                    is_thread_id(prior.get("conversation_id")),
                    f"{name} lacks the exact predecessor session reference")
        else:
            require(request.get("prior_session") is None, f"{name} unexpectedly has a predecessor")

        meta = self.read_record(task, "meta.json")
        require(meta.get("root_id") == self.root_id and meta.get("task_id") == task and
                meta.get("provider_executable") == str(self.codex) and
                meta.get("provider_version") == self.provider_version and
                meta.get("containment") == MODE and meta.get("approval") == APPROVAL,
                f"{name} meta provider or policy binding mismatch")
        effective = meta.get("effective_config")
        require(isinstance(effective, dict) and effective.get("containment") == MODE and
                effective.get("approval") == APPROVAL and isinstance(effective.get("digest"), str),
                f"{name} effective read-only policy is absent")
        policy = effective.get("policy")
        require(isinstance(policy, dict) and policy.get("workspace") == str(self.workspace) and
                policy.get("runtime_sha256") == self.provider_sha256,
                f"{name} persisted Codex runtime policy is incomplete")
        profile_revision = policy.get("profile_revision")
        require(profile_revision == NATIVE_PROFILE_REVISION,
                f"{name} native effective policy revision is absent or stale")
        if self.profile_revision is None:
            self.profile_revision = profile_revision
        require(profile_revision == self.profile_revision, f"{name} effective policy revision drifted")
        require(not policy.get("sources"), f"{name} native policy unexpectedly inventories config sources")
        roots = policy.get("writable_roots")
        require(isinstance(roots, list) and all(isinstance(root, str) for root in roots),
                f"{name} writable runtime roots are absent")
        codex_home = self.environment.get("CODEX_HOME", str(Path.home() / ".codex"))
        require(str(Path(codex_home).resolve()) in {str(Path(root).resolve()) for root in roots},
                f"{name} CODEX_HOME was not preserved in runtime policy")
        predicate = meta.get("predicate")
        require(predicate == {"adapter": PROVIDER, "mode": MODE, "version": PREDICATE_VERSION,
                              "sha256": PREDICATE_SHA256}, f"{name} predicate binding mismatch")
        require(meta.get("output_writer_contract") == OUTPUT_WRITER_CONTRACT,
                f"{name} output-writer contract mismatch")
        artifacts = meta.get("output_artifacts")
        require(isinstance(artifacts, list) and len(artifacts) == 1 and isinstance(artifacts[0], dict) and
                artifacts[0].get("name") == OUTPUT_NAME and
                artifacts[0].get("argument_index") == OUTPUT_ARGUMENT_INDEX,
                f"{name} Codex output staging declaration is absent")

        submit = self.read_record(task, "submit.json")
        require(submit.get("root_id") == self.root_id and submit.get("task_id") == task and
                submit.get("label") == self.labels[name] and
                submit.get("spec_sha256") == meta.get("spec_sha256") and
                submit.get("meta_sha256") == digest(directory / "meta.json"),
                f"{name} submit binding mismatch")
        supervisor = submit.get("supervisor")
        require(isinstance(supervisor, dict) and supervisor.get("config_path") == str(self.pueue_config) and
                supervisor.get("observed_version") == "pueue " + PUEUE_VERSION and
                supervisor.get("client_executable") == str(self.pueue),
                f"{name} submit supervisor binding mismatch")

        start = self.read_record(task, "provider.start")
        require(start.get("root_id") == self.root_id and start.get("task_id") == task and
                start.get("spec_sha256") == meta.get("spec_sha256") and
                start.get("meta_sha256") == digest(directory / "meta.json") and
                isinstance(start.get("budget_nanos"), int) and start["budget_nanos"] > 0,
                f"{name} provider.start binding mismatch")
        started = self.read_record(task, "provider.started.json")
        require(started.get("root_id") == self.root_id and started.get("task_id") == task and
                started.get("spec_sha256") == meta.get("spec_sha256") and
                started.get("meta_sha256") == digest(directory / "meta.json") and
                isinstance(started.get("diagnostic_nanos"), int) and started["diagnostic_nanos"] >= 0,
                f"{name} provider.started evidence mismatch")
        seal = self.read_record(task, "provider.exit")
        require(seal.get("root_id") == self.root_id and seal.get("task_id") == task and
                seal.get("spec_sha256") == meta.get("spec_sha256") and
                seal.get("meta_sha256") == digest(directory / "meta.json") and
                seal.get("invocation_state") == "started" and seal.get("exit_code") == 0 and
                seal.get("error") == "" and seal.get("predicate") == predicate,
                f"{name} provider seal mismatch")
        validate_manifest(directory, seal)

        raw_stdout = (directory / "raw" / "stdout").read_bytes()
        raw_stderr = (directory / "raw" / "stderr").read_bytes()
        require(len(raw_stdout) <= MAX_PROVIDER_BYTES and len(raw_stderr) <= MAX_PROVIDER_BYTES,
                f"{name} raw stream exceeds acceptance bound")
        parsed = parse_codex_events(raw_stdout)
        output_artifact = directory / "raw" / OUTPUT_NAME
        require(output_artifact.is_file() and not output_artifact.is_symlink(),
                f"{name} Codex last-message artifact is absent")
        output_bytes = output_artifact.read_bytes()
        require(output_bytes == parsed["final_message"], f"{name} output artifact disagrees with final agent message")
        require(output_bytes == expected_answer, f"{name} final answer bytes differ from acceptance expectation")
        resume_controls: dict[str, object] | None = None
        if predecessor is not None:
            resume_controls = validate_tool_free_resume(parsed, expected_answer)

        reference = self.read_record(task, "provider.ref.json")
        require(reference.get("root_id") == self.root_id and reference.get("task_id") == task and
                reference.get("spec_sha256") == meta.get("spec_sha256") and
                reference.get("meta_sha256") == digest(directory / "meta.json") and
                reference.get("provider") == PROVIDER and reference.get("conversation_id") == parsed["thread_id"],
                f"{name} provider thread identity mismatch")
        require(is_thread_id(reference.get("conversation_id")), f"{name} provider thread ID is invalid")
        outcome = self.read_record(task, "outcome.json")
        require(outcome.get("root_id") == self.root_id and outcome.get("task_id") == task and
                outcome.get("spec_sha256") == meta.get("spec_sha256") and
                outcome.get("meta_sha256") == digest(directory / "meta.json") and
                outcome.get("verdict") == "committed" and outcome.get("predicate") == predicate and
                outcome.get("evidence_sha256") == seal.get("manifest_sha256"),
                f"{name} outcome authority mismatch")
        validate_record_usage(outcome, parsed, name)
        payload = outcome.get("payload")
        require(isinstance(payload, dict) and payload.get("basename") == "result.txt" and
                payload.get("length") == len(expected_answer) and
                payload.get("sha256") == sha(expected_answer) and
                (directory / "result.txt").read_bytes() == expected_answer,
                f"{name} committed payload mismatch")
        return {"directory": directory, "request": request, "meta": meta, "seal": seal,
                "reference": reference, "outcome": outcome, "payload": deepcopy(payload),
                "parsed": parsed, "stderr_sha256": sha(raw_stderr),
                "resume_controls": resume_controls,
                "snapshot": provider_record_snapshot(directory)}

    def run_fresh(self) -> dict[str, object]:
        self.require_probe_unchanged("before fresh dispatch")
        brief = self.fresh_brief()
        task = task_id()
        response = self.dispatch("fresh", task, brief)
        self.wait_task("fresh", task)
        collected = self.collect("fresh-collect", task)
        outcome, data = verify_collected_outcome(collected, self.state / "tasks" / task, "committed")
        require(data and self.nonce in data, "fresh final answer did not report the nonce")
        self.require_probe_unchanged("after fresh collection")
        parsed = parse_codex_events((self.state / "tasks" / task / "raw/stdout").read_bytes())
        controls = validate_command_execution_controls(parsed, self.probe, self.inside_sentinel,
                                                        self.sibling_sentinel, self.nonce)
        require(self.workspace_before == snapshot(self.workspace, ignored_prefixes=()),
                "Codex changed the scratch workspace or workspace sentinel")
        require(self.sibling_before == snapshot(self.sibling, ignored_prefixes=()),
                "Codex changed the sibling sentinel")
        record = self.validate_task("fresh", task, data)
        record.update({"dispatch": response, "collect": collected, "controls": controls})
        self.records["fresh"] = record
        write_json(self.output / "fresh-control.json", {
            "task_id": task,
            "conversation_id": record["reference"]["conversation_id"],
            "nonce_sha256": sha(self.nonce),
            "probe_command_id": controls["probe_command_id"],
            "probe_command": controls["probe_command"],
            "probe_sha256": self.probe_sha256,
            "probe_bytes": self.probe_size,
            "read_rc": controls["read_rc"],
            "workspace_write_rc": controls["workspace_rc"],
            "sibling_write_rc": controls["sibling_rc"],
            "workspace_denial": controls["workspace_stderr"],
            "sibling_denial": controls["sibling_stderr"],
            "probe_unchanged": True,
            "workspace_unchanged": True,
            "sibling_unchanged": True,
            "provider_exit_manifest_sha256": record["seal"]["manifest_sha256"],
            "outcome_sha256": digest(record["directory"] / "outcome.json"),
            "outcome": outcome,
        })
        return record

    def run_resume(self, predecessor: dict[str, object]) -> dict[str, object]:
        self.require_probe_unchanged("before resume dispatch")
        predecessor_task = self.tasks["fresh"]
        original_snapshot = predecessor["snapshot"]
        require(isinstance(original_snapshot, dict), "fresh task snapshot is unavailable")
        self.nonce_file.unlink(missing_ok=False)
        require(not self.nonce_file.exists(), "resume nonce file still exists")
        workspace_before_resume = snapshot(self.workspace, ignored_prefixes=())
        brief = self.resume_brief()
        task = task_id()
        require(task != predecessor_task, "resume must be admitted as a new task")
        response = self.dispatch("resume", task, brief, predecessor_task)
        self.wait_task("resume", task)
        collected = self.collect("resume-collect", task)
        outcome, data = verify_collected_outcome(collected, self.state / "tasks" / task, "committed")
        self.require_probe_unchanged("after resume collection")
        require(data == self.nonce, "resume did not recover exactly the prior nonce")
        record = self.validate_task("resume", task, data, predecessor_task)
        require(record["reference"]["conversation_id"] == predecessor["reference"]["conversation_id"],
                "resume changed the Codex conversation identity")
        require(original_snapshot == provider_record_snapshot(predecessor["directory"]),
                "resume changed the immutable predecessor task records")
        require(workspace_before_resume == snapshot(self.workspace, ignored_prefixes=()),
                "resume changed the workspace or workspace sentinel")
        require(self.sibling_before == snapshot(self.sibling, ignored_prefixes=()),
                "resume changed the sibling sentinel")
        resume_controls = record.get("resume_controls")
        require(isinstance(resume_controls, dict) and resume_controls.get("tool_free") is True,
                "resume tool-free oracle result is absent")
        record.update({"dispatch": response, "collect": collected})
        self.records["resume"] = record
        write_json(self.output / "resume-control.json", {
            "task_id": task,
            "predecessor_task_id": predecessor_task,
            "conversation_id": record["reference"]["conversation_id"],
            "nonce_sha256": sha(data),
            "resume_brief_contains_nonce": False,
            "tool_free": True,
            "resume_item_count": resume_controls["item_count"],
            "resume_item_types": resume_controls["item_types"],
            "original_records_unchanged": True,
            "provider_exit_manifest_sha256": record["seal"]["manifest_sha256"],
            "outcome_sha256": digest(record["directory"] / "outcome.json"),
            "outcome": outcome,
        })
        return record

    def queue_status(self, name: str) -> dict[str, object]:
        self.ops.bind_supervisor(self.pueue_config, self.daemon)
        return self.ops.queue_status(name)

    def replay(self) -> None:
        self.require_probe_unchanged("before replay")
        self.ops.root_id = self.root_id
        self.ops.replay(self.records, names=("fresh", "resume"))
        self.queue_before_replay = self.ops.queue_before_replay
        self.require_probe_unchanged("after replay")

    def failure_queue_finished(self, status: dict[str, object]) -> bool:
        return failure_queue_finished(status, self.dispatch_attempts, self.tasks,
                                      self.labels, self.runner, self.state,
                                      self.numbers)

    def shutdown(self) -> None:
        self.ops.root_id = self.root_id
        self.ops.bind_supervisor(self.pueue_config, self.daemon)
        self.ops.shutdown()
        self.closed = self.ops.closed

    def safe_failure_shutdown(self) -> bool:
        """Shutdown only after an all-Done observation; never signals unknown work."""
        self.ops.root_id = self.root_id
        if self.pueue_config is not None:
            self.ops.bind_supervisor(self.pueue_config, self.daemon)
        result = self.ops.safe_failure_shutdown()
        self.closed = self.ops.closed
        return result

    def run(self) -> None:
        try:
            self.setup()
            fresh = self.run_fresh()
            self.run_resume(fresh)
            self.replay()
            self.shutdown()
            write_json(self.output / "success.json", {
                "status": ACCEPTANCE_STATUS,
                "provider": PROVIDER,
                "native_ai_turns": len(self.tasks),
                "planned_native_ai_turns": PLANNED_NATIVE_AI_TURNS,
                "tasks": {name: {"task_id": self.tasks[name],
                                  "conversation_id": self.records[name]["reference"]["conversation_id"],
                                  "outcome_sha256": digest(self.records[name]["directory"] / "outcome.json"),
                                  "evidence_sha256": self.records[name]["outcome"]["evidence_sha256"]}
                           for name in ("fresh", "resume")},
                "replay_launches": 0,
                "natural_daemon_shutdown": self.closed,
                "signals_sent": 0,
            })
            print("PASS Codex acceptance", flush=True)
        except BaseException as error:
            naturally_shutdown = self.safe_failure_shutdown()
            write_json(self.output / "failure.json", {
                "status": "BLOCKED" if isinstance(error, BlockedFailure) else "failed",
                "error": str(error),
                "traceback": traceback.format_exc(),
                "natural_daemon_shutdown": naturally_shutdown or self.closed,
                "signals_sent": 0,
                "unknown_termination_preserved": True,
                "state": str(self.state),
                "workspace": str(self.workspace),
                "tasks": dict(self.tasks),
                "dispatch_attempts": sorted(self.dispatch_attempts),
                "daemon_pid": None if self.daemon is None else self.daemon.pid,
            })
            raise


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tools", required=True, help="directory containing shipped delegate binaries")
    parser.add_argument("--pueue", required=True)
    parser.add_argument("--pueued", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--codex", default=None, help="installed Codex executable (defaults to PATH discovery)")
    parser.add_argument("--delegate", default=None, help="delegate executable override for acceptance composition")
    parser.add_argument("--runner", default=None, help="delegate-run executable override for acceptance composition")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        CodexAcceptance(args).run()
    except BlockedFailure as error:
        print("BLOCKED: " + str(error), file=sys.stderr, flush=True)
        return 2
    except AcceptanceFailure as error:
        print("FAIL: " + str(error), file=sys.stderr, flush=True)
        return 1
    except BaseException as error:
        print("FAIL: " + str(error), file=sys.stderr, flush=True)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
