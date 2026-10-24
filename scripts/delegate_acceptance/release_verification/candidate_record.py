"""Record one explicit maintainer record in the development candidate store.

The store branch owns every record, and only ``candidate_store.append``
writes it; its canonical admission decides whether a group is allowed. Each
command reads existing canonical documents with their codec owners, builds
the one group of its family from them unchanged and hands it to one append:

- ``register`` pairs a locator with the manifest its digest binds. It
  downloads nothing and claims no artifact availability or execution.
- ``intent`` records an ``AttemptIntent`` as written. It numbers no attempt
  and copies no scope, platform or replacement.
- ``triage`` records a ``TriageRecord`` as written. It infers no closure,
  diagnosis or approval.
- ``install`` imports one existing install proof and its witness. It runs no
  installation; admission reads the bound installer source itself.
- ``freeze`` selects one stored review by digest, observes the checkout's
  actual HEAD and tree and asks ``authorize_publication`` for the freeze
  record on the strict snapshot's head. The reviewed scope is the scope that
  readiness requires the latest intent to match.
- ``check`` makes the same selection and observation, requires the checkout
  to be clean and reports the canonical ``evaluate_readiness`` verdict for
  them. It appends nothing, needs no pull request or work directory and is
  an observation, never a freeze, merge or publication grant.

Every command supplies no closure facts. Nothing here retries, recomputes
or reads back: a head that moved before admission refuses canonically, and
a ref update that may not have landed stays ambiguous. Only an
``AppendConfirmed`` names the stored commit, the freeze F included. The
receipt (``candidate_record_receipt``) is a one-shot observation, never
progress state or an approval.
"""

from __future__ import annotations

import argparse
from dataclasses import dataclass
import os
from pathlib import Path

from ..evidence.bounded_files import BoundedReadError, read_bounded_regular_file
from ..evidence.digests import sha
from .candidate_attempts import ChangeScope, ReviewedSource, encode_intent, parse_intent, scope_record
from .candidate_build_source import child_environment, head_tree, require_clean
from .candidate_history import encode_publication, encode_triage, parse_triage
from .candidate_install_contract import encode_install_proof, parse_install_proof
from .candidate_manifest import bind_manifest, encode_locator, encode_manifest, parse_locator
from .candidate_readiness import STAGE as READINESS_STAGE
from .candidate_readiness import SourceObservation, authorize_publication, evaluate_readiness
from .candidate_record_receipt import ReceiptIdentity, disposition, refusal, refused, report_unresolved, run_once
from .candidate_store import append, load_candidate
from .candidate_store_contract import FAMILY_MAX_BYTES, EntryFamily, EntryName, StoredBlob, StoreLocation, record_blob
from .candidate_store_replay import CandidateSnapshot
from .contract import HEX_SHA256, VerificationError
from .github import GitHubClient


STAGE = "record-candidate"


RECEIPT = ReceiptIdentity(kind="delegation-layer-candidate-record", stage=STAGE,
                          unavailable_code="record_receipt_unavailable",
                          write_failed_code="record_receipt_write_failed")


CHECK_RECEIPT = ReceiptIdentity(kind="delegation-layer-candidate-readiness-check", stage=STAGE,
                                unavailable_code=RECEIPT.unavailable_code,
                                write_failed_code=RECEIPT.write_failed_code)


CHECK_NOTICE = ("Observation only: a confirmed check means the canonical readiness verdict was ready for the "
                "strict store head, the selected review's scope and the clean checkout HEAD and tree this run "
                "observed. It appended nothing, says nothing about a later head and grants no freeze, merge or "
                "publication.")


@dataclass(frozen=True)
class RecordGroup:
    """One candidate's canonical group of records, exactly as one append receives it."""

    candidate_id: str
    additions: tuple[StoredBlob, ...]


def registration(locator_data: bytes, manifest_data: bytes) -> RecordGroup:
    """Pair the locator with the manifest its digest binds; nothing is downloaded."""
    locator = parse_locator(locator_data)
    manifest = bind_manifest(locator, manifest_data)
    return RecordGroup(locator.candidate_id, (
        record_blob(EntryName(EntryFamily.MANIFEST), encode_manifest(manifest)),
        record_blob(EntryName(EntryFamily.LOCATOR), encode_locator(locator))))


def attempt_intent(data: bytes) -> RecordGroup:
    """The intent under its own attempt number; admission decides whether that attempt may launch."""
    intent = parse_intent(data)
    return RecordGroup(intent.candidate_id, (
        record_blob(EntryName(EntryFamily.INTENT, attempt=intent.attempt), encode_intent(intent)),))


