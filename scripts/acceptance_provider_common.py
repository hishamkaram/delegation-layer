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
import stat
import sys
import time
import unicodedata
from typing import Callable, Iterable

from acceptance_supervisor_common import (
    config_for,
    digest,
    failure_cleanup_deadline,
    read_json,
    sha,
    wait_for_failure_cleanup,
    write_json,
)


MAX_CONTROL_BYTES = 1 << 20
MAX_PROVIDER_BYTES = 8 << 20
INSPECTION_GROUP_PREFIX = "delegation-inspection-"
INSPECTION_FAILURE_RESULTS = {"Killed", "Errored", "DependencyFailed", "Failed", "FailedToSpawn"}
ADMISSION_INSPECTION_TIMEOUT_SECONDS = (60, 20)
MODEL_DISCOVERY_TIMEOUT_SECONDS = (300, 60)
MAX_LIFETIME_LAUNCHES = 64
PUEUE_STATE_VARIANTS = frozenset({"Queued", "Running", "Paused", "Stashed", "Done", "Locked"})
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
_TASK_CONFIG_KEYS = {"interactive", "model", "effort", "permission", "budget", "native_timeout",
                     "review_target"}
_PRIOR_SESSION_KEYS = {"provider", "conversation_id", "predecessor_task_id"}
_INSPECTION_BINDING_KEYS = {
    "definition_revision", "definition_sha256", "helper_executable", "helper_sha256",
    "worker_executable", "worker_sha256", "supervisor",
}
_INSPECTION_BINDING_OPTIONAL_KEYS = {
    "capability_sha256", "environment", "environment_recorded", "runner_ownership",
    "worker_exit_proof",
}
_SUPERVISOR_BINDING_KEYS = {
    "client_executable", "client_sha256", "resolved_config_sha256", "endpoint",
    "config_path", "config_digest", "observed_version",
}
_SUPERVISOR_RESOLUTION_KEYS = {
    "resolution_os", "resolution_home", "resolution_data_local",
    "resolution_config", "resolution_runtime", "resolution_username",
    # Accepted for records written before recovery stopped using the caller cwd.
    "resolution_cwd",
}
_SUPERVISOR_BINDING_OPTIONAL_KEYS = {
    "daemon_executable", "daemon_sha256", *_SUPERVISOR_RESOLUTION_KEYS,
}
_META_REQUIRED_KEYS = {
    "schema_version", "root_id", "task_id", "spec_sha256", "requested_config",
    "effective_config", "containment", "approval", "provider_executable",
    "provider_version", "publisher_build", "publisher_version", "predicate",
    "supervisor_config", "created_at",
}
_META_OPTIONAL_KEYS = {
    "runner_executable", "runner_ownership", "environment", "environment_recorded",
    "runtime_preflight_sha256", "output_artifacts", "output_writer_contract", "input_files",
}
_EFFECTIVE_CONFIG_KEYS = {"containment", "approval", "digest"}
_PREDICATE_KEYS = {"adapter", "mode", "version", "sha256"}
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


def _required_keys(value: dict[str, object], required: set[str],
                   label: str = "record") -> None:
    """Require protocol fields while allowing additive producer fields."""
    require(isinstance(value, dict), f"{label} is not an object")
    require(required <= set(value), f"{label} has missing fields")


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
    canonical_config: dict[str, object] = {}
    if config.get("interactive") is True:
        canonical_config["interactive"] = True
    for key in ("model", "effort", "permission", "budget", "native_timeout"):
        if config.get(key) not in (None, ""):
            canonical_config[key] = config[key]
    review_target = config.get("review_target")
    if review_target is not None:
        canonical_config["review_target"] = _canonical_review_target(
            review_target, "inspection task requested_config review_target")
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
    for key, item in config.items():
        if key == "interactive":
            require(item is True, f"{label} requested_config interactive is invalid")
        elif key == "review_target":
            _canonical_review_target(item, f"{label} requested_config review_target")
        else:
            require(isinstance(item, str) and item != "",
                    f"{label} requested_config has invalid fields")
    prior = value.get("prior_session")
    if prior is not None:
        require(isinstance(prior, dict), f"{label} prior_session is invalid")
        _exact_keys(prior, _PRIOR_SESSION_KEYS, label=f"{label} prior_session")
        require(all(isinstance(item, str) and item for item in prior.values()),
                f"{label} prior_session has invalid fields")
    return sha(_canonical_task_bytes(value))


def _canonical_review_target(value: object, label: str) -> dict[str, str]:
    """Validate and normalize the persisted ReviewTarget JSON shape."""
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, {"kind"}, {"value"}, label)
    kind = value.get("kind")
    target_value = value.get("value", "")
    require(isinstance(kind, str) and kind in {"uncommitted", "base", "commit"},
            f"{label} kind is invalid")
    require(isinstance(target_value, str), f"{label} value is invalid")
    if kind == "uncommitted":
        require(target_value == "", f"{label} uncommitted target has a value")
        return {"kind": kind}
    require(target_value != "" and target_value == target_value.strip() and
            len(target_value) <= MAX_CONTROL_BYTES and
            all(0x20 <= ord(character) < 0x7f or ord(character) > 0x9f
                for character in target_value),
            f"{label} value is invalid")
    return {"kind": kind, "value": target_value}


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


def runtime_capability_sha(definition: dict[str, object]) -> str:
    """Hash runtime capability fields while excluding executable identity."""
    runtime = definition.get("runtime")
    require(isinstance(runtime, dict), "runtime capability definition is missing runtime fields")

    def go_slice(value: object) -> object:
        # The Go digest clones slices into nil-backed values, so an empty
        # slice is encoded as JSON null. Keep this mirror byte-identical.
        return None if isinstance(value, list) and not value else value

    contract = {
        "revision": definition["revision"],
        "directory": definition["directory"],
        "environment": go_slice(definition["environment"]),
        "output_limit": definition["output_limit"],
        "help_args": go_slice(runtime["help_args"]),
        "required_flags": go_slice(runtime["required_flags"]),
    }
    return sha(canonical_go_json(contract))


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
                       config_digest: str, pueue_version: str) -> dict[str, object]:
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
        "observed_version": pueue_version,
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


def bounded_text(path: Path, label: str, bound: int = MAX_CONTROL_BYTES) -> str:
    """Read bounded UTF-8 text before any whitespace normalization."""
    require(type(bound) is int and bound > 0, label + " bound is invalid")
    with Path(path).open("rb") as stream:
        data = stream.read(bound + 1)
    require(len(data) <= bound, label + " exceeds the bounded output limit")
    try:
        return data.decode("utf-8")
    except UnicodeDecodeError as error:
        raise AcceptanceFailure(label + " is not valid UTF-8") from error


def observed_pueue_version(value: str | bytes, command: str) -> str:
    """Validate one bounded pueue executable version observation."""
    if isinstance(value, bytes):
        require(len(value) <= MAX_CONTROL_BYTES, f"{command} version output exceeds the control bound")
        try:
            value = value.decode("utf-8")
        except UnicodeDecodeError as error:
            raise AcceptanceFailure(f"{command} version output is not valid UTF-8") from error
    require(isinstance(value, str), f"{command} version output is malformed")
    try:
        raw_length = len(value.encode("utf-8"))
    except UnicodeEncodeError as error:
        raise AcceptanceFailure(f"{command} version output is not valid UTF-8") from error
    require(raw_length <= MAX_CONTROL_BYTES,
            f"{command} version output exceeds the control bound")
    observed = value.strip()
    require(observed and
            not any(unicodedata.category(character) == "Cc" for character in observed),
            f"{command} version output is empty or malformed")
    return observed


def row_state(row: dict[str, object]) -> str:
    value = row.get("status")
    require(isinstance(value, dict), "pueue row status is not a one-state object")
    require(set(value) <= PUEUE_STATE_VARIANTS,
            "pueue row status contains an unknown lifecycle variant")
    recognized = [name for name in value if name in PUEUE_STATE_VARIANTS]
    require(len(recognized) == 1, "pueue row status is not a one-state object")
    return recognized[0]


