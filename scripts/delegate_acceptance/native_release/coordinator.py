"""Invocation stages compose existing verification, execution, and evidence owners."""

from __future__ import annotations

from scripts.delegate_acceptance.release_verification.assets import gh_environment
from scripts.delegate_acceptance.release_verification.github import GitHubClient
from .candidate import execute_candidate, resume_candidate
from .cleanup import cleanup
from .contract import CandidateRequest, ReleaseError, release_request
from .execution import archive, child_environment, download, finalize, native, observe_native
from .preflight import preflight
from .projection import locally_promotable, project, read_public
from .receipt import Receipt
from .submission import dispatch, upload
from .watching import watch


def github_client() -> GitHubClient:
    # Stored gh credentials remain owned by gh; never read or export a token.
    return GitHubClient({"commands": []}, gh_environment(auth_mode="local-config"))


def execute(receipt: Receipt) -> None:
    client = github_client()
    if isinstance(receipt.invocation.request, CandidateRequest):
        execute_candidate(receipt, client)
        return
    receipt.intend("preflight", {"request": release_request(receipt.invocation).document()})
    receipt.complete("preflight", preflight(receipt.invocation, client))
    download(receipt, client)
    env = child_environment(receipt.invocation)
    archive(receipt, env)
    native(receipt, env)
    finalize(receipt, env)
    project(receipt)
    submit(receipt, client)


def resume(receipt: Receipt) -> None:
    """No path from recovery calls download, archive verification, or native launch."""
    if isinstance(receipt.invocation.request, CandidateRequest):
        resume_candidate(receipt, github_client())
        return
    request = release_request(receipt.invocation)
    if receipt.stage("project").state != "complete":
        if receipt.stage("preflight").state != "complete" or receipt.stage("archive").state != "complete":
            raise ReleaseError("pre_native_invocation_incomplete_no_rerun", incomplete=True)
        if receipt.stage("native").state == "pending":
            raise ReleaseError("native_stage_not_executed_no_rerun", incomplete=True)
        observed = observe_native(receipt)
        receipt.complete("native", observed)
        env = child_environment(receipt.invocation)
        if receipt.stage("finalize").state != "complete":
            finalize(receipt, env)
        project(receipt)
    else:
        read_public(receipt)
    submit(receipt, None if request.no_submit else github_client())


def submit(receipt: Receipt, client: GitHubClient | None) -> None:
    if release_request(receipt.invocation).no_submit:
        cleanup(receipt)
        result = receipt.stage("project").result
        status = "FAIL" if result.get("case_results_verdict") == "FAIL" else "LOCAL_ONLY"
        verdict = str(result.get("case_results_verdict", "UNRESOLVED")).lower()
        receipt.report(status, f"no_submit_native_{verdict}_not_promotion_authority")
        return
    if client is None:
        raise ReleaseError("github_submission_client_missing")
    upload(receipt, client)
    if receipt.stage("dispatch").state != "complete":
        dispatch(receipt, client)
    if receipt.stage("watch").state != "complete":
        watch(receipt, client)
    if locally_promotable(receipt) and receipt.stage("watch").result.get("promotable") is True:
        cleanup(receipt)
        receipt.report("PROMOTABLE", "hosted_native_proof_and_history_validated")
    else:
        native_failed = receipt.stage("project").result.get("case_results_verdict") == "FAIL"
        receipt.report("FAIL" if native_failed else "BLOCKED", "native_validation_not_promotable")
