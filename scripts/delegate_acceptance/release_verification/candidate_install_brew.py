"""Observe one fresh Homebrew installation of the exact unpublished candidate formula.

The caller owns the validated request, the receipt, the workspace and its lifetime; this
module only observes and returns facts or raises the original failure. Every Homebrew and
default-path child runs through ConsumerPorts.run in a credential-free environment whose HOMEBREW_CACHE and
HOMEBREW_LOGS lie inside the caller's workspace. The formula keeps its original release
URL; Homebrew finds the archive only at the source cache path it reports, preseeded with the
exact candidate bytes, so a cache miss fails. Homebrew owns the new tap, the formula file
in it and the keg on the disposable host; nothing here adopts, uninstalls or removes them.
The caller's BootstrapProbe owns the children and resources of the installed fixture turn.
"""

from __future__ import annotations

from pathlib import Path
import json
import os
import secrets
import shutil
import stat
from .assets import exercise_binary_identity, isolated_default_environment, observe_default_state
from .candidate_install_contract import (
    CHANNEL_BREW, BootstrapObservation, BootstrapProbe, BootstrapTarget, CandidateInstallRequest, InstallObservation,
    require_bootstrap,
)
from .candidate_manifest import ArtifactKind, entry_limit
from .contract import MAX_BINARY, VerificationError, parse_binary_digests
from .filesystem import _reject_parent_components, _reject_symlink_components, bounded_digest, create_new_file
from .published_contract import (
    HOMEBREW_FORMULA, compare_hashes, homebrew_environment, homebrew_prefixes, parse_single_line,
)
from .published_homebrew import installed_layout, keg_binaries, require_fresh
from .published_ports import ConsumerLayout, ConsumerPorts, tool_path


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
    """Fresh check, unique tap, exact formula and preseeded cache, then the source install.

    Returns the prefix supported on the observed ``host_os``, the tap formula file and the
    preseeded cache file.
    """
    facts: dict[str, object] = {"tap": brew.tap, "spec": brew.spec}
    brew.receipt["candidate_brew"] = facts
    prefix = facts["prefix"] = require_fresh(brew.run, brew.brew, brew.tap, host_os=host_os)
    brew.run("candidate-brew-tap-new", [brew.brew, "tap-new", "--no-git", brew.tap])
    tap = _real_directory(Path(brew.line("candidate-brew-tap-repository", "--repository", brew.tap)),
                          "homebrew_tap_invalid")
    tapped = _place_formula(tap, formula, formula_sha256)
    facts["tap_formula"] = str(tapped)
    cache = _cache_target(brew.layout.cache, brew.line(
        "candidate-brew-cache-path", "--cache", "--build-from-source", "--formula", brew.spec))
    cache_facts = facts["cache"] = {"path": str(cache)}  # recorded before a refusal can raise
    cache_facts["state"] = _preseed(cache, archive, archive_sha256)
    brew.run("candidate-brew-install", [brew.brew, "install", "--build-from-source", "--formula", brew.spec],
             timeout=_INSTALL_TIMEOUT)
    return prefix, tapped, cache


def _observe(brew: _Brew, request: CandidateInstallRequest, info: dict[str, str], prefix: str,
             probe: BootstrapProbe) -> tuple[dict[str, str], str, tuple[str, str, str], BootstrapObservation]:
    """Layout, keg hashes, linked by-name delegate, exact libexec supervisors, default state, bootstrap."""
    version = request.package_version()
    try:
        document = json.loads(brew.run("candidate-brew-info", [brew.brew, "info", "--json=v2", "--formula", brew.spec],
                                       capture=True, timeout=60))
    except (UnicodeError, json.JSONDecodeError, RecursionError):
        raise VerificationError("homebrew_info_invalid", "homebrew-info") from None
    brew.receipt["candidate_brew"]["formula"] = installed_layout(document, version, brew.tap)
    paths = keg_binaries(Path(brew.line("candidate-brew-keg", "--prefix", brew.spec)))
    observed = {name: bounded_digest(path, MAX_BINARY, "homebrew_binary_invalid") for name, path in paths.items()}
    compare_hashes(request.binary_hashes(), observed, "installed_binary_hash_mismatch", "verify-binaries")
    brew.receipt["binaries"] = {name: {"path": str(path), "sha256": observed[name]} for name, path in paths.items()}
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
    # The linked bin is the only PATH directory; the keg's libexec pair is selected, never searched.
    expected = parse_binary_digests(request.binary_hashes(), "verify-binaries")
    bootstrap = require_bootstrap(probe(BootstrapTarget(
        command_directory=Path(prefix) / "bin", delegate=paths["delegate"],
        supervisor_directory=paths["pueue"].parent, binaries=expected)), expected)
    brew.run("candidate-brew-test", [brew.brew, "test", brew.spec], timeout=_TEST_TIMEOUT)
    return observed, actual, default_state, bootstrap


def observe_brew_install(request: CandidateInstallRequest, *, ports: ConsumerPorts, probe: BootstrapProbe,
                         receipt: dict, layout: ConsumerLayout, info: dict[str, str]) -> InstallObservation:
    """Install the candidate formula on a fresh supported Homebrew and return its observation.

    ``info`` is the caller's observed host platform; only an OS with a supported Homebrew
    prefix (macOS or Linux) is accepted, before any child. The default-state tuple proves only the
    default root and that status created nothing; a fresh status returns before any supervisor
    backend is selected. The bootstrap, run by ``probe`` after the keg hashes and that status,
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
    binaries, version, default_state, bootstrap = _observe(brew, request, info, prefix, probe)
    for path, kind, digest in ((formula, ArtifactKind.HOMEBREW_FORMULA, formula_sha256),
                               (tapped, ArtifactKind.HOMEBREW_FORMULA, formula_sha256),
                               (archive, ArtifactKind.ARCHIVE, archive_sha256),
                               (cache, ArtifactKind.ARCHIVE, archive_sha256)):
        if _digest(path, kind, "candidate_file_changed") != digest:
            raise VerificationError("candidate_file_changed", "verify-candidate")
    return InstallObservation(
        channel=CHANNEL_BREW, requests=(), binaries=tuple(sorted(binaries.items())), skill_files=0,
        delegate_version=version, default_state=default_state, bootstrap=bootstrap,
        installer_sha256=formula_sha256)
