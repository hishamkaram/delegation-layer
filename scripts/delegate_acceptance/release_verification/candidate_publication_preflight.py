"""One shared preflight of a candidate publication: validated observations, never publication authority.

Root and hosted delivery call ``preflight`` once, before any mutation. In order:

1. The client, the typed ``PublicationRequest`` and the required store choice
   are checked before any read.
2. The configured repository and the canonical development-readiness workflow
   descriptor are read. The repository's privacy must be an exact bool; it is
   only carried, since readiness may run privately and only the actual
   publisher requires a public repository. The exact requested run attempt (never the latest) must
   be that workflow's ``workflow_dispatch`` on ``main`` in this repository, and
   ``completed``/``success`` before its artifact or receipt is used: the upload
   step runs ``always()``, so a failed attempt can still upload a ready-looking
   receipt. The adapter exits 1 when its final receipt write fails and the step
   does not continue on error, so a successful attempt with a ready receipt
   proves that write. ``receipt_finalized`` is only printed, never persisted.
3. The run's artifact listing is enumerated completely; exactly one
   ``development-readiness-{run}-{attempt}`` must exist, unexpired, bound to the
   run, its head and this repository, with a positive bounded compressed size
   and a ``sha256:`` digest. The artifact has no attempt field: the producer's
   name and the attempt endpoint bind the attempt. One download must have
   exactly that length and digest.
4. The archive is decoded in memory: exactly the producer's three flat members
   (``receipt.json`` and both status request bodies), each a plain STORED or
   DEFLATED file within its bound, read through EOF so the CRC is checked.
5. The receipt must be a ready, successful observation of this attempt whose
   harness is the run's committed main commit (not candidate B), whose request
   is this candidate, F and store, and whose two acknowledged statuses on B
   match the archived request bodies. N is read only after these bindings.
6. The store is read fresh: strictly through ``load_candidate`` when Root
   verifies protection (no fallback), otherwise at the branch head with
   protection unverified. Its F, publication, selected review, B/T and scope
   must equal the receipt's; its head may have moved. Canonical readiness of
   B/T is evaluated again and must be ready.

The result is a set of observations. Merge observation, delivery and every
mutation belong to later owners; nothing here authorizes publication.
"""

from __future__ import annotations

from dataclasses import dataclass
import hashlib

from scripts.delegate_acceptance.native_evidence.public_contract import PublicProofError, parse_json
from scripts.delegate_acceptance.native_release.contract import PublicationRequest, trusted_main_workflow_paths
from . import actions_artifacts
from .candidate_attempts import ReviewedSource, scope_record
from .candidate_history import FindingRef
from .candidate_readiness import SourceObservation, evaluate_readiness
from .candidate_readiness_check import (
    KIND, MAX_RECEIPT_BYTES, RUN_FIELDS, STATUS_CONTEXT, WORKFLOW_PATH, RunContext, Target, run_context,
    selected_review,
)
from .candidate_store import load_candidate, load_candidate_at_branch_head
from .candidate_store_contract import GIT_OBJECT_RE
from .candidate_store_replay import CandidateSnapshot
from .contract import HEX_SHA256, VerificationError
from .github import GitHubClient


STAGE = "publication-preflight"


WORKFLOW_FILE = WORKFLOW_PATH.rsplit("/", 1)[1]


# A run reports its workflow path bare or main-qualified; the descriptor reports only ``WORKFLOW_PATH``.
ATTEMPT_PATHS = trusted_main_workflow_paths(WORKFLOW_FILE)


# A ready receipt is at most 1 MiB and each status body a few dozen bytes; deflate only shrinks them.
MAX_ARCHIVE_BYTES = 2 * 1024 * 1024


MAX_STATUS_BODY_BYTES = 4 * 1024


RECEIPT_MEMBER = "receipt.json"


# The producer's writes in its one uploaded directory, keyed to their bounds.
MEMBER_LIMITS = {RECEIPT_MEMBER: MAX_RECEIPT_BYTES, "status-pending-body.json": MAX_STATUS_BODY_BYTES,
                 "status-terminal-body.json": MAX_STATUS_BODY_BYTES}


# Each receipt status record, the state it must carry and its archived request body.
STATUSES = (("pending", "pending", "status-pending-body.json"),
            ("terminal", "success", "status-terminal-body.json"))


