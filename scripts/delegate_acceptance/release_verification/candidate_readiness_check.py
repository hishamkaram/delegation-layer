"""Trusted-main observation of one pull request's development readiness, posted as a commit status.

One invocation, from the reviewed main workflow only, after its receipt is
reserved and every local input, the run context and the executing harness are
validated:

1. GET the requested pull request of the configured repository. Its number,
   base repository and ``main`` base must match; ``head.repo`` may be null. Its
   ``head.sha`` is B. A failed or foreign identity posts nothing: there is no
   verified target to post to.
2. GET ``git/commits/B``; its identity must be B and its tree is T.
3. POST ``pending`` on B under the fixed context ``Development readiness``.
4. Only ``open`` with ``merged`` false or ``closed`` with ``merged`` true is a
   valid pull request; a closed, unmerged one is a definite denial and any
   other combination is an invalid observation.
5. Read the candidate store head once through the canonical
   ``load_candidate_at_branch_head``. Omitting all four store options selects the
   reviewed production store; supplying any one requires all four. The candidate must be present and frozen
   by exactly the requested FREEZE commit F; the stored review selected by the
   publication digest supplies the scope for the canonical
   ``evaluate_readiness`` of actual B and T.
6. POST the terminal state on the same B: ``success`` only for a canonical
   ready verdict, ``failure`` for a definite denial, ``error`` otherwise.

Every acknowledgment must carry a positive integer id, the exact requested
state, context and run URL, and the exact API URL of B. A failed or malformed
POST stops the run with delivery unknown: no retry, no readback and no second
POST to repair it. The pull request is read once; the result concerns B, T and
the store head as observed and says nothing about a later head. A merged pull request is an
after-merge observation, never pre-merge enforcement. No rule or ruleset is
read, so branch protection stays unverified, and nothing here grants a merge,
freeze or publication.
"""

from __future__ import annotations

import argparse
from collections.abc import Mapping
from dataclasses import dataclass
import json
import os
from pathlib import Path

from scripts.delegate_acceptance.native_release.contract import PRODUCTION_STORE
from .assets import gh_environment
from .candidate_attempts import ReviewedSource, scope_record
from .candidate_history import FindingRef
from .candidate_readiness import SourceObservation, evaluate_readiness
from .candidate_store import load_candidate_at_branch_head
from .candidate_store_contract import GIT_OBJECT_RE, StoreLocation, candidate_path
from .candidate_store_replay import CandidateSnapshot, ReplayDenied
from .contract import VerificationError
from .filesystem import create_new_file, write_receipt
from .github import GitHubClient, object_value
from .source import observe_harness


STAGE = "check-candidate-readiness"


KIND = "development-readiness-check-receipt"


STATUS_CONTEXT = "Development readiness"


WORKFLOW_PATH = ".github/workflows/development-readiness.yml"


TRUSTED_REF = "refs/heads/main"


BASE_REF = "main"


SERVER_URL = "https://github.com"


API_URL = "https://api.github.com"


# The executing entry point and this module; the fingerprint follows their static imports.
HARNESS_SEEDS = ("scripts/check_candidate_readiness.py",
                 "scripts/delegate_acceptance/release_verification/candidate_readiness_check.py")


# All four omitted select ``PRODUCTION_STORE``; any one supplied requires all four.
STORE_OPTIONS = ("store-repository", "store-branch", "store-genesis-commit", "store-marker-blob")


# A pull request, commit or status reply is a few KiB.
MAX_REPLY_BYTES = 256 * 1024


MAX_RECEIPT_BYTES = 1024 * 1024


# Non-secret run identity; nothing else from the environment enters the receipt.
RUN_FIELDS = ("GITHUB_REPOSITORY", "GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_WORKFLOW_REF", "GITHUB_SHA",
              "GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT", "GITHUB_SERVER_URL", "GITHUB_API_URL")


# Facts this observation cannot establish; a consumer must not read them into the result.
UNRESOLVED_FACTS = (
    "branch_protection_unverified",
    "pull_request_head_after_observation_unverified",
    "merge_commit_not_observed",
)


NOTICE = ("Observation only: exit 0 means canonical readiness held for the observed pull request head B, its "
          "tree T and the candidate store head H, and its success status on B was acknowledged. It is not "
          "branch protection, merge or publication authority; a merged pull request is an after-merge "
          "observation, never pre-merge enforcement. This receipt is final only when the run's printed summary "
          "reports receipt_finalized true.")