def done_result(row: dict[str, object]) -> str | None:
    """Return a recognized pueue terminal result, if the row is Done."""
    status = row.get("status")
    if not isinstance(status, dict):
        return None
    if not set(status) <= PUEUE_STATE_VARIANTS:
        return None
    recognized = [name for name in status if name in PUEUE_STATE_VARIANTS]
    if recognized != ["Done"]:
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
    require(isinstance(value, dict), f"pueue {label} state is invalid")
    require(depth <= 32, f"pueue {label} state nesting is excessive")
    require(set(value) <= PUEUE_STATE_VARIANTS,
            f"pueue {label} state contains an unknown lifecycle variant")
    recognized = [name for name in value if name in PUEUE_STATE_VARIANTS]
    require(len(recognized) == 1, f"pueue {label} state is invalid")
    name = recognized[0]
    state = value[name]
    if name == "Queued":
        _required_keys(state, {"enqueued_at"}, label=f"pueue {label} queued state")
        _validate_pueue_timestamp(state["enqueued_at"], f"{label} enqueued_at")
    elif name in {"Running", "Paused"}:
        _required_keys(state, {"enqueued_at", "start"}, label=f"pueue {label} running state")
        _validate_pueue_timestamp(state["enqueued_at"], f"{label} enqueued_at")
        _validate_pueue_timestamp(state["start"], f"{label} start")
    elif name == "Stashed":
        _required_keys(state, {"enqueue_at"}, label=f"pueue {label} stashed state")
        if state["enqueue_at"] is not None:
            _validate_pueue_timestamp(state["enqueue_at"], f"{label} enqueue_at")
    elif name == "Done":
        _required_keys(state, {"enqueued_at", "start", "end", "result"},
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
        _required_keys(state, {"previous_status"}, label=f"pueue {label} locked state")
        _validate_pueue_state(state["previous_status"], label, depth + 1)


def _validate_pueue_status(value: dict[str, object]) -> None:
    _required_keys(value, _PUEUE_STATUS_KEYS, label="pueue status")
    tasks = value["tasks"]
    groups = value["groups"]
    require(isinstance(tasks, dict), "pueue status JSON has no tasks object")
    require(isinstance(groups, dict), "pueue status JSON has no groups object")
    for name, group in groups.items():
        require(isinstance(name, str), "pueue group name is invalid")
        require(isinstance(group, dict), f"pueue group {name} is not an object")
        _required_keys(group, _PUEUE_GROUP_KEYS, label=f"pueue group {name}")
        require(group["status"] in {"Running", "Paused", "Reset"},
                f"pueue group {name} status is invalid")
        require(type(group["parallel_tasks"]) is int and group["parallel_tasks"] >= 0,
                f"pueue group {name} parallelism is invalid")
    for key, row in tasks.items():
        require(isinstance(key, str) and key.isdecimal() and str(int(key)) == key,
                "pueue task map key is not canonical")
        require(isinstance(row, dict), f"pueue task {key} is not an object")
        _required_keys(row, _PUEUE_ROW_KEYS, label=f"pueue task {key}")
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
                       group: str | None = None,
                       supervised: bool = False) -> None:
    """Bind a pueue status row to one exact runner/state/task invocation."""
    require(row.get("label") == label, f"pueue label mismatch for {task}")
    for key in ("original_command", "command", "path"):
        require(isinstance(row.get(key), str), f"pueue row missing {key} for {task}")
    if numeric is not None:
        require(type(row.get("id")) is int and row["id"] == numeric,
                f"pueue numeric identity mismatch for {task}")
    if group is not None:
        require(row.get("group") == group, f"pueue group mismatch for {task}")
    require(type(supervised) is bool, f"runner supervision mode is invalid for {task}")
    expected = [str(runner)]
    if supervised:
        expected.extend(["--guardian", "--supervised"])
    expected.extend(["--root", str(state), task])
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


def validate_provider_runtime_metadata(
        meta: dict[str, object], *, observed_version: str | None = None,
        launch_evidence: tuple[dict[str, object], dict[str, object]] | None = None,
        label: str = "provider runtime metadata") -> dict[str, str] | None:
    """Validate pending or historical runtime metadata and terminal identity."""
    require(isinstance(meta, dict), f"{label} is not an object")
    executable = meta.get("provider_executable")
    require(isinstance(executable, str) and Path(executable).is_absolute() and
            Path(executable) == Path(os.path.normpath(executable)),
            f"{label} executable is invalid")
    pending = "runtime_preflight_sha256" in meta
    version = meta.get("provider_version")
    if pending:
        require(_hex_digest(meta.get("runtime_preflight_sha256")),
                f"{label} preflight digest is invalid")
        require(version == "", f"{label} pending version is not empty")
        require(meta.get("environment_recorded") is True,
                f"{label} pending environment is not recorded")
    else:
        require(isinstance(version, str) and bool(version.strip()),
                f"{label} version is invalid")
        if observed_version is not None:
            require(version == observed_version,
                    f"{label} version does not match observed runtime")
    if "environment_recorded" in meta:
        require(type(meta["environment_recorded"]) is bool,
                f"{label} environment marker is invalid")
    if launch_evidence is None:
        return None
    started, exited = launch_evidence
    identities: list[dict[str, str]] = []
    for phase, evidence in (("started", started), ("exit", exited)):
        require(isinstance(evidence, dict), f"{label} {phase} evidence is not an object")
        evidence_executable = evidence.get("provider_executable")
        evidence_version = evidence.get("provider_version")
        evidence_sha256 = evidence.get("provider_sha256")
        require(isinstance(evidence_executable, str) and
                Path(evidence_executable).is_absolute() and
                Path(evidence_executable) == Path(os.path.normpath(evidence_executable)),
                f"{label} {phase} executable is invalid")
        require(isinstance(evidence_version, str) and bool(evidence_version.strip()),
                f"{label} {phase} version is invalid")
        require(_hex_digest(evidence_sha256),
                f"{label} {phase} digest is invalid")
        identities.append({
            "executable": evidence_executable,
            "version": evidence_version,
            "sha256": evidence_sha256,
        })
    require(identities[0] == identities[1],
            f"{label} started and exit runtime identities differ")
    identity = identities[0]
    if not pending:
        require(executable == identity["executable"] and version == identity["version"],
                f"{label} metadata does not match launch identity")
    return identity


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
    daemon_executable = value.get("daemon_executable")
    daemon_sha256 = value.get("daemon_sha256")
    require((daemon_executable is None) == (daemon_sha256 is None),
            f"{label} daemon provenance is incomplete")
    if daemon_executable is not None:
        require(isinstance(daemon_executable, str) and
                Path(daemon_executable).is_absolute() and
                Path(daemon_executable) == Path(os.path.normpath(daemon_executable)),
                f"{label} daemon_executable is invalid")
        require(_hex_digest(daemon_sha256), f"{label} daemon_sha256 is invalid")
    require(isinstance(value.get("endpoint"), str) and value["endpoint"],
            f"{label} endpoint is invalid")
    observed_pueue_version(value.get("observed_version"), "pueue")
    resolution_keys = _SUPERVISOR_RESOLUTION_KEYS & set(value)
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
    if "capability_sha256" in value:
        require(_hex_digest(value["capability_sha256"]), f"{label} capability_sha256 is invalid")
    if "runner_ownership" in value:
        require(value["runner_ownership"] in {"managed", "custom"},
                f"{label} runner ownership is invalid")
    if "worker_exit_proof" in value:
        require(value["worker_exit_proof"] == "guardian-wait-v1",
                f"{label} worker exit proof is invalid")
    if "environment_recorded" in value:
        require(type(value["environment_recorded"]) is bool,
                f"{label} environment_recorded is invalid")
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


def _lifetime_token(value: object) -> bool:
    return (isinstance(value, str) and bool(value) and value == value.strip() and
            "\x00" not in value and len(value.encode("utf-8")) <= MAX_CONTROL_BYTES)


def _lifetime_record_name(owner_kind: str, record: str, sequence: int | None = None) -> str:
    """Return the persisted filename used by one lifetime record owner."""
    if owner_kind == "inspection":
        names = {
            "intent": "lifetime-intent.json",
            "registration": "lifetime-registration.json",
            "worker": "lifetime-worker.json",
            "worker-wait": "lifetime-worker-wait.json",
            "termination": "lifetime-termination.json",
        }
        if record == "launch":
            require(sequence is not None, "inspection lifetime launch sequence is absent")
            return f"lifetime-launch-{sequence:020d}.json"
    else:
        require(owner_kind == "task", "lifetime owner kind is invalid")
        names = {
            "intent": "intent.json",
            "registration": "registration.json",
            "worker": "worker.json",
            "worker-wait": "worker-wait.json",
            "termination": "terminated.json",
        }
        if record == "launch":
            require(sequence is not None, "ordinary task lifetime launch sequence is absent")
            return f"{sequence:020d}.launch.json"
    require(record in names, "lifetime record kind is invalid")
    return names[record]


