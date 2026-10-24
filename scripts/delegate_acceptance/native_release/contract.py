"""Immutable release, candidate and publication inputs and checked receipt values."""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
import re

from scripts.delegate_acceptance.release_verification.candidate_codec import encode_record, load_record
from scripts.delegate_acceptance.release_verification.candidate_manifest import (
    LOCATOR_STAGE, MAX_LOCATOR_BYTES, CandidateLocator, encode_locator, parse_locator,
)
from scripts.delegate_acceptance.release_verification.candidate_publication_contract import (
    MAX_REFERENCE_BYTES, REFERENCE_STAGE, CandidateReference, encode_reference, parse_reference,
)
from scripts.delegate_acceptance.release_verification.candidate_store_contract import StoreLocation
from scripts.delegate_acceptance.release_verification.contract import VerificationError


CANONICAL_REPOSITORY = "hishamkaram/delegation-layer"
# The reviewed production evidence store; selecting it grants nothing, and its branch stays the proof state owner.
PRODUCTION_STORE = StoreLocation(repository=CANONICAL_REPOSITORY, branch="development-proof/evidence",
                                 genesis_commit="d3582a5613e0194899170b5034833d1b88ddaa09",
                                 marker_blob="8b5bcb36f5acbed53dcef51220d7056643ed70b9")
WORKFLOW = "release-native-acceptance.yml"


def trusted_main_workflow_paths(filename: str) -> tuple[str, str]:
    """The two run paths of a maintained workflow file dispatched on main; no other ref is trusted.

    Observed runs report the bare path; GitHub's run reference documents the
    main-qualified form: https://docs.github.com/en/rest/actions/workflow-runs#get-a-workflow-run
    """
    path = f".github/workflows/{filename}"
    return path, f"{path}@main"


WORKFLOW_RUN_PATHS = trusted_main_workflow_paths(WORKFLOW)
STAGES = ("preflight", "download", "archive", "native", "finalize", "project",
          "upload", "dispatch", "watch", "cleanup")
# A development candidate has no release to finalize, upload to or validate.
CANDIDATE_STAGES = ("preflight", "download", "archive", "claim", "native", "project", "record", "cleanup")
# A publication delivers already-proven bytes; it downloads, archives and launches nothing native.
PUBLICATION_STAGES = ("preflight", "prepare", "dispatch", "observe", "verify", "cleanup")
RELEASE_RECEIPT_KIND = "local-native-release"
CANDIDATE_RECEIPT_KIND = "local-native-candidate"
PUBLICATION_RECEIPT_KIND = "local-candidate-publication"
MAX_RECEIPT = 1 << 20
_MODEL = r"[A-Za-z0-9][A-Za-z0-9._:+/-]{0,127}"


class ReleaseError(Exception):
    def __init__(self, code: str, *, incomplete: bool = False,
                 mismatched: tuple[str, ...] = ()):
        super().__init__(code)
        self.code = code
        self.incomplete = incomplete
        # Fixed field names chosen by the validator, never observed values.
        self.mismatched = mismatched

    @property
    def diagnostic(self) -> str:
        if not self.mismatched:
            return self.code
        return f"{self.code}:mismatch={','.join(self.mismatched)}"


def object_value(value: object) -> dict[str, object]:
    if not isinstance(value, dict) or any(not isinstance(key, str) for key in value):
        raise ReleaseError("release_record_object_invalid")
    return {key: item for key, item in value.items()}


def text_value(value: object, pattern: str | None = None) -> str:
    if (not isinstance(value, str) or not value or "\x00" in value
            or pattern is not None and re.fullmatch(pattern, value) is None):
        raise ReleaseError("release_record_text_invalid")
    return value


def integer(value: object, *, minimum: int = 0) -> int:
    if type(value) is not int or value < minimum:
        raise ReleaseError("release_record_integer_invalid")
    return value


@dataclass(frozen=True)
class DispatchAck:
    """The one run a workflow dispatch reply acknowledged; both URLs name exactly that run."""

    run_id: int
    html_url: str
    run_url: str


