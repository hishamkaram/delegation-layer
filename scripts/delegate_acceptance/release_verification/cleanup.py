"""Cleanup for exact release verification."""

from __future__ import annotations

from . import filesystem

from pathlib import Path
import json
import os
import sys
from ..directories.operations import DirectoryOwner
from ..directories.journal import cleanup_journal_digest
from ..directories.journal import prepare_directory_cleanup
from ..directories.journal import read_directory_cleanup
from ..directories.journal import resume_directory_cleanup
from .contract import (MAX_RECEIPT, VerificationError, _release_cleanup_authorization, _release_cleanup_transaction, set_failure)
from .filesystem import _bounded_file_bytes


def _validate_completed_cleanup(receipt: dict) -> int:
    cleanup, transaction, authorization = _release_cleanup_transaction(receipt)
    journal = Path(transaction["journal_path"])
    try:
        record = read_directory_cleanup(journal, authorization)
        digest = cleanup_journal_digest(journal)
    except (OSError, RuntimeError, ValueError) as error:
        raise VerificationError("release_cleanup_journal_invalid", "receipt-replay") from error
    if (receipt.get("verdict") not in {"pass", "blocked"}
            or cleanup.get("status") != "complete"
            or record.get("state") != "complete"
            or record.get("target") != transaction["target_identity"]
            or transaction.get("journal_sha256") != digest):
        raise VerificationError("release_terminal_receipt_invalid", "receipt-replay")
    return 0


def _resume_cleanup_receipt(path: Path, receipt: dict) -> int:
    cleanup, transaction, authorization = _release_cleanup_transaction(receipt)
    journal = Path(transaction["journal_path"])
    expected_journal = path.with_name(path.stem + ".cleanup.json")
    if journal.absolute() != expected_journal.absolute():
        raise VerificationError("release_cleanup_journal_mismatch", "cleanup-replay")
    identity = transaction["target_identity"]
    try:
        if not journal.exists() and not journal.is_symlink():
            prepare_directory_cleanup(
                journal,
                "release verification assets",
                identity,
                Path(identity["parent"]),
                authorization,
            )
        else:
            record = read_directory_cleanup(journal, authorization)
            if record.get("target") != identity:
                raise RuntimeError("release cleanup target identity changed")
        record = resume_directory_cleanup(journal, authorization, identity)
        digest = cleanup_journal_digest(journal)
    except (OSError, RuntimeError, ValueError) as error:
        raise VerificationError("release_cleanup_failed", "cleanup-replay") from error
    if record.get("state") != "complete" or record.get("target") != identity:
        raise VerificationError("release_cleanup_incomplete", "cleanup-replay")
    receipt["verdict"] = receipt["proof"]["final_verdict"]
    receipt["stage"] = "complete"
    identities = cleanup.get("directory_identities")
    removed = list(cleanup.get("removed_paths", []))
    removed.extend(sorted(identities) if isinstance(identities, dict) else [identity["path"]])
    receipt["cleanup"].update({
        "status": "complete",
        "removed_paths": list(dict.fromkeys(removed)),
        "retained_paths": [],
    })
    receipt["cleanup"]["transaction"] = {
        **transaction,
        "journal_sha256": digest,
    }
    receipt["failure"] = None
    receipt["completion"] = {"state": "complete"}
    filesystem.write_receipt(path, receipt)
    return 0


def _replay_prepared_cleanup(
        path: Path,
        *,
        gate: str,
        claim: str,
        tag: str,
        revision: str) -> int | None:
    if not path.exists() and not path.is_symlink():
        return None
    if path.is_symlink():
        raise VerificationError("release_existing_receipt_invalid", "receipt-replay")
    try:
        raw = _bounded_file_bytes(path, MAX_RECEIPT, "release_cleanup_journal_invalid")
        receipt = json.loads(raw)
    except (VerificationError, UnicodeError, json.JSONDecodeError, RecursionError) as error:
        raise VerificationError("release_existing_receipt_invalid", "receipt-replay") from error
    if not isinstance(receipt, dict):
        raise VerificationError("release_existing_receipt_invalid", "receipt-replay")
    matches_request = (receipt.get("gate") == gate and receipt.get("claim") == claim
                       and receipt.get("tag") == tag
                       and receipt.get("source_revision") == revision)
    completion = receipt.get("completion")
    if (isinstance(completion, dict) and completion.get("state") == "complete"):
        if not matches_request:
            raise VerificationError("release_terminal_receipt_mismatch", "receipt-replay")
        return _validate_completed_cleanup(receipt)
    cleanup = receipt.get("cleanup")
    if (receipt.get("verdict") == "fail" and isinstance(cleanup, dict)
            and cleanup.get("status") == "retained-for-investigation"):
        if not matches_request:
            raise VerificationError("release_terminal_receipt_mismatch", "receipt-replay")
        return 1
    if (receipt.get("stage") == "complete" and receipt.get("verdict") in {"pass", "blocked"}):
        if not matches_request:
            raise VerificationError("release_terminal_receipt_mismatch", "receipt-replay")
        if (isinstance(cleanup, dict)
                and cleanup.get("status") == "retained-for-native-acceptance"):
            return 0
        raise VerificationError("release_terminal_receipt_invalid", "receipt-replay")
    if (not isinstance(cleanup, dict) or cleanup.get("status") not in {"prepared", "incomplete"}
            or not isinstance(completion, dict) or completion.get("state") != "prepared"):
        if receipt.get("verdict") != "in_progress" or receipt.get("stage") != "initialize":
            raise VerificationError("release_existing_receipt_invalid", "receipt-replay")
        if not matches_request:
            raise VerificationError("release_existing_receipt_mismatch", "receipt-replay")
        return None
    if not matches_request:
        raise VerificationError("release_cleanup_journal_mismatch", "cleanup-replay")
    return _resume_cleanup_receipt(path, receipt)


