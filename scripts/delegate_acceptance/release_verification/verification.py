"""Verification for exact release verification."""

from __future__ import annotations

from . import filesystem

from pathlib import Path
import argparse
import re
import tempfile
from ..directories.operations import DirectoryOwner
from ..native_evidence.contract import EvidenceError
from ..directories.journal import release_exclusive_file_lock
from ..native_evidence.validation import validate_aggregate_receipt
from .assets import (exercise_default_path, extract_archive, gh_environment, inspect_binaries, platform_identity, release_paths, remember_directory, resolve_release_identity, verify_checksum)
from .cleanup import (_replay_prepared_cleanup, finish_gate)
from .commands import (command)
from .contract import (_ArchiveEvidence, _validate_archive_header, _validate_archived_default_path, _native_case_hashes, _record_native_verdict, BINARIES, MAX_ARCHIVE, MAX_OUTPUT, REVISION_RE, TAG_RE, VerificationError, expected_archive, new_receipt)
from .filesystem import _acquire_receipt_lock, _reject_parent_components, bounded_digest, exit_code, read_receipt
from .source import (source_fingerprint)


def verify(args: argparse.Namespace) -> int:
    receipt_path = Path(args.receipt).absolute()
    lock_fd = _acquire_receipt_lock(receipt_path)
    try:
        return _verify_locked(args, receipt_path)
    finally:
        release_exclusive_file_lock(lock_fd)


def _verify_locked(args: argparse.Namespace, receipt_path: Path) -> int:
    replayed = _replay_prepared_cleanup(
        receipt_path, gate=args.mode, claim="packaged-default-path",
        tag=args.tag, revision=args.source_revision,
    )
    if replayed is not None:
        return replayed
    receipt = new_receipt(args.mode, args.tag, args.source_revision)
    filesystem.write_receipt(receipt_path, receipt)
    owner = DirectoryOwner()
    locations = {}

    def action():
        runner, assets, extracted = release_paths(args, receipt_path)
        locations.update(assets=assets, extracted=extracted)
        if not TAG_RE.fullmatch(args.tag) or not REVISION_RE.fullmatch(args.source_revision):
            raise VerificationError("release_identity_invalid", "validate-release-identity")
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", args.repo):
            raise VerificationError("repository_invalid", "validate-repository")
        info = platform_identity()
        receipt["platform"] = info
        receipt["source_harness"] = source_fingerprint(args.mode)
        release_identity = resolve_release_identity(args.repo, args.tag, receipt, gh_environment())
        receipt["release_identity"] = release_identity
        receipt["release_revision"] = release_identity["commit_sha"]
        receipt["harness_revision"] = args.source_revision
        name = expected_archive(info, args.tag)
        if args.mode == "archive-smoke":
            token = owner.create("release assets", assets, runner)
        else:
            if not assets.is_dir():
                raise VerificationError("release_assets_missing", "prepare-assets")
            token = owner.adopt_created("release assets", assets, runner)
        remember_directory(receipt, token)
        if args.mode == "archive-smoke":
            command(receipt, "github-release-download", [
                "gh", "release", "download", args.tag, "--repo", args.repo,
                "--pattern", name, "--pattern", "checksums.txt", "--dir", str(assets),
            ], gh_environment(), timeout=120)
        receipt["stage"] = "verify-checksum"
        archive = verify_checksum(assets, name, receipt)
        command(receipt, "github-attestation-verify", [
            "gh", "attestation", "verify", str(archive), "--repo", args.repo,
            "--signer-workflow", f"{args.repo}/.github/workflows/release.yml",
            "--source-ref", release_identity["ref"],
            "--source-digest", release_identity["commit_sha"],
        ], gh_environment(), timeout=60)
        receipt["artifact"]["attestation"] = "verified"
        receipt["stage"] = "extract-archive"
        extract_archive(archive, extracted, owner, receipt)
        paths = inspect_binaries(extracted, receipt)
        with tempfile.TemporaryDirectory(prefix="delegate-release-home-", dir=assets) as home:
            locations["isolated_home"] = Path(home)
            receipt["stage"] = "exercise-packaged-cli"
            exercise_default_path(paths, extracted, receipt, receipt["package_version"], info, Path(home))

    return finish_gate(receipt_path, receipt, owner, action,
                       "remove" if args.mode == "archive-smoke" else "retain", locations)


