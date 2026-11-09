"""Immutable installation observations and pure rules for testing distribution channels."""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum
from pathlib import Path
import re
from typing import Protocol
from scripts.acceptance.evidence.source import text
from scripts.release_verification.candidate_manifest import CHECKSUMS_PATH
from scripts.release_verification.candidate_manifest import ArtifactKind
# EXPECTED_DEFAULT_STATE is re-exported: the shared contract owns it for every install probe.
from scripts.release_verification.contract import EXPECTED_DEFAULT_STATE as EXPECTED_DEFAULT_STATE
from scripts.release_verification.contract import TAG_RE
from scripts.release_verification.contract import InstalledExecutables
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.contract import expected_archive
from scripts.release_verification.published_contract import NPM_PACKAGE


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
    """One verified candidate distribution and its expected skill files."""

    tag: str
    repo: str
    installer: Path
    files: CandidateFiles
    skill: tuple[tuple[str, str], ...]

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


# The only facts of one installed bootstrap turn that install acceptance admits.
BOOTSTRAP_OUTCOME = "committed"


BOOTSTRAP_WORKER_RESULT = "success"


BOOTSTRAP_TERMINATION_CAUSE = "completed"


BOOTSTRAP_SHUTDOWN = "daemon-exited-endpoint-closed"


# Root and task identities are opaque to this contract; only their shape is checked.
_BOOTSTRAP_ID_RE = re.compile(r"[0-9a-f]{32}")


@dataclass(frozen=True)
class BootstrapTarget:
    """The installed layout one bootstrap probe exercises, after the channel's archive and layout were verified.

    ``command_directory`` is the only installed directory on the probe's PATH and
    ``delegate`` the installed entry that ``delegate`` must resolve to there.
    ``supervisor_directory`` holds the supervisor pair the installation ships: the
    command directory itself for an archive install, the keg's libexec for Homebrew,
    which never joins PATH. ``binaries`` are this installation's selected paths.
    """

    command_directory: Path
    delegate: Path
    supervisor_directory: Path
    binaries: InstalledExecutables


@dataclass(frozen=True)
class BootstrapObservation:
    """Compact facts of one fixture task run through an installed default path.

    ``root_id`` and ``task_id`` are the identities that admission, status, collection
    and the task's own managed lifetime closure all reported. ``binaries`` are the
    observed installed paths of the executed delegate, the runner the task recorded and the
    supervisor pair its state root selected. The remaining fields are the collected
    verdict, the task's worker Wait result, its termination cause and empty scope, and
    the verified daemon exit with endpoint closure.
    """

    root_id: str
    task_id: str
    binaries: InstalledExecutables
    outcome: str
    worker_result: str
    termination_cause: str
    scope_empty: bool
    shutdown: str

    def __post_init__(self) -> None:
        if (not isinstance(self.binaries, InstalledExecutables) or type(self.scope_empty) is not bool
                or not all(isinstance(value, str) for value in (
                    self.outcome, self.worker_result, self.termination_cause, self.shutdown))):
            raise VerificationError("install_bootstrap_invalid", STAGE)
        text(self.root_id, _BOOTSTRAP_ID_RE, STAGE)
        text(self.task_id, _BOOTSTRAP_ID_RE, STAGE)

    def to_record(self) -> dict[str, object]:
        return {
            "root_id": self.root_id, "task_id": self.task_id, "binaries": self.binaries.to_record(),
            "outcome": self.outcome, "worker_result": self.worker_result,
            "termination_cause": self.termination_cause, "scope_empty": self.scope_empty,
            "shutdown": self.shutdown,
        }


class BootstrapProbe(Protocol):
    """The consumer-owned port that runs one installed default fixture turn.

    It returns only after the task's closure and the daemon's shutdown were verified
    and raises VerificationError otherwise. The supplier owns every resource it
    creates and decides their retention; the consumer only supplies the target.
    """

    def __call__(self, target: BootstrapTarget) -> BootstrapObservation: ...


def require_bootstrap(observation: object, binaries: InstalledExecutables) -> BootstrapObservation:
    """Accept only a committed, closed and shut-down turn of exactly ``binaries``."""
    if (not isinstance(observation, BootstrapObservation) or observation.binaries != binaries
            or (observation.outcome, observation.worker_result, observation.termination_cause,
                observation.scope_empty, observation.shutdown) != (
                    BOOTSTRAP_OUTCOME, BOOTSTRAP_WORKER_RESULT, BOOTSTRAP_TERMINATION_CAUSE, True,
                    BOOTSTRAP_SHUTDOWN)):
        raise VerificationError("install_bootstrap_mismatch", STAGE)
    return observation


@dataclass(frozen=True)
class InstallObservation:
    """Compact, secret-free facts of one installation channel.

    ``installer_sha256`` is the digest of the installer bytes this channel
    executed; channels of one run may execute different installers.
    ``bootstrap`` is the channel's installed default fixture turn.
    """

    channel: str
    requests: tuple[RequestRecord, ...]
    binaries: InstalledExecutables
    skill_files: int
    delegate_version: str
    default_state: tuple[str, str, str]
    bootstrap: BootstrapObservation
    installer_sha256: str | None = None
    onboarding: dict[str, object] | None = None


@dataclass(frozen=True)
class CandidateInstallFacts:
    """The observations of one install run; ``platform`` is the (os, arch) of the installing host."""

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
                    "binaries": item.binaries.to_record(),
                    "skill_files": item.skill_files,
                    "delegate_version": item.delegate_version,
                    "default_state": list(item.default_state),
                    "bootstrap": item.bootstrap.to_record(),
                    "onboarding": item.onboarding,
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
    """Shape checks only; the caller validates the distribution payload."""
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



STAGE = "candidate-install"
INSTALLER_REPO_PATH = "install.sh"


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
        frozenset({ArtifactKind.ARCHIVE, ArtifactKind.CHECKSUMS, ArtifactKind.NPM_PACKAGE}), None, True),
    InstallChannel.NPM: ChannelRule(
        frozenset({ArtifactKind.ARCHIVE, ArtifactKind.CHECKSUMS, ArtifactKind.NPM_PACKAGE}),
        ArtifactKind.NPM_PACKAGE, True),
    InstallChannel.BREW: ChannelRule(
        frozenset({ArtifactKind.ARCHIVE, ArtifactKind.HOMEBREW_FORMULA, ArtifactKind.NPM_PACKAGE}),
        ArtifactKind.HOMEBREW_FORMULA, False),
}
