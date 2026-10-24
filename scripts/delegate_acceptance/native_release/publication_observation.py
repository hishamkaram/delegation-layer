"""Read-only observation of one dispatched publication run and its one report artifact.

Root's receipt owns every decision; this module only reads GitHub through the
canonical client and returns bound facts. Every refusal here happens after a
dispatch was intended, so each is incomplete: it never proves that the hosted
job did nothing, and nothing here dispatches, retries or writes.

The run is bound by ``bind_dispatched_run`` to the exact title, H, main,
``workflow_dispatch`` and the trusted ``release.yml`` path. Only attempt 1 is
observed: a rerun replaces the run's current attempt, so any other attempt
number refuses instead of substituting. The acknowledged run was once read
with only its title unbound and later read with the exact title, so a
nonterminal attempt 1 whose title alone is unbound is read again within the
same deadline. That read grants nothing: a completed run binds exactly, and a
title still unbound at the deadline refuses as unbound. The report is the artifact
``candidate-publication-{run_id}-{run_attempt}`` holding exactly one plain flat
``receipt.json``; its listed size and digest must equal the downloaded bytes,
and the archive is decoded in memory through EOF so each CRC is checked.
"""

from __future__ import annotations

import hashlib
import time

from scripts.delegate_acceptance.native_evidence.public_contract import PublicProofError, parse_json
from scripts.delegate_acceptance.release_verification import actions_artifacts
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.github import GitHubClient
from .contract import MAX_RECEIPT, ReleaseError, integer, object_value, text_value
from .publication_contract import (
    PUBLICATION_RUN_ATTEMPT, PUBLICATION_RUN_PATHS, PUBLICATION_WORKFLOW, PublicationDispatch,
)
from .submission import bind_dispatched_run


OBSERVED_ATTEMPT = PUBLICATION_RUN_ATTEMPT
RUN_PAGE_SIZE = 100
MAX_RUN_PAGES = 10
# One page of 100 run rows stays well below this; it bounds the reply only.
MAX_LISTING_BYTES = 8 << 20
MAX_RUN_BYTES = 1 << 20
REPORT_MEMBER = "receipt.json"
# The report is a canonical receipt within the shared receipt bound; deflate only shrinks it.
MAX_REPORT_ARCHIVE_BYTES = 2 * MAX_RECEIPT
POLL_SECONDS = 10
OBSERVE_TIMEOUT = 90 * 60
_CONCLUSION = r"[a-z_]{1,32}"
_UNBOUND = "publication_run_unbound"
# The shared artifact mechanisms' refusals under this report's own codes.
_ARTIFACT_CODES = {
    actions_artifacts.LISTING_INVALID: "publication_report_listing_invalid",
    actions_artifacts.LISTING_INCOMPLETE: "publication_report_listing_incomplete",
    actions_artifacts.ZIP_INVALID: "publication_report_zip_invalid",
    actions_artifacts.ZIP_INVENTORY_MISMATCH: "publication_report_zip_inventory_mismatch",
    actions_artifacts.ZIP_MEMBER_INVALID: "publication_report_zip_member_invalid",
    actions_artifacts.ZIP_MEMBER_TOO_LARGE: "publication_report_too_large",
}


def _refuse(code: str) -> ReleaseError:
    return ReleaseError(code, incomplete=True)


def report_artifact_name(run_id: int, run_attempt: int) -> str:
    return f"candidate-publication-{integer(run_id, minimum=1)}-{integer(run_attempt, minimum=1)}"


def _bind(value: object, dispatch: PublicationDispatch, expected_id: int | None) -> dict[str, object]:
    return bind_dispatched_run(value, repository=dispatch.request.repository, title=dispatch.title,
                               revision=dispatch.harness, paths=PUBLICATION_RUN_PATHS, expected_id=expected_id,
                               code=_UNBOUND)


def find_dispatched_run(client: GitHubClient, dispatch: PublicationDispatch) -> dict[str, object] | None:
    """The one run carrying this dispatch's title in the complete main dispatch inventory, or None.

    Every row with the exact title must bind to H and the trusted path; two
    such rows refuse explicitly. An inconsistent or unfinished listing refuses
    rather than reporting absence.
    """
    repository, title = dispatch.request.repository, dispatch.title
    matches: list[dict[str, object]] = []
    seen: set[int] = set()
    total: int | None = None
    for page in range(1, MAX_RUN_PAGES + 1):
        reply = object_value(client.json(
            f"repos/{repository}/actions/workflows/{PUBLICATION_WORKFLOW}/runs?branch=main"
            f"&event=workflow_dispatch&per_page={RUN_PAGE_SIZE}&page={page}", limit=MAX_LISTING_BYTES))
        count, listed = reply.get("total_count"), reply.get("workflow_runs")
        if (type(count) is not int or count < 0 or not isinstance(listed, list) or len(listed) > RUN_PAGE_SIZE
                or total is not None and count != total):
            raise _refuse("publication_run_listing_invalid")
        total = count
        for value in listed:
            row = object_value(value)
            identity = integer(row.get("id"), minimum=1)
            if identity in seen:
                raise _refuse("publication_run_listing_invalid")
            seen.add(identity)
            if row.get("display_title") == title:
                matches.append(row)
        if len(seen) == total:
            break
        if len(listed) < RUN_PAGE_SIZE or len(seen) > total:
            raise _refuse("publication_run_listing_incomplete")
    else:
        raise _refuse("publication_run_listing_incomplete")
    if len(matches) > 1:
        raise _refuse("publication_run_ambiguous")
    return _bind(matches[0], dispatch, None) if matches else None


