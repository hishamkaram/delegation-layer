"""Small, provider-neutral assertions shared by native acceptance drivers.

The supervisor process lifecycle remains in ``acceptance_supervisor_common``.
This module only contains observations and the narrow task operations that are
common to provider acceptance gates, so a provider driver does not have to
import another provider's fixtures or silently copy its oracles.
"""

from __future__ import annotations

from copy import deepcopy
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import pwd
import re
import shlex
import sys
import time
from typing import Callable

from acceptance_supervisor_common import config_for, digest, read_json, sha, write_json


MAX_CONTROL_BYTES = 1 << 20
MAX_PROVIDER_BYTES = 8 << 20
INSPECTION_GROUP_PREFIX = "delegation-inspection-"
INSPECTION_FAILURE_RESULTS = {"Killed", "Errored", "DependencyFailed", "Failed", "FailedToSpawn"}
PUEUE_VERSION = "4.0.4"
STATIC_REFUSAL_ERROR = "unsupported-effective-config"
AUTHENTICATION_REFUSAL_ERROR = "provider authentication unavailable"
_STATIC_REFUSAL_RESPONSE_KEYS = {
    "schema_version", "command", "root_id", "task_id", "admission",
    "liveness", "publication", "error",
}
_SIMPLE_FOLD_TABLE = Path(__file__).resolve().parent / "_data" / "unicode-simple-fold.json"


def _load_simple_fold_table() -> dict[int, int]:
    """Load the Go Unicode SimpleFold minimum-rune translation table."""
    try:
        raw = json.loads(_SIMPLE_FOLD_TABLE.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, TypeError, ValueError) as error:
        raise RuntimeError(f"cannot load Unicode SimpleFold table: {error}") from error
    if not isinstance(raw, dict):
        raise RuntimeError("Unicode SimpleFold table is not an object")
    table: dict[int, int] = {}
    for encoded, minimum in raw.items():
        if (not isinstance(encoded, str) or not encoded.isascii() or not encoded.isdecimal()
                or isinstance(minimum, bool) or not isinstance(minimum, int)):
            raise RuntimeError("Unicode SimpleFold table has an invalid entry")
        codepoint = int(encoded)
        if not 0 <= codepoint <= 0x10FFFF or not 0 <= minimum <= 0x10FFFF:
            raise RuntimeError("Unicode SimpleFold table has an out-of-range entry")
        table[codepoint] = minimum
    return table


_SIMPLE_FOLD_TRANSLATION = _load_simple_fold_table()
_TIMESTAMP_PATTERN = re.compile(
    r"^(?P<date>\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})"
    r"(?:\.(?P<fraction>\d{1,9}))?Z$")
_TASK_REQUIRED_KEYS = {
    "schema_version", "root_id", "task_id", "provider", "mode", "canonical_cwd",
    "requested_config", "budget_nanos", "brief_sha256", "brief_length",
}
_TASK_OPTIONAL_KEYS = {"prior_session"}
_TASK_CONFIG_KEYS = {"model", "effort", "permission", "budget", "native_timeout"}
_PRIOR_SESSION_KEYS = {"provider", "conversation_id", "predecessor_task_id"}
_INSPECTION_BINDING_KEYS = {
    "definition_revision", "definition_sha256", "helper_executable", "helper_sha256",
    "worker_executable", "worker_sha256", "supervisor",
}
_INSPECTION_BINDING_OPTIONAL_KEYS = {"environment"}
_SUPERVISOR_BINDING_KEYS = {
    "client_executable", "client_sha256", "resolved_config_sha256", "endpoint",
    "config_path", "config_digest", "observed_version",
}
_SUPERVISOR_BINDING_OPTIONAL_KEYS = {
    "resolution_os", "resolution_home", "resolution_data_local",
    "resolution_config", "resolution_runtime", "resolution_username",
    # Accepted for records written before recovery stopped using the caller cwd.
    "resolution_cwd",
}
_PUEUE_STATUS_KEYS = {"tasks", "groups"}
_PUEUE_GROUP_KEYS = {"status", "parallel_tasks"}
_PUEUE_ROW_KEYS = {
    "id", "created_at", "original_command", "command", "path", "envs", "group",
    "dependencies", "priority", "label", "status",
}


class AcceptanceFailure(RuntimeError):
    """A provider acceptance oracle could not be established."""


def unique_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    """Decode objects with no exact or Go SimpleFold-equivalent duplicate keys.

    Go's registered provider decoders reject aliases such as ``session_id`` and
    ``Session_ID`` at every nesting level.  This uses the minimum rune from
    each Go ``unicode.SimpleFold`` cycle, rather than Python's full Unicode
    casefold, so multi-rune folds such as ``ß`` → ``ss`` remain distinct.
    Keeping this hook shared makes the Python acceptance parsers enforce the
    same fail-closed rule before provider-specific field selection.
    """
    result: dict[str, object] = {}
    folded: dict[str, str] = {}
    for key, value in pairs:
        normalized = key.translate(_SIMPLE_FOLD_TRANSLATION)
        require(normalized not in folded,
                "duplicate JSON object key: " + key)
        folded[normalized] = key
        result[key] = value
    return result


def _read_strict_json(path: Path, bound: int) -> object:
    """Read bounded JSON with the same alias rejection as the Go decoder."""
    try:
        with Path(path).open("rb") as stream:
            data = stream.read(bound + 1)
    except OSError:
        raise
    require(len(data) <= bound, "JSON evidence exceeds bound: " + str(path))

    def reject_constant(value: str) -> object:
        raise ValueError("non-standard JSON constant: " + value)

    try:
        return json.loads(data, object_pairs_hook=unique_object,
                          parse_constant=reject_constant)
    except (json.JSONDecodeError, UnicodeDecodeError, RecursionError, TypeError, ValueError) as error:
        raise AcceptanceFailure("invalid JSON evidence: " + str(path) + ": " + str(error)) from error


def _exact_keys(value: dict[str, object], required: set[str], optional: set[str] = frozenset(),
                label: str = "record") -> None:
    require(isinstance(value, dict), f"{label} is not an object")
    keys = set(value)
    require(required <= keys and keys <= required | optional,
            f"{label} has unknown or missing fields")


