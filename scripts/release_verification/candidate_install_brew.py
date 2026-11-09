"""Observe one fresh Homebrew installation of the exact unpublished candidate formula.

The caller owns the validated request, the receipt, the workspace and its lifetime; this
module only observes and returns facts or raises the original failure. Every Homebrew and
default-path child runs through ConsumerPorts.run in a credential-free environment whose HOMEBREW_CACHE and
HOMEBREW_LOGS lie inside the caller's workspace. The formula keeps its original release
URL; Homebrew finds the archive only at the source cache path it reports, preseeded with the
exact candidate bytes, so a cache miss fails. Homebrew owns the new tap, the formula file
in it, its trust state and the keg on the disposable host; nothing here adopts, uninstalls
or removes them. When Brew's public command inventory advertises ``trust``, the harness asks
Brew to trust exactly the validated candidate formula in the private HOME, never a whole tap.
The caller's BootstrapProbe owns the children and resources of the installed fixture turn.
"""

from __future__ import annotations

from pathlib import Path
import json
import os
import secrets
import shutil
import stat
from scripts.release_verification.assets import exercise_binary_identity
from scripts.release_verification.assets import isolated_default_environment
from scripts.release_verification.assets import observe_default_state
from scripts.release_verification.candidate_install_contract import CHANNEL_BREW
from scripts.release_verification.candidate_install_contract import BootstrapObservation
from scripts.release_verification.candidate_install_contract import BootstrapProbe
from scripts.release_verification.candidate_install_contract import BootstrapTarget
from scripts.release_verification.candidate_install_contract import CandidateInstallRequest
from scripts.release_verification.candidate_install_contract import InstallObservation
from scripts.release_verification.candidate_install_contract import require_bootstrap
from scripts.release_verification.candidate_install_setup import CandidateSetup
from scripts.release_verification.candidate_install_shim import prepare_shim
from scripts.release_verification.candidate_manifest import ArtifactKind
from scripts.release_verification.candidate_manifest import entry_limit
from scripts.release_verification.contract import InstalledExecutables
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.contract import installed_executables
from scripts.acceptance.evidence.source import _reject_parent_components
from scripts.acceptance.evidence.source import _reject_symlink_components
from scripts.release_verification.filesystem import bounded_digest
from scripts.release_verification.filesystem import create_new_file
from scripts.release_verification.published_contract import HOMEBREW_FORMULA
from scripts.release_verification.published_contract import homebrew_environment
from scripts.release_verification.published_contract import homebrew_prefixes
from scripts.release_verification.published_contract import parse_single_line
from scripts.release_verification.published_homebrew import installed_layout
from scripts.release_verification.published_homebrew import keg_binaries
from scripts.release_verification.published_homebrew import require_fresh
from scripts.release_verification.published_ports import ConsumerLayout
from scripts.release_verification.published_ports import ConsumerPorts
from scripts.release_verification.published_ports import tool_path


STAGE = "candidate-install-brew"


_INSTALL_TIMEOUT = 900


_TEST_TIMEOUT = 300


def require_brew_host(os_name: str) -> None:
    """Refuse, before any child, a host OS that has no supported Homebrew prefix."""
    if not homebrew_prefixes(os_name):
        raise VerificationError("homebrew_host_unsupported", "homebrew-observe")


def _digest(path: Path, kind: ArtifactKind, code: str) -> str:
    return bounded_digest(path, entry_limit(kind), code)


def _real_directory(path: Path, code: str) -> Path:
    """An absolute directory reached through no symbolic link."""
    if not path.is_absolute():
        raise VerificationError(code, STAGE)
    _reject_parent_components(path, code)
    _reject_symlink_components(path, code)
    try:
        if not stat.S_ISDIR(path.lstat().st_mode):
            raise VerificationError(code, STAGE)
    except OSError:
        raise VerificationError(code, STAGE) from None
    return path


