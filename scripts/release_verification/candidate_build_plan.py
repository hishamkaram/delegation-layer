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

from scripts.release_verification.candidate_codec import VERSION_RE
from scripts.acceptance.evidence.source import revision
from scripts.acceptance.evidence.source import text
from scripts.release_verification.candidate_manifest import REPOSITORY_RE
from scripts.release_verification.candidate_manifest import ArtifactEntry
from scripts.release_verification.candidate_manifest import BuildInputs
from scripts.release_verification.candidate_manifest import CandidateManifest
from scripts.release_verification.candidate_manifest import SourceIdentity
from scripts.release_verification.candidate_manifest import Toolchains
from scripts.release_verification.candidate_manifest import expected_inventory
from scripts.release_verification.candidate_manifest import npm_package_path
from scripts.acceptance.evidence.source import VerificationError


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
    keyed = False
    if isinstance(value, list) and len(value) == 1:
        entry = value[0]
    elif isinstance(value, dict) and set(value) == {PACKAGE_NAME}:
        keyed = True
        entry = value[PACKAGE_NAME]
    else:
        raise VerificationError("npm_pack_output_invalid", STAGE)
    expected = npm_package_path(version)
    if not isinstance(entry, dict) or entry.get("filename") != expected:
        raise VerificationError("npm_pack_output_invalid", STAGE)
    identities = {"name": PACKAGE_NAME, "version": version, "id": f"{PACKAGE_NAME}@{version}"}
    # Legacy reports may omit identity fields; their exact filename still
    # binds name/version. npm 12's keyed report requires explicit identity.
    if any((field in entry or keyed and field != "id") and entry.get(field) != identity
           for field, identity in identities.items()):
        raise VerificationError("npm_pack_output_invalid", STAGE)
    return expected


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


def build_inputs(*, config_sha256: str, go_sum_sha256: str,
                 go: str, goreleaser: str, python: str, node: str) -> BuildInputs:
    return BuildInputs(
        goreleaser_config_sha256=config_sha256,
        go_sum_sha256=go_sum_sha256,
        toolchains=Toolchains(go=go, goreleaser=goreleaser, python=python, node=node))