EXIT_READY, EXIT_INCOMPLETE, EXIT_DENIED = 0, 1, 3


SUCCESS, FAILURE, ERROR = "success", "failure", "error"


RESULTS = {EXIT_READY: "ready", EXIT_INCOMPLETE: "incomplete", EXIT_DENIED: "denied"}


VALID_PULL_REQUEST_STATES = (("open", False), ("closed", True))


def _refuse(code: str) -> VerificationError:
    return VerificationError(code, STAGE)


def _failure(error: VerificationError) -> dict[str, object]:
    return {"code": error.code, "stage": error.stage}


@dataclass(frozen=True)
class Request:
    """The validated dispatch: one pull request, candidate and FREEZE commit in one pinned store."""

    pull_request: int
    candidate_id: str
    freeze_commit: str
    store: StoreLocation


@dataclass(frozen=True)
class RunContext:
    """The trusted run's commit and the canonical run URL every status names."""

    harness_commit: str
    target_url: str


@dataclass(frozen=True)
class Target:
    """The pull request's actual head commit B and its tree T: the only status target."""

    commit: str
    tree: str


@dataclass(frozen=True)
class Decision:
    """The terminal status state and the exit code it implies."""

    state: str
    reason: str
    exit_code: int


def missing_store_options(args: argparse.Namespace) -> list[str]:
    """The unset store options once any one is supplied; an empty string is supplied, not unset."""
    missing = [f"--{name}" for name in STORE_OPTIONS if getattr(args, name.replace("-", "_")) is None]
    return [] if len(missing) == len(STORE_OPTIONS) else missing


def parse_request(args: argparse.Namespace) -> Request:
    """Validate the dispatch inputs and the selected store before any read; a partial store refuses."""
    if args.pull_request <= 0:
        raise _refuse("pull_request_number_invalid")
    candidate_path(args.candidate_id)
    if GIT_OBJECT_RE.fullmatch(args.freeze_commit) is None:
        raise _refuse("freeze_commit_invalid")
    if missing_store_options(args):
        raise _refuse("store_options_incomplete")
    store = PRODUCTION_STORE if args.store_repository is None else StoreLocation(
        repository=args.store_repository, branch=args.store_branch,
        genesis_commit=args.store_genesis_commit, marker_blob=args.store_marker_blob)
    return Request(pull_request=args.pull_request, candidate_id=args.candidate_id,
                   freeze_commit=args.freeze_commit, store=store)


def _positive_decimal(value: str) -> bool:
    return value.isascii() and value.isdigit() and not value.startswith("0")


def run_context(environment: Mapping[str, str], repository: str) -> tuple[dict[str, bool], RunContext | None]:
    """Compare the runner context with a main-branch dispatch of this workflow in the store repository."""
    run_id, commit = environment.get("GITHUB_RUN_ID", ""), environment.get("GITHUB_SHA", "")
    checks = {
        "repository_matches": environment.get("GITHUB_REPOSITORY") == repository,
        "event_is_workflow_dispatch": environment.get("GITHUB_EVENT_NAME") == "workflow_dispatch",
        "ref_is_main": environment.get("GITHUB_REF") == TRUSTED_REF,
        "workflow_ref_matches": environment.get("GITHUB_WORKFLOW_REF") == f"{repository}/{WORKFLOW_PATH}@{TRUSTED_REF}",
        "run_id_valid": _positive_decimal(run_id),
        "run_attempt_valid": _positive_decimal(environment.get("GITHUB_RUN_ATTEMPT", "")),
        "sha_valid": GIT_OBJECT_RE.fullmatch(commit) is not None,
        "server_url_matches": environment.get("GITHUB_SERVER_URL") == SERVER_URL,
        "api_url_matches": environment.get("GITHUB_API_URL") == API_URL,
    }
    if not all(checks.values()):
        return checks, None
    return checks, RunContext(harness_commit=commit, target_url=f"{SERVER_URL}/{repository}/actions/runs/{run_id}")


def checkout_root() -> Path:
    """The checkout this module executes from, never a caller-supplied path."""
    return Path(__file__).resolve().parents[3]


def bind_harness(receipt: dict, environment: Mapping[str, str], run: RunContext) -> None:
    """Require the executing source closure to be exactly the run's committed main harness."""
    head, fingerprint, committed = observe_harness(receipt, environment, checkout=checkout_root(),
                                                   seeds=HARNESS_SEEDS)
    receipt["harness"] = {"commit": head, "sha256": fingerprint, "committed": committed}
    if head != run.harness_commit:
        raise _refuse("harness_commit_mismatch")
    if not committed:
        raise _refuse("harness_uncommitted")


