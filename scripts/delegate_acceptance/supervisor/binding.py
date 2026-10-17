"""Pure supervisor binding, version, and historical identity rules."""

from __future__ import annotations

import unicodedata
from scripts.delegate_acceptance.evidence.limits import MAX_CONTROL_BYTES
from scripts.delegate_acceptance.evidence.validation import (
    AcceptanceFailure,
    _exact_keys,
    _hex_digest,
    require,
)
from scripts.delegate_acceptance.paths.policy import historical_clean_absolute_path


_SUPERVISOR_JSON_ORDER = (
    "client_executable", "client_sha256", "daemon_executable", "daemon_sha256",
    "resolution_os", "resolution_home", "resolution_data_local", "resolution_config",
    "resolution_runtime", "resolution_username", "resolution_cwd",
    "resolved_config_sha256", "endpoint", "config_path", "config_digest",
    "observed_version",
)


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


def _validate_supervisor_binding(value: object, label: str) -> None:
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, _SUPERVISOR_BINDING_KEYS, _SUPERVISOR_BINDING_OPTIONAL_KEYS, label)
    for key in ("client_executable", "config_path"):
        path = value.get(key)
        require(historical_clean_absolute_path(path),
                f"{label} {key} is invalid")
    for key in ("client_sha256", "resolved_config_sha256", "config_digest"):
        require(_hex_digest(value.get(key)), f"{label} {key} is invalid")
    daemon_executable = value.get("daemon_executable")
    daemon_sha256 = value.get("daemon_sha256")
    require((daemon_executable is None) == (daemon_sha256 is None),
            f"{label} daemon provenance is incomplete")
    if daemon_executable is not None:
        require(historical_clean_absolute_path(daemon_executable),
                f"{label} daemon_executable is invalid")
        require(_hex_digest(daemon_sha256), f"{label} daemon_sha256 is invalid")
    require(isinstance(value.get("endpoint"), str) and value["endpoint"],
            f"{label} endpoint is invalid")
    observed_pueue_version(value.get("observed_version"), "pueue")
    resolution_keys = _SUPERVISOR_RESOLUTION_KEYS & set(value)
    if resolution_keys == {"resolution_cwd"}:
        path = value["resolution_cwd"]
        require(historical_clean_absolute_path(path),
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
                require(historical_clean_absolute_path(path),
                        f"{label} {key} is invalid")
        username = value["resolution_username"]
        require(isinstance(username, str) and username and
                "/" not in username and "\\" not in username and "\x00" not in username,
                f"{label} resolution username is invalid")


def _canonical_supervisor_ref(value: object, label: str) -> dict[str, object]:
    """Validate and order a task.SupervisorRef for Go's JSON encoder."""
    _validate_supervisor_binding(value, label)
    require(isinstance(value, dict) and all(value[key] is not None for key in value),
            f"{label} contains null fields")
    return {key: value[key] for key in _SUPERVISOR_JSON_ORDER if key in value}
