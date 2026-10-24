"""Create-once release submission with exact remote observation after uncertainty."""

from __future__ import annotations

import hashlib

from scripts.delegate_acceptance.native_evidence.public_contract import MAX_PUBLIC_ZIP_BYTES
from scripts.delegate_acceptance.release_verification.commands import command
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.github import (
    ASSETS_STAGE, GitHubClient, iter_release_assets,
)
from scripts.delegate_acceptance.release_verification.native_history import proof_title
from scripts.delegate_acceptance.release_verification.native_remote_history import matching_runs
from scripts.delegate_acceptance.release_verification.native_reports import ReportScope
from .contract import (
    ReleaseError, WORKFLOW, WORKFLOW_RUN_PATHS, digest_value, integer, object_value,
    parse_workflow_dispatch_ack, text_value,
)
from .preflight import recheck_submission
from .projection import read_public
from .receipt import Receipt


# The shared listing's own refusals keep this receipt's legacy resumable identities.
_LEGACY_ASSET_CODES = {
    ("github_release_assets_invalid", ASSETS_STAGE): "release_assets_invalid",
    ("github_release_assets_pagination_incomplete", ASSETS_STAGE): "release_assets_pagination_incomplete",
}


def _asset_matches(receipt: Receipt, client: GitHubClient) -> list[dict[str, object]]:
    invocation = receipt.invocation
    release_id = integer(receipt.stage("preflight").result.get("release_id"), minimum=1)
    found: list[dict[str, object]] = []
    try:
        for value in iter_release_assets(client, invocation.request.repo, release_id):
            item = object_value(value)
            if item.get("name") == invocation.proof_name:
                found.append(item)
    except VerificationError as error:
        legacy = _LEGACY_ASSET_CODES.get((error.code, error.stage))
        if legacy is None:
            raise
        raise ReleaseError(legacy, incomplete=True) from error
    return found


def reconcile_asset(receipt: Receipt, client: GitHubClient) -> dict[str, object] | None:
    matches = _asset_matches(receipt, client)
    if not matches:
        return None
    if len(matches) != 1:
        raise ReleaseError("proof_asset_name_ambiguous", incomplete=True)
    asset = matches[0]
    proof = receipt.stage("project").result
    asset_id = integer(asset.get("id"), minimum=1)
    size = integer(asset.get("size"), minimum=1)
    digest = digest_value(proof.get("proof_sha256"))
    uploader = object_value(asset.get("uploader"))
    if size != proof.get("size") or asset.get("state") != "uploaded":
        raise ReleaseError("proof_asset_identity_mismatch", incomplete=True)
    if uploader.get("id") != receipt.stage("preflight").result.get("actor_id"):
        raise ReleaseError("proof_uploader_actor_mismatch", incomplete=True)
    remote_digest = asset.get("digest")
    if remote_digest is not None and remote_digest != f"sha256:{digest}":
        raise ReleaseError("proof_asset_digest_mismatch", incomplete=True)
    data = client.bytes(f"repos/{receipt.invocation.request.repo}/releases/assets/{asset_id}",
                        limit=MAX_PUBLIC_ZIP_BYTES)
    if len(data) != size or hashlib.sha256(data).hexdigest() != digest:
        raise ReleaseError("proof_asset_bytes_mismatch", incomplete=True)
    return {"asset_id": asset_id, "proof_sha256": digest, "size": size,
            "name": receipt.invocation.proof_name}


def upload(receipt: Receipt, client: GitHubClient) -> None:
    read_public(receipt)
    recheck_submission(receipt.invocation, client, receipt.stage("preflight").result)
    invocation, request = receipt.invocation, receipt.invocation.request
    intent = {"name": invocation.proof_name, **receipt.stage("project").result}
    if receipt.stage("upload").state == "pending":
        observed = reconcile_asset(receipt, client)
        fresh = receipt.intend("upload", intent)
    else:
        fresh = receipt.intend("upload", intent)
        observed = reconcile_asset(receipt, client)
    if observed is None and fresh:
        try:
            command({"commands": []}, "native-proof-upload", [
                "gh", "release", "upload", request.tag, str(invocation.proof), "--repo", request.repo,
            ], client.environment, cwd=invocation.checkout, timeout=120)
        except VerificationError:
            # The mutation may have reached GitHub. Only exact remote bytes can
            # settle it; absence never grants another upload from this intent.
            observed = reconcile_asset(receipt, client)
        else:
            observed = reconcile_asset(receipt, client)
    if observed is None:
        raise ReleaseError("proof_upload_response_unresolved", incomplete=True)
    receipt.complete("upload", observed)