def observe_pull_request(client: GitHubClient, request: Request) -> tuple[dict[str, object], str]:
    """The single pull request read: its identity, actual head B and state as the API returned them.

    ``state`` and ``merged`` are recorded unjudged; ``pull_request_state`` decides them
    once a verified target can carry the verdict.
    """
    reply = object_value(client.json(f"repos/{request.store.repository}/pulls/{request.pull_request}",
                                     limit=MAX_REPLY_BYTES), STAGE)
    base, head = object_value(reply.get("base"), STAGE), object_value(reply.get("head"), STAGE)
    base_repository = object_value(base.get("repo"), STAGE)
    if (type(reply.get("number")) is not int or reply.get("number") != request.pull_request
            or base_repository.get("full_name") != request.store.repository or base.get("ref") != BASE_REF):
        raise _refuse("pull_request_identity_mismatch")
    commit = head.get("sha")
    if not isinstance(commit, str) or GIT_OBJECT_RE.fullmatch(commit) is None:
        raise _refuse("pull_request_head_invalid")
    head_repository = head.get("repo")
    head_name = head_repository.get("full_name") if isinstance(head_repository, dict) else None
    return {"number": request.pull_request, "base_repository": request.store.repository, "base_ref": BASE_REF,
            "head_sha": commit, "head_repository": head_name if isinstance(head_name, str) else None,
            "state": reply.get("state"), "merged": reply.get("merged")}, commit


def observe_target(client: GitHubClient, repository: str, commit: str) -> Target:
    """Read B's own commit object; its identity must be B and its tree is T."""
    reply = object_value(client.json(f"repos/{repository}/git/commits/{commit}", limit=MAX_REPLY_BYTES), STAGE)
    tree = reply.get("tree")
    tree_sha = tree.get("sha") if isinstance(tree, dict) else None
    if reply.get("sha") != commit or not isinstance(tree_sha, str) or GIT_OBJECT_RE.fullmatch(tree_sha) is None:
        raise _refuse("status_target_mismatch")
    return Target(commit=commit, tree=tree_sha)


def pull_request_state(observed: Mapping[str, object]) -> Decision | None:
    """None for a valid open or merged pull request; otherwise the terminal decision it forces."""
    state, merged = observed.get("state"), observed.get("merged")
    if type(state) is str and type(merged) is bool and (state, merged) in VALID_PULL_REQUEST_STATES:
        return None
    if state == "closed" and merged is False:
        return Decision(FAILURE, "pull_request_closed_unmerged", EXIT_DENIED)
    return Decision(ERROR, "pull_request_state_invalid", EXIT_INCOMPLETE)


def ack_mismatches(reply: object, request: Mapping[str, str], api_url: str) -> list[str]:
    """Every acknowledgment field that differs from what was sent; the API URL must be exact, not a suffix."""
    if not isinstance(reply, dict):
        return ["reply"]
    found = [name for name in ("state", "context", "target_url") if reply.get(name) != request[name]]
    if reply.get("url") != api_url:
        found.append("url")
    if type(reply.get("id")) is not int or reply["id"] <= 0:
        found.append("id")
    return found


def post_status(client: GitHubClient, repository: str, target: Target, run: RunContext, state: str,
                body_path: Path) -> dict[str, object]:
    """Send exactly one status POST on B; anything but a matching acknowledgment is unknown delivery."""
    endpoint = f"repos/{repository}/statuses/{target.commit}"
    request = {"state": state, "context": STATUS_CONTEXT, "target_url": run.target_url}
    record: dict[str, object] = {**request, "commit": target.commit, "delivery": "unknown"}
    try:
        reply = client.json_body(endpoint, dict(request), method="POST", body_path=body_path,
                                 limit=MAX_REPLY_BYTES)
    except VerificationError as error:
        record["failure"] = _failure(error)
        return record
    mismatches = ack_mismatches(reply, request, f"{API_URL}/{endpoint}")
    if mismatches:
        record["ack_mismatches"] = mismatches
        return record
    record.update(delivery="acknowledged", id=reply["id"])
    return record


def _snapshot_facts(snapshot: CandidateSnapshot) -> dict[str, object]:
    source = snapshot.candidate.manifest.source
    publication = snapshot.history.publication
    return {"head": snapshot.head, "candidate_id": snapshot.candidate.candidate_id,
            "freeze_commit": snapshot.freeze_commit,
            "publication_sha256": None if publication is None else publication.sha256,
            "candidate_source": {"commit": source.commit, "tree": source.tree, "base_commit": source.base_commit},
            "candidate_build_harness_commit": snapshot.candidate.manifest.build.harness_commit}