MAX_REPLY_BYTES = 1024 * 1024


# The shared artifact mechanisms' refusals under this preflight's own readiness codes.
_ARTIFACT_CODES = {
    actions_artifacts.LISTING_INVALID: "readiness_artifact_listing_invalid",
    actions_artifacts.LISTING_INCOMPLETE: "readiness_artifact_listing_incomplete",
    actions_artifacts.ZIP_INVALID: "readiness_zip_invalid",
    actions_artifacts.ZIP_INVENTORY_MISMATCH: "readiness_zip_inventory_mismatch",
    actions_artifacts.ZIP_MEMBER_INVALID: "readiness_zip_member_invalid",
    actions_artifacts.ZIP_MEMBER_TOO_LARGE: "readiness_zip_member_too_large",
}


class PublicationNotReady(VerificationError):
    """Fresh canonical readiness refused the frozen candidate; ``findings`` are its exact findings."""

    def __init__(self, findings: tuple[FindingRef, ...]):
        super().__init__("publication_readiness_denied", STAGE)
        self.findings = findings


@dataclass(frozen=True)
class ReadinessArtifact:
    """The selected attempt's one readiness artifact and the archive digest its metadata and bytes share."""

    id: int
    sha256: str


@dataclass(frozen=True)
class PublicationContext:
    """What the preflight observed for one publication request; it grants nothing.

    ``pull_request`` is N and ``target`` is B/T from the validated receipt, both
    equal to the fresh candidate's source. The readiness run and attempt stay in
    ``request``. The harness is the trusted main commit of that run, never B.
    ``protection_unverified`` is True unless the strict store reader verified
    the store branch protection. ``repository_private`` is the repository's
    exact observed privacy; it authorizes nothing here.
    """

    request: PublicationRequest
    snapshot: CandidateSnapshot
    review: ReviewedSource
    pull_request: int
    target: Target
    artifact: ReadinessArtifact
    harness_commit: str
    harness_sha256: str
    protection_unverified: bool
    repository_private: bool


@dataclass(frozen=True)
class _Attempt:
    """The selected attempt's repository identity and privacy, and its immutable head (the trusted main harness)."""

    repository_id: int
    repository_private: bool
    head_sha: str


@dataclass(frozen=True)
class _ReadyReceipt:
    """The receipt facts the fresh store must match, after every binding to the attempt held."""

    pull_request: int
    target: Target
    harness_sha256: str
    publication_sha256: str
    review_sha256: str
    scope: object
    candidate_source: object


def _refuse(code: str) -> VerificationError:
    return VerificationError(code, STAGE)


def _record(value: object, code: str) -> dict[str, object]:
    if not isinstance(value, dict) or any(not isinstance(key, str) for key in value):
        raise _refuse(code)
    return value


def _exact_int(value: object, expected: int) -> bool:
    return type(value) is int and value == expected


def _positive_int(value: object) -> bool:
    return type(value) is int and value > 0


def _get(client: GitHubClient, endpoint: str, code: str) -> dict[str, object]:
    return _record(client.json(endpoint, limit=MAX_REPLY_BYTES), code)


def preflight(client: GitHubClient, request: PublicationRequest, *,
              verify_store_protection: bool) -> PublicationContext:
    """Validate the selected readiness attempt, its artifact and receipt, then the fresh store and readiness."""
    if (not isinstance(client, GitHubClient) or not isinstance(request, PublicationRequest)
            or type(verify_store_protection) is not bool):
        raise _refuse("publication_preflight_request_invalid")
    attempt = _observe_attempt(client, request)
    artifact, size = _select_artifact(client, request, attempt)
    members = _archive_members(_download(client, request.repository, artifact, size))
    ready = _bind_receipt(members, request, attempt)
    snapshot = _fresh_snapshot(client, request, verify_store_protection=verify_store_protection)
    review = _bind_fresh(snapshot, ready, request)
    return PublicationContext(request=request, snapshot=snapshot, review=review,
                              pull_request=ready.pull_request, target=ready.target, artifact=artifact,
                              harness_commit=attempt.head_sha, harness_sha256=ready.harness_sha256,
                              protection_unverified=not verify_store_protection,
                              repository_private=attempt.repository_private)


