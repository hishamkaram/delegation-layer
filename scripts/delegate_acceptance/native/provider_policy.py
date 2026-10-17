"""Provider prerequisites and model discovery response rules."""

from __future__ import annotations

from datetime import datetime

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require
from scripts.delegate_acceptance.native.constants import (
    MODEL_DISCOVERY_EXIT_CODES, MODEL_DISCOVERY_OPTIONAL_KEYS,
    MODEL_DISCOVERY_REQUIRED_KEYS,
    MODEL_INFO_OPTIONAL_KEYS, MODEL_INFO_REQUIRED_KEYS,
)


def authentication_blocked_response(response: object) -> bool:
    """Recognize the delegate's structured native-authentication block."""
    if not isinstance(response, dict) or response.get("status") != "blocked":
        return False
    capability = response.get("capability")
    live = capability.get("live_acceptance") if isinstance(capability, dict) else None
    return (isinstance(live, dict) and live.get("status") == "blocked" and
            live.get("authentication") == "blocked" and
            live.get("reason_code") == "authentication_unavailable")


def authentication_blocked(response: object, _diagnostic: str) -> bool:
    """Trust only Delegate's structured pre-admission authentication state."""
    return authentication_blocked_response(response)


def model_discovery_exit_code(status: str) -> int:
    try:
        return MODEL_DISCOVERY_EXIT_CODES[status]
    except KeyError as error:
        raise AcceptanceFailure(f"unknown model discovery status: {status!r}") from error


def validate_model_discovery_response(response: object, provider: str) -> dict[str, object]:
    """Validate the public model observation without turning it into a policy."""
    require(isinstance(response, dict), "model discovery response is not an object")
    keys = set(response)
    require(MODEL_DISCOVERY_REQUIRED_KEYS <= keys <=
            MODEL_DISCOVERY_REQUIRED_KEYS | MODEL_DISCOVERY_OPTIONAL_KEYS,
            "model discovery response has unknown or missing fields")
    require(type(response.get("schema_version")) is int and response["schema_version"] == 1,
            "model discovery schema version is invalid")
    require(response.get("command") == "models", "model discovery command is invalid")
    require(response.get("provider") == provider, "model discovery provider is invalid")
    observed_at = response.get("observed_at")
    require(isinstance(observed_at, str) and observed_at, "model discovery timestamp is invalid")
    try:
        parsed = datetime.fromisoformat(observed_at.replace("Z", "+00:00"))
    except ValueError as error:
        raise AcceptanceFailure("model discovery timestamp is invalid") from error
    require(parsed.tzinfo is not None, "model discovery timestamp has no timezone")
    status = response.get("status")
    require(isinstance(status, str) and status in MODEL_DISCOVERY_EXIT_CODES,
            "model discovery status is invalid")
    reason_code = response.get("reason_code")
    require(isinstance(reason_code, str) and reason_code and
            not any(character.isspace() for character in reason_code),
            "model discovery reason code is invalid")
    require(type(response.get("complete")) is bool, "model discovery complete flag is invalid")
    source = response.get("source")
    require(isinstance(source, str), "model discovery source is invalid")
    efforts = response.get("efforts")
    _validate_model_choices(efforts, "model discovery efforts")
    models = response.get("models")
    require(isinstance(models, list), "model discovery models is not an array")
    if status == "available":
        require(response["complete"] is True and models,
                "available model discovery must enumerate a complete nonempty list")
    elif status == "partial":
        require(response["complete"] is False and models,
                "partial model discovery must enumerate an incomplete nonempty list")
    elif status in {"blocked", "unavailable"}:
        require(response["complete"] is False and not models,
                f"{status} model discovery must not enumerate models")
    else:
        require(response["complete"] is False and not models,
                "failed model discovery must not enumerate models")
    seen: set[str] = set()
    for index, model in enumerate(models):
        require(isinstance(model, dict), f"model discovery model {index} is not an object")
        model_keys = set(model)
        require(MODEL_INFO_REQUIRED_KEYS <= model_keys <=
                MODEL_INFO_REQUIRED_KEYS | MODEL_INFO_OPTIONAL_KEYS,
                f"model discovery model {index} has unknown or missing fields")
        model_id = model.get("id")
        require(isinstance(model_id, str) and model_id and
                not any(ord(character) < 32 for character in model_id),
                f"model discovery model {index} id is invalid")
        require(model_id not in seen, f"model discovery has duplicate model id {model_id!r}")
        seen.add(model_id)
        for key in ("name", "default_effort"):
            if key in model:
                require(model[key] is None or
                        (isinstance(model[key], str) and bool(model[key]) and
                         not any(ord(character) < 32 for character in model[key])),
                        f"model discovery model {index} {key} is invalid")
        _validate_model_choices(model.get("efforts"),
                                f"model discovery model {index} efforts")
        default_effort = model.get("default_effort")
        model_efforts = model.get("efforts")
        if default_effort is not None and model_efforts is not None:
            require(default_effort in model_efforts,
                    f"model discovery model {index} default effort is not listed")
    return {
        "status": status,
        "reason_code": reason_code,
        "complete": response["complete"],
        "source": source,
        "model_count": len(models),
        "effort_count": None if efforts is None else len(efforts),
    }


def _validate_model_choices(value: object, label: str) -> None:
    require(value is None or isinstance(value, list), f"{label} is not null or an array")
    if value is None:
        return
    seen: set[str] = set()
    for choice in value:
        require(isinstance(choice, str) and choice and
                not any(ord(character) < 32 for character in choice),
                f"{label} contains an invalid choice")
        require(choice not in seen, f"{label} contains a duplicate choice")
        seen.add(choice)
