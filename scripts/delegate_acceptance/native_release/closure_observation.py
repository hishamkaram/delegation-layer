"""Read-only observation of one candidate invocation's retained closure state.

The original coordinator receipt, its proof ZIP and every retained private
record stay unchanged. This owner loads the receipt and verifies its root
identity, re-validates the unchanged proof against the persisted expectation,
re-reads the native child's natural completion, and for each unresolved case
verifies the private records the case bound by digest, every retained owned
directory identity, every retained capture's natural Wait, the model
discovery admission and the canonical managed-lifetime projection of each
state root. It classifies the bound identities from the case's own failure
diagnostics. It writes one new sanitized supplement and judges it with the
same semantic reader replay uses; it never cleans, retries, signals or
launches anything. A missing, changed or contradictory record refuses.
"""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import sys

from scripts.delegate_acceptance.evidence.bounded_files import BoundedReadError, read_bounded_regular_file
from scripts.delegate_acceptance.evidence.files import write_bytes
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.native.acceptance_lifecycle import managed_root_closure
from scripts.delegate_acceptance.native_evidence.candidate_proof import validate_candidate_proof
from scripts.delegate_acceptance.native_evidence.lifetime_closure import (
    LIFETIME_CLOSURE_KIND, LIFETIME_CLOSURE_SCHEMA_VERSION,
)
from scripts.delegate_acceptance.native_evidence.public_contract import (
    MAX_PUBLIC_ZIP_BYTES, CaseClosureAnchor, PublicProofError, ValidatedCandidateProof, canonical_json,
    parse_json,
)
from scripts.delegate_acceptance.release_verification.candidate_proof_closure import (
    CaseClosure, OrdinaryTask, ProofClosure, RetainedObservation, encode_proof_closure, read_proof_closure,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from .candidate import candidate_expectation
from .contract import ReleaseError, candidate_request, object_value, text_value
from .execution import observe_child_completion, read_natural_capture
from .storage import load_receipt, safe_path, validate_identity


MAX_CASE_RECORD_BYTES = 1 << 20


# The labels the native case cleanup receipt gives the retained owned directories this owner reads.
STATE_LABEL, MODELS_LABEL, CAPTURES_LABEL = "acceptance state", "model discovery state", "raw process captures"


def _sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _bound(path: Path, expected: str | None) -> bytes:
    """Read one private record the case bound by digest; an unbound or changed record refuses."""
    data = read_bounded_regular_file(path, MAX_CASE_RECORD_BYTES)
    if expected is None or _sha(data) != expected:
        raise ReleaseError("closure_record_binding_mismatch", incomplete=True)
    return data


def _retained(cleanup: dict[str, object]) -> dict[str, dict[str, object]]:
    """Verify every retained owned directory's recorded identity; return them by label.

    The acceptance finalizer writes ``removed`` when its chosen plan finished
    without failures after observed supervisor shutdown, and lists a failed
    run's explicitly retained sources; ``retained`` means shutdown was not
    observed and ``partial`` that the plan failed, so neither closes anything.
    """
    labels, identities = cleanup.get("retained"), cleanup.get("retained_owned_directories")
    if (cleanup.get("status") != "removed" or cleanup.get("supervisor_shutdown_observed") is not True
            or cleanup.get("completion") != "failed" or "failures" in cleanup
            or cleanup.get("raw_process_captures") != "retained"
            or not isinstance(labels, list) or not isinstance(identities, list)
            or len(labels) != len(identities) or len(set(map(str, labels))) != len(labels)):
        raise ReleaseError("closure_retention_invalid", incomplete=True)
    retained = {}
    for label, identity in zip(labels, identities, strict=True):
        identity = object_value(identity)
        validate_identity(identity, Path(text_value(identity.get("path"))))
        retained[text_value(label)] = identity
    if not {STATE_LABEL, MODELS_LABEL, CAPTURES_LABEL} <= set(retained):
        raise ReleaseError("closure_retention_incomplete", incomplete=True)
    return retained


def _captures(directory: Path) -> list[dict[str, str]]:
    """Every retained capture is a directory whose own records prove a natural Wait."""
    entries = sorted(directory.iterdir(), key=lambda entry: entry.name)
    if not entries or not all(entry.is_dir() and not entry.is_symlink() for entry in entries):
        raise ReleaseError("closure_capture_ambiguous", incomplete=True)
    captures = []
    for entry in entries:
        capture = read_natural_capture(entry)
        captures.append({"name": entry.name, "started_sha256": capture.started_sha256,
                         "invocation_sha256": capture.invocation_sha256,
                         "completed_sha256": capture.completed_sha256})
    return captures


def _classified(diagnostics: dict[str, object]) -> tuple[tuple[OrdinaryTask, ...], str | None, bool]:
    """Ordinary tasks, the discovery inspection and the discovery admission the diagnostics recorded."""
    tasks = []
    for item in diagnostics.get("tasks") or []:
        task = object_value(item)
        files = object_value(task.get("files"))
        tasks.append(OrdinaryTask(task_id=text_value(task.get("task_id"), "[0-9a-f]{32}"),
                                  spec_sha256=text_value(object_value(files.get("task.json")).get("sha256")),
                                  meta_sha256=text_value(object_value(files.get("meta.json")).get("sha256"))))
    inspection = diagnostics.get("model_discovery_inspection")
    discovery = None if inspection is None else object_value(inspection).get("journal_task_id")
    admitted = object_value(diagnostics.get("failure")).get("discover_models")
    if type(admitted) is not bool or (discovery is not None and not admitted):
        raise ReleaseError("closure_discovery_admission_invalid", incomplete=True)
    return tuple(sorted(tasks, key=lambda task: task.task_id)), None if discovery is None else str(discovery), admitted


def _no_launch(state: Path) -> bool:
    return not (state / "tasks").exists() and not (state / "inspections").exists()


def _closure(state: Path, models: Path, ordinary: tuple[OrdinaryTask, ...],
             discovery: str | None, admitted: bool) -> dict[str, object]:
    """Project each root through the canonical managed-lifetime projection; a no-launch root admitted nothing."""
    if discovery is None and (admitted or not _no_launch(models)):
        raise ReleaseError("closure_discovery_unresolved", incomplete=True)
    no_launch = {"root_id": None, "launch": "none", "tasks": {}, "inspections": {}}
    ordinary_root = (no_launch if not ordinary and _no_launch(state)
                     else managed_root_closure(state, {task.task_id for task in ordinary}))
    return {"schema_version": LIFETIME_CLOSURE_SCHEMA_VERSION, "kind": LIFETIME_CLOSURE_KIND,
            "ordinary": ordinary_root,
            "model_discovery": None if discovery is None else managed_root_closure(models, set())}


def observe_case_closure(native: Path, anchor: CaseClosureAnchor) -> tuple[CaseClosure, list[object], list[object]]:
    """The case's closure, its retained directory identities and its retained captures."""
    case_dir = native / "cases" / anchor.case_id
    _bound(native / "receipts" / f"{anchor.case_id}.json", anchor.source_compact_sha256)
    _bound(case_dir / "failure.json", anchor.source_receipt_sha256)
    cleanup = object_value(parse_json(_bound(case_dir / "cleanup.json", anchor.cleanup_receipt_sha256)))
    diagnostics = object_value(parse_json(_bound(case_dir / "failure-diagnostics.json",
                                                 anchor.failure_diagnostics_sha256)))
    retained = _retained(cleanup)
    captures = Path(text_value(retained[CAPTURES_LABEL].get("path")))
    if captures != case_dir / "processes":
        raise ReleaseError("closure_capture_location_invalid", incomplete=True)
    ordinary, discovery, admitted = _classified(diagnostics)
    closure = _closure(Path(text_value(retained[STATE_LABEL].get("path"))),
                       Path(text_value(retained[MODELS_LABEL].get("path"))), ordinary, discovery, admitted)
    case = CaseClosure(
        case_id=anchor.case_id, source_compact_sha256=anchor.source_compact_sha256,
        source_receipt_sha256=str(anchor.source_receipt_sha256),
        cleanup_receipt_sha256=str(anchor.cleanup_receipt_sha256),
        failure_diagnostics_sha256=str(anchor.failure_diagnostics_sha256), ordinary_tasks=ordinary,
        discovery_inspection_id=discovery, lifetime_closure=closure)
    return case, list(retained.values()), [{"case_id": anchor.case_id, **item} for item in _captures(captures)]


def observe_proof_closure(receipt_path: Path, proof_sha256: str) -> bytes:
    """Return the supplement bytes for every unresolved case of the receipt's unchanged proof."""
    receipt = load_receipt(receipt_path)
    invocation = receipt.invocation
    expectation = candidate_expectation(receipt, candidate_request(invocation))
    data = read_bounded_regular_file(invocation.root / invocation.proof_name, MAX_PUBLIC_ZIP_BYTES)
    if _sha(data) != proof_sha256:
        raise ReleaseError("closure_proof_changed", incomplete=True)
    validated: ValidatedCandidateProof = validate_candidate_proof(data, expectation)
    completion = observe_child_completion(receipt, "native")
    if completion != validated.completion or not validated.closure_anchors:
        raise ReleaseError("closure_native_completion_mismatch", incomplete=True)
    observed = [observe_case_closure(invocation.root / "native", anchor) for anchor in validated.closure_anchors]
    identities = [invocation.directory_identity, *(item for _, retained, _ in observed for item in retained)]
    captures = [item for _, _, case_captures in observed for item in case_captures]
    supplement = encode_proof_closure(ProofClosure(
        proof_sha256=validated.public_zip_sha256, invocation_id=validated.invocation_id,
        started_sha256=completion.started_sha256, invocation_sha256=completion.invocation_sha256,
        completed_sha256=completion.completed_sha256,
        observation=RetainedObservation(
            coordinator_receipt_sha256=_sha(read_bounded_regular_file(receipt_path, MAX_CASE_RECORD_BYTES)),
            directory_identity_sha256=_sha(canonical_json(identities)),
            capture_records_sha256=_sha(canonical_json(captures)), capture_count=len(captures)),
        cases=tuple(case for case, _, _ in observed)))
    # The one semantic reader replay uses judges the supplement before anything is written.
    read_proof_closure(supplement, (validated,))
    return supplement


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Observe one candidate invocation's retained closure; read-only.")
    parser.add_argument("--receipt", type=Path, required=True, help="the original coordinator receipt.json")
    parser.add_argument("--proof-sha256", required=True, help="the stored proof ZIP's SHA-256")
    parser.add_argument("--output", type=Path, required=True,
                        help="a new supplement file outside the invocation root; an existing one is refused")
    return parser


def main(argv: list[str] | None = None) -> int:
    """0 the supplement was written; 1 the retained state does not prove its closure."""
    arguments = _parser().parse_args(argv)
    output, receipt = arguments.output.absolute(), arguments.receipt.absolute()
    try:
        # A clean path with no symlink component names itself, so lexical containment is exact.
        safe_path(output)
        safe_path(receipt)
        if output.is_relative_to(receipt.parent) or output.exists():
            raise ReleaseError("closure_output_unsafe")
        supplement = observe_proof_closure(receipt, arguments.proof_sha256)
        write_bytes(output, supplement)
    except (ReleaseError, VerificationError, PublicProofError, AcceptanceFailure, BoundedReadError,
            OSError, ValueError) as error:
        code = getattr(error, "code", type(error).__name__)
        print(json.dumps({"status": "refused", "code": code}, sort_keys=True))
        return 1
    print(json.dumps({"status": "written", "supplement_sha256": _sha(supplement)}, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
