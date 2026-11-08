"""Exact distribution payload manifest and API-derived artifact identity."""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum
import posixpath
import re

from scripts.release_verification.candidate_codec import HEX_SHA256
from scripts.release_verification.candidate_codec import VERSION_RE
from scripts.release_verification.candidate_codec import encode_record
from scripts.release_verification.candidate_codec import exact_fields
from scripts.release_verification.candidate_codec import load_record
from scripts.release_verification.candidate_codec import object_list
from scripts.release_verification.candidate_codec import positive_int
from scripts.release_verification.candidate_codec import require_header
from scripts.acceptance.evidence.source import revision
from scripts.release_verification.candidate_codec import sha256_hex
from scripts.acceptance.evidence.source import text
from scripts.release_verification.contract import MAX_ARCHIVE
from scripts.acceptance.evidence.source import MAX_OUTPUT
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.contract import expected_archive
from scripts.release_verification.published_contract import MAX_PACKAGE


MANIFEST_SCHEMA_VERSION = 2


MANIFEST_KIND = "delegation-layer-development-candidate"


MAX_MANIFEST_BYTES = 64 * 1024


# Bounds decoding; the exact inventory check owns the accepted count.
MAX_ARTIFACT_ENTRIES = 64


# Development builds always produce these four native archives.
PLATFORMS = (("Darwin", "amd64"), ("Darwin", "arm64"), ("Linux", "amd64"), ("Linux", "arm64"))


# The candidate payload holds the manifest beside the seven inventory files.
MANIFEST_PATH = "candidate-manifest.json"


CHECKSUMS_PATH = "checksums.txt"


HOMEBREW_FORMULA_PATH = "delegation-layer.rb"


# The CI workflow whose distribution job builds and uploads every candidate payload.
CANDIDATE_WORKFLOW_FILE = "ci.yml"


CANDIDATE_WORKFLOW_PATH = ".github/workflows/" + CANDIDATE_WORKFLOW_FILE


REPOSITORY_RE = re.compile(r"^[A-Za-z0-9-]{1,39}/[A-Za-z0-9._-]{1,100}$")


STAGE = "validate-candidate-manifest"


LOCATOR_STAGE = "validate-candidate-locator"


class ArtifactKind(Enum):
    ARCHIVE = "archive"
    CHECKSUMS = "checksums"
    NPM_PACKAGE = "npm-package"
    HOMEBREW_FORMULA = "homebrew-formula"


# Each kind is bounded by the reader that consumes it, so no inventory entry can
# describe a file its own consumer would refuse.
_ENTRY_LIMITS = {
    ArtifactKind.ARCHIVE: MAX_ARCHIVE,
    ArtifactKind.NPM_PACKAGE: MAX_PACKAGE,
    ArtifactKind.CHECKSUMS: MAX_OUTPUT,
    ArtifactKind.HOMEBREW_FORMULA: MAX_OUTPUT,
}


def entry_limit(kind: ArtifactKind) -> int:
    """Return the largest byte size an inventory entry of this kind may have."""
    return _ENTRY_LIMITS[kind]


# One member per native archive, one per other artifact kind, and the manifest.
PAYLOAD_MEMBER_COUNT = (len(PLATFORMS) + sum(1 for kind in ArtifactKind if kind is not ArtifactKind.ARCHIVE)
                        + 1)


# A generous resource budget for the hosted container of one payload: every member
# is allowed the largest entry size, leaving ample header and compression slack.
# It bounds what a loader accepts to fetch; it is not a proof that a ZIP fits.
MAX_CONTAINER_BYTES = PAYLOAD_MEMBER_COUNT * MAX_ARCHIVE


def npm_package_path(version: str) -> str:
    return f"delegation-layer-{version}.tgz"


def _safe_relative_path(value: object) -> bool:
    return (isinstance(value, str) and 0 < len(value) <= 255 and value.isascii()
            and value.isprintable() and not value.startswith("/") and "\\" not in value
            and posixpath.normpath(value) == value and ".." not in value.split("/"))


@dataclass(frozen=True)
class ArtifactEntry:
    path: str
    kind: ArtifactKind
    os: str | None
    arch: str | None
    size: int
    sha256: str

    def __post_init__(self) -> None:
        if not _safe_relative_path(self.path):
            raise VerificationError("candidate_artifact_path_invalid", STAGE)
        if (not isinstance(self.kind, ArtifactKind) or isinstance(self.size, bool)
                or not isinstance(self.size, int)
                or not isinstance(self.sha256, str) or HEX_SHA256.fullmatch(self.sha256) is None):
            raise VerificationError("candidate_artifact_invalid", STAGE)
        if not 0 < self.size <= entry_limit(self.kind):
            raise VerificationError("candidate_artifact_invalid", STAGE)