def _canonical_timestamp(value: object, label: str) -> tuple[datetime, int]:
    """Parse the canonical UTC RFC3339Nano form used by Go records."""
    require(isinstance(value, str), f"{label} is not a timestamp")
    match = _TIMESTAMP_PATTERN.fullmatch(value)
    require(match is not None, f"{label} is not canonical UTC RFC3339Nano")
    fraction = match.group("fraction") or ""
    # RFC3339Nano omits insignificant trailing zeroes in a fractional part.
    require(not fraction or not fraction.endswith("0"),
            f"{label} is not canonical UTC RFC3339Nano")
    try:
        parsed = datetime.strptime(match.group("date"), "%Y-%m-%dT%H:%M:%S")
    except ValueError as error:
        raise AcceptanceFailure(f"{label} is not a valid timestamp") from error
    nanoseconds = int(fraction.ljust(9, "0")) if fraction else 0
    parsed = parsed.replace(microsecond=nanoseconds // 1000, tzinfo=timezone.utc)
    return parsed, nanoseconds % 1000


def _canonical_task_bytes(value: dict[str, object]) -> bytes:
    """Marshal a TaskRecord in the field order and escaping used by Go."""
    _exact_keys(value, _TASK_REQUIRED_KEYS, _TASK_OPTIONAL_KEYS, "inspection task")
    config = value["requested_config"]
    require(isinstance(config, dict), "inspection task requested_config is not an object")
    _exact_keys(config, set(), _TASK_CONFIG_KEYS, "inspection task requested_config")
    canonical_config = {
        key: config[key] for key in ("model", "effort", "permission", "budget", "native_timeout")
        if config.get(key) not in (None, "")
    }
    prior = value.get("prior_session")
    canonical_prior = None
    if prior is not None:
        require(isinstance(prior, dict), "inspection task prior_session is not an object")
        _exact_keys(prior, _PRIOR_SESSION_KEYS, label="inspection task prior_session")
        require(all(isinstance(prior[key], str) and prior[key] for key in _PRIOR_SESSION_KEYS),
                "inspection task prior_session has invalid fields")
        canonical_prior = {key: prior[key] for key in (
            "provider", "conversation_id", "predecessor_task_id")}
    canonical = {
        "schema_version": value["schema_version"],
        "root_id": value["root_id"],
        "task_id": value["task_id"],
        "provider": value["provider"],
        "mode": value["mode"],
        "canonical_cwd": value["canonical_cwd"],
        "requested_config": canonical_config,
        "budget_nanos": value["budget_nanos"],
    }
    if canonical_prior is not None:
        canonical["prior_session"] = canonical_prior
    canonical["brief_sha256"] = value["brief_sha256"]
    canonical["brief_length"] = value["brief_length"]
    try:
        encoded = json.dumps(canonical, ensure_ascii=False, separators=(",", ":"),
                             allow_nan=False).encode("utf-8")
    except (TypeError, UnicodeEncodeError, ValueError) as error:
        raise AcceptanceFailure("inspection task cannot be canonically encoded") from error
    # encoding/json escapes HTML-sensitive bytes and the two line-separator
    # runes even when it otherwise emits UTF-8 rather than \u escapes.
    encoded = (encoded.replace(b"&", b"\\u0026").replace(b"<", b"\\u003c")
               .replace(b">", b"\\u003e").replace("\u2028".encode(), b"\\u2028")
               .replace("\u2029".encode(), b"\\u2029"))
    return encoded + b"\n"


def _validate_task_record(value: object, root: str, task: str, label: str) -> str:
    """Check the bounded TaskRecord shape and return its canonical digest."""
    require(isinstance(value, dict), f"{label} is not a task record")
    _exact_keys(value, _TASK_REQUIRED_KEYS, _TASK_OPTIONAL_KEYS, label)
    require(type(value.get("schema_version")) is int and value["schema_version"] == 1,
            f"{label} schema mismatch")
    require(value.get("root_id") == root and value.get("task_id") == task,
            f"{label} identity mismatch")
    for key in ("provider", "mode", "canonical_cwd"):
        require(isinstance(value.get(key), str) and bool(value[key]),
                f"{label} {key} is invalid")
    cwd = value["canonical_cwd"]
    require(Path(cwd).is_absolute() and Path(cwd) == Path(os.path.normpath(cwd)),
            f"{label} canonical_cwd is invalid")
    budget = value.get("budget_nanos")
    require(type(budget) is int and budget > 0, f"{label} budget_nanos is invalid")
    brief_length = value.get("brief_length")
    require(type(brief_length) is int and 0 < brief_length <= 8 * 1024 * 1024,
            f"{label} brief_length is invalid")
    require(_hex_digest(value.get("brief_sha256")), f"{label} brief digest is invalid")
    config = value.get("requested_config")
    require(isinstance(config, dict), f"{label} requested_config is invalid")
    _exact_keys(config, set(), _TASK_CONFIG_KEYS, f"{label} requested_config")
    require(all(isinstance(item, str) and item != "" for item in config.values()),
            f"{label} requested_config has invalid fields")
    prior = value.get("prior_session")
    if prior is not None:
        require(isinstance(prior, dict), f"{label} prior_session is invalid")
        _exact_keys(prior, _PRIOR_SESSION_KEYS, label=f"{label} prior_session")
        require(all(isinstance(item, str) and item for item in prior.values()),
                f"{label} prior_session has invalid fields")
    return sha(_canonical_task_bytes(value))


def require(condition: object, message: str) -> None:
    if not condition:
        raise AcceptanceFailure(message)


def canonical_go_json(value: object, newline: bool = True) -> bytes:
    """Encode bounded acceptance records with Go encoding/json semantics."""
    try:
        data = json.dumps(value, ensure_ascii=False, separators=(",", ":"),
                          allow_nan=False).encode("utf-8")
    except (TypeError, UnicodeEncodeError, ValueError) as error:
        raise AcceptanceFailure("record cannot be canonically encoded") from error
    data = (data.replace(b"&", b"\\u0026").replace(b"<", b"\\u003c")
            .replace(b">", b"\\u003e").replace("\u2028".encode(), b"\\u2028")
            .replace("\u2029".encode(), b"\\u2029"))
    return data + (b"\n" if newline else b"")


def resolved_supervisor_config(base: Path) -> dict[str, object]:
    """Map the trusted pueue config source into ResolvedConfig field order."""
    source = config_for(Path(base))
    client = source["client"]
    daemon = source["daemon"]
    shared = source["shared"]
    require(isinstance(client, dict) and isinstance(daemon, dict) and
            isinstance(shared, dict), "supervisor config source is malformed")
    env_vars = daemon["env_vars"]
    require(isinstance(env_vars, dict), "supervisor environment source is malformed")
    return {
        "Client": {
            "RestartInPlace": client["restart_in_place"],
            "ReadLocalLogs": client["read_local_logs"],
            "ShowConfirmationQuestions": client["show_confirmation_questions"],
            "EditMode": client["edit_mode"],
            "ShowExpandedAliases": client["show_expanded_aliases"],
            "DarkMode": client["dark_mode"],
            "MaxStatusLines": client["max_status_lines"],
            "StatusTimeFormat": client["status_time_format"],
            "StatusDatetimeFormat": client["status_datetime_format"],
        },
        "Daemon": {
            "PauseGroupOnFailure": daemon["pause_group_on_failure"],
            "PauseAllOnFailure": daemon["pause_all_on_failure"],
            "CompressStateFile": daemon["compress_state_file"],
            "Callback": daemon["callback"],
            "EnvVars": {key: env_vars[key] for key in sorted(env_vars)},
            "CallbackLogLines": daemon["callback_log_lines"],
            "ShellCommand": daemon["shell_command"],
        },
        "PueueDirectory": shared["pueue_directory"],
        "RuntimeDirectory": shared["runtime_directory"],
        "AliasFile": shared["alias_file"],
        "SocketPath": shared["unix_socket_path"],
        "PIDPath": shared["pid_path"],
        "SecretPath": shared["shared_secret_path"],
        "CertificatePath": shared["daemon_cert"],
        "KeyPath": shared["daemon_key"],
        "SocketPermissions": shared["unix_socket_permissions"],
        "Host": shared["host"],
        "Port": shared["port"],
    }


def resolved_supervisor_config_digest(base: Path) -> str:
    """Hash ResolvedConfig exactly as pueue does, without a record newline."""
    return sha(canonical_go_json(resolved_supervisor_config(base), newline=False))


def supervisor_binding(pueue: Path, config: Path, base: Path,
                       config_digest: str) -> dict[str, object]:
    """Build a supervisor binding from harness-owned executable/config sources."""
    base = Path(base).resolve()
    config = Path(config).resolve()
    pueue = Path(pueue).resolve()
    binding = {
        "client_executable": str(pueue),
        "client_sha256": digest(pueue),
        "resolved_config_sha256": resolved_supervisor_config_digest(base),
        "endpoint": "unix:" + str(base / "run" / "p.sock"),
        "config_path": str(config),
        "config_digest": config_digest,
        "observed_version": "pueue " + PUEUE_VERSION,
    }
    binding.update(supervisor_resolution())
    return binding


def supervisor_resolution() -> dict[str, str]:
    """Mirror the Go supervisor path selectors used by acceptance processes."""
    environment = {
        key: value for key, value in os.environ.items()
        if key in {"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR"}
    }
    home = os.path.normpath(environment.get("HOME") or pwd.getpwuid(os.getuid()).pw_dir)
    if sys.platform == "linux":
        operating_system = "linux"
        data_local = _absolute_environment_or(
            environment, "XDG_DATA_HOME", os.path.join(home, ".local", "share"))
        config = _absolute_environment_or(
            environment, "XDG_CONFIG_HOME", os.path.join(home, ".config"))
        runtime = _absolute_environment_or(environment, "XDG_RUNTIME_DIR", "")
    elif sys.platform == "darwin":
        operating_system = "darwin"
        data_local = os.path.normpath(os.path.join(home, "Library", "Application Support"))
        config = data_local
        runtime = ""
    else:
        raise AcceptanceFailure("unsupported acceptance platform: " + sys.platform)
    result = {
        "resolution_os": operating_system,
        "resolution_home": home,
        "resolution_data_local": data_local,
        "resolution_config": config,
        "resolution_username": pwd.getpwuid(os.getuid()).pw_name,
    }
    if runtime:
        result["resolution_runtime"] = runtime
    return result


def _absolute_environment_or(environment: dict[str, str], key: str, fallback: str) -> str:
    value = environment.get(key, "")
    if value and os.path.isabs(value):
        return os.path.normpath(value)
    return os.path.normpath(fallback) if fallback else ""


def verify_collected_outcome(collected: object, directory: Path, expected: str) -> tuple[dict[str, object], bytes]:
    """Bind a public collection response to the immutable outcome and bytes."""
    require(isinstance(collected, dict), "collection response is not an object")
    outcome = read_json(directory / "outcome.json")
    require(isinstance(outcome, dict), "sole outcome authority is not an object")
    require(collected.get("outcome") == outcome, "CLI and sole outcome authority disagree")
    require(outcome.get("verdict") == expected, "collected outcome mismatch")
    return outcome, read_outcome_payload(directory, outcome)


def read_outcome_payload(directory: Path, outcome: dict[str, object]) -> bytes:
    """Read only the protocol's regular task-local publication file."""
    names = {"committed": "result.txt", "rejected": "publish.reject"}
    expected = names.get(outcome.get("verdict"))
    require(expected is not None, "outcome verdict has no publication payload")
    payload = outcome.get("payload")
    require(isinstance(payload, dict), "outcome payload descriptor is absent")
    require(payload.get("basename") == expected, "outcome payload basename is invalid")
    length = payload.get("length")
    require(type(length) is int and length >= 0, "outcome payload length is invalid")
    path = directory / expected
    require(directory.is_dir() and not directory.is_symlink() and
            path.is_file() and not path.is_symlink(),
            "outcome payload is not a regular task-local file")
    data = path.read_bytes()
    require(len(data) == length and sha(data) == payload.get("sha256"),
            "payload digest mismatch")
    return data


def clean_absolute(value: object, label: str) -> Path:
    require(isinstance(value, str) and bool(value), f"{label} must be a nonempty path")
    path = Path(value)
    require(path.is_absolute() and path == Path(os.path.normpath(value)),
            f"{label} must be absolute and clean")
    return path


def reject_tmp(path: Path, label: str) -> None:
    resolved = path.resolve(strict=False)
    excluded = [Path(item).resolve(strict=False)
                for item in ("/tmp", "/var/tmp", "/var/folders", "/dev")]
    require(not any(resolved == root or root in resolved.parents for root in excluded),
            f"{label} must be outside provider temporary roots: {resolved}")


def path_is_within(path: Path, parent: Path) -> bool:
    child = path.resolve(strict=False)
    ancestor = parent.resolve(strict=False)
    return child == ancestor or ancestor in child.parents


def ensure_private_directory(path: Path, label: str, create: bool = False) -> Path:
    reject_tmp(path, label)
    if create:
        path.mkdir(mode=0o700, parents=True, exist_ok=False)
    require(path.is_dir() and not path.is_symlink(), f"{label} is not a private directory: {path}")
    os.chmod(path, 0o700)
    require(path.stat().st_mode & 0o077 == 0, f"{label} is group/world accessible: {path}")
    return path.resolve()


def write_bytes(path: Path, data: bytes) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    with path.open("xb") as stream:
        stream.write(data)
        stream.flush()
        os.fsync(stream.fileno())
    os.chmod(path, 0o600)


def no_prompt_argv(argv: list[object], briefs: list[Path], forbidden: tuple[str, ...] = ()) -> None:
    """Prove finite prompts travel through files/stdin, never child argv."""
    strings = [str(value) for value in argv]
    joined = "\x00".join(strings)
    for brief in briefs:
        content = brief.read_bytes().decode("utf-8", errors="replace")
        require(content not in joined, "brief content leaked into child argv")
    for value in forbidden:
        require(value not in strings, "forbidden provider argument leaked into child argv: " + value)


def parse_json_output(process: object, label: str, bound: int = MAX_CONTROL_BYTES) -> dict[str, object]:
    """Read one bounded JSON object from an owned process's stdout."""
    try:
        value = _read_strict_json(process.directory / "stdout", bound)
    except (OSError, ValueError, TypeError, RecursionError) as error:
        raise AcceptanceFailure(f"{label} did not produce one JSON object: {error}") from error
    require(isinstance(value, dict), f"{label} JSON response must be an object")
    return value


def status_jobs(process: object) -> dict[str, object]:
    """Read one bounded pueue status object from an owned client."""
    result = getattr(process, "result", None)
    require(isinstance(result, dict) and result.get("natural_wait") is True and
            type(result.get("exit_code")) is int and result.get("exit_code") == 0,
            "pueue status did not exit successfully")
    value = parse_json_output(process, "pueue status", MAX_PROVIDER_BYTES)
    _validate_pueue_status(value)
    return value


def row_state(row: dict[str, object]) -> str:
    value = row.get("status")
    require(isinstance(value, dict) and len(value) == 1,
            "pueue row status is not a one-state object")
    return next(iter(value))


def done_result(row: dict[str, object]) -> str | None:
    """Return a recognized pueue terminal result, if the row is Done."""
    status = row.get("status")
    if not isinstance(status, dict) or set(status) != {"Done"}:
        return None
    done = status.get("Done")
    if not isinstance(done, dict):
        return None
    result = done.get("result")
    if isinstance(result, str) and result in {"Success", "Killed", "Errored", "DependencyFailed"}:
        return result
    if not isinstance(result, dict) or len(result) != 1:
        return None
    name, value = next(iter(result.items()))
    if name == "Failed" and isinstance(value, int) and not isinstance(value, bool):
        return name
    if name == "FailedToSpawn" and isinstance(value, str):
        return name
    return None


def _validate_pueue_timestamp(value: object, label: str) -> None:
    require(isinstance(value, str) and value, f"pueue {label} is invalid")
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as error:
        raise AcceptanceFailure(f"pueue {label} is invalid") from error
    require(parsed.tzinfo is not None, f"pueue {label} has no timezone")


def _validate_pueue_state(value: object, label: str, depth: int = 0) -> None:
    require(isinstance(value, dict) and len(value) == 1, f"pueue {label} state is invalid")
    require(depth <= 32, f"pueue {label} state nesting is excessive")
    name, state = next(iter(value.items()))
    if name == "Queued":
        _exact_keys(state, {"enqueued_at"}, label=f"pueue {label} queued state")
        _validate_pueue_timestamp(state["enqueued_at"], f"{label} enqueued_at")
    elif name in {"Running", "Paused"}:
        _exact_keys(state, {"enqueued_at", "start"}, label=f"pueue {label} running state")
        _validate_pueue_timestamp(state["enqueued_at"], f"{label} enqueued_at")
        _validate_pueue_timestamp(state["start"], f"{label} start")
    elif name == "Stashed":
        _exact_keys(state, {"enqueue_at"}, label=f"pueue {label} stashed state")
        if state["enqueue_at"] is not None:
            _validate_pueue_timestamp(state["enqueue_at"], f"{label} enqueue_at")
    elif name == "Done":
        _exact_keys(state, {"enqueued_at", "start", "end", "result"},
                    label=f"pueue {label} done state")
        for key in ("enqueued_at", "start", "end"):
            _validate_pueue_timestamp(state[key], f"{label} {key}")
        result = state["result"]
        if isinstance(result, str):
            require(result in {"Success", "Killed", "Errored", "DependencyFailed"},
                    f"pueue {label} result is unknown")
        else:
            require(isinstance(result, dict) and len(result) == 1,
                    f"pueue {label} result is invalid")
            variant, detail = next(iter(result.items()))
            if variant == "Failed":
                require(type(detail) is int, f"pueue {label} failure result is invalid")
            elif variant == "FailedToSpawn":
                require(isinstance(detail, str), f"pueue {label} spawn result is invalid")
            else:
                raise AcceptanceFailure(f"pueue {label} result is unknown")
    elif name == "Locked":
        _exact_keys(state, {"previous_status"}, label=f"pueue {label} locked state")
        _validate_pueue_state(state["previous_status"], label, depth + 1)
    else:
        raise AcceptanceFailure(f"pueue {label} state is unknown")


def _validate_pueue_status(value: dict[str, object]) -> None:
    _exact_keys(value, _PUEUE_STATUS_KEYS, label="pueue status")
    tasks = value["tasks"]
    groups = value["groups"]
    require(isinstance(tasks, dict), "pueue status JSON has no tasks object")
    require(isinstance(groups, dict), "pueue status JSON has no groups object")
    for name, group in groups.items():
        require(isinstance(name, str), "pueue group name is invalid")
        require(isinstance(group, dict), f"pueue group {name} is not an object")
        _exact_keys(group, _PUEUE_GROUP_KEYS, label=f"pueue group {name}")
        require(group["status"] in {"Running", "Paused", "Reset"},
                f"pueue group {name} status is invalid")
        require(type(group["parallel_tasks"]) is int and group["parallel_tasks"] >= 0,
                f"pueue group {name} parallelism is invalid")
    for key, row in tasks.items():
        require(isinstance(key, str) and key.isdecimal() and str(int(key)) == key,
                "pueue task map key is not canonical")
        require(isinstance(row, dict), f"pueue task {key} is not an object")
        _exact_keys(row, _PUEUE_ROW_KEYS, label=f"pueue task {key}")
        require(type(row["id"]) is int and row["id"] >= 0 and row["id"] == int(key),
                f"pueue task {key} numeric identity is invalid")
        _validate_pueue_timestamp(row["created_at"], f"task {key} created_at")
        for field in ("original_command", "command", "path", "group"):
            require(isinstance(row[field], str), f"pueue task {key} {field} is invalid")
        envs = row["envs"]
        require(isinstance(envs, dict) and all(isinstance(k, str) and isinstance(v, str)
                                                for k, v in envs.items()),
                f"pueue task {key} envs is invalid")
        dependencies = row["dependencies"]
        require(isinstance(dependencies, list) and all(type(item) is int and item >= 0
                                                       for item in dependencies),
                f"pueue task {key} dependencies are invalid")
        require(type(row["priority"]) is int, f"pueue task {key} priority is invalid")
        require(row["label"] is None or isinstance(row["label"], str),
                f"pueue task {key} label is invalid")
        _validate_pueue_state(row["status"], f"task {key}")


def validate_queue_row(row: dict[str, object], label: str, runner: Path,
                       state: Path, task: str, numeric: int | None = None,
                       group: str | None = None) -> None:
    """Bind a pueue status row to one exact runner/state/task invocation."""
    require(row.get("label") == label, f"pueue label mismatch for {task}")
    for key in ("original_command", "command", "path"):
        require(isinstance(row.get(key), str), f"pueue row missing {key} for {task}")
    if numeric is not None:
        require(type(row.get("id")) is int and row["id"] == numeric,
                f"pueue numeric identity mismatch for {task}")
    if group is not None:
        require(row.get("group") == group, f"pueue group mismatch for {task}")
    expected = [str(runner), "--root", str(state), task]
    commands = []
    for key in ("original_command", "command"):
        try:
            commands.append(shlex.split(row[key]))
        except ValueError as error:
            raise AcceptanceFailure(f"pueue row command is invalid for {task}") from error
    require(commands[0] == expected and commands[1] == expected,
            f"pueue row does not bind exact runner/state/task argv for {task}")


def _hex_digest(value: object) -> bool:
    return (isinstance(value, str) and len(value) == 64 and
            all(character in "0123456789abcdef" for character in value))


def _task_identity(value: object) -> bool:
    return (isinstance(value, str) and len(value) == 32 and
            all(character in "0123456789abcdef" for character in value))


def _inspection_group(root_id: str) -> str:
    return INSPECTION_GROUP_PREFIX + root_id


def _inspection_label(root_id: str, task_id: str) -> str:
    return _inspection_group(root_id) + "-" + task_id


def _validate_supervisor_binding(value: object, label: str) -> None:
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, _SUPERVISOR_BINDING_KEYS, _SUPERVISOR_BINDING_OPTIONAL_KEYS, label)
    for key in ("client_executable", "config_path"):
        path = value.get(key)
        require(isinstance(path, str) and Path(path).is_absolute() and
                Path(path) == Path(os.path.normpath(path)),
                f"{label} {key} is invalid")
    for key in ("client_sha256", "resolved_config_sha256", "config_digest"):
        require(_hex_digest(value.get(key)), f"{label} {key} is invalid")
    require(isinstance(value.get("endpoint"), str) and value["endpoint"],
            f"{label} endpoint is invalid")
    require(value.get("observed_version") == "pueue " + PUEUE_VERSION,
            f"{label} version is unsupported")
    resolution_keys = _SUPERVISOR_BINDING_OPTIONAL_KEYS & set(value)
    if resolution_keys == {"resolution_cwd"}:
        path = value["resolution_cwd"]
        require(isinstance(path, str) and Path(path).is_absolute() and
                Path(path) == Path(os.path.normpath(path)),
                f"{label} resolution_cwd is invalid")
        return
    if resolution_keys:
        required = {"resolution_os", "resolution_home", "resolution_data_local",
                    "resolution_config", "resolution_username"}
        require(required <= resolution_keys, f"{label} resolution is incomplete")
        require(value["resolution_os"] in {"linux", "darwin"},
                f"{label} resolution operating system is invalid")
        for key in ("resolution_home", "resolution_data_local", "resolution_config",
                    "resolution_runtime", "resolution_cwd"):
            if key in value:
                path = value[key]
                require(isinstance(path, str) and Path(path).is_absolute() and
                        Path(path) == Path(os.path.normpath(path)),
                        f"{label} {key} is invalid")
        username = value["resolution_username"]
        require(isinstance(username, str) and username and
                "/" not in username and "\\" not in username and "\x00" not in username,
                f"{label} resolution username is invalid")


