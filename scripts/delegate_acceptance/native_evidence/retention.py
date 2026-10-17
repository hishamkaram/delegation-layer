"""Retention for native acceptance evidence."""

from __future__ import annotations

from pathlib import Path
from ..directories.journal import acquire_exclusive_file_lock
from ..directories.journal import cleanup_journal_digest
from ..directories.journal import prepare_directory_cleanup
from ..directories.journal import read_directory_cleanup
from ..directories.journal import release_exclusive_file_lock
from ..directories.journal import resume_directory_cleanup
from .contract import (
    AGGREGATE_KIND,
    AGGREGATE_RECEIPT_NAME,
    CASE_ROOT_NAME,
    COMPACT_RECEIPT_ROOT_NAME,
    EXPECTED_CASES,
    EvidenceError,
    HEX_DIGEST,
    MAX_AGGREGATE_BYTES,
    MAX_RECEIPT_BYTES,
    RETENTION_CLEANUP_ROOT_NAME,
    RETENTION_PLAN_NAME,
    SCHEMA_VERSION,
    _aggregate_exit,
    _empty_summary,
    _summary_with_removed,
)
from .filesystem import (
    _bounded_children,
    _ensure_directory,
    _identity_matches,
    _read_json,
    _tree_summary,
    _write_atomic,
)
from .retention_records import (
    _aggregate_root_summary,
    _current_retention_entry,
    _retention_authorization,
    _retention_plan_result,
    _validate_retention_plan,
)
from .validation import (
    validate_aggregate_receipt,
)


def _resolved_receipt(path: Path) -> tuple[dict[str, object], int] | None:
    try:
        value, _ = _read_json(path, MAX_AGGREGATE_BYTES)
    except EvidenceError:
        return None
    if not isinstance(value, dict) or value.get("kind") != AGGREGATE_KIND:
        return None
    try:
        validate_aggregate_receipt(path)
    except EvidenceError:
        return None
    status = value.get("status")
    cases = value.get("cases")
    aggregate_root = path.parent
    owner = value.get("aggregate_owner")
    if (not _identity_matches(aggregate_root, owner)
            or not isinstance(owner, dict)
            or owner.get("path") != str(aggregate_root.absolute())):
        return None
    case_root = aggregate_root / CASE_ROOT_NAME
    try:
        case_root_empty = not case_root.is_symlink() and not _bounded_children(case_root)
    except EvidenceError:
        return None
    if (status not in {"PASS", "BLOCKED"} or value.get("unresolved_case_count") != 0
            or value.get("matrix_complete") is not True
            or value.get("expected_case_count") != len(EXPECTED_CASES)
            or value.get("case_count") != len(EXPECTED_CASES)
            or not isinstance(cases, list) or len(cases) != len(EXPECTED_CASES)
            or any(not isinstance(case, dict) or case.get("evidence_status") != "RESOLVED"
                   or case.get("status") not in {"PASS", "BLOCKED"} for case in cases)
            or case_root.is_symlink()
            or (case_root.exists() and not case_root_empty)
            or not isinstance(value.get("completed_at_ns"), int)
            or not isinstance(value.get("terminal_evidence_published"), bool)
            or value.get("terminal_evidence_published") is not True):
        return None
    completed = value.get("completed_at_ns")
    order = completed if type(completed) is int and completed >= 0 else -1
    if order < 0:
        return None
    return value, order


def _reconcile_pending_retention(parent: Path, current_root: Path) -> None:
    for candidate in sorted(_bounded_children(parent), key=lambda item: item.name):
        if (candidate == current_root or not candidate.is_dir() or candidate.is_symlink()
                or not candidate.name.startswith("native-aggregate-")):
            continue
        plan_path = candidate / RETENTION_PLAN_NAME
        if not plan_path.exists() or plan_path.is_symlink():
            continue
        candidate_lock = acquire_exclusive_file_lock(
            candidate.parent / ("." + candidate.name + ".lock"),
        )
        try:
            plan, _ = _read_json(plan_path, MAX_RECEIPT_BYTES)
            if (not isinstance(plan, dict)
                    or plan.get("state") not in {"prepared", "partial", "complete"}):
                raise EvidenceError("retention_plan_invalid")
            receipt_path = candidate / AGGREGATE_RECEIPT_NAME
            receipt, _ = _read_json(receipt_path, MAX_AGGREGATE_BYTES)
            configured = plan.get("configured_count")
            if not isinstance(receipt, dict) or type(configured) is not int:
                raise EvidenceError("retention_plan_invalid")
            if receipt.get("terminal_evidence_published") is True:
                if plan.get("state") != "complete":
                    raise EvidenceError("aggregate_retention_plan_invalid")
                continue
            result = _enforce_retention_locked(candidate, receipt, configured)
            if result.get("status") != "complete":
                raise EvidenceError("retention_pending_plan_incomplete")
            _publish_completed_retention_receipt(candidate, receipt_path, receipt, result)
        finally:
            release_exclusive_file_lock(candidate_lock)


