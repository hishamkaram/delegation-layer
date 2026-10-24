"""Native children use the canonical process owner and exact completion readback."""

from __future__ import annotations

from dataclasses import dataclass
import hashlib
import os
from pathlib import Path
import sys

from scripts.delegate_acceptance.directories.journal import directory_identity_record
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.evidence.bounded_files import BoundedReadError, read_bounded_regular_file
from scripts.delegate_acceptance.evidence.files import write_bytes
from scripts.delegate_acceptance.native_evidence.lifetime_closure import case_lifetime_closed
from scripts.delegate_acceptance.native_evidence.public_contract import NativeCompletion, parse_json
from scripts.delegate_acceptance.native_evidence.validation import validate_aggregate_receipt
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.release_verification.commands import command
from scripts.delegate_acceptance.release_verification.contract import expected_archive
from scripts.delegate_acceptance.release_verification.github import GitHubClient
from .contract import (
    CandidateRequest, Invocation, ReleaseError, integer, object_value, release_request, text_value,
)
from .receipt import Receipt


def read_object(path: Path, limit: int = 1 << 20) -> dict[str, object]:
    return object_value(parse_json(read_bounded_regular_file(path, limit)))


def file_sha(path: Path, limit: int = 1 << 20) -> str:
    return hashlib.sha256(read_bounded_regular_file(path, limit)).hexdigest()


# The one finite native run: the canonical shell over the whole case matrix.
NATIVE_TIMEOUT_SECONDS = 4 * 60 * 60


def _native_environment(invocation: Invocation) -> dict[str, str]:
    """The user's environment and settings plus the invocation's paths and selected models."""
    env = os.environ.copy()
    env.update({
        "RUNNER_TEMP": str(invocation.root / "runner"),
        "DELEGATE_ACCEPTANCE_BIN_DIR": str(invocation.assets / "extracted"),
        "DELEGATE_ACCEPTANCE_OUTPUT_ROOT": str(invocation.aggregate.parent),
        "OPENCODE_LIVE_MODEL": invocation.request.opencode_model,
        "CCR_LIVE_MODEL": invocation.request.ccr_model or "",
        "AGY_LIVE_MODEL": invocation.request.agy_model or "",
        "DELEGATION_REQUIRE_LIVE_ACCEPTANCE": "0",
        "DELEGATE_ACCEPTANCE_RESOLVED_RETENTION": "3",
        "PATH": str(invocation.assets / "extracted") + os.pathsep + env.get("PATH", os.defpath),
    })
    return env


def child_environment(invocation: Invocation) -> dict[str, str]:
    return {**_native_environment(invocation), "DELEGATE_ACCEPTANCE_TAG": release_request(invocation).tag}


def candidate_environment(invocation: Invocation, harness_revision: str) -> dict[str, str]:
    """A candidate has no release tag: an inherited one would bind into its aggregate.

    Only ``DELEGATE_ACCEPTANCE_TAG``, the native harness's tag input, is removed;
    every other user setting, an unrelated ``TAG`` included, is kept.
    """
    env = _native_environment(invocation)
    env.pop("DELEGATE_ACCEPTANCE_TAG", None)
    env["DELEGATE_ACCEPTANCE_SOURCE_REVISION"] = text_value(harness_revision)
    return env


def native_argv(invocation: Invocation) -> list[str]:
    return [str(Path("/bin/sh").resolve()), str(invocation.checkout / "scripts/acceptance_native_all.sh")]


def child_intent(receipt: Receipt, name: str, argv: list[str]) -> dict[str, object]:
    root = receipt.invocation.root
    return {"argv": argv, "cwd": str(receipt.invocation.checkout),
            "completion": str(root / "runner" / f"{name}-processes" / f"001-{name}" / "completed.json")}


def observe_child_completion(receipt: Receipt, name: str) -> NativeCompletion:
    """Observe one finished child's natural completion; never launches.

    Each record is read once, and the same bytes are validated and hashed. Every
    failure to prove the completion leaves the child's closure incomplete.
    """
    try:
        return _validated_completion(receipt, name)
    except ReleaseError as error:
        if error.incomplete:
            raise
        raise ReleaseError(error.code, incomplete=True, mismatched=error.mismatched) from error
    except (BoundedReadError, ValueError) as error:
        raise ReleaseError("child_natural_completion_unproven", incomplete=True) from error


@dataclass(frozen=True)
class NaturalCapture:
    """One retained process capture whose own records prove a natural Wait."""

    exit_code: int
    invocation: dict[str, object]
    started_sha256: str
    invocation_sha256: str
    completed_sha256: str