def _copy_exact(source: Path, destination: Path, digest: str, kind: ArtifactKind, code: str) -> None:
    """Stream ``source`` into a new file; the written bytes must hash to ``digest``."""
    descriptor = create_new_file(destination, code, stage=STAGE)
    with os.fdopen(descriptor, "wb") as output, source.open("rb") as data:
        shutil.copyfileobj(data, output, 1024 * 1024)
        output.flush()
        os.fsync(output.fileno())
    if _digest(destination, kind, code) != digest:
        raise VerificationError(code, STAGE)


def _place_formula(tap: Path, formula: Path, digest: str) -> Path:
    """Add the exact candidate formula bytes as a new file in the new tap's Formula directory."""
    directory = tap / "Formula"
    if not os.path.lexists(directory):
        directory.mkdir(mode=0o755)
    target = _real_directory(directory, "homebrew_tap_invalid") / f"{HOMEBREW_FORMULA}.rb"
    _copy_exact(formula, target, digest, ArtifactKind.HOMEBREW_FORMULA, "homebrew_tap_formula_conflict")
    return target


def _brew_commands(reply: bytes) -> frozenset[str]:
    """Brew's public command names; the proof needs at least ``install`` and ``tap-new``."""
    try:
        names = frozenset(reply.decode("utf-8").split())
    except UnicodeDecodeError:
        raise VerificationError("homebrew_commands_invalid", STAGE) from None
    if not {"install", "tap-new"} <= names:
        raise VerificationError("homebrew_commands_invalid", STAGE)
    return names


def _cache_target(cache: Path, reported: str) -> Path:
    """Brew's reported source cache file, refused unless strictly inside the owned cache.

    Parent and symbolic-link components are refused before any directory is created.
    """
    target = Path(reported)
    if not target.is_absolute():
        raise VerificationError("homebrew_cache_unexpected", STAGE)
    _reject_parent_components(target, "homebrew_cache_unexpected")
    if not target.is_relative_to(cache) or target == cache:
        raise VerificationError("homebrew_cache_unexpected", STAGE)
    _reject_symlink_components(target, "homebrew_cache_unexpected")
    current = cache
    for part in target.relative_to(cache).parts[:-1]:
        current = current / part
        if not os.path.lexists(current):
            current.mkdir(mode=0o700)
        _real_directory(current, "homebrew_cache_unexpected")
    return target


def _preseed(target: Path, archive: Path, digest: str) -> str:
    """Place the archive at the cache path, or accept identical bytes; never replace others."""
    if os.path.lexists(target):
        if _digest(target, ArtifactKind.ARCHIVE, "homebrew_cache_conflict") != digest:
            raise VerificationError("homebrew_cache_conflict", STAGE)
        return "existing-matching"
    _copy_exact(archive, target, digest, ArtifactKind.ARCHIVE, "homebrew_cache_conflict")
    return "absent-seeded"


class _Brew:
    """One Brew session: its executable, environment, unique tap and receipt-recorded children."""

    def __init__(self, ports: ConsumerPorts, receipt: dict, layout: ConsumerLayout):
        brew = ports.which("brew")
        if brew is None:
            raise VerificationError("brew_unavailable", "locate-tools")
        base = layout.root / "brew"
        base.mkdir(mode=0o700)
        (base / "logs").mkdir(mode=0o700)
        self.ports, self.receipt, self.layout, self.base, self.brew = ports, receipt, layout, base, brew
        self.tap = f"dlcandidate-{secrets.token_hex(6)}/candidate"
        self.spec = f"{self.tap}/{HOMEBREW_FORMULA}"
        self.env = {
            **homebrew_environment(path=tool_path(ports, ("brew",)), home=str(layout.home),
                                   inherited=ports.inherited),
            "HOMEBREW_CACHE": str(layout.cache), "HOMEBREW_LOGS": str(base / "logs"),
        }

    def run(self, name: str, argv: list[str], *, capture: bool = False, timeout: int = 120) -> bytes:
        return self.ports.run(self.receipt, name, argv, self.env, capture=capture, timeout=timeout,
                              cwd=self.layout.cwd)

    def line(self, name: str, *arguments: str) -> str:
        return parse_single_line(self.run(name, [self.brew, *arguments], capture=True),
                                 code=f"{name}_invalid", stage=STAGE)