def _validate_inspection_binding(value: object, label: str) -> None:
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, _INSPECTION_BINDING_KEYS, _INSPECTION_BINDING_OPTIONAL_KEYS, label)
    require(isinstance(value.get("definition_revision"), str) and
            value["definition_revision"], f"{label} definition revision is invalid")
    for key in ("definition_sha256", "helper_sha256", "worker_sha256"):
        require(_hex_digest(value.get(key)), f"{label} {key} is invalid")
    for key in ("helper_executable", "worker_executable"):
        path = value.get(key)
        require(isinstance(path, str) and Path(path).is_absolute() and
                Path(path) == Path(os.path.normpath(path)),
                f"{label} {key} is invalid")
    if "environment" in value:
        environment = value["environment"]
        require(isinstance(environment, list) and
                all(isinstance(entry, str) and "=" in entry and
                    entry.split("=", 1)[0] for entry in environment),
                f"{label} environment is invalid")
    _validate_supervisor_binding(value.get("supervisor"), label + " supervisor")


def _validate_claim_record(value: object, request_digest: str, label: str,
                           created: tuple[datetime, int], deadline: tuple[datetime, int]) -> tuple[datetime, int]:
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, {"schema_version", "request_sha256", "created_at"}, label=label)
    require(type(value.get("schema_version")) is int and value["schema_version"] == 1 and
            value.get("request_sha256") == request_digest,
            f"{label} binding mismatch")
    claimed = _canonical_timestamp(value.get("created_at"), label + " created_at")
    require(_timestamp_at_or_after(claimed, created) and _timestamp_before(claimed, deadline),
            f"{label} is outside inspection admission window")
    return claimed


