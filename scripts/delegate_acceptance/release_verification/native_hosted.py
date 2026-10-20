"""Hosted validation of maintainer-attested execution and exact promotion proofs."""

from __future__ import annotations

import argparse
import os
from pathlib import Path

from scripts.delegate_acceptance.native_evidence.public_contract import (
    MAX_PUBLIC_ZIP_BYTES, PublicProofError,
)
from scripts.delegate_acceptance.native_evidence.public_validation import (
    inspect_public_proof, validate_public_identity, validate_public_proof,
)
# GITHUB_AUTH_MODES is re-exported for the promotion CLI's accepted choices.
from .assets import GITHUB_AUTH_MODES, gh_environment
from .contract import MAX_RECEIPT, VerificationError
from .filesystem import _bounded_file_bytes, read_receipt, write_receipt
from .github import GitHubClient, object_value, positive_id
from .native_archive import archive_expectation
from .native_history import ValidationObservation, classify_native_history, proof_title
from .native_remote_history import history_reports
from .native_reports import ReportScope
from .native_submission import (
    authenticate_submission, repository_identity, require_public_repository,
)


def _base_report(args: argparse.Namespace, event: dict[str, object]) -> dict:
    repository = object_value(event.get("repository"), "validation-event")
    return {
        "schema_version": 1, "kind": "native-proof-validation",
        "repository_id": positive_id(repository.get("id"), "validation-event"),
        "tag": args.tag, "harness_revision": os.environ.get("GITHUB_SHA", ""),
        "run_id": int(os.environ.get("GITHUB_RUN_ID", "0")),
        "run_attempt": int(os.environ.get("GITHUB_RUN_ATTEMPT", "0")),
        "proof_sha256": args.proof_sha256, "proof_asset_id": args.asset_id,
        "invocation_id": args.invocation_id, "opencode_model": args.opencode_model,
        "ccr_model": args.ccr_model or None,
        "admission": "not_admitted", "verdict": "rejected", "stage": "authenticate",
        "claim": "maintainer-attested-native-execution-validated-by-ci",
        "failure": None,
    }


def validate_submission(args: argparse.Namespace) -> int:
    # The workflow writes an INCOMPLETE report before checkout/setup; retain it
    # even if argument or event validation fails before this entry point.
    report_path = Path(args.report).absolute()
    existing, _ = read_receipt(report_path, max_bytes=MAX_RECEIPT)
    report = existing
    try:
        import json
        event = object_value(json.loads(_bounded_file_bytes(
            Path(os.environ["GITHUB_EVENT_PATH"]), 1 << 20, "validation_event_invalid",
        )), "validation-event")
        report = _base_report(args, event)
        write_receipt(report_path, report, max_bytes=MAX_RECEIPT)
        client = GitHubClient({"commands": []}, gh_environment())
        _validate_submission(client, args, report, event, report_path)
    except (VerificationError, PublicProofError, ValueError, OSError, KeyError) as error:
        code = error.code if isinstance(error, VerificationError) else "native_proof_invalid"
        report["failure"] = {"code": code, "stage": report.get("stage", "validation")}
        if report.get("admission") == "admitted" and report.get("native_verdict") != "pass":
            report["verdict"] = "fail"
        write_receipt(report_path, report, max_bytes=MAX_RECEIPT)
        return 1
    write_receipt(report_path, report, max_bytes=MAX_RECEIPT)
    return 0