def finalize_native(args: argparse.Namespace) -> int:
    receipt_path = Path(args.receipt).absolute()
    lock_fd = _acquire_receipt_lock(receipt_path)
    try:
        return _finalize_native_locked(args, receipt_path)
    finally:
        release_exclusive_file_lock(lock_fd)


def _native_aggregate_path(
        aggregate_argument: str | None, archive_receipt: str,
        assets: Path, locations: dict[str, Path]) -> Path:
    if not aggregate_argument:
        raise VerificationError("aggregate_receipt_missing", "validate-aggregate-receipt")
    _reject_parent_components(Path(aggregate_argument), "unsafe_aggregate_path")
    _reject_parent_components(Path(archive_receipt), "unsafe_archive_receipt_path")
    aggregate_path = Path(aggregate_argument).absolute()
    try:
        aggregate_path.resolve().relative_to(assets.resolve())
    except ValueError:
        pass
    else:
        raise VerificationError("aggregate_receipt_inside_cleanup_scope", "validate-aggregate-receipt")
    aggregate_root = aggregate_path.parent
    if not aggregate_root.is_dir() or aggregate_root.is_symlink():
        raise VerificationError("aggregate_receipt_missing_or_invalid", "validate-aggregate-receipt")
    locations["aggregate_root"] = aggregate_root
    return aggregate_path


def _verify_archived_artifact(
        archived: dict, info: dict[str, str], tag: str,
        assets: Path, extracted: Path) -> dict:
    artifact = archived.get("artifact")
    archive_name = expected_archive(info, tag)
    archive_file, checksum_file = assets / archive_name, assets / "checksums.txt"
    if (not isinstance(artifact, dict) or artifact.get("attestation") != "verified"
            or not re.fullmatch(r"[0-9a-f]{64}", str(artifact.get("archive_sha256", "")))
            or not re.fullmatch(r"[0-9a-f]{64}", str(artifact.get("checksums_sha256", "")))
            or artifact.get("archive_name") != archive_name
            or archive_file.is_symlink() or checksum_file.is_symlink()
            or Path(str(artifact.get("archive_path", ""))).resolve() != archive_file.resolve()
            or Path(str(artifact.get("checksums_path", ""))).resolve() != checksum_file.resolve()
            or Path(str(artifact.get("extraction_path", ""))).resolve() != extracted.resolve()
            or not archive_file.is_file() or not checksum_file.is_file()
            or bounded_digest(archive_file, MAX_ARCHIVE, "archive_too_large")
            != artifact.get("archive_sha256")
            or bounded_digest(checksum_file, MAX_OUTPUT, "checksum_file_too_large")
            != artifact.get("checksums_sha256")):
        raise VerificationError("archive_receipt_invalid", "validate-archive-receipt")
    return artifact