def _finish_removed_gate(
        path: Path, receipt: dict, owner: DirectoryOwner, target: dict,
        final_verdict: str, incidental_removed: list[str]) -> int:
    identities = receipt["cleanup"]["directory_identities"]
    journal = path.with_name(path.stem + ".cleanup.json")
    authorization = _release_cleanup_authorization(receipt, final_verdict, target)
    receipt["stage"] = "cleanup-prepared"
    receipt["verdict"] = "in_progress"
    receipt["failure"] = None
    receipt["proof"] = {
        "state": "committed",
        "authorization_sha256": authorization,
        "final_verdict": final_verdict,
    }
    receipt["cleanup"].update({
        "status": "prepared",
        "removed_paths": incidental_removed,
        "retained_paths": [],
        "transaction": {
            "journal_path": str(journal.absolute()),
            "authorization_sha256": authorization,
            "target_identity": target,
        },
    })
    receipt["completion"] = {"state": "prepared"}
    try:
        filesystem.write_receipt(path, receipt)
    except BaseException:
        owner.release()
        raise

    try:
        owner.release()
        prepare_directory_cleanup(
            journal,
            "release verification assets",
            target,
            Path(target["parent"]),
            authorization,
        )
        record = resume_directory_cleanup(journal, authorization, target)
        digest = cleanup_journal_digest(journal)
        if record.get("state") != "complete" or record.get("target") != target:
            raise RuntimeError("release cleanup did not reach its committed state")
    except (OSError, RuntimeError, ValueError) as caught:
        receipt["cleanup"]["status"] = "incomplete"
        receipt["cleanup"]["errors"].append("release_cleanup_failed")
        set_failure(receipt, "release_cleanup_failed", "cleanup")
        receipt["completion"] = {"state": "prepared"}
        filesystem.write_receipt(path, receipt)
        print("release verification failed: release_cleanup_failed", file=sys.stderr)
        return 1

    receipt["verdict"] = final_verdict
    receipt["stage"] = "complete"
    receipt["failure"] = None
    receipt["cleanup"].update({
        "status": "complete",
        "removed_paths": list(dict.fromkeys([*incidental_removed, *identities])),
        "retained_paths": [],
        "transaction": {
            **receipt["cleanup"]["transaction"],
            "journal_sha256": digest,
        },
    })
    receipt["completion"] = {"state": "complete"}
    filesystem.write_receipt(path, receipt)
    return 0


def _finish_retained_gate(
        path: Path, receipt: dict, owner: DirectoryOwner,
        error: VerificationError | None, final_verdict: str,
        incidental_removed: list[str], locations: dict[str, Path]) -> int:
    identities = receipt["cleanup"]["directory_identities"]
    try:
        owner.release()
    except Exception:
        receipt["cleanup"]["errors"].append("owned_directory_identity_check_failed")
        if error is None:
            error = VerificationError("release_cleanup_failed", "cleanup")

    paths = list(identities)
    if error is None:
        receipt["verdict"] = final_verdict
        receipt["stage"] = "complete"
        receipt["cleanup"].update({
            "status": "retained-for-native-acceptance",
            "removed_paths": incidental_removed,
            "retained_paths": paths,
        })
    else:
        set_failure(receipt, error.code, error.stage)
        receipt["cleanup"]["status"] = "retained-for-investigation"
        retained = paths + [str(item) for item in locations.values() if os.path.lexists(item)]
        receipt["cleanup"]["retained_paths"] = list(dict.fromkeys(
            item for item in retained if os.path.lexists(item)
        ))
    filesystem.write_receipt(path, receipt)
    if error is not None:
        print(f"release verification failed: {error.code}", file=sys.stderr)
        return 1
    return 0


def finish_gate(path: Path, receipt: dict, owner: DirectoryOwner, action,
                cleanup_mode: str, locations: dict[str, Path]) -> int:
    error = None
    try:
        action()
    except VerificationError as caught:
        error = caught
    except Exception:
        error = VerificationError("verification_internal_error", receipt["stage"])

    identities = receipt["cleanup"]["directory_identities"]
    temporary = locations.get("isolated_home")
    incidental_removed = (
        [str(temporary)] if temporary is not None and not os.path.lexists(temporary) else []
    )
    final_verdict = receipt.get("verdict") if receipt.get("verdict") == "blocked" else "pass"
    if error is None and cleanup_mode == "remove":
        assets = locations.get("assets")
        target = identities.get(str(assets.resolve())) if assets is not None else None
        if not isinstance(target, dict):
            error = VerificationError("release_cleanup_identity_missing", "cleanup")

    if error is None and cleanup_mode == "remove":
        return _finish_removed_gate(
            path, receipt, owner, target, final_verdict, incidental_removed,
        )
    return _finish_retained_gate(
        path, receipt, owner, error, final_verdict, incidental_removed, locations,
    )