def _lifetime_launch_sequence(owner_kind: str, name: str) -> int | None:
    if owner_kind == "inspection":
        match = re.fullmatch(r"lifetime-launch-([0-9]{20})\.json", name)
    else:
        require(owner_kind == "task", "lifetime owner kind is invalid")
        match = re.fullmatch(r"([0-9]{20})\.launch\.json", name)
    if match is None:
        return None
    sequence = int(match.group(1))
    return sequence if sequence > 0 else None


def _validate_managed_lifetime(
        directory: Path, root: str, task: str, owner_kind: str, owner_digest: str,
        created_floor: tuple[datetime, int] | None = None,
        expected_deadline: str | None = None,
        window_deadline: tuple[datetime, int] | None = None) -> dict[str, object]:
    """Validate one owner's complete managed-worker and empty-scope proof."""
    evidence = "inspection" if owner_kind == "inspection" else "ordinary task"
    require(owner_kind in {"inspection", "task"} and _hex_digest(owner_digest),
            f"{evidence} lifetime owner is invalid for {task}")
    require(directory.is_dir() and not directory.is_symlink(),
            f"{evidence} lifetime directory is invalid for {task}")

    intent_path = directory / _lifetime_record_name(owner_kind, "intent")
    intent = _read_control_record(intent_path, f"{evidence} lifetime intent for {task}")
    _exact_keys(intent, {
        "schema_version", "root_id", "task_id", "owner_kind", "owner_sha256",
        "scope_kind", "assurance", "scope_plan", "boot_id", "deadline", "created_at",
    }, label=f"{evidence} lifetime intent for {task}")
    require(type(intent.get("schema_version")) is int and intent["schema_version"] == 1 and
            intent.get("root_id") == root and intent.get("task_id") == task and
            intent.get("owner_kind") == owner_kind and
            intent.get("owner_sha256") == owner_digest,
            f"{evidence} lifetime intent binding mismatch for {task}")
    scope_kind = intent.get("scope_kind")
    assurance = intent.get("assurance")
    require((scope_kind == "cgroup-v2" and assurance == "exact") or
            (scope_kind == "process-group" and assurance == "cooperative"),
            f"{evidence} lifetime intent scope is invalid for {task}")
    intent_deadline = _canonical_timestamp(
        intent.get("deadline"), f"{evidence} lifetime intent deadline for {task}")
    require(_lifetime_token(intent.get("scope_plan")) and
            _lifetime_token(intent.get("boot_id")) and
            (expected_deadline is None or intent.get("deadline") == expected_deadline) and
            (window_deadline is None or intent_deadline == window_deadline),
            f"{evidence} lifetime intent facts are invalid for {task}")
    intent_created = _canonical_timestamp(
        intent.get("created_at"), f"{evidence} lifetime intent created_at for {task}")
    require((created_floor is None or _timestamp_at_or_after(intent_created, created_floor)) and
            (window_deadline is None or _timestamp_before(intent_created, window_deadline)),
            f"{evidence} lifetime intent time is invalid for {task}")

    registration_path = directory / _lifetime_record_name(owner_kind, "registration")
    registration = _read_control_record(
        registration_path, f"{evidence} lifetime registration for {task}")
    _exact_keys(registration, {
        "schema_version", "root_id", "task_id", "owner_kind", "owner_sha256",
        "scope_kind", "assurance", "scope_id", "boot_id", "deadline", "created_at",
    }, label=f"{evidence} lifetime registration for {task}")
    require(type(registration.get("schema_version")) is int and
            registration["schema_version"] == 1 and
            registration.get("root_id") == root and registration.get("task_id") == task and
            registration.get("owner_kind") == owner_kind and
            registration.get("owner_sha256") == owner_digest and
            registration.get("scope_kind") == scope_kind and
            registration.get("assurance") == assurance and
            registration.get("boot_id") == intent.get("boot_id") and
            registration.get("deadline") == intent.get("deadline") and
            _lifetime_token(registration.get("scope_id")),
            f"{evidence} lifetime registration binding mismatch for {task}")
    registration_created = _canonical_timestamp(
        registration.get("created_at"), f"{evidence} lifetime registration created_at for {task}")
    require(_timestamp_at_or_after(registration_created, intent_created) and
            (window_deadline is None or _timestamp_before(registration_created, window_deadline)),
            f"{evidence} lifetime registration time is invalid for {task}")
    registration_digest = digest(registration_path)

    worker_path = directory / _lifetime_record_name(owner_kind, "worker")
    worker = _read_optional_control_record(worker_path, f"{evidence} lifetime worker for {task}")
    worker_started = registration_created
    worker_digest = ""
    if worker is not None:
        _exact_keys(worker, {
            "schema_version", "root_id", "task_id", "registration_sha256", "pid",
            "process_start", "boot_id", "started_at",
        }, label=f"{evidence} lifetime worker for {task}")
        worker_pid = worker.get("pid")
        require(type(worker.get("schema_version")) is int and worker["schema_version"] == 1 and
                worker.get("root_id") == root and worker.get("task_id") == task and
                worker.get("registration_sha256") == registration_digest and
                type(worker_pid) is int and worker_pid > 0 and
                _lifetime_token(worker.get("process_start")) and
                worker.get("boot_id") == registration.get("boot_id"),
                f"{evidence} lifetime worker binding mismatch for {task}")
        worker_started = _canonical_timestamp(
            worker.get("started_at"), f"{evidence} lifetime worker started_at for {task}")
        require(_timestamp_at_or_after(worker_started, registration_created),
                f"{evidence} lifetime worker time is invalid for {task}")
        worker_digest = digest(worker_path)

    wait = _read_optional_control_record(
        directory / _lifetime_record_name(owner_kind, "worker-wait"),
        f"{evidence} lifetime worker Wait for {task}")
    if wait is not None:
        _exact_keys(wait, {
            "schema_version", "root_id", "task_id", "registration_sha256",
            "child_pid", "result", "observed_at",
        }, {"worker_sha256", "exit_code"}, label=f"{evidence} lifetime worker Wait for {task}")
        wait_result = wait.get("result")
        exit_code = wait.get("exit_code")
        worker_sha = wait.get("worker_sha256", "")
        child_pid = wait.get("child_pid")
        started_wait = (
            (wait_result == "success" and type(exit_code) is int and exit_code == 0) or
            (wait_result == "exit-error" and type(exit_code) is int and exit_code != 0) or
            (wait_result == "wait-error" and (exit_code is None or type(exit_code) is int)))
        not_started = (wait_result == "not-started" and worker_sha == "" and
                       child_pid == 0 and exit_code is None)
        require(type(wait.get("schema_version")) is int and wait["schema_version"] == 1 and
                wait.get("root_id") == root and wait.get("task_id") == task and
                wait.get("registration_sha256") == registration_digest and
                (worker_sha == "" or _hex_digest(worker_sha)) and
                type(child_pid) is int and (started_wait or not_started),
                f"{evidence} lifetime worker Wait binding mismatch for {task}")
        if worker is None:
            require(not_started or
                    (assurance == "exact" and worker_sha == "" and child_pid > 0),
                    f"{evidence} lifetime worker Wait binding mismatch for {task}")
        else:
            require(worker_sha == worker_digest and child_pid == worker["pid"] and started_wait,
                    f"{evidence} lifetime worker Wait binding mismatch for {task}")
        wait_observed = _canonical_timestamp(
            wait.get("observed_at"), f"{evidence} lifetime worker Wait observed_at for {task}")
        require(_timestamp_at_or_after(wait_observed, worker_started),
                f"{evidence} lifetime worker Wait time is invalid for {task}")

    launches: list[dict[str, object]] = []
    try:
        sequences = sorted(sequence for entry in directory.iterdir()
                           if (sequence := _lifetime_launch_sequence(
                               owner_kind, entry.name)) is not None)
    except OSError as error:
        raise AcceptanceFailure(
            f"cannot inspect {evidence} lifetime launches for {task}") from error
    require(len(sequences) <= MAX_LIFETIME_LAUNCHES and
            sequences == list(range(1, len(sequences) + 1)),
            f"{evidence} lifetime launch sequence is invalid for {task}")
    for sequence in sequences:
        launch = _read_control_record(
            directory / _lifetime_record_name(owner_kind, "launch", sequence),
            f"{evidence} lifetime launch {sequence} for {task}")
        _exact_keys(launch, {
            "schema_version", "root_id", "task_id", "registration_sha256", "sequence",
            "pid", "process_start", "started_at",
        }, {"pgid"}, label=f"{evidence} lifetime launch {sequence} for {task}")
        pid = launch.get("pid")
        pgid = launch.get("pgid", 0)
        require(type(launch.get("schema_version")) is int and launch["schema_version"] == 1 and
                launch.get("root_id") == root and launch.get("task_id") == task and
                launch.get("registration_sha256") == registration_digest and
                type(launch.get("sequence")) is int and launch["sequence"] == sequence and
                type(pid) is int and pid > 0 and
                type(pgid) is int and pgid >= 0 and _lifetime_token(launch.get("process_start")),
                f"{evidence} lifetime launch binding mismatch for {task}")
        launched_at = _canonical_timestamp(
            launch.get("started_at"), f"{evidence} lifetime launch started_at for {task}")
        require(_timestamp_at_or_after(launched_at, registration_created) and
                (window_deadline is None or _timestamp_before(launched_at, window_deadline)),
                f"{evidence} lifetime launch time is invalid for {task}")
        launches.append(launch)

    termination = _read_control_record(
        directory / _lifetime_record_name(owner_kind, "termination"),
        f"{evidence} lifetime termination for {task}")
    _exact_keys(termination, {
        "schema_version", "root_id", "task_id", "registration_sha256", "launch_count",
        "launch_set_sha256", "cause", "scope_empty", "observed_at",
    }, label=f"{evidence} lifetime termination for {task}")
    canonical_launches = []
    for launch in launches:
        canonical_launch = {
            "schema_version": launch["schema_version"],
            "root_id": launch["root_id"],
            "task_id": launch["task_id"],
            "registration_sha256": launch["registration_sha256"],
            "sequence": launch["sequence"],
            "pid": launch["pid"],
            "process_start": launch["process_start"],
        }
        if launch.get("pgid", 0) != 0:
            canonical_launch["pgid"] = launch["pgid"]
        canonical_launch["started_at"] = launch["started_at"]
        canonical_launches.append(canonical_launch)
    launch_set_digest = sha(canonical_go_json(canonical_launches))
    require(type(termination.get("schema_version")) is int and
            termination["schema_version"] == 1 and
            termination.get("root_id") == root and termination.get("task_id") == task and
            termination.get("registration_sha256") == registration_digest and
            type(termination.get("launch_count")) is int and
            termination["launch_count"] == len(launches) and
            termination.get("launch_set_sha256") == launch_set_digest and
            termination.get("cause") in {
                "completed", "budget", "requested", "recovery", "previous-boot"} and
            termination.get("scope_empty") is True,
            f"{evidence} lifetime termination binding mismatch for {task}")
    terminated_at = _canonical_timestamp(
        termination.get("observed_at"), f"{evidence} lifetime termination observed_at for {task}")
    require(_timestamp_at_or_after(terminated_at, registration_created),
            f"{evidence} lifetime termination time is invalid for {task}")
    require(wait is not None or termination.get("cause") == "previous-boot",
            f"{evidence} lifetime worker Wait is missing for {task}")
    return {"intent": intent, "registration": registration, "worker": worker,
            "worker_wait": wait, "launches": launches, "termination": termination}


