"""Homebrew consumer verification: fresh macOS install of the published formula."""

from __future__ import annotations

from pathlib import Path
import json
import os
import stat
from .assets import exercise_binary_identity
from .contract import InstalledExecutables, installed_executables, BINARIES, MAX_BINARY, VerificationError
from .published_contract import (
    ArchiveFacts, HOMEBREW_FORMULA, homebrew_environment, homebrew_prefixes,
    parse_single_line, record_fact,
)
from .published_ports import ConsumerLayout, ConsumerPorts, tool_path


_LIBEXEC = frozenset({"pueue", "pueued"})


def _lines(raw: bytes, code: str) -> list[str]:
    try:
        return raw.decode("utf-8").splitlines()
    except UnicodeError:
        raise VerificationError(code, "homebrew-observe") from None


def require_fresh(run, brew: str, tap: str, *, host_os: str) -> str:
    """Return the prefix supported on ``host_os``; refuse a Homebrew that already has the tap or formula."""
    prefix = _lines(run("homebrew-prefix", [brew, "--prefix"], capture=True, timeout=30),
                    "homebrew_prefix_invalid")
    if len(prefix) != 1 or prefix[0] not in homebrew_prefixes(host_os):
        raise VerificationError("homebrew_prefix_unsupported", "homebrew-observe")
    taps = _lines(run("homebrew-taps", [brew, "tap"], capture=True, timeout=30), "homebrew_tap_list_invalid")
    installed = _lines(run("homebrew-installed", [brew, "list", "--formula", "-1"], capture=True, timeout=60),
                       "homebrew_list_invalid")
    if tap in taps or {HOMEBREW_FORMULA, f"{tap}/{HOMEBREW_FORMULA}"} & set(installed):
        raise VerificationError("homebrew_not_fresh", "homebrew-observe")
    return prefix[0]


def installed_layout(info: dict, version: str, tap: str) -> dict[str, str]:
    """The ``brew info --json=v2`` facts of exactly one installed version from ``tap``."""
    try:
        formula = info["formulae"][0]
        stable = formula["versions"]["stable"]
        installed = [item["version"] for item in formula["installed"]]
    except (KeyError, IndexError, TypeError):
        raise VerificationError("homebrew_info_invalid", "homebrew-info") from None
    if (formula.get("name") != HOMEBREW_FORMULA or formula.get("tap") != tap
            or stable != version or installed != [version]):
        raise VerificationError("homebrew_version_mismatch", "homebrew-info")
    return {"version": version, "tap": tap}


def keg_binaries(keg: Path) -> dict[str, Path]:
    """The four regular executables of the resolved keg: bin/ for delegate, libexec/ for pueue."""
    paths = {}
    try:
        keg = keg.resolve(strict=True)
        for name in BINARIES:
            path = keg / ("libexec" if name in _LIBEXEC else "bin") / name
            info = path.lstat()
            if (not stat.S_ISREG(info.st_mode) or not os.access(path, os.X_OK)
                    or path.resolve(strict=True).parent != path.parent or info.st_size > MAX_BINARY):
                raise VerificationError("homebrew_binary_invalid", "inspect-binaries")
            paths[name] = path
    except OSError:
        raise VerificationError("homebrew_binary_invalid", "inspect-binaries") from None
    return paths


# Earlier names of the same rules, kept for existing callers.
_require_fresh = require_fresh
_installed_layout = installed_layout
_keg_binaries = keg_binaries


def verify_homebrew_consumer(
        *, ports: ConsumerPorts, receipt: dict, archive: ArchiveFacts, layout: ConsumerLayout,
        info: dict[str, str], repo: str) -> InstalledExecutables:
    """Install the published formula on a fresh macOS Homebrew and compare its four binaries."""
    if info["os"] != "Darwin":
        raise VerificationError("homebrew_requires_macos", "homebrew-observe")
    brew = ports.which("brew")
    if brew is None:
        raise VerificationError("brew_unavailable", "locate-tools")
    env = homebrew_environment(
        path=tool_path(ports, ("brew",)), home=str(layout.home), inherited=ports.inherited)
    tap = f"{repo.split('/')[0].lower()}/tap"
    spec = f"{tap}/{HOMEBREW_FORMULA}"

    def run(name: str, argv: list[str], *, capture: bool = False, timeout: int = 120) -> bytes:
        return ports.run(receipt, name, argv, env, capture=capture, timeout=timeout, cwd=layout.cwd)

    record_fact(receipt, "homebrew_prefix", require_fresh(run, brew, tap, host_os=info["os"]))
    run("homebrew-install", [brew, "install", "--formula", spec], timeout=900)
    record_fact(receipt, "homebrew_install", "completed-and-retained")
    try:
        formula = json.loads(run("homebrew-info", [brew, "info", "--json=v2", "--formula", spec],
                                 capture=True, timeout=60))
    except (UnicodeError, json.JSONDecodeError, RecursionError):
        raise VerificationError("homebrew_info_invalid", "homebrew-info") from None
    record_fact(receipt, "homebrew_formula", installed_layout(formula, archive.package_version, tap))
    keg = Path(parse_single_line(run("homebrew-keg", [brew, "--prefix", spec], capture=True, timeout=30),
                                 code="homebrew_keg_invalid", stage="homebrew-observe"))
    paths = keg_binaries(keg)
    observed = installed_executables(paths)
    receipt["binaries"] = {name: {"path": str(path)} for name, path in paths.items()}
    _exercise(run, paths, archive.package_version, receipt)
    return observed


def _exercise(run, paths: dict[str, Path], version: str, receipt: dict) -> None:
    exercise_binary_identity(run=run, executables={name: str(paths[name]) for name in ("delegate", "pueue", "pueued")},
                             package_version=version, receipt=receipt)
