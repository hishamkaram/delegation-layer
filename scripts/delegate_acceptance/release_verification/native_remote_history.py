"""Read every scoped workflow attempt and its exact trusted report artifact."""

from __future__ import annotations

from dataclasses import dataclass
import io
import json
import re
import zipfile

from .contract import MAX_RECEIPT, VerificationError
from .github import GitHubClient, object_value, positive_id
from .native_history import (
    MAX_MATCHING_RUNS, ValidationObservation, title_proof_sha,
)
from .native_reports import ReportScope, report_observation


MAX_REPORT_ARCHIVE = MAX_RECEIPT * 2


@dataclass(frozen=True)
class AttemptReport:
    started_at: str
    observation: ValidationObservation
    report: dict[str, object] | None
    status: str
    conclusion: str | None


def _report_bytes(data: bytes) -> dict[str, object]:
    try:
        with zipfile.ZipFile(io.BytesIO(data)) as bundle:
            entries = bundle.infolist()
            if (len(entries) != 1 or entries[0].filename != "report.json"
                    or entries[0].file_size > MAX_RECEIPT or entries[0].flag_bits & 1):
                raise ValueError("invalid report inventory")
            value = json.loads(bundle.read(entries[0]))
        return object_value(value, "native-history-report")
    except (OSError, ValueError, KeyError, zipfile.BadZipFile, RecursionError) as error:
        raise VerificationError("native_report_invalid", "native-history-report") from error


def _read_report(client: GitHubClient, repo: str, run_id: int,
                 attempt: int) -> dict[str, object] | None:
    name = f"native-proof-validation-{run_id}-{attempt}"
    found: list[dict[str, object]] = []
    for page in range(1, 11):
        value = object_value(client.json(
            f"repos/{repo}/actions/runs/{run_id}/artifacts?per_page=100&page={page}",
            limit=1024 * 1024,
        ), "native-history-artifacts")
        rows = value.get("artifacts")
        if not isinstance(rows, list):
            raise VerificationError("native_artifact_list_invalid", "native-history")
        found.extend(object_value(row, "native-artifact") for row in rows
                     if isinstance(row, dict) and row.get("name") == name)
        if len(rows) < 100:
            break
    else:
        raise VerificationError("native_artifact_pagination_incomplete", "native-history")
    if len(found) != 1 or found[0].get("expired") is not False:
        return None
    artifact_id = positive_id(found[0].get("id"), "native-report-artifact")
    return _report_bytes(client.bytes(
        f"repos/{repo}/actions/artifacts/{artifact_id}/zip", limit=MAX_REPORT_ARCHIVE,
    ))


def matching_runs(client: GitHubClient, repo: str,
                  scope: ReportScope) -> tuple[dict[str, object], ...]:
    found: list[dict[str, object]] = []
    seen: set[int] = set()
    expected_total: int | None = None
    for page in range(1, 11):
        response = object_value(client.json(
            f"repos/{repo}/actions/workflows/release-native-acceptance.yml/runs"
            f"?event=workflow_dispatch&head_sha={scope.harness_revision}&branch=main"
            f"&per_page=100&page={page}", limit=1024 * 1024,
        ), "native-history-runs")
        total, rows = response.get("total_count"), response.get("workflow_runs")
        if (type(total) is not int or not 0 <= total <= MAX_MATCHING_RUNS
                or not isinstance(rows, list) or len(rows) > 100
                or expected_total is not None and total != expected_total):
            raise VerificationError("native_history_incomplete", "native-history")
        expected_total = total
        for row in rows:
            item = object_value(row, "native-history-run")
            run_id = positive_id(item.get("id"), "native-history-run")
            if run_id in seen:
                raise VerificationError("native_history_incomplete", "native-history")
            seen.add(run_id)
            if title_proof_sha(str(item.get("display_title", "")), scope.tag) is not None:
                found.append(item)
        if len(seen) == total:
            return tuple(found)
        if len(rows) < 100 or len(seen) > total:
            raise VerificationError("native_history_incomplete", "native-history")
    raise VerificationError("native_history_incomplete", "native-history")


def history_reports(client: GitHubClient, repo: str,
                    scope: ReportScope) -> tuple[AttemptReport, ...]:
    reports: list[AttemptReport] = []
    for run in matching_runs(client, repo, scope):
        run_id = positive_id(run.get("id"), "native-history-run")
        count = positive_id(run.get("run_attempt"), "native-history-attempt")
        if count > MAX_MATCHING_RUNS:
            raise VerificationError("native_history_incomplete", "native-history")
        proof_sha = title_proof_sha(str(run.get("display_title", "")), scope.tag)
        if proof_sha is None:
            raise VerificationError("native_history_invalid", "native-history")
        for attempt in range(1, count + 1):
            reports.append(_attempt_report(client, repo, scope, run_id=run_id,
                                           attempt=attempt, proof_sha=proof_sha))
    return tuple(sorted(reports, key=lambda row: (row.started_at, row.observation.run_id,
                                                 row.observation.attempt)))


def _attempt_report(client: GitHubClient, repo: str, scope: ReportScope, *,
                    run_id: int, attempt: int, proof_sha: str) -> AttemptReport:
    facts = object_value(client.json(
        f"repos/{repo}/actions/runs/{run_id}/attempts/{attempt}",
    ), "native-history-attempt")
    started = facts.get("run_started_at")
    if (facts.get("head_sha") != scope.harness_revision or facts.get("head_branch") != "main"
            or facts.get("event") != "workflow_dispatch"
            or facts.get("id") != run_id or facts.get("run_attempt") != attempt
            or not isinstance(started, str)
            or re.fullmatch(r"[0-9]{4}-[0-9T:.-]+Z", started) is None):
        raise VerificationError("native_history_attempt_unbound", "native-history")
    report = _read_report(client, repo, run_id, attempt)
    observation = (
        ValidationObservation(run_id, attempt, proof_sha, "unknown", "incomplete")
        if report is None else report_observation(
            report, scope, run_id=run_id, attempt=attempt, proof_sha256=proof_sha,
        )
    )
    status, conclusion = facts.get("status"), facts.get("conclusion")
    if not isinstance(status, str) or conclusion is not None and not isinstance(conclusion, str):
        raise VerificationError("native_history_attempt_invalid", "native-history")
    return AttemptReport(started, observation, report, status, conclusion)