def _validate_managed_inspection_lifetime(directory: Path, root: str, task: str,
                                          request_digest: str,
                                          request_created: tuple[datetime, int],
                                          request_deadline: str,
                                          deadline: tuple[datetime, int]) -> dict[str, object]:
    """Validate the guardian-owned Wait and empty-scope proof for one inspection."""
    return _validate_managed_lifetime(
        directory, root, task, "inspection", request_digest,
        created_floor=request_created, expected_deadline=request_deadline,
        window_deadline=deadline)


def ordinary_lifetime_owner_digest(spec_digest: str, meta_digest: str) -> str:
    """Match task.LifetimeTaskOwnerSHA256's domain-separated owner identity."""
    require(_hex_digest(spec_digest) and _hex_digest(meta_digest),
            "ordinary task lifetime owner inputs are invalid")
    material = ("delegation-layer:lifetime-owner:task:v1\n" + spec_digest +
                "\n" + meta_digest).encode("ascii")
    return sha(material)


def _validate_managed_ordinary_lifetime(directory: Path, root: str, task: str,
                                        spec_digest: str,
                                        meta_digest: str) -> dict[str, object]:
    """Validate taskdir's managed lifetime records for one ordinary task."""
    return _validate_managed_lifetime(
        directory / "lifetime", root, task, "task",
        ordinary_lifetime_owner_digest(spec_digest, meta_digest))


def _ordinary_lifetime_control_started(directory: Path) -> bool:
    """Return whether durable managed-lifetime evidence was published."""
    lifetime = directory / "lifetime"
    try:
        lifetime.lstat()
    except FileNotFoundError:
        return False
    except OSError as error:
        raise AcceptanceFailure("cannot inspect ordinary task lifetime records") from error
    require(lifetime.is_dir() and not lifetime.is_symlink(),
            "ordinary task lifetime path is not a regular directory")
    try:
        entries = list(lifetime.iterdir())
    except OSError as error:
        raise AcceptanceFailure("cannot inspect ordinary task lifetime records") from error
    for entry in entries:
        if entry.name == "owner.lock":
            require(entry.is_file() and not entry.is_symlink(),
                    "ordinary task lifetime owner lock is not a regular file")
            continue
        # intent.json is the first durable authorization record. Any other
        # entry must also enter full validation so malformed or partial
        # journals fail closed rather than being mistaken for an unused lock.
        return True
    return False


def _ordinary_supervisor_handoff_started(directory: Path) -> bool:
    """Return whether the supervisor durably accepted the staged task."""
    receipt = directory / "supervisor.ref.json"
    try:
        info = receipt.lstat()
    except FileNotFoundError:
        return False
    except OSError as error:
        raise AcceptanceFailure("cannot inspect ordinary supervisor receipt") from error
    require(stat.S_ISREG(info.st_mode) and not receipt.is_symlink(),
            "ordinary supervisor receipt is not a regular file")
    return True


def _state_root_identity(state: Path, label: str) -> tuple[str, tuple[datetime, int]]:
    """Read the immutable root identity used by task and inspection records."""
    root = _read_control_record(state / "root.json", label)
    _exact_keys(root, {"schema_version", "root_id", "created_at"}, label=label)
    require(type(root.get("schema_version")) is int and root["schema_version"] == 1,
            f"{label} schema mismatch")
    created_at = _canonical_timestamp(root.get("created_at"), f"{label} created_at")
    root_id = root.get("root_id")
    require(_task_identity(root_id), f"{label} has no valid root identity")
    return root_id, created_at


def _scan_managed_ordinary_lifetime_closure(
        state: Path, root: str, task_ids: Iterable[str] = ()) -> dict[str, dict[str, object]]:
    """Scan ordinary state and reuse the canonical task lifetime validator."""
    requested = set(task_ids)
    require(all(isinstance(task, str) and _task_identity(task) for task in requested),
            "ordinary task lifetime task identity is invalid")
    tasks_parent = state / "tasks"
    try:
        entries = tuple(tasks_parent.iterdir())
    except FileNotFoundError:
        require(not requested, "ordinary task state is missing")
        return {}
    except OSError as error:
        raise AcceptanceFailure("cannot inspect ordinary task state") from error
    require(tasks_parent.is_dir() and not tasks_parent.is_symlink(),
            "ordinary task state is not a regular directory")

    candidates = set(requested)
    for entry in entries:
        require(_task_identity(entry.name),
                "ordinary task state contains an invalid task identity")
        require(entry.is_dir() and not entry.is_symlink(),
                f"ordinary task directory is invalid for {entry.name}")
        candidates.add(entry.name)

    closures: dict[str, dict[str, object]] = {}
    for task in sorted(candidates):
        directory = tasks_parent / task
        require(directory.is_dir() and not directory.is_symlink(),
                f"ordinary task directory is missing for {task}")
        meta_path = directory / "meta.json"
        meta = _read_control_record(meta_path, f"ordinary task metadata for {task}")
        ownership = meta.get("runner_ownership")
        require(ownership is None or ownership in {"managed", "custom"},
                f"ordinary task runner ownership is invalid for {task}")
        lifetime_started = _ordinary_lifetime_control_started(directory)
        handoff_started = _ordinary_supervisor_handoff_started(directory)
        if ownership == "managed" and not lifetime_started and not handoff_started:
            # A managed runner recorded in metadata is only a launch plan. The
            # immutable supervisor receipt is the handoff authority. Without
            # either that receipt or a lifetime journal, no worker was owned.
            continue
        if ownership != "managed" and not lifetime_started:
            continue

        task_path = directory / "task.json"
        require(task_path.is_file() and not task_path.is_symlink(),
                f"ordinary task record is missing for {task}")
        require(meta_path.is_file() and not meta_path.is_symlink(),
                f"ordinary task metadata is invalid for {task}")
        closure = _validate_managed_ordinary_lifetime(
            directory, root, task, digest(task_path), digest(meta_path))
        closures[task] = closure
    return closures