def parse_workflow_dispatch_ack(response: object, repository: str) -> DispatchAck:
    """Read a dispatch reply: an object, a positive run ID, then both exact run URLs in ``repository``."""
    record = object_value(response)
    run_id = integer(record.get("workflow_run_id"), minimum=1)
    html_url = f"https://github.com/{repository}/actions/runs/{run_id}"
    run_url = f"https://api.github.com/repos/{repository}/actions/runs/{run_id}"
    if record.get("html_url") != html_url or record.get("run_url") != run_url:
        raise ReleaseError("dispatch_response_identity_invalid", incomplete=True)
    return DispatchAck(run_id=run_id, html_url=html_url, run_url=run_url)


def digest_value(value: object) -> str:
    return text_value(value, r"[0-9a-f]{64}")


def revision_value(value: object) -> str:
    return text_value(value, r"[0-9a-f]{40,64}")


def _validate_models(opencode_model: object, ccr_model: object, agy_model: object) -> None:
    """OpenCode always names a model; None selects the CCR or AGY default."""
    text_value(opencode_model, _MODEL)
    for model in (ccr_model, agy_model):
        if model is not None:
            text_value(model, _MODEL)


@dataclass(frozen=True)
class ReleaseRequest:
    repo: str
    tag: str
    opencode_model: str
    ccr_model: str | None
    no_submit: bool
    # None selects AGY's native default model; stored requests omit the key.
    agy_model: str | None = None

    def __post_init__(self) -> None:
        text_value(self.repo, r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+")
        text_value(self.tag, r"v[0-9]+\.[0-9]+\.[0-9]+")
        _validate_models(self.opencode_model, self.ccr_model, self.agy_model)
        if type(self.no_submit) is not bool:
            raise ReleaseError("release_submission_option_invalid")

    def document(self) -> dict[str, object]:
        result: dict[str, object] = {
            "repo": self.repo, "tag": self.tag, "opencode_model": self.opencode_model,
            "ccr_model": self.ccr_model, "no_submit": self.no_submit,
        }
        if self.agy_model is not None:
            result["agy_model"] = self.agy_model
        return result

    @classmethod
    def parse(cls, value: object) -> ReleaseRequest:
        record = object_value(value)
        ccr = record.get("ccr_model")
        no_submit = record.get("no_submit")
        agy = record.get("agy_model")
        keys = {"repo", "tag", "opencode_model", "ccr_model", "no_submit"}
        # One encoding per value: an omitted key is the only form of no AGY model.
        if (set(record) not in (keys, keys | {"agy_model"})
                or "agy_model" in record and not isinstance(agy, str)
                or ccr is not None and not isinstance(ccr, str)
                or type(no_submit) is not bool):
            raise ReleaseError("release_request_invalid")
        return cls(repo=text_value(record["repo"]), tag=text_value(record["tag"]),
                   opencode_model=text_value(record["opencode_model"]),
                   ccr_model=ccr, no_submit=no_submit,
                   agy_model=None if agy is None else text_value(agy))


_CANDIDATE_REQUEST_KEYS = frozenset({"locator", "store", "attempt", "opencode_model", "ccr_model", "agy_model"})
_PUBLICATION_REQUEST_KEYS = frozenset({
    "store", "reference", "readiness_run_id", "readiness_run_attempt", "resume_published",
})
_STORE_KEYS = frozenset({"repository", "branch", "genesis_commit", "marker_blob"})


def _store_document(store: StoreLocation) -> dict[str, object]:
    return {"repository": store.repository, "branch": store.branch,
            "genesis_commit": store.genesis_commit, "marker_blob": store.marker_blob}


def _parse_store(value: object, code: str) -> StoreLocation:
    """Read the one store encoding; a shape or location fault refuses with the request's ``code``."""
    store = object_value(value)
    if set(store) != _STORE_KEYS:
        raise ReleaseError(code)
    try:
        return StoreLocation(repository=text_value(store["repository"]), branch=text_value(store["branch"]),
                             genesis_commit=text_value(store["genesis_commit"]),
                             marker_blob=text_value(store["marker_blob"]))
    except VerificationError as error:
        raise ReleaseError(code) from error


@dataclass(frozen=True)
class CandidateRequest:
    """One native attempt of a development candidate, pinned to its locator and evidence store.

    The canonical locator and store owners validate their values; the request
    keeps them whole, never their input file names or a mutable branch head.
    None selects the CCR or AGY default model.
    """

    locator: CandidateLocator
    store: StoreLocation
    attempt: int
    opencode_model: str
    ccr_model: str | None
    agy_model: str | None = None

    def __post_init__(self) -> None:
        if not isinstance(self.locator, CandidateLocator) or not isinstance(self.store, StoreLocation):
            raise ReleaseError("candidate_request_invalid")
        if self.store.repository != self.locator.repository:
            raise ReleaseError("candidate_store_repository_mismatch")
        integer(self.attempt, minimum=1)
        _validate_models(self.opencode_model, self.ccr_model, self.agy_model)

    @property
    def repository(self) -> str:
        return self.locator.repository

    @property
    def candidate_id(self) -> str:
        return self.locator.candidate_id

    def document(self) -> dict[str, object]:
        return {
            "locator": load_record(encode_locator(self.locator), max_bytes=MAX_LOCATOR_BYTES, stage=LOCATOR_STAGE),
            "store": _store_document(self.store),
            "attempt": self.attempt, "opencode_model": self.opencode_model,
            "ccr_model": self.ccr_model, "agy_model": self.agy_model,
        }

    @classmethod
    def parse(cls, value: object) -> CandidateRequest:
        """Read the one encoding ``document`` writes; every key is present and a default model is null."""
        record = object_value(value)
        if (set(record) != _CANDIDATE_REQUEST_KEYS
                or any(record[key] is not None and not isinstance(record[key], str)
                       for key in ("ccr_model", "agy_model"))):
            raise ReleaseError("candidate_request_invalid")
        location = _parse_store(record["store"], "candidate_request_invalid")
        try:
            locator = parse_locator(encode_record(object_value(record["locator"]), LOCATOR_STAGE))
        except VerificationError as error:
            raise ReleaseError("candidate_request_invalid") from error
        ccr, agy = record["ccr_model"], record["agy_model"]
        return cls(locator=locator, store=location, attempt=integer(record["attempt"], minimum=1),
                   opencode_model=text_value(record["opencode_model"]),
                   ccr_model=None if ccr is None else text_value(ccr),
                   agy_model=None if agy is None else text_value(agy))


@dataclass(frozen=True)
class PublicationRequest:
    """One Root publication invocation of a frozen candidate, pinned to its trusted evidence store.

    The canonical reference and store owners validate their values; the store
    is the only source of the repository. The selected readiness run and the
    explicit ``resume_published`` choice are caller inputs a preflight must
    still verify. Prepared release identities, harness revisions and
    destination observations are stage results, never request data. The
    request grants no authorization.
    """

    store: StoreLocation
    reference: CandidateReference
    readiness_run_id: int
    readiness_run_attempt: int
    resume_published: bool

    def __post_init__(self) -> None:
        if not isinstance(self.store, StoreLocation) or not isinstance(self.reference, CandidateReference):
            raise ReleaseError("publication_request_invalid")
        integer(self.readiness_run_id, minimum=1)
        integer(self.readiness_run_attempt, minimum=1)
        if type(self.resume_published) is not bool:
            raise ReleaseError("publication_resume_option_invalid")

    @property
    def repository(self) -> str:
        return self.store.repository

    @property
    def candidate_id(self) -> str:
        return self.reference.candidate_id

    def document(self) -> dict[str, object]:
        return {
            "store": _store_document(self.store),
            "reference": load_record(encode_reference(self.reference), max_bytes=MAX_REFERENCE_BYTES,
                                     stage=REFERENCE_STAGE),
            "readiness_run_id": self.readiness_run_id, "readiness_run_attempt": self.readiness_run_attempt,
            "resume_published": self.resume_published,
        }

    @classmethod
    def parse(cls, value: object) -> PublicationRequest:
        """Read the one encoding ``document`` writes; every key is present."""
        record = object_value(value)
        if set(record) != _PUBLICATION_REQUEST_KEYS or type(record["resume_published"]) is not bool:
            raise ReleaseError("publication_request_invalid")
        store = _parse_store(record["store"], "publication_request_invalid")
        try:
            reference = parse_reference(encode_record(object_value(record["reference"]), REFERENCE_STAGE))
        except VerificationError as error:
            raise ReleaseError("publication_request_invalid") from error
        return cls(store=store, reference=reference,
                   readiness_run_id=integer(record["readiness_run_id"], minimum=1),
                   readiness_run_attempt=integer(record["readiness_run_attempt"], minimum=1),
                   resume_published=record["resume_published"])


# The closed set of targets one invocation may run.
Request = ReleaseRequest | CandidateRequest | PublicationRequest


def receipt_kind(request: Request) -> str:
    if isinstance(request, CandidateRequest):
        return CANDIDATE_RECEIPT_KIND
    if isinstance(request, PublicationRequest):
        return PUBLICATION_RECEIPT_KIND
    return RELEASE_RECEIPT_KIND


def receipt_stages(request: Request) -> tuple[str, ...]:
    if isinstance(request, CandidateRequest):
        return CANDIDATE_STAGES
    if isinstance(request, PublicationRequest):
        return PUBLICATION_STAGES
    return STAGES


# Each target's closed receipt status vocabulary; only a publication can be PUBLISHED.
NATIVE_STATUSES = ("INCOMPLETE", "BLOCKED", "FAIL", "LOCAL_ONLY", "PROMOTABLE")
PUBLICATION_STATUSES = ("INCOMPLETE", "BLOCKED", "PUBLISHED")


def receipt_statuses(request: Request) -> tuple[str, ...]:
    return PUBLICATION_STATUSES if isinstance(request, PublicationRequest) else NATIVE_STATUSES


def status_value(request: Request, value: object) -> str:
    """The one status reader of a receipt: text first, then only ``request``'s own vocabulary."""
    status = text_value(value)
    if status not in receipt_statuses(request):
        raise ReleaseError("release_record_text_invalid")
    return status


def request_parser(kind: object) -> Callable[[object], Request]:
    """Select the request reader of a receipt ``kind``; an unknown kind refuses before anything is read."""
    if kind == RELEASE_RECEIPT_KIND:
        return ReleaseRequest.parse
    if kind == CANDIDATE_RECEIPT_KIND:
        return CandidateRequest.parse
    if kind == PUBLICATION_RECEIPT_KIND:
        return PublicationRequest.parse
    raise ReleaseError("release_receipt_invalid")


def invocation_name(request: Request, invocation_id: str) -> str:
    """The one directory name of an invocation, used to create its root and to check it on load."""
    if isinstance(request, CandidateRequest):
        return f"candidate-{request.candidate_id}-{invocation_id}"
    if isinstance(request, PublicationRequest):
        return f"publication-{request.candidate_id}-{invocation_id}"
    return f"{request.tag}-{invocation_id}"


@dataclass(frozen=True)
class Invocation:
    invocation_id: str
    request: Request
    checkout: Path
    root: Path
    platform_os: str
    platform_arch: str
    directory_identity: dict[str, object]

    @property
    def proof_name(self) -> str:
        return f"native-proof-{self.platform_os}-{self.platform_arch}-{self.invocation_id}.zip"

    @property
    def assets(self) -> Path:
        return self.root / "runner" / "delegation-layer-release"

    @property
    def aggregate(self) -> Path:
        return self.root / "native" / "aggregate-receipt.json"

    @property
    def archive_receipt(self) -> Path:
        return self.root / "archive-receipt.json"

    @property
    def native_receipt(self) -> Path:
        return self.root / "native-acceptance-receipt.json"

    @property
    def proof(self) -> Path:
        return self.root / self.proof_name


def release_request(invocation: Invocation) -> ReleaseRequest:
    """The release request of a legacy stage; a candidate invocation never reaches release reads."""
    request = invocation.request
    if not isinstance(request, ReleaseRequest):
        raise ReleaseError("release_request_required")
    return request


def candidate_request(invocation: Invocation) -> CandidateRequest:
    """The candidate request of a candidate stage; a release invocation never reaches candidate reads."""
    request = invocation.request
    if not isinstance(request, CandidateRequest):
        raise ReleaseError("candidate_request_required")
    return request


def publication_request(invocation: Invocation) -> PublicationRequest:
    """The publication request of a publication stage; a native invocation never reaches publication reads."""
    request = invocation.request
    if not isinstance(request, PublicationRequest):
        raise ReleaseError("publication_request_required")
    return request
