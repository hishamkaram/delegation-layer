"""Finite foreground observation of the exact hosted run and trusted report."""

from __future__ import annotations

from dataclasses import asdict
import time

from scripts.delegate_acceptance.native_evidence.public_contract import canonical_json, sha256
from scripts.delegate_acceptance.release_verification.github import GitHubClient
from scripts.delegate_acceptance.release_verification.native_history import classify_native_history
from scripts.delegate_acceptance.release_verification.native_remote_history import history_reports
from scripts.delegate_acceptance.release_verification.native_reports import ReportScope
from .contract import ReleaseError, integer, object_value, text_value
from .receipt import Receipt
from .submission import validate_run


def _terminal_report(receipt: Receipt, client: GitHubClient,
                     run: dict[str, object]) -> dict[str, object]:
    preflight = receipt.stage("preflight").result
    run_id = integer(run.get("id"), minimum=1)
    attempt = integer(run.get("run_attempt"), minimum=1)
    scope = ReportScope(integer(preflight.get("repository_id"), minimum=1),
                        receipt.invocation.request.tag, text_value(preflight.get("harness_revision")))
    history = history_reports(client, receipt.invocation.request.repo, scope)
    selected = [row for row in history if row.observation.run_id == run_id
                and row.observation.attempt == attempt]
    if len(selected) != 1 or selected[0].report is None:
        raise ReleaseError("completed_native_validation_report_missing")
    report = object_value(selected[0].report)
    inputs = object_value(receipt.stage("dispatch").intent.get("inputs"))
    if (str(report.get("proof_asset_id")) != inputs.get("proof_asset_id")
            or report.get("invocation_id") != inputs.get("invocation_id")
            or report.get("opencode_model") != inputs.get("opencode_model")
            or (report.get("ccr_model") or "") != inputs.get("ccr_model")
            or report.get("proof_sha256") != inputs.get("proof_sha256")
            or report.get("admission") == "admitted"
            and report.get("sender_id") != preflight.get("actor_id")):
        raise ReleaseError("native_validation_report_asset_mismatch")
    decision = classify_native_history(tuple(row.observation for row in history),
                                       complete=True, selected_run_id=run_id)
    promotable = (run.get("conclusion") == "success" and report.get("stage") == "complete"
                  and selected[0].observation.admission == "admitted"
                  and selected[0].observation.verdict == "pass" and decision.allowed)
    return {"run_id": run_id, "run_attempt": attempt, "conclusion": run.get("conclusion"),
            "report_sha256": sha256(canonical_json(report)),
            "observation": asdict(selected[0].observation), "promotable": promotable,
            "history_reason": decision.reason}


def watch(receipt: Receipt, client: GitHubClient, *, timeout: float = 40 * 60) -> None:
    run_id = integer(receipt.stage("dispatch").result.get("run_id"), minimum=1)
    receipt.intend("watch", {"run_id": run_id})
    deadline = time.monotonic() + timeout
    while True:
        run = validate_run(receipt, client.json(
            f"repos/{receipt.invocation.request.repo}/actions/runs/{run_id}",
        ), expected_id=run_id)
        if run.get("status") == "completed":
            receipt.complete("watch", _terminal_report(receipt, client, run))
            return
        if time.monotonic() >= deadline:
            raise ReleaseError("native_validation_watch_expired", incomplete=True)
        time.sleep(min(5, max(0, deadline - time.monotonic())))
