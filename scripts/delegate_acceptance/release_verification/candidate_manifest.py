"""Development candidate manifest and its external hosted-artifact locator.

The manifest owns the exact package inventory of one development build. It
never contains its own digest or the digest of the hosted container artifact
that carries it; the external locator binds both, so neither record is
circular. The manifest's source commit is the build's actual VCS revision; a
later squash-merge commit is a separate publication fact.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum
import posixpath
import re

from ..evidence.digests import sha
from .candidate_codec import (
    HEX_SHA256, VERSION_RE, encode_record, exact_fields, load_record, object_list,
    positive_int, require_header, revision, sha256_hex, text,
)
from .contract import MAX_ARCHIVE, MAX_OUTPUT, VerificationError, expected_archive
from .published_contract import MAX_PACKAGE


MANIFEST_SCHEMA_VERSION = 1


MANIFEST_KIND = "delegation-layer-development-candidate"


LOCATOR_SCHEMA_VERSION = 1


LOCATOR_KIND = "delegation-layer-candidate-locator"


MAX_MANIFEST_BYTES = 64 * 1024


MAX_LOCATOR_BYTES = 4 * 1024


# Bounds decoding; the exact inventory check owns the accepted count.
MAX_ARTIFACT_ENTRIES = 64


# Development builds always produce these four native archives.
PLATFORMS = (("Darwin", "amd64"), ("Darwin", "arm64"), ("Linux", "amd64"), ("Linux", "arm64"))


REQUIRED_OSES = ("Darwin", "Linux")


# The candidate payload holds the manifest beside the seven inventory files.
MANIFEST_PATH = "candidate-manifest.json"


CHECKSUMS_PATH = "checksums.txt"


HOMEBREW_FORMULA_PATH = "delegation-layer.rb"


# The CI workflow whose manual job builds and uploads every candidate payload.
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
    """Harness, toolchain, configuration and dependency identity of the build.

    ``harness_sha256`` is the build harness source fingerprint computed by the
    existing ``release_verification.source.fingerprint_sources`` owner. It is
    distinct from the source commit and from any native proof fingerprint.
    """

    harness_commit: str
    harness_sha256: str
    goreleaser_config_sha256: str
    go_sum_sha256: str
    toolchains: Toolchains

    def __post_init__(self) -> None:
        revision(self.harness_commit, STAGE)
        sha256_hex(self.harness_sha256, STAGE)
        sha256_hex(self.goreleaser_config_sha256, STAGE)
        sha256_hex(self.go_sum_sha256, STAGE)


def expected_inventory(version: str) -> tuple[tuple[str, ArtifactKind, str | None, str | None], ...]:
    """Return the exact (path, kind, os, arch) inventory, sorted by path."""
    tag = "v" + version
    entries = [(expected_archive({"os": os_name, "arch": arch}, tag), ArtifactKind.ARCHIVE, os_name, arch)
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
    """External binding of one hosted workflow artifact to its manifest bytes."""

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
            "harness_commit": manifest.build.harness_commit,
            "harness_sha256": manifest.build.harness_sha256,
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
    for key in ("os", "arch"):
        if fields[key] is not None and not isinstance(fields[key], str):
            raise VerificationError("candidate_artifact_invalid", STAGE)
    return ArtifactEntry(path=fields["path"], kind=kind, os=fields["os"], arch=fields["arch"],
                         size=fields["size"], sha256=fields["sha256"])


def _parse_build(value: object) -> BuildInputs:
    fields = exact_fields(value, frozenset({
        "harness_commit", "harness_sha256", "goreleaser_config_sha256", "go_sum_sha256",
        "toolchains"}), STAGE)
    tools = exact_fields(fields["toolchains"], frozenset({"go", "goreleaser", "python", "node"}), STAGE)
    return BuildInputs(
        harness_commit=fields["harness_commit"], harness_sha256=fields["harness_sha256"],
        goreleaser_config_sha256=fields["goreleaser_config_sha256"],
        go_sum_sha256=fields["go_sum_sha256"],
        toolchains=Toolchains(go=tools["go"], goreleaser=tools["goreleaser"],
                              python=tools["python"], node=tools["node"]),
    )


def parse_manifest(data: bytes) -> CandidateManifest:
    value = load_record(data, max_bytes=MAX_MANIFEST_BYTES, stage=STAGE)
    require_header(value, schema_version=MANIFEST_SCHEMA_VERSION, kind=MANIFEST_KIND,
                   error="candidate_manifest_kind_invalid", stage=STAGE)
    fields = exact_fields(value, frozenset({
        "schema_version", "kind", "version", "build_tag", "source", "build", "artifacts"}), STAGE)
    source = exact_fields(fields["source"], frozenset({"commit", "tree", "base_commit"}), STAGE)
    artifacts = object_list(fields["artifacts"], max_items=MAX_ARTIFACT_ENTRIES, stage=STAGE)
    return CandidateManifest(
        version=fields["version"], build_tag=fields["build_tag"],
        source=SourceIdentity(commit=source["commit"], tree=source["tree"],
                              base_commit=source["base_commit"]),
        build=_parse_build(fields["build"]),
        artifacts=tuple(_parse_artifact(item) for item in artifacts),
    )


def encode_locator(locator: CandidateLocator) -> bytes:
    return encode_record({
        "schema_version": LOCATOR_SCHEMA_VERSION, "kind": LOCATOR_KIND,
        "repository": locator.repository, "run_id": locator.run_id,
        "run_attempt": locator.run_attempt, "artifact_id": locator.artifact_id,
        "artifact_archive_sha256": locator.artifact_archive_sha256,
        "manifest_sha256": locator.manifest_sha256,
    }, LOCATOR_STAGE)


def parse_locator(data: bytes) -> CandidateLocator:
    value = load_record(data, max_bytes=MAX_LOCATOR_BYTES, stage=LOCATOR_STAGE)
    require_header(value, schema_version=LOCATOR_SCHEMA_VERSION, kind=LOCATOR_KIND,
                   error="candidate_locator_kind_invalid", stage=LOCATOR_STAGE)
    fields = exact_fields(value, frozenset({
        "schema_version", "kind", "repository", "run_id", "run_attempt", "artifact_id",
        "artifact_archive_sha256", "manifest_sha256"}), LOCATOR_STAGE)
    return CandidateLocator(
        repository=fields["repository"], run_id=fields["run_id"], run_attempt=fields["run_attempt"],
        artifact_id=fields["artifact_id"], artifact_archive_sha256=fields["artifact_archive_sha256"],
        manifest_sha256=fields["manifest_sha256"],
    )


def bind_manifest(locator: CandidateLocator, data: bytes) -> CandidateManifest:
    """Parse manifest bytes only after they match the locator's digest."""
    if sha(data) != locator.manifest_sha256:
        raise VerificationError("candidate_manifest_digest_mismatch", LOCATOR_STAGE)
    return parse_manifest(data)