def _findings(findings: tuple[FindingRef, ...]) -> list[dict[str, str]]:
    return [{"code": item.code, "subject": item.subject} for item in findings]


def selected_review(snapshot: CandidateSnapshot) -> ReviewedSource:
    """The one stored review the freeze record names; replay already bound it, so any other count is integrity."""
    publication = snapshot.history.publication
    if publication is None:
        raise _refuse("candidate_unfrozen")
    matches = [review for review in snapshot.reviews if review.sha256 == publication.review_sha256]
    if len(matches) != 1:
        raise _refuse("selected_review_invalid")
    return matches[0]


def evaluate(client: GitHubClient, request: Request, target: Target, receipt: dict[str, object]) -> Decision:
    """Read H once, select the frozen evidence and return the canonical verdict as a decision."""
    try:
        snapshot = load_candidate_at_branch_head(client, request.store, request.candidate_id,
                                                 closure_resolutions=())
    except ReplayDenied as error:
        receipt["store_failure"] = {**_failure(error), "findings": _findings(error.findings)}
        return Decision(ERROR, "store_replay_denied", EXIT_INCOMPLETE)
    except VerificationError as error:
        receipt["store_failure"] = _failure(error)
        return Decision(ERROR, "store_unavailable", EXIT_INCOMPLETE)
    if snapshot is None:
        receipt["store"] = {"absent": True}
        return Decision(FAILURE, "candidate_absent", EXIT_DENIED)
    receipt["store"] = _snapshot_facts(snapshot)
    if snapshot.freeze_commit is None:
        return Decision(FAILURE, "candidate_unfrozen", EXIT_DENIED)
    if snapshot.freeze_commit != request.freeze_commit:
        return Decision(FAILURE, "freeze_commit_mismatch", EXIT_DENIED)
    try:
        review = selected_review(snapshot)
        receipt["selected_review_sha256"] = review.sha256
        scope = review.record.reviewed_scope.scope
        receipt["scope"] = scope_record(scope)
        readiness = evaluate_readiness(candidate=snapshot.candidate,
                                       observed=SourceObservation(head_commit=target.commit, tree=target.tree),
                                       scope=scope, history=snapshot.history, review=review)
    except VerificationError as error:
        receipt["evaluation_failure"] = _failure(error)
        return Decision(ERROR, "evaluation_failed", EXIT_INCOMPLETE)
    receipt["readiness"] = {"ready": readiness.ready, "review_sha256": readiness.review_sha256,
                            "findings": _findings(readiness.findings)}
    if readiness.ready:
        return Decision(SUCCESS, "ready", EXIT_READY)
    return Decision(FAILURE, "readiness_denied", EXIT_DENIED)


def observe(client: GitHubClient, request: Request, run: RunContext, output: Path,
            receipt: dict[str, object]) -> int:
    """Run the protocol once on one target B; the exit code is the terminal decision when delivered."""
    repository = request.store.repository
    try:
        receipt["pull_request"], commit = observe_pull_request(client, request)
        target = observe_target(client, repository, commit)
    except VerificationError as error:
        receipt["remote_failure"] = _failure(error)
        return EXIT_INCOMPLETE
    receipt["target"] = {"commit": target.commit, "tree": target.tree}
    statuses: dict[str, object] = {}
    receipt["statuses"] = statuses
    statuses["pending"] = pending = post_status(client, repository, target, run, "pending",
                                                output / "status-pending-body.json")
    if pending["delivery"] != "acknowledged":
        return EXIT_INCOMPLETE
    decision = pull_request_state(receipt["pull_request"]) or evaluate(client, request, target, receipt)
    receipt["decision"] = {"state": decision.state, "reason": decision.reason}
    statuses["terminal"] = terminal = post_status(client, repository, target, run, decision.state,
                                                  output / "status-terminal-body.json")
    if terminal["delivery"] != "acknowledged":
        return EXIT_INCOMPLETE
    return decision.exit_code


def new_receipt(environment: Mapping[str, str], commands: list[dict[str, object]]) -> dict[str, object]:
    return {
        "schema_version": 1, "kind": KIND, "notice": NOTICE, "commands": commands,
        "run": {name: environment.get(name) for name in RUN_FIELDS}, "status_context": STATUS_CONTEXT,
        "protection_unverified": True, "unresolved_facts": list(UNRESOLVED_FACTS),
        "result": RESULTS[EXIT_INCOMPLETE], "exit_code": None,
    }


