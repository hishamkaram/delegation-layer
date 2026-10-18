"""Homebrew consumer verification: fresh macOS install of the published formula."""

from __future__ import annotations

from pathlib import Path
import json
import os
import stat
from .contract import BINARIES, MAX_BINARY, VerificationError, binary_version
from .filesystem import bounded_digest
from .published_contract import (
    ArchiveFacts, HOMEBREW_FORMULA, HOMEBREW_PREFIXES, compare_hashes, homebrew_environment,
    record_fact,
)
from .published_ports import ConsumerLayout, ConsumerPorts, tool_path


_LIBEXEC = frozenset({"pueue", "pueued"})


def _lines(raw: bytes, code: str) -> list[str]:
    try:
        return raw.decode("utf-8").splitlines()
    except UnicodeError:
        raise VerificationError(code, "homebrew-observe") from None


def _require_fresh(run, brew: str, tap: str) -> str:
    prefix = _lines(run("homebrew-prefix", [brew, "--prefix"], capture=True, timeout=30),
                    "homebrew_prefix_invalid")
    if len(prefix) != 1 or prefix[0] not in HOMEBREW_PREFIXES:
        raise VerificationError("homebrew_prefix_unsupported", "homebrew-observe")
    taps = _lines(run("homebrew-taps", [brew, "tap"], capture=True, timeout=30), "homebrew_tap_list_invalid")
    installed = _lines(run("homebrew-installed", [brew, "list", "--formula", "-1"], capture=True, timeout=60),
                       "homebrew_list_invalid")
    if tap in taps or {HOMEBREW_FORMULA, f"{tap}/{HOMEBREW_FORMULA}"} & set(installed):
        raise VerificationError("homebrew_not_fresh", "homebrew-observe")
    return prefix[0]


def _installed_layout(info: dict, version: str, tap: str) -> dict[str, str]:
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


def _keg_binaries(keg: Path) -> dict[str, Path]:
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


def verify_homebrew_consumer(
        *, ports: ConsumerPorts, receipt: dict, archive: ArchiveFacts, layout: ConsumerLayout,
        info: dict[str, str], repo: str) -> dict[str, str]:
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

    record_fact(receipt, "homebrew_prefix", _require_fresh(run, brew, tap))
    run("homebrew-install", [brew, "install", "--formula", spec], timeout=900)
    record_fact(receipt, "homebrew_install", "completed-and-retained")
    try:
        formula = json.loads(run("homebrew-info", [brew, "info", "--json=v2", "--formula", spec],
                                 capture=True, timeout=60))
    except (UnicodeError, json.JSONDecodeError, RecursionError):
        raise VerificationError("homebrew_info_invalid", "homebrew-info") from None
    record_fact(receipt, "homebrew_formula", _installed_layout(formula, archive.package_version, tap))
    keg = Path(_lines(run("homebrew-keg", [brew, "--prefix", spec], capture=True, timeout=30),
                      "homebrew_keg_invalid")[0])
    paths = _keg_binaries(keg)
    observed = {name: bounded_digest(path, MAX_BINARY, "homebrew_binary_invalid")
                for name, path in paths.items()}
    compare_hashes(archive.binary_hashes(), observed, "installed_binary_hash_mismatch", "verify-binaries")
    receipt["binaries"] = {name: {"path": str(path), "sha256": observed[name]} for name, path in paths.items()}
    _exercise(run, paths, archive.package_version, receipt)
    return observed


def _exercise(run, paths: dict[str, Path], version: str, receipt: dict) -> None:
    delegate = str(paths["delegate"])
    actual = binary_version(run("delegate-version", [delegate, "version"], capture=True), "delegate")
    if actual != version:
        raise VerificationError("delegate_version_mismatch", "verify-delegate-version")
    receipt["delegate_build_identity"] = {"version": actual}
    for name in ("pueue", "pueued"):
        receipt["binaries"][name]["version"] = binary_version(
            run(f"{name}-version", [str(paths[name]), "--version"], capture=True), name)
    run("delegate-providers", [delegate, "providers", "--json"])
    run("delegate-capabilities", [delegate, "capabilities", "--provider", "pi:json", "--json"])
