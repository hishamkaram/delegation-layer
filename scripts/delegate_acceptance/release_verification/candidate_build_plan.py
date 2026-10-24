"""Pure rules for building one development candidate.

This module owns what a build must observe and prove: the requested identity,
how tool output is read, which Go VCS stamp is acceptable, and how observed
file digests become the candidate manifest. It performs no I/O; the build
runtime supplies bytes and runs the commands.
"""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass
import json
import re

from ..evidence.digests import sha
from .candidate_codec import VERSION_RE, revision, text
from .candidate_manifest import (
    REPOSITORY_RE, ArtifactEntry, BuildInputs, CandidateLocator, CandidateManifest, SourceIdentity,
    Toolchains, encode_locator, expected_inventory, npm_package_path, parse_manifest,
)
from .contract import VerificationError


STAGE = "build-candidate"


PACKAGE_NAME = "delegation-layer"


# Binaries built from this module; their Go VCS stamp must name the built commit.
VCS_STAMPED_BINARIES = ("delegate", "delegate-run")


_GO_VERSION = re.compile(rb"go version go(\d+\.\d+\.\d+) \S+/\S+\n?")


_NODE_VERSION = re.compile(rb"v(\d+\.\d+\.\d+)\n?")


_PYTHON_VERSION = re.compile(rb"Python (\d+\.\d+\.\d+)\n?")


_GORELEASER_VERSION = re.compile(rb"^GitVersion:\s+v?(\d+\.\d+\.\d+)\s*$", re.MULTILINE)


_VCS_SETTING = re.compile(rb"^\tbuild\t(vcs\.[a-z.]+)=(.*)$", re.MULTILINE)


@dataclass(frozen=True)
class BuildRequest:
    """The identity a caller asks to build; the build must prove each field."""

    version: str
    commit: str
    base_commit: str
    repository: str

    def __post_init__(self) -> None:
        text(self.version, VERSION_RE, STAGE)
        revision(self.commit, STAGE)
        revision(self.base_commit, STAGE)
        text(self.repository, REPOSITORY_RE, STAGE)

    @property
    def tag(self) -> str:
        return "v" + self.version


def _whole_output(pattern: re.Pattern[bytes], output: bytes, code: str) -> str:
    match = pattern.fullmatch(output)
    if match is None:
        raise VerificationError(code, STAGE)
    return match.group(1).decode("ascii")


def parse_go_version(output: bytes) -> str:
    return _whole_output(_GO_VERSION, output, "go_version_output_invalid")


def parse_node_version(output: bytes) -> str:
    return _whole_output(_NODE_VERSION, output, "node_version_output_invalid")


def parse_python_version(output: bytes) -> str:
    return _whole_output(_PYTHON_VERSION, output, "python_version_output_invalid")


def parse_goreleaser_version(output: bytes) -> str:
    """Read the GitVersion line of the multi-line banner `goreleaser --version` prints."""
    matches = _GORELEASER_VERSION.findall(output)
    if len(matches) != 1:
        raise VerificationError("goreleaser_version_output_invalid", STAGE)
    return matches[0].decode("ascii")


def parse_go_pin(data: bytes) -> str:
    """Read the one stable X.Y.Z line the source commit pins for Go."""
    if not data.endswith(b"\n") or b"\n" in data[:-1]:
        raise VerificationError("go_pin_invalid", STAGE)
    return text(data[:-1].decode("ascii", errors="replace"), VERSION_RE, STAGE)


def require_pinned_go(observed: str, pin: str) -> None:
    if observed != pin:
        raise VerificationError("go_toolchain_pin_mismatch", STAGE)


def package_version(package_json: bytes) -> str:
    try:
        value = json.loads(package_json)
    except ValueError:
        raise VerificationError("package_json_invalid", STAGE) from None
    if not isinstance(value, dict) or value.get("name") != PACKAGE_NAME:
        raise VerificationError("package_name_mismatch", STAGE)
    return text(value.get("version"), VERSION_RE, STAGE)


def require_package_version(package_json: bytes, request: BuildRequest) -> None:
    if package_version(package_json) != request.version:
        raise VerificationError("package_version_mismatch", STAGE)


def packed_filename(output: bytes, version: str) -> str:
    """Return the single tarball name `npm pack --json` reports for this version."""
    try:
        value = json.loads(output)
    except ValueError:
        raise VerificationError("npm_pack_output_invalid", STAGE) from None
    if (not isinstance(value, list) or len(value) != 1 or not isinstance(value[0], dict)
            or value[0].get("filename") != npm_package_path(version)):
        raise VerificationError("npm_pack_output_invalid", STAGE)
    return npm_package_path(version)


def require_vcs_identity(go_version_output: bytes, commit: str) -> None:
    """Require the binary's own Go VCS stamp: the built commit, unmodified."""
    settings: dict[str, bytes] = {}
    for key, value in _VCS_SETTING.findall(go_version_output):
        if key.decode("ascii") in settings:
            raise VerificationError("go_vcs_stamp_invalid", STAGE)
        settings[key.decode("ascii")] = value
    if settings.get("vcs.revision") != commit.encode("ascii"):
        raise VerificationError("go_vcs_revision_mismatch", STAGE)
    if settings.get("vcs.modified") != b"false":
        raise VerificationError("go_vcs_modified", STAGE)


def build_manifest(request: BuildRequest, *, tree: str, inputs: BuildInputs,
                   files: Mapping[str, tuple[int, str]]) -> CandidateManifest:
    """Bind observed (size, sha256) per inventory path to the requested identity."""
    inventory = expected_inventory(request.version)
    if set(files) != {path for path, _kind, _os, _arch in inventory}:
        raise VerificationError("candidate_files_mismatch", STAGE)
    artifacts = tuple(
        ArtifactEntry(path=path, kind=kind, os=os_name, arch=arch, size=files[path][0],
                      sha256=files[path][1])
        for path, kind, os_name, arch in inventory)
    return CandidateManifest(
        version=request.version, build_tag=request.tag,
        source=SourceIdentity(commit=request.commit, tree=tree, base_commit=request.base_commit),
        build=inputs, artifacts=artifacts)


def build_inputs(*, harness_commit: str, harness_sha256: str, config_sha256: str, go_sum_sha256: str,
                 go: str, goreleaser: str, python: str, node: str) -> BuildInputs:
    return BuildInputs(
        harness_commit=harness_commit, harness_sha256=harness_sha256,
        goreleaser_config_sha256=config_sha256,
        go_sum_sha256=go_sum_sha256,
        toolchains=Toolchains(go=go, goreleaser=goreleaser, python=python, node=node))


def locator_bytes(*, repository: str, run_id: int, run_attempt: int, artifact_id: int,
                  artifact_digest: str, manifest: bytes) -> bytes:
    """Encode the external locator for an uploaded payload; the manifest never names itself."""
    parse_manifest(manifest)
    digest = artifact_digest.removeprefix("sha256:")
    return encode_locator(CandidateLocator(
        repository=repository, run_id=run_id, run_attempt=run_attempt, artifact_id=artifact_id,
        artifact_archive_sha256=digest, manifest_sha256=sha(manifest)))
