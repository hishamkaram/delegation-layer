"""Release scratch cleanup reuses canonical identity-pinned directory journals."""

from __future__ import annotations

from pathlib import Path

from scripts.delegate_acceptance.directories.journal import (
    cleanup_journal_digest, prepare_directory_cleanup, read_directory_cleanup, resume_directory_cleanup,
)
from scripts.delegate_acceptance.native_evidence.public_contract import canonical_json, sha256
from .contract import ReleaseError, digest_value, object_value, release_request, text_value
from .execution import observe_child
from .receipt import Receipt


def cleanup(receipt: Receipt) -> None:
    request = release_request(receipt.invocation)
    project = receipt.stage("project").result
    if (receipt.stage("native").result.get("lifetime_closed") is not True
            or project.get("failed_count") != 0 or project.get("unresolved_case_count") != 0):
        return  # A real native failure retains all private evidence.
    if not request.no_submit and receipt.stage("watch").result.get("promotable") is not True:
        return
    _authorized_cleanup(receipt, children=("archive", "native", "finalize"),
                        authorization={"consumer": receipt.stage("watch").result, "no_submit": request.no_submit})


def cleanup_candidate(receipt: Receipt) -> None:
    """Remove candidate scratch only after this invocation's exact proof append is confirmed.

    Only a durable confirmation proves that the store command which appended
    the proof completed; a readback proves the proof landed, not that command's
    closure. The authorization is the record stage's stored fact, never a
    release watch; the native run is the only child. Anything else, including a
    stale cleanup stage beside a readback, retains every private file.
    """
    project, record = receipt.stage("project"), receipt.stage("record")
    if (project.state != "complete" or record.state != "complete"
            or record.result.get("disposition") != "confirmed"
            or project.result.get("case_results_verdict") == "FAIL"
            or project.result.get("failed_count") != 0 or project.result.get("unresolved_case_ids") != []):
        return
    _authorized_cleanup(receipt, children=("native",), authorization={"consumer": record.result})


def _authorized_cleanup(receipt: Receipt, *, children: tuple[str, ...],
                        authorization: dict[str, object]) -> None:
    """Bind the cleanup to the target, the native result, the proof and its consumer, then replay it."""
    if receipt.stage("cleanup").state == "complete":
        _replay(receipt)
        return
    identity = object_value(receipt.stage("download").result.get("runner_identity"))
    digest = sha256(canonical_json({
        "invocation_id": receipt.invocation.invocation_id, "target": identity,
        "native": receipt.stage("native").result, "proof": receipt.stage("project").result, **authorization,
    }))
    if receipt.stage("cleanup").state == "pending":
        for name in children:
            observed = observe_child(receipt, name)
            if any(receipt.stage(name).result.get(key) != value for key, value in observed.items()):
                raise ReleaseError("cleanup_child_completion_changed", incomplete=True)
    receipt.intend("cleanup", {"target": identity, "authorization_sha256": digest})
    _replay(receipt)


def _replay(receipt: Receipt) -> None:
    intent = receipt.stage("cleanup").intent
    identity = object_value(intent.get("target"))
    if identity.get("path") != str(receipt.invocation.root / "runner"):
        raise ReleaseError("cleanup_target_mismatch", incomplete=True)
    authorization = digest_value(intent.get("authorization_sha256"))
    journal = receipt.invocation.root / "runner.cleanup.json"
    try:
        if journal.exists() or journal.is_symlink():
            record = read_directory_cleanup(journal, authorization)
            if record.get("target") != identity:
                raise ReleaseError("cleanup_journal_target_conflict", incomplete=True)
        else:
            prepare_directory_cleanup(journal, "native release scratch", identity,
                                      Path(text_value(identity["parent"])), authorization)
        record = resume_directory_cleanup(journal, authorization, identity)
        if record.get("state") != "complete":
            raise ReleaseError("release_scratch_cleanup_incomplete", incomplete=True)
        receipt.complete("cleanup", {"journal_sha256": cleanup_journal_digest(journal)})
    except (OSError, RuntimeError, ValueError) as error:
        raise ReleaseError("release_scratch_cleanup_unresolved", incomplete=True) from error
