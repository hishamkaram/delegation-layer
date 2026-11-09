"""Install the exact unpublished candidate through the original shell and npm installers.

The caller owns candidate validation, the receipt and the workspace lifetime; this module
only observes installations. Every installer and default-path child runs through
ConsumerPorts.run in a scrubbed environment whose PATH starts with a curl shim, so
install.sh (directly or as the packaged install-cli child) can fetch only the two intended
release URLs, served from local files. After the installed layout and the fresh default
state match, the caller's required BootstrapProbe runs one fixture task through the
installed delegate; that probe, not this module, owns its children and resources.
Any failure leaves the workspace to the caller's retain-for-investigation path.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
import os
import secrets
from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.evidence.digests import sha
from scripts.release_verification.assets import exercise_default_path
from scripts.release_verification.assets import inspect_binaries
from scripts.release_verification.assets import platform_identity
from scripts.release_verification.assets import remember_directory
from scripts.release_verification.candidate_install_contract import CHANNEL_BREW
from scripts.release_verification.candidate_install_contract import CHANNEL_NPM
from scripts.release_verification.candidate_install_contract import CHANNEL_SHELL
from scripts.release_verification.candidate_install_contract import WITNESS_ROLE_KINDS
from scripts.release_verification.candidate_install_contract import BootstrapProbe
from scripts.release_verification.candidate_install_contract import BootstrapTarget
from scripts.release_verification.candidate_install_contract import CandidateInstallFacts
from scripts.release_verification.candidate_install_contract import CandidateInstallRequest
from scripts.release_verification.candidate_install_contract import InstallObservation
from scripts.release_verification.candidate_install_contract import ReleaseUrls
from scripts.release_verification.candidate_install_contract import package_command_args
from scripts.release_verification.candidate_install_contract import reject_denied
from scripts.release_verification.candidate_install_contract import release_urls
from scripts.release_verification.candidate_install_contract import require_bootstrap
from scripts.release_verification.candidate_install_contract import require_requests
from scripts.release_verification.candidate_install_contract import url_served_files
from scripts.release_verification.candidate_install_contract import validate_request
from scripts.release_verification.candidate_install_brew import observe_brew_install
from scripts.release_verification.candidate_install_brew import require_brew_host
from scripts.release_verification.candidate_install_setup import CandidateSetup
from scripts.release_verification.candidate_install_shim import CurlShim
from scripts.release_verification.candidate_install_shim import prepare_shim
from scripts.release_verification.candidate_manifest import entry_limit
from scripts.acceptance.evidence.source import MAX_SOURCE_FILE
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.contract import installed_executables
from scripts.release_verification.filesystem import bounded_digest
from scripts.release_verification.published_contract import NPM_PACKAGE
from scripts.release_verification.published_contract import UNRESOLVED_OWNERSHIP_CODES
from scripts.release_verification.published_contract import compare_hashes
from scripts.release_verification.published_contract import npm_environment
from scripts.release_verification.published_files import observe_installed_skill
from scripts.release_verification.published_files import require_installed_skill
from scripts.release_verification.published_files import read_package
from scripts.release_verification.published_files import read_package_bytes
from scripts.release_verification.published_files import require_exact_binaries
from scripts.release_verification.published_ports import SYSTEM_PATH
from scripts.release_verification.published_ports import ConsumerLayout
from scripts.release_verification.published_ports import ConsumerPorts
from scripts.release_verification.published_ports import prepare_layout
from scripts.release_verification.published_ports import tool_path


CHANNELS = (CHANNEL_SHELL, CHANNEL_NPM)


# Brew is observed only on explicit request, on a fresh disposable supported Homebrew host.
SUPPORTED_CHANNELS = (*CHANNELS, CHANNEL_BREW)


@dataclass(frozen=True)
class _Channel:
    """Everything the children of one installation channel share.

    ``info`` is the observed host platform; ``installer_sha256`` is the digest
    of the installer bytes this channel executes, observed before any child.
    """

    name: str
    ports: ConsumerPorts
    probe: BootstrapProbe
    receipt: dict
    request: CandidateInstallRequest
    urls: ReleaseUrls
    info: dict[str, str]
    layout: ConsumerLayout
    base: Path
    shim: CurlShim
    installer_sha256: str
    onboarding: bool = False

    @property
    def bin_dir(self) -> Path:
        return self.base / "bin"

    @property
    def skill_dir(self) -> Path:
        return self.base / "skill"

    def run(self, label: str, argv: list[str], env: dict[str, str], *, timeout: int) -> None:
        try:
            self.ports.run(
                self.receipt, f"candidate-{self.name}-{label}", argv, env,
                timeout=timeout, cwd=self.layout.cwd)
        except VerificationError as error:
            if error.code not in UNRESOLVED_OWNERSHIP_CODES:
                reject_denied(self.shim.requests())
            raise


def open_workspace(*, owner: DirectoryOwner, parent: Path, receipt: dict) -> ConsumerLayout:
    """Create the one owned private workspace; the caller retains or removes it via the owner."""
    workspace = parent / f"delegation-layer-candidate-install-{secrets.token_hex(6)}"
    token = owner.create("candidate install workspace", workspace, parent)
    remember_directory(receipt, token)
    return prepare_layout(workspace.resolve())


def _candidate_digests(request: CandidateInstallRequest) -> tuple[tuple[str, str], ...]:
    return tuple(
        (role, bounded_digest(path, entry_limit(WITNESS_ROLE_KINDS[role]), "candidate_file_invalid"))
        for role, path in request.files.roles())


def _installer_digest(request: CandidateInstallRequest) -> str:
    """The digest of the shell installer bytes; the caller keeps the file immutable while it runs."""
    return bounded_digest(request.installer, MAX_SOURCE_FILE, "candidate_file_invalid")


def _require_package_identity(request: CandidateInstallRequest) -> str:
    """The local tarball is the intended package and ships exactly the expected skill files.

    Returns the digest of the exact package bytes that were checked.
    """
    if request.files.package is None:
        raise VerificationError("candidate_package_missing", "verify-package")
    data = read_package_bytes(request.files.package)
    contents = read_package(data)
    if (contents.manifest.get("name") != NPM_PACKAGE
            or contents.manifest.get("version") != request.package_version()):
        raise VerificationError("npm_pack_manifest_mismatch", "verify-package")
    compare_hashes(request.skill_digests(), contents.skill_digests,
                   "npm_pack_skill_mismatch", "verify-package-skill")
    return sha(data)


def _installer_digests(request: CandidateInstallRequest, channels: tuple[str, ...],
                       candidate: tuple[tuple[str, str], ...]) -> dict[str, str]:
    """Observe, before any child, the installer bytes each selected channel executes.

    The npm channel executes the packaged installer, so its identity is the whole
    package, which must be the bytes the candidate file observation recorded.
    """
    installers = {}
    if CHANNEL_NPM in channels:
        installers[CHANNEL_NPM] = _require_package_identity(request)
        if installers[CHANNEL_NPM] != dict(candidate)["package"]:
            raise VerificationError("candidate_file_changed", "verify-candidate")
    if CHANNEL_SHELL in channels:
        installers[CHANNEL_SHELL] = _installer_digest(request)
    if CHANNEL_BREW in channels and "formula" not in dict(candidate):
        raise VerificationError("candidate_formula_missing", "validate-candidate")
    return installers


def _open_channel(
        name: str, *, ports: ConsumerPorts, probe: BootstrapProbe, receipt: dict, request: CandidateInstallRequest,
        urls: ReleaseUrls, info: dict[str, str], layout: ConsumerLayout, installer_sha256: str,
        onboarding: bool = False) -> _Channel:
    base = layout.root / name
    base.mkdir(mode=0o700)
    shim = prepare_shim(base, url_served_files(request, urls))
    return _Channel(
        name=name, ports=ports, probe=probe, receipt=receipt, request=request, urls=urls, info=info,
        layout=layout, base=base, shim=shim, installer_sha256=installer_sha256, onboarding=onboarding)


def _shell_environment(channel: _Channel) -> dict[str, str]:
    home = str(channel.layout.home)
    return {
        "PATH": os.pathsep.join([str(channel.shim.directory), *SYSTEM_PATH]),
        "HOME": home, "TMPDIR": home, "LANG": "C", "LC_ALL": "C",
        "DELEGATION_LAYER_VERSION": channel.request.tag,
        "DELEGATION_LAYER_INSTALL_DIR": str(channel.bin_dir),
        **channel.shim.environment(),
    }


def _npm_environment(channel: _Channel) -> dict[str, str]:
    layout = channel.layout
    path = os.pathsep.join([str(channel.shim.directory), tool_path(channel.ports, ("npm", "node"))])
    return {
        **npm_environment(
            path=path, home=str(layout.home), cache=str(layout.cache),
            user_npmrc=str(layout.user_npmrc), global_npmrc=str(layout.global_npmrc),
            inherited=channel.ports.inherited),
        "npm_config_offline": "true", **channel.shim.environment(),
    }


def _install_shell(channel: _Channel) -> None:
    shell = channel.ports.which("sh")
    if shell is None:
        raise VerificationError("sh_unavailable", "locate-tools")
    channel.run("install", [shell, str(channel.request.installer)], _shell_environment(channel), timeout=300)


def _setup(channel: _Channel) -> CandidateSetup:
    return CandidateSetup(channel.request, channel.ports, channel.receipt, channel.layout, channel.name,
                          channel.base, channel.bin_dir,
                          {**channel.shim.environment(), "PATH": _npm_environment(channel)["PATH"]})


def _install_npm(channel: _Channel) -> None:
    if channel.onboarding:
        _setup(channel).fresh()
        return
    npm = channel.ports.which("npm")
    if npm is None:
        raise VerificationError("npm_unavailable", "locate-tools")
    env = _npm_environment(channel)
    package = channel.request.files.package
    if package is None:
        raise VerificationError("candidate_package_missing", "verify-package")
    install_cli = ["install-cli", "--version", channel.request.tag, "--install-dir", str(channel.bin_dir)]
    channel.run("install-cli", [npm, *package_command_args(package, install_cli)], env, timeout=600)
    channel.run("skill", [npm, *package_command_args(package, ["--target", str(channel.skill_dir)])],
                env, timeout=180)


def _observe(channel: _Channel) -> InstallObservation:
    """Bind served requests, installed binaries and skill files, then the default-path checks and turn."""
    requests = channel.shim.requests()
    require_requests(requests, channel.urls)
    require_exact_binaries(channel.bin_dir)
    scratch: dict = {"binaries": {}, "commands": channel.receipt["commands"]}
    paths = inspect_binaries(channel.bin_dir, scratch)
    installed = installed_executables(paths)
    skill_files = 0
    if channel.name == CHANNEL_NPM:
        skill_directory = (_setup(channel).home / ".claude" / "skills" / "agent-integration"
                           if channel.onboarding else channel.skill_dir)
        skill = observe_installed_skill(skill_directory)
        require_installed_skill(channel.request.skill_digests(), skill, "installed_skill_mismatch", "verify-skill")
        skill_files = len(skill.files)
    home = channel.base / "exercise-home"
    home.mkdir(mode=0o700)
    exercise_default_path(
        paths, channel.bin_dir, scratch, channel.request.package_version(), channel.info, home,
        cwd=channel.layout.cwd)
    state = scratch["default_path_observation"]
    onboarding = None
    if channel.onboarding:
        setup = _setup(channel)
        if channel.name != CHANNEL_NPM:
            setup.fresh()
        onboarding = setup.verify_repeat(paths["delegate"])
        require_requests(channel.shim.requests(), channel.urls)
    # The archive layout ships the supervisor pair beside delegate, in the one PATH directory.
    expected = installed
    bootstrap = require_bootstrap(channel.probe(BootstrapTarget(
        command_directory=channel.bin_dir, delegate=paths["delegate"],
        supervisor_directory=channel.bin_dir, binaries=expected)), expected)
    return InstallObservation(
        channel=channel.name, requests=requests, binaries=installed,
        skill_files=skill_files, delegate_version=scratch["delegate_build_identity"]["version"],
        default_state=(state["state_before"], state["observed_state"], state["state_after"]),
        bootstrap=bootstrap, installer_sha256=channel.installer_sha256, onboarding=onboarding)


def verify_candidate_install(
        request: CandidateInstallRequest, *, ports: ConsumerPorts, probe: BootstrapProbe, receipt: dict,
        layout: ConsumerLayout, info: dict[str, str],
        channels: tuple[str, ...] = CHANNELS, onboarding: bool = False) -> CandidateInstallFacts:
    """Install the candidate per channel and prove binaries, skill and default-path behavior.

    ``onboarding`` adds packed setup to this reusable observer; the production proof
    always enables it. Legacy distribution-only consumers can retain their narrower scope.
    ``probe`` runs each channel's installed default fixture turn only after that
    channel's selected layout and fresh default state matched; it is never skipped.
    ``info`` is the caller's expected platform; the facts record the observed host
    platform, and a mismatch is refused before any child. Raises VerificationError with
    the failing code; the caller must retain the workspace on every failure (a timed-out
    child keeps unresolved ownership). Tools a channel does not use are never required.
    """
    if (not channels or any(name not in SUPPORTED_CHANNELS for name in channels)
            or len(set(channels)) != len(channels)):
        raise VerificationError("candidate_channels_invalid", "validate-channels")
    validate_request(request)
    before = _candidate_digests(request)
    if onboarding and _require_package_identity(request) != dict(before).get("package"):
        raise VerificationError("candidate_file_changed", "verify-candidate")
    installers = _installer_digests(request, channels, before)
    actual = platform_identity()
    if (info.get("os"), info.get("arch")) != (actual["os"], actual["arch"]):
        raise VerificationError("candidate_platform_mismatch", "inspect-platform")
    if CHANNEL_BREW in channels:
        require_brew_host(actual["os"])
    urls = release_urls(request, actual)
    observed = []
    for name in channels:
        if name == CHANNEL_BREW:
            receipt["stage"] = "candidate-install-brew"
            observed.append(observe_brew_install(
                request, ports=ports, probe=probe, receipt=receipt, layout=layout, info=actual,
                onboarding=onboarding))
            continue
        channel = _open_channel(
            name, ports=ports, probe=probe, receipt=receipt, request=request, urls=urls, info=actual,
            layout=layout, installer_sha256=installers[name], onboarding=onboarding)
        receipt["stage"] = f"candidate-install-{name}"
        (_install_shell if name == CHANNEL_SHELL else _install_npm)(channel)
        observed.append(_observe(channel))
    if _candidate_digests(request) != before or (
            CHANNEL_SHELL in installers and _installer_digest(request) != installers[CHANNEL_SHELL]):
        raise VerificationError("candidate_file_changed", "verify-candidate")
    return CandidateInstallFacts(
        request.tag, before, tuple(observed), platform=(actual["os"], actual["arch"]))
