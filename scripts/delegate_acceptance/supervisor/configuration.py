"""Resolve supervisor coordinates and bind observed executable/config bytes."""

from __future__ import annotations

import os
import pwd
import sys
from scripts.delegate_acceptance.evidence.files import digest
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.supervisor.config_policy import resolved_supervisor_config_digest
from pathlib import Path


def supervisor_binding(pueue: Path, config: Path, base: Path,
                       config_digest: str, pueue_version: str) -> dict[str, object]:
    """Build a supervisor binding from harness-owned executable/config sources."""
    base = Path(base).resolve()
    config = Path(config).resolve()
    pueue = Path(pueue).resolve()
    binding = {
        "client_executable": str(pueue),
        "client_sha256": digest(pueue),
        "resolved_config_sha256": resolved_supervisor_config_digest(base),
        "endpoint": "unix:" + str(base / "run" / "p.sock"),
        "config_path": str(config),
        "config_digest": config_digest,
        "observed_version": pueue_version,
    }
    binding.update(supervisor_resolution())
    return binding


def supervisor_resolution() -> dict[str, str]:
    """Mirror the Go supervisor path selectors used by acceptance processes."""
    environment = {
        key: value for key, value in os.environ.items()
        if key in {"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR"}
    }
    home = os.path.normpath(environment.get("HOME") or pwd.getpwuid(os.getuid()).pw_dir)
    if sys.platform == "linux":
        operating_system = "linux"
        data_local = _absolute_environment_or(
            environment, "XDG_DATA_HOME", os.path.join(home, ".local", "share"))
        config = _absolute_environment_or(
            environment, "XDG_CONFIG_HOME", os.path.join(home, ".config"))
        runtime = _absolute_environment_or(environment, "XDG_RUNTIME_DIR", "")
    elif sys.platform == "darwin":
        operating_system = "darwin"
        data_local = os.path.normpath(os.path.join(home, "Library", "Application Support"))
        config = data_local
        runtime = ""
    else:
        raise AcceptanceFailure("unsupported acceptance platform: " + sys.platform)
    result = {
        "resolution_os": operating_system,
        "resolution_home": home,
        "resolution_data_local": data_local,
        "resolution_config": config,
        "resolution_username": pwd.getpwuid(os.getuid()).pw_name,
    }
    if runtime:
        result["resolution_runtime"] = runtime
    return result


def _absolute_environment_or(environment: dict[str, str], key: str, fallback: str) -> str:
    value = environment.get(key, "")
    if value and os.path.isabs(value):
        return os.path.normpath(value)
    return os.path.normpath(fallback) if fallback else ""


def pueue_command(pueue: Path, config: Path, *args: str) -> list[object]:
    return [str(pueue), "-c", str(config), *args]
