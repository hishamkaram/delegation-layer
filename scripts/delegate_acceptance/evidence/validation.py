"""Shared acceptance error identity and pure evidence shape checks."""

from __future__ import annotations



class AcceptanceFailure(RuntimeError):
    """A provider acceptance oracle could not be established."""


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


def require(condition: object, message: str) -> None:
    if not condition:
        raise AcceptanceFailure(message)


def _hex_digest(value: object) -> bool:
    return (isinstance(value, str) and len(value) == 64 and
            all(character in "0123456789abcdef" for character in value))


def _task_identity(value: object) -> bool:
    return (isinstance(value, str) and len(value) == 32 and
            all(character in "0123456789abcdef" for character in value))
