"""Build, collect and prove the seven files of one development candidate.

Every file comes from the frozen clone: GoReleaser builds all four archives and
the checksums once, `npm pack` runs once, and the canonical shell renderer
writes the formula once from the built checksums. Nothing here publishes,
tags remotely or reads a credential.
"""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path

from ..directories.operations import DirectoryOwner
from .assets import extract_archive, inspect_binaries, remember_directory, verify_checksum
from .candidate_build_plan import (
    STAGE, VCS_STAMPED_BINARIES, BuildRequest, packed_filename, require_vcs_identity,
)
from .candidate_manifest import (
    CHECKSUMS_PATH, HOMEBREW_FORMULA_PATH, PLATFORMS, ArtifactKind, entry_limit, expected_inventory,
)
from .commands import command
from .contract import VerificationError, expected_archive
from .filesystem import _bounded_children, _bounded_file_bytes, bounded_fingerprint


FETCH_TIMEOUT_SECONDS = 900


GORELEASER_TIMEOUT_SECONDS = 3600


PACK_TIMEOUT_SECONDS = 600


RENDER_TIMEOUT_SECONDS = 60


INSPECT_TIMEOUT_SECONDS = 120


PUEUE_ROOT = Path(".release") / "pueue"


@dataclass(frozen=True)
class BuildContext:
    """Paths, owner and environment shared by the build steps of one candidate."""

    request: BuildRequest
    receipt: dict
    environment: Mapping[str, str]
    clone: Path
    work: Path
    output: Path
    owner: DirectoryOwner
    goreleaser: Path


def run(context: BuildContext, name: str, argv: list[str], *, cwd: Path, timeout: float,
        capture: bool = False) -> bytes:
    return command(context.receipt, name, argv, dict(context.environment), capture=capture,
                   timeout=timeout, cwd=cwd)


def write_new(path: Path, data: bytes) -> None:
    try:
        with path.open("xb") as handle:
            handle.write(data)
        path.chmod(0o644)
    except OSError:
        raise VerificationError("candidate_output_unwritable", STAGE) from None


def _copy_new(source: Path, destination: Path, limit: int, code: str) -> None:
    write_new(destination, _bounded_file_bytes(source, limit, code))


def build_release_files(context: BuildContext) -> None:
    """Fetch the pinned Pueue bundle, then build every archive once without publishing."""
    run(context, "fetch-pueue", ["bash", "scripts/fetch-pueue-assets.sh", str(PUEUE_ROOT)],
        cwd=context.clone, timeout=FETCH_TIMEOUT_SECONDS)
    run(context, "goreleaser-release",
        [str(context.goreleaser), "release", "--clean", "--skip=publish"],
        cwd=context.clone, timeout=GORELEASER_TIMEOUT_SECONDS)


def collect_goreleaser_files(context: BuildContext) -> None:
    dist = context.clone / "dist"
    for path, kind, _os, _arch in expected_inventory(context.request.version):
        if kind in (ArtifactKind.ARCHIVE, ArtifactKind.CHECKSUMS):
            _copy_new(dist / path, context.output / path, entry_limit(kind), "goreleaser_output_missing")
    checksums = _bounded_file_bytes(context.output / CHECKSUMS_PATH,
                                    entry_limit(ArtifactKind.CHECKSUMS), "checksum_file_too_large")
    if len(checksums.splitlines()) != len(PLATFORMS):
        raise VerificationError("checksum_entries_not_exact", STAGE)


def pack_npm(context: BuildContext) -> None:
    destination = context.work / "npm"
    remember_directory(context.receipt,
                       context.owner.create("npm pack destination", destination, context.work))
    reported = run(
        context, "npm-pack",
        ["npm", "pack", "--ignore-scripts", "--json", "--pack-destination", str(destination)],
        cwd=context.clone, timeout=PACK_TIMEOUT_SECONDS, capture=True)
    filename = packed_filename(reported, context.request.version)
    if [child.name for child in _bounded_children(destination)] != [filename]:
        raise VerificationError("npm_pack_destination_unexpected", STAGE)
    _copy_new(destination / filename, context.output / filename,
              entry_limit(ArtifactKind.NPM_PACKAGE), "npm_package_unreadable")


def render_formula(context: BuildContext) -> None:
    formula = run(
        context, "render-formula",
        ["bash", "scripts/render-homebrew-formula.sh", context.request.tag,
         str(context.output / CHECKSUMS_PATH)],
        cwd=context.clone, timeout=RENDER_TIMEOUT_SECONDS, capture=True)
    write_new(context.output / HOMEBREW_FORMULA_PATH, formula)


def _require_vcs_stamps(context: BuildContext, extracted: Path) -> None:
    for name in VCS_STAMPED_BINARIES:
        stamp = run(context, "go-version-m", ["go", "version", "-m", str(extracted / name)],
                    cwd=context.clone, timeout=INSPECT_TIMEOUT_SECONDS, capture=True)
        require_vcs_identity(stamp, context.request.commit)


def verify_bundles(context: BuildContext) -> None:
    """Prove each archive matches its checksum, contains the required roles and keeps the Go stamp."""
    for os_name, arch in PLATFORMS:
        name = expected_archive({"os": os_name, "arch": arch}, context.request.tag)
        archive = verify_checksum(context.output, name, context.receipt)
        extracted = context.work / f"extract-{os_name}-{arch}"
        extract_archive(archive, extracted, context.owner, context.receipt)
        inspect_binaries(extracted, {"binaries": {}})
        _require_vcs_stamps(context, extracted)


def observed_files(context: BuildContext) -> dict[str, tuple[int, str]]:
    """Return (size, sha256) of every inventory file, read once from the output directory."""
    files = {}
    for path, kind, _os, _arch in expected_inventory(context.request.version):
        fingerprint = bounded_fingerprint(context.output / path, entry_limit(kind), "candidate_output_unreadable")
        files[path] = (fingerprint.size, fingerprint.sha256)
    return files