def _adopt_archived_directories(
        archived: dict, receipt: dict, owner: DirectoryOwner,
        runner: Path, assets: Path, extracted: Path) -> None:
    asset_token = owner.adopt_created("release assets", assets, runner)
    recorded = archived.get("cleanup", {}).get("directory_identities", {})
    asset_identity = recorded.get(str(assets.resolve())) if isinstance(recorded, dict) else None
    if (not isinstance(asset_identity, dict)
            or asset_identity.get("path") != str(assets.resolve())
            or asset_identity.get("parent") != str(runner.resolve())
            or (asset_identity.get("device"), asset_identity.get("inode")) != asset_token.identity
            or (asset_identity.get("parent_device"), asset_identity.get("parent_inode"))
            != asset_token.parent_identity):
        raise VerificationError("release_directory_identity_changed", "validate-release-directory")
    remember_directory(receipt, asset_token)
    extracted_token = owner.adopt_created("release extraction", extracted, assets)
    extracted_identity = recorded.get(str(extracted.resolve())) if isinstance(recorded, dict) else None
    if (not isinstance(extracted_identity, dict)
            or extracted_identity.get("path") != str(extracted.resolve())
            or extracted_identity.get("parent") != str(assets.resolve())
            or (extracted_identity.get("device"), extracted_identity.get("inode"))
            != extracted_token.identity
            or (extracted_identity.get("parent_device"), extracted_identity.get("parent_inode"))
            != extracted_token.parent_identity):
        raise VerificationError("release_directory_identity_changed", "validate-release-directory")
    remember_directory(receipt, extracted_token)


def _verify_archived_binaries(
        archived: dict, receipt: dict, extracted: Path, package_version: str) -> dict:
    current = inspect_binaries(extracted, receipt)
    old_binaries = archived.get("binaries")
    if not isinstance(old_binaries, dict) or set(old_binaries) != set(BINARIES):
        raise VerificationError("archive_receipt_invalid", "validate-binary-identity")
    for name, path in current.items():
        previous = old_binaries[name]
        if (not isinstance(previous, dict) or previous.get("path") != str(path)
                or previous.get("sha256") != receipt["binaries"][name]["sha256"]):
            raise VerificationError("release_binary_identity_changed", "validate-binary-identity")
        if "version" in previous:
            receipt["binaries"][name]["version"] = previous["version"]
    build = archived.get("delegate_build_identity")
    if not isinstance(build, dict) or build.get("version") != package_version:
        raise VerificationError("archive_receipt_invalid", "validate-build-identity")
    return build


def _prepare_native_release(
        args: argparse.Namespace, receipt_path: Path, receipt: dict,
        owner: DirectoryOwner, locations: dict[str, Path]) -> _ArchiveEvidence:
    runner, assets, extracted = release_paths(args, receipt_path)
    locations.update(assets=assets, extracted=extracted)
    if not REVISION_RE.fullmatch(args.source_revision):
        raise VerificationError("source_revision_invalid", "validate-source-revision")
    info = platform_identity()
    receipt["platform"] = info
    receipt["source_harness"] = source_fingerprint("native-acceptance")
    receipt["stage"] = "validate-archive-receipt"
    aggregate_argument = getattr(args, "aggregate_receipt", None)
    archived, archive_receipt_hash = read_receipt(Path(args.archive_receipt).absolute())
    release_identity, package_version = _validate_archive_header(
        archived, args.tag, args.source_revision, getattr(args, "repo", None), info,
    )
    aggregate_path = _native_aggregate_path(
        aggregate_argument, args.archive_receipt, assets, locations,
    )
    artifact = _verify_archived_artifact(archived, info, args.tag, assets, extracted)
    observation = _validate_archived_default_path(archived)
    _adopt_archived_directories(archived, receipt, owner, runner, assets, extracted)
    build = _verify_archived_binaries(archived, receipt, extracted, package_version)
    return _ArchiveEvidence(
        aggregate_path=aggregate_path,
        extracted=extracted,
        platform=info,
        release_identity=release_identity,
        artifact=artifact,
        observation=observation,
        build=build,
        receipt_sha256=archive_receipt_hash,
    )