def read_natural_capture(directory: Path) -> NaturalCapture:
    """Read one capture's started, invocation and completed records once and prove its natural Wait."""
    completed_bytes = read_bounded_regular_file(directory / "completed.json", 1 << 20)
    started_bytes = read_bounded_regular_file(directory / "started.json", 1 << 20)
    invocation_bytes = read_bounded_regular_file(directory / "invocation.json", 1 << 20)
    result = object_value(parse_json(completed_bytes))
    started = object_value(parse_json(started_bytes))
    exit_code = result.get("exit_code")
    if (result.get("natural_wait") is not True or type(exit_code) is not int
            or not 0 <= exit_code <= 255
            or integer(result.get("pid"), minimum=1) != integer(started.get("pid"), minimum=1)
            or integer(result.get("ended_ns")) < integer(started.get("started_ns"))):
        raise ReleaseError("child_natural_completion_unproven", incomplete=True)
    return NaturalCapture(
        exit_code=exit_code, invocation=object_value(parse_json(invocation_bytes)),
        started_sha256=hashlib.sha256(started_bytes).hexdigest(),
        invocation_sha256=hashlib.sha256(invocation_bytes).hexdigest(),
        completed_sha256=hashlib.sha256(completed_bytes).hexdigest())


def _validated_completion(receipt: Receipt, name: str) -> NativeCompletion:
    intent = receipt.stage(name).intent
    completed = Path(text_value(intent.get("completion")))
    expected = receipt.invocation.root / "runner" / f"{name}-processes" / f"001-{name}"
    if completed != expected / "completed.json":
        raise ReleaseError("child_completion_location_invalid", incomplete=True)
    capture = read_natural_capture(expected)
    if (capture.invocation.get("argv") != intent.get("argv")
            or capture.invocation.get("cwd") != intent.get("cwd")):
        raise ReleaseError("child_natural_completion_unproven", incomplete=True)
    return NativeCompletion(
        invocation_id=receipt.invocation.invocation_id, natural_wait=True, exit_code=capture.exit_code,
        started_sha256=capture.started_sha256, invocation_sha256=capture.invocation_sha256,
        completed_sha256=capture.completed_sha256)


def observe_child(receipt: Receipt, name: str) -> dict[str, object]:
    completion = observe_child_completion(receipt, name)
    return {"exit_code": completion.exit_code, "completion_sha256": completion.completed_sha256}


def run_child(receipt: Receipt, name: str, argv: list[str],
              env: dict[str, str], *, timeout: int) -> dict[str, object]:
    """Run or observe one legacy release child; a candidate is refused before any intent."""
    release_request(receipt.invocation)
    return _child_transaction(receipt, name, argv, env, timeout=timeout)


def _child_transaction(receipt: Receipt, name: str, argv: list[str],
                       env: dict[str, str], *, timeout: int) -> dict[str, object]:
    """Persist the child intent, then Start, Wait and observe it; the request type selects the policy.

    A release child already intended is observed, never started again. A candidate
    runs only its native child, and an existing intent stops before any Start: only
    the caller's fresh claim acknowledgement may lead here.
    """
    candidate = isinstance(receipt.invocation.request, CandidateRequest)
    if candidate and name != "native":
        raise ReleaseError("candidate_child_invalid")
    if not receipt.intend(name, child_intent(receipt, name, argv)):
        if candidate:
            raise ReleaseError("candidate_native_intent_exists_no_launch", incomplete=True)
        return observe_child(receipt, name)
    capture = receipt.invocation.root / "runner" / f"{name}-processes"
    owner = DirectoryOwner()
    try:
        processes = Processes(capture, environment=env, owner=owner,
                              environment_keys_only=True, return_result=True)
        process = processes.start(name, argv, receipt.invocation.checkout)
        process.wait(timeout=timeout, expected=None)
        if not processes.finished_naturally():
            raise ReleaseError("native_child_ownership_unresolved", incomplete=True)
        return observe_child(receipt, name)
    except RuntimeError as error:
        raise ReleaseError("native_child_observation_incomplete", incomplete=True) from error
    finally:
        owner.release()


def download(receipt: Receipt, client: GitHubClient) -> None:
    invocation = receipt.invocation
    request, assets = release_request(invocation), invocation.assets
    intent = {"assets": str(assets), "tag": request.tag}
    if not receipt.intend("download", intent):
        raise ReleaseError("archive_download_incomplete", incomplete=True)
    owner = DirectoryOwner()
    try:
        try:
            stage = owner.create(
                "release private stage", invocation.root / "private-stage", invocation.root,
            )
        except FileExistsError as error:
            raise ReleaseError("private_stage_identity_unavailable", incomplete=True) from error
        runner = owner.create("release runner", assets.parent, invocation.root)
        token = owner.create("release assets", assets, assets.parent)
        name = expected_archive({"os": invocation.platform_os, "arch": invocation.platform_arch}, request.tag)
        command({"commands": []}, "release-download", [
            "gh", "release", "download", request.tag, "--repo", request.repo,
            "--pattern", name, "--pattern", "checksums.txt", "--dir", str(assets),
        ], client.environment, cwd=invocation.checkout, timeout=120)
        receipt.complete("download", {"stage_identity": directory_identity_record(stage),
                                      "runner_identity": directory_identity_record(runner),
                                      "assets_identity": directory_identity_record(token)})
    finally:
        owner.release()