def _validate_submission(client: GitHubClient, args: argparse.Namespace, report: dict,
                         event: dict[str, object], report_path: Path) -> None:
    repo = os.environ["GITHUB_REPOSITORY"]
    sender = object_value(event.get("sender"), "validation-sender")
    title = proof_title(args.tag, args.invocation_id, args.proof_sha256)
    if os.environ.get("GITHUB_REF") != "refs/heads/main":
        raise VerificationError("native_validation_not_main", "validation-event")
    identity, data = authenticate_submission(
        client, repo, args.tag, asset_id=args.asset_id, proof_sha256=args.proof_sha256,
        harness_revision=report["harness_revision"],
        sender_id=positive_id(sender.get("id"), "validation-sender"),
        max_proof_bytes=MAX_PUBLIC_ZIP_BYTES,
    )
    report.update(admission="unknown", verdict="incomplete", stage="inspect-envelope")
    write_receipt(report_path, report, max_bytes=MAX_RECEIPT)
    envelope = inspect_public_proof(data)
    platform = object_value(envelope["platform"], "native-platform")
    expected_name = f"native-proof-{platform['os']}-{platform['arch']}-{args.invocation_id}.zip"
    if identity.asset_name != expected_name:
        report.update(admission="not_admitted", verdict="rejected")
        raise VerificationError("native_invocation_asset_mismatch", "validation-envelope")
    expectation = archive_expectation(
        client, identity, tag=args.tag, platform_os=str(platform["os"]),
        platform_arch=str(platform["arch"]), root=report_path.parent,
        opencode_model=args.opencode_model, ccr_model=args.ccr_model or None,
    )
    try:
        validate_public_identity(data, expectation)
    except PublicProofError:
        report.update(admission="not_admitted", verdict="rejected")
        raise
    report.update(admission="admitted", verdict="incomplete", stage="validate-native-facts",
                  repository_id=identity.repository_id, release_revision=identity.release_revision,
                  sender_id=identity.sender_id, run_title=title)
    write_receipt(report_path, report, max_bytes=MAX_RECEIPT)
    result = validate_public_proof(data, expectation)
    report["native_status"] = result.get("status")
    if result.get("status") != "PASS" or result.get("case_results_verdict") != "PASS":
        raise VerificationError("native_acceptance_not_passing", "validate-native-facts")
    report.update(verdict="pass", native_verdict="pass", stage="validate-history")
    write_receipt(report_path, report, max_bytes=MAX_RECEIPT)
    _enforce_history(client, repo, report)
    report["stage"] = "complete"


def _enforce_history(client: GitHubClient, repo: str, report: dict) -> None:
    scope = ReportScope(report["repository_id"], report["tag"], report["harness_revision"])
    prior = history_reports(client, repo, scope)
    run_id, attempt = report["run_id"], report["run_attempt"]
    current = ValidationObservation(run_id, attempt, report["proof_sha256"], "admitted", "pass")
    observations: list[ValidationObservation] = []
    matched = 0
    for row in prior:
        if (row.observation.run_id, row.observation.attempt) == (run_id, attempt):
            observations.append(current)
            matched += 1
        else:
            observations.append(row.observation)
    if matched != 1:
        raise VerificationError("native_history_current_attempt_unobserved", "validate-history")
    decision = classify_native_history(tuple(observations), complete=True, selected_run_id=run_id)
    report["history"] = {"allowed": decision.allowed, "reason": decision.reason,
                         "observed_attempts": len(observations)}
    if not decision.allowed:
        raise VerificationError(decision.reason, "validate-history")


def validate_promotion(args: argparse.Namespace) -> int:
    auth_mode = getattr(args, "github_auth", "environment")
    client = GitHubClient({"commands": []}, gh_environment(auth_mode=auth_mode))
    repo, harness_revision = args.repo, args.source_revision
    repository_id = repository_identity(client, repo, harness_revision)
    require_public_repository(client, repo)
    rows = history_reports(client, repo, ReportScope(repository_id, args.tag, harness_revision))
    decision = classify_native_history(tuple(row.observation for row in rows), complete=True,
                                       selected_run_id=args.native_validation_run_id)
    if not decision.allowed:
        raise VerificationError(decision.reason, "promotion-history")
    selected = [row for row in rows if row.observation.run_id == args.native_validation_run_id]
    if not selected or selected[-1].report is None:
        raise VerificationError("native_validation_report_missing", "promotion-proof")
    report = selected[-1].report
    if (selected[-1].status != "completed" or selected[-1].conclusion != "success"
            or report.get("stage") != "complete"):
        raise VerificationError("native_validation_run_not_successful", "promotion-proof")
    _revalidate_selected(client, args, report)
    return 0


def _revalidate_selected(client: GitHubClient, args: argparse.Namespace, report: dict) -> None:
    asset_id = positive_id(report.get("proof_asset_id"), "promotion-proof")
    identity, data = authenticate_submission(
        client, args.repo, args.tag, asset_id=asset_id,
        proof_sha256=str(report["proof_sha256"]), harness_revision=args.source_revision,
        sender_id=positive_id(report.get("sender_id"), "promotion-proof"),
        max_proof_bytes=MAX_PUBLIC_ZIP_BYTES,
    )
    envelope = inspect_public_proof(data)
    platform = object_value(envelope["platform"], "promotion-platform")
    expectation = archive_expectation(
        client, identity, tag=args.tag, platform_os=str(platform["os"]),
        platform_arch=str(platform["arch"]), root=Path(args.work_dir),
        opencode_model=str(report["opencode_model"]),
        ccr_model=report.get("ccr_model") if isinstance(report.get("ccr_model"), str) else None,
    )
    result = validate_public_proof(data, expectation)
    if result.get("status") != "PASS" or result.get("case_results_verdict") != "PASS":
        raise VerificationError("native_promotion_proof_not_passing", "promotion-proof")