def _read_native_aggregate(
        args: argparse.Namespace, archive: _ArchiveEvidence) -> tuple[dict, str, int]:
    try:
        aggregate, aggregate_hash = validate_aggregate_receipt(
            archive.aggregate_path,
            expected_tag=args.tag,
            expected_source_revision=args.source_revision,
        )
    except EvidenceError as error:
        code = error.args[0] if error.args else "aggregate_receipt_invalid"
        raise VerificationError(code, "validate-aggregate-receipt") from None
    if aggregate.get("platform") not in (None, archive.platform):
        raise VerificationError("aggregate_platform_mismatch", "validate-aggregate-receipt")
    aggregate_status = aggregate.get("status")
    aggregate_exit = aggregate.get("exit_code")
    if aggregate_status not in {"PASS", "BLOCKED", "FAIL"} or type(aggregate_exit) is not int:
        raise VerificationError("aggregate_receipt_invalid", "validate-aggregate-receipt")
    recorded_code = exit_code(getattr(args, "exit_code_file", None))
    if recorded_code is None or recorded_code != aggregate_exit:
        raise VerificationError("native_acceptance_exit_mismatch", "validate-aggregate-receipt")
    if ((aggregate_exit == 0 and args.acceptance_outcome != "success")
            or (aggregate_exit != 0 and args.acceptance_outcome == "success")):
        raise VerificationError("native_acceptance_outcome_mismatch", "validate-aggregate-receipt")
    if args.acceptance_outcome in {"cancelled", "skipped"}:
        raise VerificationError("native_acceptance_outcome_incomplete", "validate-aggregate-receipt")
    return aggregate, aggregate_hash, recorded_code


def _record_native_result(
        args: argparse.Namespace, receipt: dict, archive: _ArchiveEvidence,
        aggregate: dict, aggregate_hash: str, recorded_code: int,
        case_hashes: list[dict]) -> None:
    aggregate_status = aggregate["status"]
    aggregate_exit = aggregate["exit_code"]
    receipt["aggregate_receipt_sha256"] = aggregate_hash
    receipt["release_identity"] = archive.release_identity.copy()
    receipt["release_revision"] = archive.release_identity["commit_sha"]
    receipt["harness_revision"] = args.source_revision
    receipt["native_acceptance"] = {
        "claims_scope": "provider-permission-scenario-case-results-only",
        "aggregate_status": aggregate_status,
        "aggregate_exit_code": aggregate_exit,
        "aggregate_receipt_path": str(archive.aggregate_path.resolve()),
        "aggregate_receipt_sha256": aggregate_hash,
        "case_receipt_hashes": case_hashes,
        "blocked_scope": "authentication-or-prerequisite-only",
    }

    receipt["artifact"] = archive.artifact.copy()
    receipt["delegate_build_identity"] = archive.build.copy()
    receipt["default_path_observation"] = archive.observation.copy()
    receipt["archive_receipt_sha256"] = archive.receipt_sha256
    receipt["acceptance"] = {
        "command": ["./scripts/acceptance_native_all.sh"], "outcome": args.acceptance_outcome,
        "exit_code": recorded_code, "binary_directory": str(archive.extracted.resolve()),
        "aggregate_status": aggregate_status,
    }


def _finalize_native_locked(args: argparse.Namespace, receipt_path: Path) -> int:
    replayed = _replay_prepared_cleanup(
        receipt_path, gate="native-acceptance", claim="native-acceptance-case-results",
        tag=args.tag, revision=args.source_revision,
    )
    if replayed is not None:
        return replayed
    receipt = new_receipt("native-acceptance", args.tag, args.source_revision)
    receipt["claim"] = "native-acceptance-case-results"
    receipt["claims_scope"] = "provider-permission-scenario-case-results-only"
    filesystem.write_receipt(receipt_path, receipt)
    owner, locations = DirectoryOwner(), {}

    def action():
        archive = _prepare_native_release(args, receipt_path, receipt, owner, locations)
        aggregate, aggregate_hash, recorded_code = _read_native_aggregate(args, archive)
        case_hashes = _native_case_hashes(aggregate, receipt)
        _record_native_result(
            args, receipt, archive, aggregate, aggregate_hash, recorded_code, case_hashes,
        )
        _record_native_verdict(aggregate, receipt)

    return finish_gate(receipt_path, receipt, owner, action, "remove", locations)
