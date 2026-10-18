"""Bind supplied native proof to attested archive bytes without executing them."""

from __future__ import annotations

from pathlib import Path

from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.native_evidence.public_contract import ProofExpectation
from .assets import extract_archive, inspect_binaries, verify_checksum
from .commands import command
from .contract import VerificationError, expected_archive
from .github import GitHubClient
from .native_submission import SubmissionIdentity
from .source import source_fingerprint


def archive_expectation(
        client: GitHubClient, identity: SubmissionIdentity, *, tag: str,
        platform_os: str, platform_arch: str, root: Path,
        opencode_model: str, ccr_model: str | None) -> ProofExpectation:
    if platform_os not in {"Linux", "Darwin"} or platform_arch not in {"amd64", "arm64"}:
        raise VerificationError("native_platform_invalid", "native-archive")
    info = {"os": platform_os, "arch": platform_arch}
    name = expected_archive(info, tag)
    assets, extracted = root / "assets", root / "assets" / "extracted"
    receipt: dict = {"commands": [], "artifact": {}, "binaries": {},
                     "cleanup": {"directory_identities": {}}}
    owner = DirectoryOwner()
    try:
        owner.create("hosted proof assets", assets, root)
        command(receipt, "native-release-download", [
            "gh", "release", "download", tag, "--repo", identity.repository,
            "--pattern", name, "--pattern", "checksums.txt", "--dir", str(assets),
        ], client.environment, timeout=120)
        archive = verify_checksum(assets, name, receipt)
        command(receipt, "native-attestation", [
            "gh", "attestation", "verify", str(archive), "--repo", identity.repository,
            "--signer-workflow", f"{identity.repository}/.github/workflows/release.yml",
            "--source-ref", f"refs/tags/{tag}", "--source-digest", identity.release_revision,
        ], client.environment, timeout=60)
        extract_archive(archive, extracted, owner, receipt)
        inspect_binaries(extracted, receipt)
        binaries = receipt["binaries"]
        return ProofExpectation(
            repo=identity.repository, tag=tag, release_revision=identity.release_revision,
            harness_revision=identity.harness_revision, platform_os=platform_os,
            platform_arch=platform_arch, opencode_model=opencode_model, ccr_model=ccr_model,
            source_harness_sha256=str(source_fingerprint("native-acceptance")["sha256"]),
            archive_sha256=receipt["artifact"]["archive_sha256"],
            delegate_sha256=binaries["delegate"]["sha256"],
            runner_sha256=binaries["delegate-run"]["sha256"],
            pueue_sha256=binaries["pueue"]["sha256"],
            pueued_sha256=binaries["pueued"]["sha256"],
        )
    finally:
        # Hosted disposable storage is retained through validation; this releases
        # only descriptor authority and never claims native lifetime cleanup.
        owner.release()
