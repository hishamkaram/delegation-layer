"""Pure installed-role and historical provider identity receipt validation."""

from pathlib import Path
import posixpath
import re

from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.tasks.runtime_metadata import _provider_runtime_identity


def _safe_runtime_executable(value: object) -> str | None:
    if (not isinstance(value, str) or not value or len(value) > 4096
            or not posixpath.isabs(value) or any(ord(character) < 32 for character in value)):
        return None
    return value


def _compact_installed_paths(value: object, *, complete: bool) -> dict[str, str] | None:
    """Validate the explicit four-role installed selection without reading its files."""
    roles = {"delegate", "delegate-run", "pueue", "pueued"}
    if not isinstance(value, dict) or (set(value) != roles if complete else not set(value) <= roles):
        return None
    if any(not isinstance(path, str) or not Path(path).is_absolute()
           or ".." in Path(path).parts or str(Path(path)) != path
           or any(ord(char) < 32 for char in path) for path in value.values()):
        return None
    return dict(value)


def _compact_runtime_identity(value: object) -> dict[str, str] | None:
    """Project receipt keys through the canonical task diagnostic validator."""
    if (not isinstance(value, dict) or "executable" not in value
            or not set(value) <= {"executable", "version", "sha256"}
            or _safe_runtime_executable(value.get("executable")) is None):
        return None
    try:
        return _provider_runtime_identity({"provider_" + key: item for key, item in value.items()}, "recorded", "native receipt")
    except AcceptanceFailure:
        return None


def _compact_provider_runtime_identities(
        raw: dict[str, object]) -> dict[str, dict[str, str]] | None:
    value = raw.get("provider_runtime_identities")
    if not isinstance(value, dict) or not value or len(value) > 64:
        return None
    result: dict[str, dict[str, str]] = {}
    for task_id, identity in value.items():
        normalized = _compact_runtime_identity(identity)
        if (not isinstance(task_id, str) or re.fullmatch(r"[0-9a-f]{32}", task_id) is None
                or normalized is None):
            return None
        result[task_id] = normalized
    return dict(sorted(result.items()))


def _runtime_identity_matches(left: dict[str, str], right: dict[str, str]) -> bool:
    return left.get("executable") == right.get("executable") and all(
        left[key] == right[key] for key in ("version",) if left.get(key) and right.get(key))
