"""Immutable facts and pure rules for pre-publication candidate install proof.

The witness types record what one install run observed. An ``InstallProof``
binds one channel's success on one platform to exact candidate artifacts and
to the installer bytes actually executed; ``read_install_proof`` is the only
reader that grants it authority, after cross-checking the witness bytes.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum
from pathlib import Path
import re
from ..evidence.digests import sha
from .candidate_codec import (
    VERSION_RE, encode_record, exact_fields, load_record, non_negative_int, object_list,
    require_header, revision, sha256_hex, text,
)
from .candidate_manifest import (
    CHECKSUMS_PATH, PLATFORMS, ArtifactEntry, ArtifactKind, CandidateManifest, _safe_relative_path,
    encode_manifest,
)
# EXPECTED_DEFAULT_STATE is re-exported: the shared contract owns it for every install probe.
from .contract import (
    EXPECTED_DEFAULT_STATE, TAG_RE, BinaryDigests, VerificationError, expected_archive, parse_binary_digests,
)
from .published_contract import NPM_PACKAGE


REQUEST_SERVED = "served"


REQUEST_DENIED = "denied"


class InstallChannel(Enum):
    SHELL = "shell"
    NPM = "npm"
    BREW = "brew"


CHANNEL_SHELL = InstallChannel.SHELL.value


CHANNEL_NPM = InstallChannel.NPM.value


CHANNEL_BREW = InstallChannel.BREW.value


_REPOSITORY_RE = re.compile(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+")


@dataclass(frozen=True)
class CandidateFiles:
    """The caller-validated local files that stand in for the future release assets.

    Each channel supplies only the files it consumes: Homebrew reads the
    archive through its formula and never fetches the checksums.
    """

    archive: Path
    checksums: Path | None = None
    package: Path | None = None
    formula: Path | None = None

    def roles(self) -> tuple[tuple[str, Path], ...]:
        roles = (("archive", self.archive), ("checksums", self.checksums), ("package", self.package),
                 ("formula", self.formula))
        return tuple((role, path) for role, path in roles if path is not None)


# The candidate manifest kind of each witness file role.
WITNESS_ROLE_KINDS = {
    "archive": ArtifactKind.ARCHIVE, "checksums": ArtifactKind.CHECKSUMS,
    "package": ArtifactKind.NPM_PACKAGE, "formula": ArtifactKind.HOMEBREW_FORMULA,
}


@dataclass(frozen=True)
class CandidateInstallRequest:
    """One exact candidate; hashes come from the caller's already verified candidate."""

    tag: str
    repo: str
    installer: Path
    files: CandidateFiles
    binaries: tuple[tuple[str, str], ...]
    skill: tuple[tuple[str, str], ...]

    def binary_hashes(self) -> dict[str, str]:
        return dict(self.binaries)

    def skill_digests(self) -> dict[str, str]:
        return dict(self.skill)

    def package_version(self) -> str:
        match = TAG_RE.fullmatch(self.tag)
        if match is None:
            raise VerificationError("invalid_release_tag", "validate-tag")
        return match.group(1)


@dataclass(frozen=True)
class ReleaseUrls:
    """The only two URLs the installer may request for the intended tag."""

    archive: str
    checksums: str

    def expected(self) -> tuple[str, str]:
        return (self.archive, self.checksums)


@dataclass(frozen=True)
class RequestRecord:
    outcome: str
    url: str


@dataclass(frozen=True)
class InstallObservation:
    """Compact, secret-free facts of one installation channel.

    ``installer_sha256`` is the digest of the installer bytes this channel
    executed; channels of one run may execute different installers.
    """

    channel: str
    requests: tuple[RequestRecord, ...]
    binaries: tuple[tuple[str, str], ...]
    skill_files: int
    delegate_version: str
    default_state: tuple[str, str, str]
    installer_sha256: str | None = None


@dataclass(frozen=True)
class CandidateInstallFacts:
    """The witness of one install run; ``platform`` is the (os, arch) of the installing host."""

    tag: str
    candidate_files: tuple[tuple[str, str], ...]
    installs: tuple[InstallObservation, ...]
    platform: tuple[str, str] | None = None

    def to_record(self) -> dict:
        return {
            "tag": self.tag,
            "platform": None if self.platform is None else {"os": self.platform[0], "arch": self.platform[1]},
            "candidate_files": dict(self.candidate_files),
            "installs": [
                {
                    "channel": item.channel,
                    "installer_sha256": item.installer_sha256,
                    "requests": [{"outcome": r.outcome, "url": r.url} for r in item.requests],
                    "binaries": dict(item.binaries),
                    "skill_files": item.skill_files,
                    "delegate_version": item.delegate_version,
                    "default_state": list(item.default_state),
                }
                for item in self.installs
            ],
        }