def _timestamp_at_or_after(left: tuple[datetime, int], right: tuple[datetime, int]) -> bool:
    return left[0] > right[0] or (left[0] == right[0] and left[1] >= right[1])


def _timestamp_before(left: tuple[datetime, int], right: tuple[datetime, int]) -> bool:
    return left[0] < right[0] or (left[0] == right[0] and left[1] < right[1])


def _read_control_record(path: Path, label: str) -> dict[str, object]:
    require(path.is_file() and not path.is_symlink(), f"{label} is not a regular record")
    require(path.stat().st_size <= MAX_CONTROL_BYTES, f"{label} exceeds the control bound")
    value = _read_strict_json(path, MAX_CONTROL_BYTES)
    require(isinstance(value, dict), f"{label} is not an object")
    return value


def _read_optional_control_record(path: Path, label: str) -> dict[str, object] | None:
    if not path.exists() and not path.is_symlink():
        return None
    return _read_control_record(path, label)


def pueue_command(pueue: Path, config: Path, *args: str) -> list[object]:
    return [str(pueue), "-c", str(config), *args]


def canonical_queue(value: dict[str, object]) -> dict[str, object]:
    """Keep the complete private queue snapshot, including groups."""
    tasks = value.get("tasks")
    groups = value.get("groups")
    require(isinstance(tasks, dict) and isinstance(groups, dict), "queue snapshot is malformed")
    return {"tasks": deepcopy(tasks), "groups": deepcopy(groups)}


def failure_queue_finished(status: dict[str, object], dispatch_attempts: set[str],
                           tasks: dict[str, str], labels: dict[str, str],
                           runner: Path, state: Path,
                           numbers: dict[str, int]) -> bool:
    """Return true only for a complete, positively observed private queue."""
    rows = status.get("tasks")
    groups = status.get("groups")
    if not isinstance(rows, dict) or not isinstance(groups, dict):
        return False
    if set(groups) - {"default"}:
        return False
    if "default" in groups:
        try:
            _exact_keys(groups["default"], _PUEUE_GROUP_KEYS, label="private queue default group")
            require(groups["default"].get("status") in {"Running", "Paused", "Reset"} and
                    type(groups["default"].get("parallel_tasks")) is int and
                    groups["default"]["parallel_tasks"] >= 0,
                    "private queue default group is malformed")
        except (AcceptanceFailure, KeyError, TypeError):
            return False
    if not dispatch_attempts:
        return not rows
    expected: dict[str, tuple[str, str]] = {}
    for name, task in tasks.items():
        if task not in dispatch_attempts:
            return False
        label = labels.get(name)
        if not isinstance(label, str) or label in expected:
            return False
        expected[label] = (name, task)
    if len(expected) != len(dispatch_attempts) or {
            task for _, task in expected.values()} != dispatch_attempts:
        return False
    if len(rows) != len(expected):
        return False
    seen_labels: set[str] = set()
    for key, row in rows.items():
        if not isinstance(row, dict):
            return False
        if (not isinstance(key, str) or not key.isdecimal() or str(int(key)) != key or
                type(row.get("id")) is not int or row["id"] < 0 or row["id"] != int(key)):
            return False
        label = row.get("label")
        if not isinstance(label, str) or label not in expected or label in seen_labels:
            return False
        seen_labels.add(label)
        name, task = expected[label]
        if name not in numbers or type(numbers[name]) is not int or numbers[name] < 0:
            return False
        numeric = numbers[name]
        try:
            validate_queue_row(row, label, runner, state, task, numeric, "default")
            state_name = row_state(row)
        except (AcceptanceFailure, KeyError):
            return False
        if state_name != "Done" or done_result(row) is None:
            return False
    return seen_labels == set(expected)