def _scan_managed_inspection_lifetime_closure(
        state: Path, root: str) -> dict[str, dict[str, object]]:
    """Scan inspection state and reuse the canonical inspection lifetime validator."""
    inspections_parent = state / "inspections"
    try:
        inspections_parent.lstat()
    except FileNotFoundError:
        return {}
    except OSError as error:
        raise AcceptanceFailure("cannot inspect managed inspection state") from error
    require(inspections_parent.is_dir() and not inspections_parent.is_symlink(),
            "managed inspection state is not a regular directory")

    closures: dict[str, dict[str, object]] = {}
    try:
        entries = tuple(inspections_parent.iterdir())
    except OSError as error:
        raise AcceptanceFailure("cannot inspect managed inspection state") from error
    for directory in entries:
        require(_task_identity(directory.name),
                "managed inspection state contains an invalid task identity")
        require(directory.is_dir() and not directory.is_symlink(),
                f"managed inspection directory is invalid for {directory.name}")
        task = directory.name
        request_path = directory / "request.json"
        request = _read_control_record(request_path, f"inspection request for {task}")
        require(type(request.get("schema_version")) is int and
                request["schema_version"] == 1 and request.get("root_id") == root and
                request.get("task_id") == task,
                f"inspection request identity mismatch for {task}")
        binding = request.get("binding")
        _validate_inspection_binding(binding, f"inspection request binding for {task}")
        created = _canonical_timestamp(
            request.get("created_at"), f"inspection request created_at for {task}")
        request_deadline = request.get("deadline")
        deadline = _canonical_timestamp(
            request_deadline, f"inspection request deadline for {task}")

        if not _inspection_managed_authority_started(directory):
            # A request and its task-local proof reference are intent-only. No
            # managed worker or scope exists until a lifecycle authority is
            # durably published.
            _validate_unsubmitted_inspection_records(directory, task)
            continue
        authority = _inspection_exit_authority(
            directory, root, task, binding, digest(request_path), created,
            request_deadline, deadline)
        if authority["mode"] == "queue":
            # Historical and custom inspections retain their queue-owned
            # observation contract until a durable managed lifetime marker exists.
            continue
        require(authority["mode"] == "guardian" and
                isinstance(authority.get("managed_lifetime"), dict),
                f"managed inspection lifetime evidence is absent for {task}")
        closures[task] = authority["managed_lifetime"]
    return closures


def validate_managed_lifetime_closure(
        state: Path, task_ids: Iterable[str] = ()) -> dict[str, object]:
    """Authorize cleanup from every managed lifetime under one state root.

    The state root is authoritative: task and inspection directories are
    scanned instead of relying on queue rows or dispatch response files.
    Historical and custom inspections without a durable managed lifetime
    marker remain under their existing queue-observation contract.
    """
    state = Path(state)
    root, _ = _state_root_identity(state, "managed lifetime root record")
    return {
        "root_id": root,
        "tasks": _scan_managed_ordinary_lifetime_closure(state, root, task_ids),
        "inspections": _scan_managed_inspection_lifetime_closure(state, root),
    }


def _inspection_lifetime_control_started(directory: Path) -> bool:
    """Detect any managed lifetime journal before choosing historical fallback."""
    try:
        for entry in directory.iterdir():
            if entry.name.startswith("lifetime-") and entry.name.endswith(".json"):
                return True
    except OSError as error:
        raise AcceptanceFailure("cannot inspect managed inspection lifetime records") from error
    return False


def _inspection_managed_authority_started(directory: Path) -> bool:
    """Return whether the inspection published a lifecycle authority.

    The immutable request is admission intent, and a proof reference is a
    task-local pointer to already-materialized facts. Neither creates a local
    worker or a managed scope. Submission, receipt, start, result, completion,
    observation, upgrade, or lifetime records do establish that the operation
    advanced into a lifecycle that must have a closure proof.
    """
    lifecycle_records = (
        "submission.json",
        "receipt.json",
        "start.json",
        "result.json",
        "completion.json",
        "worker-observation.json",
        "worker-upgrade.json",
    )
    for name in lifecycle_records:
        try:
            (directory / name).lstat()
        except FileNotFoundError:
            continue
        except OSError as error:
            raise AcceptanceFailure(
                "cannot inspect managed inspection lifecycle records") from error
        return True
    return _inspection_lifetime_control_started(directory)


def _validate_unsubmitted_inspection_records(directory: Path, task: str) -> None:
    """Allow only intent and an optional opaque proof reference before submit."""
    allowed = {"request.json", "proof-reference.json"}
    try:
        entries = tuple(directory.iterdir())
    except OSError as error:
        raise AcceptanceFailure(
            "cannot inspect unsubmitted managed inspection records") from error
    unexpected = sorted(entry.name for entry in entries if entry.name not in allowed)
    require(not unexpected,
            f"unsubmitted inspection has unexpected records for {task}: " +
            ", ".join(unexpected))
    proof_reference = directory / "proof-reference.json"
    if proof_reference.exists() or proof_reference.is_symlink():
        _read_control_record(proof_reference, f"inspection proof reference for {task}")