def require_repository(repo: object, stage: str) -> str:
    """Return a GitHub owner/name; a release URL never selects the repository."""
    if not isinstance(repo, str) or _REPOSITORY_RE.fullmatch(repo) is None:
        raise VerificationError("repository_invalid", stage)
    return repo


def validate_request(request: CandidateInstallRequest) -> None:
    """Shape checks only; candidate validity and readiness stay with the caller."""
    request.package_version()
    require_repository(request.repo, "validate-repository")
    for path in (request.installer, *(path for _, path in request.files.roles())):
        if not path.is_absolute() or any(char in str(path) for char in "\t\r\n\0"):
            raise VerificationError("candidate_path_invalid", "validate-candidate")


def release_url_pair(repo: str, tag: str, archive: str) -> ReleaseUrls:
    base = f"https://github.com/{repo}/releases/download/{tag}/"
    return ReleaseUrls(base + archive, base + CHECKSUMS_PATH)


def release_urls(request: CandidateInstallRequest, info: dict[str, str]) -> ReleaseUrls:
    return release_url_pair(request.repo, request.tag, expected_archive(info, request.tag))


def url_served_files(request: CandidateInstallRequest, urls: ReleaseUrls) -> dict[str, Path]:
    """Exact URL to local file map; nothing else is ever answered."""
    if request.files.checksums is None:
        raise VerificationError("candidate_checksums_missing", "validate-candidate")
    return {urls.archive: request.files.archive, urls.checksums: request.files.checksums}


def parse_request_log(raw: bytes) -> tuple[RequestRecord, ...]:
    try:
        lines = raw.decode("utf-8").splitlines()
    except UnicodeError:
        raise VerificationError("candidate_request_log_invalid", "verify-requests") from None
    records = []
    for line in lines:
        outcome, separator, url = line.partition("\t")
        if (not separator or outcome not in {REQUEST_SERVED, REQUEST_DENIED}
                or not url or any(char.isspace() for char in url)):
            raise VerificationError("candidate_request_log_invalid", "verify-requests")
        records.append(RequestRecord(outcome, url))
    return tuple(records)


def reject_denied(records: tuple[RequestRecord, ...]) -> None:
    if any(record.outcome == REQUEST_DENIED for record in records):
        raise VerificationError("candidate_url_denied", "verify-requests")


def require_requests(records: tuple[RequestRecord, ...], urls: ReleaseUrls) -> None:
    """The installer downloaded exactly the archive then the checksums, with no refusal."""
    reject_denied(records)
    if records != tuple(RequestRecord(REQUEST_SERVED, url) for url in urls.expected()):
        raise VerificationError("candidate_requests_unexpected", "verify-requests")


def package_command_args(package: Path, extra: list[str]) -> list[str]:
    """npm exec arguments for the exact local package; extra are the package CLI arguments."""
    return ["exec", "--yes", f"--package={package}", "--", NPM_PACKAGE, *extra]


PROOF_STAGE = "validate-candidate-install-proof"


INSTALL_PROOF_SCHEMA_VERSION = 1


INSTALL_PROOF_KIND = "delegation-layer-candidate-install-proof"


MAX_INSTALL_PROOF_BYTES = 8 * 1024


MAX_INSTALL_WITNESS_BYTES = 32 * 1024


# The shell installer is the source commit's own install.sh, never a built artifact.
INSTALLER_REPO_PATH = "install.sh"


@dataclass(frozen=True)
class ArtifactRef:
    """One candidate manifest artifact a channel consumed, by its exact identity."""

    kind: ArtifactKind
    path: str
    sha256: str

    def __post_init__(self) -> None:
        if not isinstance(self.kind, ArtifactKind) or not _safe_relative_path(self.path):
            raise VerificationError("install_artifact_ref_invalid", PROOF_STAGE)
        sha256_hex(self.sha256, PROOF_STAGE)

    def matches(self, entry: ArtifactEntry) -> bool:
        return (entry.kind, entry.path, entry.sha256) == (self.kind, self.path, self.sha256)


@dataclass(frozen=True)
class SourceFileBinding:
    """An installer executed from the exact bytes of a file at the candidate source commit."""

    source_commit: str
    repo_path: str
    sha256: str

    def __post_init__(self) -> None:
        revision(self.source_commit, PROOF_STAGE)
        sha256_hex(self.sha256, PROOF_STAGE)
        if not _safe_relative_path(self.repo_path):
            raise VerificationError("install_binding_invalid", PROOF_STAGE)


