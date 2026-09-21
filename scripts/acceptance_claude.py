#!/usr/bin/env python3
"""Run the bounded Claude ``print`` acceptance gate.

The driver deliberately keeps Claude's stream/result and tool assertions
local.  Admission, supervision, capture, sealing, publication, continuation,
and replay use the shipped delegate and the small shared ``NativeTaskOps``
observer; this file never starts Claude directly.
"""

from __future__ import annotations

import argparse
from collections.abc import Mapping
from copy import deepcopy
from decimal import Decimal, InvalidOperation
import json
import os
from pathlib import Path
import pwd
import re
import secrets
import shutil
import sys
import tempfile
import time
import traceback
from datetime import datetime, timezone
import uuid

from acceptance_provider_common import (
    AcceptanceFailure,
    NativeTaskOps,
    canonical_go_json,
    clean_absolute,
    ensure_private_directory,
    no_prompt_argv,
    path_is_within,
    reject_tmp,
    read_outcome_payload,
    require,
    snapshot,
    supervisor_binding,
    unique_object,
    verify_collected_outcome,
    write_bytes,
    write_json,
)
from acceptance_supervisor_common import Processes, config_for, digest, read_json, sha


PROVIDER = "claude:print"
MODE = "read-only"
APPROVAL = "plan"
# The predicate revision describes the current native output contract. It is
# independent of the installed Claude CLI release, which is admitted through
# the runtime capability probe.
PREDICATE_VERSION = "native-permissions-v1"
PREDICATE_SHA256 = "8049da50895cfdbdacf9f81678c1a2d4c1f15137e2b89b6a9255d6d1e3873642"
NATIVE_PROFILE_REVISION = "native-permissions-v1"
TASK_BUDGET = "120s"
CANONICAL_TASK_BUDGET = "2m0s"
WATCH_SECONDS = 150
PLANNED_NATIVE_AI_TURNS = 2
ACCEPTANCE_STATUS = "acceptance-passed"
PRELAUNCH_STATUS = "planned"
PUEUE_VERSION = "4.0.4"
MAX_CONTROL_BYTES = 1 << 20
MAX_PROVIDER_BYTES = 8 << 20
CLAUDE_XDG_DIRECTORY = "claude"
SCRIPT_DIR = Path(__file__).resolve().parent
DEFAULT_STATE_PARENT = Path.home() / "Library" / "Application Support" / "delegation-layer-acceptance"


def pueue_parent_for_platform(platform: str, home: Path) -> Path:
    """Keep the live supervisor/workspace parent outside provider temp roots."""
    return Path("/Users/Shared") if platform == "darwin" else Path(home) / ".dl-acceptance"


