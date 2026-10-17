"""Codex provider runtime identity and prerequisite checks."""

from __future__ import annotations

import os
import secrets
import shutil
from pathlib import Path

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.evidence.encoding import canonical_go_json
from scripts.delegate_acceptance.paths.operations import no_prompt_argv as _no_prompt_argv
from scripts.delegate_acceptance.tasks.runtime_metadata import runtime_capability_sha
from scripts.delegate_acceptance.supervisor.configuration import supervisor_binding
from scripts.delegate_acceptance.evidence.files import digest
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.provider_drivers.codex.constants import (
    CODEX_ENVIRONMENT_KEYS, RUNTIME_HELP_ARGS, RUNTIME_INSPECTION_REVISION,
    RUNTIME_REQUIRED_FLAGS,
)

import sys
from scripts.delegate_acceptance.provider_drivers.codex.identity import is_task_id as is_task_id
from scripts.delegate_acceptance.provider_drivers.codex.identity import is_thread_id as is_thread_id


DEFAULT_STATE_PARENT = Path.home() / "Library" / "Application Support" / "delegation-layer-acceptance"
DEFAULT_WORKSPACE_PARENT = Path.home() / "Active-Projects" / "delegation-layer-acceptance"
PUEUE_PARENT = Path("/Users/Shared") if sys.platform == "darwin" else Path.home() / ".dl-acceptance"


def expected_codex_inspection_binding(
        *, pueue: Path, config: Path, base: Path, config_digest: str,
        runner: Path, workspace: Path, environment: dict[str, str],
        provider: Path, provider_digest: str,
        pueue_version: str) -> dict[str, object]:
    """Build the expected runtime-only inspection binding from setup sources."""
    values = sorted(key + "=" + environment[key]
                    for key in CODEX_ENVIRONMENT_KEYS if key in environment)
    definition = {
        "revision": RUNTIME_INSPECTION_REVISION,
        "executable": str(provider),
        "executable_sha256": provider_digest,
        "arguments": None,
        "directory": str(workspace),
        "environment": values,
        "output_limit": 1 << 20,
        "runtime": {
            "executable": str(provider),
            "executable_sha256": provider_digest,
            "directory": str(workspace),
            "environment": values,
            "help_args": RUNTIME_HELP_ARGS,
            "required_flags": RUNTIME_REQUIRED_FLAGS,
        },
    }
    return {
        "definition_revision": RUNTIME_INSPECTION_REVISION,
        "definition_sha256": sha(canonical_go_json(definition)),
        "capability_sha256": runtime_capability_sha(definition),
        "helper_executable": str(provider),
        "helper_sha256": provider_digest,
        "worker_executable": str(runner),
        "worker_sha256": digest(runner),
        "runner_ownership": "custom",
        "environment": values,
        "environment_recorded": True,
        "supervisor": supervisor_binding(pueue, config, base, config_digest, pueue_version),
    }


class BlockedFailure(AcceptanceFailure):
    """A required native prerequisite is unavailable on this host."""


def provider_runtime_roots(environment: dict[str, str]) -> list[Path]:
    """Return roots where Codex or its toolchain may write runtime state."""
    home = Path(environment.get("HOME", str(Path.home()))).resolve(strict=False)
    roots = [Path(item) for item in ("/tmp", "/var/tmp", "/var/folders", "/dev")]
    roots.extend(home / item for item in (
        ".codex", ".cache", ".cargo", ".rustup", ".npm", ".nvm", ".bun", ".gradle",
        ".m2", ".local", "Library/Caches", "Library/Logs",
    ))
    for name in ("TMPDIR", "TMP", "TEMP", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
                 "GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTUP_HOME",
                 "GRADLE_USER_HOME", "NPM_CONFIG_CACHE"):
        value = environment.get(name)
        if value and not (name in {"GOCACHE", "GOMODCACHE"} and value == "off"):
            roots.append(Path(value))
    if environment.get("CODEX_HOME"):
        roots.append(Path(environment["CODEX_HOME"]))
    if environment.get("GOPATH"):
        roots.extend(Path(item) for item in environment["GOPATH"].split(os.pathsep) if item)
    return sorted({root.resolve(strict=False) for root in roots})


def reject_runtime_roots(path: Path, label: str, environment: dict[str, str]) -> None:
    resolved = path.resolve(strict=False)
    for root in provider_runtime_roots(environment):
        if resolved == root or root in resolved.parents:
            raise AcceptanceFailure(f"{label} is inside provider writable runtime root {root}: {resolved}")


def task_id() -> str:
    return secrets.token_hex(16)






def resolve_executable(value: str | None, default: Path, label: str) -> Path:
    candidate = Path(value) if value else default
    if not candidate.is_absolute():
        candidate = candidate.resolve()
    # Installed package managers commonly expose the selected CLI through a
    # symlink. Resolve that alias before validating, recording, and hashing the
    # regular executable so PATH discovery and invocation use the same binary.
    if not candidate.is_file() or not os.access(candidate, os.X_OK):
        raise BlockedFailure(f"{label} is unavailable or not executable: {candidate}")
    resolved = candidate.resolve(strict=True)
    if not resolved.is_file() or not os.access(resolved, os.X_OK):
        raise BlockedFailure(f"{label} resolved target is not executable: {resolved}")
    return resolved


def no_prompt_argv(argv: list[object], briefs: list[Path]) -> None:
    """Codex transport guard, including its continuation-only prohibition."""
    _no_prompt_argv(argv, briefs, forbidden=("--ephemeral",))


def require_discovery(name: str, selected: Path) -> None:
    discovered = shutil.which(name)
    if discovered is None:
        raise BlockedFailure(f"{name} is not discoverable in the inherited PATH")
    if Path(discovered).resolve() != selected:
        raise BlockedFailure(f"{name} discovery does not match selected executable; PATH shadowing is forbidden")