@dataclass(frozen=True)
class SourceIdentity:
    """The commit actually built, its tree, and the PR base it was built against."""

    commit: str
    tree: str
    base_commit: str

    def __post_init__(self) -> None:
        for value in (self.commit, self.tree, self.base_commit):
            revision(value, STAGE)


@dataclass(frozen=True)
class Toolchains:
    go: str
    goreleaser: str
    python: str
    node: str

    def __post_init__(self) -> None:
        for value in (self.go, self.goreleaser, self.python, self.node):
            text(value, VERSION_RE, STAGE)


@dataclass(frozen=True)
class BuildInputs:
    """Toolchain, configuration and dependency identity of the build."""

    goreleaser_config_sha256: str
    go_sum_sha256: str
    toolchains: Toolchains

    def __post_init__(self) -> None:
        sha256_hex(self.goreleaser_config_sha256, STAGE)
        sha256_hex(self.go_sum_sha256, STAGE)


def expected_inventory(version: str) -> tuple[tuple[str, ArtifactKind, str | None, str | None], ...]:
    """Return the exact (path, kind, os, arch) inventory, sorted by path."""
    tag = "v" + version
    entries: list[tuple[str, ArtifactKind, str | None, str | None]] = [
        (expected_archive({"os": os_name, "arch": arch}, tag), ArtifactKind.ARCHIVE, os_name, arch)
        for os_name, arch in PLATFORMS]
    entries.extend((
        (CHECKSUMS_PATH, ArtifactKind.CHECKSUMS, None, None),
        (npm_package_path(version), ArtifactKind.NPM_PACKAGE, None, None),
        (HOMEBREW_FORMULA_PATH, ArtifactKind.HOMEBREW_FORMULA, None, None),
    ))
    return tuple(sorted(entries, key=lambda entry: entry[0]))


def payload_paths(version: str) -> tuple[str, ...]:
    """Return the sorted flat names of the uploaded payload: the inventory and its manifest."""
    return tuple(sorted([path for path, _kind, _os, _arch in expected_inventory(version)] + [MANIFEST_PATH]))


@dataclass(frozen=True)
class CandidateManifest:
    version: str
    build_tag: str
    source: SourceIdentity
    build: BuildInputs
    artifacts: tuple[ArtifactEntry, ...]

    def __post_init__(self) -> None:
        text(self.version, VERSION_RE, STAGE)
        if self.build_tag != "v" + self.version:
            raise VerificationError("candidate_build_tag_mismatch", STAGE)
        paths = [entry.path for entry in self.artifacts]
        if len(set(paths)) != len(paths):
            raise VerificationError("candidate_artifact_duplicate", STAGE)
        observed = tuple((entry.path, entry.kind, entry.os, entry.arch) for entry in self.artifacts)
        if observed != expected_inventory(self.version):
            raise VerificationError("candidate_artifact_inventory_mismatch", STAGE)

    def archive(self, os_name: str, arch: str) -> ArtifactEntry:
        for entry in self.artifacts:
            if entry.kind is ArtifactKind.ARCHIVE and (entry.os, entry.arch) == (os_name, arch):
                return entry
        raise VerificationError("candidate_platform_unknown", STAGE)


@dataclass(frozen=True)
class CandidateLocator:
    """Artifact identity derived by the loader from GitHub and verified manifest bytes."""

    repository: str
    run_id: int
    run_attempt: int
    artifact_id: int
    artifact_archive_sha256: str
    manifest_sha256: str

    def __post_init__(self) -> None:
        text(self.repository, REPOSITORY_RE, LOCATOR_STAGE)
        for value in (self.run_id, self.run_attempt, self.artifact_id):
            positive_int(value, LOCATOR_STAGE)
        sha256_hex(self.artifact_archive_sha256, LOCATOR_STAGE)
        sha256_hex(self.manifest_sha256, LOCATOR_STAGE)

    @property
    def candidate_id(self) -> str:
        """The candidate key is the digest of its exact manifest bytes."""
        return self.manifest_sha256


def candidate_artifact_name(run_id: int, run_attempt: int) -> str:
    """The hosted payload artifact name the CI job gives one run attempt."""
    return f"development-candidate-{run_id}-{run_attempt}"


