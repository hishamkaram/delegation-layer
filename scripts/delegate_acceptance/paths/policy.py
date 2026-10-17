"""Pure lexical Unix path predicates for persisted evidence."""

from __future__ import annotations

import posixpath
from pathlib import Path

from scripts.delegate_acceptance.evidence.validation import require


def historical_clean_absolute_path(value: object) -> bool:
    """Preserve Path equality used by historical records, not raw spelling.

    This deliberately accepts repeated separators, "." segments, and NULs.
    Filesystem validity and the stricter Go spelling identity are separate rules.
    """
    return (isinstance(value, str) and Path(value).is_absolute() and
            Path(value) == Path(posixpath.normpath(value)))


def clean_absolute(value: object, label: str) -> Path:
    require(isinstance(value, str) and bool(value), f"{label} must be a nonempty path")
    path = Path(value)
    require(historical_clean_absolute_path(value),
            f"{label} must be absolute and clean")
    return path


def _raw_clean_absolute_path(value: object) -> bool:
    """Match the Unix filepath.Clean identity used by the Go records."""
    return (isinstance(value, str) and Path(value).is_absolute() and
            not value.startswith("//") and value == posixpath.normpath(value))
