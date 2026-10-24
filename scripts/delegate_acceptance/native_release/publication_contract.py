"""Pure publication wire, run report and visibility rules shared by Root and hosted delivery.

This module reads no storage, network, process or clock, stores nothing and
grants no approval. Root's receipt, under its exclusive invocation lock, owns
stage and status progression; the GitHub run owns execution; the hosted writer
alone mutates destinations; each platform owns its published objects. Here are
only the exact workflow dispatch inputs and their run title, the one report the
hosted job nests in its ``finish_gate`` receipt, and the public-repository guard.

H, the dispatch ``harness``, is the trusted-main publication code revision. It is
never the selected readiness run's own harness or the evidence store head.
"""

from __future__ import annotations

from copy import deepcopy
from dataclasses import dataclass, fields
import json
import re

from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.release_verification.candidate_codec import encode_record
from scripts.delegate_acceptance.release_verification.candidate_publication_contract import (
    CandidatePromotion, CandidateReference, ObservedRelease, decide_release, encode_promotion,
)
from scripts.delegate_acceptance.release_verification.candidate_store_contract import GIT_OBJECT_RE
from scripts.delegate_acceptance.release_verification.contract import HEX_SHA256, REVISION_RE, VerificationError
from scripts.delegate_acceptance.release_verification.published_contract import PackageIdentity, parse_npm_view
from .contract import (
    PRODUCTION_STORE, PublicationRequest, ReleaseError, integer, object_value, trusted_main_workflow_paths,
)


STAGE = "publication-protocol"
PUBLICATION_WORKFLOW = "release.yml"
PUBLICATION_RUN_PATHS = trusted_main_workflow_paths(PUBLICATION_WORKFLOW)
INVOCATION_ID_RE = re.compile(r"^[0-9a-f]{32}$")
# At most 18 digits, so every accepted run identity fits GitHub's signed 64-bit IDs.
_DECIMAL_RE = re.compile(r"^[1-9][0-9]{0,17}$")
_BOOLEAN_RE = re.compile(r"^(true|false)$")
# The nine workflow inputs, every one a string in exactly one canonical form. The store is never an input.
_INPUT_PATTERNS: dict[str, re.Pattern[str]] = {
    "candidate_id": HEX_SHA256, "freeze_commit": REVISION_RE, "merged_commit": REVISION_RE,
    "readiness_run_id": _DECIMAL_RE, "readiness_run_attempt": _DECIMAL_RE, "resume_published": _BOOLEAN_RE,
    "invocation_id": INVOCATION_ID_RE, "request_sha256": HEX_SHA256, "harness": GIT_OBJECT_RE,
}
INPUT_NAMES = tuple(_INPUT_PATTERNS)


def _matches(pattern: re.Pattern[str], value: object) -> bool:
    return isinstance(value, str) and pattern.fullmatch(value) is not None


def request_digest(request: PublicationRequest) -> str:
    """SHA-256 of the canonical encoding of the request's one document."""
    if not isinstance(request, PublicationRequest):
        raise ReleaseError("publication_request_invalid")
    return sha(encode_record(request.document(), STAGE))


def publication_title(invocation_id: str, request_sha256: str) -> str:
    """The one run title of an exact dispatch; equal inputs always give the same title."""
    if not _matches(INVOCATION_ID_RE, invocation_id) or not _matches(HEX_SHA256, request_sha256):
        raise ReleaseError("publication_title_identity_invalid")
    return f"Candidate publication {invocation_id} {request_sha256}"


@dataclass(frozen=True)
class PublicationDispatch:
    """One Root invocation's exact workflow dispatch: the immutable request, the invocation and H.

    Only the production store can be dispatched, since the workflow takes no
    store input. The value grants nothing; the run still has to be observed.
    """

    request: PublicationRequest
    invocation_id: str
    harness: str

    def __post_init__(self) -> None:
        if not isinstance(self.request, PublicationRequest):
            raise ReleaseError("publication_request_invalid")
        if self.request.store != PRODUCTION_STORE:
            raise ReleaseError("publication_store_not_production")
        if (not _matches(INVOCATION_ID_RE, self.invocation_id) or not _matches(GIT_OBJECT_RE, self.harness)
                or not _matches(_DECIMAL_RE, str(self.request.readiness_run_id))
                or not _matches(_DECIMAL_RE, str(self.request.readiness_run_attempt))):
            raise ReleaseError("publication_dispatch_invalid")

    @property
    def request_sha256(self) -> str:
        return request_digest(self.request)

    @property
    def title(self) -> str:
        return publication_title(self.invocation_id, self.request_sha256)

    def inputs(self) -> dict[str, str]:
        """The exact workflow inputs ``parse_dispatch_inputs`` reads back to an equal dispatch."""
        request, reference = self.request, self.request.reference
        return {
            "candidate_id": reference.candidate_id, "freeze_commit": reference.freeze_commit,
            "merged_commit": reference.merged_commit, "readiness_run_id": str(request.readiness_run_id),
            "readiness_run_attempt": str(request.readiness_run_attempt),
            "resume_published": "true" if request.resume_published else "false",
            "invocation_id": self.invocation_id, "request_sha256": self.request_sha256, "harness": self.harness,
        }