def _inspection_exit_authority(directory: Path, root: str, task: str,
                               binding: dict[str, object], request_digest: str,
                               request_created: tuple[datetime, int],
                               request_deadline: str,
                               deadline: tuple[datetime, int]) -> dict[str, object]:
    """Choose the sole durable exit-proof policy for one inspection journal.

    Fresh inspections prove the child Wait and independently empty scope
    through the guardian. Markerless journals retain the historical queue
    observation only when no managed lifetime record was ever started. Once any
    lifetime record exists, incomplete evidence is a managed evidence fault and
    cannot be replaced by a terminal queue row.
    """
    lifetime_started = _inspection_lifetime_control_started(directory)
    managed = (binding.get("worker_exit_proof") == "guardian-wait-v1" or lifetime_started)
    if not managed:
        return {"mode": "queue", "managed_lifetime": None}
    if not lifetime_started:
        return {"mode": "guardian-unstarted", "managed_lifetime": None}
    lifetime = _validate_managed_inspection_lifetime(
        directory, root, task, request_digest, request_created, request_deadline, deadline)
    return {"mode": "guardian", "managed_lifetime": lifetime}


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
            _required_keys(groups["default"], _PUEUE_GROUP_KEYS, label="private queue default group")
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
                 expected_inspection_binding_required: bool = True,
                 managed_runner: bool = False) -> None:
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
        require(type(managed_runner) is bool, "managed runner mode is invalid")
        self.expected_inspection_binding_required = expected_inspection_binding_required
        self.managed_runner = managed_runner
        self.pueue_config: Path | None = None
        self.daemon: object | None = None
        self.root_id: str | None = None
        self.root_created_at: tuple[datetime, int] | None = None
        self.tasks: dict[str, str] = {}
        self.labels: dict[str, str] = {}
        self.numbers: dict[str, int] = {}
        self.expected_results: dict[str, str] = {}
        self.dispatch_attempts: set[str] = set()
        self.dispatch_observations: list[dict[str, object]] = []
        self.inspection_attempts: set[str] = set()
        self.inspection_bindings: dict[str, dict[str, object]] = {}
        self.queue_before_replay: dict[str, object] | None = None
        self.closed = False

    def _ordinary_runner_executable(self, task: str) -> Path:
        if not self.managed_runner:
            return self.runner
        meta = _read_control_record(
            self.state / "tasks" / task / "meta.json",
            f"ordinary task metadata for {task}")
        executable = meta.get("runner_executable")
        require(meta.get("runner_ownership") == "managed" and
                isinstance(executable, str) and Path(executable).is_absolute() and
                Path(executable) == Path(os.path.normpath(executable)),
                f"ordinary task managed runner binding is invalid for {task}")
        runner = Path(executable)
        require(runner.is_file() and not runner.is_symlink() and
                digest(runner) == digest(self.runner),
                f"ordinary task managed runner executable changed for {task}")
        return runner

    def _ordinary_lifetime_validation_required(self, task: str) -> bool:
        """Detect managed ordinary work without changing markerless history."""
        directory = self.state / "tasks" / task
        if self.managed_runner or _ordinary_lifetime_control_started(directory):
            return True
        meta_path = directory / "meta.json"
        if not meta_path.exists() and not meta_path.is_symlink():
            return False
        meta = _read_control_record(meta_path, f"ordinary task metadata for {task}")
        ownership = meta.get("runner_ownership")
        require(ownership is None or ownership in {"managed", "custom"},
                f"ordinary task runner ownership is invalid for {task}")
        return ownership == "managed"

    def validate_ordinary_queue_row(
            self, row: dict[str, object], label: str, task: str,
            numeric: int | None = None, group: str | None = None) -> None:
        validate_queue_row(
            row, label, self._ordinary_runner_executable(task), self.state, task,
            numeric, group, supervised=self.managed_runner)
        if done_result(row) is None or not self._ordinary_lifetime_validation_required(task):
            return
        durable = self._durable_ordinary_admission(task, None)
        require(durable is not None and durable.get("task_id") == task and
                durable.get("label") == label and
                (numeric is None or durable.get("numeric_task_id") == numeric) and
                isinstance(durable.get("managed_lifetime"), dict),
                f"ordinary task lacks authoritative lifetime closure for {task}")

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
                                     response: dict[str, object] | None,
                                     command: str = "dispatch") -> None:
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
        write_json(self.output / (name + "-" + command + "-attempt.json"),
                   {**observation, "receipt_written": True})
        observation["receipt_written"] = True

    def record_admission(self, name: str, task: str, response: dict[str, object],
                         command: str = "dispatch") -> dict[str, object]:
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
        self.expected_results[name] = "Success"
        write_json(self.output / (name + "-" + command + ".json"), response)
        return response

    def continue_task(self, name: str, argv: list[object]) -> tuple[str, dict[str, object]]:
        """Admit one public continuation after Delegate assigns its successor ID."""
        require(not self.closed, "acceptance supervisor is closed")
        process = self.processes.run(name, argv, self.state_parent,
                                     expected=None, timeout=45)
        response = parse_json_output(process, name)
        task = response.get("task_id")
        require(isinstance(task, str) and _task_identity(task),
                f"{name} continuation has no valid successor task identity")
        require(task not in self.dispatch_attempts,
                "continuation reused an existing task identity")
        self.dispatch_attempts.add(task)
        if response.get("admission") == "admitted":
            try:
                admitted = self.record_admission(name, task, response, command="continue")
            except BaseException:
                self._record_dispatch_observation(name, task, process, response, command="continue")
                raise
            self._record_dispatch_observation(name, task, process, response, command="continue")
            result = getattr(process, "result", None)
            require(isinstance(result, dict) and result.get("natural_wait") is True and
                    type(result.get("exit_code")) is int and result.get("exit_code") == 0,
                    f"{name} continuation did not exit successfully")
            return task, admitted
        self._record_dispatch_observation(name, task, process, response, command="continue")
        return task, self.record_admission(name, task, response, command="continue")

    def expect_terminal_result(self, name: str, result: str) -> None:
        require(name in self.tasks, f"terminal result has no admitted task: {name}")
        require(result in {"Success", "Killed", "Errored", "DependencyFailed", "Failed", "FailedToSpawn"},
                "expected terminal result is invalid")
        self.expected_results[name] = result

    def wait_task(self, name: str, task: str,
                  expected_result: str | frozenset[str] | None = None) -> dict[str, object]:
        require(self.root_id is not None, "root identity is unavailable")
        if expected_result is None:
            expected_result = self.expected_results.get(name, "Success")
        row = wait_runner_terminal(self.client, self.root_id, task, expected_result,
                                   timeout=self.watch_seconds,
                                   sleep_fn=time.sleep, monotonic_fn=time.monotonic)
        require(isinstance(row, dict), "runner completion row is malformed")
        require(name in self.numbers, f"runner task {task} has no recorded numeric identity")
        self.validate_ordinary_queue_row(
            row, self.labels[name], task, self.numbers[name], "default")
        write_json(self.output / (name + "-done.json"), row)
        return row

    def collect(self, name: str, task: str, expected: int | set[int] = 0) -> dict[str, object]:
        require(self.pueue_config is not None, "pueue is not configured")
        process = self.direct(name, [self.delegate, "--root", self.state,
                                     "--pueue-config", self.pueue_config,
                                     "--runner", self.runner, "collect", task,
                                     "--watch", "0s", "--json"], expected=expected, timeout=45)
        response = parse_json_output(process, name)
        require(response.get("task_id") == task, f"{name} collected the wrong task")
        write_json(self.output / (name + ".json"), response)
        return response

    def status(self, name: str, task: str,
               expected: int | set[int] = 0, timeout: float = 45) -> dict[str, object]:
        require(self.pueue_config is not None, "pueue is not configured")
        require(timeout > 0, "status observation timeout must be positive")
        process = self.direct(name, [self.delegate, "--root", self.state,
                                     "--pueue-config", self.pueue_config,
                                     "--runner", self.runner, "status", task,
                                     "--json"], expected=expected, timeout=timeout)
        response = parse_json_output(process, name)
        require(response.get("task_id") == task, f"{name} observed the wrong task")
        write_json(self.output / (name + ".json"), response)
        return response

    def queue_status(self, name: str) -> dict[str, object]:
        status = status_jobs(self.client(name, ["status", "--json"], timeout=20))
        write_json(self.output / (name + ".json"), status)
        return status

    def _known_root_id(self) -> str:
        value, root_created = _state_root_identity(self.state, "inspection root record")
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
        timeout_seconds = (MODEL_DISCOVERY_TIMEOUT_SECONDS if standalone
                           else ADMISSION_INSPECTION_TIMEOUT_SECONDS)
        require(deadline[0] - created[0] in {timedelta(seconds=value) for value in timeout_seconds} and
                deadline[1] == created[1],
                f"inspection request deadline is not exactly {timeout_seconds[0]} seconds for {task}")

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
                supervisor.get("config_digest") == digest(self.pueue_config),
                f"inspection request supervisor binding mismatch for {task}")
        supervisor_base = self.pueue_config.parent
        require(supervisor.get("resolved_config_sha256") ==
                resolved_supervisor_config_digest(supervisor_base) and
                supervisor.get("endpoint") ==
                "unix:" + str(supervisor_base / "run" / "p.sock"),
                f"inspection request supervisor generation mismatch for {task}")
        observed_pueue_version(supervisor.get("observed_version"), "pueue")

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
        authority = _inspection_exit_authority(
            directory, root, task, binding, request_digest, created,
            request["deadline"], deadline)
        managed_lifetime = authority["managed_lifetime"]
        require(isinstance(managed_lifetime, dict) or managed_lifetime is None,
                f"inspection exit authority is invalid for {task}")
        managed_worker_succeeded = False
        if authority["mode"] in {"guardian", "guardian-unstarted"}:
            require(observation is None,
                    f"managed inspection has conflicting queue observation for {task}")
        if authority["mode"] == "guardian":
            managed_wait = managed_lifetime["worker_wait"]
            require(isinstance(managed_wait, dict),
                    f"inspection lifetime worker Wait is invalid for {task}")
            managed_worker_succeeded = managed_wait.get("result") == "success"
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
                require(not ordinary_present and observation is None and
                        (managed_lifetime is None or not managed_worker_succeeded),
                        f"failed inspection has an ordinary task or observation for {task}")
            elif result.get("reason") == "eligible":
                require((standalone or ordinary_present) and completion is not None and
                        completion.get("native_exit") == "successful-exit" and
                        ((managed_lifetime is not None and managed_worker_succeeded) or
                         observation is not None),
                        f"eligible inspection lacks independent success proof for {task}")
            else:
                require(not ordinary_present and observation is None and
                        completion is not None and completion.get("native_exit") == "unavailable" and
                        (managed_lifetime is None or not managed_worker_succeeded),
                        f"failed inspection has conflicting success proof for {task}")
        else:
            require(result is not None and result.get("reason") == "eligible" and
                    (standalone or ordinary_present) and completion is not None and
                    completion.get("native_exit") == "successful-exit" and
                    ((managed_lifetime is not None and managed_worker_succeeded) or
                     observation is not None),
                    f"successful inspection proof is incomplete for {task}")
            if managed_lifetime is not None:
                termination = managed_lifetime["termination"]
                require(isinstance(termination, dict) and
                        termination.get("cause") == "completed" and
                        _timestamp_before(_canonical_timestamp(
                            termination.get("observed_at"),
                            f"inspection lifetime termination observed_at for {task}"), deadline),
                        f"successful inspection lifetime proof expired for {task}")
        return {"task_id": task, "root_id": root, "group": group, "label": label,
                "numeric_task_id": receipt_id, "request": request, "request_digest": request_digest,
                "receipt": receipt, "submission": submission, "start": start,
                "result": result, "completion": completion,
                "worker_observation": observation,
                "managed_lifetime": managed_lifetime,
                "inspection_exit_authority": authority,
                "ordinary_task_present": ordinary_present}

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
            _required_keys(group, _PUEUE_GROUP_KEYS, label=f"private queue group {name}")
            require(group.get("status") in {"Running", "Paused", "Reset"} and
                    type(group.get("parallel_tasks")) is int and
                    group["parallel_tasks"] >= 0,
                    f"private queue group {name} is malformed")

    def _validate_inspection_queue_row(self, row: dict[str, object], journal: dict[str, object],
                                       allow_failure: bool,
                                       terminal_required: bool = True) -> None:
        task = journal["task_id"]
        label = journal["label"]
        require(row.get("label") == label and row.get("group") == journal["group"],
                f"inspection queue identity mismatch for {task}")
        require(type(row.get("id")) is int and
                row.get("id") == journal["numeric_task_id"],
                f"inspection queue numeric identity mismatch for {task}")
        row_path = row.get("path")
        require(self.pueue_config is not None,
                "inspection supervisor config is unavailable")
        # A fresh client uses the invocation directory. A client reconstructed
        # from its durable binding intentionally uses the config directory so
        # recovery cannot depend on an ambient directory that may be gone.
        expected_paths = {self.state_parent, self.pueue_config.parent}
        require(isinstance(row_path, str) and Path(row_path) in expected_paths,
                f"inspection queue working directory mismatch for {task}")
        require(type(terminal_required) is bool, "inspection queue terminal mode is invalid")
        state = row_state(row)
        if terminal_required:
            require(state == "Done", f"inspection worker is not Done for {task}")
        result = done_result(row)
        inspection_result = journal["result"]
        successful_result = (isinstance(inspection_result, dict) and
                             inspection_result.get("reason") == "eligible")
        if terminal_required:
            expected_result = "Success" if not allow_failure or successful_result else None
            if expected_result == "Success":
                require(result == "Success", f"inspection worker did not succeed for {task}")
            else:
                require(result in INSPECTION_FAILURE_RESULTS,
                        f"inspection worker failure was not positively observed for {task}")
        expected = [str(self.runner), "--inspection", "--guardian", "--supervised",
                    "--root", str(self.state), str(task)]
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

    def _durable_ordinary_admission(
            self, task: str,
            journal: dict[str, object] | None) -> dict[str, object] | None:
        """Read one immutable ordinary submission when its caller response was lost."""
        directory = self.state / "tasks" / task
        present = directory.exists() or directory.is_symlink()
        if not present:
            return None
        require(directory.is_dir() and not directory.is_symlink(),
                f"ordinary task directory is invalid for {task}")
        root = self._known_root_id()

        task_path = directory / "task.json"
        task_record = _read_control_record(task_path, f"ordinary task record for {task}")
        spec_digest = _validate_task_record(
            task_record, root, task, f"ordinary task record for {task}")
        require(spec_digest == digest(task_path),
                f"ordinary task record is not canonical for {task}")
        brief_path = directory / "brief.md"
        require(brief_path.is_file() and not brief_path.is_symlink(),
                f"ordinary task brief is not a regular file for {task}")
        require(brief_path.stat().st_size == task_record["brief_length"] and
                brief_path.stat().st_size <= MAX_PROVIDER_BYTES and
                digest(brief_path) == task_record["brief_sha256"],
                f"ordinary task brief binding is invalid for {task}")

        journal_binding = None
        if journal is not None:
            require(journal.get("ordinary_task_present") is True and
                    journal.get("task_id") == task and journal.get("root_id") == root,
                    f"inspection journal does not bind ordinary task {task}")
            request = journal["request"]
            require(request.get("task") == task_record and
                    request.get("task_sha256") == spec_digest,
                    f"inspection journal task binding changed for {task}")
            journal_binding = request["binding"]["supervisor"]

        meta_path = directory / "meta.json"
        meta = _read_control_record(meta_path, f"ordinary task metadata for {task}")
        _exact_keys(meta, _META_REQUIRED_KEYS, _META_OPTIONAL_KEYS,
                    label=f"ordinary task metadata for {task}")
        validate_provider_runtime_metadata(
            meta, label=f"ordinary task provider runtime metadata for {task}")
        require(type(meta.get("schema_version")) is int and meta["schema_version"] == 1 and
                meta.get("root_id") == root and meta.get("task_id") == task and
                meta.get("spec_sha256") == spec_digest and
                meta.get("requested_config") == task_record["requested_config"],
                f"ordinary task metadata identity is invalid for {task}")
        effective = meta.get("effective_config")
        require(isinstance(effective, dict),
                f"ordinary task effective configuration is invalid for {task}")
        _exact_keys(effective, _EFFECTIVE_CONFIG_KEYS, {"policy"},
                    label=f"ordinary task effective configuration for {task}")
        require(isinstance(effective.get("containment"), str) and effective["containment"] and
                isinstance(effective.get("approval"), str) and effective["approval"] and
                _hex_digest(effective.get("digest")) and
                meta.get("containment") == effective["containment"] and
                meta.get("approval") == effective["approval"],
                f"ordinary task effective configuration changed for {task}")
        if "policy" in effective:
            require(isinstance(effective["policy"], dict),
                    f"ordinary task policy is invalid for {task}")
        for key in ("provider_executable", "runner_executable"):
            if key == "runner_executable" and key not in meta:
                continue
            executable = meta.get(key)
            require(isinstance(executable, str) and Path(executable).is_absolute() and
                    Path(executable) == Path(os.path.normpath(executable)),
                    f"ordinary task {key} is invalid for {task}")
        if "runner_ownership" in meta:
            require(meta["runner_ownership"] in {"managed", "custom"},
                    f"ordinary task runner ownership is invalid for {task}")
        for key in ("provider_version", "publisher_build", "publisher_version"):
            require(isinstance(meta.get(key), str) and bool(meta[key].strip()),
                    f"ordinary task {key} is invalid for {task}")
        if "environment" in meta:
            environment = meta["environment"]
            require(isinstance(environment, list) and
                    all(isinstance(item, str) and "=" in item and item.split("=", 1)[0]
                        for item in environment),
                    f"ordinary task environment is invalid for {task}")
        if "environment_recorded" in meta:
            require(type(meta["environment_recorded"]) is bool,
                    f"ordinary task environment marker is invalid for {task}")
        for key in ("input_files", "output_artifacts"):
            if key in meta:
                require(isinstance(meta[key], list),
                        f"ordinary task {key} is invalid for {task}")
        if "output_writer_contract" in meta:
            require(isinstance(meta["output_writer_contract"], str),
                    f"ordinary task output writer contract is invalid for {task}")
        predicate = meta.get("predicate")
        require(isinstance(predicate, dict), f"ordinary task predicate is invalid for {task}")
        _exact_keys(predicate, _PREDICATE_KEYS, label=f"ordinary task predicate for {task}")
        require(predicate.get("adapter") == task_record["provider"] and
                predicate.get("mode") == task_record["mode"] and
                isinstance(predicate.get("version"), str) and bool(predicate["version"].strip()) and
                _hex_digest(predicate.get("sha256")),
                f"ordinary task predicate binding is invalid for {task}")
        _canonical_timestamp(meta.get("created_at"), f"ordinary task created_at for {task}")

        binding = meta.get("supervisor_config")
        _validate_supervisor_binding(binding, f"ordinary task supervisor for {task}")
        require(journal_binding is None or binding == journal_binding,
                f"ordinary task metadata supervisor changed for {task}")

        expected_label = f"delegate:{root}:{task}"
        submit = _read_control_record(directory / "submit.json", f"ordinary submit record for {task}")
        _exact_keys(submit, {"schema_version", "root_id", "task_id", "spec_sha256",
                             "meta_sha256", "label", "supervisor", "created_at"},
                    label=f"ordinary submit record for {task}")
        meta_digest = digest(meta_path)
        require(type(submit.get("schema_version")) is int and submit["schema_version"] == 1 and
                submit.get("root_id") == root and submit.get("task_id") == task and
                submit.get("spec_sha256") == spec_digest and
                submit.get("meta_sha256") == meta_digest and
                submit.get("label") == expected_label and submit.get("supervisor") == binding,
                f"ordinary submit record is not bound to task {task}")
        _canonical_timestamp(submit.get("created_at"), f"ordinary submit created_at for {task}")

        supervisor = _read_control_record(
            directory / "supervisor.ref.json", f"ordinary supervisor receipt for {task}")
        _exact_keys(supervisor, {
            "schema_version", "root_id", "task_id", "spec_sha256", "meta_sha256",
            "numeric_task_id", "label", *_SUPERVISOR_BINDING_KEYS,
        }, _SUPERVISOR_BINDING_OPTIONAL_KEYS,
                    label=f"ordinary supervisor receipt for {task}")
        numeric = supervisor.get("numeric_task_id")
        require(type(supervisor.get("schema_version")) is int and
                supervisor["schema_version"] == 1 and
                supervisor.get("root_id") == root and supervisor.get("task_id") == task and
                supervisor.get("spec_sha256") == spec_digest and
                supervisor.get("meta_sha256") == meta_digest and
                supervisor.get("label") == expected_label and
                type(numeric) is int and numeric >= 0,
                f"ordinary supervisor receipt identity is invalid for {task}")
        receipt_binding = {
            key: supervisor[key]
            for key in _SUPERVISOR_BINDING_KEYS | _SUPERVISOR_BINDING_OPTIONAL_KEYS
            if key in supervisor
        }
        _validate_supervisor_binding(
            receipt_binding, f"ordinary supervisor receipt binding for {task}")
        require(receipt_binding == binding,
                f"ordinary supervisor receipt binding changed for {task}")

        lifetime_started = _ordinary_lifetime_control_started(directory)
        managed_lifetime = None
        if meta.get("runner_ownership") == "managed" or lifetime_started:
            managed_lifetime = _validate_managed_ordinary_lifetime(
                directory, root, task, spec_digest, meta_digest)
        require(not self.managed_runner or meta.get("runner_ownership") == "managed",
                f"ordinary task managed runner marker is missing for {task}")
        return {"task_id": task, "label": expected_label, "numeric_task_id": numeric,
                "expected_result": "Success", "managed_lifetime": managed_lifetime}

    def _validate_queue_membership(self, status: dict[str, object], allow_failure: bool) -> None:
        tasks = status.get("tasks")
        require(isinstance(tasks, dict), "private queue tasks object is absent")
        require(self.dispatch_attempts, "no supervisor dispatch attempt was recorded")
        journals = self._inspection_journals(
            allow_failure, self.expected_inspection_binding_required)
        observed: dict[str, dict[str, object]] = {}
        for name, task in self.tasks.items():
            require(name in self.labels and name in self.numbers,
                    f"ordinary task response is incomplete for {task}")
            observed[task] = {
                "task_id": task,
                "label": self.labels[name],
                "numeric_task_id": self.numbers[name],
                "expected_result": self.expected_results.get(name, "Success"),
            }
        if allow_failure:
            ordinary: dict[str, dict[str, object]] = {}
            for task in self.dispatch_attempts:
                durable = self._durable_ordinary_admission(task, journals.get(task))
                if durable is None:
                    continue
                if task in observed:
                    require(all(observed[task][key] == durable[key]
                                for key in ("task_id", "label", "numeric_task_id")),
                            f"ordinary task response disagrees with durable admission for {task}")
                ordinary[durable["task_id"]] = durable
        else:
            ordinary = observed
        ordinary_task_ids = set(ordinary)
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
        ordinary_labels = {value["label"] for value in ordinary.values()}
        queue_journals = [journal for journal in journals.values()
                          if journal["inspection_exit_authority"]["mode"] in
                          {"queue", "guardian-unstarted"}]
        guardian_journals = [journal for journal in journals.values()
                             if journal["inspection_exit_authority"]["mode"] == "guardian"]
        required_labels = ordinary_labels | {journal["label"] for journal in queue_journals}
        optional_guardian_labels = {journal["label"] for journal in guardian_journals}
        allowed_labels = required_labels | optional_guardian_labels
        rows = [row for row in tasks.values() if isinstance(row, dict)]
        require(len(rows) == len(tasks), "private queue contains a malformed row")
        labels = [row.get("label") for row in rows]
        require(all(isinstance(label, str) for label in labels) and len(set(labels)) == len(labels) and
                set(labels) <= allowed_labels and required_labels <= set(labels),
                "private queue labels are not exact")
        for key, row in tasks.items():
            require(isinstance(key, str) and key.isdecimal() and str(int(key)) == key and
                    type(row.get("id")) is int and row["id"] == int(key),
                    "private queue task numeric identity is not exact")
        for task, admission in ordinary.items():
            row = next((candidate for candidate in rows
                        if candidate.get("label") == admission["label"]), None)
            require(isinstance(row, dict), f"private queue lost ordinary task {task}")
            self.validate_ordinary_queue_row(
                row, admission["label"], task,
                admission["numeric_task_id"], "default")
            expected_result = admission["expected_result"]
            require(row_state(row) == "Done" and
                    (not allow_failure or done_result(row) is not None) and
                    (allow_failure or done_result(row) == expected_result),
                    f"ordinary task is not in an acceptable terminal state: {task}")
        for journal in queue_journals:
            row = next((candidate for candidate in rows if candidate.get("label") == journal["label"]), None)
            require(isinstance(row, dict), f"private queue lost inspection task {journal['task_id']}")
            self._validate_inspection_queue_row(row, journal, allow_failure)
        for journal in guardian_journals:
            row = next((candidate for candidate in rows if candidate.get("label") == journal["label"]), None)
            if row is not None:
                self._validate_inspection_queue_row(
                    row, journal, allow_failure, terminal_required=False)

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
        # Shared native-provider acceptance uses an explicit external
        # supervisor config; it must not impose private-supervisor policy.
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
        deadline = failure_cleanup_deadline(self.watch_seconds)

        def wait(delay: float, reason: str) -> bool:
            if wait_for_failure_cleanup(deadline, delay):
                return True
            write_json(self.output / "failure-cleanup-expired.json", {
                "reason": reason,
                "timeout_seconds": self.watch_seconds,
                "supervisor_closed": self.closed,
                "signals_sent": 0,
            }, replace=(self.output / "failure-cleanup-expired.json").exists())
            return False

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
                if not wait(observation_retry_delay,
                            "owned process completion could not be observed"):
                    return False
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
                    if self.dispatch_attempts:
                        return False
                    self.closed = True
                    return True
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
            if not wait(delay, "owned processes or private queue did not finish naturally"):
                return False


