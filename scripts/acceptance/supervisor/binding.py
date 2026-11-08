"""Pure supervisor binding, version, and historical identity rules."""

from __future__ import annotations

import unicodedata
from scripts.acceptance.evidence.limits import MAX_CONTROL_BYTES
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.evidence.validation import _exact_keys
from scripts.acceptance.evidence.validation import _hex_digest
from scripts.acceptance.evidence.validation import require
from scripts.acceptance.paths.policy import historical_clean_absolute_path


_SUPERVISOR_JSON_ORDER = (
    "client_executable", "client_sha256", "daemon_executable", "daemon_sha256",
    "executable_identity",
    "resolution_os", "resolution_home", "resolution_data_local", "resolution_config",
    "resolution_runtime", "resolution_username", "resolution_cwd",
    "resolved_config_sha256", "endpoint", "config_path", "config_digest",
    "observed_version",
)


_SUPERVISOR_BINDING_KEYS = {
    "client_executable", "resolved_config_sha256", "endpoint",
    "config_path", "config_digest", "observed_version",
}


_SUPERVISOR_RESOLUTION_KEYS = {
    "resolution_os", "resolution_home", "resolution_data_local",
    "resolution_config", "resolution_runtime", "resolution_username",
    # Accepted for records written before recovery stopped using the caller cwd.
    "resolution_cwd",
}


_SUPERVISOR_BINDING_OPTIONAL_KEYS = {
    "client_sha256", "daemon_executable", "daemon_sha256", "executable_identity",
    *_SUPERVISOR_RESOLUTION_KEYS,
}


def observed_pueue_version(value: object, command: str) -> str:
    """Validate one bounded pueue executable version observation."""
    if isinstance(value, bytes):
        require(len(value) <= MAX_CONTROL_BYTES, f"{command} version output exceeds the control bound")
        try:
            value = value.decode("utf-8")
        except UnicodeDecodeError as error:
            raise AcceptanceFailure(f"{command} version output is not valid UTF-8") from error
    if not isinstance(value, str):
        raise AcceptanceFailure(f"{command} version output is malformed")
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
    if not isinstance(value, dict):
        raise AcceptanceFailure(f"{label} is not an object")
    _exact_keys(value, _SUPERVISOR_BINDING_KEYS, _SUPERVISOR_BINDING_OPTIONAL_KEYS, label)
    identity = value.get("executable_identity")
    require(identity is None or identity == "installed-path-v1",
            f"{label} executable identity is unsupported")
    if identity == "installed-path-v1":
        require("client_sha256" not in value and "daemon_sha256" not in value,
                f"{label} installed identity contains executable SHA")
    else:
        require(_hex_digest(value.get("client_sha256")),
                f"{label} historical client_sha256 is invalid")
    for key in ("client_executable", "config_path"):
        path = value.get(key)
        require(historical_clean_absolute_path(path),
                f"{label} {key} is invalid")
    for key in ("resolved_config_sha256", "config_digest"):
        require(_hex_digest(value.get(key)), f"{label} {key} is invalid")
    if "client_sha256" in value:
        require(_hex_digest(value["client_sha256"]), f"{label} historical client_sha256 is invalid")
    daemon_executable = value.get("daemon_executable")
    daemon_sha256 = value.get("daemon_sha256")
    require(daemon_sha256 is None or daemon_executable is not None,
            f"{label} historical daemon provenance is incomplete")
    if daemon_executable is not None:
        require(historical_clean_absolute_path(daemon_executable),
                f"{label} daemon_executable is invalid")
    if "daemon_sha256" in value:
        require(_hex_digest(daemon_sha256), f"{label} historical daemon_sha256 is invalid")
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
        require(isinstance(value["resolution_os"], str) and value["resolution_os"] in {"linux", "darwin"},
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
    if not isinstance(value, dict) or not all(value[key] is not None for key in value):
        raise AcceptanceFailure(f"{label} contains null fields")
    return {key: value[key] for key in _SUPERVISOR_JSON_ORDER if key in value}
