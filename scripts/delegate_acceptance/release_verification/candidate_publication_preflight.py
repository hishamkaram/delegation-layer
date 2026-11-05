"""Require the selected distribution's latest CI producer and install jobs on main.

GitHub owns run/job history. Read all pages with filter=all, then select the
latest execution of each required logical job. A carried successful job is
valid; an older artifact than the latest producer is not. No store or approval
record participates in this decision.
"""

from __future__ import annotations

from .actions_artifacts import MAX_PAGES, PAGE_SIZE
from .candidate_artifact import CandidateArtifact
from .candidate_manifest import CANDIDATE_WORKFLOW_PATH
from .contract import VerificationError
from .github import GitHubClient, object_value, positive_id


STAGE = "publication-preflight"
PRODUCER_JOB = "Distribution build"
INSTALL_JOBS = tuple(f"Distribution install ({host}, {channel})"
                     for host in ("ubuntu-24.04", "macos-15")
                     for channel in ("shell", "npm", "homebrew"))
CHECK_JOBS = ("Gate (Linux ubuntu-24.04)", "Gate (macOS macos-15)",
              "Agent skill package (ubuntu-24.04)", "Agent skill package (macos-15)")
REQUIRED_JOBS = (PRODUCER_JOB, *INSTALL_JOBS, *CHECK_JOBS)
MAX_REPLY_BYTES = 4 * 1024 * 1024


def _refuse(code: str) -> VerificationError:
    return VerificationError(code, STAGE)


def _run(client: GitHubClient, artifact: CandidateArtifact) -> int:
    locator = artifact.locator
    record = object_value(client.json(
        f"repos/{locator.repository}/actions/runs/{locator.run_id}", limit=MAX_REPLY_BYTES), STAGE)
    owner = object_value(record.get("repository"), STAGE)
    head_owner = object_value(record.get("head_repository"), STAGE)
    if (record.get("id") != locator.run_id or record.get("workflow_id") != artifact.workflow.id
            or record.get("path") not in (CANDIDATE_WORKFLOW_PATH, CANDIDATE_WORKFLOW_PATH + "@main",
                                           CANDIDATE_WORKFLOW_PATH + "@refs/heads/main")
            or owner.get("id") != artifact.repository_id or owner.get("full_name") != locator.repository
            or head_owner.get("id") != artifact.repository_id
            or head_owner.get("full_name") != locator.repository
            or record.get("head_branch") != "main" or record.get("head_sha") != artifact.manifest.source.commit
            or record.get("event") not in ("push", "workflow_dispatch")):
        raise _refuse("distribution_source_untrusted")
    attempt = positive_id(record.get("run_attempt"), STAGE)
    if (record.get("status"), record.get("conclusion")) != ("completed", "success"):
        raise _refuse("distribution_ci_not_successful")
    return attempt


def _jobs(client: GitHubClient, repository: str, run_id: int) -> list[dict[str, object]]:
    rows: list[dict[str, object]] = []
    seen: set[int] = set()
    total: int | None = None
    for page in range(1, MAX_PAGES + 1):
        endpoint = (f"repos/{repository}/actions/runs/{run_id}/jobs"
                    f"?filter=all&per_page={PAGE_SIZE}&page={page}")
        reply = object_value(client.json(endpoint, limit=MAX_REPLY_BYTES), STAGE)
        count, listed = reply.get("total_count"), reply.get("jobs")
        if (type(count) is not int or not 0 <= count <= PAGE_SIZE * MAX_PAGES
                or not isinstance(listed, list) or len(listed) > PAGE_SIZE
                or total is not None and count != total):
            raise _refuse("distribution_jobs_invalid")
        total = count
        for value in listed:
            row = object_value(value, STAGE)
            identity = positive_id(row.get("id"), STAGE)
            if identity in seen:
                raise _refuse("distribution_jobs_invalid")
            seen.add(identity)
            rows.append(row)
        if len(rows) == total:
            return rows
        if len(listed) < PAGE_SIZE or len(rows) > total:
            raise _refuse("distribution_jobs_incomplete")
    raise _refuse("distribution_jobs_incomplete")


def latest_jobs(rows: list[dict[str, object]], *, run_id: int,
                head_sha: str, latest_attempt: int) -> dict[str, dict[str, object]]:
    """Pure selection over complete history; duplicate executions in one attempt are unproven."""
    selected: dict[str, dict[str, object]] = {}
    ambiguous: set[str] = set()
    for row in rows:
        name = row.get("name")
        if not isinstance(name, str) or name not in REQUIRED_JOBS:
            continue
        attempt = positive_id(row.get("run_attempt"), STAGE)
        if row.get("run_id") != run_id or row.get("head_sha") != head_sha or attempt > latest_attempt:
            raise _refuse("distribution_job_source_mismatch")
        previous = selected.get(name)
        previous_attempt = positive_id(previous.get("run_attempt"), STAGE) if previous is not None else None
        if previous_attempt == attempt:
            ambiguous.add(name)
        if previous_attempt is None or attempt > previous_attempt:
            selected[name] = row
            ambiguous.discard(name)
    if set(selected) != set(REQUIRED_JOBS):
        raise _refuse("distribution_job_missing")
    if ambiguous:
        raise _refuse("distribution_job_ambiguous")
    if any((row.get("status"), row.get("conclusion")) != ("completed", "success")
           for row in selected.values()):
        raise _refuse("distribution_job_not_successful")
    return selected


def require_publishable(client: GitHubClient, artifact: CandidateArtifact) -> None:
    """Check the actual source/run, all required jobs and the artifact's producer chain."""
    locator = artifact.locator
    identity = object_value(client.json(f"repos/{locator.repository}"), STAGE)
    if (identity.get("id") != artifact.repository_id or identity.get("full_name") != locator.repository
            or identity.get("private") is not False):
        raise _refuse("publication_repository_not_public")
    run = _run(client, artifact)
    jobs = latest_jobs(_jobs(client, locator.repository, locator.run_id), run_id=locator.run_id,
                       head_sha=artifact.manifest.source.commit, latest_attempt=run)
    producer = positive_id(jobs[PRODUCER_JOB].get("run_attempt"), STAGE)
    if producer != locator.run_attempt:
        raise _refuse("distribution_artifact_superseded")
    if any(positive_id(jobs[name].get("run_attempt"), STAGE) < producer for name in INSTALL_JOBS):
        raise _refuse("distribution_install_predates_producer")
    commit = object_value(client.json(
        f"repos/{locator.repository}/commits/{artifact.manifest.source.commit}"), STAGE)
    tree = object_value(object_value(commit.get("commit"), STAGE).get("tree"), STAGE)
    if commit.get("sha") != artifact.manifest.source.commit or tree.get("sha") != artifact.manifest.source.tree:
        raise _refuse("distribution_source_mismatch")
    main = object_value(client.json(f"repos/{locator.repository}/branches/main"), STAGE)
    head = object_value(main.get("commit"), STAGE).get("sha")
    if head != artifact.manifest.source.commit:
        comparison = object_value(client.json(
            f"repos/{locator.repository}/compare/{artifact.manifest.source.commit}...{head}"), STAGE)
        ancestor = object_value(comparison.get("merge_base_commit"), STAGE)
        if comparison.get("status") != "ahead" or ancestor.get("sha") != artifact.manifest.source.commit:
            raise _refuse("distribution_source_not_on_main")
    if _run(client, artifact) != run:
        raise _refuse("distribution_ci_changed")