def _scope(receipt: Receipt) -> ReportScope:
    facts = receipt.stage("preflight").result
    return ReportScope(integer(facts.get("repository_id"), minimum=1),
                       receipt.invocation.request.tag, text_value(facts.get("harness_revision")))


def bind_dispatched_run(value: object, *, repository: str, title: object, revision: object,
                        paths: tuple[str, ...], expected_id: int | None, code: str) -> dict[str, object]:
    """Bind one listed or read run to the exact dispatch: its ID, title, head, main, event, path and URL.

    The caller supplies the trusted expectations and its own refusal ``code``.
    A title or revision that is not text binds nothing. A mismatch names only
    the fixed fields that failed, never observed values.
    """
    row = object_value(value)
    run_id = integer(row.get("id"), minimum=1)
    bindings = (
        ("id", expected_id is None or run_id == expected_id),
        ("display_title", isinstance(title, str) and row.get("display_title") == title),
        ("head_sha", isinstance(revision, str) and row.get("head_sha") == revision),
        ("head_branch", row.get("head_branch") == "main"),
        ("event", row.get("event") == "workflow_dispatch"),
        ("path", any(row.get("path") == path for path in paths)),
        ("html_url", row.get("html_url") == f"https://github.com/{repository}/actions/runs/{run_id}"),
    )
    mismatched = tuple(name for name, bound in bindings if not bound)
    if mismatched:
        raise ReleaseError(code, incomplete=True, mismatched=mismatched)
    return row


def validate_run(receipt: Receipt, value: object, *, expected_id: int | None = None,
                 expected_title: str | None = None) -> dict[str, object]:
    """The native validation run of a legacy release receipt, bound by ``bind_dispatched_run``."""
    title = (receipt.stage("dispatch").intent.get("title")
             if expected_title is None else expected_title)
    return bind_dispatched_run(value, repository=receipt.invocation.request.repo, title=title,
                               revision=receipt.stage("preflight").result.get("harness_revision"),
                               paths=WORKFLOW_RUN_PATHS, expected_id=expected_id,
                               code="native_validation_run_unbound")


def reconcile_dispatch(receipt: Receipt, client: GitHubClient, *,
                       expected_title: str | None = None) -> dict[str, object] | None:
    title = (receipt.stage("dispatch").intent.get("title")
             if expected_title is None else expected_title)
    matches = [run for run in matching_runs(client, receipt.invocation.request.repo, _scope(receipt))
               if run.get("display_title") == title]
    if not matches:
        return None
    if len(matches) != 1:
        raise ReleaseError("native_validation_run_ambiguous", incomplete=True)
    return validate_run(receipt, matches[0], expected_title=expected_title)


def dispatch(receipt: Receipt, client: GitHubClient) -> None:
    recheck_submission(receipt.invocation, client, receipt.stage("preflight").result)
    request = receipt.invocation.request
    asset = receipt.stage("upload").result
    digest = digest_value(asset.get("proof_sha256"))
    inputs: dict[str, str] = {
        "tag": request.tag, "proof_asset_id": str(integer(asset.get("asset_id"), minimum=1)),
        "proof_sha256": digest, "invocation_id": receipt.invocation.invocation_id,
        "opencode_model": request.opencode_model, "ccr_model": request.ccr_model or "",
    }
    title = proof_title(request.tag, receipt.invocation.invocation_id, digest)
    intent: dict[str, object] = {"title": title, "ref": "main", "inputs": inputs}
    if receipt.stage("dispatch").state == "pending":
        observed = reconcile_dispatch(receipt, client, expected_title=title)
        fresh = receipt.intend("dispatch", intent)
    else:
        fresh = receipt.intend("dispatch", intent)
        observed = reconcile_dispatch(receipt, client)
    if observed is None and fresh:
        fields = {"ref": "main", **{f"inputs[{key}]": value for key, value in inputs.items()}}
        try:
            ack = parse_workflow_dispatch_ack(client.json(
                f"repos/{request.repo}/actions/workflows/{WORKFLOW}/dispatches", method="POST", fields=fields,
            ), request.repo)
            receipt.complete("dispatch", {"run_id": ack.run_id, "html_url": ack.html_url, "title": title})
            return
        except (VerificationError, ReleaseError):
            observed = reconcile_dispatch(receipt, client)
    if observed is None:
        raise ReleaseError("workflow_dispatch_response_unresolved", incomplete=True)
    receipt.complete("dispatch", {"run_id": integer(observed.get("id"), minimum=1),
                                  "html_url": text_value(observed.get("html_url")), "title": title})