def _observe_attempt(client: GitHubClient, request: PublicationRequest) -> _Attempt:
    """The exact requested attempt of the canonical workflow on main; only a successful one is used."""
    repository = request.repository
    identity = _get(client, f"repos/{repository}", "readiness_repository_mismatch")
    repository_id = identity.get("id")
    if not _positive_int(repository_id) or identity.get("full_name") != repository:
        raise _refuse("readiness_repository_mismatch")
    private = identity.get("private")
    if type(private) is not bool:
        raise _refuse("repository_visibility_unknown")
    workflow = _get(client, f"repos/{repository}/actions/workflows/{WORKFLOW_FILE}", "readiness_workflow_mismatch")
    workflow_id = workflow.get("id")
    if not _positive_int(workflow_id) or workflow.get("path") != WORKFLOW_PATH:
        raise _refuse("readiness_workflow_mismatch")
    record = _get(client, f"repos/{repository}/actions/runs/{request.readiness_run_id}"
                          f"/attempts/{request.readiness_run_attempt}", "readiness_attempt_mismatch")
    owner = _record(record.get("repository"), "readiness_attempt_mismatch")
    head_owner = _record(record.get("head_repository"), "readiness_attempt_mismatch")
    head = record.get("head_sha")
    if not (_exact_int(record.get("id"), request.readiness_run_id)
            and _exact_int(record.get("run_attempt"), request.readiness_run_attempt)
            and _exact_int(record.get("workflow_id"), workflow_id) and record.get("path") in ATTEMPT_PATHS
            and record.get("head_branch") == "main" and record.get("event") == "workflow_dispatch"
            and _exact_int(owner.get("id"), repository_id) and owner.get("full_name") == repository
            and _exact_int(head_owner.get("id"), repository_id)
            and isinstance(head, str) and GIT_OBJECT_RE.fullmatch(head) is not None):
        raise _refuse("readiness_attempt_mismatch")
    if (record.get("status"), record.get("conclusion")) != ("completed", "success"):
        raise _refuse("readiness_attempt_not_successful")
    return _Attempt(repository_id=repository_id, repository_private=private, head_sha=head)


def _listed_artifacts(client: GitHubClient, repository: str, run_id: int) -> list[dict[str, object]]:
    """Every artifact row of the run; an inconsistent, duplicated or unfinished listing refuses."""
    try:
        return actions_artifacts.list_run_artifacts(client, repository, run_id, reply_limit=MAX_REPLY_BYTES)
    except VerificationError as error:
        code = actions_artifacts.refusal_code(error, _ARTIFACT_CODES)
        if code is None:
            raise
        raise _refuse(code) from error


def _select_artifact(client: GitHubClient, request: PublicationRequest,
                     attempt: _Attempt) -> tuple[ReadinessArtifact, int]:
    """The one artifact the producer names for this attempt, with its compressed size."""
    name = f"development-readiness-{request.readiness_run_id}-{request.readiness_run_attempt}"
    matches = [row for row in _listed_artifacts(client, request.repository, request.readiness_run_id)
               if row.get("name") == name]
    if len(matches) != 1:
        raise _refuse("readiness_artifact_missing" if not matches else "readiness_artifact_ambiguous")
    row = matches[0]
    run = _record(row.get("workflow_run"), "readiness_artifact_invalid")
    size, digest, expired = row.get("size_in_bytes"), row.get("digest"), row.get("expired")
    if (not _positive_int(size) or type(expired) is not bool or not isinstance(digest, str)
            or not digest.startswith("sha256:") or HEX_SHA256.fullmatch(digest[7:]) is None):
        raise _refuse("readiness_artifact_invalid")
    if expired:
        raise _refuse("readiness_artifact_expired")
    if size > MAX_ARCHIVE_BYTES:
        raise _refuse("readiness_artifact_too_large")
    if not (_exact_int(run.get("id"), request.readiness_run_id) and run.get("head_sha") == attempt.head_sha
            and _exact_int(run.get("repository_id"), attempt.repository_id)
            and _exact_int(run.get("head_repository_id"), attempt.repository_id)):
        raise _refuse("readiness_artifact_mismatch")
    return ReadinessArtifact(id=row["id"], sha256=digest[7:]), size