def _artifact_record(entry: ArtifactEntry) -> dict[str, object]:
    return {"path": entry.path, "kind": entry.kind.value, "os": entry.os, "arch": entry.arch,
            "size": entry.size, "sha256": entry.sha256}


def encode_manifest(manifest: CandidateManifest) -> bytes:
    toolchains = manifest.build.toolchains
    return encode_record({
        "schema_version": MANIFEST_SCHEMA_VERSION, "kind": MANIFEST_KIND,
        "version": manifest.version, "build_tag": manifest.build_tag,
        "source": {"commit": manifest.source.commit, "tree": manifest.source.tree,
                   "base_commit": manifest.source.base_commit},
        "build": {
            "goreleaser_config_sha256": manifest.build.goreleaser_config_sha256,
            "go_sum_sha256": manifest.build.go_sum_sha256,
            "toolchains": {"go": toolchains.go, "goreleaser": toolchains.goreleaser,
                           "python": toolchains.python, "node": toolchains.node},
        },
        "artifacts": [_artifact_record(entry) for entry in manifest.artifacts],
    }, STAGE)


def _parse_artifact(value: object) -> ArtifactEntry:
    fields = exact_fields(value, frozenset({"path", "kind", "os", "arch", "size", "sha256"}), STAGE)
    try:
        kind = ArtifactKind(fields["kind"])
    except ValueError:
        raise VerificationError("candidate_artifact_invalid", STAGE) from None
    os_name, arch = fields["os"], fields["arch"]
    if ((os_name is not None and not isinstance(os_name, str)) or
            (arch is not None and not isinstance(arch, str))):
        raise VerificationError("candidate_artifact_invalid", STAGE)
    path, size, digest = fields["path"], fields["size"], fields["sha256"]
    if not isinstance(path, str) or not _safe_relative_path(path):
        raise VerificationError("candidate_artifact_path_invalid", STAGE)
    if not isinstance(size, int) or not isinstance(digest, str):
        raise VerificationError("candidate_artifact_invalid", STAGE)
    return ArtifactEntry(path=path, kind=kind, os=os_name, arch=arch, size=size, sha256=digest)


def _parse_build(value: object) -> BuildInputs:
    fields = exact_fields(value, frozenset({
        "goreleaser_config_sha256", "go_sum_sha256",
        "toolchains"}), STAGE)
    tools = exact_fields(fields["toolchains"], frozenset({"go", "goreleaser", "python", "node"}), STAGE)
    toolchains = Toolchains(go=text(tools["go"], VERSION_RE, STAGE),
                            goreleaser=text(tools["goreleaser"], VERSION_RE, STAGE),
                            python=text(tools["python"], VERSION_RE, STAGE),
                            node=text(tools["node"], VERSION_RE, STAGE))
    return BuildInputs(
        goreleaser_config_sha256=sha256_hex(fields["goreleaser_config_sha256"], STAGE),
        go_sum_sha256=sha256_hex(fields["go_sum_sha256"], STAGE),
        toolchains=toolchains,
    )


def parse_manifest(data: bytes) -> CandidateManifest:
    value = load_record(data, max_bytes=MAX_MANIFEST_BYTES, stage=STAGE)
    require_header(value, schema_version=MANIFEST_SCHEMA_VERSION, kind=MANIFEST_KIND,
                   error="candidate_manifest_kind_invalid", stage=STAGE)
    fields = exact_fields(value, frozenset({
        "schema_version", "kind", "version", "build_tag", "source", "build", "artifacts"}), STAGE)
    source = exact_fields(fields["source"], frozenset({"commit", "tree", "base_commit"}), STAGE)
    artifacts = object_list(fields["artifacts"], max_items=MAX_ARTIFACT_ENTRIES, stage=STAGE)
    source_identity = SourceIdentity(commit=revision(source["commit"], STAGE),
                                     tree=revision(source["tree"], STAGE),
                                     base_commit=revision(source["base_commit"], STAGE))
    build = _parse_build(fields["build"])
    entries = tuple(_parse_artifact(item) for item in artifacts)
    version = text(fields["version"], VERSION_RE, STAGE)
    build_tag = fields["build_tag"]
    if not isinstance(build_tag, str):
        raise VerificationError("candidate_build_tag_mismatch", STAGE)
    return CandidateManifest(
        version=version, build_tag=build_tag,
        source=source_identity, build=build, artifacts=entries,
    )