def parse_dispatch_inputs(value: object) -> PublicationDispatch:
    """Read exactly the nine canonical string inputs, then require the request digest they name.

    The request is rebuilt with the production store and its digest is checked
    here, so a caller performs no I/O for a wrong, malformed or foreign input.
    A field fault names only the fixed input names that failed.
    """
    record = object_value(value)
    if set(record) != set(INPUT_NAMES) or any(type(item) is not str for item in record.values()):
        raise ReleaseError("publication_dispatch_inputs_invalid")
    mismatched = tuple(name for name, pattern in _INPUT_PATTERNS.items() if not _matches(pattern, record[name]))
    if mismatched:
        raise ReleaseError("publication_dispatch_input_invalid", mismatched=mismatched)
    reference = CandidateReference(candidate_id=record["candidate_id"], freeze_commit=record["freeze_commit"],
                                   merged_commit=record["merged_commit"])
    request = PublicationRequest(store=PRODUCTION_STORE, reference=reference,
                                 readiness_run_id=int(record["readiness_run_id"]),
                                 readiness_run_attempt=int(record["readiness_run_attempt"]),
                                 resume_published=record["resume_published"] == "true")
    dispatch = PublicationDispatch(request=request, invocation_id=record["invocation_id"], harness=record["harness"])
    if dispatch.request_sha256 != record["request_sha256"]:
        raise ReleaseError("publication_request_digest_mismatch")
    return dispatch


def require_public_repository(repository_private: bool) -> None:
    """Refuse an actual publication unless the shared preflight observed an exactly public repository."""
    if type(repository_private) is not bool:
        raise ReleaseError("repository_visibility_unknown")
    if repository_private:
        raise ReleaseError("publication_repository_private")


REPORT_SCHEMA_VERSION = 1
REPORT_KIND = "delegation-layer-publication-report"
# The key of the one report inside the hosted job's ``finish_gate`` receipt.
REPORT_FIELD = "publication"
_REPORT_KEYS = frozenset({"schema_version", "kind", "binding", "first_mutation_started", "delivery"})
_DELIVERY_KEYS = frozenset({"promotion", "github", "npm", "tap"})
_NPM_KEYS = frozenset({"name", "version", "integrity", "shasum", "tarball", "git_head", "latest"})
# A finish_gate failure code or stage: fixed lowercase identifiers, never observed text.
_DIAGNOSTIC_RE = re.compile(r"^[a-z0-9][a-z0-9_.:,=-]{0,127}$")


@dataclass(frozen=True)
class ReportBinding:
    """The exact Root invocation, request digest, hosted run attempt and code H one report describes."""

    invocation_id: str
    request_sha256: str
    run_id: int
    run_attempt: int
    harness: str

    def __post_init__(self) -> None:
        integer(self.run_id, minimum=1)
        integer(self.run_attempt, minimum=1)
        if (not _matches(INVOCATION_ID_RE, self.invocation_id) or not _matches(HEX_SHA256, self.request_sha256)
                or not _matches(GIT_OBJECT_RE, self.harness)):
            raise ReleaseError("publication_report_binding_invalid")

    @classmethod
    def of(cls, dispatch: PublicationDispatch, *, run_id: int, run_attempt: int) -> ReportBinding:
        if not isinstance(dispatch, PublicationDispatch):
            raise ReleaseError("publication_dispatch_invalid")
        return cls(invocation_id=dispatch.invocation_id, request_sha256=dispatch.request_sha256,
                   run_id=run_id, run_attempt=run_attempt, harness=dispatch.harness)

    def document(self) -> dict[str, object]:
        return {item.name: getattr(self, item.name) for item in fields(self)}

    @classmethod
    def parse(cls, value: object) -> ReportBinding:
        record = object_value(value)
        if set(record) != {item.name for item in fields(cls)}:
            raise ReleaseError("publication_report_binding_invalid")
        return cls(invocation_id=record["invocation_id"], request_sha256=record["request_sha256"],
                   run_id=record["run_id"], run_attempt=record["run_attempt"], harness=record["harness"])