@dataclass(frozen=True)
class ManifestArtifactBinding:
    """An installer executed from a candidate artifact: the npm package or the formula."""

    artifact: ArtifactRef

    def __post_init__(self) -> None:
        if not isinstance(self.artifact, ArtifactRef):
            raise VerificationError("install_binding_invalid", PROOF_STAGE)

    @property
    def sha256(self) -> str:
        return self.artifact.sha256


InstallerBinding = SourceFileBinding | ManifestArtifactBinding


@dataclass(frozen=True)
class HarnessIdentity:
    """The commit and source fingerprint of the harness that executed the install."""

    commit: str
    sha256: str

    def __post_init__(self) -> None:
        revision(self.commit, PROOF_STAGE)
        sha256_hex(self.sha256, PROOF_STAGE)


@dataclass(frozen=True)
class ChannelRule:
    """The exact artifact kinds a channel consumes and how its installer is bound.

    ``installer_kind`` is None when the installer is the source commit's
    install.sh; ``fetches_release`` is whether the installer requests the
    release archive and checksums URLs.
    """

    consumed: frozenset[ArtifactKind]
    installer_kind: ArtifactKind | None
    fetches_release: bool


CHANNEL_RULES = {
    InstallChannel.SHELL: ChannelRule(
        frozenset({ArtifactKind.ARCHIVE, ArtifactKind.CHECKSUMS}), None, True),
    InstallChannel.NPM: ChannelRule(
        frozenset({ArtifactKind.ARCHIVE, ArtifactKind.CHECKSUMS, ArtifactKind.NPM_PACKAGE}),
        ArtifactKind.NPM_PACKAGE, True),
    InstallChannel.BREW: ChannelRule(
        frozenset({ArtifactKind.ARCHIVE, ArtifactKind.HOMEBREW_FORMULA}),
        ArtifactKind.HOMEBREW_FORMULA, False),
}


def _binding_follows(rule: ChannelRule, binding: object, consumed: tuple[ArtifactRef, ...]) -> bool:
    if rule.installer_kind is None:
        return isinstance(binding, SourceFileBinding) and binding.repo_path == INSTALLER_REPO_PATH
    return (isinstance(binding, ManifestArtifactBinding) and binding.artifact.kind is rule.installer_kind
            and binding.artifact in consumed)


@dataclass(frozen=True)
class InstallProof:
    """One successful candidate install on one platform through one channel.

    ``consumed`` is sorted by path and holds exactly the channel's artifact
    kinds; ``witness_sha256`` is the digest of the exact witness bytes.
    """

    candidate_id: str
    os: str
    arch: str
    channel: InstallChannel
    consumed: tuple[ArtifactRef, ...]
    installer: InstallerBinding
    harness: HarnessIdentity
    binaries: BinaryDigests
    delegate_version: str
    default_state: tuple[str, str, str]
    witness_sha256: str

    def __post_init__(self) -> None:
        sha256_hex(self.candidate_id, PROOF_STAGE)
        sha256_hex(self.witness_sha256, PROOF_STAGE)
        text(self.delegate_version, VERSION_RE, PROOF_STAGE)
        rule = CHANNEL_RULES[self.channel] if isinstance(self.channel, InstallChannel) else None
        if (rule is None or (self.os, self.arch) not in PLATFORMS or not isinstance(self.consumed, tuple)
                or not all(isinstance(ref, ArtifactRef) for ref in self.consumed)
                or [ref.path for ref in self.consumed] != sorted({ref.path for ref in self.consumed})
                or len(self.consumed) != len(rule.consumed)
                or {ref.kind for ref in self.consumed} != rule.consumed
                or not _binding_follows(rule, self.installer, self.consumed)
                or not isinstance(self.harness, HarnessIdentity)
                or not isinstance(self.binaries, BinaryDigests)
                or self.default_state != EXPECTED_DEFAULT_STATE):
            raise VerificationError("install_proof_invalid", PROOF_STAGE)

    @property
    def platform(self) -> tuple[str, str]:
        return (self.os, self.arch)

    @property
    def sha256(self) -> str:
        """The digest of the canonical proof bytes; it binds ``witness_sha256`` through them."""
        return sha(encode_install_proof(self))

    def consumed_ref(self, kind: ArtifactKind) -> ArtifactRef:
        return next(ref for ref in self.consumed if ref.kind is kind)