PUEUE_PARENT = pueue_parent_for_platform(sys.platform, Path.home())
RUNTIME_INSPECTION_REVISION = "runtime-capability-v1"
RUNTIME_HELP_ARGS: list[str] | None = None
RUNTIME_REQUIRED_FLAGS = (
    "--print", "--input-format", "--output-format", "--verbose", "--permission-mode",
    "--permission-prompts", "--resume", "--session-id",
)
NATIVE_ENVIRONMENT_KEYS = (
    "HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE",
    "TZ", "TMPDIR", "TMP", "TEMP", "__CF_USER_TEXT_ENCODING",
)
NATIVE_DISCOVERY_ENVIRONMENT_KEYS = (
    "XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME", "XDG_DATA_DIRS",
    "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS",
    "CLAUDE_CONFIG_DIR", "ANTHROPIC_CONFIG_DIR",
)
PATH_VALIDATION_ENVIRONMENT_KEYS = frozenset({
    "HOME", "TMPDIR", "TMP", "TEMP", *NATIVE_DISCOVERY_ENVIRONMENT_KEYS,
})
UUID_PATTERN = re.compile(
    r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")
TASK_PATTERN = re.compile(r"^[0-9a-f]{32}$")
ROOT_PATTERN = re.compile(r"^[0-9a-f]{32}$")
KNOWN_CONTENT_TYPES = frozenset({"text", "thinking", "redacted_thinking", "tool_use", "tool_result"})
ERROR_SUBTYPES = frozenset({
    "error_max_turns", "error_max_budget_usd", "error_during_execution",
    "error_max_structured_output_retries",
})


class BlockedFailure(AcceptanceFailure):
    """A required native prerequisite is unavailable on this host."""


def acceptance_temp_parent() -> Path:
    """Create the platform-specific mkdtemp parent before allocating children."""
    parent = PUEUE_PARENT
    try:
        if parent.exists() and parent.is_symlink():
            raise AcceptanceFailure("acceptance temporary parent must not be a symlink: " + str(parent))
        parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        if sys.platform != "darwin":
            os.chmod(parent, 0o700)
    except OSError as error:
        raise BlockedFailure(f"cannot create acceptance temporary parent: {parent}") from error
    require(parent.is_dir() and not parent.is_symlink(),
            f"acceptance temporary parent is not a directory: {parent}")
    if sys.platform != "darwin":
        require(parent.stat().st_mode & 0o077 == 0,
                f"acceptance temporary parent is group/world accessible: {parent}")
    return parent.resolve()


def private_mkdtemp(prefix: str, label: str) -> Path:
    """Allocate one private directory below the platform-specific parent."""
    parent = acceptance_temp_parent()
    try:
        path = Path(tempfile.mkdtemp(prefix=prefix, dir=str(parent)))
    except OSError as error:
        raise BlockedFailure(f"cannot create {label}") from error
    return ensure_private_directory(path, label)


def path_validation_environment(inherited: Mapping[str, str]) -> dict[str, str]:
    """Read only path-related variables needed for harness-root validation."""
    return {key: inherited[key] for key in PATH_VALIDATION_ENVIRONMENT_KEYS if key in inherited}


def isolated_acceptance_environment(
        inherited: Mapping[str, str], home: Path, temporary: Path) -> dict[str, str]:
    """Build a fully task-owned Claude environment for hermetic fixtures."""
    values = {key: inherited[key] for key in NATIVE_ENVIRONMENT_KEYS if key in inherited}
    values["HOME"] = str(Path(home).resolve())
    for key in ("TMPDIR", "TMP", "TEMP"):
        values[key] = str(Path(temporary).resolve())
    return values


def native_account_acceptance_environment(
        inherited: Mapping[str, str], home: Path, temporary: Path) -> dict[str, str]:
    """Use the signed-in native account while relocating mutable temp state.

    Claude's macOS Keychain lookup is tied to the host login context. The
    acceptance run therefore keeps the real HOME/Keychain account, while its
    temporary, cache, and log paths remain task-owned. No credential bytes are
    read by this driver or copied into the task roots.
    """
    keys = NATIVE_ENVIRONMENT_KEYS + NATIVE_DISCOVERY_ENVIRONMENT_KEYS
    values = {key: inherited[key] for key in keys if key in inherited}
    values["HOME"] = str(Path(home).resolve(strict=True))
    for key in ("TMPDIR", "TMP", "TEMP"):
        values[key] = str(Path(temporary).resolve())
    return values


def host_home_directory() -> Path:
    """Resolve the OS account home without trusting an overridden HOME."""
    try:
        return Path(pwd.getpwuid(os.getuid()).pw_dir).resolve(strict=True)
    except (KeyError, OSError) as error:
        raise BlockedFailure("native Claude home cannot be resolved") from error


def _native_account(environment: dict[str, str]) -> str:
    """Mirror the provider's non-secret Keychain account selection."""
    account = environment.get("USER", "")
    if account == "":
        try:
            account = pwd.getpwuid(os.getuid()).pw_name
        except (KeyError, OSError):
            account = ""
    if account and all(char.isascii() and (char.isalnum() or char in "._-") for char in account):
        return account
    return "claude-code-user"


def _native_environment(environment: dict[str, str]) -> list[str]:
    """Build bounded native discovery/session environment without secrets."""
    keys = NATIVE_ENVIRONMENT_KEYS + NATIVE_DISCOVERY_ENVIRONMENT_KEYS
    values = [key + "=" + environment[key] for key in keys if key in environment]
    return sorted(values)


def expected_claude_inspection_binding(
        pueue: Path, config: Path, base: Path, config_digest: str,
        workspace: Path, executable: Path,
        runner: Path, environment: dict[str, str]) -> dict[str, object]:
    """Build Claude's expected portable runtime inspection binding."""
    executable = Path(executable).resolve()
    runner = Path(runner).resolve()
    values = _native_environment(environment)
    definition = {
        "revision": RUNTIME_INSPECTION_REVISION,
        "executable": str(executable),
        "executable_sha256": digest(executable),
        "arguments": None,
        "directory": str(Path(workspace).resolve(strict=True)),
        "environment": values,
        "output_limit": 1 << 20,
        "runtime": {
            "executable": str(executable),
            "executable_sha256": digest(executable),
            "directory": str(Path(workspace).resolve(strict=True)),
            "environment": values,
            "help_args": RUNTIME_HELP_ARGS,
            "required_flags": list(RUNTIME_REQUIRED_FLAGS),
        },
    }
    return {
        "definition_revision": RUNTIME_INSPECTION_REVISION,
        "definition_sha256": sha(canonical_go_json(definition)),
        "helper_executable": str(executable),
        "helper_sha256": digest(executable),
        "worker_executable": str(runner),
        "worker_sha256": digest(runner),
        "environment": values,
        "supervisor": supervisor_binding(pueue, config, base, config_digest),
    }


def provider_runtime_roots(environment: dict[str, str]) -> list[Path]:
    """Return Claude's known writable runtime/cache roots."""
    home = Path(environment.get("HOME", str(Path.home()))).resolve(strict=False)
    roots = [Path(item) for item in ("/tmp", "/var/tmp", "/var/folders", "/dev")]
    roots.extend(home / item for item in (
        ".claude", ".claude.json", ".cache", "Library/Caches", "Library/Logs",
        "Library/Application Support/Claude", "Library/Keychains",
    ))
    for name in ("TMP", "TMPDIR", "TEMP"):
        value = environment.get(name)
        if value:
            roots.append(Path(value))
    for name in ("XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"):
        value = environment.get(name)
        if value and Path(value).is_absolute():
            roots.append(Path(value) / CLAUDE_XDG_DIRECTORY)
    for name in ("XDG_CONFIG_DIRS", "XDG_DATA_DIRS"):
        for value in environment.get(name, "").split(os.pathsep):
            if value and Path(value).is_absolute():
                roots.append(Path(value) / CLAUDE_XDG_DIRECTORY)
    for name in ("CLAUDE_CONFIG_DIR", "ANTHROPIC_CONFIG_DIR"):
        value = environment.get(name)
        if value and Path(value).is_absolute():
            roots.append(Path(value))
    return sorted({root.resolve(strict=False) for root in roots})


def reject_runtime_roots(path: Path, label: str, environment: dict[str, str]) -> None:
    resolved = path.resolve(strict=False)
    for root in provider_runtime_roots(environment):
        if resolved == root or root in resolved.parents:
            raise AcceptanceFailure(f"{label} is inside provider writable runtime root {root}: {resolved}")


def resolve_executable(value: str | None, default: Path, label: str) -> Path:
    candidate = Path(value) if value else default
    if not candidate.is_absolute():
        candidate = candidate.resolve()
    if not candidate.is_file() or not os.access(candidate, os.X_OK):
        raise BlockedFailure(f"{label} is unavailable or not executable: {candidate}")
    resolved = candidate.resolve(strict=True)
    if not resolved.is_file() or not os.access(resolved, os.X_OK):
        raise BlockedFailure(f"{label} resolved target is not executable: {resolved}")
    return resolved


def require_discovery(name: str, selected: Path) -> None:
    discovered = shutil.which(name)
    if discovered is None:
        raise BlockedFailure(f"{name} is not discoverable in the inherited PATH")
    if Path(discovered).resolve() != selected:
        raise BlockedFailure(f"{name} discovery does not match selected executable; PATH shadowing is forbidden")


def task_id() -> str:
    return secrets.token_hex(16)


def is_task_id(value: object) -> bool:
    return isinstance(value, str) and TASK_PATTERN.fullmatch(value) is not None


def fresh_session_id(root_id: str, task: str) -> str:
    """Derive UUIDv5 from the complete root bytes and canonical task ID."""
    require(isinstance(root_id, str) and ROOT_PATTERN.fullmatch(root_id) is not None,
            "fresh Claude root identity is not a canonical 16-byte hex value")
    require(is_task_id(task), "fresh Claude task identity is not canonical")
    try:
        namespace = uuid.UUID(bytes=bytes.fromhex(root_id))
    except (ValueError, TypeError) as error:
        raise AcceptanceFailure("fresh Claude root identity cannot be decoded as UUID bytes") from error
    return str(uuid.uuid5(namespace, task))


def is_session_id(value: object) -> bool:
    return isinstance(value, str) and UUID_PATTERN.fullmatch(value) is not None


def reject_constant(value: str) -> None:
    raise ValueError("nonfinite JSON number: " + value)


def decode_event(line: bytes) -> dict[str, object]:
    require(line and line.strip(), "blank Claude JSONL event line")
    require(len(line) <= MAX_CONTROL_BYTES, "Claude JSONL event line exceeds 1 MiB")
    try:
        value = json.loads(line.decode("utf-8"), object_pairs_hook=unique_object,
                           parse_constant=reject_constant, parse_float=Decimal)
    except (UnicodeError, ValueError, TypeError, RecursionError) as error:
        raise AcceptanceFailure(f"invalid Claude JSONL event: {error}") from error
    require(isinstance(value, dict), "Claude JSONL event is not an object")
    event_type = value.get("type")
    require(isinstance(event_type, str) and event_type, "Claude JSONL event type is missing")
    return value


def _string_field(value: object, label: str) -> str:
    require(isinstance(value, str), label + " is not a string")
    return value


def _content_blocks(event: dict[str, object]) -> list[dict[str, object]]:
    message = event.get("message")
    if message is None:
        return []
    require(isinstance(message, dict), "Claude message event has no message object")
    content = message.get("content")
    if content is None:
        return []
    require(isinstance(content, list), "Claude message content is not an array")
    blocks: list[dict[str, object]] = []
    for block in content:
        require(isinstance(block, dict), "Claude message content block is not an object")
        kind = block.get("type")
        require(isinstance(kind, str) and kind in KNOWN_CONTENT_TYPES,
                "unknown Claude content block type")
        if kind == "text":
            require(isinstance(block.get("text"), str), "Claude text block has no text")
        elif kind == "thinking":
            require(isinstance(block.get("thinking"), str), "Claude thinking block has no thinking text")
        elif kind == "redacted_thinking":
            require(any(isinstance(block.get(field), str) for field in ("data", "text", "thinking")),
                    "Claude redacted thinking block has no data")
        elif kind == "tool_use":
            require(isinstance(block.get("id"), str) and block["id"],
                    "Claude tool_use has no id")
            require(isinstance(block.get("name"), str) and block["name"],
                    "Claude tool_use has no name")
            require(isinstance(block.get("input"), dict), "Claude tool_use input is not an object")
        else:
            require(isinstance(block.get("tool_use_id"), str) and block["tool_use_id"],
                    "Claude tool_result has no tool_use_id")
            if "is_error" in block:
                require(isinstance(block["is_error"], bool), "Claude tool_result is_error is not boolean")
            content_value = block.get("content", "")
            require(content_value is None or isinstance(content_value, (str, list, dict)),
                    "Claude tool_result content has an invalid shape")
        blocks.append(block)
    return blocks


def _content_text(value: object) -> str:
    if isinstance(value, str):
        return value
    if isinstance(value, list):
        return "\n".join(_content_text(entry) for entry in value)
    if isinstance(value, dict):
        return "\n".join(_content_text(entry) for entry in value.values())
    return ""


def _counter(value: object, label: str) -> None:
    if value is None:
        return
    require(isinstance(value, int) and not isinstance(value, bool) and 0 <= value <= (1 << 63) - 1,
            f"Claude {label} is not a bounded nonnegative integer")


def _decimal(value: object, label: str) -> None:
    if value is None:
        return
    require(isinstance(value, (int, float, Decimal)) and not isinstance(value, bool),
            f"Claude {label} is not a JSON number")
    try:
        number = Decimal(str(value))
    except (InvalidOperation, TypeError, ValueError) as error:
        raise AcceptanceFailure(f"Claude {label} is not a JSON number") from error
    require(number.is_finite() and number >= 0, f"Claude {label} is not finite and nonnegative")


def validate_usage_shapes(result: dict[str, object]) -> None:
    """Bound counters while preserving missing values and independent views."""
    usage = result.get("usage")
    if usage is not None:
        require(isinstance(usage, dict), "Claude result usage is not an object")
        for field in ("input_tokens", "output_tokens", "thinking_tokens", "total_tokens",
                      "cache_read_input_tokens", "cache_creation_input_tokens"):
            _counter(usage.get(field), "usage." + field)
    models = result.get("modelUsage")
    if models is not None:
        require(isinstance(models, dict) and len(models) <= 128,
                "Claude result modelUsage exceeds its bounded record count")
        for model, fields in models.items():
            require(isinstance(model, str) and model.strip() == model and model,
                    "Claude result modelUsage has an invalid model name")
            require(isinstance(fields, dict), "Claude result modelUsage entry is not an object")
            for field in ("inputTokens", "outputTokens", "thinkingTokens", "totalTokens",
                          "cacheReadInputTokens", "cacheCreationInputTokens"):
                _counter(fields.get(field), "modelUsage." + field)
            _decimal(fields.get("costUSD"), "modelUsage.costUSD")
    _decimal(result.get("total_cost_usd"), "result.total_cost_usd")
    for field in ("duration_ms", "num_turns"):
        _counter(result.get(field), "result." + field)


def parse_claude_events(raw: bytes) -> dict[str, object]:
    """Parse the bounded Claude print stream without selecting prose as success."""
    require(len(raw) <= MAX_PROVIDER_BYTES, "Claude stdout exceeds observation bound")
    lines = raw.split(b"\n")
    if lines and lines[-1] == b"":
        lines.pop()
    require(lines, "Claude JSONL stdout is empty")
    init: dict[str, object] | None = None
    result: dict[str, object] | None = None
    events: list[dict[str, object]] = []
    tool_uses: list[dict[str, object]] = []
    tool_results: list[dict[str, object]] = []
    permission_denials: list[dict[str, object]] = []
    event_session: str | None = None
    result_seen = False

    for event_index, raw_line in enumerate(lines):
        event = decode_event(raw_line)
        event_type = event["type"]
        if result_seen and event_type in {"assistant", "user", "result"}:
            raise AcceptanceFailure("Claude emitted substantive output after terminal result")
        if event_type == "error":
            raise AcceptanceFailure("top-level Claude error event is terminally ambiguous")
        if isinstance(event_type, str) and (event_type.startswith("thread.") or
                                             event_type.startswith("turn.") or
                                             event_type.startswith("session.")):
            raise AcceptanceFailure("unknown Claude thread/turn/session lifecycle event: " + event_type)
        session = event.get("session_id")
        if session is not None:
            require(is_session_id(session), "Claude event session_id is not a UUID")
            if event_session is None:
                event_session = session
            require(event_session == session, "Claude event session identity changed")
        if event_type == "system":
            subtype = event.get("subtype")
            if subtype is not None:
                require(isinstance(subtype, str), "Claude system subtype is not a string")
            if subtype == "init":
                require(init is None, "multiple Claude init events")
                init = event
                init_session = event.get("session_id")
                require(is_session_id(init_session), "Claude init session_id is invalid")
                if event_session is None:
                    event_session = init_session
                require(event_session == init_session, "Claude init session identity conflicts")
            elif isinstance(subtype, str):
                lower = subtype.lower()
                if any(marker in lower for marker in ("abort", "cancel", "error", "fail", "interrupt", "shutdown", "stop", "timeout", "timed_out", "timed-out", "refusal")):
                    raise AcceptanceFailure("unknown terminal Claude system lifecycle event: " + subtype)
            events.append(event)
        elif event_type in {"assistant", "user"}:
            require(init is not None, "Claude assistant/user event precedes system/init")
            blocks = _content_blocks(event)
            for block in blocks:
                if block["type"] == "tool_use":
                    tool_uses.append({
                        "id": block["id"], "name": block["name"],
                        "input": deepcopy(block["input"]), "event_type": event_type,
                        "event_index": event_index,
                    })
                elif block["type"] == "tool_result":
                    tool_results.append({
                        "tool_use_id": block["tool_use_id"],
                        "is_error": block.get("is_error", False),
                        "content": deepcopy(block.get("content", "")),
                        "event_type": event_type, "event_index": event_index,
                    })
            events.append(event)
        elif event_type == "result":
            require(result is None, "multiple Claude result events")
            require(init is not None, "Claude result precedes system/init")
            result = event
            result_seen = True
            result_session = event.get("session_id")
            require(is_session_id(result_session), "Claude result session_id is invalid")
            if event_session is None:
                event_session = result_session
            require(event_session == result_session, "Claude result session identity conflicts")
            subtype = event.get("subtype")
            require(isinstance(subtype, str), "Claude result subtype is missing or not a string")
            require(subtype == "success" or subtype in ERROR_SUBTYPES,
                    "unknown Claude result subtype: " + subtype)
            require(isinstance(event.get("is_error"), bool),
                    "Claude result is_error is missing or not boolean")
            if subtype == "success":
                require(event["is_error"] is False, "Claude successful result has is_error=true")
            else:
                require(event["is_error"] is True, "Claude error result has is_error=false")
            if "result" in event:
                require(isinstance(event["result"], str), "Claude result answer is not a string")
            if "stop_reason" in event:
                require(event["stop_reason"] is None or isinstance(event["stop_reason"], str),
                        "Claude stop_reason is not a string or null")
            for field in ("usage", "modelUsage"):
                if field in event:
                    require(event[field] is None or isinstance(event[field], dict),
                            "Claude " + field + " is not an object or null")
            denials = event.get("permission_denials")
            if "permission_denials" in event:
                require(isinstance(denials, list), "Claude permission_denials is not an array")
                for denial in denials:
                    require(isinstance(denial, dict), "Claude permission denial is not an object")
                    permission_denials.append(deepcopy(denial))
            errors = event.get("errors")
            if "errors" in event:
                require(isinstance(errors, list) and all(isinstance(value, str) for value in errors),
                        "Claude result errors are not an array of strings")
                require(subtype != "success" or not errors,
                        "Claude successful result has nonempty errors")
            validate_usage_shapes(event)
            events.append(event)
        else:
            # Unknown non-lifecycle events are telemetry.  Their payload is
            # intentionally not retained as an answer or permission evidence.
            events.append({"type": event_type})

    require(init is not None, "Claude init event is absent")
    require(result is not None, "Claude terminal result event is absent")
    require(event_session is not None and is_session_id(event_session),
            "Claude stream session identity is absent")
    return {
        "session_id": event_session,
        "init": init,
        "result_event": result,
        "events": events,
        "tool_uses": tool_uses,
        "tool_results": tool_results,
        "permission_denials": permission_denials,
        "usage": result.get("usage"),
        "model_usage": result.get("modelUsage"),
    }


def validate_claude_result(parsed: dict[str, object], expected_session: str | None = None,
                           expected_answer: bytes | None = None) -> bytes:
    """Require Claude's terminal result predicate before accepting bytes."""
    session = parsed.get("session_id")
    require(is_session_id(session), "Claude result has no valid session identity")
    if expected_session is not None:
        require(session == expected_session, "Claude result session identity mismatch")
    result = parsed.get("result_event")
    require(isinstance(result, dict), "Claude result event is absent")
    subtype = result.get("subtype")
    require(subtype == "success", "Claude result is not a successful subtype")
    require(result.get("is_error") is False, "Claude successful result is_error is not false")
    answer = result.get("result")
    require(isinstance(answer, str) and answer.strip(), "Claude result answer is blank or absent")
    stop_reason = result.get("stop_reason")
    if stop_reason is not None:
        require(stop_reason.lower() in {"end_turn", "stop_sequence"},
                "Claude result has an incomplete/refused/unknown stop reason")
    require(subtype not in ERROR_SUBTYPES, "Claude result has an error subtype")
    data = answer.encode("utf-8")
    if expected_answer is not None:
        require(data == expected_answer, "Claude result bytes differ from expected answer")
    return data


def validate_init_profile(parsed: dict[str, object], expected_session: str | None = None,
                          expected_cwd: str | None = None) -> dict[str, object]:
    """Bind identity and requested permission while leaving policy to Claude."""
    init = parsed.get("init")
    require(isinstance(init, dict), "Claude init evidence is absent")
    session = init.get("session_id")
    require(is_session_id(session), "Claude init session identity is invalid")
    if expected_session is not None:
        require(session == expected_session, "Claude init session identity mismatch")
    version = init.get("claude_code_version")
    require(isinstance(version, str) and version.strip() == version and version,
            "Claude init runtime version is absent or malformed")
    cwd = init.get("cwd")
    require(isinstance(cwd, str) and cwd and "\x00" not in cwd,
            "Claude init cwd is absent or malformed")
    if expected_cwd is not None:
        require(cwd == expected_cwd, "Claude init cwd differs from the acceptance workspace")
    tools = init.get("tools")
    require(isinstance(tools, list), "Claude init tool list is absent or malformed")
    require(all(isinstance(tool, str) and tool for tool in tools),
            "Claude init tool list is malformed")
    require(len(set(tools)) == len(tools), "Claude init tool list is duplicated")
    permission = init.get("permissionMode")
    require(permission == APPROVAL, "Claude init permission mode is not plan")
    if "model" in init:
        model = init["model"]
        require(isinstance(model, str) and model.strip() == model and model and "\x00" not in model,
                "Claude init model is malformed")
    api_key_source = init.get("apiKeySource")
    require(isinstance(api_key_source, str) and api_key_source.strip() == api_key_source and api_key_source,
            "Claude init apiKeySource is missing or malformed")
    mcp_servers = init.get("mcp_servers")
    require(isinstance(mcp_servers, list), "Claude init mcp_servers are not an array")
    return {"session_id": session, "tools": list(tools), "permission_mode": permission,
            "api_key_source": api_key_source, "mcp_server_count": len(mcp_servers)}


def _successful_tool_result(results: list[dict[str, object]], use: dict[str, object]) -> dict[str, object]:
    matches = [value for value in results if value.get("tool_use_id") == use["id"]]
    require(len(matches) == 1, "Claude tool use has no unique result")
    result = matches[0]
    require(use.get("event_type") == "assistant" and result.get("event_type") == "user" and
            type(use.get("event_index")) is int and type(result.get("event_index")) is int and
            use["event_index"] < result["event_index"],
            "Claude tool result lacks a preceding assistant tool use")
    require(result.get("is_error") is False, "Claude positive tool failed")
    return result


def _tool_input_mentions_path(use: dict[str, object], path: Path) -> bool:
    inputs = use.get("input")
    require(isinstance(inputs, dict), "Claude nonce tool input is malformed")
    try:
        encoded = json.dumps(inputs, ensure_ascii=True, sort_keys=True, separators=(",", ":"))
    except (TypeError, ValueError) as error:
        raise AcceptanceFailure("Claude nonce tool input is not encodable") from error
    return str(path) in encoded


def _nonce_tool_uses(results: list[dict[str, object]], uses: list[dict[str, object]],
                     nonce_file: Path, nonce: bytes) -> list[dict[str, object]]:
    nonce_text = nonce.decode("ascii")
    matches: list[dict[str, object]] = []
    for use in uses:
        if not _tool_input_mentions_path(use, nonce_file):
            continue
        result = _successful_tool_result(results, use)
        content = _content_text(result.get("content"))
        if nonce_text in content:
            matches.append(use)
    require(matches, "Claude did not provide a successful tool read containing the hidden nonce")
    return matches


def validate_fresh_controls(parsed: dict[str, object], nonce_file: Path,
                            workspace: Path, sibling: Path, nonce: bytes,
                            expected_session: str | None = None) -> dict[str, object]:
    """Require real read/search calls and preserve native policy evidence."""
    init = validate_init_profile(parsed, expected_session=expected_session,
                                 expected_cwd=str(workspace))
    validate_claude_result(parsed, expected_session=str(init["session_id"]),
                           expected_answer=nonce)
    uses = parsed.get("tool_uses")
    results = parsed.get("tool_results")
    require(isinstance(uses, list) and isinstance(results, list), "Claude tool evidence is absent")
    for use in uses:
        require(isinstance(use, dict), "Claude tool use evidence is malformed")
        require(isinstance(use.get("name"), str) and use["name"],
                "Claude tool name evidence is malformed")
    require(len({use["id"] for use in uses}) == len(uses), "Claude tool use IDs are not unique")
    nonce_uses = _nonce_tool_uses(results, uses, nonce_file, nonce)
    denials = parsed.get("permission_denials")
    require(isinstance(denials, list), "Claude permission-denial evidence is malformed")
    return {
        "session_id": init["session_id"],
        "tool_names": list(init["tools"]),
        "api_key_source": init.get("api_key_source"),
        "mcp_server_count": init["mcp_server_count"],
        "nonce_read": True,
        "nonce_tool_name": nonce_uses[0]["name"],
        "nonce_read_count": len(nonce_uses),
        "permission_denial_count": len(denials),
        "tool_use_count": len(uses),
    }


def validate_tool_free_resume(parsed: dict[str, object], expected_answer: bytes,
                              expected_session: str | None = None) -> dict[str, object]:
    """Accept a continuation only when no Claude tool item/event occurred."""
    init = validate_init_profile(parsed, expected_session)
    answer = validate_claude_result(parsed, expected_session=str(init["session_id"]))
    require(answer == expected_answer, "Claude resume returned an unexpected continuation answer")
    uses = parsed.get("tool_uses")
    results = parsed.get("tool_results")
    require(isinstance(uses, list) and not uses, "Claude resume used a tool")
    require(isinstance(results, list) and not results, "Claude resume returned a tool result")
    denials = parsed.get("permission_denials")
    require(isinstance(denials, list) and not denials,
            "Claude resume contains permission-denial evidence")
    item_types: list[str] = []
    item_count = 0
    for event in parsed.get("events", []):
        if not isinstance(event, dict) or event.get("type") not in {"assistant", "user"}:
            continue
        blocks = _content_blocks(event)
        for block in blocks:
            require(block["type"] in {"text", "thinking", "redacted_thinking"},
                    "Claude resume contains a disallowed item type")
            item_count += 1
            if block["type"] not in item_types:
                item_types.append(block["type"])
    return {"tool_free": True, "item_count": item_count, "item_types": item_types,
            "session_id": init["session_id"]}


def provider_record_snapshot(directory: Path) -> dict[str, dict[str, object]]:
    return snapshot(directory, ignored_prefixes=(".ack",))


def validate_manifest(directory: Path, seal: dict[str, object]) -> None:
    manifest = seal.get("raw_manifest")
    require(isinstance(manifest, list), "Claude raw manifest is absent")
    paths: set[str] = set()
    previous: str | None = None
    for entry in manifest:
        require(isinstance(entry, dict), "Claude raw manifest entry is not an object")
        relative = entry.get("path")
        size = entry.get("size")
        checksum = entry.get("sha256")
        require(isinstance(relative, str) and relative.startswith("raw/") and relative not in paths,
                "Claude raw manifest path is malformed or duplicated")
        basename = relative[4:]
        require(basename in {"stdout", "stderr"} or
                re.fullmatch(r"[a-z0-9][a-z0-9._-]{0,127}", basename) is not None,
                "Claude raw manifest path is not a safe task-local basename")
        require(previous is None or previous < relative, "Claude raw manifest is not strictly sorted")
        require(isinstance(size, int) and not isinstance(size, bool) and
                0 <= size <= MAX_PROVIDER_BYTES and isinstance(checksum, str) and
                re.fullmatch(r"[0-9a-f]{64}", checksum) is not None,
                "Claude raw manifest descriptor is malformed")
        path = directory / relative
        require(path.is_file() and not path.is_symlink(), "Claude raw manifest file is absent")
        require(path.stat().st_size == size and digest(path) == checksum,
                "Claude raw manifest digest mismatch")
        paths.add(relative)
        previous = relative
    require({"raw/stdout", "raw/stderr"} <= paths, "Claude raw manifest omitted required evidence")
    canonical = json.dumps(
        [{"path": entry["path"], "size": entry["size"], "sha256": entry["sha256"]}
         for entry in manifest], ensure_ascii=True, separators=(",", ":")) + "\n"
    require(seal.get("manifest_sha256") == sha(canonical.encode("utf-8")),
            "Claude provider.exit manifest_sha256 does not match canonical raw manifest")


def validate_record_usage(outcome: dict[str, object], parsed: dict[str, object], name: str) -> None:
    """Check every native usage view, preserving null counters and scopes."""
    result = parsed.get("result_event")
    require(isinstance(result, dict), f"{name} result usage source is absent")
    usage = result.get("usage")
    model_usage = result.get("modelUsage")
    native_counter = any(result.get(field) is not None for field in ("duration_ms", "num_turns"))
    has_native_usage = (usage is not None or bool(model_usage) or
                        result.get("total_cost_usd") is not None or native_counter)
    recorded = outcome.get("usage")
    if not has_native_usage:
        require(recorded in (None, []), f"{name} invented usage without provider counters")
        return
    require(isinstance(recorded, list), f"{name} usage is not a list")

    expected: list[dict[str, object]] = []

    def usage_record(scope: str) -> dict[str, object]:
        return {
            "scope": scope, "source": "provider-envelope", "reliability": "reported",
            "input_tokens": None, "output_tokens": None, "thinking_tokens": None,
            "total_tokens": None, "cache_read_tokens": None, "cache_creation_tokens": None,
            "num_turns": None, "duration_seconds": None, "model": None,
            "estimated_cost_usd": None,
        }

    def set_if_present(target: dict[str, object], source: dict[str, object], source_name: str,
                      target_name: str) -> None:
        if source_name in source and source[source_name] is not None:
            target[target_name] = source[source_name]

    if isinstance(usage, dict) or native_counter:
        main = usage_record("main-agent")
        if isinstance(usage, dict):
            for source_name, target_name in (
                    ("input_tokens", "input_tokens"), ("output_tokens", "output_tokens"),
                    ("thinking_tokens", "thinking_tokens"), ("total_tokens", "total_tokens"),
                    ("cache_read_input_tokens", "cache_read_tokens"),
                    ("cache_creation_input_tokens", "cache_creation_tokens")):
                set_if_present(main, usage, source_name, target_name)
        set_if_present(main, result, "num_turns", "num_turns")
        if result.get("duration_ms") is not None:
            milliseconds = result["duration_ms"]
            require(isinstance(milliseconds, int) and not isinstance(milliseconds, bool),
                    f"{name} duration_ms is not an integer")
            seconds = str(milliseconds // 1000)
            remainder = milliseconds % 1000
            if remainder:
                seconds = (seconds + ".%03d" % remainder).rstrip("0")
            main["duration_seconds"] = seconds
        init = parsed.get("init")
        if isinstance(init, dict) and init.get("model") is not None:
            main["model"] = init.get("model")
        expected.append(main)

    if result.get("total_cost_usd") is not None:
        total = usage_record("whole-tree")
        total["estimated_cost_usd"] = result["total_cost_usd"]
        expected.append(total)

    if isinstance(model_usage, dict):
        for model in sorted(model_usage):
            fields = model_usage[model]
            require(isinstance(fields, dict), f"{name} modelUsage entry is not an object")
            record = usage_record("whole-tree")
            record["model"] = model
            for source_name, target_name in (
                    ("inputTokens", "input_tokens"), ("outputTokens", "output_tokens"),
                    ("thinkingTokens", "thinking_tokens"), ("totalTokens", "total_tokens"),
                    ("cacheReadInputTokens", "cache_read_tokens"),
                    ("cacheCreationInputTokens", "cache_creation_tokens"),
                    ("costUSD", "estimated_cost_usd")):
                set_if_present(record, fields, source_name, target_name)
            expected.append(record)

    if not expected:
        require(recorded in (None, []), f"{name} invented usage without provider counters")
        return
    require(len(recorded) == len(expected),
            f"{name} usage records were omitted or synthesized")
    integer_fields = {
        "input_tokens", "output_tokens", "thinking_tokens", "total_tokens",
        "cache_read_tokens", "cache_creation_tokens", "num_turns",
    }
    for index, (actual, want) in enumerate(zip(recorded, expected)):
        require(isinstance(actual, dict), f"{name} usage entry {index} is not an object")
        require("task_delta" not in actual, f"{name} synthesized a task usage delta")
        require(set(actual) <= set(want), f"{name} usage entry {index} has unknown fields")
        for field, value in want.items():
            if value is None:
                require(field not in actual or actual[field] is None,
                        f"{name} synthesized missing usage field {field}")
                continue
            if field == "estimated_cost_usd":
                require(isinstance(actual.get(field), str),
                        f"{name} usage cost {field} is not a string")
                try:
                    actual_number = Decimal(str(actual.get(field)))
                    expected_number = Decimal(str(value))
                except (InvalidOperation, TypeError, ValueError) as error:
                    raise AcceptanceFailure(f"{name} usage cost {field} is malformed") from error
                require(actual_number == expected_number,
                        f"{name} usage field {field} differs from native value")
            elif field in integer_fields:
                require(isinstance(actual.get(field), int) and
                        not isinstance(actual.get(field), bool) and
                        actual.get(field) == value,
                        f"{name} usage field {field} differs from native value")
            else:
                require(actual.get(field) == value,
                        f"{name} usage field {field} differs from native value")


class ClaudeAcceptance:
    def __init__(self, args: argparse.Namespace):
        self.args = args
        self.tools = Path(args.tools).resolve(strict=True)
        require(self.tools.is_dir() and not self.tools.is_symlink(), "--tools must be a directory")
        self.delegate = resolve_executable(args.delegate, self.tools / "delegate", "delegate")
        self.runner = resolve_executable(args.runner, self.tools / "delegate-run", "delegate-run")
        self.pueue = resolve_executable(args.pueue, Path("/opt/homebrew/bin/pueue"), "pueue")
        self.pueued = resolve_executable(args.pueued, Path("/opt/homebrew/bin/pueued"), "pueued")
        self.claude = resolve_executable(args.claude, Path(shutil.which("claude") or "/opt/homebrew/bin/claude"), "claude")
        self.provider_version: str | None = None
        self.provider_sha256 = digest(self.claude)
        self.profile_revision: str | None = None
        require_discovery("claude", self.claude)
        require_discovery("pueue", self.pueue)
        require_discovery("pueued", self.pueued)
        self.output = clean_absolute(args.output, "evidence output")
        reject_tmp(self.output, "evidence output")
        reject_runtime_roots(self.output, "evidence output", path_validation_environment(os.environ))
        require(not self.output.exists(), f"evidence output already exists: {self.output}")
        self.output.mkdir(mode=0o700, parents=True, exist_ok=False)
        os.chmod(self.output, 0o700)
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + secrets.token_hex(6)
        self.state_parent = ensure_private_directory(DEFAULT_STATE_PARENT / ("claude-" + stamp),
                                                      "acceptance state parent", create=True)
        self.test_tmp = ensure_private_directory(self.state_parent / "tmp", "isolated Claude temporary root", create=True)
        self.host_home = host_home_directory()
        self.environment = native_account_acceptance_environment(os.environ, self.host_home, self.test_tmp)
        self.state = ensure_private_directory(self.state_parent / "state", "acceptance state", create=True)
        # Keep the scratch checkout outside the caller's home tree. Claude's
        # project-policy walk includes every ancestor up to the filesystem
        # root, so a home-contained checkout would still discover the real
        # ~/.claude project settings even with an isolated HOME.
        workspace_parent = private_mkdtemp(
            "delegation-layer-claude-workspace-", "acceptance workspace parent")
        self.workspace = ensure_private_directory(workspace_parent / "fresh",
                                                   "acceptance workspace", create=True)
        self.sibling = ensure_private_directory(workspace_parent / "sibling",
                                                 "acceptance sibling", create=True)
        self.briefs = ensure_private_directory(self.state_parent / "briefs", "acceptance briefs", create=True)
        for path, label in ((self.state, "acceptance state"), (self.workspace, "acceptance workspace"),
                            (self.sibling, "acceptance sibling"), (self.output, "evidence output")):
            reject_runtime_roots(path, label, self.environment)
            if path.exists() and path.is_dir():
                require(path.resolve(strict=True) == path, f"{label} must be canonical")
        require(not path_is_within(self.output, self.state_parent) and
                not path_is_within(self.state_parent, self.output) and
                not path_is_within(self.output, self.workspace) and
                not path_is_within(self.workspace, self.output) and
                not path_is_within(self.output, self.sibling) and
                not path_is_within(self.sibling, self.output),
                "evidence output overlaps acceptance state or workspace")
        self.nonce_file = self.workspace / "nonce.txt"
        self.workspace_sentinel = self.workspace / "workspace-sentinel.txt"
        self.sibling_sentinel = self.sibling / "sibling-sentinel.txt"
        self.nonce = secrets.token_hex(24).encode("ascii")
        self.inside_bytes = b"WORKSPACE-SENTINEL-ORIGINAL\n"
        self.sibling_bytes = b"SIBLING-SENTINEL-ORIGINAL\n"
        write_bytes(self.nonce_file, self.nonce + b"\n")
        write_bytes(self.workspace_sentinel, self.inside_bytes)
        write_bytes(self.sibling_sentinel, self.sibling_bytes)
        self.workspace_before = None
        self.sibling_before = None
        self.processes = Processes(self.output / "processes", self.environment)
        self.ops = NativeTaskOps(self.processes, self.delegate, self.runner, self.pueue,
                                 self.state_parent, self.state, self.output,
                                 watch_seconds=WATCH_SECONDS)
        self.pueue_base: Path | None = None
        self.pueue_config: Path | None = None
        self.daemon = None
        self.closed = False
        self.root_id: str | None = None
        self.inspection_binding: dict[str, object] | None = None
        self.tasks = self.ops.tasks
        self.labels = self.ops.labels
        self.numbers = self.ops.numbers
        self.dispatch_attempts = self.ops.dispatch_attempts
        self.records: dict[str, dict[str, object]] = {}
        self.queue_before_replay = None

    def provider_argv(self, session_id: str, resume: bool = False) -> list[str]:
        args = [str(self.claude), "--print", "--input-format", "text", "--output-format", "stream-json",
                "--verbose", "--permission-mode", "plan", "--permission-prompts", "none"]
        args.extend(["--resume", session_id] if resume else ["--session-id", session_id])
        return args

    def setup(self) -> None:
        self.pueue_base = private_mkdtemp("delegation-layer-claude-", "private pueue base")
        ensure_private_directory(self.pueue_base / "state", "private pueue state", create=True)
        ensure_private_directory(self.pueue_base / "run", "private pueue runtime", create=True)
        write_bytes(self.pueue_base / "aliases.yml", b"{}\n")
        self.pueue_config = self.pueue_base / "pueue.yml"
        write_json(self.pueue_config, config_for(self.pueue_base))
        config_digest = digest(self.pueue_config)
        self.inspection_binding = expected_claude_inspection_binding(
            self.pueue, self.pueue_config, self.pueue_base, config_digest,
            self.workspace, self.claude, self.runner, self.environment)
        pueue_version = self.ops.direct("pueue-version", [self.pueue, "--version"], timeout=15)
        pueued_version = self.ops.direct("pueued-version", [self.pueued, "-c", self.pueue_config, "--version"], timeout=15)
        require((pueue_version.directory / "stdout").read_text().strip() == "pueue " + PUEUE_VERSION,
                "unexpected pueue version")
        require((pueued_version.directory / "stdout").read_text().strip() == "pueued " + PUEUE_VERSION,
                "unexpected pueued version")
        claude_version = self.ops.direct("claude-version", [self.claude, "--version"], timeout=15)
        observed = (claude_version.directory / "stdout").read_text().strip()
        require(observed, "Claude returned an empty version")
        self.provider_version = observed
        self.daemon = self.processes.start("private-daemon", [self.pueued, "-c", self.pueue_config], self.state_parent)
        self.ops.bind_supervisor(self.pueue_config, self.daemon)
        for _ in range(200):
            require(self.daemon.poll() is None, "private pueued exited before readiness")
            process = self.ops.client("ready", ["status", "--json"], expected={0, 1}, timeout=15)
            if int(process.result["exit_code"]) == 0:
                status = self.ops.queue_status("ready-status")
                require(status["tasks"] == {}, "new private pueue queue was not empty")
                break
            time.sleep(0.1)
        else:
            raise AcceptanceFailure("private pueue readiness was not established")
        write_json(self.output / "binding.json", {
            "provider": PROVIDER, "mode": MODE, "approval": APPROVAL,
            "claude": str(self.claude), "claude_version": self.provider_version,
            "claude_sha256": self.provider_sha256, "delegate": str(self.delegate),
            "delegate_sha256": digest(self.delegate), "runner": str(self.runner),
            "runner_sha256": digest(self.runner), "pueue": str(self.pueue),
            "pueue_sha256": digest(self.pueue), "pueued": str(self.pueued),
            "pueued_sha256": digest(self.pueued), "pueue_version": PUEUE_VERSION,
            "pueued_version": PUEUE_VERSION, "pueue_base": str(self.pueue_base),
            "pueue_config": str(self.pueue_config), "config_sha256": digest(self.pueue_config),
            "state": str(self.state), "workspace": str(self.workspace), "sibling": str(self.sibling),
            "host_home": str(self.host_home), "isolated_temporary_root": str(self.test_tmp),
            "environment_isolated": True, "host_home_inherited": True,
            "native_auth_context": "host-login-keychain",
            "nonce_sha256": sha(self.nonce + b"\n"), "environment_keys": sorted(self.environment),
            "environment_values_in_record": False,
            "acceptance_status": PRELAUNCH_STATUS,
            "planned_native_ai_turns": PLANNED_NATIVE_AI_TURNS, "driver_sha256": digest(__file__),
        })
        self.workspace_before = snapshot(self.workspace, ignored_prefixes=())
        self.sibling_before = snapshot(self.sibling, ignored_prefixes=())

    def fresh_brief(self) -> Path:
        path = self.briefs / "fresh.md"
        text = (
            "Use any available native read-only tool to read exactly this file: " + str(self.nonce_file) +
            ". Do not modify the workspace or its sibling. Return exactly the 48-character nonce read from "
            "that file as the result, with no prose or explanation."
        )
        write_bytes(path, text.encode())
        require(self.nonce.decode() not in path.read_text(), "fresh Claude brief contains the nonce")
        return path

    def resume_brief(self) -> Path:
        path = self.briefs / "resume.md"
        text = ("Use no tools. For each of the 48 hexadecimal characters in the answer from the previous turn, "
                "write two bits: the first bit is 1 for a-f and 0 for 0-9; the second bit is 1 for odd and 0 "
                "for even. Concatenate the 48 pairs and reply only with that 96-bit string.")
        write_bytes(path, text.encode())
        require(self.nonce.decode() not in path.read_text(), "resume Claude brief contains the nonce")
        return path

    def resume_challenge(self) -> bytes:
        """Require a high-entropy, nonce-dependent continuation transform."""
        return "".join(
            ("1" if character in "abcdef" else "0") +
            ("1" if int(character, 16) % 2 else "0")
            for character in self.nonce.decode("ascii")
        ).encode("ascii")

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
                "expected Claude inspection binding is unavailable")
        self.ops.expect_inspection(task, self.inspection_binding)
        response = self.ops.dispatch(name, task, self.dispatch_arguments(task, brief, predecessor))
        root = response.get("root_id")
        require(is_task_id(root), f"{name} omitted a valid root identity")
        self.root_id = root
        self.ops.root_id = root
        return response

    def wait_task(self, name: str, task: str) -> dict[str, object]:
        require(self.root_id is not None, "root identity is unavailable")
        self.ops.root_id = self.root_id
        return self.ops.wait_task(name, task)

    def collect(self, name: str, task: str) -> dict[str, object]:
        return self.ops.collect(name, task)

    def read_record(self, task: str, name: str) -> dict[str, object]:
        value = read_json(self.state / "tasks" / task / name)
        require(isinstance(value, dict), f"{task}/{name} must be an object")
        return value

    def validate_task(self, name: str, task: str, expected_answer: bytes,
                      predecessor: str | None = None,
                      expected_session: str | None = None) -> dict[str, object]:
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
        if predecessor is None:
            require(request.get("prior_session") is None, f"{name} unexpectedly has a predecessor")
        else:
            prior = request.get("prior_session")
            require(isinstance(prior, dict) and prior.get("provider") == PROVIDER and
                    prior.get("predecessor_task_id") == predecessor and
                    is_session_id(prior.get("conversation_id")),
                    f"{name} lacks the exact predecessor session reference")
        meta = self.read_record(task, "meta.json")
        require(meta.get("root_id") == self.root_id and meta.get("task_id") == task and
                meta.get("provider_executable") == str(self.claude) and
                meta.get("provider_version") == self.provider_version and
                meta.get("containment") == MODE and meta.get("approval") == APPROVAL,
                f"{name} meta provider or policy binding mismatch")
        effective = meta.get("effective_config")
        require(isinstance(effective, dict) and effective.get("containment") == MODE and
                effective.get("approval") == APPROVAL and isinstance(effective.get("digest"), str),
                f"{name} effective Claude policy is absent")
        policy = effective.get("policy")
        require(isinstance(policy, dict) and policy.get("workspace") == str(self.workspace) and
                policy.get("runtime_sha256") == self.provider_sha256,
                f"{name} persisted Claude profile policy is incomplete")
        profile_revision = policy.get("profile_revision")
        require(profile_revision == NATIVE_PROFILE_REVISION,
                f"{name} effective policy is not the native Claude profile")
        require(policy.get("sources") in (None, []),
                f"{name} native policy unexpectedly contains source inventory")
        if self.profile_revision is None:
            self.profile_revision = profile_revision
        require(profile_revision == self.profile_revision, f"{name} effective policy revision drifted")
        predicate = meta.get("predicate")
        require(predicate == {"adapter": PROVIDER, "mode": MODE,
                              "version": PREDICATE_VERSION, "sha256": PREDICATE_SHA256},
                f"{name} predicate binding is absent")
        inputs = meta.get("input_files")
        require(inputs in (None, []),
                f"{name} native Claude task unexpectedly declares configuration inputs")
        require(meta.get("output_artifacts") in (None, []) and
                meta.get("output_writer_contract") in (None, ""),
                f"{name} unexpectedly declared a Claude output artifact")
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
        raw = (directory / "raw/stdout").read_bytes()
        require(len(raw) <= MAX_PROVIDER_BYTES and (directory / "raw/stderr").stat().st_size <= MAX_PROVIDER_BYTES,
                f"{name} raw stream exceeds acceptance bound")
        parsed = parse_claude_events(raw)
        profile = validate_init_profile(parsed, expected_session=expected_session,
                                        expected_cwd=str(self.workspace))
        answer = validate_claude_result(parsed, expected_session=expected_session,
                                        expected_answer=expected_answer)
        require(answer == expected_answer, f"{name} final result differs from committed answer")
        reference = self.read_record(task, "provider.ref.json")
        require(reference.get("root_id") == self.root_id and reference.get("task_id") == task and
                reference.get("provider") == PROVIDER and
                reference.get("conversation_id") == parsed["session_id"] and
                is_session_id(reference.get("conversation_id")),
                f"{name} provider session identity mismatch")
        outcome = self.read_record(task, "outcome.json")
        require(outcome.get("root_id") == self.root_id and outcome.get("task_id") == task and
                outcome.get("spec_sha256") == meta.get("spec_sha256") and
                outcome.get("meta_sha256") == digest(directory / "meta.json") and
                outcome.get("verdict") == "committed" and outcome.get("predicate") == predicate and
                outcome.get("evidence_sha256") == seal.get("manifest_sha256"),
                f"{name} outcome authority mismatch")
        validate_record_usage(outcome, parsed, name)
        payload = outcome.get("payload")
        require(read_outcome_payload(directory, outcome) == expected_answer,
                f"{name} committed payload mismatch")
        resume_controls = None
        if predecessor is not None:
            resume_controls = validate_tool_free_resume(parsed, expected_answer,
                                                         str(reference["conversation_id"]))
        return {"directory": directory, "request": request, "meta": meta, "seal": seal,
                "reference": reference, "outcome": outcome, "payload": deepcopy(payload),
                "parsed": parsed, "init": profile, "resume_controls": resume_controls,
                "snapshot": provider_record_snapshot(directory)}

    def run_fresh(self) -> dict[str, object]:
        brief = self.fresh_brief()
        task = task_id()
        response = self.dispatch("fresh", task, brief)
        self.wait_task("fresh", task)
        collected = self.collect("fresh-collect", task)
        outcome, data = verify_collected_outcome(collected, self.state / "tasks" / task, "committed")
        require(data == self.nonce, "fresh Claude result was not exactly the nonce")
        require(self.root_id is not None, "fresh Claude root identity is unavailable")
        expected_session = fresh_session_id(self.root_id, task)
        parsed = parse_claude_events((self.state / "tasks" / task / "raw/stdout").read_bytes())
        controls = validate_fresh_controls(parsed, self.nonce_file, self.workspace,
                                           self.sibling, self.nonce, expected_session)
        require(self.workspace_before == snapshot(self.workspace, ignored_prefixes=()),
                "Claude changed the scratch workspace or workspace sentinel")
        require(self.sibling_before == snapshot(self.sibling, ignored_prefixes=()),
                "Claude changed the sibling sentinel")
        record = self.validate_task("fresh", task, data, expected_session=expected_session)
        record.update({"dispatch": response, "collect": collected, "controls": controls})
        self.records["fresh"] = record
        write_json(self.output / "fresh-control.json", {
            "task_id": task, "conversation_id": record["reference"]["conversation_id"],
            "nonce_sha256": sha(self.nonce), "tool_names": controls["tool_names"],
            "api_key_source": controls["api_key_source"],
            "mcp_server_count": controls["mcp_server_count"],
            "nonce_read": True, "nonce_tool_name": controls["nonce_tool_name"],
            "nonce_read_count": controls["nonce_read_count"],
            "tool_use_count": controls["tool_use_count"],
            "permission_denial_count": controls["permission_denial_count"],
            "workspace_unchanged": True,
            "sibling_unchanged": True, "provider_exit_manifest_sha256": record["seal"]["manifest_sha256"],
            "outcome_sha256": digest(record["directory"] / "outcome.json"), "outcome": outcome,
        })
        return record

    def run_resume(self, predecessor: dict[str, object]) -> dict[str, object]:
        predecessor_task = self.tasks["fresh"]
        original_snapshot = predecessor["snapshot"]
        require(isinstance(original_snapshot, dict), "fresh task snapshot is unavailable")
        self.nonce_file.unlink(missing_ok=False)
        require(not self.nonce_file.exists(), "resume nonce file still exists")
        workspace_before = snapshot(self.workspace, ignored_prefixes=())
        task = task_id()
        response = self.dispatch("resume", task, self.resume_brief(), predecessor_task)
        self.wait_task("resume", task)
        collected = self.collect("resume-collect", task)
        outcome, data = verify_collected_outcome(collected, self.state / "tasks" / task, "committed")
        require(data == self.resume_challenge(), "Claude resume did not recover the prior answer context")
        predecessor_reference = predecessor.get("reference")
        require(isinstance(predecessor_reference, dict) and
                is_session_id(predecessor_reference.get("conversation_id")),
                "fresh Claude predecessor session identity is unavailable")
        expected_session = str(predecessor_reference["conversation_id"])
        record = self.validate_task("resume", task, data, predecessor_task, expected_session)
        require(record["reference"]["conversation_id"] == predecessor["reference"]["conversation_id"],
                "Claude resume changed the conversation identity")
        require(original_snapshot == provider_record_snapshot(predecessor["directory"]),
                "Claude resume changed immutable predecessor task records")
        require(workspace_before == snapshot(self.workspace, ignored_prefixes=()),
                "Claude resume changed workspace or sentinel")
        require(self.sibling_before == snapshot(self.sibling, ignored_prefixes=()),
                "Claude resume changed sibling sentinel")
        controls = record.get("resume_controls")
        require(isinstance(controls, dict) and controls.get("tool_free") is True,
                "Claude resume tool-free oracle result is absent")
        record.update({"dispatch": response, "collect": collected})
        self.records["resume"] = record
        write_json(self.output / "resume-control.json", {
            "task_id": task, "predecessor_task_id": predecessor_task,
            "conversation_id": record["reference"]["conversation_id"], "nonce_sha256": sha(self.nonce),
            "resume_challenge_sha256": sha(data), "resume_brief_contains_nonce": False, "tool_free": True,
            "resume_item_count": controls["item_count"], "resume_item_types": controls["item_types"],
            "original_records_unchanged": True, "provider_exit_manifest_sha256": record["seal"]["manifest_sha256"],
            "outcome_sha256": digest(record["directory"] / "outcome.json"), "outcome": outcome,
        })
        return record

    def replay(self) -> None:
        self.ops.root_id = self.root_id
        self.ops.replay(self.records, names=("fresh", "resume"))
        self.queue_before_replay = self.ops.queue_before_replay

    def failure_queue_finished(self, status: dict[str, object]) -> bool:
        return self.ops.failure_queue_finished(status)

    def shutdown(self) -> None:
        self.ops.root_id = self.root_id
        self.ops.bind_supervisor(self.pueue_config, self.daemon)
        self.ops.shutdown()
        self.closed = self.ops.closed

    def safe_failure_shutdown(self) -> bool:
        self.ops.root_id = self.root_id
        if self.pueue_config is not None:
            self.ops.bind_supervisor(self.pueue_config, self.daemon)
        result = self.ops.safe_failure_shutdown()
        self.closed = self.ops.closed
        return result

    def retain_failure_ownership(self) -> bool:
        """Retain active acceptance processes until exact natural cleanup."""
        self.ops.root_id = self.root_id
        if self.pueue_config is not None:
            self.ops.bind_supervisor(self.pueue_config, self.daemon)
        result = self.ops.retain_failure_ownership()
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
                "status": ACCEPTANCE_STATUS, "provider": PROVIDER,
                "native_ai_turns": len(self.tasks),
                "planned_native_ai_turns": PLANNED_NATIVE_AI_TURNS,
                "tasks": {name: {"task_id": self.tasks[name],
                                  "conversation_id": self.records[name]["reference"]["conversation_id"],
                                  "outcome_sha256": digest(self.records[name]["directory"] / "outcome.json"),
                                  "evidence_sha256": self.records[name]["outcome"]["evidence_sha256"]}
                           for name in ("fresh", "resume")},
                "replay_launches": 0, "natural_daemon_shutdown": self.closed, "signals_sent": 0,
            })
            print("PASS Claude acceptance", flush=True)
        except BaseException as error:
            traceback_text = traceback.format_exc()
            diagnostics_written = False
            naturally_shutdown = False
            try:
                try:
                    write_json(self.output / "failure.json", {
                        "status": "BLOCKED" if isinstance(error, BlockedFailure) else "failed",
                        "error": str(error), "traceback": traceback_text,
                        "natural_daemon_shutdown": self.closed, "signals_sent": 0,
                        "unknown_termination_preserved": True, "state": str(self.state),
                        "workspace": str(self.workspace), "tasks": dict(self.tasks),
                        "dispatch_attempts": sorted(self.dispatch_attempts),
                        "daemon_pid": None if self.daemon is None else self.daemon.pid,
                    })
                    diagnostics_written = True
                except BaseException:
                    # Failure diagnostics are best effort; ownership cleanup
                    # must still run if serialization or the write fails.
                    pass
            finally:
                try:
                    naturally_shutdown = self.retain_failure_ownership()
                except BaseException:
                    naturally_shutdown = False
                try:
                    write_json(self.output / "failure-cleanup.json", {
                        "natural_daemon_shutdown": naturally_shutdown,
                        "signals_sent": 0,
                        "owned_processes_joined": naturally_shutdown,
                        "failure_diagnostics_written": diagnostics_written,
                    })
                except BaseException:
                    pass
            raise


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tools", required=True, help="directory containing shipped delegate binaries")
    parser.add_argument("--pueue", required=True)
    parser.add_argument("--pueued", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--claude", default=None, help="installed Claude executable override")
    parser.add_argument("--delegate", default=None, help="delegate executable override")
    parser.add_argument("--runner", default=None, help="delegate-run executable override")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        ClaudeAcceptance(args).run()
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
