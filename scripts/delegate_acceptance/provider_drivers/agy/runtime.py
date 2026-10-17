"""Provider executable, runtime-root, and identity helpers."""

from __future__ import annotations

import os
import secrets
import shutil
from datetime import datetime, timezone
from pathlib import Path

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require
from scripts.delegate_acceptance.paths.operations import write_bytes
from scripts.delegate_acceptance.evidence.files import read_json
from scripts.delegate_acceptance.provider_drivers.agy.constants import AUTHENTICATION_DIAGNOSTIC

import sys


DEFAULT_STATE_PARENT = Path.home() / "Library" / "Application Support" / "delegation-layer-acceptance"
DEFAULT_WORKSPACE_PARENT = Path.home() / "Active-Projects" / "delegation-layer-acceptance"
DEFAULT_EVIDENCE_PARENT = Path.home() / "Library" / "Application Support" / "delegation-layer-evidence"
PUEUE_PARENT = Path("/Users/Shared") if sys.platform == "darwin" else Path.home() / ".dl-acceptance"


class BlockedFailure(AcceptanceFailure):
    """A required native authentication or prerequisite is unavailable."""

def provider_runtime_roots(environment: dict[str, str]) -> list[Path]:
    home = Path(environment.get("HOME", str(Path.home()))).resolve(strict=False)
    roots = [Path(item) for item in ("/tmp", "/var/tmp", "/var/folders", "/dev")]
    roots.extend(home / item for item in (
        ".gemini", ".cache", ".cargo", ".rustup", ".npm", ".nvm", ".bun", ".gradle", ".m2",
        ".dotnet", ".nuget", ".nimble", ".docker", ".local", "go", "Library/Caches", "Library/Logs",
        "Library/Application Support/Antigravity", "Library/Application Support/Google/Antigravity",
    ))
    for name in ("TMPDIR", "TMP", "TEMP", "XDG_CACHE_HOME", "GOCACHE", "GOMODCACHE", "CARGO_HOME",
                 "RUSTUP_HOME", "GRADLE_USER_HOME", "NPM_CONFIG_CACHE"):
        if environment.get(name) and not (name == "GOCACHE" and environment[name] == "off"):
            roots.append(Path(environment[name]))
    if environment.get("GOPATH"):
        roots.extend(Path(item) for item in environment["GOPATH"].split(os.pathsep) if item)
    for name in ("XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
                 "GEMINI_HOME", "GEMINI_CLI_HOME", "ANTIGRAVITY_HOME", "ANTIGRAVITY_CONFIG_HOME",
                 "AGY_HOME", "AGY_CONFIG_HOME"):
        if environment.get(name):
            roots.append(Path(environment[name]))
    for name in ("XDG_CONFIG_DIRS", "XDG_DATA_DIRS"):
        roots.extend(Path(item) for item in environment.get(name, "").split(os.pathsep) if item)
    return sorted({root.resolve(strict=False) for root in roots})

def reject_runtime_roots(path: Path, label: str, environment: dict[str, str]) -> None:
    resolved = path.resolve(strict=False)
    for root in provider_runtime_roots(environment):
        if resolved == root or root in resolved.parents:
            raise AcceptanceFailure(f"{label} is inside provider writable runtime root {root}: {resolved}")

def copy_bytes(source: Path, destination: Path, label: str) -> None:
    require(source.is_file() and not source.is_symlink(), f"{label} is not a regular file: {source}")
    write_bytes(destination, source.read_bytes())

def utc_stamp() -> str:
    return datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")

def task_id() -> str:
    return secrets.token_hex(16)

def no_prompt_argv(argv: list[str], brief_paths: list[Path]) -> None:
    joined = "\x00".join(argv)
    for brief in brief_paths:
        content = brief.read_bytes()
        require(content.decode(errors="replace") not in joined, "brief content leaked into child argv")
    require("--" not in argv or argv.count("--") == 0,
            "native acceptance command must not carry a raw argv separator")

def resolve_executable(value: str | None, default: Path, label: str) -> Path:
    candidate = Path(value) if value else default
    require(candidate.is_absolute(), f"{label} must be an absolute executable path")
    # Homebrew and other package managers expose commands through symlinked
    # aliases. Validate and record the canonical regular executable.
    require(candidate.is_file() and os.access(candidate, os.X_OK),
            f"{label} is unavailable or not executable: {candidate}")
    resolved = candidate.resolve(strict=True)
    require(resolved.is_file() and os.access(resolved, os.X_OK), f"{label} resolved target is not executable: {resolved}")
    return resolved

def require_discovery(name: str, selected: Path) -> None:
    discovered = shutil.which(name)
    require(discovered is not None, f"{name} is not discoverable in the inherited PATH")
    require(Path(discovered).resolve() == selected,
            f"{name} discovery does not match selected executable; PATH shadowing is forbidden")

def selected_pueue(default: Path) -> Path:
    explicit = os.environ.get("DELEGATE_TEST_PUEUE")
    return resolve_executable(explicit, default, "pueue")

def selected_pueued(default: Path) -> Path:
    explicit = os.environ.get("DELEGATE_TEST_PUEUED")
    return resolve_executable(explicit, default, "pueued")

def selected_tools(repo: Path) -> Path:
    return resolve_executable(os.environ.get("DELEGATE_HARNESS_TOOLS"), repo / "bin" / "harness-tools" / "harnessprobe", "harnessprobe")

def authentication_unavailable(directory: Path) -> bool:
    """Classify only the terminal structured authentication diagnostic."""
    try:
        outcome = read_json(directory / "outcome.json")
    except (OSError, ValueError, TypeError, RecursionError, RuntimeError):
        # Authentication is a positive classification. An absent or
        # unreadable terminal record cannot establish it.
        return False
    if not isinstance(outcome, dict) or outcome.get("verdict") != "rejected":
        return False
    payload = outcome.get("payload")
    if not isinstance(payload, dict) or payload.get("basename") != "publish.reject":
        return False
    diagnostics = outcome.get("diagnostics")
    if not isinstance(diagnostics, list) or not any(
            isinstance(diagnostic, dict) and
            diagnostic.get("code") == AUTHENTICATION_DIAGNOSTIC
            for diagnostic in diagnostics):
        return False
    diagnostic = directory / "publish.reject"
    if not diagnostic.is_file() or diagnostic.is_symlink():
        return False
    try:
        return diagnostic.read_bytes() == AUTHENTICATION_DIAGNOSTIC.encode()
    except OSError:
        return False