def _ref_record(ref: ArtifactRef) -> dict[str, str]:
    return {"kind": ref.kind.value, "path": ref.path, "sha256": ref.sha256}


def _installer_record(binding: InstallerBinding) -> dict[str, object]:
    if isinstance(binding, SourceFileBinding):
        return {"source_file": {"source_commit": binding.source_commit, "repo_path": binding.repo_path,
                                "sha256": binding.sha256}}
    return {"manifest_artifact": _ref_record(binding.artifact)}


def encode_install_proof(proof: InstallProof) -> bytes:
    return encode_record({
        "schema_version": INSTALL_PROOF_SCHEMA_VERSION, "kind": INSTALL_PROOF_KIND,
        "candidate_id": proof.candidate_id, "os": proof.os, "arch": proof.arch,
        "channel": proof.channel.value, "consumed": [_ref_record(ref) for ref in proof.consumed],
        "installer": _installer_record(proof.installer),
        "harness": {"commit": proof.harness.commit, "sha256": proof.harness.sha256},
        "binaries": proof.binaries.as_mapping(), "delegate_version": proof.delegate_version,
        "default_state": list(proof.default_state), "witness_sha256": proof.witness_sha256,
    }, PROOF_STAGE)


def encode_install_witness(facts: CandidateInstallFacts) -> bytes:
    """Return the canonical witness bytes whose digest an install proof binds."""
    return encode_record(facts.to_record(), PROOF_STAGE)


_PROOF_FIELDS = frozenset({
    "schema_version", "kind", "candidate_id", "os", "arch", "channel", "consumed", "installer",
    "harness", "binaries", "delegate_version", "default_state", "witness_sha256",
})


_REF_FIELDS = frozenset({"kind", "path", "sha256"})


def _parse_ref(value: object) -> ArtifactRef:
    fields = exact_fields(value, _REF_FIELDS, PROOF_STAGE)
    try:
        kind = ArtifactKind(fields["kind"])
    except ValueError:
        raise VerificationError("install_artifact_ref_invalid", PROOF_STAGE) from None
    return ArtifactRef(kind=kind, path=fields["path"], sha256=fields["sha256"])


def _parse_channel(value: object) -> InstallChannel:
    try:
        return InstallChannel(value)
    except ValueError:
        raise VerificationError("install_proof_invalid", PROOF_STAGE) from None


def _parse_installer(value: object) -> InstallerBinding:
    if isinstance(value, dict) and set(value) == {"source_file"}:
        fields = exact_fields(value["source_file"], frozenset({"source_commit", "repo_path", "sha256"}),
                              PROOF_STAGE)
        return SourceFileBinding(source_commit=fields["source_commit"], repo_path=fields["repo_path"],
                                 sha256=fields["sha256"])
    if isinstance(value, dict) and set(value) == {"manifest_artifact"}:
        return ManifestArtifactBinding(artifact=_parse_ref(value["manifest_artifact"]))
    raise VerificationError("install_binding_invalid", PROOF_STAGE)


def _parse_default_state(value: object) -> tuple[str, str, str]:
    if not isinstance(value, list) or len(value) != 3 or not all(isinstance(item, str) for item in value):
        raise VerificationError("install_proof_invalid", PROOF_STAGE)
    return (value[0], value[1], value[2])


def parse_install_proof(data: bytes) -> InstallProof:
    """Read the exact proof shape; this grants no authority (see ``read_install_proof``)."""
    value = load_record(data, max_bytes=MAX_INSTALL_PROOF_BYTES, stage=PROOF_STAGE)
    require_header(value, schema_version=INSTALL_PROOF_SCHEMA_VERSION, kind=INSTALL_PROOF_KIND,
                   error="install_proof_kind_invalid", stage=PROOF_STAGE)
    fields = exact_fields(value, _PROOF_FIELDS, PROOF_STAGE)
    harness = exact_fields(fields["harness"], frozenset({"commit", "sha256"}), PROOF_STAGE)
    return InstallProof(
        candidate_id=fields["candidate_id"], os=fields["os"], arch=fields["arch"],
        channel=_parse_channel(fields["channel"]),
        consumed=tuple(_parse_ref(item) for item in object_list(
            fields["consumed"], max_items=len(ArtifactKind), stage=PROOF_STAGE)),
        installer=_parse_installer(fields["installer"]),
        harness=HarnessIdentity(commit=harness["commit"], sha256=harness["sha256"]),
        binaries=parse_binary_digests(fields["binaries"], PROOF_STAGE),
        delegate_version=fields["delegate_version"],
        default_state=_parse_default_state(fields["default_state"]),
        witness_sha256=fields["witness_sha256"],
    )


