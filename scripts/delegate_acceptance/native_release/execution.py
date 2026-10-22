"""Native children use the canonical process owner and exact completion readback."""

from __future__ import annotations

import hashlib
import os
from pathlib import Path
import sys

from scripts.delegate_acceptance.directories.journal import directory_identity_record
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.evidence.bounded_files import read_bounded_regular_file
from scripts.delegate_acceptance.evidence.files import write_bytes
from scripts.delegate_acceptance.native_evidence.public_contract import parse_json
from scripts.delegate_acceptance.native_evidence.validation import validate_aggregate_receipt
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.release_verification.commands import command
from scripts.delegate_acceptance.release_verification.contract import expected_archive
from scripts.delegate_acceptance.release_verification.github import GitHubClient
from .contract import Invocation, ReleaseError, integer, object_value, text_value
from .receipt import Receipt


def read_object(path: Path, limit: int = 1 << 20) -> dict[str, object]:
    return object_value(parse_json(read_bounded_regular_file(path, limit)))


def file_sha(path: Path, limit: int = 1 << 20) -> str:
    return hashlib.sha256(read_bounded_regular_file(path, limit)).hexdigest()


def child_environment(invocation: Invocation) -> dict[str, str]:
    env = os.environ.copy()
    env.update({
        "RUNNER_TEMP": str(invocation.root / "runner"),
        "DELEGATE_ACCEPTANCE_BIN_DIR": str(invocation.assets / "extracted"),
        "DELEGATE_ACCEPTANCE_OUTPUT_ROOT": str(invocation.aggregate.parent),
        "DELEGATE_ACCEPTANCE_TAG": invocation.request.tag,
        "OPENCODE_LIVE_MODEL": invocation.request.opencode_model,
        "CCR_LIVE_MODEL": invocation.request.ccr_model or "",
        "AGY_LIVE_MODEL": invocation.request.agy_model or "",
        "DELEGATION_REQUIRE_LIVE_ACCEPTANCE": "0",
        "DELEGATE_ACCEPTANCE_RESOLVED_RETENTION": "3",
        "PATH": str(invocation.assets / "extracted") + os.pathsep + env.get("PATH", os.defpath),
    })
    return env


def child_intent(receipt: Receipt, name: str, argv: list[str]) -> dict[str, object]:
    root = receipt.invocation.root
    return {"argv": argv, "cwd": str(receipt.invocation.checkout),
            "completion": str(root / "runner" / f"{name}-processes" / f"001-{name}" / "completed.json")}


def observe_child(receipt: Receipt, name: str) -> dict[str, object]:
    stage = receipt.stage(name)
    intent = stage.intent
    completed = Path(text_value(intent.get("completion")))
    expected = receipt.invocation.root / "runner" / f"{name}-processes" / f"001-{name}"
    if completed != expected / "completed.json":
        raise ReleaseError("child_completion_location_invalid", incomplete=True)
    try:
        result = read_object(completed)
        started = read_object(expected / "started.json")
        invocation = read_object(expected / "invocation.json")
    except (OSError, ValueError) as error:
        raise ReleaseError("child_natural_completion_unproven", incomplete=True) from error
    if (result.get("natural_wait") is not True or type(result.get("exit_code")) is not int
            or not 0 <= result["exit_code"] <= 255
            or integer(result.get("pid"), minimum=1) != integer(started.get("pid"), minimum=1)
            or integer(result.get("ended_ns")) < integer(started.get("started_ns"))
            or invocation.get("argv") != intent.get("argv")
            or invocation.get("cwd") != intent.get("cwd")):
        raise ReleaseError("child_natural_completion_unproven", incomplete=True)
    return {"exit_code": result["exit_code"], "completion_sha256": file_sha(completed)}


def run_child(receipt: Receipt, name: str, argv: list[str],
              env: dict[str, str], *, timeout: int) -> dict[str, object]:
    if not receipt.intend(name, child_intent(receipt, name, argv)):
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
    request, assets = invocation.request, invocation.assets
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
    facts = receipt.stage("preflight").result
    argv = [str(Path(sys.executable).resolve()), str(invocation.checkout / "scripts/verify_release_archive.py"),
            "verify", "--mode", "native-acceptance", "--github-auth", "local-config",
            "--assets-dir", str(invocation.assets), "--extract-dir", str(invocation.assets / "extracted"),
            "--receipt", str(invocation.archive_receipt), "--tag", invocation.request.tag,
            "--repo", invocation.request.repo, "--source-revision", text_value(facts["harness_revision"])]
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
        invocation.aggregate, expected_tag=invocation.request.tag,
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
        blocked_before_runtime = (case.get("status") == "BLOCKED"
                                  and observation.get("method") == "blocked-before-runtime-cleanup"
                                  and proof.get("state") == "blocked-before-runtime")
        # Cleanup records supervisor shutdown. A PASS also needs the validated
        # lifecycle state, which binds every admitted task's managed lifetime
        # closure; failure.json has no success-only closure proof.
        closed = lifecycle.get("supervisor_shutdown_observed") is True
        if case.get("status") == "PASS" and lifecycle.get("state") != "complete":
            raise ReleaseError("native_ownership_unproven", incomplete=True)
        if not closed and not blocked_before_runtime:
            raise ReleaseError("native_ownership_unproven", incomplete=True)
    return {**result, "aggregate_sha256": digest, "status": aggregate["status"],
            "lifetime_closed": True}


def native(receipt: Receipt, env: dict[str, str]) -> None:
    env = {**env, "DELEGATE_ACCEPTANCE_SOURCE_REVISION":
           text_value(receipt.stage("preflight").result["harness_revision"])}
    invocation = receipt.invocation
    argv = [str(Path("/bin/sh").resolve()), str(invocation.checkout / "scripts/acceptance_native_all.sh")]
    run_child(receipt, "native", argv, env, timeout=4 * 60 * 60)
    receipt.complete("native", observe_native(receipt))


def finalize(receipt: Receipt, env: dict[str, str]) -> None:
    invocation = receipt.invocation
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
            "--tag", invocation.request.tag, "--repo", invocation.request.repo,
            "--source-revision", text_value(receipt.stage("preflight").result["harness_revision"])]
    result = run_child(receipt, "finalize", argv, env, timeout=600)
    evidence = read_object(invocation.native_receipt)
    native_facts = object_value(evidence.get("native_acceptance"))
    if native_facts.get("aggregate_receipt_sha256") != receipt.stage("native").result.get("aggregate_sha256"):
        raise ReleaseError("native_finalizer_receipt_unbound", incomplete=True)
    if evidence.get("verdict") not in {"pass", "blocked", "fail"}:
        raise ReleaseError("native_finalizer_incomplete", incomplete=True)
    receipt.complete("finalize", {**result, "receipt_sha256": file_sha(invocation.native_receipt)})
