"""Record one reviewer's source review of a registered development candidate.

The store branch owns review records, and only ``candidate_store.append``
writes it. The source observer owns the exact Git facts of the manifest
source. The submitted ``SourceReview`` owns the scope classification,
rationale, reviewer and findings. This edge proves that every binding the
review submits equals what is observed now, then hands the unchanged record
to one append and returns its outcome unchanged. It classifies nothing,
never rewrites, retries or reads back, and never grants provenance: only a
later canonical store read establishes which commit introduced the review.

The maintainer command reads an existing ``SourceReview`` document, reserves
a new receipt before any store mutation and writes it once with the actual
outcome. The receipt is a one-shot observation, never progress state or an
approval. The caller keeps the work directory and the receipt.
"""

from __future__ import annotations

import argparse
from collections.abc import Callable, Mapping
import os
from pathlib import Path

from ..evidence.bounded_files import BoundedReadError, read_bounded_regular_file
from ..evidence.digests import sha
from .candidate_attempts import SourceReview, encode_review, parse_review
from .candidate_build_source import child_environment
from .candidate_closure import ClosureResolution
from .candidate_record_receipt import ReceiptIdentity, disposition, refusal, report_unresolved, run_once
from .candidate_source_inventory import ObservedSourceChange, reviewed_change_scope
from .candidate_source_observation import observe_source_change
from .candidate_store import append, load_candidate
from .candidate_store_contract import (
    FAMILY_MAX_BYTES, AppendOutcome, EntryFamily, EntryName, StoreLocation, record_blob,
)
from .contract import VerificationError
from .github import GitHubClient


STAGE = "record-candidate-review"


RECEIPT_KIND = "delegation-layer-candidate-review-record"


RECEIPT = ReceiptIdentity(kind=RECEIPT_KIND, stage=STAGE, unavailable_code="review_receipt_unavailable",
                          write_failed_code="review_receipt_write_failed")


def record_source_review(client: GitHubClient, location: StoreLocation, review: SourceReview, *,
                         repository: Path, environment: Mapping[str, str], work_directory: Path,
                         closure_resolutions: tuple[ClosureResolution, ...],
                         observe_failure: Callable[[VerificationError], None]) -> AppendOutcome:
    """Append ``review`` once if its bindings equal the registered manifest's observed source.

    The strict store read finds the manifest; a source mismatch refuses before
    any store mutation. Git commands run credential-free and are recorded in
    ``client.receipt``. The same ``closure_resolutions`` reach the load and
    the append. Canonical refusals, ``ReplayDenied`` findings included, raise
    unchanged; ``work_directory`` and ``observe_failure`` follow the store
    writer's contract.
    """
    if not isinstance(review, SourceReview):
        raise VerificationError("candidate_review_invalid", STAGE)
    snapshot = load_candidate(client, location, review.candidate_id, closure_resolutions=closure_resolutions)
    if snapshot is None:
        raise VerificationError("candidate_not_registered", STAGE)
    observed = observe_source_change(client.receipt, child_environment(environment), repository=repository,
                                     source=snapshot.candidate.manifest.source)
    _require_observed(review, snapshot.candidate.candidate_id, observed)
    data = encode_review(review)
    return append(client, location, review.candidate_id,
                  (record_blob(EntryName(EntryFamily.REVIEW, digest=sha(data)), data),),
                  closure_resolutions=closure_resolutions, work_directory=work_directory,
                  observe_failure=observe_failure)


def _require_observed(review: SourceReview, candidate_id: str, observed: ObservedSourceChange) -> None:
    """Refuse unless every submitted binding equals the observation; the review's own scope is not judged."""
    source = observed.source
    if (review.candidate_id, review.head_commit, review.tree) != (candidate_id, source.commit, source.tree):
        raise VerificationError("review_binding_mismatch", STAGE)
    submitted = review.reviewed_scope
    expected = reviewed_change_scope(observed, scope=submitted.scope, rationale=submitted.rationale)
    if (submitted.base_commit, submitted.base_tree) != (expected.base_commit, expected.base_tree):
        raise VerificationError("review_baseline_mismatch", STAGE)
    if submitted != expected:
        raise VerificationError("review_change_mismatch", STAGE)


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Record one source review of a registered development candidate.")
    parser.add_argument("--review-input", type=Path, required=True, help="an existing source review document")
    parser.add_argument("--repository-root", type=Path, required=True)
    for name in ("store-repository", "store-branch", "store-genesis-commit", "store-marker-blob"):
        parser.add_argument(f"--{name}", required=True)
    parser.add_argument("--work-dir", type=Path, required=True)
    parser.add_argument("--receipt", type=Path, required=True, help="a new file; an existing one is refused")
    return parser


def _read_review(path: Path) -> SourceReview:
    try:
        data = read_bounded_regular_file(path, FAMILY_MAX_BYTES[EntryFamily.REVIEW])
    except BoundedReadError:
        raise VerificationError("review_input_unreadable", STAGE) from None
    return parse_review(data)


def _bindings(review: SourceReview) -> dict[str, object]:
    reviewed = review.reviewed_scope
    return {"candidate_id": review.candidate_id, "review_sha256": sha(encode_review(review)),
            "reviewer": review.reviewer, "head_commit": review.head_commit, "tree": review.tree,
            "base_commit": reviewed.base_commit, "base_tree": reviewed.base_tree,
            "changed_path_count": reviewed.changed_path_count,
            "changed_paths_sha256": reviewed.changed_paths_sha256,
            "subsystem_registry_sha256": reviewed.subsystem_registry_sha256}


def _record(arguments: argparse.Namespace, client: GitHubClient, document: dict[str, object]) -> dict[str, object]:
    """Read the review, record it once and return the sanitized outcome; nothing here retries."""
    try:
        review = _read_review(arguments.review_input)
        document["review"] = _bindings(review)
        location = StoreLocation(repository=arguments.store_repository, branch=arguments.store_branch,
                                 genesis_commit=arguments.store_genesis_commit,
                                 marker_blob=arguments.store_marker_blob)
        outcome = record_source_review(
            client, location, review, repository=arguments.repository_root, environment=os.environ,
            work_directory=arguments.work_dir, closure_resolutions=(), observe_failure=report_unresolved)
    except VerificationError as error:
        return refusal(error)
    return disposition(outcome, stage=STAGE)


def main(argv: list[str] | None = None) -> int:
    """0 confirmed; 1 refused or failed; 2 usage (argparse); 3 ambiguous, with the work directory kept."""
    arguments = _parser().parse_args(argv)
    return run_once(arguments.receipt, RECEIPT, {"review": None},
                    lambda client, document: _record(arguments, client, document))
