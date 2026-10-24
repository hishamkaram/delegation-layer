"""Root publication of one frozen candidate: preflight, prepare, one dispatch, observe, verify, cleanup.

Owner: the invocation lock holder writes this receipt's six stages; readers are
the CLI report and a later explicit resume. The trusted-main hosted job alone
writes destinations; Root downloads no candidate payload and runs no package,
provider or install work. Invariants:

- the dispatch intent, with its exact title, inputs and H, is durable before
  the one POST; an unknown acknowledgement stops, and nothing here ever
  dispatches again;
- an explicit resume only observes: a pending dispatch proves nothing was
  sent, and an intended one binds only a unique exact run in the complete
  inventory;
- PUBLISHED needs the canonical finalized hosted success with exact bindings
  and delivery, a successful run attempt, and a completed local cleanup stage.

H is the clean committed protected-main checkout running this command; the
selected readiness run keeps its own harness and the store head may move only
as the shared preflight allows.
"""

from __future__ import annotations

from pathlib import Path

from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.release_verification.candidate_build_source import child_environment, git
from scripts.delegate_acceptance.release_verification.candidate_merge_observation import (
    observe_publication_source, observe_required_checks,
)
from scripts.delegate_acceptance.release_verification.candidate_publication_contract import (
    CandidatePromotion, encode_promotion,
)
from scripts.delegate_acceptance.release_verification.candidate_publication_preflight import (
    PublicationContext, preflight,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.github import GitHubClient
from scripts.delegate_acceptance.release_verification.source import observe_harness
from .contract import (
    PublicationRequest, ReleaseError, integer, object_value, parse_workflow_dispatch_ack, publication_request,
    revision_value, text_value,
)
from .preflight import require_python
from .publication_contract import (
    PUBLICATION_WORKFLOW, PublicationDispatch, ReportBinding, require_failed, require_public_repository,
    require_published,
)
from .publication_observation import OBSERVED_ATTEMPT, find_dispatched_run, observe_run, read_report
from .receipt import Receipt


# The executed entry point; the fingerprint follows its whole maintained import closure.
PUBLICATION_SEEDS = ("scripts/release_native.py",)
# Root keeps every intermediate value in its receipt or in memory, never in scratch files.
NO_SCRATCH: dict[str, object] = {"root_scratch": []}


def execute_publication(receipt: Receipt, client: GitHubClient) -> None:
    request = publication_request(receipt.invocation)
    receipt.intend("preflight", {"request": request.document()})
    promotion, facts = _preflight(receipt.invocation.checkout, request, client)
    receipt.complete("preflight", facts)
    dispatch = _prepare(receipt)
    _dispatch(receipt, client, dispatch)
    _observe_and_verify(receipt, client, dispatch, promotion)


def resume_publication(receipt: Receipt, client: GitHubClient) -> None:
    """Observe only; a dispatch never intended is a definite refusal and nothing is ever sent."""
    request = publication_request(receipt.invocation)
    if receipt.stage("cleanup").state == "complete":
        _report_final(receipt)
        return
    if receipt.stage("dispatch").state == "pending":
        receipt.report("BLOCKED", "publication_never_dispatched_start_a_new_invocation")
        return
    dispatch = _prepared_dispatch(receipt)
    if receipt.stage("dispatch").state == "intended":
        row = find_dispatched_run(client, dispatch)
        if row is None:
            raise ReleaseError("publication_dispatched_run_not_found_no_redispatch", incomplete=True)
        receipt.complete("dispatch", _run_facts(integer(row.get("id"), minimum=1), dispatch, "inventory"))
    promotion = None
    if receipt.stage("verify").state != "complete":
        _, promotion = _promotion(client, request)
        if sha(encode_promotion(promotion)) != receipt.stage("preflight").result.get("promotion_sha256"):
            raise ReleaseError("publication_promotion_changed", incomplete=True)
    _observe_and_verify(receipt, client, dispatch, promotion)


def publication_status(receipt: Receipt) -> str:
    """BLOCKED only while no dispatch was ever intended; after that every unresolved fault is INCOMPLETE."""
    return "BLOCKED" if receipt.stage("dispatch").state == "pending" else "INCOMPLETE"


def _promotion(client: GitHubClient, request: PublicationRequest) -> tuple[PublicationContext, CandidatePromotion]:
    """Fresh strict-store preflight, then the canonical merge observation and promotion statement."""
    context = preflight(client, request, verify_store_protection=True)
    freeze = context.snapshot.history.publication
    if freeze is None:
        raise ReleaseError("publication_freeze_missing")
    source = observe_publication_source(client, candidate=context.snapshot.candidate,
                                        pull_request=context.pull_request,
                                        merged_commit=request.reference.merged_commit)
    return context, CandidatePromotion(reference=request.reference, candidate=context.snapshot.candidate,
                                       freeze=freeze, source=source)


def _preflight(checkout: Path, request: PublicationRequest,
               client: GitHubClient) -> tuple[CandidatePromotion, dict[str, object]]:
    require_python()
    context, promotion = _promotion(client, request)
    checks = observe_required_checks(client, context=context)
    require_public_repository(context.repository_private)
    harness, harness_sha256 = _publisher_harness(client, checkout, request.repository)
    return promotion, {
        "harness": harness, "harness_sha256": harness_sha256, "readiness_harness": context.harness_commit,
        "readiness_artifact_sha256": context.artifact.sha256, "store_head": context.snapshot.head,
        "pull_request": context.pull_request,
        "target": {"commit": context.target.commit, "tree": context.target.tree},
        "merged_tree": promotion.source.tree, "promotion_sha256": sha(encode_promotion(promotion)),
        "required_checks": [check.context for check in checks.checks], "repository_private": False,
    }


def _publisher_harness(client: GitHubClient, checkout: Path, repository: str) -> tuple[str, str]:
    """H: this checkout's committed, clean HEAD, which must be protected main now; and its fingerprint."""
    environment = client.environment
    head, fingerprint, committed = observe_harness(client.receipt, environment, checkout=checkout,
                                                   seeds=PUBLICATION_SEEDS)
    dirty = git(client.receipt, child_environment(environment), "publication-git-status",
                ["status", "--porcelain", "--untracked-files=all"], cwd=checkout)
    branch = object_value(client.json(f"repos/{repository}/branches/main"))
    main = revision_value(object_value(branch.get("commit")).get("sha"))
    if not committed or dirty or branch.get("protected") is not True or head != main:
        raise ReleaseError("publication_requires_clean_current_main_harness")
    return head, fingerprint


def _prepared_document(dispatch: PublicationDispatch) -> dict[str, object]:
    return {"title": dispatch.title, "request_sha256": dispatch.request_sha256, "harness": dispatch.harness,
            "inputs": dispatch.inputs()}


def _dispatch_intent(dispatch: PublicationDispatch) -> dict[str, object]:
    return {"workflow": PUBLICATION_WORKFLOW, "ref": "main", "title": dispatch.title, "inputs": dispatch.inputs()}


def _prepare(receipt: Receipt) -> PublicationDispatch:
    """The immutable request data of the one dispatch; nothing is written outside the receipt."""
    harness = text_value(receipt.stage("preflight").result.get("harness"))
    receipt.intend("prepare", {"harness": harness})
    dispatch = PublicationDispatch(request=publication_request(receipt.invocation),
                                   invocation_id=receipt.invocation.invocation_id, harness=harness)
    receipt.complete("prepare", _prepared_document(dispatch))
    return dispatch


def _prepared_dispatch(receipt: Receipt) -> PublicationDispatch:
    """Rebuild the persisted dispatch; its prepared data and dispatch intent must be exactly what was stored."""
    prepared = receipt.stage("prepare")
    if prepared.state != "complete":
        raise ReleaseError("publication_prepared_dispatch_mismatch", incomplete=True)
    dispatch = PublicationDispatch(request=publication_request(receipt.invocation),
                                   invocation_id=receipt.invocation.invocation_id,
                                   harness=text_value(prepared.intent.get("harness")))
    if (prepared.result != _prepared_document(dispatch)
            or receipt.stage("dispatch").intent != _dispatch_intent(dispatch)):
        raise ReleaseError("publication_prepared_dispatch_mismatch", incomplete=True)
    return dispatch


def _run_facts(run_id: int, dispatch: PublicationDispatch, bound_by: str) -> dict[str, object]:
    repository = dispatch.request.repository
    return {"run_id": run_id, "html_url": f"https://github.com/{repository}/actions/runs/{run_id}",
            "run_url": f"https://api.github.com/repos/{repository}/actions/runs/{run_id}", "bound_by": bound_by}


def _dispatch(receipt: Receipt, client: GitHubClient, dispatch: PublicationDispatch) -> None:
    """Persist the intent, then POST once; any doubt about the reply stops for an observation-only resume."""
    repository = dispatch.request.repository
    if not receipt.intend("dispatch", _dispatch_intent(dispatch)):
        raise ReleaseError("publication_dispatch_already_intended", incomplete=True)
    fields = {"ref": "main", **{f"inputs[{name}]": value for name, value in dispatch.inputs().items()}}
    try:
        ack = parse_workflow_dispatch_ack(client.json(
            f"repos/{repository}/actions/workflows/{PUBLICATION_WORKFLOW}/dispatches", method="POST",
            fields=fields), repository)
    except (VerificationError, ReleaseError) as error:
        raise ReleaseError("publication_dispatch_unacknowledged_resume_observes_only",
                           incomplete=True) from error
    receipt.complete("dispatch", _run_facts(ack.run_id, dispatch, "acknowledgement"))


def _observe_and_verify(receipt: Receipt, client: GitHubClient, dispatch: PublicationDispatch,
                        promotion: CandidatePromotion | None) -> None:
    run_id = integer(receipt.stage("dispatch").result.get("run_id"), minimum=1)
    if receipt.stage("observe").state != "complete":
        receipt.intend("observe", {"run_id": run_id, "run_attempt": OBSERVED_ATTEMPT})
        receipt.complete("observe", observe_run(client, dispatch, run_id))
    if receipt.stage("verify").state != "complete":
        if promotion is None:
            raise ReleaseError("publication_promotion_unavailable", incomplete=True)
        binding = ReportBinding.of(dispatch, run_id=run_id, run_attempt=OBSERVED_ATTEMPT)
        receipt.intend("verify", {"binding": binding.document(),
                                  "promotion_sha256": sha(encode_promotion(promotion))})
        gate, artifact = read_report(client, dispatch, run_id=run_id, run_attempt=OBSERVED_ATTEMPT)
        receipt.complete("verify", _verdict(gate, binding, promotion, receipt.stage("observe").result, artifact))
    receipt.intend("cleanup", NO_SCRATCH)
    receipt.complete("cleanup", NO_SCRATCH)
    _report_final(receipt)


def _verdict(gate: dict[str, object], binding: ReportBinding, promotion: CandidatePromotion,
             observed: dict[str, object], artifact: dict[str, object]) -> dict[str, object]:
    """The canonical validator chosen by the hosted verdict decides; a status string alone grants nothing."""
    if gate.get("verdict") == "pass":
        report = require_published(gate, binding=binding, promotion=promotion)
        if observed.get("conclusion") != "success":
            raise ReleaseError("publication_run_conclusion_conflict", incomplete=True)
        return {"outcome": "published", "artifact": artifact, "delivery": report["delivery"]}
    failure = require_failed(gate, binding=binding, promotion=promotion)
    return {"outcome": "failed", "artifact": artifact,
            "failure": {"code": failure.code, "stage": failure.stage,
                        "first_mutation_started": failure.first_mutation_started,
                        "delivery_confirmed": failure.delivery_confirmed}}


def _report_final(receipt: Receipt) -> None:
    """A verified success is PUBLISHED; a hosted failure is BLOCKED only if it started no mutation."""
    verdict = receipt.stage("verify").result
    if verdict.get("outcome") == "published":
        receipt.report("PUBLISHED", "hosted_delivery_verified_and_receipt_finalized")
        return
    failure = object_value(verdict.get("failure"))
    status = "BLOCKED" if failure.get("first_mutation_started") is False else "INCOMPLETE"
    receipt.report(status, f"hosted_publication_failed:{text_value(failure.get('stage'))}:"
                           f"{text_value(failure.get('code'))}")
