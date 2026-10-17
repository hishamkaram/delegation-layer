"""Pure TaskRecord validation and exact Go-compatible canonical encoding."""

from __future__ import annotations

import json
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.evidence.limits import MAX_CONTROL_BYTES
from scripts.delegate_acceptance.evidence.validation import (
    AcceptanceFailure,
    _exact_keys,
    _hex_digest,
    _task_identity,
    require,
)
from scripts.delegate_acceptance.paths.policy import historical_clean_absolute_path


_TASK_REQUIRED_KEYS = {
    "schema_version", "root_id", "task_id", "provider", "mode", "canonical_cwd",
    "requested_config", "budget_nanos", "brief_sha256", "brief_length",
}


_TASK_OPTIONAL_KEYS = {"prior_session", "workspace"}


_WORKSPACE_BINDING_KEYS = {"id", "reservation", "request_sha256", "budget_nanos"}


_TASK_CONFIG_KEYS = {"interactive", "model", "effort", "permission", "budget", "native_timeout",
                     "review_target"}


_PRIOR_SESSION_KEYS = {"provider", "conversation_id", "predecessor_task_id"}


_MAX_INT64 = (1 << 63) - 1


_MAX_UINT64 = (1 << 64) - 1


def _canonical_workspace_binding(value: object, label: str) -> dict[str, object]:
    """Validate and order the optional TaskRecord workspace binding."""
    require(isinstance(value, dict), f"{label} is not an object")
    _exact_keys(value, _WORKSPACE_BINDING_KEYS, label=label)
    binding_id = value.get("id")
    reservation = value.get("reservation")
    request_sha256 = value.get("request_sha256")
    budget_nanos = value.get("budget_nanos")
    require(_task_identity(binding_id), f"{label} id is invalid")
    require(type(reservation) is int and 0 < reservation <= _MAX_UINT64,
            f"{label} reservation is invalid")
    require(_hex_digest(request_sha256), f"{label} request_sha256 is invalid")
    require(type(budget_nanos) is int and 0 < budget_nanos <= _MAX_INT64,
            f"{label} budget_nanos is invalid")
    return {
        "id": binding_id,
        "reservation": reservation,
        "request_sha256": request_sha256,
        "budget_nanos": budget_nanos,
    }


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
    workspace = value.get("workspace")
    if workspace is not None:
        canonical["workspace"] = _canonical_workspace_binding(
            workspace, "inspection task workspace")
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
    require(historical_clean_absolute_path(cwd),
            f"{label} canonical_cwd is invalid")
    budget = value.get("budget_nanos")
    require(type(budget) is int and 0 < budget <= _MAX_INT64,
            f"{label} budget_nanos is invalid")
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
    if "workspace" in value:
        _canonical_workspace_binding(value["workspace"], f"{label} workspace")
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