def triage(data: bytes) -> RecordGroup:
    """The triage alone, named by its digest; no closure fact accompanies it."""
    record = parse_triage(data)
    encoded = encode_triage(record)
    return RecordGroup(record.candidate_id, (
        record_blob(EntryName(EntryFamily.TRIAGE, digest=sha(encoded)), encoded),))


def install(proof_data: bytes, witness_data: bytes) -> RecordGroup:
    """The proof and its exact witness bytes, both named by the proof's own platform and channel."""
    proof = parse_install_proof(proof_data)
    role = {"platform": proof.platform, "channel": proof.channel}
    return RecordGroup(proof.candidate_id, (
        record_blob(EntryName(EntryFamily.INSTALL_PROOF, **role), encode_install_proof(proof)),
        record_blob(EntryName(EntryFamily.INSTALL_WITNESS, **role), witness_data)))


def _read(path: Path, family: EntryFamily, label: str, inputs: dict[str, object]) -> bytes:
    """Read one input within its family's bound; only bytes actually read get a digest."""
    try:
        data = read_bounded_regular_file(path, FAMILY_MAX_BYTES[family])
    except BoundedReadError:
        raise VerificationError(f"{label}_input_unreadable", STAGE) from None
    inputs[f"{label}_sha256"] = sha(data)
    return data


def _file_group(arguments: argparse.Namespace, inputs: dict[str, object]) -> RecordGroup:
    if arguments.command == "register":
        return registration(_read(arguments.locator, EntryFamily.LOCATOR, "locator", inputs),
                            _read(arguments.manifest, EntryFamily.MANIFEST, "manifest", inputs))
    if arguments.command == "intent":
        return attempt_intent(_read(arguments.intent, EntryFamily.INTENT, "intent", inputs))
    if arguments.command == "triage":
        return triage(_read(arguments.triage, EntryFamily.TRIAGE, "triage", inputs))
    return install(_read(arguments.proof, EntryFamily.INSTALL_PROOF, "proof", inputs),
                   _read(arguments.witness, EntryFamily.INSTALL_WITNESS, "witness", inputs))


@dataclass(frozen=True)
class ReviewedCheckout:
    """The strict snapshot, its stored review selected by digest and the checkout's observed HEAD and tree."""

    snapshot: CandidateSnapshot
    review: ReviewedSource
    observed: SourceObservation

    @property
    def scope(self) -> ChangeScope:
        """The reviewed scope, which readiness requires the latest intent to match."""
        return self.review.record.reviewed_scope.scope


def _reviewed_checkout(arguments: argparse.Namespace, client: GitHubClient, location: StoreLocation,
                       inputs: dict[str, object]) -> ReviewedCheckout:
    """Select the stored review on the strict snapshot, then observe the checkout; nothing is decided here."""
    inputs.update(candidate_id=arguments.candidate_id, review_sha256=arguments.review_sha256)
    snapshot = load_candidate(client, location, arguments.candidate_id, closure_resolutions=())
    if snapshot is None:
        raise VerificationError("candidate_not_registered", STAGE)
    review = next((item for item in snapshot.reviews if item.sha256 == arguments.review_sha256), None)
    if review is None:
        raise VerificationError("freeze_review_missing", STAGE)
    head, tree = head_tree(client.receipt, child_environment(os.environ), repository=arguments.repository_root)
    inputs.update(observed_head_commit=head, observed_tree=tree)
    return ReviewedCheckout(snapshot, review, SourceObservation(head_commit=head, tree=tree))


def _freeze_group(arguments: argparse.Namespace, client: GitHubClient, location: StoreLocation,
                  inputs: dict[str, object]) -> RecordGroup:
    """The freeze record of the strict snapshot's head for the selected review and the observed checkout."""
    checkout = _reviewed_checkout(arguments, client, location, inputs)
    snapshot = checkout.snapshot
    record = authorize_publication(
        candidate=snapshot.candidate, observed=checkout.observed, scope=checkout.scope, history=snapshot.history,
        review=checkout.review, evidence_parent=snapshot.head)
    return RecordGroup(snapshot.candidate.candidate_id,
                       (record_blob(EntryName(EntryFamily.PUBLICATION), encode_publication(record)),))