def reserve_receipt(path: Path) -> None:
    """Create the new, empty receipt before any request; an existing path is refused and left untouched."""
    descriptor = create_new_file(path, "readiness_receipt_path_invalid", stage=STAGE)
    try:
        os.close(descriptor)
    except OSError:
        raise _refuse("readiness_receipt_reservation_failed") from None


def finalize_receipt(path: Path, receipt: dict[str, object]) -> dict[str, object] | None:
    """Replace the reservation with the final record once; None on success, otherwise the failure."""
    try:
        write_receipt(path, receipt, max_bytes=MAX_RECEIPT_BYTES)
    except VerificationError as error:
        return _failure(error)
    except OSError:
        return {"code": "readiness_receipt_write_failed", "stage": STAGE}
    return None


def prepare(args: argparse.Namespace, environment: Mapping[str, str],
            receipt: dict[str, object]) -> tuple[GitHubClient, Request, RunContext]:
    """Validate every local input, the run and the executing harness before any request."""
    request = parse_request(args)
    receipt["request"] = {"pull_request": request.pull_request, "candidate_id": request.candidate_id,
                          "freeze_commit": request.freeze_commit,
                          "store": {"repository": request.store.repository, "branch": request.store.branch,
                                    "genesis_commit": request.store.genesis_commit,
                                    "marker_blob": request.store.marker_blob}}
    checks, run = run_context(environment, request.store.repository)
    receipt["context_checks"] = checks
    if run is None:
        raise _refuse("run_context_mismatch")
    bind_harness(receipt, environment, run)
    return GitHubClient(receipt, gh_environment(auth_mode="environment")), request, run


def status_deliveries(receipt: Mapping[str, object]) -> dict[str, object]:
    """Each attempted status POST's delivery and acknowledged id, for the printed summary."""
    statuses = receipt.get("statuses")
    if not isinstance(statuses, dict):
        return {}
    return {name: {"state": record.get("state"), "delivery": record.get("delivery"), "id": record.get("id")}
            for name, record in statuses.items() if isinstance(record, dict)}


def _parser() -> argparse.ArgumentParser:
    # The store options are the maintainer CLIs' names; no abbreviation may stand in for one.
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter,
                                     allow_abbrev=False)
    parser.add_argument("--pull-request", type=int, required=True)
    parser.add_argument("--candidate-id", required=True)
    parser.add_argument("--freeze-commit", required=True)
    for name in STORE_OPTIONS:
        parser.add_argument(f"--{name}")
    parser.add_argument("--output-dir", type=Path, required=True)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = _parser()
    args = parser.parse_args(argv)
    missing = missing_store_options(args)
    if missing:
        parser.error(f"a store override requires all four store options; missing: {' '.join(missing)}")
    commands: list[dict[str, object]] = []
    receipt = new_receipt(os.environ, commands)
    path = args.output_dir / "receipt.json"
    try:
        reserve_receipt(path)
    except VerificationError as error:
        print(json.dumps({"result": "receipt_unavailable", "code": error.code, "exit_code": EXIT_INCOMPLETE,
                          "receipt_finalized": False}, sort_keys=True))
        return EXIT_INCOMPLETE
    code = EXIT_INCOMPLETE
    try:
        try:
            client, request, run = prepare(args, os.environ, receipt)
        except VerificationError as error:
            receipt["setup_failure"] = _failure(error)
        else:
            code = observe(client, request, run, args.output_dir, receipt)
    except BaseException as error:
        receipt["internal_failure"] = type(error).__name__
        raise
    finally:
        receipt.update(result=RESULTS[code], exit_code=code)
        receipt_failure = finalize_receipt(path, receipt)
    observed_result = RESULTS[code]
    if receipt_failure is not None:
        code = EXIT_INCOMPLETE
    # The trusted main harness and the candidate's own source and build stay separate facts.
    summary = {key: receipt.get(key)
               for key in ("setup_failure", "remote_failure", "decision", "harness", "target", "store")}
    summary.update(result=RESULTS[code], exit_code=code, observed_result=observed_result,
                   receipt_finalized=receipt_failure is None, receipt_failure=receipt_failure,
                   status_deliveries=status_deliveries(receipt))
    print(json.dumps(summary, sort_keys=True))
    return code
