"""Retention records for native acceptance evidence."""

from __future__ import annotations

from pathlib import Path
import os
import re
from .contract import (
    DEFAULT_RETENTION_COUNT,
    EXPECTED_CASES,
    EvidenceError,
    MAX_RECEIPT_BYTES,
    MAX_RETENTION_COUNT,
    SCHEMA_VERSION,
    _digest_bytes,
    canonical_json,
)
from .filesystem import (
    _digest_file,
    _identity_matches,
    _tree_summary,
)


def _retention_count(value: int | None) -> int:
    if value is None:
        raw = os.environ.get("DELEGATE_ACCEPTANCE_RESOLVED_RETENTION", str(DEFAULT_RETENTION_COUNT))
        if not re.fullmatch(r"[0-9]+", raw):
            raise EvidenceError("retention_count_invalid")
        value = int(raw)
    if value < 1 or value > MAX_RETENTION_COUNT:
        raise EvidenceError("retention_count_invalid")
    return value


def _aggregate_root_summary(entries: list[tuple[Path, dict[str, object], int]]) -> dict[str, object]:
    summaries = [_tree_summary(path) for path, _, _ in entries]
    complete = all(summary.get("complete") is True for summary in summaries)
    file_count = sum(int(summary.get("file_count", 0)) for summary in summaries)
    byte_count = sum(int(summary.get("byte_count", 0)) for summary in summaries)
    result: dict[str, object] = {
        "root_count": len(entries), "file_count": file_count, "byte_count": byte_count,
        "complete": complete,
        "count": len(entries), "bytes": byte_count,
    }
    return result


def _current_retention_entry(
        root: Path,
        receipt: dict[str, object]) -> tuple[Path, dict[str, object], int] | None:
    cases = receipt.get("cases")
    completed = receipt.get("completed_at_ns")
    if (receipt.get("case_results_verdict") not in {"PASS", "BLOCKED"}
            or receipt.get("matrix_complete") is not True
            or not isinstance(cases, list) or len(cases) != len(EXPECTED_CASES)
            or any(not isinstance(case, dict) or case.get("evidence_status") != "RESOLVED"
                   or case.get("status") not in {"PASS", "BLOCKED"} for case in cases)
            or type(completed) is not int or completed < 0
            or not _identity_matches(root, receipt.get("aggregate_owner"))):
        return None
    return root, receipt, completed


def _retention_authorization(
        current_owner: dict[str, object],
        configured_count: int,
        path: Path,
        identity: dict[str, object],
        before: dict[str, object]) -> str:
    return _digest_bytes(canonical_json({
        "kind": "native-acceptance-retention-victim",
        "current_owner": current_owner,
        "configured_count": configured_count,
        "path": str(path.absolute()),
        "identity": identity,
        "before": before,
    }))


def _retention_plan_result(plan: dict[str, object], plan_path: Path) -> dict[str, object]:
    result = plan.get("result")
    if not isinstance(result, dict):
        raise EvidenceError("retention_plan_invalid")
    return {
        **result,
        "plan_path": str(plan_path.resolve()),
        "plan_sha256": _digest_file(plan_path, MAX_RECEIPT_BYTES),
    }


def _validate_retention_plan(
        plan: object,
        root: Path,
        current_receipt: dict[str, object],
        count: int) -> dict[str, object]:
    current_owner = current_receipt.get("aggregate_owner")
    current_entry = _current_retention_entry(root, current_receipt)
    expected_counted = current_entry is not None
    expected_order = current_entry[2] if current_entry is not None else None
    if (not isinstance(plan, dict) or plan.get("schema_version") != SCHEMA_VERSION
            or plan.get("kind") != "native-acceptance-retention-plan"
            or plan.get("state") not in {"prepared", "partial", "complete"}
            or plan.get("configured_count") != count
            or plan.get("current_root") != str(root.absolute())
            or plan.get("current_owner") != current_owner
            or plan.get("current_counted") is not expected_counted
            or plan.get("current_order") != expected_order
            or not isinstance(plan.get("retained"), list)
            or not isinstance(plan.get("victims"), list)):
        raise EvidenceError("retention_plan_invalid")
    return plan