def _require_manifest_artifacts(proof: InstallProof, manifest: CandidateManifest) -> None:
    entries = {entry.path: entry for entry in manifest.artifacts}
    for ref in proof.consumed:
        entry = entries.get(ref.path)
        if entry is None or not ref.matches(entry):
            raise VerificationError("install_artifact_unknown", PROOF_STAGE)
    if not proof.consumed_ref(ArtifactKind.ARCHIVE).matches(manifest.archive(proof.os, proof.arch)):
        raise VerificationError("install_archive_mismatch", PROOF_STAGE)


def _require_installer_source(proof: InstallProof, manifest: CandidateManifest,
                              installer_source: bytes | None) -> None:
    binding = proof.installer
    if not isinstance(binding, SourceFileBinding):
        if installer_source is not None:
            raise VerificationError("install_source_unexpected", PROOF_STAGE)
        return
    if binding.source_commit != manifest.source.commit:
        raise VerificationError("install_source_commit_mismatch", PROOF_STAGE)
    if not isinstance(installer_source, bytes) or sha(installer_source) != binding.sha256:
        raise VerificationError("install_source_mismatch", PROOF_STAGE)


_WITNESS_FIELDS = frozenset({"tag", "platform", "candidate_files", "installs"})


_OBSERVATION_FIELDS = frozenset({
    "channel", "installer_sha256", "requests", "binaries", "skill_files", "delegate_version",
    "default_state",
})


def _require_witness(proof: InstallProof, witness: bytes, *, tag: str, repository: str) -> None:
    """The exact witness bytes record this proof's single channel run on its platform.

    A release-fetching channel served exactly the archive then the checksums
    of ``tag`` from the caller's expected ``repository``.
    """
    if not isinstance(witness, bytes) or sha(witness) != proof.witness_sha256:
        raise VerificationError("install_witness_digest_mismatch", PROOF_STAGE)
    record = exact_fields(load_record(witness, max_bytes=MAX_INSTALL_WITNESS_BYTES, stage=PROOF_STAGE),
                          _WITNESS_FIELDS, PROOF_STAGE)
    installs = object_list(record["installs"], max_items=len(InstallChannel), stage=PROOF_STAGE)
    files = {role: ref.sha256 for role, kind in WITNESS_ROLE_KINDS.items()
             for ref in proof.consumed if ref.kind is kind}
    if (len(installs) != 1 or record["tag"] != tag or record["candidate_files"] != files
            or record["platform"] != {"os": proof.os, "arch": proof.arch}):
        raise VerificationError("install_witness_mismatch", PROOF_STAGE)
    observation = exact_fields(installs[0], _OBSERVATION_FIELDS, PROOF_STAGE)
    non_negative_int(observation["skill_files"], PROOF_STAGE)
    expected = {
        "channel": proof.channel.value, "installer_sha256": proof.installer.sha256,
        "binaries": proof.binaries.as_mapping(), "delegate_version": proof.delegate_version,
        "default_state": list(proof.default_state),
    }
    urls = release_url_pair(repository, tag, proof.consumed_ref(ArtifactKind.ARCHIVE).path).expected()
    requests = ([{"outcome": REQUEST_SERVED, "url": url} for url in urls]
                if CHANNEL_RULES[proof.channel].fetches_release else [])
    if observation["requests"] != requests or any(observation[key] != value for key, value in expected.items()):
        raise VerificationError("install_witness_mismatch", PROOF_STAGE)


def read_install_proof(proof: bytes, witness: bytes, *, manifest: CandidateManifest,
                       installer_source: bytes | None, repository: str) -> InstallProof:
    """Return a proof only when its candidate, artifacts, installer bytes and witness all bind.

    ``installer_source`` is the exact source-commit file a ``SourceFileBinding``
    names, read by the caller's I/O edge; it must be None for artifact bindings.
    ``repository`` is the caller's validated ``CandidateLocator.repository``.
    """
    require_repository(repository, PROOF_STAGE)
    value = parse_install_proof(proof)
    if not isinstance(manifest, CandidateManifest) or value.candidate_id != sha(encode_manifest(manifest)):
        raise VerificationError("install_proof_candidate_mismatch", PROOF_STAGE)
    _require_manifest_artifacts(value, manifest)
    _require_installer_source(value, manifest, installer_source)
    if value.delegate_version != manifest.version:
        raise VerificationError("install_proof_version_mismatch", PROOF_STAGE)
    _require_witness(value, witness, tag=manifest.build_tag, repository=repository)
    return value