def wait_runner_terminal(
    client: Callable[..., object],
    root_id: str,
    task_id: str,
    expected_result: str | frozenset[str],
    timeout: float = 150,
    sleep_fn: Callable[[float], None] = time.sleep,
    monotonic_fn: Callable[[], float] = time.monotonic,
) -> dict[str, object]:
    """Observe one exact delegate runner row until it reaches one expected result."""
    label = "delegate:" + root_id + ":" + task_id
    deadline = monotonic_fn() + timeout
    while monotonic_fn() < deadline:
        remaining = deadline - monotonic_fn()
        result = client("runner-status", ["status", "--json"], timeout=remaining)
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
            actual_result = done_result(row)
            if isinstance(expected_result, str):
                accepted = actual_result == expected_result
                expectation = repr(expected_result)
            else:
                require(isinstance(expected_result, frozenset) and expected_result and
                        all(isinstance(item, str) for item in expected_result),
                        "expected supervisor results are invalid")
                accepted = actual_result in expected_result
                expectation = "one of " + repr(sorted(expected_result))
            require(accepted,
                    f"supervisor result {actual_result!r}, expected {expectation}")
            return row
        sleep_fn(0.1)
    raise RuntimeError("runner completion was not observed within acceptance deadline")


def wait_runner_done(
    client: Callable[[str, list[str]], object],
    root_id: str,
    task_id: str,
    timeout: float = 150,
    sleep_fn: Callable[[float], None] = time.sleep,
    monotonic_fn: Callable[[], float] = time.monotonic,
) -> dict[str, object]:
    """Observe one exact delegate runner row until it finishes successfully."""
    return wait_runner_terminal(client, root_id, task_id, "Success", timeout, sleep_fn, monotonic_fn)


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
