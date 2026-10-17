"""Claude provider runtime preparation."""

from __future__ import annotations

from collections.abc import Mapping
import os
from pathlib import Path
import pwd
import secrets
import shutil
import sys
import tempfile
import uuid
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.evidence.encoding import canonical_go_json
from scripts.delegate_acceptance.evidence.validation import require
from scripts.delegate_acceptance.tasks.runtime_metadata import runtime_capability_sha
from scripts.delegate_acceptance.supervisor.configuration import supervisor_binding
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.evidence.files import digest
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.provider_drivers.claude.constants import (
    CLAUDE_XDG_DIRECTORY,
    NATIVE_DISCOVERY_ENVIRONMENT_KEYS,
    NATIVE_ENVIRONMENT_KEYS,
    PATH_VALIDATION_ENVIRONMENT_KEYS,
    ROOT_PATTERN,
    RUNTIME_HELP_ARGS,
    RUNTIME_INSPECTION_REVISION,
    RUNTIME_REQUIRED_FLAGS,
)

from scripts.delegate_acceptance.provider_drivers.claude.identity import is_session_id as is_session_id
from scripts.delegate_acceptance.provider_drivers.claude.identity import is_task_id as is_task_id


DEFAULT_STATE_PARENT = Path.home() / "Library" / "Application Support" / "delegation-layer-acceptance"



class BlockedFailure(AcceptanceFailure):
    """A required native prerequisite is unavailable on this host."""


def pueue_parent_for_platform(platform: str, home: Path) -> Path:
    """Keep the live supervisor/workspace parent outside provider temp roots."""
    return Path("/Users/Shared") if platform == "darwin" else Path(home) / ".dl-acceptance"


PUEUE_PARENT = pueue_parent_for_platform(sys.platform, Path.home())


def ensure_acceptance_temp_parent(parent: Path, platform: str) -> Path:
    """Create and validate the dedicated parent used for private temp roots."""
    try:
        if parent.exists() and parent.is_symlink():
            raise AcceptanceFailure("acceptance temporary parent must not be a symlink: " + str(parent))
        parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        if platform != "darwin":
            os.chmod(parent, 0o700)
    except OSError as error:
        raise BlockedFailure(f"cannot create acceptance temporary parent: {parent}") from error
    require(parent.is_dir() and not parent.is_symlink(),
            f"acceptance temporary parent is not a directory: {parent}")
    if platform != "darwin":
        require(parent.stat().st_mode & 0o077 == 0,
                f"acceptance temporary parent is group/world accessible: {parent}")
    return parent.resolve()


def private_mkdtemp_under_parent(prefix: str, label: str, owner: DirectoryOwner,
                                 parent: Path):
    """Allocate one private, owner-tracked directory below a validated parent."""
    try:
        path = Path(tempfile.mkdtemp(prefix=prefix, dir=str(parent)))
    except OSError as error:
        raise BlockedFailure(f"cannot create {label}") from error
    token = owner.adopt_created(label, path, parent)
    os.chmod(token.directory, 0o700)
    require(token.directory.is_dir() and not token.directory.is_symlink(),
            f"{label} is not a private directory: {token.directory}")
    require(token.directory.stat().st_mode & 0o077 == 0,
            f"{label} is group/world accessible: {token.directory}")
    return token


def acceptance_temp_parent() -> Path:
    """Create the platform-specific mkdtemp parent before allocating children."""
    return ensure_acceptance_temp_parent(PUEUE_PARENT, sys.platform)


def private_mkdtemp(prefix: str, label: str, owner: DirectoryOwner):
    """Allocate one private directory below the platform-specific parent."""
    return private_mkdtemp_under_parent(prefix, label, owner, acceptance_temp_parent())
def _native_account(environment: dict[str, str]) -> str:
    """Mirror the provider's non-secret Keychain account selection."""
    account = environment.get("USER", "")
    if account == "":
        try:
            account = pwd.getpwuid(os.getuid()).pw_name
        except (KeyError, OSError):
            account = ""
    if account and all(char.isascii() and (char.isalnum() or char in "._-") for char in account):
        return account
    return "claude-code-user"
def _native_environment(environment: dict[str, str]) -> list[str]:
    """Build bounded native discovery/session environment without secrets."""
    keys = NATIVE_ENVIRONMENT_KEYS + NATIVE_DISCOVERY_ENVIRONMENT_KEYS
    values = [key + "=" + environment[key] for key in keys if key in environment]
    return sorted(values)
def expected_claude_inspection_binding(
        pueue: Path, config: Path, base: Path, config_digest: str,
        workspace: Path, executable: Path,
        runner: Path, environment: dict[str, str],
        pueue_version: str) -> dict[str, object]:
    """Build Claude's expected portable runtime inspection binding."""
    executable = Path(executable).resolve()
    runner = Path(runner).resolve()
    values = _native_environment(environment)
    definition = {
        "revision": RUNTIME_INSPECTION_REVISION,
        "executable": str(executable),
        "executable_sha256": digest(executable),
        "arguments": None,
        "directory": str(Path(workspace).resolve(strict=True)),
        "environment": values,
        "output_limit": 1 << 20,
        "runtime": {
            "executable": str(executable),
            "executable_sha256": digest(executable),
            "directory": str(Path(workspace).resolve(strict=True)),
            "environment": values,
            "help_args": RUNTIME_HELP_ARGS,
            "required_flags": list(RUNTIME_REQUIRED_FLAGS),
        },
    }
    return {
        "definition_revision": RUNTIME_INSPECTION_REVISION,
        "definition_sha256": sha(canonical_go_json(definition)),
        "capability_sha256": runtime_capability_sha(definition),
        "helper_executable": str(executable),
        "helper_sha256": digest(executable),
        "worker_executable": str(runner),
        "worker_sha256": digest(runner),
        "runner_ownership": "custom",
        "environment": values,
        "environment_recorded": True,
        "supervisor": supervisor_binding(pueue, config, base, config_digest, pueue_version),
    }
