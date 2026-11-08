"""Verified release-archive facts consumed by published-consumer verification."""

from __future__ import annotations

from argparse import Namespace
from pathlib import Path
from scripts.release_verification.assets import release_paths
from scripts.release_verification.contract import installed_executables
from scripts.release_verification.contract import BINARIES
from scripts.acceptance.evidence.source import REVISION_RE
from scripts.release_verification.contract import TAG_RE
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.contract import _validate_archive_header
from scripts.release_verification.filesystem import read_receipt
from scripts.release_verification.published_contract import ArchiveFacts
from scripts.release_verification.published_contract import PublishedRequest
from scripts.release_verification.verification import _verify_archived_artifact
from scripts.release_verification.verification import _verify_archived_binaries


def _require_recorded_directory(archived: dict, extracted: Path) -> None:
    recorded = archived.get("cleanup", {}).get("directory_identities", {})
    identity = recorded.get(str(extracted.resolve())) if isinstance(recorded, dict) else None
    try:
        observed = extracted.lstat()
    except OSError:
        raise VerificationError("release_directory_identity_changed", "validate-release-directory") from None
    if (not isinstance(identity, dict) or extracted.is_symlink()
            or (identity.get("device"), identity.get("inode")) != (observed.st_dev, observed.st_ino)):
        raise VerificationError("release_directory_identity_changed", "validate-release-directory")


def published_paths(request: PublishedRequest) -> tuple[Path, Path, Path]:
    """Runner, retained assets and extraction directories; the receipt may not be inside them."""
    paths = Namespace(assets_dir=str(request.extract_dir.parent), extract_dir=str(request.extract_dir))
    return release_paths(paths, request.receipt.absolute())


def load_archive_facts(request: PublishedRequest, info: dict[str, str]) -> ArchiveFacts:
    """Bind the caller's archive receipt, retained assets and extracted binaries to one release.

    The returned selection is the extracted installation after the receipt's
    checksum and attestation chain was re-validated, never entries of checksums.txt.
    """
    if not TAG_RE.fullmatch(request.tag) or not REVISION_RE.fullmatch(request.source_revision):
        raise VerificationError("release_identity_invalid", "validate-release-identity")
    _, assets, extracted = published_paths(request)
    archived, _ = read_receipt(request.archive_receipt.absolute())
    release_identity, package_version = _validate_archive_header(
        archived, request.tag, request.source_revision, request.repo, info)
    artifact = _verify_archived_artifact(archived, info, request.tag, assets, extracted)
    _require_recorded_directory(archived, extracted)
    ledger: dict = {"binaries": {}}
    _verify_archived_binaries(archived, ledger, extracted, package_version)
    return ArchiveFacts(
        tag=request.tag, package_version=package_version,
        release_commit=release_identity["commit_sha"], archive_sha256=artifact["archive_sha256"],
        binaries=installed_executables({name: Path(ledger["binaries"][name]["path"]) for name in BINARIES}),
    )