def _check(arguments: argparse.Namespace, client: GitHubClient, location: StoreLocation,
           inputs: dict[str, object]) -> dict[str, object]:
    """The canonical readiness verdict for the selected review and the clean checkout; nothing is appended."""
    checkout = _reviewed_checkout(arguments, client, location, inputs)
    snapshot = checkout.snapshot
    inputs["store_head"] = snapshot.head
    require_clean(client.receipt, child_environment(os.environ), repository=arguments.repository_root,
                  code="check_checkout_not_clean")
    readiness = evaluate_readiness(candidate=snapshot.candidate, observed=checkout.observed, scope=checkout.scope,
                                   history=snapshot.history, review=checkout.review)
    observation = {"kind": "readiness-check", "notice": CHECK_NOTICE, "ready": readiness.ready,
                   "scope": scope_record(checkout.scope),
                   "attempt_verdicts": [{"attempt": verdict.attempt, "state": verdict.state.value,
                                         "findings": [{"code": item.code, "subject": item.subject}
                                                      for item in verdict.findings]}
                                        for verdict in readiness.attempt_verdicts]}
    if not readiness.ready:
        return {**refused("publication_not_ready", READINESS_STAGE, readiness.findings), **observation}
    return {"disposition": "confirmed", "findings": [], **observation}


def _record(arguments: argparse.Namespace, client: GitHubClient, document: dict[str, object]) -> dict[str, object]:
    """Build the command's group, append it once and return the sanitized outcome; nothing here retries.

    A check returns its readiness observation instead and never reaches the append.
    """
    inputs: dict[str, object] = {}
    document["inputs"] = inputs
    try:
        location = StoreLocation(repository=arguments.store_repository, branch=arguments.store_branch,
                                 genesis_commit=arguments.store_genesis_commit,
                                 marker_blob=arguments.store_marker_blob)
        if arguments.command == "check":
            return _check(arguments, client, location, inputs)
        if arguments.command == "freeze":
            group = _freeze_group(arguments, client, location, inputs)
        else:
            group = _file_group(arguments, inputs)
        inputs["candidate_id"] = group.candidate_id
        outcome = append(client, location, group.candidate_id, group.additions, closure_resolutions=(),
                         work_directory=arguments.work_dir, observe_failure=report_unresolved)
    except VerificationError as error:
        return refusal(error)
    return disposition(outcome, stage=STAGE)


def _digest(value: str) -> str:
    if HEX_SHA256.fullmatch(value) is None:
        raise argparse.ArgumentTypeError("expected a lowercase SHA-256 hex digest")
    return value


def _parser() -> argparse.ArgumentParser:
    store = argparse.ArgumentParser(add_help=False)
    for name in ("store-repository", "store-branch", "store-genesis-commit", "store-marker-blob"):
        store.add_argument(f"--{name}", required=True)
    store.add_argument("--receipt", type=Path, required=True, help="a new file; an existing one is refused")
    # Only a command that may append has request bodies to keep.
    common = argparse.ArgumentParser(add_help=False, parents=[store])
    common.add_argument("--work-dir", type=Path, required=True)
    reviewed = argparse.ArgumentParser(add_help=False)
    reviewed.add_argument("--candidate-id", type=_digest, required=True)
    reviewed.add_argument("--review-sha256", type=_digest, required=True)
    reviewed.add_argument("--repository-root", type=Path, required=True)
    parser = argparse.ArgumentParser(description="Record one explicit development candidate store record.")
    commands = parser.add_subparsers(dest="command", required=True)
    register = commands.add_parser("register", parents=[common], help="a locator and the manifest it binds")
    register.add_argument("--locator", type=Path, required=True)
    register.add_argument("--manifest", type=Path, required=True)
    commands.add_parser("intent", parents=[common], help="an existing attempt intent").add_argument(
        "--intent", type=Path, required=True)
    commands.add_parser("triage", parents=[common], help="an existing attempt triage").add_argument(
        "--triage", type=Path, required=True)
    paired = commands.add_parser("install", parents=[common], help="an existing install proof and its witness")
    paired.add_argument("--proof", type=Path, required=True)
    paired.add_argument("--witness", type=Path, required=True)
    commands.add_parser("freeze", parents=[common, reviewed], help="the freeze of a ready candidate")
    commands.add_parser("check", parents=[store, reviewed],
                        help="observe a candidate's canonical readiness on a clean checkout; appends nothing")
    return parser


def main(argv: list[str] | None = None) -> int:
    """0 confirmed (a check: ready); 1 refused or failed; 2 usage (argparse); 3 ambiguous, with the work directory kept."""
    arguments = _parser().parse_args(argv)
    identity = CHECK_RECEIPT if arguments.command == "check" else RECEIPT
    return run_once(arguments.receipt, identity, {"command": arguments.command},
                    lambda client, document: _record(arguments, client, document))