def _download(client: GitHubClient, repository: str, artifact: ReadinessArtifact, size: int) -> bytes:
    """One download; its length and SHA-256 must be the listed compressed size and digest."""
    data = client.artifact_archive(repository, artifact.id, limit=MAX_ARCHIVE_BYTES)
    if len(data) != size or hashlib.sha256(data).hexdigest() != artifact.sha256:
        raise _refuse("readiness_archive_mismatch")
    return data


def _archive_members(data: bytes) -> dict[str, bytes]:
    """Exactly the producer's three members, each read through EOF within its bound."""
    try:
        return actions_artifacts.read_exact_zip(data, MEMBER_LIMITS)
    except VerificationError as error:
        code = actions_artifacts.refusal_code(error, _ARTIFACT_CODES)
        if code is None:
            raise
        raise _refuse(code) from error


def _json_object(data: bytes) -> dict[str, object]:
    """The canonical strict parse; its own reason names the refusal and no member content is kept."""
    try:
        value = parse_json(data, "readiness_member_json_invalid")
    except PublicProofError as error:
        raise _refuse(str(error.args[0])) from error
    return _record(value, "readiness_member_json_invalid")


def _bind_receipt(members: dict[str, bytes], request: PublicationRequest, attempt: _Attempt) -> _ReadyReceipt:
    """Bind the ready receipt to the attempt, the request and its archived status bodies; N comes last."""
    receipt = _json_object(members[RECEIPT_MEMBER])
    readiness = _record(receipt.get("readiness"), "readiness_receipt_invalid")
    if (not _exact_int(receipt.get("schema_version"), 1) or receipt.get("kind") != KIND
            or receipt.get("result") != "ready" or not _exact_int(receipt.get("exit_code"), 0)
            or receipt.get("decision") != {"state": "success", "reason": "ready"}
            or readiness.get("ready") is not True or readiness.get("findings") != []
            or receipt.get("status_context") != STATUS_CONTEXT or receipt.get("protection_unverified") is not True):
        raise _refuse("readiness_receipt_not_ready")
    run = _bind_run(receipt, request, attempt)
    harness = _record(receipt.get("harness"), "readiness_receipt_invalid")
    fingerprint = harness.get("sha256")
    if (harness.get("committed") is not True or harness.get("commit") != attempt.head_sha
            or not isinstance(fingerprint, str) or HEX_SHA256.fullmatch(fingerprint) is None):
        raise _refuse("readiness_harness_mismatch")
    store = _bind_request(receipt, request)
    target = _bind_target(receipt)
    _bind_statuses(receipt, members, run, target)
    pull_request = _record(receipt.get("pull_request"), "readiness_receipt_invalid").get("number")
    requested = _record(receipt.get("request"), "readiness_receipt_invalid").get("pull_request")
    if not _positive_int(pull_request) or not _exact_int(requested, pull_request):
        raise _refuse("readiness_pull_request_mismatch")
    publication, review = store.get("publication_sha256"), receipt.get("selected_review_sha256")
    if (not isinstance(publication, str) or HEX_SHA256.fullmatch(publication) is None
            or not isinstance(review, str) or readiness.get("review_sha256") != review):
        raise _refuse("readiness_receipt_invalid")
    return _ReadyReceipt(pull_request=pull_request, target=target, harness_sha256=fingerprint,
                         publication_sha256=publication, review_sha256=review, scope=receipt.get("scope"),
                         candidate_source=store.get("candidate_source"))


def _bind_run(receipt: dict[str, object], request: PublicationRequest, attempt: _Attempt) -> RunContext:
    """The producer's own run-context policy over the recorded run, then its identity with this attempt."""
    run = _record(receipt.get("run"), "readiness_receipt_invalid")
    if set(run) != set(RUN_FIELDS) or any(not isinstance(value, str) for value in run.values()):
        raise _refuse("readiness_run_mismatch")
    checks, context = run_context(run, request.repository)
    if (context is None or not all(checks.values()) or run["GITHUB_RUN_ID"] != str(request.readiness_run_id)
            or run["GITHUB_RUN_ATTEMPT"] != str(request.readiness_run_attempt)
            or context.harness_commit != attempt.head_sha):
        raise _refuse("readiness_run_mismatch")
    return context