def observe_run(client: GitHubClient, dispatch: PublicationDispatch, run_id: int, *,
                timeout: float = OBSERVE_TIMEOUT) -> dict[str, object]:
    """Wait, within ``timeout``, for attempt 1 of the bound run to complete; return its conclusion.

    A run that is not completed and whose title alone is unbound is read again
    once attempt 1 is checked; that unbound refusal is raised at the deadline.
    """
    deadline = time.monotonic() + timeout
    while True:
        run = object_value(client.json(f"repos/{dispatch.request.repository}/actions/runs/{run_id}",
                                       limit=MAX_RUN_BYTES))
        unbound: ReleaseError | None = None
        try:
            _bind(run, dispatch, run_id)
        except ReleaseError as error:
            if (error.code != _UNBOUND or error.mismatched != ("display_title",)
                    or run.get("status") == "completed"):
                raise
            unbound = error
        if integer(run.get("run_attempt"), minimum=1) != OBSERVED_ATTEMPT:
            raise _refuse("publication_run_attempt_substituted")
        if run.get("status") == "completed":
            return {"run_id": run_id, "run_attempt": OBSERVED_ATTEMPT,
                    "conclusion": text_value(run.get("conclusion"), _CONCLUSION)}
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise unbound if unbound is not None else _refuse("publication_observation_expired")
        time.sleep(min(POLL_SECONDS, remaining))


def read_report(client: GitHubClient, dispatch: PublicationDispatch, *, run_id: int,
                run_attempt: int) -> tuple[dict[str, object], dict[str, object]]:
    """The hosted gate receipt in the run attempt's one report artifact, and the artifact facts that bind it."""
    repository, name = dispatch.request.repository, report_artifact_name(run_id, run_attempt)
    try:
        rows = actions_artifacts.list_run_artifacts(client, repository, run_id, reply_limit=MAX_LISTING_BYTES)
    except VerificationError as error:
        code = actions_artifacts.refusal_code(error, _ARTIFACT_CODES)
        if code is None:
            raise
        raise _refuse(code) from error
    matches = [row for row in rows if row.get("name") == name]
    if len(matches) != 1:
        raise _refuse("publication_report_missing" if not matches else "publication_report_ambiguous")
    row = matches[0]
    workflow_run = object_value(row.get("workflow_run"))
    size, digest, expired = row.get("size_in_bytes"), row.get("digest"), row.get("expired")
    if (type(size) is not int or size <= 0 or type(expired) is not bool or not isinstance(digest, str)
            or not digest.startswith("sha256:")):
        raise _refuse("publication_report_artifact_invalid")
    archive_sha256 = text_value(digest[7:], r"[0-9a-f]{64}")
    if expired:
        raise _refuse("publication_report_expired")
    if size > MAX_REPORT_ARCHIVE_BYTES:
        raise _refuse("publication_report_too_large")
    if workflow_run.get("id") != run_id or workflow_run.get("head_sha") != dispatch.harness:
        raise _refuse("publication_report_artifact_mismatch")
    artifact_id = integer(row.get("id"), minimum=1)
    data = client.artifact_archive(repository, artifact_id, limit=MAX_REPORT_ARCHIVE_BYTES)
    if len(data) != size or hashlib.sha256(data).hexdigest() != archive_sha256:
        raise _refuse("publication_report_archive_mismatch")
    try:
        member = actions_artifacts.read_exact_zip(data, {REPORT_MEMBER: MAX_RECEIPT})[REPORT_MEMBER]
    except VerificationError as error:
        code = actions_artifacts.refusal_code(error, _ARTIFACT_CODES)
        if code is None:
            raise
        raise _refuse(code) from error
    try:
        gate = object_value(parse_json(member, "publication_report_json_invalid"))
    except PublicProofError as error:
        raise _refuse("publication_report_json_invalid") from error
    return gate, {"artifact_id": artifact_id, "artifact_name": name, "archive_sha256": archive_sha256,
                  "size": size, "receipt_sha256": hashlib.sha256(member).hexdigest()}
