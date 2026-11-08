"""Shared acceptance error identity and pure evidence shape checks."""

from __future__ import annotations

from collections.abc import Set
from typing import TypeGuard


class AcceptanceFailure(RuntimeError):
    """A provider acceptance oracle could not be established."""


def _exact_keys(value: object, required: Set[str], optional: Set[str] = frozenset(),
                label: str = "record") -> None:
    if not isinstance(value, dict):
        raise AcceptanceFailure(f"{label} is not an object")
    keys = set(value)
    require(required <= keys and keys <= required | optional,
            f"{label} has unknown or missing fields")


def _required_keys(value: object, required: Set[str],
                   label: str = "record") -> None:
    """Require protocol fields while allowing additive producer fields."""
    if not isinstance(value, dict):
        raise AcceptanceFailure(f"{label} is not an object")
    require(required <= set(value), f"{label} has missing fields")


def require(condition: object, message: str) -> None:
    if not condition:
        raise AcceptanceFailure(message)


def _hex_digest(value: object) -> TypeGuard[str]:
    return (isinstance(value, str) and len(value) == 64 and
            all(character in "0123456789abcdef" for character in value))


def _optional_hex_digest(value: object) -> TypeGuard[str]:
    """Permit an unobserved diagnostic, while validating populated digests."""
    return isinstance(value, str) and (value == "" or _hex_digest(value))


def _task_identity(value: object) -> TypeGuard[str]:
    return (isinstance(value, str) and len(value) == 32 and
            all(character in "0123456789abcdef" for character in value))