def archive(receipt: Receipt, env: dict[str, str]) -> None:
    invocation = receipt.invocation
    request = release_request(invocation)
    facts = receipt.stage("preflight").result
    argv = [str(Path(sys.executable).resolve()), str(invocation.checkout / "scripts/verify_release_archive.py"),
            "verify", "--mode", "native-acceptance", "--github-auth", "local-config",
            "--assets-dir", str(invocation.assets), "--extract-dir", str(invocation.assets / "extracted"),
            "--receipt", str(invocation.archive_receipt), "--tag", request.tag,
            "--repo", request.repo, "--source-revision", text_value(facts["harness_revision"])]
    result = run_child(receipt, "archive", argv, env, timeout=600)
    evidence = read_object(invocation.archive_receipt)
    if (result["exit_code"] != 0 or evidence.get("verdict") != "pass"
            or evidence.get("release_revision") != facts.get("release_revision")
            or evidence.get("harness_revision") != facts.get("harness_revision")
            or object_value(evidence.get("source_harness")).get("sha256")
            != facts.get("source_harness_sha256")):
        raise ReleaseError("verified_archive_prerequisite_failed")
    receipt.complete("archive", {**result, "receipt_sha256": file_sha(invocation.archive_receipt)})


def observe_native(receipt: Receipt) -> dict[str, object]:
    result = observe_child(receipt, "native")
    invocation = receipt.invocation
    aggregate, digest = validate_aggregate_receipt(
        invocation.aggregate, expected_tag=release_request(invocation).tag,
        expected_source_revision=text_value(receipt.stage("preflight").result["harness_revision"]),
    )
    if aggregate.get("exit_code") != result["exit_code"]:
        raise ReleaseError("native_aggregate_exit_mismatch", incomplete=True)
    cases = aggregate.get("cases")
    if not isinstance(cases, list) or not cases:
        raise ReleaseError("native_ownership_unproven", incomplete=True)
    for value in cases:
        case = object_value(value)
        compact = read_object(Path(text_value(case.get("receipt_path"))))
        observation = object_value(object_value(compact.get("cleanup")).get("observation"))
        proof = object_value(compact.get("source_proof"))
        lifecycle = object_value(proof.get("lifecycle_proof"))
        # Cleanup records supervisor shutdown. A PASS also needs the validated
        # lifecycle state, which binds every admitted task's managed lifetime
        # closure; failure.json has no success-only closure proof.
        if not case_lifetime_closed(
                status=case.get("status"), cleanup_method=observation.get("method"),
                proof_state=proof.get("state"), lifecycle_state=lifecycle.get("state"),
                supervisor_shutdown_observed=lifecycle.get("supervisor_shutdown_observed")):
            raise ReleaseError("native_ownership_unproven", incomplete=True)
    return {**result, "aggregate_sha256": digest, "status": aggregate["status"],
            "lifetime_closed": True}


def native(receipt: Receipt, env: dict[str, str]) -> None:
    env = {**env, "DELEGATE_ACCEPTANCE_SOURCE_REVISION":
           text_value(receipt.stage("preflight").result["harness_revision"])}
    run_child(receipt, "native", native_argv(receipt.invocation), env, timeout=NATIVE_TIMEOUT_SECONDS)
    receipt.complete("native", observe_native(receipt))


def finalize(receipt: Receipt, env: dict[str, str]) -> None:
    invocation = receipt.invocation
    request = release_request(invocation)
    code = integer(receipt.stage("native").result.get("exit_code"))
    exit_file = invocation.root / "native-exit-code.txt"
    expected_exit = f"{code}\n".encode("ascii")
    try:
        write_bytes(exit_file, expected_exit)
    except FileExistsError:
        pass
    if read_bounded_regular_file(exit_file, 16) != expected_exit:
        raise ReleaseError("native_exit_readback_conflict", incomplete=True)
    argv = [str(Path(sys.executable).resolve()), str(invocation.checkout / "scripts/verify_release_archive.py"),
            "finalize-native", "--archive-receipt", str(invocation.archive_receipt),
            "--aggregate-receipt", str(invocation.aggregate), "--assets-dir", str(invocation.assets),
            "--extract-dir", str(invocation.assets / "extracted"), "--receipt", str(invocation.native_receipt),
            "--exit-code-file", str(exit_file), "--acceptance-outcome", "success" if code == 0 else "failure",
            "--tag", request.tag, "--repo", request.repo,
            "--source-revision", text_value(receipt.stage("preflight").result["harness_revision"])]
    result = run_child(receipt, "finalize", argv, env, timeout=600)
    evidence = read_object(invocation.native_receipt)
    native_facts = object_value(evidence.get("native_acceptance"))
    if native_facts.get("aggregate_receipt_sha256") != receipt.stage("native").result.get("aggregate_sha256"):
        raise ReleaseError("native_finalizer_receipt_unbound", incomplete=True)
    if evidence.get("verdict") not in {"pass", "blocked", "fail"}:
        raise ReleaseError("native_finalizer_incomplete", incomplete=True)
    receipt.complete("finalize", {**result, "receipt_sha256": file_sha(invocation.native_receipt)})