def _enforce_retention(root: Path, current_receipt: dict[str, object], count: int) -> dict[str, object]:
    """Run retention while the caller holds the parent coordinator lock."""
    parent = root.parent
    _reconcile_pending_retention(parent, root)
    return _enforce_retention_locked(root, current_receipt, count)


def _new_retention_plan(
        root: Path, current_entry: tuple[Path, dict[str, object], int] | None,
        current_owner: dict[str, object], count: int, cleanup_root: Path) -> dict[str, object]:
    parent = root.parent
    old: list[tuple[Path, dict[str, object], int]] = []
    try:
        children = sorted(_bounded_children(parent), key=lambda item: item.name)
    except EvidenceError as error:
        raise EvidenceError("retention_parent_unreadable") from error
    for child in children:
        if child == root or not child.name.startswith("native-aggregate-"):
            continue
        if not child.is_dir() or child.is_symlink():
            continue
        resolved = _resolved_receipt(child / AGGREGATE_RECEIPT_NAME)
        if resolved is not None:
            old.append((child, resolved[0], resolved[1]))
    old.sort(key=lambda item: (item[2], item[0].name), reverse=True)
    retained_slots = max(0, count - 1) if current_entry is not None else count
    retained_old = old[:retained_slots]
    victims_raw = old[retained_slots:]
    retained: list[dict[str, object]] = []
    victims: list[dict[str, object]] = []
    for path, candidate, order in retained_old:
        identity = candidate.get("aggregate_owner")
        if not isinstance(identity, dict) or not _identity_matches(path, identity):
            raise EvidenceError("retention_identity_changed")
        retained.append({"path": str(path.absolute()), "identity": identity, "order": order})
    for index, (path, candidate, order) in enumerate(victims_raw):
        identity = candidate.get("aggregate_owner")
        before = _tree_summary(path)
        if (not isinstance(identity, dict) or not _identity_matches(path, identity)
                or before.get("complete") is not True):
            raise EvidenceError("retention_candidate_invalid")
        journal = cleanup_root / f"victim-{index:02d}.json"
        authorization = _retention_authorization(
            current_owner, count, path, identity, before,
        )
        victims.append({
            "path": str(path.absolute()),
            "identity": identity,
            "order": order,
            "before": before,
            "journal_path": str(journal.absolute()),
            "authorization_sha256": authorization,
            "journal_sha256": None,
        })
    plan = {
        "schema_version": SCHEMA_VERSION,
        "kind": "native-acceptance-retention-plan",
        "state": "prepared",
        "configured_count": count,
        "current_root": str(root.absolute()),
        "current_owner": current_owner,
        "current_counted": current_entry is not None,
        "current_order": current_entry[2] if current_entry is not None else None,
        "retained": retained,
        "victims": victims,
        "result": None,
    }
    return plan


def _prune_retention_victims(
        parent: Path, current_owner: dict[str, object], count: int,
        plan_path: Path, plan: dict[str, object]) -> list[str]:
    errors: list[str] = []
    for victim in plan["victims"]:
        if not isinstance(victim, dict):
            raise EvidenceError("retention_plan_invalid")
        path = Path(str(victim.get("path", "")))
        identity = victim.get("identity")
        before = victim.get("before")
        journal = Path(str(victim.get("journal_path", "")))
        authorization = victim.get("authorization_sha256")
        if (not isinstance(identity, dict) or not isinstance(authorization, str)
                or HEX_DIGEST.fullmatch(authorization) is None
                or not isinstance(before, dict)
                or authorization != _retention_authorization(
                    current_owner, count, path, identity, before,
                )):
            raise EvidenceError("retention_plan_invalid")
        victim_lock = -1
        try:
            victim_lock = acquire_exclusive_file_lock(
                path.parent / ("." + path.name + ".lock"),
            )
            if not journal.exists() and not journal.is_symlink():
                prepare_directory_cleanup(
                    journal,
                    "resolved native aggregate retention",
                    identity,
                    parent,
                    authorization,
                )
            else:
                existing = read_directory_cleanup(journal, authorization)
                if existing.get("target") != identity:
                    raise RuntimeError("retention cleanup target identity changed")
            completed = resume_directory_cleanup(journal, authorization, identity)
            if completed.get("state") != "complete" or completed.get("target") != identity:
                raise RuntimeError("retention cleanup did not complete")
            victim["journal_sha256"] = cleanup_journal_digest(journal)
            _write_atomic(plan_path, plan)
            if path.exists() or path.is_symlink():
                raise RuntimeError("retention target pathname was reoccupied")
        except (OSError, RuntimeError, ValueError) as error:
            errors.append(f"{path.name}:{str(error)[:96]}")
            break
        finally:
            if victim_lock >= 0:
                release_exclusive_file_lock(victim_lock)

    return errors