def fresh_session_id(root_id: str, task: str) -> str:
    """Derive UUIDv5 from the complete root bytes and canonical task ID."""
    require(isinstance(root_id, str) and ROOT_PATTERN.fullmatch(root_id) is not None,
            "fresh Claude root identity is not a canonical 16-byte hex value")
    require(is_task_id(task), "fresh Claude task identity is not canonical")
    try:
        namespace = uuid.UUID(bytes=bytes.fromhex(root_id))
    except (ValueError, TypeError) as error:
        raise AcceptanceFailure("fresh Claude root identity cannot be decoded as UUID bytes") from error
    return str(uuid.uuid5(namespace, task))
def host_home_directory() -> Path:
    """Resolve the OS account home without trusting an overridden HOME."""
    try:
        return Path(pwd.getpwuid(os.getuid()).pw_dir).resolve(strict=True)
    except (KeyError, OSError) as error:
        raise BlockedFailure("native Claude home cannot be resolved") from error
def isolated_acceptance_environment(
        inherited: Mapping[str, str], home: Path, temporary: Path) -> dict[str, str]:
    """Build a fully task-owned Claude environment for hermetic fixtures."""
    values = {key: inherited[key] for key in NATIVE_ENVIRONMENT_KEYS if key in inherited}
    values["HOME"] = str(Path(home).resolve())
    for key in ("TMPDIR", "TMP", "TEMP"):
        values[key] = str(Path(temporary).resolve())
    return values
def native_account_acceptance_environment(
        inherited: Mapping[str, str], home: Path, temporary: Path) -> dict[str, str]:
    """Use the signed-in native account while relocating mutable temp state.

    Claude's macOS Keychain lookup is tied to the host login context. The
    acceptance run therefore keeps the real HOME/Keychain account, while its
    temporary, cache, and log paths remain task-owned. No credential bytes are
    read by this driver or copied into the task roots.
    """
    keys = NATIVE_ENVIRONMENT_KEYS + NATIVE_DISCOVERY_ENVIRONMENT_KEYS
    values = {key: inherited[key] for key in keys if key in inherited}
    values["HOME"] = str(Path(home).resolve(strict=True))
    for key in ("TMPDIR", "TMP", "TEMP"):
        values[key] = str(Path(temporary).resolve())
    return values
def path_validation_environment(inherited: Mapping[str, str]) -> dict[str, str]:
    """Read only path-related variables needed for harness-root validation."""
    return {key: inherited[key] for key in PATH_VALIDATION_ENVIRONMENT_KEYS if key in inherited}
def provider_runtime_roots(environment: dict[str, str]) -> list[Path]:
    """Return Claude's known writable runtime/cache roots."""
    home = Path(environment.get("HOME", str(Path.home()))).resolve(strict=False)
    roots = [Path(item) for item in ("/tmp", "/var/tmp", "/var/folders", "/dev")]
    roots.extend(home / item for item in (
        ".claude", ".claude.json", ".cache", "Library/Caches", "Library/Logs",
        "Library/Application Support/Claude", "Library/Keychains",
    ))
    for name in ("TMP", "TMPDIR", "TEMP"):
        value = environment.get(name)
        if value:
            roots.append(Path(value))
    for name in ("XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"):
        value = environment.get(name)
        if value and Path(value).is_absolute():
            roots.append(Path(value) / CLAUDE_XDG_DIRECTORY)
    for name in ("XDG_CONFIG_DIRS", "XDG_DATA_DIRS"):
        for value in environment.get(name, "").split(os.pathsep):
            if value and Path(value).is_absolute():
                roots.append(Path(value) / CLAUDE_XDG_DIRECTORY)
    for name in ("CLAUDE_CONFIG_DIR", "ANTHROPIC_CONFIG_DIR"):
        value = environment.get(name)
        if value and Path(value).is_absolute():
            roots.append(Path(value))
    return sorted({root.resolve(strict=False) for root in roots})
def reject_runtime_roots(path: Path, label: str, environment: dict[str, str]) -> None:
    resolved = path.resolve(strict=False)
    for root in provider_runtime_roots(environment):
        if resolved == root or root in resolved.parents:
            raise AcceptanceFailure(f"{label} is inside provider writable runtime root {root}: {resolved}")
def require_discovery(name: str, selected: Path) -> None:
    discovered = shutil.which(name)
    if discovered is None:
        raise BlockedFailure(f"{name} is not discoverable in the inherited PATH")
    if Path(discovered).resolve() != selected:
        raise BlockedFailure(f"{name} discovery does not match selected executable; PATH shadowing is forbidden")
def resolve_executable(value: str | None, default: Path, label: str) -> Path:
    candidate = Path(value) if value else default
    if not candidate.is_absolute():
        candidate = candidate.resolve()
    if not candidate.is_file() or not os.access(candidate, os.X_OK):
        raise BlockedFailure(f"{label} is unavailable or not executable: {candidate}")
    resolved = candidate.resolve(strict=True)
    if not resolved.is_file() or not os.access(resolved, os.X_OK):
        raise BlockedFailure(f"{label} resolved target is not executable: {resolved}")
    return resolved
def task_id() -> str:
    return secrets.token_hex(16)
