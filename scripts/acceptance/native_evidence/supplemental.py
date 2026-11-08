"""Bind supplemental public-continue cases and the Pi session-miss control to one candidate.

The canonical 17-case aggregate stays the only positive matrix. The
supplemental group re-runs its twelve plain resumable rows with unchanged case
identities, admitting each successor through public ``continue``; the Pi
control proves a continuation under an empty session directory is refused.

Each native case's evidence stays authoritative and is only observed here:
the aggregate's own per-case observation and the canonical compact proof
validator judge every case, and nothing is compacted, created, deleted or
rewritten, so a successful or refused validation can be repeated from the same
evidence. Case cleanup remains with its own owner. The returned summary is the
local handoff artifact, never a store; ``scripts/acceptance/native_evidence/continuation_cli.py``
is the one command that qualifies it against a registered candidate.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
import platform
import re
from typing import Mapping, TypedDict

from scripts.acceptance.native.constants import PI_SESSION_MISS_GATE
from scripts.acceptance.native.constants import PI_SESSION_MISS_SCENARIO
from scripts.acceptance.evidence.source import VerificationError
from scripts.acceptance.evidence.source import observe_harness
from scripts.acceptance.native_evidence.source_contract import NATIVE_SOURCE_SEEDS
from scripts.acceptance.native_evidence.aggregate import _observe_case
from scripts.acceptance.native_evidence.cases import _sanitize_case
from scripts.acceptance.native_evidence.contract import EXPECTED_CASES
from scripts.acceptance.native_evidence.contract import MAX_INPUT_BYTES
from scripts.acceptance.native_evidence.contract import MAX_RECEIPT_BYTES
from scripts.acceptance.native_evidence.contract import CaseDefinition
from scripts.acceptance.native_evidence.contract import EvidenceError
from scripts.acceptance.native_evidence.contract import MatrixRow
from scripts.acceptance.native_evidence.contract import _digest_bytes
from scripts.acceptance.native_evidence.contract import _parse_optional_revision
from scripts.acceptance.native_evidence.filesystem import _read_json
from scripts.acceptance.native_evidence.filesystem import _tree_summary
from scripts.acceptance.native_evidence.lifetime_closure import lifetime_closure_error
from scripts.acceptance.native_evidence.proof import _cleanup_observation
from scripts.acceptance.native_evidence.validation import validate_aggregate_receipt
from scripts.acceptance.native_evidence.validation import validate_case_proof


CANDIDATE_BINARIES = ("delegate", "delegate-run", "pueue", "pueued")
# NativeTaskOps writes one attempt record per admitted command, named for that command.
CONTINUE_ATTEMPT_NAME = "resume-continue-attempt.json"
DISPATCH_ATTEMPT_NAME = "resume-dispatch-attempt.json"
# The one harness both the producer and the qualified validator observe: the
# native release gate's entry points plus the validator command's import closure.
CONTINUATION_HARNESS_SEEDS = (
    *NATIVE_SOURCE_SEEDS, "scripts/acceptance/native_evidence/continuation_cli.py",
)
PI_SESSION_DIRECTORIES = {"fresh": "control", "resume": "control", "session_miss": "empty"}
CONTINUATION_TMPDIR_SCOPES = {"fresh": "predecessor", "resume": "successor", "replay": "ambient"}
_TASK_ID = re.compile(r"[0-9a-f]{32}")


class HarnessObservation(TypedDict):
    harness_revision: str
    harness_sha256: str
    committed: bool


class ContinueCase(TypedDict):
    case_id: str
    command: str
    tasks: dict[str, str]
    source_receipt_sha256: str | None
    continue_attempt_sha256: str
    source_proof: dict[str, object]


class SupplementalSummary(TypedDict):
    kind: str
    claim: str
    source_revision: str
    harness_revision: str
    harness_sha256: str
    canonical_aggregate_sha256: str
    binaries: dict[str, str]
    host_os: str
    cases: list[ContinueCase]
    pi_session_miss: dict[str, object]
    credentials_in_receipt: bool


def supplemental_cases() -> tuple[CaseDefinition, ...]:
    """Return the canonical plain resumable rows the supplemental group repeats."""
    return tuple(case for case in EXPECTED_CASES
                 if case.scenario in {"read-only", "write"} and case.provider != "codex:review")


def observe_continuation_harness(environment: Mapping[str, str], checkout: Path) -> HarnessObservation:
    """Observe the HEAD, import-closure fingerprint and committed match of the executing harness."""
    try:
        head, fingerprint, committed = observe_harness(
            {"commands": []}, environment, checkout=checkout, seeds=CONTINUATION_HARNESS_SEEDS)
    except VerificationError as error:
        raise EvidenceError("harness_observation_" + error.code) from error
    return {"harness_revision": head, "harness_sha256": fingerprint, "committed": committed}


@dataclass(frozen=True)
class Candidate:
    """The frozen candidate one canonical aggregate proves, under the observed committed harness."""
    harness: Mapping[str, object]
    aggregate_sha256: str
    binaries: Mapping[str, str]
    task_ids: frozenset[str]


def candidate_from_aggregate(receipt: Path, harness: Mapping[str, object], delegate: Path) -> Candidate:
    """Read the candidate from a complete, all-PASS canonical aggregate bound to the observed harness."""
    if harness.get("committed") is not True:
        raise EvidenceError("supplemental_harness_not_committed")
    value, aggregate_sha256 = validate_aggregate_receipt(
        receipt, expected_source_revision=str(harness["harness_revision"]))
    # The canonical validator also accepts BLOCKED and FAIL verdicts; only a full pass names a
    # candidate. The aggregate verdict also covers unresolved evidence and retention.
    if value["status"] != "PASS":
        raise EvidenceError("canonical_aggregate_not_passed")
    binaries: dict[str, str] | None = None
    task_ids: set[str] = set()
    cases = value["cases"]
    if not isinstance(cases, list):
        raise EvidenceError("canonical_aggregate_not_passed")
    for case in cases:
        identities = case.get("binary_identities") if isinstance(case, dict) else None
        if not isinstance(identities, dict):
            raise EvidenceError("canonical_candidate_binaries_incomplete")
        recorded_tasks = identities.get("task_ids")
        if not isinstance(recorded_tasks, list) or any(not isinstance(task, str) for task in recorded_tasks):
            raise EvidenceError("canonical_candidate_binaries_incomplete")
        task_ids.update(recorded_tasks)
        if case["status"] != "PASS":
            raise EvidenceError("canonical_aggregate_not_passed")
        paths = identities.get("paths")
        if not isinstance(paths, dict) or set(paths) != set(CANDIDATE_BINARIES):
            raise EvidenceError("canonical_candidate_selection_incomplete")
        if identities["state"] != "complete":
            raise EvidenceError("canonical_candidate_binaries_incomplete")
        selected: dict[str, str] = {}
        for key in CANDIDATE_BINARIES:
            installed_path = paths.get(key)
            if not isinstance(installed_path, str):
                raise EvidenceError("canonical_candidate_binaries_incomplete")
            selected[key] = installed_path
        if binaries is not None and selected != binaries:
            raise EvidenceError("canonical_candidate_binaries_inconsistent")
        binaries = selected
    if binaries is None:
        raise EvidenceError("canonical_candidate_binaries_unavailable")
    if delegate.resolve(strict=True) != Path(binaries["delegate"]).resolve(strict=True):
        raise EvidenceError("candidate_delegate_mismatch")
    return Candidate(dict(harness), aggregate_sha256, binaries, frozenset(task_ids))


def require_selected_binaries(candidate: Candidate, selected: Mapping[str, Path]) -> None:
    """Refuse a run whose resolved installed paths are not the candidate selection."""
    if not set(selected) <= set(CANDIDATE_BINARIES):
        raise EvidenceError("candidate_binary_selection_invalid")
    for key, path in selected.items():
        if path.resolve(strict=True) != Path(candidate.binaries[key]).resolve(strict=False):
            raise EvidenceError("candidate_" + key + "_mismatch")


def _require_producer_observations(source: dict[str, object], candidate: Candidate) -> None:
    if source.get("harness") != dict(candidate.harness):
        raise EvidenceError("supplemental_harness_mismatch")
    # The producer recorded the canonical aggregate it validated before its first dispatch.
    if source.get("canonical_aggregate_sha256") != candidate.aggregate_sha256:
        raise EvidenceError("supplemental_canonical_aggregate_mismatch")


def _task_aliases(source: dict[str, object], aliases: frozenset[str], excluded: set[str]) -> dict[str, str]:
    tasks = source.get("tasks")
    if (not isinstance(tasks, dict) or set(tasks) != aliases
            or any(not isinstance(task, str) or _TASK_ID.fullmatch(task) is None for task in tasks.values())
            or len(set(tasks.values())) != len(tasks)):
        raise EvidenceError("supplemental_tasks_invalid")
    if set(tasks.values()) & excluded:
        raise EvidenceError("supplemental_task_overlap")
    return dict(tasks)


def _continue_attempt(directory: Path, tasks: dict[str, str]) -> str:
    """Bind the successor to its retained public-continue admission record."""
    if (directory / DISPATCH_ATTEMPT_NAME).exists() or (directory / DISPATCH_ATTEMPT_NAME).is_symlink():
        raise EvidenceError("supplemental_dispatch_substituted")
    attempt, data = _read_json(directory / CONTINUE_ATTEMPT_NAME, MAX_RECEIPT_BYTES)
    if not isinstance(attempt, dict):
        raise EvidenceError("supplemental_continue_binding_invalid")
    response = attempt.get("response")
    if (not isinstance(response, dict) or attempt.get("task_id") != tasks["resume"]
            or response.get("command") != "continue" or response.get("admission") != "admitted"
            or response.get("task_id") != tasks["resume"]
            or response.get("parent_task_id") != tasks["fresh"]):
        raise EvidenceError("supplemental_continue_binding_invalid")
    return _digest_bytes(data)


def _validate_continue_case(root: Path, definition: CaseDefinition, candidate: Candidate,
                            excluded: set[str]) -> ContinueCase:
    """Judge one raw case with the canonical observation and proof owners, mutating nothing."""
    row = MatrixRow(definition, 0, "PASS")
    observation = _observe_case(row, root)
    if observation.error is not None:
        raise EvidenceError(observation.error)
    inventory = _tree_summary(observation.directory)
    if inventory.get("complete") is not True:
        raise EvidenceError("case_evidence_too_large")
    source = observation.source
    if not isinstance(source, dict) or source.get("continuation_command") != "continue":
        raise EvidenceError("supplemental_command_not_continue")
    scope = source.get("continuation_scope")
    if (not isinstance(scope, dict) or scope.get("tmpdir_scopes") != CONTINUATION_TMPDIR_SCOPES
            or scope.get("writable_roots") != "own-tmpdir" or scope.get("selection_inherited") is not True):
        raise EvidenceError("supplemental_scope_unproven")
    _require_producer_observations(source, candidate)
    tasks = _task_aliases(source, frozenset({"fresh", "resume"}), excluded)
    attempt_sha256 = _continue_attempt(observation.directory, tasks)
    compact = _sanitize_case(
        row=row, source=source, source_digest=observation.source_digest, cleanup=observation.cleanup,
        before=inventory, after=inventory, evidence_status="RESOLVED", evidence_error=None,
        source_proof=observation.proof, creation_identity=observation.creation_identity,
        terminal_state="published-before-cleanup", raw_deleted=False)
    reference = {key: compact[key] for key in (
        "case_id", "provider", "permission", "scenario", "status", "exit_code", "evidence_status")}
    reference["binary_identities"] = observation.proof["binary_identities"]
    proof = validate_case_proof(reference, compact)
    binaries = proof["binary_identities"]
    if not isinstance(binaries, dict) or not isinstance(binaries.get("paths"), dict):
        raise EvidenceError("supplemental_binary_mismatch")
    if proof["source_receipt_sha256"] != observation.source_digest:
        raise EvidenceError("supplemental_source_binding_invalid")
    if (binaries["task_ids"] != sorted(tasks.values())
            or binaries.get("paths") != dict(candidate.binaries)):
        raise EvidenceError("supplemental_binary_mismatch")
    excluded.update(tasks.values())
    return {"case_id": definition.case_id, "command": "continue", "tasks": tasks,
            "source_receipt_sha256": observation.source_digest, "continue_attempt_sha256": attempt_sha256,
            "source_proof": proof}


def _validate_pi_miss(source: dict[str, object], tasks: dict[str, str]) -> None:
    sessions = source.get("provider_sessions")
    miss = source.get("session_miss")
    if (not isinstance(sessions, dict) or set(sessions) != {"fresh", "resume"}
            or not isinstance(sessions["fresh"], str) or not sessions["fresh"]
            or sessions["fresh"] != sessions["resume"]):
        raise EvidenceError("pi_control_session_continuity_invalid")
    if (not isinstance(miss, dict) or miss.get("task_id") != tasks["session_miss"]
            or miss.get("publication") != "rejected" or miss.get("session_lookup") != "not-found"
            or miss.get("fresh_session_created") is not False or miss.get("provider_exit_code") != 1
            or source.get("session_directories") != PI_SESSION_DIRECTORIES):
        raise EvidenceError("pi_control_session_miss_invalid")
    closure = source.get("lifetime_closure")
    if (lifetime_closure_error(closure, task_ids=tasks.values()) is not None
            or source.get("natural_daemon_shutdown") is not True):
        raise EvidenceError("pi_control_lifetime_unproven")


def validate_pi_control(directory: Path, candidate: Candidate, excluded: set[str]) -> dict[str, object]:
    """Bind the Pi session-miss control receipt to the candidate; it stays outside the matrix."""
    source, source_data = _read_json(directory / "success.json", MAX_INPUT_BYTES)
    if (not isinstance(source, dict) or source.get("gate") != PI_SESSION_MISS_GATE
            or source.get("status") != "passed"
            or source.get("provider") != "pi:json" or source.get("mode") != "read-only"
            or source.get("scenario") != PI_SESSION_MISS_SCENARIO
            or source.get("continuation_command") != "continue"):
        raise EvidenceError("pi_control_identity_invalid")
    _require_producer_observations(source, candidate)
    tasks = _task_aliases(source, frozenset({"fresh", "resume", "session_miss"}), excluded)
    _validate_pi_miss(source, tasks)
    binding, binding_data = _read_json(directory / "binding.json", MAX_RECEIPT_BYTES)
    if (not isinstance(binding, dict) or binding.get("provider") != "pi:json"
            or binding.get("scenario") != PI_SESSION_MISS_SCENARIO
            or binding.get("installed_executables") != dict(candidate.binaries)):
        raise EvidenceError("pi_control_binary_mismatch")
    cleanup_ok, _, cleanup_error = _cleanup_observation(directory, "PASS")
    if not cleanup_ok:
        raise EvidenceError(cleanup_error or "pi_control_cleanup_unverified")
    excluded.update(tasks.values())
    return {"gate": PI_SESSION_MISS_GATE, "tasks": tasks, "source_receipt_sha256": _digest_bytes(source_data),
            "binding_receipt_sha256": _digest_bytes(binding_data)}


def validate_supplemental(*, supplemental_root: Path, canonical_aggregate: Path, source_revision: str | None,
                          candidate_delegate: Path, pi_control: Path, environment: Mapping[str, str],
                          checkout: Path) -> SupplementalSummary:
    """Validate every mandatory supplemental receipt; any refusal leaves all evidence unchanged."""
    harness = observe_continuation_harness(environment, checkout)
    revision = _parse_optional_revision(source_revision)
    if revision is None or revision != harness["harness_revision"]:
        raise EvidenceError("supplemental_source_revision_mismatch")
    candidate = candidate_from_aggregate(canonical_aggregate.absolute(), harness, candidate_delegate)
    root = supplemental_root.absolute()
    excluded = set(candidate.task_ids)
    cases = []
    for definition in supplemental_cases():
        try:
            cases.append(_validate_continue_case(root, definition, candidate, excluded))
        except EvidenceError as error:
            raise EvidenceError(f"{definition.case_id}: {error.args[0] if error.args else error}") from error
    control = validate_pi_control(pi_control.absolute(), candidate, excluded)
    return {
        "kind": "native-acceptance-supplemental-summary",
        "claim": "public-continue-cases-and-pi-session-miss-control",
        "source_revision": harness["harness_revision"], "harness_revision": harness["harness_revision"],
        "harness_sha256": harness["harness_sha256"],
        "canonical_aggregate_sha256": candidate.aggregate_sha256,
        "binaries": dict(candidate.binaries), "host_os": platform.system(),
        "cases": cases, "pi_session_miss": control, "credentials_in_receipt": False,
    }