class NativeTaskOps:
    """Small pueue/task lifecycle used by the Codex and Claude gates.

    The provider drivers still own their profiles, event parsers, and positive
    controls.  This object only owns the repeated supervisor observations and
    immutable collection/replay checks.  In particular it never starts a
    provider process itself: the shipped delegate remains the only launcher.
    """

    def __init__(self, processes: object, delegate: Path, runner: Path,
                 pueue: Path, state_parent: Path, state: Path, output: Path,
                 watch_seconds: float = 150,
                 expected_inspection_binding_required: bool = True) -> None:
        self.processes = processes
        self.delegate = Path(delegate)
        self.runner = Path(runner)
        self.pueue = Path(pueue)
        self.state_parent = Path(state_parent)
        self.state = Path(state)
        self.output = Path(output)
        self.watch_seconds = watch_seconds
        require(type(expected_inspection_binding_required) is bool,
                "expected inspection binding mode is invalid")
        self.expected_inspection_binding_required = expected_inspection_binding_required
        self.pueue_config: Path | None = None
        self.daemon: object | None = None
        self.root_id: str | None = None
        self.root_created_at: tuple[datetime, int] | None = None
        self.tasks: dict[str, str] = {}
        self.labels: dict[str, str] = {}
        self.numbers: dict[str, int] = {}
        self.dispatch_attempts: set[str] = set()
        self.dispatch_observations: list[dict[str, object]] = []
        self.inspection_attempts: set[str] = set()
        self.inspection_bindings: dict[str, dict[str, object]] = {}
        self.queue_before_replay: dict[str, object] | None = None
        self.closed = False

    def bind_supervisor(self, config: Path, daemon: object | None = None) -> None:
        self.pueue_config = Path(config)
        if daemon is not None:
            self.daemon = daemon

    def expect_inspection(self, task_id: str,
                          binding: dict[str, object] | None = None) -> None:
        """Declare an inspection expected for a task before dispatch."""
        require(_task_identity(task_id), "inspection task identity is invalid")
        require(not self.closed, "acceptance supervisor is closed")
        if task_id in self.dispatch_attempts:
            raise AcceptanceFailure("inspection expectation must precede dispatch")
        existing = self.inspection_bindings.get(task_id)
        if binding is not None:
            _validate_inspection_binding(binding, "expected inspection binding")
            if existing is not None:
                require(binding == existing,
                        "expected inspection binding was re-registered with different source")
            else:
                self.inspection_bindings[task_id] = deepcopy(binding)
        else:
            require(existing is not None, "expected inspection binding is required")
        self.inspection_attempts.add(task_id)

    def direct(self, name: str, argv: list[object], expected: int | set[int] = 0,
               timeout: float = 30) -> object:
        require(not self.closed, "acceptance supervisor is closed")
        return self.processes.run(name, argv, self.state_parent,
                                  expected=expected, timeout=timeout)

    def client(self, name: str, operation: list[str], expected: int | set[int] = 0,
               timeout: float = 30) -> object:
        require(self.pueue_config is not None, "pueue is not configured")
        return self.direct(name, pueue_command(self.pueue, self.pueue_config, *operation),
                           expected, timeout)

    def dispatch(self, name: str, task: str, argv: list[object]) -> dict[str, object]:
        """Admit one exact delegate task and bind its supervisor identity."""
        require(not self.closed, "acceptance supervisor is closed")
        require(task not in self.dispatch_attempts, "duplicate dispatch attempt")
        self.dispatch_attempts.add(task)
        process = self.processes.run(name, argv, self.state_parent,
                                     expected=None, timeout=45)
        try:
            response = parse_json_output(process, name)
        except BaseException:
            self._record_dispatch_observation(name, task, process, None)
            raise
        if response.get("admission") == "admitted":
            # Retain a valid positive identity before writing the auxiliary
            # dispatch-attempt receipt.  A receipt failure must not discard
            # the queue authority needed for natural failure cleanup.
            try:
                admitted = self.record_admission(name, task, response)
            except BaseException:
                self._record_dispatch_observation(name, task, process, response)
                raise
            self._record_dispatch_observation(name, task, process, response)
            result = getattr(process, "result", None)
            require(isinstance(result, dict) and result.get("natural_wait") is True and
                    type(result.get("exit_code")) is int and result.get("exit_code") == 0,
                    f"{name} dispatch did not exit successfully")
            return admitted
        self._record_dispatch_observation(name, task, process, response)
        return self.record_admission(name, task, response)

    def _record_dispatch_observation(self, name: str, task: str, process: object,
                                     response: dict[str, object] | None) -> None:
        result = getattr(process, "result", None)
        observation = {
            "name": name,
            "task_id": task,
            "exit_code": result.get("exit_code") if isinstance(result, dict) else None,
            "natural_wait": result.get("natural_wait") if isinstance(result, dict) else None,
            "response": response,
            "receipt_written": False,
        }
        self.dispatch_observations.append(observation)
        write_json(self.output / (name + "-dispatch-attempt.json"),
                   {**observation, "receipt_written": True})
        observation["receipt_written"] = True

    def record_admission(self, name: str, task: str, response: dict[str, object]) -> dict[str, object]:
        """Retain a positively observed admission, including concurrent callers."""
        require(task in self.dispatch_attempts, "admission has no declared dispatch attempt")
        require(response.get("admission") == "admitted", f"{name} was not admitted")
        require(response.get("task_id") == task, f"{name} task identity mismatch")
        root = response.get("root_id")
        require(isinstance(root, str) and root, f"{name} omitted a root identity")
        if self.root_id is None:
            self.root_id = root
        require(self.root_id == root, f"{name} changed root identity")
        supervisor = response.get("supervisor")
        require(isinstance(supervisor, dict) and supervisor.get("matched") is True,
                f"{name} has no positive supervisor admission")
        numeric = supervisor.get("numeric_task_id")
        require(isinstance(numeric, int) and not isinstance(numeric, bool) and numeric >= 0,
                f"{name} has no numeric pueue identity")
        require(name not in self.tasks and task not in self.tasks.values(),
                f"{name} duplicated an admitted task")
        self.tasks[name] = task
        self.labels[name] = f"delegate:{root}:{task}"
        self.numbers[name] = numeric
        write_json(self.output / (name + "-dispatch.json"), response)
        return response

    def wait_task(self, name: str, task: str) -> dict[str, object]:
        require(self.root_id is not None, "root identity is unavailable")
        row = wait_runner_done(self.client, self.root_id, task,
                               timeout=self.watch_seconds,
                               sleep_fn=time.sleep, monotonic_fn=time.monotonic)
        require(isinstance(row, dict), "runner completion row is malformed")
        require(name in self.numbers, f"runner task {task} has no recorded numeric identity")
        validate_queue_row(row, self.labels[name], self.runner, self.state, task,
                           self.numbers[name], "default")
        write_json(self.output / (name + "-done.json"), row)
        return row

    def collect(self, name: str, task: str) -> dict[str, object]:
        require(self.pueue_config is not None, "pueue is not configured")
        process = self.direct(name, [self.delegate, "--root", self.state,
                                     "--pueue-config", self.pueue_config,
                                     "--runner", self.runner, "collect", task,
                                     "--watch", "0s", "--json"], timeout=45)
        response = parse_json_output(process, name)
        require(response.get("task_id") == task, f"{name} collected the wrong task")
        write_json(self.output / (name + ".json"), response)
        return response

    def queue_status(self, name: str) -> dict[str, object]:
        status = status_jobs(self.client(name, ["status", "--json"], timeout=20))
        write_json(self.output / (name + ".json"), status)
        return status

    def _known_root_id(self) -> str:
        root = _read_control_record(self.state / "root.json", "inspection root record")
        _exact_keys(root, {"schema_version", "root_id", "created_at"},
                    label="inspection root record")
        require(type(root.get("schema_version")) is int and root["schema_version"] == 1,
                "inspection root record schema mismatch")
        root_created = _canonical_timestamp(root.get("created_at"), "inspection root created_at")
        value = root.get("root_id")
        require(_task_identity(value), "inspection root record has no valid root identity")
        if self.root_id is None:
            self.root_id = value
        require(self.root_id == value, "inspection root identity changed")
        if self.root_created_at is None:
            self.root_created_at = root_created
        require(self.root_created_at == root_created, "inspection root creation timestamp changed")
        return value

    def _inspection_journal_paths(self) -> dict[str, Path]:
        parent = self.state / "inspections"
        try:
            parent.lstat()
        except FileNotFoundError:
            return {}
        except OSError as error:
            raise AcceptanceFailure(f"cannot inspect inspection journal parent: {error}") from error
        require(parent.is_dir() and not parent.is_symlink(),
                "inspection journal parent is not a regular directory")
        paths: dict[str, Path] = {}
        for path in parent.iterdir():
            require(_task_identity(path.name), "inspection journal has an invalid task identity")
            require(path.is_dir() and not path.is_symlink(),
                    "inspection journal task entry is not a regular directory")
            require(path.name in self.dispatch_attempts,
                    "inspection journal exists for an unattempted task")
            paths[path.name] = path
        self.inspection_attempts.update(paths)
        return paths

    def validate_inspection_journal(self, task: str, directory: Path,
                                    allow_failure: bool = False,
                                    expected_binding_required: bool = True) -> dict[str, object]:
        """Validate one complete inspection journal for an acceptance gate.

        Provider-specific gates normally compare the journal binding with a
        definition they constructed before dispatch.  A native provider gate
        cannot reconstruct that definition without duplicating provider setup,
        so it may disable only that equality check while retaining every
        structural, timing, executable, supervisor, and result predicate.
        """
        return self._validate_inspection_journal(
            task, directory, allow_failure, expected_binding_required)

    def _validate_inspection_journal(self, task: str, directory: Path,
                                     allow_failure: bool,
                                     expected_binding_required: bool = True) -> dict[str, object]:
        root = self._known_root_id()
        request_path = directory / "request.json"
        request = _read_control_record(request_path, f"inspection request for {task}")
        _exact_keys(request, {
            "schema_version", "root_id", "task_id", "task", "task_sha256", "binding",
            "group", "label", "created_at", "deadline",
        }, label=f"inspection request for {task}")
        require(type(request.get("schema_version")) is int and request["schema_version"] == 1,
                f"inspection request schema mismatch for {task}")
        require(request.get("root_id") == root and request.get("task_id") == task,
                f"inspection request identity mismatch for {task}")
        group = _inspection_group(root)
        label = _inspection_label(root, task)
        require(request.get("group") == group and request.get("label") == label,
                f"inspection request group or label mismatch for {task}")
        embedded_task = request.get("task")
        embedded_digest = _validate_task_record(embedded_task, root, task,
                                                f"inspection request task for {task}")
        task_digest = request.get("task_sha256")
        require(_hex_digest(task_digest) and task_digest == embedded_digest,
                f"inspection request task digest is invalid for {task}")
        created = _canonical_timestamp(request.get("created_at"),
                                       f"inspection request created_at for {task}")
        deadline = _canonical_timestamp(request.get("deadline"),
                                        f"inspection request deadline for {task}")
        require(self.root_created_at is not None and
                _timestamp_at_or_after(created, self.root_created_at),
                f"inspection request precedes root creation for {task}")
        binding = request.get("binding")
        _validate_inspection_binding(binding, f"inspection request binding for {task}")
        standalone = binding.get("definition_revision") == "model-discovery-v1"
        timeout_seconds = 60 if standalone else 20
        require(deadline[0] - created[0] == timedelta(seconds=timeout_seconds) and
                deadline[1] == created[1],
                f"inspection request deadline is not exactly {timeout_seconds} seconds for {task}")

        ordinary_directory = self.state / "tasks" / task
        ordinary_present = ordinary_directory.exists() or ordinary_directory.is_symlink()
        if ordinary_present:
            require(ordinary_directory.is_dir() and not ordinary_directory.is_symlink(),
                    f"ordinary task directory is not a regular directory for {task}")
            ordinary_task_path = ordinary_directory / "task.json"
            ordinary_task = _read_control_record(ordinary_task_path,
                                                 f"ordinary task record for {task}")
            ordinary_digest = _validate_task_record(ordinary_task, root, task,
                                                    f"ordinary task record for {task}")
            require(embedded_task == ordinary_task and task_digest == ordinary_digest and
                    task_digest == digest(ordinary_task_path),
                    f"inspection task record is not bound to ordinary task {task}")
        require(not ordinary_present or not standalone,
                f"standalone discovery admitted an ordinary task for {task}")
        expected_binding = self.inspection_bindings.get(task)
        if expected_binding_required:
            require(expected_binding is not None,
                    f"inspection request has no expected source binding for {task}")
            require(binding == expected_binding,
                    f"inspection request binding changed from expected source for {task}")
        require(binding.get("worker_executable") == str(self.runner),
                f"inspection request worker mismatch for {task}")
        require(binding.get("worker_sha256") == digest(self.runner),
                f"inspection request worker digest mismatch for {task}")
        supervisor = binding.get("supervisor")
        require(self.pueue_config is not None, "inspection supervisor config is unavailable")
        require(supervisor.get("client_executable") == str(self.pueue) and
                supervisor.get("client_sha256") == digest(self.pueue) and
                supervisor.get("config_path") == str(self.pueue_config) and
                supervisor.get("config_digest") == digest(self.pueue_config) and
                supervisor.get("observed_version") == "pueue " + PUEUE_VERSION,
                f"inspection request supervisor binding mismatch for {task}")

        request_digest = digest(request_path)
        receipt = _read_control_record(directory / "receipt.json", f"inspection receipt for {task}")
        _exact_keys(receipt, {"schema_version", "request_sha256", "numeric_task_id"},
                    label=f"inspection receipt for {task}")
        receipt_id = receipt.get("numeric_task_id")
        require(type(receipt.get("schema_version")) is int and receipt["schema_version"] == 1 and
                receipt.get("request_sha256") == request_digest and
                isinstance(receipt_id, int) and not isinstance(receipt_id, bool) and receipt_id >= 0,
                f"inspection receipt binding mismatch for {task}")

        submission = _read_control_record(directory / "submission.json",
                                           f"inspection submission for {task}")
        submission_created = _validate_claim_record(
            submission, request_digest, f"inspection submission for {task}", created, deadline)

        result_path = directory / "result.json"
        completion_path = directory / "completion.json"
        observation_path = directory / "worker-observation.json"
        result = _read_optional_control_record(result_path, f"inspection result for {task}")
        completion = _read_optional_control_record(completion_path, f"inspection completion for {task}")
        observation = _read_optional_control_record(observation_path, f"inspection worker observation for {task}")
        start = _read_optional_control_record(directory / "start.json",
                                              f"inspection start for {task}")
        if result is not None or completion is not None or observation is not None or ordinary_present:
            require(start is not None, f"inspection start guard is missing for {task}")
        start_created: tuple[datetime, int] | None = None
        if start is not None:
            start_created = _validate_claim_record(
                start, request_digest, f"inspection start for {task}", created, deadline)
            require(_timestamp_at_or_after(start_created, submission_created),
                    f"inspection start precedes submission for {task}")
        if result is None:
            require(completion is None and observation is None,
                    f"inspection completion or observation has no result for {task}")
        else:
            _exact_keys(result, {"schema_version", "request_sha256", "reason", "facts"},
                        label=f"inspection result for {task}")
            require(type(result.get("schema_version")) is int and result["schema_version"] == 1 and
                    result.get("request_sha256") == request_digest,
                    f"inspection result binding mismatch for {task}")
            require(result.get("reason") in {"eligible", "unavailable", "deadline-expired"},
                    f"inspection result reason is invalid for {task}")
            require(isinstance(result.get("facts"), dict),
                    f"inspection result facts are invalid for {task}")
            if result.get("reason") != "eligible":
                require(result["facts"] == {},
                        f"noneligible inspection result contains facts for {task}")
        if completion is not None:
            _exact_keys(completion, {
                "schema_version", "request_sha256", "result_sha256", "completed_at", "native_exit",
            }, label=f"inspection completion for {task}")
            require(result is not None and type(completion.get("schema_version")) is int and
                    completion["schema_version"] == 1 and
                    completion.get("request_sha256") == request_digest and
                    completion.get("result_sha256") == digest(result_path),
                    f"inspection completion binding mismatch for {task}")
            require((result.get("reason") == "eligible" and
                     completion.get("native_exit") == "successful-exit") or
                    (result.get("reason") != "eligible" and
                     completion.get("native_exit") == "unavailable"),
                    f"inspection completion exit disagrees with result for {task}")
            require(completion.get("native_exit") in {"successful-exit", "unavailable"},
                    f"inspection completion exit classification is invalid for {task}")
            completed = _canonical_timestamp(completion.get("completed_at"),
                                             f"inspection completion for {task}")
            require(_timestamp_at_or_after(completed, created),
                    f"inspection completion precedes request for {task}")
            require(start_created is not None and _timestamp_at_or_after(completed, start_created),
                    f"inspection completion precedes start for {task}")
            if result.get("reason") == "eligible":
                require(_timestamp_before(completed, deadline),
                        f"eligible inspection completed after deadline for {task}")
        if observation is not None:
            _exact_keys(observation, {
                "schema_version", "request_sha256", "completion_sha256", "numeric_task_id", "state",
            }, label=f"inspection worker observation for {task}")
            require(type(observation.get("schema_version")) is int and observation["schema_version"] == 1 and
                    observation.get("request_sha256") == request_digest and
                    isinstance(completion, dict) and
                    observation.get("completion_sha256") == digest(completion_path) and
                    observation.get("numeric_task_id") == receipt_id and
                    type(observation.get("numeric_task_id")) is int and
                    observation.get("state") == "succeeded",
                    f"inspection worker observation binding mismatch for {task}")

        if allow_failure:
            if result is None:
                require(not ordinary_present and observation is None,
                        f"failed inspection has an ordinary task or observation for {task}")
            elif result.get("reason") == "eligible":
                require((standalone or ordinary_present) and completion is not None and
                        completion.get("native_exit") == "successful-exit" and observation is not None,
                        f"eligible inspection lacks independent success proof for {task}")
            else:
                require(not ordinary_present and observation is None and
                        completion is not None and completion.get("native_exit") == "unavailable",
                        f"failed inspection has conflicting success proof for {task}")
        else:
            require(result is not None and result.get("reason") == "eligible" and
                    (standalone or ordinary_present) and completion is not None and
                    completion.get("native_exit") == "successful-exit" and observation is not None,
                    f"successful inspection proof is incomplete for {task}")
        return {"task_id": task, "root_id": root, "group": group, "label": label,
                "numeric_task_id": receipt_id, "request": request, "request_digest": request_digest,
                "receipt": receipt, "submission": submission, "start": start,
                "result": result, "completion": completion,
                "worker_observation": observation}

    def validate_inspection_journals(self, allow_failure: bool = False,
                                     expected_binding_required: bool | None = None) -> dict[str, dict[str, object]]:
        """Validate every inspection journal and require exact expected coverage."""
        if expected_binding_required is None:
            expected_binding_required = self.expected_inspection_binding_required
        return self._inspection_journals(allow_failure, expected_binding_required)

    def _inspection_journals(self, allow_failure: bool,
                             expected_binding_required: bool | None = None) -> dict[str, dict[str, object]]:
        if expected_binding_required is None:
            expected_binding_required = self.expected_inspection_binding_required
        journals = {}
        for task, directory in self._inspection_journal_paths().items():
            journals[task] = self._validate_inspection_journal(
                task, directory, allow_failure, expected_binding_required)
        require(self.inspection_attempts <= set(journals),
                "an expected inspection journal is missing")
        return journals

    @staticmethod
    def _validate_group_snapshot(groups: object, expected: set[str]) -> None:
        require(isinstance(groups, dict), "private queue groups object is absent")
        require(set(groups) == expected, "private queue contains an unknown supervisor group")
        for name in expected:
            group = groups[name]
            require(isinstance(group, dict), f"private queue group {name} is malformed")
            _exact_keys(group, _PUEUE_GROUP_KEYS, label=f"private queue group {name}")
            require(group.get("status") in {"Running", "Paused", "Reset"} and
                    type(group.get("parallel_tasks")) is int and
                    group["parallel_tasks"] >= 0,
                    f"private queue group {name} is malformed")

    def _validate_inspection_queue_row(self, row: dict[str, object], journal: dict[str, object],
                                       allow_failure: bool) -> None:
        task = journal["task_id"]
        label = journal["label"]
        require(row.get("label") == label and row.get("group") == journal["group"],
                f"inspection queue identity mismatch for {task}")
        require(row.get("id") == journal["numeric_task_id"],
                f"inspection queue numeric identity mismatch for {task}")
        require(row_state(row) == "Done", f"inspection worker is not Done for {task}")
        result = done_result(row)
        inspection_result = journal["result"]
        successful_result = (isinstance(inspection_result, dict) and
                             inspection_result.get("reason") == "eligible")
        expected_result = "Success" if not allow_failure or successful_result else None
        if expected_result == "Success":
            require(result == "Success", f"inspection worker did not succeed for {task}")
        else:
            require(result in INSPECTION_FAILURE_RESULTS,
                    f"inspection worker failure was not positively observed for {task}")
        expected = [str(self.runner), "--inspection", "--root", str(self.state), str(task)]
        commands = []
        for key in ("original_command", "command"):
            value = row.get(key)
            require(isinstance(value, str), f"inspection queue row missing {key} for {task}")
            try:
                commands.append(shlex.split(value))
            except ValueError:
                commands.append([])
        require(all(command == expected for command in commands),
                f"inspection queue argv mismatch for {task}")

    def _validate_queue_membership(self, status: dict[str, object], allow_failure: bool) -> None:
        tasks = status.get("tasks")
        require(isinstance(tasks, dict), "private queue tasks object is absent")
        require(self.dispatch_attempts, "no supervisor dispatch attempt was recorded")
        journals = self._inspection_journals(
            allow_failure, self.expected_inspection_binding_required)
        ordinary_task_ids = set(self.tasks.values())
        require(ordinary_task_ids <= self.dispatch_attempts,
                "ordinary task response has no matching dispatch attempt")
        inspection_task_ids = set(journals)
        require(ordinary_task_ids | inspection_task_ids == self.dispatch_attempts,
                "a dispatch attempt has no exact ordinary or inspection record")
        if not allow_failure:
            require(ordinary_task_ids == self.dispatch_attempts,
                    "a successful dispatch attempt has no exact ordinary task response")
        groups = status.get("groups")
        expected_groups = {"default"} | {journal["group"] for journal in journals.values()}
        self._validate_group_snapshot(groups, expected_groups)
        for journal in journals.values():
            group = groups.get(journal["group"])
            require(isinstance(group, dict) and group.get("status") == "Running" and
                    isinstance(group.get("parallel_tasks"), int) and
                    not isinstance(group.get("parallel_tasks"), bool) and
                    group.get("parallel_tasks") == 1,
                    f"inspection group is not running at parallelism one for {journal['task_id']}")
        ordinary_labels = {self.labels[name] for name in self.tasks}
        expected_labels = ordinary_labels | {journal["label"] for journal in journals.values()}
        expected_count = len(self.tasks) + len(journals)
        require(len(tasks) == expected_count, "unexpected private queue membership")
        rows = [row for row in tasks.values() if isinstance(row, dict)]
        require(len(rows) == len(tasks), "private queue contains a malformed row")
        labels = [row.get("label") for row in rows]
        require(all(isinstance(label, str) for label in labels) and len(set(labels)) == len(labels) and
                set(labels) == expected_labels, "private queue labels are not exact")
        for name, task in self.tasks.items():
            row = next((candidate for candidate in rows if candidate.get("label") == self.labels[name]), None)
            require(isinstance(row, dict), f"private queue lost ordinary task {task}")
            require(name in self.numbers, f"ordinary task {task} has no recorded numeric identity")
            validate_queue_row(row, self.labels[name], self.runner, self.state, task,
                               self.numbers[name], "default")
            require(row_state(row) == "Done" and
                    (not allow_failure or done_result(row) is not None) and
                    (allow_failure or done_result(row) == "Success"),
                    f"ordinary task is not in an acceptable terminal state: {task}")
        for journal in journals.values():
            row = next((candidate for candidate in rows if candidate.get("label") == journal["label"]), None)
            require(isinstance(row, dict), f"private queue lost inspection task {journal['task_id']}")
            self._validate_inspection_queue_row(row, journal, allow_failure)

    def _inspection_snapshot(self) -> dict[str, dict[str, object]]:
        parent = self.state / "inspections"
        try:
            parent.lstat()
        except FileNotFoundError:
            return {}
        except OSError as error:
            raise AcceptanceFailure(f"cannot snapshot inspection journals: {error}") from error
        return snapshot(parent)

    def _inspection_control_started(self) -> bool:
        if self.inspection_attempts:
            return True
        for name in ("inspection-group", "inspections"):
            try:
                (self.state / name).lstat()
            except FileNotFoundError:
                continue
            except OSError:
                return True
            return True
        return False

    @staticmethod
    def _empty_private_queue(status: dict[str, object]) -> bool:
        tasks = status.get("tasks")
        groups = status.get("groups")
        if not isinstance(tasks, dict) or not isinstance(groups, dict) or tasks:
            return False
        if not groups:
            return True
        # Pueue may expose its implicit default group even when no task has
        # ever been admitted. Any named group is durable supervisor work and
        # therefore makes this static-refusal oracle inapplicable.
        if set(groups) != {"default"}:
            return False
        default = groups["default"]
        return (isinstance(default, dict) and default.get("status") == "Running" and
                type(default.get("parallel_tasks")) is int and
                default.get("parallel_tasks") == 1)

    def _static_refusal_state_is_clean(self, initialized: bool = True) -> bool:
        """Reject every durable task, inspection, and group authorization record."""
        try:
            if not self.state.is_dir() or self.state.is_symlink():
                return False
            entries = list(self.state.iterdir())
        except OSError:
            return False
        names = {entry.name for entry in entries}
        if initialized and not {"root.json", ".maintenance.lock"} <= names:
            return False
        if not names <= {"root.json", ".maintenance.lock", ".probe"}:
            return False
        for entry in entries:
            if entry.name in {"root.json", ".maintenance.lock"}:
                if not entry.is_file() or entry.is_symlink():
                    return False
            elif entry.is_symlink() or not entry.is_dir():
                return False
        return True

    def _unattempted_queue_finished(self, status: dict[str, object]) -> bool:
        """Allow setup cleanup only with no attempted or durable task work."""
        if (self.tasks or self.labels or self.numbers or self.dispatch_attempts or
                self.dispatch_observations or self.inspection_attempts or self.inspection_bindings):
            return False
        if not self._empty_private_queue(status) or not self._static_refusal_state_is_clean(False):
            return False
        try:
            if (self.state / "root.json").exists():
                self._known_root_id()
            elif self.root_id is not None or self.root_created_at is not None:
                return False
        except (AcceptanceFailure, OSError, TypeError, ValueError, RecursionError):
            return False
        return True

    @staticmethod
    def _is_static_refusal_observation(observation: dict[str, object], root: str,
                                       task: str) -> bool:
        if (observation.get("receipt_written") is not True or
                observation.get("task_id") != task or
                observation.get("exit_code") != 2 or
                observation.get("natural_wait") is not True):
            return False
        response = observation.get("response")
        if not isinstance(response, dict):
            return False
        error = response.get("error")
        if not isinstance(error, str) or not error:
            return False
        static_refusal = (set(response) == _STATIC_REFUSAL_RESPONSE_KEYS and
                          (error == STATIC_REFUSAL_ERROR or
                           error.startswith(STATIC_REFUSAL_ERROR + ":")))
        auth_refusal = (set(response) == _STATIC_REFUSAL_RESPONSE_KEYS | {"status"} and
                        response.get("status") == "blocked" and
                        error.startswith(AUTHENTICATION_REFUSAL_ERROR + ": " +
                                         STATIC_REFUSAL_ERROR + ":"))
        return ((static_refusal or auth_refusal) and
                type(response.get("schema_version")) is int and
                response.get("schema_version") == 1 and response.get("command") == "dispatch" and
                response.get("root_id") == root and response.get("task_id") == task and
                response.get("admission") == "unknown" and
                response.get("liveness") == "undetermined" and
                response.get("publication") == "unknown")

    def _static_refusal_queue_finished(self, status: dict[str, object]) -> bool:
        """Allow cleanup for an exact pre-admission static refusal only."""
        if not self.dispatch_attempts or not self.dispatch_observations:
            return False
        if len(self.dispatch_observations) != len(self.dispatch_attempts):
            return False
        if not self.inspection_attempts <= self.dispatch_attempts:
            return False
        tasks = [observation.get("task_id") for observation in self.dispatch_observations]
        if any(not _task_identity(task) for task in tasks):
            return False
        if len(set(tasks)) != len(tasks) or set(tasks) != self.dispatch_attempts:
            return False
        if not self._empty_private_queue(status):
            return False
        try:
            root = self._known_root_id()
        except (AcceptanceFailure, OSError, TypeError, ValueError, RecursionError):
            return False
        if not self._static_refusal_state_is_clean():
            return False
        return all(self._is_static_refusal_observation(observation, root, task)
                   for observation, task in zip(self.dispatch_observations, tasks))

    def replay(self, records: dict[str, dict[str, object]],
               names: tuple[str, ...] = ("fresh", "resume"),
               expected: str = "committed") -> None:
        """Re-collect exact records and prove replay created no queue work."""
        before_queue = self.queue_status("replay-queue-before")
        self._validate_queue_membership(before_queue, allow_failure=False)
        self.queue_before_replay = canonical_queue(before_queue)
        before_inspections = self._inspection_snapshot()
        for name in names:
            require(name in self.tasks and name in records, f"replay lacks {name} task")
            task = self.tasks[name]
            record = records[name]
            directory = record.get("directory")
            require(isinstance(directory, Path), f"replay {name} record has no directory")
            before = snapshot(directory)
            outcome = record.get("outcome")
            payload = record.get("payload")
            require(isinstance(outcome, dict) and isinstance(payload, dict),
                    f"replay {name} record is incomplete")
            original = {"outcome": deepcopy(outcome), "payload": deepcopy(payload),
                        "evidence_sha256": outcome.get("evidence_sha256")}
            collected = self.collect("replay-" + name, task)
            actual_outcome, _ = verify_collected_outcome(collected, directory, expected)
            actual = {"outcome": deepcopy(actual_outcome),
                      "payload": deepcopy(collected.get("payload")),
                      "evidence_sha256": collected.get("evidence_sha256")}
            require(actual == original, f"{name} replay changed outcome, payload or evidence hash")
            require(before == snapshot(directory), f"{name} replay changed immutable task records")
        after_queue = self.queue_status("replay-queue-after")
        self._validate_queue_membership(after_queue, allow_failure=False)
        require(canonical_queue(after_queue) == self.queue_before_replay,
                "collection replay changed the private supervisor queue")
        require(self._inspection_snapshot() == before_inspections,
                "collection replay changed inspection journal records")
        write_json(self.output / "replay-control.json", {
            "tasks": dict(self.tasks),
            "provider_launches": len(self.tasks),
            "replay_launches": 0,
            "queue_unchanged": True,
            "outcomes_unchanged": True,
        })

    def failure_queue_finished(self, status: dict[str, object]) -> bool:
        if not self.dispatch_attempts:
            return self._unattempted_queue_finished(status)
        if self._static_refusal_queue_finished(status):
            return True
        if not self._inspection_control_started():
            # Providers without an inspection journal retain the established
            # queue-only failure oracle.
            try:
                expected_groups = {"default"} if self.tasks else set()
                self._validate_group_snapshot(status.get("groups"), expected_groups)
                return failure_queue_finished(status, self.dispatch_attempts, self.tasks,
                                              self.labels, self.runner, self.state, self.numbers)
            except (AcceptanceFailure, KeyError, OSError, TypeError, ValueError, RecursionError):
                return False
        try:
            self._validate_queue_membership(status, allow_failure=True)
        except (AcceptanceFailure, KeyError, OSError, TypeError, ValueError, RecursionError):
            return False
        return True

    def shutdown(self) -> None:
        """Naturally shut down only after every owned task is Done.Success."""
        status = self.queue_status("final-queue")
        self._validate_queue_membership(status, allow_failure=False)
        excluded = () if self.daemon is None else (self.daemon,)
        pending = self.processes.drain(timeout=1, exclude=excluded)
        require(not pending, "owned acceptance process remains active")
        self.client("private-shutdown", ["shutdown"], timeout=20)
        require(self.daemon is not None, "private daemon was not started")
        self.daemon.wait(timeout=30, expected=0)
        self.closed = True

    def safe_failure_shutdown(self) -> bool:
        """Observe and close only a positively finished private queue."""
        if self.daemon is None or self.daemon.poll() is not None or self.pueue_config is None:
            return False
        try:
            pending = self.processes.drain(timeout=0, exclude=(self.daemon,))
            if pending:
                return False
            status = status_jobs(self.client("failure-queue", ["status", "--json"],
                                             expected=0, timeout=20))
            if not self.failure_queue_finished(status):
                return False
            self.client("failure-shutdown", ["shutdown"], timeout=20)
            self.daemon.wait(timeout=30, expected=0)
            self.closed = True
            return True
        except BaseException:
            return False

    def retain_failure_ownership(self) -> bool:
        """Retain owned callers until natural completion and exact queue cleanup.

        Failure cleanup is an observation loop.  It never sends a signal or
        attempts PID-based recovery: an active caller or daemon remains owned
        until it ends naturally, and an exact terminal queue observation is
        still required before reporting a successful shutdown.
        """
        reported = False
        queue_retry_at = 0.0
        queue_retry_delay = 0.25
        observation_retry_delay = 0.25
        while True:
            excluded = () if self.daemon is None else (self.daemon,)
            try:
                pending = self.processes.drain(timeout=0, exclude=excluded)
            except BaseException:
                # A completion receipt can fail after a child has exited.
                # Keep the owner and retry observation rather than assuming
                # that the caller or daemon is naturally finished.
                pending = None
                if not reported:
                    print("Acceptance assertion failed; retaining owned processes until exact cleanup is observed.",
                          flush=True)
                    reported = True
                time.sleep(observation_retry_delay)
                observation_retry_delay = min(observation_retry_delay * 2, 5.0)
                continue
            observation_retry_delay = 0.25
            if not pending and self.closed:
                return True
            if not pending and self.daemon is not None:
                now = time.monotonic()
                if now >= queue_retry_at:
                    try:
                        cleaned = self.safe_failure_shutdown()
                    except BaseException:
                        cleaned = False
                    if cleaned:
                        return True
                    queue_retry_at = now + queue_retry_delay
                    queue_retry_delay = min(queue_retry_delay * 2, 5.0)
            if not pending:
                if self.daemon is None:
                    return False
                try:
                    daemon_finished = self.daemon.poll() is not None
                except BaseException:
                    daemon_finished = False
                if daemon_finished:
                    return False
            if not reported:
                print("Acceptance assertion failed; retaining owned processes until exact cleanup is observed.",
                      flush=True)
                reported = True
            delay = 0.25
            if not pending and self.daemon is not None:
                delay = min(delay, max(0.0, queue_retry_at - time.monotonic()))
            time.sleep(delay)