def _bind_request(receipt: dict[str, object], request: PublicationRequest) -> dict[str, object]:
    """The recorded request and store facts name this candidate, F and every store pin; return the store facts."""
    recorded = _record(receipt.get("request"), "readiness_receipt_invalid")
    store = _record(receipt.get("store"), "readiness_receipt_invalid")
    location, reference = request.store, request.reference
    pins = {"repository": location.repository, "branch": location.branch,
            "genesis_commit": location.genesis_commit, "marker_blob": location.marker_blob}
    if (recorded.get("candidate_id") != reference.candidate_id
            or recorded.get("freeze_commit") != reference.freeze_commit or recorded.get("store") != pins
            or store.get("candidate_id") != reference.candidate_id
            or store.get("freeze_commit") != reference.freeze_commit):
        raise _refuse("readiness_request_mismatch")
    return store


def _bind_target(receipt: dict[str, object]) -> Target:
    """B is the observed pull request head and the status target; T is its recorded tree."""
    pull_request = _record(receipt.get("pull_request"), "readiness_receipt_invalid")
    target = _record(receipt.get("target"), "readiness_receipt_invalid")
    commit, tree = target.get("commit"), target.get("tree")
    if (not isinstance(commit, str) or GIT_OBJECT_RE.fullmatch(commit) is None or not isinstance(tree, str)
            or GIT_OBJECT_RE.fullmatch(tree) is None or pull_request.get("head_sha") != commit):
        raise _refuse("readiness_target_mismatch")
    return Target(commit=commit, tree=tree)


def _bind_statuses(receipt: dict[str, object], members: dict[str, bytes], run: RunContext, target: Target) -> None:
    """Both statuses were acknowledged on B with distinct IDs, and each matches its archived request body."""
    statuses = _record(receipt.get("statuses"), "readiness_receipt_invalid")
    identities: set[int] = set()
    for name, state, member in STATUSES:
        record = _record(statuses.get(name), "readiness_status_mismatch")
        sent = {"state": state, "context": STATUS_CONTEXT, "target_url": run.target_url}
        identity = record.get("id")
        if (not _positive_int(identity) or identity in identities
                or record != {**sent, "commit": target.commit, "delivery": "acknowledged", "id": identity}):
            raise _refuse("readiness_status_mismatch")
        identities.add(identity)
        if _json_object(members[member]) != sent:
            raise _refuse("readiness_status_body_mismatch")


def _fresh_snapshot(client: GitHubClient, request: PublicationRequest, *,
                    verify_store_protection: bool) -> CandidateSnapshot:
    """One fresh canonical read; Root's strict refusal is final and never retried without protection."""
    reader = load_candidate if verify_store_protection else load_candidate_at_branch_head
    snapshot = reader(client, request.store, request.candidate_id, closure_resolutions=())
    if snapshot is None:
        raise _refuse("publication_candidate_absent")
    if snapshot.freeze_commit != request.reference.freeze_commit:
        raise _refuse("publication_freeze_mismatch")
    return snapshot


def _bind_fresh(snapshot: CandidateSnapshot, ready: _ReadyReceipt, request: PublicationRequest) -> ReviewedSource:
    """The fresh freeze, review, source and scope are the receipt's; canonical readiness must hold again."""
    review = selected_review(snapshot)
    publication, source = snapshot.history.publication, snapshot.candidate.manifest.source
    scope = review.record.reviewed_scope.scope
    if (snapshot.candidate.candidate_id != request.candidate_id or publication is None
            or publication.sha256 != ready.publication_sha256 or review.sha256 != ready.review_sha256
            or (source.commit, source.tree) != (ready.target.commit, ready.target.tree)
            or ready.candidate_source != {"commit": source.commit, "tree": source.tree,
                                          "base_commit": source.base_commit}
            or ready.scope != scope_record(scope)):
        raise _refuse("publication_snapshot_mismatch")
    readiness = evaluate_readiness(candidate=snapshot.candidate,
                                   observed=SourceObservation(head_commit=source.commit, tree=source.tree),
                                   scope=scope, history=snapshot.history, review=review)
    if not readiness.ready:
        raise PublicationNotReady(readiness.findings)
    return review