@dataclass(frozen=True)
class PublishedDelivery:
    """What the hosted writer confirmed after its own delivery postconditions; it approves nothing.

    ``release`` is the published release read by ``release_id``; ``npm`` and
    ``npm_latest`` are the registry's confirmed identity and ``latest`` tag;
    ``tap_blob_sha`` is the confirmed formula blob.
    """

    release_id: int
    release: ObservedRelease
    npm: PackageIdentity
    npm_latest: str
    tap_blob_sha: str

    def __post_init__(self) -> None:
        if (not isinstance(self.release, ObservedRelease) or not isinstance(self.npm, PackageIdentity)
                or type(self.npm_latest) is not str or type(self.tap_blob_sha) is not str):
            raise ReleaseError("publication_delivery_invalid")
        integer(self.release_id, minimum=1)


@dataclass(frozen=True)
class PublicationFailure:
    """A finalized failed hosted run of the expected binding; it is never a success.

    ``code`` and ``stage`` are ``finish_gate``'s own sanitized failure.
    ``delivery_confirmed`` is True when every delivery postcondition held but
    the job still failed, for example in its own cleanup.
    """

    first_mutation_started: bool
    delivery_confirmed: bool
    code: str
    stage: str


def publication_report(binding: ReportBinding, *, first_mutation_started: bool,
                       promotion: CandidatePromotion | None = None,
                       delivery: PublishedDelivery | None = None) -> dict[str, object]:
    """The report the hosted job nests under ``REPORT_FIELD``; delivery facts need both values.

    A delivery is recorded only when the canonical release decision finds the
    published release complete and every fact passes the same check Root runs.
    """
    if (not isinstance(binding, ReportBinding) or type(first_mutation_started) is not bool
            or (promotion is None) != (delivery is None)):
        raise ReleaseError("publication_report_invalid")
    delivered: dict[str, object] | None = None
    if promotion is not None and delivery is not None:
        if not isinstance(promotion, CandidatePromotion) or not isinstance(delivery, PublishedDelivery):
            raise ReleaseError("publication_report_invalid")
        if not decide_release(promotion=promotion, observed=delivery.release, resume_published=False).complete:
            raise ReleaseError("publication_report_release_incomplete")
        delivered = _delivery_document(promotion, delivery)
        _check_delivery(delivered, promotion)
    return {"schema_version": REPORT_SCHEMA_VERSION, "kind": REPORT_KIND, "binding": binding.document(),
            "first_mutation_started": first_mutation_started, "delivery": delivered}


def require_published(receipt: object, *, binding: ReportBinding,
                      promotion: CandidatePromotion) -> dict[str, object]:
    """Return a copy of the report of a finalized successful hosted receipt with exact bindings and delivery.

    The receipt must be ``finish_gate``'s completed removal: verdict ``pass``,
    stage ``complete``, no failure, completion ``complete`` and cleanup status
    ``complete`` with no retained path when ``retained_paths`` is present.
    Historical cleanup ``errors`` of a completed recovery stay evidence; the
    canonical completion is the authority. The npm integrity and shasum are
    the registry values the hosted writer confirmed; this check validates
    their form and identity, never the package bytes.
    """
    gate = object_value(receipt)
    cleanup = gate.get("cleanup")
    if (gate.get("verdict") != "pass" or gate.get("stage") != "complete" or "failure" not in gate
            or gate["failure"] is not None or gate.get("completion") != {"state": "complete"}
            or not isinstance(cleanup, dict) or cleanup.get("status") != "complete"
            or "retained_paths" in cleanup and cleanup["retained_paths"] != []):
        raise ReleaseError("publication_report_not_finalized")
    report = _bound_report(gate, binding)
    if report["delivery"] is None:
        raise ReleaseError("publication_report_delivery_missing")
    _check_delivery(report["delivery"], _promotion(promotion))
    return deepcopy(report)


def require_failed(receipt: object, *, binding: ReportBinding, promotion: CandidatePromotion) -> PublicationFailure:
    """Read a finalized failed hosted receipt of the expected binding; any recorded delivery must be exact."""
    gate = object_value(receipt)
    failure = gate.get("failure")
    if (gate.get("verdict") != "fail" or not isinstance(failure, dict) or set(failure) != {"code", "stage"}
            or not _matches(_DIAGNOSTIC_RE, failure["code"]) or not _matches(_DIAGNOSTIC_RE, failure["stage"])
            or gate.get("stage") != failure["stage"]):
        raise ReleaseError("publication_report_not_failed")
    report = _bound_report(gate, binding)
    if report["delivery"] is not None:
        _check_delivery(report["delivery"], _promotion(promotion))
    return PublicationFailure(first_mutation_started=report["first_mutation_started"],
                              delivery_confirmed=report["delivery"] is not None,
                              code=failure["code"], stage=failure["stage"])