def wait_runner_done(
    client: Callable[[str, list[str]], object],
    root_id: str,
    task_id: str,
    timeout: float = 150,
    sleep_fn: Callable[[float], None] = time.sleep,
    monotonic_fn: Callable[[], float] = time.monotonic,
) -> dict[str, object]:
    """Observe one exact delegate runner row until it finishes successfully."""
    label = "delegate:" + root_id + ":" + task_id
    deadline = monotonic_fn() + timeout
    while monotonic_fn() < deadline:
        result = client("runner-status", ["status", "--json"])
        value = result.json()
        require(isinstance(value, dict), "supervisor status response is not an object")
        rows = value.get("tasks", {})
        require(isinstance(rows, dict), "supervisor status has no tasks object")
        matching = [row for row in rows.values()
                    if isinstance(row, dict) and row.get("label") == label]
        require(len(matching) == 1, "submitted task has no unique supervisor row")
        row = matching[0]
        status = row.get("status")
        if isinstance(status, dict) and "Done" in status:
            done = status["Done"]
            require(isinstance(done, dict) and done.get("result") == "Success",
                    "supervisor reports a failed runner")
            return row
        sleep_fn(0.1)
    raise RuntimeError("runner completion was not observed within acceptance deadline")


def snapshot(directory: Path, ignored_prefixes: tuple[str, ...] = (".ack",)) -> dict[str, dict[str, object]]:
    """Return an immutable regular-file snapshot below one evidence directory."""
    directory = Path(directory)
    require(directory.is_dir() and not directory.is_symlink(),
            "evidence directory is absent: " + str(directory))
    result: dict[str, dict[str, object]] = {}
    for path in sorted(directory.rglob("*")):
        if path.is_symlink():
            raise RuntimeError("evidence contains symlink: " + str(path))
        if not path.is_file() or path.name.endswith(".staging") or any(
                path.name.startswith(prefix) for prefix in ignored_prefixes):
            continue
        data = path.read_bytes()
        result[str(path.relative_to(directory))] = {"bytes": len(data), "sha256": sha(data)}
    return result


def task_snapshot(root: Path, task_id: str) -> dict[str, str]:
    """Snapshot the immutable records used by the contributor acceptance gate."""
    directory = Path(root) / "tasks" / task_id
    outcome = read_json(directory / "outcome.json")
    seal = read_json(directory / "provider.exit")
    names = ["task.json", "meta.json", "provider.start", "provider.exit", "outcome.json",
             "provider.ref.json", outcome["payload"]["basename"],
             *[entry["path"] for entry in seal["raw_manifest"]]]
    return {name: digest(directory / name) for name in names
            if (directory / name).exists()}