def _install(brew: _Brew, *, host_os: str, formula: Path, formula_sha256: str, archive: Path,
             archive_sha256: str) -> tuple[str, Path, Path]:
    """Fresh check, command inventory, unique tap, exact formula and its trust, preseeded cache, then install.

    Returns the prefix supported on the observed ``host_os``, the tap formula file and the
    preseeded cache file.
    """
    facts: dict[str, object] = {"tap": brew.tap, "spec": brew.spec}
    brew.receipt["candidate_brew"] = facts
    prefix = facts["prefix"] = require_fresh(brew.run, brew.brew, brew.tap, host_os=host_os)
    commands = _brew_commands(brew.run("candidate-brew-commands", [brew.brew, "commands", "--quiet"], capture=True))
    facts["trust"] = "pending" if "trust" in commands else "not-advertised"
    brew.run("candidate-brew-tap-new", [brew.brew, "tap-new", "--no-git", brew.tap])
    tap = _real_directory(Path(brew.line("candidate-brew-tap-repository", "--repository", brew.tap)),
                          "homebrew_tap_invalid")
    tapped = _place_formula(tap, formula, formula_sha256)
    facts["tap_formula"] = str(tapped)
    if facts["trust"] == "pending":
        # Only the exact formula just validated, in this session's private HOME; Brew owns the grant.
        brew.run("candidate-brew-trust", [brew.brew, "trust", "--formula", brew.spec])
        facts["trust"] = "formula"
    cache = _cache_target(brew.layout.cache, brew.line(
        "candidate-brew-cache-path", "--cache", "--build-from-source", "--formula", brew.spec))
    cache_facts = facts["cache"] = {"path": str(cache)}  # recorded before a refusal can raise
    cache_facts["state"] = _preseed(cache, archive, archive_sha256)
    brew.run("candidate-brew-install", [brew.brew, "install", "--build-from-source", "--formula", brew.spec],
             timeout=_INSTALL_TIMEOUT)
    return prefix, tapped, cache


def _observe(brew: _Brew, request: CandidateInstallRequest, info: dict[str, str], prefix: str,
             probe: BootstrapProbe, *, onboarding: bool = False
             ) -> tuple[InstalledExecutables, str, tuple[str, str, str], BootstrapObservation, dict[str, object] | None]:
    """Layout, selected keg paths, linked by-name delegate, exact libexec supervisors, default state, bootstrap."""
    version = request.package_version()
    try:
        document = json.loads(brew.run("candidate-brew-info", [brew.brew, "info", "--json=v2", "--formula", brew.spec],
                                       capture=True, timeout=60))
    except (UnicodeError, json.JSONDecodeError, RecursionError):
        raise VerificationError("homebrew_info_invalid", "homebrew-info") from None
    brew.receipt["candidate_brew"]["formula"] = installed_layout(document, version, brew.tap)
    paths = keg_binaries(Path(brew.line("candidate-brew-keg", "--prefix", brew.spec)))
    observed = installed_executables(paths)
    brew.receipt["binaries"] = {name: {"path": str(path)} for name, path in paths.items()}
    home = brew.base / "default-home"
    home.mkdir(mode=0o700)
    env, state_root = isolated_default_environment(directory=Path(prefix) / "bin", home=home, info=info)
    found = shutil.which("delegate", path=env["PATH"])
    if found is None or Path(found).resolve() != paths["delegate"]:
        raise VerificationError("brew_linked_delegate_mismatch", "resolve-delegate")

    def run(name: str, argv: list[str], *, capture: bool = False) -> bytes:
        return brew.ports.run(brew.receipt, name, argv, env, capture=capture, timeout=60, cwd=brew.layout.cwd)

    actual = exercise_binary_identity(
        run=run, executables={"delegate": "delegate", "pueue": str(paths["pueue"]), "pueued": str(paths["pueued"])},
        package_version=version, receipt=brew.receipt)
    default_state = observe_default_state(run=run, state_root=state_root, home=home, receipt=brew.receipt)
    setup_facts = None
    if onboarding:
        # Reuse the linked entry; only Delegate resolves its Homebrew libexec roles.
        shim = prepare_shim(brew.base, {})
        setup = CandidateSetup(request, brew.ports, brew.receipt, brew.layout, CHANNEL_BREW, brew.base,
                               Path(prefix) / "bin",
                               {**shim.environment(), "PATH": os.pathsep.join((
                                   str(shim.directory), str(Path(prefix) / "bin"),
                                   tool_path(brew.ports, ("npm", "node"))))}, select_directory=False)
        setup.fresh()
        setup_facts = setup.verify_repeat(paths["delegate"])
        if shim.requests():
            raise VerificationError("candidate_requests_unexpected", STAGE)
    # The linked bin is the only PATH directory; the keg's libexec pair is selected, never searched.
    expected = observed
    bootstrap = require_bootstrap(probe(BootstrapTarget(
        command_directory=Path(prefix) / "bin", delegate=paths["delegate"],
        supervisor_directory=paths["pueue"].parent, binaries=expected)), expected)
    brew.run("candidate-brew-test", [brew.brew, "test", brew.spec], timeout=_TEST_TIMEOUT)
    return observed, actual, default_state, bootstrap, setup_facts