def _promotion(value: object) -> CandidatePromotion:
    if not isinstance(value, CandidatePromotion):
        raise ReleaseError("publication_report_promotion_invalid")
    return value


def _bound_report(gate: dict[str, object], binding: ReportBinding) -> dict[str, object]:
    """The report envelope, then its binding; a mismatch names only the fixed binding fields."""
    if not isinstance(binding, ReportBinding):
        raise ReleaseError("publication_report_binding_invalid")
    report = object_value(gate.get(REPORT_FIELD))
    version = report.get("schema_version")
    if (set(report) != _REPORT_KEYS or type(version) is not int or version != REPORT_SCHEMA_VERSION
            or report["kind"] != REPORT_KIND or type(report["first_mutation_started"]) is not bool):
        raise ReleaseError("publication_report_invalid")
    observed = ReportBinding.parse(report["binding"])
    mismatched = tuple(item.name for item in fields(ReportBinding)
                       if getattr(observed, item.name) != getattr(binding, item.name))
    if mismatched:
        raise ReleaseError("publication_report_binding_mismatch", mismatched=mismatched)
    return report


def _subjects(promotion: CandidatePromotion) -> list[dict[str, object]]:
    return [{"name": subject.name, "sha256": subject.sha256} for subject in promotion.subjects]


def _delivery_document(promotion: CandidatePromotion, delivery: PublishedDelivery) -> dict[str, object]:
    release, npm = delivery.release, delivery.npm
    assets = sorted(({"name": asset.name, "sha256": asset.sha256} for asset in release.assets),
                    key=lambda asset: asset["name"])
    return {
        "promotion": {"sha256": sha(encode_promotion(promotion)), "subjects": _subjects(promotion)},
        "github": {"release_id": delivery.release_id, "tag": release.tag, "tag_commit": release.target_commit,
                   "assets": assets},
        "npm": {"name": npm.name, "version": npm.version, "integrity": npm.integrity, "shasum": npm.shasum,
                "tarball": npm.tarball, "git_head": npm.git_head, "latest": delivery.npm_latest},
        "tap": {"blob_sha": delivery.tap_blob_sha},
    }


def _exact(value: object, keys: frozenset[str]) -> dict[str, object]:
    record = object_value(value)
    if set(record) != keys:
        raise ReleaseError("publication_report_delivery_invalid")
    return record


def _check_delivery(value: object, promotion: CandidatePromotion) -> None:
    """Every delivered fact equals what the expected promotion requires; a mismatch names fixed fields only."""
    record = _exact(value, _DELIVERY_KEYS)
    stated = _exact(record["promotion"], frozenset({"sha256", "subjects"}))
    github = _exact(record["github"], frozenset({"release_id", "tag", "tag_commit", "assets"}))
    npm = _exact(record["npm"], _NPM_KEYS)
    tap = _exact(record["tap"], frozenset({"blob_sha"}))
    manifest, subjects = promotion.candidate.manifest, _subjects(promotion)
    release_id = github["release_id"]
    mismatched = tuple(name for name, bound in (
        ("promotion_sha256", stated["sha256"] == sha(encode_promotion(promotion))),
        ("subjects", stated["subjects"] == subjects),
        ("release_id", type(release_id) is int and release_id > 0),
        ("tag", github["tag"] == manifest.build_tag),
        ("tag_commit", github["tag_commit"] == promotion.source.merged_commit),
        ("assets", github["assets"] == subjects),
        ("npm_identity", _npm_identity(npm, version=manifest.version, release_commit=manifest.source.commit)),
        ("npm_latest", npm["latest"] == manifest.version),
        ("tap_blob_sha", _matches(GIT_OBJECT_RE, tap["blob_sha"])),
    ) if not bound)
    if mismatched:
        raise ReleaseError("publication_report_delivery_mismatch", mismatched=mismatched)


def _npm_identity(npm: dict[str, object], *, version: str, release_commit: str) -> bool:
    """The canonical ``parse_npm_view`` rule (identity, digest forms, tarball, gitHead) over the reported fields."""
    view = {"name": npm["name"], "version": npm["version"], "dist.integrity": npm["integrity"],
            "dist.shasum": npm["shasum"], "dist.tarball": npm["tarball"], "gitHead": npm["git_head"]}
    try:
        parse_npm_view(json.dumps(view).encode("utf-8"), version, release_commit)
    except VerificationError:
        return False
    return True
