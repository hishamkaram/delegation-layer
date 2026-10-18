"""Typed interpretation of reports emitted by the trusted hosted harness."""

from __future__ import annotations

from dataclasses import dataclass

from .native_history import DIGEST, REPORT_SCHEMA, ValidationObservation


@dataclass(frozen=True)
class ReportScope:
    repository_id: int
    tag: str
    harness_revision: str


def report_observation(
        report: dict[str, object], scope: ReportScope, *, run_id: int,
        attempt: int, proof_sha256: str) -> ValidationObservation:
    if (type(report.get("schema_version")) is not int
            or report.get("schema_version") != REPORT_SCHEMA
            or report.get("kind") != "native-proof-validation"
            or report.get("repository_id") != scope.repository_id
            or report.get("tag") != scope.tag
            or report.get("harness_revision") != scope.harness_revision
            or report.get("run_id") != run_id or report.get("run_attempt") != attempt
            or report.get("proof_sha256") != proof_sha256
            or DIGEST.fullmatch(proof_sha256) is None):
        return ValidationObservation(run_id, attempt, proof_sha256, "unknown", "incomplete")
    admission, verdict = report.get("admission"), report.get("verdict")
    allowed = {
        ("not_admitted", "rejected"), ("unknown", "incomplete"),
        ("admitted", "pass"), ("admitted", "fail"), ("admitted", "incomplete"),
    }
    if (not isinstance(admission, str) or not isinstance(verdict, str)
            or (admission, verdict) not in allowed):
        return ValidationObservation(run_id, attempt, proof_sha256, "unknown", "incomplete")
    return ValidationObservation(run_id, attempt, proof_sha256, admission, verdict)