def observe_brew_install(request: CandidateInstallRequest, *, ports: ConsumerPorts, probe: BootstrapProbe,
                         receipt: dict, layout: ConsumerLayout, info: dict[str, str],
                         onboarding: bool = False) -> InstallObservation:
    """Install the candidate formula on a fresh supported Homebrew and return its observation.

    ``info`` is the caller's observed host platform; only an OS with a supported Homebrew
    prefix (macOS or Linux) is accepted, before any child. The default-state tuple proves only the
    default root and that status created nothing; a fresh status returns before any supervisor
    backend is selected. The bootstrap, run by ``probe`` after the keg layout and that status,
    is the evidence of the linked delegate's runner and libexec pair selection, daemon startup
    and verified shutdown.
    """
    host_os = info.get("os", "")
    require_brew_host(host_os)
    formula, archive = request.files.formula, request.files.archive
    if formula is None:
        raise VerificationError("candidate_formula_missing", "validate-candidate")
    formula_sha256 = _digest(formula, ArtifactKind.HOMEBREW_FORMULA, "candidate_file_invalid")
    archive_sha256 = _digest(archive, ArtifactKind.ARCHIVE, "candidate_file_invalid")
    brew = _Brew(ports, receipt, layout)
    prefix, tapped, cache = _install(brew, host_os=host_os, formula=formula, formula_sha256=formula_sha256,
                                     archive=archive, archive_sha256=archive_sha256)
    binaries, version, default_state, bootstrap, setup_facts = _observe(
        brew, request, info, prefix, probe, onboarding=onboarding)
    for path, kind, digest in ((formula, ArtifactKind.HOMEBREW_FORMULA, formula_sha256),
                               (tapped, ArtifactKind.HOMEBREW_FORMULA, formula_sha256),
                               (archive, ArtifactKind.ARCHIVE, archive_sha256),
                               (cache, ArtifactKind.ARCHIVE, archive_sha256)):
        if _digest(path, kind, "candidate_file_changed") != digest:
            raise VerificationError("candidate_file_changed", "verify-candidate")
    return InstallObservation(
        channel=CHANNEL_BREW, requests=(), binaries=binaries, skill_files=0,
        delegate_version=version, default_state=default_state, bootstrap=bootstrap,
        installer_sha256=formula_sha256, onboarding=setup_facts)