def _retention_result(
        current_entry: tuple[Path, dict[str, object], int] | None,
        count: int, plan: dict[str, object], errors: list[str]) -> dict[str, object]:
    retained_entries: list[tuple[Path, dict[str, object], int]] = []
    if current_entry is not None:
        retained_entries.append(current_entry)
    for retained in plan["retained"]:
        if not isinstance(retained, dict):
            raise EvidenceError("retention_plan_invalid")
        path = Path(str(retained.get("path", "")))
        identity = retained.get("identity")
        order = retained.get("order")
        resolved = _resolved_receipt(path / AGGREGATE_RECEIPT_NAME)
        if (not isinstance(identity, dict) or type(order) is not int
                or not _identity_matches(path, identity) or resolved is None):
            errors.append(f"{path.name}:retention_retained_root_changed")
            continue
        retained_entries.append((path, resolved[0], order))
    after_summary = _aggregate_root_summary(retained_entries)
    before_summary = _summary_with_removed(after_summary, plan["victims"])
    pruned = [
        {
            "path": victim["path"],
            "before": victim["before"],
            "journal_path": victim["journal_path"],
            "journal_sha256": victim["journal_sha256"],
        }
        for victim in plan["victims"]
        if isinstance(victim, dict) and isinstance(victim.get("journal_sha256"), str)
    ]
    result = {
        "configured_count": count,
        "before": before_summary,
        "after": after_summary,
        "pruned": pruned,
        "errors": errors,
        "status": "complete" if (not errors and len(retained_entries) <= count
                                   and len(pruned) == len(plan["victims"])
                                   and before_summary.get("complete") is True
                                   and after_summary.get("complete") is True) else "partial",
    }
    return result


def _enforce_retention_locked(
        root: Path, current_receipt: dict[str, object], count: int) -> dict[str, object]:
    current_entry = _current_retention_entry(root, current_receipt)
    current_owner = current_receipt.get("aggregate_owner")
    if (not isinstance(current_owner, dict)
            or not _identity_matches(root, current_owner)):
        raise EvidenceError("retention_current_root_invalid")
    plan_path = root / RETENTION_PLAN_NAME
    cleanup_root = root / RETENTION_CLEANUP_ROOT_NAME
    cleanup_root.mkdir(mode=0o700, exist_ok=True)
    _ensure_directory(cleanup_root)
    if plan_path.exists() or plan_path.is_symlink():
        if plan_path.is_symlink():
            raise EvidenceError("retention_plan_invalid")
        plan_value, _ = _read_json(plan_path, MAX_RECEIPT_BYTES)
        plan = _validate_retention_plan(plan_value, root, current_receipt, count)
        if plan["state"] == "complete":
            return _retention_plan_result(plan, plan_path)
    else:
        plan = _new_retention_plan(root, current_entry, current_owner, count, cleanup_root)
        _write_atomic(plan_path, plan)
    errors = _prune_retention_victims(root.parent, current_owner, count, plan_path, plan)
    result = _retention_result(current_entry, count, plan, errors)
    plan["state"] = "complete" if result["status"] == "complete" else "partial"
    plan["result"] = result
    _write_atomic(plan_path, plan)
    return _retention_plan_result(plan, plan_path)


def _publish_completed_retention_receipt(
        root: Path,
        receipt_path: Path,
        receipt: dict[str, object],
        retention_result: dict[str, object]) -> dict[str, object]:
    """Publish one terminal aggregate after its retention plan is durable."""
    if retention_result.get("status") != "complete":
        raise EvidenceError("aggregate_retention_incomplete")
    if receipt.get("terminal_evidence_published") is not False:
        raise EvidenceError("aggregate_terminal_receipt_immutable")
    verdict = receipt.get("case_results_verdict")
    cases = receipt.get("cases")
    policy = receipt.get("policy")
    if (verdict not in {"PASS", "BLOCKED", "FAIL"}
            or not isinstance(cases, list)
            or not isinstance(policy, dict)
            or type(policy.get("require_at_least_one_pass")) is not bool):
        raise EvidenceError("aggregate_receipt_invalid")
    unresolved = [
        case["case_id"] for case in cases
        if isinstance(case, dict) and case.get("evidence_status") == "UNRESOLVED"
        and isinstance(case.get("case_id"), str)
    ]
    receipt["retention"] = retention_result
    receipt["terminal_evidence_published"] = True
    receipt["status"] = verdict
    receipt["unresolved_case_count"] = len(unresolved)
    receipt["unresolved_cases"] = unresolved
    receipt["exit_code"] = _aggregate_exit(
        verdict, verdict, bool(policy["require_at_least_one_pass"]),
    )
    evidence = receipt.get("evidence")
    if not isinstance(evidence, dict):
        raise EvidenceError("aggregate_evidence_invalid")
    evidence["case_raw"] = {
        "before": evidence.get("case_raw", {}).get("before")
        if isinstance(evidence.get("case_raw"), dict) else _empty_summary(),
        "after": _tree_summary(root / CASE_ROOT_NAME),
    }
    evidence["compact_receipts"] = _tree_summary(root / COMPACT_RECEIPT_ROOT_NAME)
    _write_atomic(receipt_path, receipt, MAX_AGGREGATE_BYTES)
    validate_aggregate_receipt(
        receipt_path,
        expected_tag=receipt.get("tag") if isinstance(receipt.get("tag"), str) else None,
        expected_source_revision=(
            receipt.get("source_revision")
            if isinstance(receipt.get("source_revision"), str) else None
        ),
    )
    return receipt
