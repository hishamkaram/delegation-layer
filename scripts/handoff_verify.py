"""Read-only check that a handoff evidence index still matches its recorded inputs.

Verification regenerates the document from the inputs the index itself declares,
through the same canonical policy that wrote it, and compares the two. It never
writes, and "complete" stays a statement about declared input completeness only.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Callable

from scripts.delegate_acceptance.evidence.bounded_files import (
    BoundedReadError, ReadFailure, read_bounded_regular_file,
)
from scripts.handoff_index import (
    MAX_INDEX_BYTES, SCHEMA_VERSION, TOOL_VERSION, EvidenceError, EvidenceRequest, build_document,
    canonical_input_path,
)

MAX_REPORTED_DIFFERENCES = 64
# The checkout location and branch name do not change the evidence; revisions and content do.
_INFORMATIONAL_REPOSITORY_FIELDS = frozenset({"root", "branch", "detached"})
_ITEM_KEYS: dict[str, Callable[[dict], str]] = {
    "source_manifest": lambda item: str(item.get("path")),
    "binaries": lambda item: str(item.get("role")),
    "receipts": lambda item: f"{item.get('claim_class')}:{item.get('path')}",
}


def _invalid() -> EvidenceError:
    return EvidenceError("index_invalid", "verify-index")


def _unique_pairs(pairs: list[tuple[str, object]]) -> dict:
    keys = [key for key, _ in pairs]
    if len(set(keys)) != len(keys):
        raise ValueError("duplicate key")
    return dict(pairs)


_INDEX_READ_CODES = {
    ReadFailure.INVALID: "index_not_regular",
    ReadFailure.TOO_LARGE: "index_too_large",
    ReadFailure.CHANGED: "index_unavailable",
    ReadFailure.UNREADABLE: "index_unavailable",
}


def _read_index(path: Path) -> dict:
    try:
        canonical = canonical_input_path(path)
        data = read_bounded_regular_file(canonical, MAX_INDEX_BYTES)
    except OSError:
        raise EvidenceError("index_unavailable", "read-index") from None
    except BoundedReadError as error:
        raise EvidenceError(_INDEX_READ_CODES[error.reason], "read-index") from None
    try:
        document = json.loads(data.decode("utf-8"), object_pairs_hook=_unique_pairs)
    except (ValueError, RecursionError):
        raise _invalid() from None
    if not isinstance(document, dict):
        raise _invalid()
    return document


def _mapping(value: object) -> dict:
    if not isinstance(value, dict):
        raise _invalid()
    return value


def _items(document: dict, key: str) -> list[dict]:
    value = document.get(key)
    if not isinstance(value, list) or not all(isinstance(item, dict) for item in value):
        raise _invalid()
    return value


def _strings(value: object) -> tuple[str, ...]:
    if not isinstance(value, list) or not all(isinstance(item, str) for item in value):
        raise _invalid()
    return tuple(value)


def _text(item: dict, key: str) -> str:
    value = item.get(key)
    if not isinstance(value, str):
        raise _invalid()
    return value


def _absolute_path(item: dict) -> str:
    path = _text(item, "path")
    if not Path(path).is_absolute():
        raise _invalid()
    return path


def _request_from_index(document: dict, base: Path) -> EvidenceRequest:
    if type(document.get("schema_version")) is not int or document["schema_version"] != SCHEMA_VERSION:
        raise EvidenceError("index_version_unsupported", "verify-index")
    if document.get("tool_version") != TOOL_VERSION:
        raise EvidenceError("index_version_unsupported", "verify-index")
    baseline = _mapping(document.get("repository")).get("baseline")
    if baseline is not None and not isinstance(baseline, str):
        raise _invalid()
    requirements = _mapping(document.get("requirements"))
    return EvidenceRequest(
        base=base, output=None, baseline=baseline,
        binaries=tuple((_text(item, "role"), _absolute_path(item), _text(item, "identity")) for item in _items(document, "binaries")),
        receipts=tuple((_text(item, "claim_class"), _absolute_path(item)) for item in _items(document, "receipts")),
        transition_rows=_strings(document.get("transition_rows")),
        unresolved=_strings(document.get("unresolved")),
        require_source=_strings(requirements.get("source_paths")),
        require_binary=_strings(requirements.get("binary_roles")),
        require_receipt=_strings(requirements.get("receipt_classes")),
    )


def _comparable(document: dict) -> dict:
    repository = _mapping(document.get("repository"))
    flat = {key: value for key, value in document.items() if key != "repository"}
    for key, value in repository.items():
        if key not in _INFORMATIONAL_REPOSITORY_FIELDS:
            flat[f"repository.{key}"] = value
    return flat


def _item_differences(field: str, recorded: list, current: list) -> list[dict]:
    key = _ITEM_KEYS[field]
    old = {key(item): item for item in recorded if isinstance(item, dict)}
    new = {key(item): item for item in current}
    found = (
        [{"field": field, "key": name, "kind": "removed"} for name in sorted(old.keys() - new.keys())]
        + [{"field": field, "key": name, "kind": "added"} for name in sorted(new.keys() - old.keys())]
        + [{"field": field, "key": name, "kind": "changed"} for name in sorted(old.keys() & new.keys()) if old[name] != new[name]]
    )
    return found or [{"field": field, "kind": "changed"}]


def _differences(recorded: dict, current: dict) -> list[dict]:
    found: list[dict] = []
    for field in sorted(recorded.keys() | current.keys()):
        if recorded.get(field) == current.get(field):
            continue
        if field in _ITEM_KEYS and isinstance(recorded.get(field), list) and isinstance(current.get(field), list):
            found.extend(_item_differences(field, recorded[field], current[field]))
        else:
            found.append({"field": field, "kind": "changed"})
    return found


def verify_index(root: Path, index_path: Path) -> tuple[dict, int]:
    """Return a bounded report and exit code: 0 verified complete, 2 verified incomplete, 1 otherwise."""
    recorded = _read_index(index_path)
    declared = recorded.get("verdict")
    if declared not in ("complete", "incomplete"):
        raise EvidenceError("index_declares_failure", "verify-index")
    current, _ = build_document(root, _request_from_index(recorded, root))
    differences = _differences(_comparable(recorded), _comparable(current))
    report = {
        "tool_version": TOOL_VERSION, "declared_verdict": declared, "verified": not differences,
        "difference_count": len(differences), "differences": differences[:MAX_REPORTED_DIFFERENCES],
    }
    if differences:
        return report, 1
    return report, 0 if declared == "complete" else 2
