"""Promotion policy over complete, scoped native-validation observations."""

from __future__ import annotations

from dataclasses import dataclass
import re


MAX_MATCHING_RUNS = 1000
REPORT_SCHEMA = 1
DIGEST = re.compile(r"[0-9a-f]{64}")


@dataclass(frozen=True)
class ValidationObservation:
    run_id: int
    attempt: int
    proof_sha256: str
    admission: str
    verdict: str


@dataclass(frozen=True)
class HistoryDecision:
    allowed: bool
    reason: str
    blocking_attempts: tuple[tuple[int, int], ...]


def proof_title(tag: str, invocation_id: str, proof_sha256: str) -> str:
    if (re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", tag) is None
            or re.fullmatch(r"[0-9a-f]{32}", invocation_id) is None
            or DIGEST.fullmatch(proof_sha256) is None):
        raise ValueError("invalid_native_validation_identity")
    return f"Native proof {tag} {invocation_id} {proof_sha256}"


def title_proof_sha(title: str, tag: str) -> str | None:
    match = re.fullmatch(
        r"Native proof " + re.escape(tag) + r" [0-9a-f]{32} ([0-9a-f]{64})", title,
    )
    return match.group(1) if match else None


def classify_native_history(
        observations: tuple[ValidationObservation, ...], *, complete: bool,
        selected_run_id: int) -> HistoryDecision:
    """Chronological observations: failure persists; later proof resolves uncertainty."""
    if not complete:
        return HistoryDecision(False, "native_history_incomplete", ())
    seen: set[tuple[int, int]] = set()
    resolved_proofs: set[str] = set()
    selected: ValidationObservation | None = None
    for item in observations:
        key = (item.run_id, item.attempt)
        if (key in seen or item.run_id <= 0 or item.attempt <= 0
                or DIGEST.fullmatch(item.proof_sha256) is None
                or item.admission not in {"admitted", "not_admitted", "unknown"}
                or item.verdict not in {"pass", "fail", "rejected", "incomplete"}
                or (item.admission == "not_admitted" and item.verdict != "rejected")):
            return HistoryDecision(False, "native_history_invalid", (key,))
        seen.add(key)
        if item.run_id == selected_run_id:
            selected = item
    blocking: list[tuple[int, int]] = []
    for item in reversed(observations):
        if ((item.admission == "admitted" and item.verdict != "pass")
                or (item.admission == "unknown" and item.proof_sha256 not in resolved_proofs)):
            blocking.append((item.run_id, item.attempt))
        if item.admission == "admitted" and item.verdict in {"pass", "fail"}:
            resolved_proofs.add(item.proof_sha256)
    blocked = tuple(reversed(blocking))
    if blocked:
        return HistoryDecision(False, "native_history_blocked", blocked)
    if selected is None or selected.admission != "admitted" or selected.verdict != "pass":
        return HistoryDecision(False, "native_validation_not_passing", ())
    return HistoryDecision(True, "native_history_pass", ())
