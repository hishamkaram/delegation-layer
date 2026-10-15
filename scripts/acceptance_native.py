#!/usr/bin/env python3
"""Run a bounded live acceptance gate for the native provider matrix.

The gate uses the shipped dispatcher and runner for every provider turn.  It
does not copy credentials, invoke a provider task directly, retry paid work,
or turn an authentication refusal into a passing result. A provider's native,
non-task authentication-status command may gate the turn when advertised here.
"""

from __future__ import annotations

import argparse
from copy import deepcopy
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import select
import secrets
import shlex
import shutil
import stat
import subprocess
import sys
import time
import traceback

from acceptance_provider_common import (
    AcceptanceFailure,
    INSPECTION_GROUP_PREFIX,
    NativeTaskOps,
    _hex_digest,
    _task_identity,
    clean_absolute,
    done_result,
    parse_json_output,
    pueue_command,
    reject_tmp,
    observed_pueue_version,
    require,
    snapshot,
    status_jobs,
    validate_queue_row,
    validate_provider_runtime_metadata,
    verify_collected_outcome,
    write_bytes,
)
from acceptance_supervisor_common import (
    DirectoryOwner, Processes,
    config_for,
    digest,
    finalize_acceptance_artifacts,
    inherited_environment,
    read_json,
    write_json,
    wait_until,
)
from native_acceptance_evidence import EvidenceError, validate_prepared_case_directory


PROFILES = {
    "ccr:print": {
        "executable": "ccr", "modes": ("read-only", "workspace-write"),
        "default_mode": "read-only",
    },
    "antigravity:print": {
        "executable": "agy", "modes": ("read-only", "workspace-write"),
        "default_mode": "workspace-write",
    },
    "claude:print": {
        "executable": "claude", "modes": ("read-only", "workspace-write"),
        "default_mode": "read-only",
        "authentication_status": ("auth", "status"),
    },
    "codex:exec": {
        "executable": "codex", "modes": ("read-only", "workspace-write"),
        "default_mode": "read-only",
    },
    "codex:review": {
        "executable": "codex", "modes": ("read-only",),
        "default_mode": "read-only",
    },
    "opencode:run": {
        "executable": "opencode", "modes": ("read-only", "workspace-write"),
        "default_mode": "read-only",
    },
    "pi:json": {
        "executable": "pi", "modes": ("read-only", "workspace-write"),
        "default_mode": "read-only",
    },
}
# A live provider may need to establish a remote session before it can perform
# the bounded fixture. Keep the gate finite, while giving that first turn the
# same ceiling already used by the model-discovery acceptance path.
TASK_BUDGET = "5m"
CANONICAL_TASK_BUDGET = "5m0s"
TASK_BUDGET_NANOS = 300_000_000_000
WATCH_SECONDS = 330
TIMEOUT_RESUME_SOURCE_BUDGET = "3m"
CANONICAL_TIMEOUT_RESUME_SOURCE_BUDGET = "3m0s"
TIMEOUT_RESUME_SOURCE_BUDGET_NANOS = 180_000_000_000
TIMEOUT_SOURCE_RUNNER_RESULTS = frozenset({"Success", "Failed"})
# Codex's Linux workspace-write sandbox temporarily materializes these missing
# protected metadata paths. Own them in the isolated timeout fixture so a
# forced provider stop cannot turn interrupted sandbox cleanup into test data.
CODEX_TIMEOUT_BASELINE_DIRECTORIES = (".agents", ".aws", ".codex")
TIMEOUT_LEASE_SETUP_SECONDS = WATCH_SECONDS
TIMEOUT_LEASE_STATUS_INTERVAL_SECONDS = 5
TIMEOUT_LEASE_OBSERVATION_SECONDS = 30
TIMEOUT_LEASE_MAX_BYTES = 512
TIMEOUT_LEASE_MODE = 0o600
# model-discovery-v1 allows provider CLIs to populate a remote model/cache
# catalog on their first run; this outer wait adds supervisor bootstrap time.
MODEL_DISCOVERY_TASK_BUDGET = "5m0s"
MODEL_DISCOVERY_TASK_BUDGET_NANOS = 300_000_000_000
MODEL_DISCOVERY_TIMEOUT_SECONDS = 330
MAX_RAW_BYTES = 8 * 1024 * 1024
MAX_CONTROL_BYTES = 1 << 20
AUTHENTICATION_STATUS_CODES = frozenset({401, 403})
MODEL_DISCOVERY_EXIT_CODES = {
    "available": 0,
    "partial": 0,
    "blocked": 2,
    "unavailable": 2,
    "failed": 1,
}
MODEL_DISCOVERY_REQUIRED_KEYS = {
    "schema_version", "command", "provider", "observed_at", "status",
    "reason_code", "complete", "source", "models", "efforts",
}
MODEL_DISCOVERY_OPTIONAL_KEYS = frozenset()
MODEL_INFO_REQUIRED_KEYS = {"id", "efforts"}
MODEL_INFO_OPTIONAL_KEYS = {"name", "default_effort"}
FAILURE_DIAGNOSTICS_BYTES = 256 * 1024
FAILURE_DIAGNOSTICS_MAX_ENTRIES = 4096
FAILURE_DIAGNOSTICS_MAX_TASKS = 512
FAILURE_DIAGNOSTICS_MAX_OMISSIONS = 64
FAILURE_DIAGNOSTICS_TEXT_BYTES = 256
# Native configuration/session locations, never inline credentials or config.
# Keep the shared fixture environment isolated; only this live gate opts in.
NATIVE_DISCOVERY_ENVIRONMENT = frozenset({
    "CODEX_HOME", "CLAUDE_CONFIG_DIR", "ANTHROPIC_CONFIG_DIR",
    "PI_CODING_AGENT_DIR", "PI_CODING_AGENT_SESSION_DIR",
    "OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_TUI_CONFIG",
    "OPENCODE_PERMISSION", "OPENCODE_AUTO_SHARE",
    "GEMINI_HOME", "GEMINI_CLI_HOME", "ANTIGRAVITY_HOME",
    "ANTIGRAVITY_CONFIG_HOME", "AGY_HOME", "AGY_CONFIG_HOME",
    "XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME", "XDG_DATA_DIRS",
    "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
    "DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK", "TMP", "TEMP",
    "GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTUP_HOME",
    "GRADLE_USER_HOME", "NPM_CONFIG_CACHE", "GOPATH",
})


def native_environment() -> dict[str, str]:
    environment = inherited_environment()
    environment.update((key, os.environ[key]) for key in NATIVE_DISCOVERY_ENVIRONMENT
                       if key in os.environ)
    return environment


class BlockedFailure(AcceptanceFailure):
    """A required provider, supervisor, or authentication prerequisite is absent."""


def resolve_executable(value: str | None, default: Path, label: str) -> Path:
    candidate = Path(value) if value else default
    if not candidate.is_absolute():
        discovered = shutil.which(str(candidate))
        if discovered is None:
            raise BlockedFailure(f"{label} is not discoverable")
        candidate = Path(discovered)
    if not candidate.is_file() or not os.access(candidate, os.X_OK):
        raise BlockedFailure(f"{label} is unavailable or not executable")
    resolved = candidate.resolve(strict=True)
    if not resolved.is_file() or not os.access(resolved, os.X_OK):
        raise BlockedFailure(f"{label} resolved target is not executable")
    return resolved


def require_discovery(name: str, selected: Path, environment: dict[str, str]) -> None:
    discovered = shutil.which(name, path=environment.get("PATH"))
    if discovered is None or Path(discovered).resolve() != selected:
        raise BlockedFailure(f"{name} discovery does not match the selected executable")


def choose_output(
        provider: str, explicit: str | None, permission: str | None = None,
        scenario: str | None = None) -> Path:
    if explicit:
        path = clean_absolute(explicit, "live acceptance output")
    else:
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + secrets.token_hex(6)
        path = Path.home() / ".local" / "state" / "delegation-layer-evidence" / f"{provider.replace(':', '-')}-{stamp}"
    reject_tmp(path, "live acceptance output")
    if path.exists() or path.is_symlink():
        if explicit and permission is not None and scenario is not None:
            try:
                return validate_prepared_case_directory(path, provider, permission, scenario)
            except EvidenceError as error:
                raise AcceptanceFailure(
                    "prepared live acceptance output is invalid: " + str(error)) from error
        raise AcceptanceFailure(f"live acceptance output already exists: {path}")
    path.mkdir(mode=0o700, parents=True, exist_ok=False)
    return path.resolve()


def authentication_unavailable(directory: Path) -> bool:
    outcome_path = directory / "outcome.json"
    if not outcome_path.is_file() or outcome_path.is_symlink():
        return False
    try:
        outcome = read_json(outcome_path)
    except (OSError, ValueError, TypeError, RecursionError, RuntimeError):
        return False
    if not isinstance(outcome, dict) or outcome.get("verdict") != "rejected":
        return False
    for relative in ("raw/stdout", "raw/stderr", "publish.reject"):
        path = directory / relative
        if not path.is_file() or path.is_symlink():
            continue
        try:
            with path.open("rb") as stream:
                data = stream.read(MAX_RAW_BYTES).decode("utf-8", errors="replace")
        except OSError:
            continue
        for line in data.splitlines():
            try:
                event = json.loads(line)
            except (TypeError, ValueError, RecursionError):
                continue
            if not isinstance(event, dict):
                continue
            error = event.get("error")
            details = error.get("data") if isinstance(error, dict) else None
            status_code = details.get("statusCode") if isinstance(details, dict) else None
            if type(status_code) is int and status_code in AUTHENTICATION_STATUS_CODES:
                return True
    return False


def authentication_blocked_response(response: object) -> bool:
    """Recognize the delegate's structured native-authentication block."""
    if not isinstance(response, dict) or response.get("status") != "blocked":
        return False
    capability = response.get("capability")
    live = capability.get("live_acceptance") if isinstance(capability, dict) else None
    return (isinstance(live, dict) and live.get("status") == "blocked" and
            live.get("authentication") == "blocked" and
            live.get("reason_code") == "authentication_unavailable")


def authentication_blocked(response: object, _diagnostic: str) -> bool:
    """Trust only Delegate's structured pre-admission authentication state."""
    return authentication_blocked_response(response)


def model_discovery_exit_code(status: str) -> int:
    try:
        return MODEL_DISCOVERY_EXIT_CODES[status]
    except KeyError as error:
        raise AcceptanceFailure(f"unknown model discovery status: {status!r}") from error


def validate_model_discovery_response(response: object, provider: str) -> dict[str, object]:
    """Validate the public model observation without turning it into a policy."""
    require(isinstance(response, dict), "model discovery response is not an object")
    keys = set(response)
    require(MODEL_DISCOVERY_REQUIRED_KEYS <= keys <=
            MODEL_DISCOVERY_REQUIRED_KEYS | MODEL_DISCOVERY_OPTIONAL_KEYS,
            "model discovery response has unknown or missing fields")
    require(type(response.get("schema_version")) is int and response["schema_version"] == 1,
            "model discovery schema version is invalid")
    require(response.get("command") == "models", "model discovery command is invalid")
    require(response.get("provider") == provider, "model discovery provider is invalid")
    observed_at = response.get("observed_at")
    require(isinstance(observed_at, str) and observed_at, "model discovery timestamp is invalid")
    try:
        parsed = datetime.fromisoformat(observed_at.replace("Z", "+00:00"))
    except ValueError as error:
        raise AcceptanceFailure("model discovery timestamp is invalid") from error
    require(parsed.tzinfo is not None, "model discovery timestamp has no timezone")
    status = response.get("status")
    require(isinstance(status, str) and status in MODEL_DISCOVERY_EXIT_CODES,
            "model discovery status is invalid")
    reason_code = response.get("reason_code")
    require(isinstance(reason_code, str) and reason_code and
            not any(character.isspace() for character in reason_code),
            "model discovery reason code is invalid")
    require(type(response.get("complete")) is bool, "model discovery complete flag is invalid")
    source = response.get("source")
    require(isinstance(source, str), "model discovery source is invalid")
    efforts = response.get("efforts")
    _validate_model_choices(efforts, "model discovery efforts")
    models = response.get("models")
    require(isinstance(models, list), "model discovery models is not an array")
    if status == "available":
        require(response["complete"] is True and models,
                "available model discovery must enumerate a complete nonempty list")
    elif status == "partial":
        require(response["complete"] is False and models,
                "partial model discovery must enumerate an incomplete nonempty list")
    elif status in {"blocked", "unavailable"}:
        require(response["complete"] is False and not models,
                f"{status} model discovery must not enumerate models")
    else:
        require(response["complete"] is False and not models,
                "failed model discovery must not enumerate models")
    seen: set[str] = set()
    for index, model in enumerate(models):
        require(isinstance(model, dict), f"model discovery model {index} is not an object")
        model_keys = set(model)
        require(MODEL_INFO_REQUIRED_KEYS <= model_keys <=
                MODEL_INFO_REQUIRED_KEYS | MODEL_INFO_OPTIONAL_KEYS,
                f"model discovery model {index} has unknown or missing fields")
        model_id = model.get("id")
        require(isinstance(model_id, str) and model_id and
                not any(ord(character) < 32 for character in model_id),
                f"model discovery model {index} id is invalid")
        require(model_id not in seen, f"model discovery has duplicate model id {model_id!r}")
        seen.add(model_id)
        for key in ("name", "default_effort"):
            if key in model:
                require(model[key] is None or
                        (isinstance(model[key], str) and bool(model[key]) and
                         not any(ord(character) < 32 for character in model[key])),
                        f"model discovery model {index} {key} is invalid")
        _validate_model_choices(model.get("efforts"),
                                f"model discovery model {index} efforts")
        default_effort = model.get("default_effort")
        model_efforts = model.get("efforts")
        if default_effort is not None and model_efforts is not None:
            require(default_effort in model_efforts,
                    f"model discovery model {index} default effort is not listed")
    return {
        "status": status,
        "reason_code": reason_code,
        "complete": response["complete"],
        "source": source,
        "model_count": len(models),
        "effort_count": None if efforts is None else len(efforts),
    }


def _validate_model_choices(value: object, label: str) -> None:
    require(value is None or isinstance(value, list), f"{label} is not null or an array")
    if value is None:
        return
    seen: set[str] = set()
    for choice in value:
        require(isinstance(choice, str) and choice and
                not any(ord(character) < 32 for character in choice),
                f"{label} contains an invalid choice")
        require(choice not in seen, f"{label} contains a duplicate choice")
        seen.add(choice)


def task_digest_snapshot(directory: Path) -> dict[str, dict[str, object]]:
    return snapshot(directory, ignored_prefixes=(".ack",))


def workspace_snapshot(directory: Path) -> dict[str, dict[str, object]]:
    """Record every workspace entry without opening special files."""
    directory = Path(directory)
    require(directory.is_dir() and not directory.is_symlink(),
            "workspace directory is absent: " + str(directory))
    result: dict[str, dict[str, object]] = {}
    for path in sorted(directory.rglob("*")):
        metadata = path.lstat()
        if stat.S_ISLNK(metadata.st_mode):
            raise AcceptanceFailure("workspace contains symlink: " + str(path))
        entry: dict[str, object] = {
            "type": stat.S_IFMT(metadata.st_mode),
            "mode": stat.S_IMODE(metadata.st_mode),
        }
        if stat.S_ISREG(metadata.st_mode):
            data = path.read_bytes()
            entry.update(bytes=len(data), sha256=hashlib.sha256(data).hexdigest())
        elif stat.S_ISCHR(metadata.st_mode) or stat.S_ISBLK(metadata.st_mode):
            entry["device"] = metadata.st_rdev
        result[str(path.relative_to(directory))] = entry
    return result


def _diagnostic_text_descriptor(value: object) -> dict[str, object]:
    """Describe text without persisting the text itself."""
    if not isinstance(value, str):
        return {"present": value is not None, "type": type(value).__name__}
    data = value.encode("utf-8", errors="replace")
    return {"present": True, "bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()}


def _delegate_status_summary(value: object) -> str:
    """Summarize Delegate's bounded public status response for a failed runner."""
    if not isinstance(value, dict):
        return "response=malformed"
    fields: list[str] = []
    for key in ("admission", "liveness", "publication", "status"):
        candidate = value.get(key)
        if isinstance(candidate, str) and candidate:
            fields.append(f"{key}={candidate[:FAILURE_DIAGNOSTICS_TEXT_BYTES]!r}")
    failure = value.get("failure")
    if isinstance(failure, dict):
        for key in ("code", "stage", "next_action"):
            candidate = failure.get(key)
            if isinstance(candidate, str) and candidate:
                fields.append(f"failure.{key}={candidate[:FAILURE_DIAGNOSTICS_TEXT_BYTES]!r}")
    error = value.get("error")
    if isinstance(error, str) and error:
        bounded = " ".join(error.split())[:FAILURE_DIAGNOSTICS_TEXT_BYTES]
        fields.append(f"error={bounded!r}")
    return ", ".join(fields) if fields else "response=no-actionable-fields"


def _diagnostic_regular_file(path: Path) -> tuple[dict[str, object] | None, str | None]:
    """Return metadata for one regular file without following unsafe entries."""
    try:
        metadata = path.lstat()
    except FileNotFoundError:
        return None, "missing"
    except OSError as error:
        return None, type(error).__name__
    if stat.S_ISLNK(metadata.st_mode):
        return None, "symlink"
    if not stat.S_ISREG(metadata.st_mode):
        return None, "special"
    try:
        return {
            "type": "file", "mode": stat.S_IMODE(metadata.st_mode),
            "bytes": metadata.st_size, "sha256": digest(path),
        }, None
    except OSError as error:
        return None, type(error).__name__


def _diagnostic_relative_path(value: object) -> str | None:
    if not isinstance(value, str) or not value or len(value) > FAILURE_DIAGNOSTICS_TEXT_BYTES:
        return None
    if any(ord(character) < 32 for character in value):
        return None
    path = Path(value)
    if path.is_absolute() or any(part in {"", ".", ".."} for part in path.parts):
        return None
    return str(path)


def _diagnostic_payload(value: object) -> dict[str, object] | None:
    if not isinstance(value, dict):
        return None
    result: dict[str, object] = {}
    basename = value.get("basename")
    if isinstance(basename, str) and basename and Path(basename).name == basename and \
            len(basename) <= FAILURE_DIAGNOSTICS_TEXT_BYTES:
        result["basename"] = basename
    length = value.get("length")
    if type(length) is int and length >= 0:
        result["length"] = length
    checksum = value.get("sha256")
    if isinstance(checksum, str) and len(checksum) == 64 and \
            all(character in "0123456789abcdef" for character in checksum):
        result["sha256"] = checksum
    return result or None


def _diagnostic_predicate(value: object) -> dict[str, object] | None:
    if not isinstance(value, dict):
        return None
    result: dict[str, object] = {}
    for key in ("adapter", "mode", "version"):
        candidate = value.get(key)
        if isinstance(candidate, str) and len(candidate) <= FAILURE_DIAGNOSTICS_TEXT_BYTES and \
                not any(ord(character) < 32 for character in candidate):
            result[key] = candidate
    checksum = value.get("sha256")
    if isinstance(checksum, str) and len(checksum) == 64 and \
            all(character in "0123456789abcdef" for character in checksum):
        result["sha256"] = checksum
    return result or None


def _diagnostic_output_diagnostics(value: object) -> list[dict[str, object]]:
    if not isinstance(value, list):
        return []
    result: list[dict[str, object]] = []
    for item in value[:FAILURE_DIAGNOSTICS_MAX_ENTRIES]:
        if not isinstance(item, dict):
            continue
        diagnostic: dict[str, object] = {}
        for key in ("code", "message", "field"):
            candidate = item.get(key)
            if isinstance(candidate, str) and candidate and \
                    len(candidate) <= FAILURE_DIAGNOSTICS_TEXT_BYTES and \
                    not any(ord(character) < 32 for character in candidate):
                diagnostic[key] = candidate
        for key in ("event", "offset"):
            candidate = item.get(key)
            if type(candidate) is int and candidate >= 0:
                diagnostic[key] = candidate
        if "code" in diagnostic and "message" in diagnostic:
            result.append(diagnostic)
    return result


def _diagnostic_outcome(value: object) -> dict[str, object] | None:
    if not isinstance(value, dict):
        return None
    result: dict[str, object] = {}
    verdict = value.get("verdict")
    if isinstance(verdict, str) and len(verdict) <= FAILURE_DIAGNOSTICS_TEXT_BYTES:
        result["verdict"] = verdict
    predicate = _diagnostic_predicate(value.get("predicate"))
    if predicate is not None:
        result["predicate"] = predicate
    payload = _diagnostic_payload(value.get("payload"))
    if payload is not None:
        result["payload"] = payload
    evidence = value.get("evidence_sha256")
    if isinstance(evidence, str) and len(evidence) == 64 and \
            all(character in "0123456789abcdef" for character in evidence):
        result["evidence_sha256"] = evidence
    diagnostics = _diagnostic_output_diagnostics(value.get("diagnostics"))
    if diagnostics:
        result["diagnostics"] = diagnostics
    return result or None


def _rejection_diagnostic_summary(outcome: dict[str, object]) -> str:
    diagnostics = _diagnostic_output_diagnostics(outcome.get("diagnostics"))
    if not diagnostics:
        return "verified rejection has no structured diagnostic"
    first = diagnostics[0]
    fields = [f"code={first['code']!r}"]
    if "field" in first:
        fields.append(f"field={first['field']!r}")
    fields.append(f"message={first['message']!r}")
    return ", ".join(fields)


def _diagnostic_provider_exit(value: object) -> dict[str, object] | None:
    if not isinstance(value, dict):
        return None
    result: dict[str, object] = {}
    for key in ("invocation_state", "exit_code"):
        candidate = value.get(key)
        if (key == "invocation_state" and isinstance(candidate, str) and
                len(candidate) <= FAILURE_DIAGNOSTICS_TEXT_BYTES) or \
                (key == "exit_code" and type(candidate) is int):
            result[key] = candidate
    if "error" in value:
        result["error"] = _diagnostic_text_descriptor(value.get("error"))
    manifest = value.get("raw_manifest")
    if isinstance(manifest, list):
        result["raw_manifest_count"] = len(manifest)
        entries: list[dict[str, object]] = []
        for item in manifest[:FAILURE_DIAGNOSTICS_MAX_ENTRIES]:
            if not isinstance(item, dict):
                continue
            entry: dict[str, object] = {}
            relative = _diagnostic_relative_path(item.get("path"))
            if relative is not None:
                entry["path"] = relative
            for key in ("bytes", "length"):
                candidate = item.get(key)
                if type(candidate) is int and candidate >= 0:
                    entry[key] = candidate
            checksum = item.get("sha256")
            if isinstance(checksum, str) and len(checksum) == 64 and \
                    all(character in "0123456789abcdef" for character in checksum):
                entry["sha256"] = checksum
            if entry:
                entries.append(entry)
        result["raw_manifest"] = entries
        if len(manifest) > FAILURE_DIAGNOSTICS_MAX_ENTRIES:
            result["raw_manifest_truncated"] = True
    manifest_checksum = value.get("manifest_sha256")
    if isinstance(manifest_checksum, str) and len(manifest_checksum) == 64 and \
            all(character in "0123456789abcdef" for character in manifest_checksum):
        result["manifest_sha256"] = manifest_checksum
    return result or None


def _diagnostic_workspace(directory: Path) -> dict[str, object]:
    """Capture only metadata and hashes from the test-owned workspace."""
    result: dict[str, object] = {"entries": {}, "omitted": [], "truncated": False}
    omitted: list[str] = []
    entries: dict[str, dict[str, object]] = {}
    try:
        root = directory.lstat()
    except OSError as error:
        result["omitted"] = [type(error).__name__]
        return result
    if stat.S_ISLNK(root.st_mode) or not stat.S_ISDIR(root.st_mode):
        result["omitted"] = ["workspace-root"]
        return result
    try:
        paths = sorted(directory.rglob("*"))
    except OSError as error:
        result["omitted"] = [type(error).__name__]
        return result
    for path in paths:
        relative = str(path.relative_to(directory))
        if len(entries) >= FAILURE_DIAGNOSTICS_MAX_ENTRIES:
            result["truncated"] = True
            break
        try:
            metadata = path.lstat()
        except OSError as error:
            omitted.append(relative + ":" + type(error).__name__)
            continue
        if stat.S_ISLNK(metadata.st_mode):
            omitted.append(relative + ":symlink")
            continue
        if stat.S_ISDIR(metadata.st_mode):
            entries[relative] = {"type": "directory", "mode": stat.S_IMODE(metadata.st_mode)}
            continue
        if not stat.S_ISREG(metadata.st_mode):
            omitted.append(relative + ":special")
            continue
        try:
            entries[relative] = {
                "type": "file", "mode": stat.S_IMODE(metadata.st_mode),
                "bytes": metadata.st_size, "sha256": digest(path),
            }
        except OSError as error:
            omitted.append(relative + ":" + type(error).__name__)
    result["entries"] = entries
    result["omitted"] = omitted[:FAILURE_DIAGNOSTICS_MAX_OMISSIONS]
    result["omitted_count"] = len(omitted)
    if len(omitted) > FAILURE_DIAGNOSTICS_MAX_OMISSIONS:
        result["omitted_truncated"] = True
    return result


def _diagnostic_json_bytes(value: object) -> bytes:
    return (json.dumps(value, sort_keys=True, indent=2) + "\n").encode()


def bounded_text(path: Path, label: str, bound: int = MAX_RAW_BYTES) -> str:
    with path.open("rb") as stream:
        data = stream.read(bound + 1)
    require(len(data) <= bound, label + " exceeds the bounded output limit")
    try:
        return data.decode("utf-8")
    except UnicodeDecodeError as error:
        raise AcceptanceFailure(label + " is not valid UTF-8") from error


def marker_answer(marker: str, continuation: bool) -> str:
    return f"DELEGATION_LIVE_{'CONTINUED' if continuation else 'OK'}: {marker}"


def random_marker() -> bytes:
    return ("DELEGATION-LIVE-" + secrets.token_hex(24) + "\n").encode()


class TimeoutFifoLease:
    """One harness-created FIFO whose writer lifetime is the liveness lease."""

    def __init__(self, path: Path, label: str, ready_marker: str) -> None:
        if not sys.platform.startswith(("linux", "darwin")):
            raise BlockedFailure("timeout liveness leases require Linux or Darwin")
        self.path = path
        self.label = label
        self.expected = (ready_marker + "\n").encode()
        self.device: int | None = None
        self.inode: int | None = None
        self.fd: int | None = None
        self.buffer = bytearray()
        self.writer_observed = False
        self.ready = False
        self.eof = False
        try:
            os.mkfifo(self.path, mode=TIMEOUT_LEASE_MODE)
            self._verify_path()
            flags = os.O_RDONLY | os.O_NONBLOCK | getattr(os, "O_CLOEXEC", 0)
            self.fd = os.open(self.path, flags)
        except (AttributeError, NotImplementedError, OSError) as error:
            self.close()
            try:
                if self.path.is_fifo() and not self.path.is_symlink():
                    self.path.unlink()
            except OSError:
                pass
            raise BlockedFailure("timeout liveness FIFO is unavailable on this filesystem") from error

    def _verify_path(self) -> None:
        try:
            metadata = self.path.lstat()
        except FileNotFoundError as error:
            raise AcceptanceFailure(f"{self.label} FIFO disappeared") from error
        except OSError as error:
            raise AcceptanceFailure(f"{self.label} FIFO cannot be inspected") from error
        require(not stat.S_ISLNK(metadata.st_mode) and stat.S_ISFIFO(metadata.st_mode),
                f"{self.label} FIFO was replaced or has the wrong type")
        if self.device is None or self.inode is None:
            self.device = metadata.st_dev
            self.inode = metadata.st_ino
            return
        require(metadata.st_dev == self.device and metadata.st_ino == self.inode,
                f"{self.label} FIFO was replaced")

    def observe(self) -> None:
        self._verify_path()
        require(self.fd is not None, f"{self.label} FIFO reader is unavailable")
        if self.eof:
            return
        poller = select.poll()
        poller.register(self.fd, select.POLLIN | select.POLLHUP | select.POLLERR | select.POLLNVAL)
        events = poller.poll(0)
        if not events and not self.writer_observed:
            return
        flags = events[0][1] if events else 0
        require(not flags & (select.POLLERR | select.POLLNVAL),
                f"{self.label} FIFO observation failed")
        while True:
            try:
                chunk = os.read(self.fd, TIMEOUT_LEASE_MAX_BYTES)
            except BlockingIOError:
                return
            except OSError as error:
                raise AcceptanceFailure(f"{self.label} FIFO could not be read") from error
            if not chunk:
                if self.writer_observed:
                    self.eof = True
                    require(self.ready,
                            f"{self.label} FIFO closed before exact readiness")
                return
            self.writer_observed = True
            self.buffer.extend(chunk)
            require(len(self.buffer) <= len(self.expected),
                    f"{self.label} FIFO has extra or malformed evidence")
            if self.buffer == self.expected:
                self.ready = True
            else:
                require(self.expected.startswith(self.buffer),
                        f"{self.label} FIFO has malformed readiness evidence")

    def close(self) -> None:
        if self.fd is None:
            return
        try:
            os.close(self.fd)
        except OSError:
            pass
        self.fd = None


def verify_read_only_answer(payload: bytes, expected: str, label: str) -> None:
    try:
        answer = payload.decode("utf-8")
    except UnicodeDecodeError as error:
        raise AcceptanceFailure(f"{label} answer is not valid UTF-8") from error
    require(answer.strip() == expected,
            f"{label} did not return the exact marker-derived answer")


def verify_interactive_answer(payload: bytes, label: str) -> None:
    verify_read_only_answer(payload, "DELEGATION_LIVE_INTERACTIVE_OK: Blue", label)


def verify_review_answer(payload: bytes, label: str) -> None:
    try:
        answer = payload.decode("utf-8")
    except UnicodeDecodeError as error:
        raise AcceptanceFailure(f"{label} review answer is not valid UTF-8") from error
    require(bool(answer.strip()), f"{label} returned an empty review")


def verify_shell_digest_artifact(target: Path, artifact: Path, label: str) -> None:
    """Require an actual shell-produced digest bound to the final target bytes."""
    require(target.is_file() and not target.is_symlink(), f"{label} target is not a regular file")
    require(artifact.is_file() and not artifact.is_symlink(), f"{label} artifact is not a regular file")
    expected = hashlib.sha256(target.read_bytes()).hexdigest()
    fields = bounded_text(artifact, f"{label} artifact").split()
    require(len(fields) == 2 and fields[0] == expected and
            Path(fields[1].lstrip("*")).name == target.name,
            f"{label} artifact is not a digest of the final target")


def verify_write_artifacts(nonce: str, target: Path, artifact: Path,
                           continuation: bool = False) -> None:
    """Validate filesystem evidence, independent of provider response prose."""
    suffix = "\ncontinued" if continuation else ""
    require(target.read_bytes().removesuffix(b"\n") == f"edited:{nonce}{suffix}".encode(),
            "write task did not leave the exact edited target")
    verify_shell_digest_artifact(target, artifact, "write task")


def verify_write_fixture(nonce: str, fixture: Path) -> None:
    """Require the pre-existing fixture to have been edited to its final value."""
    require(fixture.is_file() and not fixture.is_symlink(),
            "write task fixture is not a regular file")
    require(fixture.read_bytes().removesuffix(b"\n") ==
            f"edited-fixture:{nonce}".encode(),
            "write task did not edit the pre-existing fixture")


def shell_digest_command(artifact: str) -> str:
    program = ('import hashlib; from pathlib import Path; '
               'p = Path("acceptance-write-target.txt"); '
               'print(hashlib.sha256(p.read_bytes()).hexdigest(), p.name)')
    return f"python3 -c {shlex.quote(program)} > {shlex.quote(artifact)}"


def require_same_session(first: str, second: str) -> None:
    require(first == second, "continuation changed the provider session")


def require_replay_unchanged(before: dict[str, dict[str, object]],
                             after: dict[str, dict[str, object]]) -> None:
    require(before == after, "collection replay changed immutable evidence")


def dispatch_binding(response: dict[str, object], task_id: str, label: str) -> tuple[str, int]:
    require(response.get("task_id") == task_id, f"{label} changed task identity")
    root_id = response.get("root_id")
    require(isinstance(root_id, str) and len(root_id) == 32 and
            all(character in "0123456789abcdef" for character in root_id),
            f"{label} returned no root identity")
    supervisor = response.get("supervisor")
    require(isinstance(supervisor, dict) and supervisor.get("matched") is True,
            f"{label} has no matched supervisor identity")
    numeric_id = supervisor.get("numeric_task_id")
    require(type(numeric_id) is int and numeric_id >= 0,
            f"{label} has no numeric supervisor identity")
    return root_id, numeric_id


class ModelDiscoveryCleanupOps(NativeTaskOps):
    """Use the model discovery state root for failure cleanup."""

    def __init__(self, owner: "NativeAcceptance") -> None:
        super().__init__(owner.processes, owner.delegate, owner.runner, owner.pueue,
                         owner.models_state.parent, owner.models_state, owner.output,
                         expected_inspection_binding_required=False)
        self.owner = owner

    def failure_queue_finished(self, status: dict[str, object]) -> bool:
        self.owner._sync_model_cleanup(self)
        return super().failure_queue_finished(status)


class NativeAcceptance:
    def __init__(self, args: argparse.Namespace):
        if args.provider not in PROFILES:
            raise AcceptanceFailure(f"unsupported live provider: {args.provider}")
        profile = PROFILES[args.provider]
        requested_mode = args.mode or profile["default_mode"]
        if requested_mode not in profile["modes"]:
            supported = ", ".join(profile["modes"])
            raise AcceptanceFailure(f"{args.provider} does not support {requested_mode}; supports {supported}")
        scenario = getattr(args, "scenario", None)
        if scenario is None:
            scenario = "write" if requested_mode == "workspace-write" else "read-only"
        if scenario not in {"read-only", "write", "timeout-resume", "interactive"}:
            raise AcceptanceFailure(f"unsupported native acceptance scenario: {scenario}")
        if scenario == "read-only" and requested_mode != "read-only":
            raise AcceptanceFailure("read-only scenario requires --permission read-only")
        if scenario == "write" and requested_mode != "workspace-write":
            raise AcceptanceFailure("write scenario requires --permission workspace-write")
        if scenario == "timeout-resume" and (args.provider != "codex:exec" or
                                                requested_mode != "workspace-write"):
            raise AcceptanceFailure("timeout-resume scenario requires codex:exec workspace-write")
        if scenario == "interactive" and (args.provider != "claude:print" or
                                            requested_mode != "read-only"):
            raise AcceptanceFailure("interactive scenario requires claude:print read-only")
        discover_models = bool(getattr(args, "discover_models", False))
        self.args = args
        self.provider = args.provider
        self.profile = profile
        self.mode = requested_mode
        self.scenario = scenario
        self.discover_models_only = discover_models
        self.environment = native_environment()
        self.output = clean_absolute(args.output, "live acceptance output")
        require(self.output.is_dir() and not self.output.is_symlink(),
                "live acceptance output is not a regular directory")
        self.delegate = resolve_executable(args.delegate, Path(__file__).resolve().parent.parent / "bin/delegate", "delegate")
        self.runner = resolve_executable(args.runner, Path(__file__).resolve().parent.parent / "bin/delegate-run", "delegate-run")
        self.pueue = resolve_executable(args.pueue, Path(shutil.which("pueue", path=self.environment.get("PATH")) or "pueue"), "pueue")
        require_discovery("pueue", self.pueue, self.environment)
        self.pueued = resolve_executable(args.pueued, Path(shutil.which("pueued", path=self.environment.get("PATH")) or "pueued"), "pueued")
        discovered_provider = shutil.which(profile["executable"], path=self.environment.get("PATH"))
        self.provider_executable = resolve_executable(
            args.provider_executable, Path(discovered_provider or profile["executable"]), profile["executable"])
        require_discovery(profile["executable"], self.provider_executable, self.environment)
        self.git: Path | None = None
        if self.provider in {"codex:exec", "codex:review"}:
            discovered_git = shutil.which("git", path=self.environment.get("PATH"))
            try:
                self.git = resolve_executable(discovered_git, Path("/usr/bin/git"), "git")
            except BlockedFailure as error:
                raise BlockedFailure("Codex git prerequisite is unavailable") from error

        self._constructor_ownership = DirectoryOwner()
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + secrets.token_hex(6)
        # Provider identity is recorded in the receipt; keep the disposable path
        # short enough for Darwin's Unix socket address limit.
        self.base_parent = Path.home() / "delegation-layer-live"
        self.base = self.base_parent / stamp
        try:
            reject_tmp(self.base, "live acceptance base")
            if self.base_parent.exists() or self.base_parent.is_symlink():
                require(self.base_parent.is_dir() and not self.base_parent.is_symlink(),
                        "live acceptance base parent is unsafe")
            else:
                self.base_parent = self._constructor_ownership.create(
                    "acceptance base parent", self.base_parent,
                    self.base_parent.parent, mode=0o700, parents=True).directory
            self.base = self._constructor_ownership.create(
                "acceptance base", self.base, self.base_parent).directory
            self._initialize_runtime_roots()
        except BaseException as error:
            try:
                self._constructor_ownership.rollback()
            except BaseException as rollback_error:
                raise RuntimeError(
                    "native acceptance constructor rollback failed: " + str(rollback_error)) from error
            raise
        self.owned_directories = self._constructor_ownership.transfer()

    def _initialize_runtime_roots(self) -> None:
        self.state = self.base / "state"
        self.workspace = self.base / "workspace"
        self.briefs = self.base / "briefs"
        self.pueue_base = self.base / "pueue"
        self.models_state = self.base / "models-state"
        for label, path, parent in (
            ("acceptance state", self.state, self.base),
            ("acceptance workspace", self.workspace, self.base),
            ("acceptance briefs", self.briefs, self.base),
            ("private pueue", self.pueue_base, self.base),
            ("private pueue state", self.pueue_base / "state", self.pueue_base),
            ("private pueue runtime", self.pueue_base / "run", self.pueue_base),
            ("model discovery state", self.models_state, self.base),
        ):
            self._constructor_ownership.create(label, path, parent)
        self.timeout_marker_file = self.workspace / "acceptance-timeout-marker.txt"
        self.timeout_resume_file = self.workspace / "acceptance-timeout-resumed.txt"
        self.timeout_marker = "timeout-marker:" + secrets.token_hex(24)
        self.timeout_resume_marker = "timeout-resumed:" + secrets.token_hex(24)
        # The provider must open these leases from inside its native
        # workspace-write boundary on every supported platform.
        self.timeout_lease_directory = self.workspace / "acceptance-timeout-leases"
        self.timeout_parent_fifo = self.timeout_lease_directory / "parent.fifo"
        self.timeout_child_fifo = self.timeout_lease_directory / "child.fifo"
        self.timeout_parent_ready_marker = "timeout-parent-ready:" + secrets.token_hex(24)
        self.timeout_child_ready_marker = "timeout-child-ready:" + secrets.token_hex(24)
        self.timeout_leases: dict[str, TimeoutFifoLease] = {}
        if self.scenario == "timeout-resume":
            self._constructor_ownership.create(
                "timeout liveness leases", self.timeout_lease_directory, self.workspace)
            self.timeout_leases = {
                "parent": TimeoutFifoLease(
                    self.timeout_parent_fifo, "timeout parent", self.timeout_parent_ready_marker),
                "child": TimeoutFifoLease(
                    self.timeout_child_fifo, "timeout child", self.timeout_child_ready_marker),
            }
        self.config = self.pueue_base / "pueue.yml"
        write_json(self.config, config_for(self.pueue_base))
        self.processes = Processes(
            self.output / "processes", self.environment,
            owner=self._constructor_ownership)
        self.ops = NativeTaskOps(self.processes, self.delegate, self.runner, self.pueue,
                                 self.base, self.state, self.output,
                                 watch_seconds=WATCH_SECONDS,
                                 expected_inspection_binding_required=False,
                                 managed_runner=self.scenario == "timeout-resume")
        self.daemon = None
        self.closed = False
        self.model_discovery: dict[str, object] | None = None
        self.model_discovery_summary: dict[str, object] | None = None
        self.model_discovery_exit_code: int | None = None
        self.models_root_id: str | None = None
        self.model_discovery_task_id: str | None = None
        self.root_id: str | None = None
        self.provider_version: str | None = None
        # A provider may update or be relinked between turns.  Immutable task
        # metadata owns each admitted runtime identity; this append-only map is
        # derived from those records for the terminal acceptance receipt.
        self.provider_runtime_identities: dict[str, dict[str, str]] = {}
        self.pueue_version: str | None = None
        self.pueued_version: str | None = None
        self.interaction_observation_number = 0
        self.tasks: dict[str, str] = {}
        self.numeric_ids: dict[str, int] = {}
        self.records: dict[str, dict[str, object]] = {}
        self.workspace_baseline: dict[str, dict[str, object]] | None = None
        self.workspace_marker = self.workspace / "acceptance-marker.txt"
        self.marker_bytes = random_marker()
        write_bytes(self.workspace_marker, self.marker_bytes)
        self.marker_text = bounded_text(self.workspace_marker, "acceptance marker").removesuffix("\n")
        require(self.marker_bytes == (self.marker_text + "\n").encode(),
                "acceptance marker has an unexpected value")
        self.nonce: str | None = None
        self.nonce_file = self.workspace / "acceptance-write-nonce.txt"
        self.fixture_file = self.workspace / "acceptance-write-fixture.txt"
        self.target_file = self.workspace / "acceptance-write-target.txt"
        self.check_file = self.workspace / "acceptance-write-check.txt"
        self.continue_check_file = self.workspace / "acceptance-write-continue-check.txt"
        if self.mode == "workspace-write":
            self.nonce = secrets.token_hex(24)
            write_bytes(self.nonce_file, (self.nonce + "\n").encode())
            write_bytes(self.fixture_file, (f"preexisting:{self.nonce}\n").encode())

    @staticmethod
    def _fit_failure_diagnostics(value: dict[str, object]) -> dict[str, object]:
        """Keep the persisted diagnostic bounded while retaining structure."""
        status = value["status"]
        require(isinstance(status, dict), "failure diagnostic status is malformed")

        def mark_truncated(reason: str) -> None:
            status["partial"] = True
            truncated = status.setdefault("truncated", [])
            require(isinstance(truncated, list), "failure diagnostic truncation state is malformed")
            if reason not in truncated:
                truncated.append(reason)

        if len(_diagnostic_json_bytes(value)) <= FAILURE_DIAGNOSTICS_BYTES:
            return value

        workspace = value.get("workspace")
        entries = workspace.get("entries") if isinstance(workspace, dict) else None
        if isinstance(entries, dict):
            while entries and len(_diagnostic_json_bytes(value)) > FAILURE_DIAGNOSTICS_BYTES:
                keep = max(0, len(entries) // 2)
                workspace["entries"] = dict(list(entries.items())[:keep])
                entries = workspace["entries"]
            mark_truncated("workspace.entries")

        if len(_diagnostic_json_bytes(value)) > FAILURE_DIAGNOSTICS_BYTES:
            tasks = value.get("tasks")
            if isinstance(tasks, list):
                for task in tasks:
                    if not isinstance(task, dict):
                        continue
                    provider_exit = task.get("provider_exit")
                    if isinstance(provider_exit, dict) and provider_exit.get("raw_manifest"):
                        provider_exit["raw_manifest"] = []
                        provider_exit["raw_manifest_omitted"] = True
                        mark_truncated("provider_exit.raw_manifest")
                    if "files" in task:
                        task.pop("files")
                        task["files_omitted"] = True
                mark_truncated("task.files")

        if len(_diagnostic_json_bytes(value)) > FAILURE_DIAGNOSTICS_BYTES and isinstance(workspace, dict):
            workspace["entries"] = {}
            mark_truncated("workspace.entries.all")

        if len(_diagnostic_json_bytes(value)) > FAILURE_DIAGNOSTICS_BYTES:
            tasks = value.get("tasks")
            if isinstance(tasks, list):
                value["tasks"] = [
                    {"name": task.get("name"), "task_id": task.get("task_id")}
                    for task in tasks if isinstance(task, dict)
                ]
                mark_truncated("task.details")

        if len(_diagnostic_json_bytes(value)) > FAILURE_DIAGNOSTICS_BYTES:
            task_ids = value.get("task_ids")
            if isinstance(task_ids, list):
                while task_ids and len(_diagnostic_json_bytes(value)) > FAILURE_DIAGNOSTICS_BYTES:
                    task_ids[:] = task_ids[:max(0, len(task_ids) // 2)]
                mark_truncated("task_ids")

        require(len(_diagnostic_json_bytes(value)) <= FAILURE_DIAGNOSTICS_BYTES,
                "failure diagnostic could not be bounded")
        status["partial"] = True
        return value

    @staticmethod
    def _diagnostic_task(state: Path, task_id: str, omitted: list[str]) -> dict[str, object]:
        task_directory = state / "tasks" / task_id
        result: dict[str, object] = {"task_id": task_id, "files": {}}
        try:
            state_metadata = state.lstat()
            tasks_metadata = (state / "tasks").lstat()
            metadata = task_directory.lstat()
        except OSError as error:
            omitted.append(f"task:{task_id}:{type(error).__name__}")
            return result
        if (stat.S_ISLNK(state_metadata.st_mode) or not stat.S_ISDIR(state_metadata.st_mode) or
                stat.S_ISLNK(tasks_metadata.st_mode) or not stat.S_ISDIR(tasks_metadata.st_mode) or
                stat.S_ISLNK(metadata.st_mode) or not stat.S_ISDIR(metadata.st_mode)):
            omitted.append(f"task:{task_id}:unsafe-directory")
            return result

        files: dict[str, dict[str, object]] = {}
        decoded: dict[str, object] = {}
        for name in ("task.json", "meta.json", "provider.exit", "outcome.json",
                     "provider.ref.json", "result.txt", "publish.reject"):
            descriptor, reason = _diagnostic_regular_file(task_directory / name)
            if descriptor is not None:
                files[name] = descriptor
            elif reason not in (None, "missing"):
                omitted.append(f"task:{task_id}:{name}:{reason}")
                continue
            if name in {"provider.exit", "outcome.json"} and descriptor is not None:
                try:
                    decoded[name] = read_json(task_directory / name, bound=MAX_CONTROL_BYTES)
                except (OSError, ValueError, TypeError, RecursionError, RuntimeError):
                    omitted.append(f"task:{task_id}:{name}:unreadable")

        outcome = _diagnostic_outcome(decoded.get("outcome.json"))
        if outcome is not None:
            result["outcome"] = outcome
        provider_exit = _diagnostic_provider_exit(decoded.get("provider.exit"))
        if provider_exit is not None:
            result["provider_exit"] = provider_exit
        result["files"] = files
        return result

    def _failure_diagnostics(self) -> dict[str, object]:
        omitted: list[str] = []
        status: dict[str, object] = {
            "partial": False, "omitted": [], "omitted_count": 0, "truncated": [],
        }

        def record_omissions(values: list[str]) -> None:
            if not values:
                return
            status["partial"] = True
            status["omitted_count"] = int(status["omitted_count"]) + len(values)
            current = status["omitted"]
            require(isinstance(current, list), "failure diagnostic omission state is malformed")
            remaining = max(0, FAILURE_DIAGNOSTICS_MAX_OMISSIONS - len(current))
            current.extend(str(value)[:FAILURE_DIAGNOSTICS_TEXT_BYTES] for value in values[:remaining])

        failure: dict[str, object] = {}
        failure_path = self.output / "failure.json"
        try:
            raw_failure = read_json(failure_path, bound=MAX_CONTROL_BYTES)
        except (OSError, ValueError, TypeError, RecursionError, RuntimeError):
            raw_failure = None
            omitted.append("failure.json")
        if isinstance(raw_failure, dict):
            for key in ("gate", "status", "provider", "mode", "scenario", "task_budget"):
                value = raw_failure.get(key)
                if isinstance(value, str) and len(value) <= FAILURE_DIAGNOSTICS_TEXT_BYTES and \
                        not any(ord(character) < 32 for character in value):
                    failure[key] = value
            for key in ("requested_model", "requested_effort"):
                value = raw_failure.get(key)
                if value is None or (isinstance(value, str) and
                                     len(value) <= FAILURE_DIAGNOSTICS_TEXT_BYTES and
                                     not any(ord(character) < 32 for character in value)):
                    failure[key] = value
            for key in ("discover_models", "no_retry", "credentials_in_receipt"):
                value = raw_failure.get(key)
                if type(value) is bool:
                    failure[key] = value
            if "error" in raw_failure:
                failure["error"] = _diagnostic_text_descriptor(raw_failure.get("error"))
        else:
            record_omissions(omitted)

        task_specs: list[tuple[str, str, Path]] = []
        seen: set[str] = set()
        for name, task_id in sorted(getattr(self, "tasks", {}).items()):
            if not isinstance(name, str) or not _task_identity(task_id) or task_id in seen:
                omitted.append("admitted-task")
                continue
            seen.add(task_id)
            task_specs.append((name, task_id, self.state))
        model_task = getattr(self, "model_discovery_task_id", None)
        models_state = getattr(self, "models_state", None)
        if _task_identity(model_task) and model_task not in seen and isinstance(models_state, Path):
            task_specs.append(("model-discovery", model_task, models_state))
            seen.add(model_task)
        if len(task_specs) > FAILURE_DIAGNOSTICS_MAX_TASKS:
            omitted.append("admitted-task-list")
            task_specs = task_specs[:FAILURE_DIAGNOSTICS_MAX_TASKS]

        tasks = []
        numeric_ids = getattr(self, "numeric_ids", {})
        for name, task_id, state_root in task_specs:
            task = self._diagnostic_task(state_root, task_id, omitted)
            task["name"] = name[:FAILURE_DIAGNOSTICS_TEXT_BYTES]
            numeric_id = numeric_ids.get(task_id)
            if type(numeric_id) is int and numeric_id >= 0:
                task["numeric_id"] = numeric_id
            tasks.append(task)

        workspace_path = getattr(self, "workspace", None)
        if isinstance(workspace_path, Path):
            workspace = _diagnostic_workspace(workspace_path)
            workspace_omitted = workspace.pop("omitted", [])
            if isinstance(workspace_omitted, list):
                omitted.extend(str(value) for value in workspace_omitted)
            if workspace.pop("omitted_count", 0):
                status["partial"] = True
            if workspace.pop("omitted_truncated", False):
                status["truncated"].append("workspace.omitted")
            if workspace.pop("truncated", False):
                status["truncated"].append("workspace.entries")
                status["partial"] = True
        else:
            workspace = {"entries": {}}
            omitted.append("workspace")

        record_omissions(omitted)
        result: dict[str, object] = {
            "schema_version": 1,
            "provider": getattr(self, "provider", None),
            "mode": getattr(self, "mode", None),
            "scenario": getattr(self, "scenario", None),
            "failure": failure,
            "task_ids": [task_id for _name, task_id, _state in task_specs],
            "tasks": tasks,
            "workspace": workspace,
            "status": status,
        }
        status["partial"] = bool(status["partial"] or status["omitted_count"] or status["truncated"])
        return self._fit_failure_diagnostics(result)

    def _write_failure_diagnostics(self) -> bool:
        path = self.output / "failure-diagnostics.json"
        try:
            try:
                metadata = path.lstat()
            except FileNotFoundError:
                metadata = None
            if metadata is not None:
                require(stat.S_ISREG(metadata.st_mode) and not stat.S_ISLNK(metadata.st_mode),
                        "failure diagnostic path is unsafe")
                data = path.read_bytes()
                require(len(data) <= FAILURE_DIAGNOSTICS_BYTES,
                        "existing failure diagnostic exceeds its bound")
                value = json.loads(data)
                require(isinstance(value, dict) and value.get("schema_version") == 1,
                        "existing failure diagnostic is malformed")
                return True
            value = self._failure_diagnostics()
            data = _diagnostic_json_bytes(value)
            require(len(data) <= FAILURE_DIAGNOSTICS_BYTES,
                    "failure diagnostic exceeds its bound")
            write_json(path, value, replace=True)
            require(path.is_file() and not path.is_symlink() and
                    path.stat().st_size <= FAILURE_DIAGNOSTICS_BYTES,
                    "persisted failure diagnostic exceeds its bound")
            return True
        except BaseException:
            staging = path.with_name(path.name + ".staging")
            try:
                if staging.is_file() and not staging.is_symlink():
                    staging.unlink()
            except OSError:
                pass
            return False

    def _cleanup_timeout_leases(self) -> bool:
        leases = getattr(self, "timeout_leases", None)
        if not isinstance(leases, dict):
            return True
        failures: list[BaseException] = []
        for lease in leases.values():
            if not isinstance(lease, TimeoutFifoLease):
                failures.append(AcceptanceFailure("timeout liveness lease is malformed"))
                continue
            lease.close()
            try:
                try:
                    lease.path.lstat()
                except FileNotFoundError:
                    continue
                lease._verify_path()
                lease.path.unlink()
            except BaseException as error:
                failures.append(error)

        for path in (getattr(self, "timeout_marker_file", None),
                     getattr(self, "timeout_resume_file", None)):
            if not isinstance(path, Path):
                continue
            try:
                metadata = path.lstat()
            except FileNotFoundError:
                continue
            except OSError as error:
                failures.append(error)
                continue
            if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISREG(metadata.st_mode):
                failures.append(AcceptanceFailure("timeout artifact has an unsafe type"))
                continue
            try:
                path.unlink()
            except OSError as error:
                failures.append(error)

        directory = getattr(self, "timeout_lease_directory", None)
        if isinstance(directory, Path):
            try:
                metadata = directory.lstat()
            except FileNotFoundError:
                metadata = None
            except OSError as error:
                failures.append(error)
                metadata = None
            if metadata is not None:
                if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISDIR(metadata.st_mode):
                    failures.append(AcceptanceFailure("timeout liveness lease directory is unsafe"))
                elif any(directory.iterdir()):
                    failures.append(AcceptanceFailure(
                        "timeout liveness lease directory retained unexpected entries"))
        return not failures

    def finalize(self, success: bool) -> bool:
        """Complete natural cleanup, then remove only this test's private root."""
        failure_diagnostics_written = False
        if not success:
            # Diagnostics are independent evidence.  Persist them even when
            # supervisor shutdown cannot complete; cleanup must still run.
            failure_diagnostics_written = self._write_failure_diagnostics()
        if not self.closed:
            try:
                self.safe_shutdown()
            except BaseException:
                pass
        leases_clean = True
        if self.closed:
            leases_clean = self._cleanup_timeout_leases()
        artifacts_clean = finalize_acceptance_artifacts(
            self.output,
            self.closed,
            self.owned_directories,
            success,
            remove_failed_process_captures=failure_diagnostics_written,
            raw_process_captures_token=self.processes.capture_root,
        )
        return leases_clean and artifacts_clean

    def publish_success(self) -> None:
        receipt = getattr(self, "success_receipt", None)
        require(isinstance(receipt, dict), "acceptance success receipt is unavailable")
        write_json(self.output / "success.json", receipt)

    def direct(self, name: str, argv: list[object], expected=0, timeout=30):
        return self.ops.direct(name, argv, expected=expected, timeout=timeout)

    def _sync_ops_owned_tasks(self) -> None:
        """Copy only complete, positively admitted task identities for cleanup."""
        ops_root = self.ops.root_id
        valid_root = (isinstance(ops_root, str) and len(ops_root) == 32 and
                      all(character in "0123456789abcdef" for character in ops_root))
        if valid_root:
            if self.root_id is None:
                self.root_id = ops_root
            else:
                require(self.root_id == ops_root, "dispatch changed root identity")
        for name, task_id in self.ops.tasks.items():
            numeric_id = self.ops.numbers.get(name)
            label = self.ops.labels.get(name)
            if not (valid_root and isinstance(name, str) and isinstance(task_id, str) and
                    task_id in self.ops.dispatch_attempts and
                    label == f"delegate:{ops_root}:{task_id}" and
                    type(numeric_id) is int and numeric_id >= 0):
                continue
            existing_task = self.tasks.get(name)
            if existing_task is not None:
                require(existing_task == task_id, f"cleanup task identity changed for {name}")
            existing_numeric = self.numeric_ids.get(task_id)
            if existing_numeric is not None:
                require(existing_numeric == numeric_id,
                        f"cleanup supervisor identity changed for {task_id}")
            self.tasks[name] = task_id
            self.numeric_ids[task_id] = numeric_id

    def _sync_model_cleanup(self, ops: ModelDiscoveryCleanupOps) -> None:
        """Bind cleanup to the model inspection only after its identity exists."""
        root_path = self.models_state / "root.json"
        if not root_path.is_file() or root_path.is_symlink():
            return
        root = read_json(root_path)
        root_id = root.get("root_id")
        require(_task_identity(root_id), "model discovery root identity is invalid")
        self.models_root_id = root_id
        ops.root_id = root_id

        inspections = self.models_state / "inspections"
        if not inspections.is_dir() or inspections.is_symlink():
            return
        entries = list(inspections.iterdir())
        if not entries:
            return
        require(len(entries) == 1 and entries[0].is_dir() and not entries[0].is_symlink() and
                _task_identity(entries[0].name),
                "model discovery inspection journal coverage is not exact")
        task_id = entries[0].name
        if self.model_discovery_task_id is None:
            self.model_discovery_task_id = task_id
        require(self.model_discovery_task_id == task_id,
                "model discovery task identity changed")
        ops.dispatch_attempts = {task_id}
        ops.inspection_attempts = {task_id}

    def write_binding(self, task_id: str, provider_identity: dict[str, str]) -> None:
        binding = self.output / "binding.json"
        require(not binding.exists() and not binding.is_symlink(),
                "runtime binding was already persisted")
        require(_task_identity(task_id) and
                set(provider_identity) == {"executable", "version", "sha256"} and
                provider_identity.get("executable") == str(self.provider_executable) and
                provider_identity.get("version") == self.provider_version and
                _hex_digest(provider_identity.get("sha256")),
                "first admitted provider identity is unavailable")
        args = getattr(self, "args", None)
        write_json(binding, {
            "provider": self.provider,
            "mode": self.mode,
            "scenario": self.scenario,
            "task_budget": TASK_BUDGET,
            "canonical_task_budget": CANONICAL_TASK_BUDGET,
            "requested_model": getattr(args, "model", None),
            "requested_effort": getattr(args, "effort", None),
            "model_discovery": getattr(self, "model_discovery_summary", None),
            "native_mode": self.native_mode_receipt(),
            "provider_identity_scope": "first-admitted-task",
            "provider_task_id": task_id,
            "provider_executable": provider_identity["executable"],
            "provider_sha256": provider_identity["sha256"],
            "provider_version": provider_identity["version"],
            "delegate_sha256": digest(self.delegate),
            "runner_sha256": digest(self.runner),
            "pueue_sha256": digest(self.pueue),
            "pueued_sha256": digest(self.pueued),
            "pueue_version": self.pueue_version,
            "pueued_version": self.pueued_version,
            "credentials_in_receipt": False,
        })

    def _record_provider_runtime_identity(
            self, task_id: str, meta: dict[str, object],
            launch_evidence: tuple[dict[str, object], dict[str, object]] | None = None
            ) -> dict[str, str]:
        """Record one task's static or terminal provider runtime identity."""
        require(_task_identity(task_id), f"task {task_id} has an invalid identity")
        identity = validate_provider_runtime_metadata(
            meta, launch_evidence=launch_evidence,
            label=f"task {task_id} provider runtime metadata")
        if launch_evidence is None:
            require("runtime_preflight_sha256" not in meta,
                    f"task {task_id} pending provider runtime has no launch evidence")
            executable = meta.get("provider_executable")
            version = meta.get("provider_version")
            effective = meta.get("effective_config")
            policy = effective.get("policy") if isinstance(effective, dict) else None
            runtime_sha256 = policy.get("runtime_sha256") if isinstance(policy, dict) else None
            require(isinstance(executable, str) and isinstance(version, str) and
                    executable == str(self.provider_executable) and
                    _hex_digest(runtime_sha256),
                    f"task {task_id} has no admitted provider runtime identity")
            identity = {
                "executable": executable,
                "version": version,
                "sha256": runtime_sha256,
            }
        require(isinstance(identity, dict),
                f"task {task_id} provider runtime identity is unavailable")
        existing = self.provider_runtime_identities.get(task_id)
        require(existing is None or existing == identity,
                f"task {task_id} changed its admitted provider runtime identity")
        self.provider_runtime_identities[task_id] = identity
        return identity

    def provider_runtime_receipt(self) -> dict[str, object]:
        identities = {
            task_id: dict(identity)
            for task_id, identity in sorted(self.provider_runtime_identities.items())
        }
        expected_tasks = set(self.tasks.values())
        require(bool(expected_tasks) and set(identities) == expected_tasks,
                "provider runtime identity coverage is incomplete")
        return {
            "provider_versions": {
                task_id: identity["version"] for task_id, identity in identities.items()
            },
            "provider_runtime_identities": identities,
        }

    def native_mode_receipt(self) -> dict[str, object] | None:
        if self.provider != "antigravity:print":
            return None
        if self.mode == "read-only":
            return {"sandbox": "--sandbox", "mode": "plan", "bypass": False}
        return {"sandbox": "--sandbox", "mode": "accept-edits", "bypass": True}

    def _prepare_codex_workspace(self) -> None:
        if self.provider not in {"codex:exec", "codex:review"}:
            return
        require(self.git is not None, "Codex git prerequisite is unavailable")
        process = self.processes.run(
            "codex-git-init",
            [self.git, "init", "--quiet", "--initial-branch=main", self.workspace],
            self.workspace.parent,
            timeout=20,
        )
        require((self.workspace / ".git").is_dir() and not (self.workspace / ".git").is_symlink() and
                process.result.get("exit_code") == 0,
                "Codex disposable workspace is not a git repository")
        if self.provider == "codex:exec" and self.scenario == "timeout-resume":
            for name in CODEX_TIMEOUT_BASELINE_DIRECTORIES:
                (self.workspace / name).mkdir(mode=0o700)
        if self.provider == "codex:review":
            for key, value in (("user.email", "delegation-acceptance@example.invalid"),
                               ("user.name", "Delegation Acceptance")):
                configured = self.processes.run(
                    "codex-review-git-config-" + key.replace(".", "-"),
                    [self.git, "-C", self.workspace, "config", key, value],
                    self.workspace.parent, timeout=20)
                require(configured.result.get("exit_code") == 0,
                        "Codex review workspace Git identity could not be configured")
            staged = self.processes.run(
                "codex-review-git-add",
                [self.git, "-C", self.workspace, "add", "acceptance-marker.txt"],
                self.workspace.parent, timeout=20)
            require(staged.result.get("exit_code") == 0,
                    "Codex review workspace target could not be staged")
            committed = self.processes.run(
                "codex-review-git-commit",
                [self.git, "-C", self.workspace, "commit", "--quiet", "-m", "baseline"],
                self.workspace.parent, timeout=20)
            require(committed.result.get("exit_code") == 0,
                    "Codex review workspace baseline could not be committed")
            require(self.workspace_marker.is_file() and not self.workspace_marker.is_symlink(),
                    "Codex review marker is not a regular file")
            with self.workspace_marker.open("wb") as stream:
                stream.write(self.marker_bytes + b"review-change\n")
                stream.flush()
                os.fsync(stream.fileno())

    def discover_models(self) -> None:
        """Record advisory model facts through the already-running supervisor."""
        before = workspace_snapshot(self.workspace)
        argv = self.model_discovery_argv()
        process = self.processes.run(
            "model-discovery", argv, self.base,
            expected={0, 1, 2}, timeout=MODEL_DISCOVERY_TIMEOUT_SECONDS)
        response = parse_json_output(process, "model discovery")
        summary = validate_model_discovery_response(response, self.provider)
        expected_exit = model_discovery_exit_code(str(response["status"]))
        require(process.result.get("exit_code") == expected_exit,
                "model discovery exit status does not match its status")
        self.model_discovery = response
        self.model_discovery_summary = summary
        self.model_discovery_exit_code = expected_exit
        root_path = self.models_state / "root.json"
        if root_path.is_file() and not root_path.is_symlink():
            root = read_json(root_path)
            root_id = root.get("root_id") if isinstance(root, dict) else None
            require(isinstance(root_id, str) and len(root_id) == 32 and
                    all(character in "0123456789abcdef" for character in root_id),
                    "model discovery root identity is invalid")
            self.models_root_id = root_id
        write_json(self.output / "models.json", response)
        require(workspace_snapshot(self.workspace) == before,
                "model discovery changed the acceptance workspace")

    def model_discovery_argv(self) -> list[object]:
        return [
            self.delegate, "--root", self.models_state,
            "--pueue-config", self.config, "--runner", self.runner,
            "models", "--provider", self.provider, "--cwd", self.workspace, "--json",
        ]

    def _validate_model_discovery_evidence(self, status: dict[str, object],
                                           model_group: str) -> None:
        """Validate the standalone inspection before excluding its queue row."""
        require(self.models_root_id is not None, "model discovery root identity is unavailable")
        require(self.root_id is None and not self.tasks and not self.numeric_ids and
                not self.ops.dispatch_attempts and not self.ops.inspection_attempts,
                "model discovery admitted an ordinary provider task")
        require(not (self.state / "tasks").exists() and
                not (self.state / "inspections").exists(),
                "model discovery created ordinary provider state")
        model_tasks = self.models_state / "tasks"
        require(not model_tasks.exists() and not model_tasks.is_symlink(),
                "model discovery created an ordinary task record")

        inspections = self.models_state / "inspections"
        require(inspections.is_dir() and not inspections.is_symlink(),
                "model discovery inspection journal is absent")
        entries = list(inspections.iterdir())
        require(len(entries) == 1 and entries[0].is_dir() and not entries[0].is_symlink() and
                _task_identity(entries[0].name),
                "model discovery inspection journal coverage is not exact")
        task_id = entries[0].name
        if self.model_discovery_task_id is None:
            self.model_discovery_task_id = task_id
        require(self.model_discovery_task_id == task_id,
                "model discovery task identity changed")
        validator = NativeTaskOps(self.processes, self.delegate, self.runner, self.pueue,
                                  self.models_state.parent, self.models_state, self.output)
        validator.bind_supervisor(self.config, self.daemon)
        validator.root_id = self.models_root_id
        validator.dispatch_attempts = {task_id}
        validator.inspection_attempts = {task_id}
        journals = validator.validate_inspection_journals(
            allow_failure=True, expected_binding_required=False)
        require(set(journals) == {task_id}, "model discovery journal coverage changed")
        journal = journals[task_id]
        request = journal["request"]
        binding = request["binding"]
        helper_executable = binding.get("helper_executable")
        require(binding.get("definition_revision") == "model-discovery-v1" and
                helper_executable == str(self.provider_executable) and
                _hex_digest(binding.get("helper_sha256")),
                "model discovery provider binding changed")
        task = request["task"]
        discovery_mode = PROFILES[self.provider]["modes"][0]
        require(task.get("provider") == self.provider and
                task.get("mode") == discovery_mode and
                task.get("canonical_cwd") == str(self.workspace) and
                task.get("requested_config") == {"permission": discovery_mode, "budget": MODEL_DISCOVERY_TASK_BUDGET} and
                task.get("budget_nanos") == MODEL_DISCOVERY_TASK_BUDGET_NANOS and
                task.get("brief_length") == len(b"model discovery") and
                task.get("brief_sha256") == hashlib.sha256(b"model discovery").hexdigest(),
                "model discovery request is not the bounded metadata operation")
        result = journal["result"]
        require(isinstance(result, dict), "model discovery has no completed result")
        if result.get("reason") == "eligible":
            require(isinstance(self.model_discovery, dict), "model discovery response is absent")
            expected = {key: self.model_discovery.get(key)
                        for key in ("status", "reason_code", "complete", "source", "models", "efforts")}
            require(result.get("facts") == expected, "model discovery facts changed")
        groups, rows = status.get("groups"), status.get("tasks")
        validator._validate_group_snapshot(groups, {"default", model_group})
        require(isinstance(rows, dict) and len(rows) == 1,
                "model discovery queue membership is not exact")
        row_key, row = next(iter(rows.items()))
        require(isinstance(row, dict) and row_key == str(journal["numeric_task_id"]),
                "model discovery queue row is malformed")
        validator._validate_inspection_queue_row(row, journal, allow_failure=True)

    def setup(self) -> None:
        self.require_authentication_prerequisite()
        pueue_version = self.direct("pueue-version", pueue_command(self.pueue, self.config, "--version"), timeout=20)
        pueued_version = self.direct("pueued-version", [self.pueued, "-c", self.config, "--version"], timeout=20)
        self.pueue_version = observed_pueue_version(
            bounded_text(pueue_version.directory / "stdout", "pueue version output", MAX_CONTROL_BYTES), "pueue")
        self.pueued_version = observed_pueue_version(
            bounded_text(pueued_version.directory / "stdout", "pueued version output", MAX_CONTROL_BYTES), "pueued")
        discovery = parse_json_output(
            self.direct("discovery", [self.delegate, "providers", "--json"], timeout=30), "provider discovery")
        providers = discovery.get("providers")
        require(isinstance(providers, list), "compiled discovery has no provider list")
        selected = next((item for item in providers
                         if isinstance(item, dict) and item.get("id") == self.provider), None)
        require(isinstance(selected, dict), "selected provider is absent from compiled discovery")
        advertised = selected.get("supported_modes")
        require(isinstance(advertised, list) and self.mode in advertised,
                f"compiled discovery does not advertise {self.provider} {self.mode}")
        self._prepare_codex_workspace()
        self.daemon = self.processes.start("pueued", [self.pueued, "-c", self.config], self.base,
                                           on_spawned=self._adopt_daemon)
        self.ops.bind_supervisor(self.config, self.daemon)
        deadline = time.monotonic() + 30
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            process = self.ops.client("daemon-status", ["status", "--json"], expected={0, 1},
                                      timeout=min(20, remaining))
            if process.result.get("exit_code") == 0:
                status = status_jobs(process)
                if not status.get("tasks"):
                    self.workspace_baseline = workspace_snapshot(self.workspace)
                    return
            delay = min(0.2, max(0.0, deadline - time.monotonic()))
            if delay <= 0:
                break
            time.sleep(delay)
        raise AcceptanceFailure("private pueue readiness observation expired")

    def require_authentication_prerequisite(self) -> None:
        """Turn an explicit native signed-out status into a neutral live gate."""
        arguments = self.profile.get("authentication_status")
        if arguments is None:
            return
        require(isinstance(arguments, tuple) and arguments and
                all(isinstance(value, str) and value for value in arguments),
                f"{self.provider} authentication status command is invalid")
        process = self.direct(
            "provider-authentication-status", [self.provider_executable, *arguments],
            expected={0, 1}, timeout=20)
        response = parse_json_output(process, f"{self.provider} authentication status")
        logged_in = response.get("loggedIn")
        require(type(logged_in) is bool,
                f"{self.provider} authentication status has no loggedIn boolean")
        if not logged_in:
            raise BlockedFailure(f"{self.provider} authentication is unavailable")
        require(process.result.get("exit_code") == 0,
                f"{self.provider} authenticated status exited unsuccessfully")

    def _adopt_daemon(self, process: object) -> None:
        require(self.daemon is None, "private daemon handle was assigned twice")
        self.daemon = process

    def brief(self, name: str, continuation: bool = False) -> Path:
        path = self.briefs / f"{name}.txt"
        if self.mode == "workspace-write":
            if continuation:
                text = (
                    "Continue the exact previous conversation and complete this workspace integration test. "
                    "Use native file and shell tools in the current workspace. Verify the existing target, "
                    "then append exactly `continued` as a second line to acceptance-write-target.txt. "
                    f"Execute `{shell_digest_command('acceptance-write-continue-check.txt')}` "
                    "after the edit. Reply exactly `DELEGATION_LIVE_WRITE_CONTINUED` after completing the test. "
                    "Do not modify any other file, implement source code, change configuration, "
                    "use network, access paths outside the workspace, or claim results from prose.\n"
                )
            else:
                text = (
                    "Perform this workspace write integration test with native file and shell tools. This is an "
                    "integration test, not a request to implement code. Read acceptance-write-nonce.txt and edit "
                    "the pre-existing acceptance-write-fixture.txt so it contains exactly "
                    "`edited-fixture:<nonce>`. Create "
                    "acceptance-write-target.txt containing exactly `created:<nonce>`, edit that same file so it "
                    "contains exactly `edited:<nonce>`, then execute "
                    f"`{shell_digest_command('acceptance-write-check.txt')}` after the edit. "
                    "Reply exactly `DELEGATION_LIVE_WRITE_OK` after completing the test. "
                    "Do not modify any other file, implement source "
                    "code, change configuration, use network, access paths outside the current workspace, or claim "
                    "results from prose.\n"
                )
        else:
            if getattr(self, "provider", None) == "codex:review":
                text = (
                    "Review the current uncommitted changes in this Git workspace. Return a concise review "
                    "of the change and do not modify files, use network, or access paths outside the workspace.\n"
                )
                write_bytes(path, text.encode())
                return path
            prefix = "DELEGATION_LIVE_CONTINUED:" if continuation else "DELEGATION_LIVE_OK:"
            text = "Continue the exact previous conversation. " if continuation else ""
            provider = getattr(self, "provider", None)
            read_instruction = ("using the provider's file-read tool only"
                                if provider == "antigravity:print"
                                else "using a file-read tool or a read-only shell command")
            no_command = (" Do not use a command or shell tool."
                          if provider == "antigravity:print" else "")
            text += (f"Read the actual file `{self.workspace_marker}` {read_instruction}.{no_command} "
                     "Do not infer its contents from examples or other repositories. "
                     f"Reply with exactly the literal prefix `{prefix}` followed by one space and the exact "
                     "contents read from that file, with no other text. "
                     "Do not modify files, use network, or access paths outside the current workspace.\n")
        write_bytes(path, text.encode())
        return path

    def interactive_brief(self, name: str) -> Path:
        path = self.briefs / f"{name}.txt"
        text = (
            "You MUST call the root AskUserQuestion tool before producing any final answer. "
            "Ask exactly one question with the text `Choose a color` and exactly two options, "
            "`Blue` and `Green`; make it single-select, then wait for my answer. "
            "After I answer Blue, return exactly `DELEGATION_LIVE_INTERACTIVE_OK: Blue` as your final answer. "
            "Do not modify files, use shell, use network, or access paths outside the current workspace.\n"
        )
        write_bytes(path, text.encode())
        return path

    def timeout_brief(self, name: str, resume: bool = False) -> Path:
        path = self.briefs / f"{name}.txt"
        if resume:
            text = (
                "Continue the exact previous Codex session. The prior turn intentionally timed out after "
                "creating its marker. Verify acceptance-timeout-marker.txt, then create "
                "acceptance-timeout-resumed.txt containing exactly "
                f"`{self.timeout_resume_marker}` followed by one newline. Reply exactly "
                "`DELEGATION_TIMEOUT_RESUMED` after the file is written. Do not modify any other file, "
                "configuration, or path outside the current workspace.\n"
            )
        else:
            command = (
                "set -eu; "
                f"exec 3>{shlex.quote(str(self.timeout_parent_fifo))}; "
                f"printf '%s\\n' {shlex.quote(self.timeout_parent_ready_marker)} >&3; "
                f"(exec 3>&-; exec 4>{shlex.quote(str(self.timeout_child_fifo))}; "
                f"printf '%s\\n' {shlex.quote(self.timeout_child_ready_marker)} >&4; "
                "exec sleep 600) & "
                f"printf '%s\\n' {shlex.quote(self.timeout_marker)} > "
                f"{shlex.quote(self.timeout_marker_file.name)}; wait")
            text = (
                "This is a timeout-recovery integration test. Use the native shell tool to run exactly this "
                f"command: `{command}`. The long sleep is intentional: do not stop it, do not reply before "
                "the supervisor budget ends the turn, and do not modify any other file, configuration, or "
                "path outside the current workspace.\n"
            )
        write_bytes(path, text.encode())
        return path

    def _observe_timeout_leases(self) -> None:
        leases = getattr(self, "timeout_leases", None)
        require(isinstance(leases, dict) and set(leases) == {"parent", "child"},
                "timeout liveness leases are unavailable")
        for lease in leases.values():
            require(isinstance(lease, TimeoutFifoLease),
                    "timeout liveness lease is malformed")
            lease.observe()

    def _timeout_liveness_ready(self) -> bool:
        self._observe_timeout_leases()
        for lease in self.timeout_leases.values():
            require(not lease.eof,
                    f"{lease.label} FIFO closed before the task was observed running")
        return all(lease.ready and not lease.eof for lease in self.timeout_leases.values())

    def _timeout_liveness_receipt(self, closed: bool) -> dict[str, object]:
        resources: dict[str, dict[str, object]] = {}
        for name in ("parent", "child"):
            lease = self.timeout_leases.get(name)
            require(isinstance(lease, TimeoutFifoLease) and
                    type(lease.device) is int and type(lease.inode) is int,
                    f"timeout {name} FIFO identity is unavailable")
            resources[name] = {
                "path": str(lease.path),
                "device": lease.device,
                "inode": lease.inode,
                "type": "fifo",
                "ready": lease.ready,
                "closed": lease.eof,
            }
        return {
            "schema_version": 1,
            "resources": resources,
            "ready": all(resource["ready"] is True for resource in resources.values()),
            "closed": closed,
        }

    def _capture_timeout_liveness(self) -> bool:
        if not self._timeout_liveness_ready():
            return False
        receipt = self._timeout_liveness_receipt(closed=False)
        self.timeout_liveness = receipt
        write_json(self.output / "timeout-liveness.json", receipt)
        return True

    def record_timeout_liveness(
            self, task_id: str, timeout: float = TIMEOUT_LEASE_SETUP_SECONDS,
            sleep_fn=time.sleep, monotonic_fn=time.monotonic) -> None:
        require(self.root_id is not None, "timeout source has no root identity")
        require(timeout > 0, "timeout liveness setup deadline must be positive")
        self.ops.root_id = self.root_id
        deadline = monotonic_fn() + timeout
        next_status = monotonic_fn()
        attempt = 0
        last_status: dict[str, object] | None = None
        while True:
            remaining = deadline - monotonic_fn()
            if remaining <= 0:
                break
            if monotonic_fn() >= next_status:
                last_status = self.ops.status(
                    f"timeout-liveness-status-{attempt}", task_id,
                    expected={0, 1}, timeout=min(45, remaining))
                attempt += 1
                remaining = deadline - monotonic_fn()
                if remaining <= 0:
                    break
                failure = last_status.get("failure")
                terminal_failure = (isinstance(failure, dict) and
                                    failure.get("next_action") == "stop_and_report")
                if (last_status.get("liveness") == "ended" or
                        last_status.get("publication") in {"committed", "rejected"} or
                        last_status.get("status") == "timed_out" or
                        terminal_failure):
                    raise AcceptanceFailure(
                        "timeout source ended before both liveness leases were ready; Delegate " +
                        _delegate_status_summary(last_status))
                if last_status.get("liveness") == "running" and self._capture_timeout_liveness():
                    return
                next_status = monotonic_fn() + TIMEOUT_LEASE_STATUS_INTERVAL_SECONDS
                remaining = deadline - monotonic_fn()
                if remaining <= 0:
                    break
            sleep_fn(min(0.25, remaining))
        detail = (_delegate_status_summary(last_status) if last_status is not None
                  else "status=unavailable")
        raise AcceptanceFailure(
            "timeout source liveness leases were not ready before the bounded deadline; Delegate " +
            detail)

    def _timeout_liveness_closed(self) -> bool:
        self._observe_timeout_leases()
        return all(lease.ready and lease.eof for lease in self.timeout_leases.values())

    def wait_timeout_leases_closed(self) -> None:
        require(isinstance(getattr(self, "timeout_liveness", None), dict),
                "timeout liveness readiness was not recorded")
        wait_until(self._timeout_liveness_closed,
                   timeout=TIMEOUT_LEASE_OBSERVATION_SECONDS,
                   description="both timeout liveness FIFO leases to close")
        receipt = self._timeout_liveness_receipt(closed=True)
        receipt["closed_observed_ns"] = time.time_ns()
        write_json(self.output / "timeout-liveness.json", receipt, replace=True)
        self.timeout_liveness = receipt

    def task_command_prefix(self) -> list[object]:
        argv: list[object] = [
            self.delegate, "--root", self.state, "--pueue-config", self.config,
        ]
        if getattr(self, "scenario", None) != "timeout-resume":
            argv.extend(["--runner", self.runner])
        return argv

    def dispatch_argv(self, task_id: str, brief: Path,
                      predecessor: str | None = None,
                      budget: str = TASK_BUDGET) -> list[object]:
        argv = self.task_command_prefix()
        argv.extend(["dispatch", "--provider", self.provider,
                     "--brief", brief, "--cwd", self.workspace, "--id", task_id,
                     "--permission", self.mode, "--budget", budget, "--json"])
        model = getattr(self.args, "model", None)
        effort = getattr(self.args, "effort", None)
        if model:
            argv.extend(["--model", model])
        if effort:
            argv.extend(["--effort", effort])
        if getattr(self, "scenario", None) == "interactive":
            argv.append("--interactive")
        if self.provider == "codex:review":
            argv.extend(["--review-target", "uncommitted"])
        if predecessor:
            argv.extend(["--resume-task", predecessor])
        return argv

    def dispatch(self, name: str, brief: Path, predecessor: str | None = None,
                 budget: str = TASK_BUDGET) -> str:
        task_id = secrets.token_hex(16)
        argv = self.dispatch_argv(task_id, brief, predecessor, budget)
        self.ops.root_id = self.root_id
        try:
            response = self.ops.dispatch(name, task_id, argv)
            root_id, numeric_id = dispatch_binding(response, task_id, name)
            if self.root_id is None:
                self.root_id = root_id
            require(root_id == self.root_id, "dispatch changed root identity")
            self.ops.root_id = self.root_id
            self.numeric_ids[task_id] = numeric_id
            self.tasks[name] = task_id
            return task_id
        except AcceptanceFailure:
            self._sync_ops_owned_tasks()
            observation = self.ops.dispatch_observations[-1] if self.ops.dispatch_observations else {}
            refused = observation.get("response") if isinstance(observation, dict) else None
            message = str(refused.get("error", "")) if isinstance(refused, dict) else ""
            if authentication_blocked(refused, message):
                raise BlockedFailure(f"{self.provider} authentication is unavailable")
            raise

    def continue_task(self, name: str, brief: Path, predecessor: str) -> str:
        return self.dispatch(name, brief, predecessor=predecessor)

    def _interaction_events(self, name: str, task_id: str, after: int,
                            watch_seconds: int) -> dict[str, object]:
        argv: list[object] = [
            self.delegate, "--root", self.state, "--pueue-config", self.config,
            "--runner", self.runner, "events", task_id, "--after", str(after),
            "--watch", f"{watch_seconds}s", "--json",
        ]
        process = self.direct(name, argv, expected={0, 3},
                              timeout=watch_seconds + 20)
        response = parse_json_output(process, name)
        require(response.get("task_id") == task_id,
                f"{name} observed the wrong interactive task")
        state = response.get("interaction")
        require(isinstance(state, dict), f"{name} returned no interaction state")
        events = response.get("events", [])
        require(isinstance(events, list) and all(isinstance(event, dict) for event in events),
                f"{name} returned malformed interaction events")
        cursor = response.get("cursor")
        require(type(cursor) is int and cursor >= after,
                f"{name} returned an invalid interaction cursor")
        write_json(self.output / (name + ".json"), response)
        return response

    def wait_for_interaction(self, task_id: str, after: int = 0,
                             pending: bool = False) -> tuple[dict[str, object], int, list[dict[str, object]]]:
        deadline = time.monotonic() + WATCH_SECONDS
        observed: list[dict[str, object]] = []
        cursor = after
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise AcceptanceFailure("interactive event observation expired")
            self.interaction_observation_number += 1
            response = self._interaction_events(
                f"interactive-events-{self.interaction_observation_number}", task_id, cursor,
                max(1, min(5, int(remaining))))
            state = response["interaction"]
            events = response.get("events", [])
            require(isinstance(state, dict) and isinstance(events, list),
                    "interactive event response changed shape")
            observed.extend(event for event in events if isinstance(event, dict))
            next_cursor = response.get("cursor")
            require(type(next_cursor) is int and next_cursor >= cursor,
                    "interactive event cursor moved backwards")
            cursor = next_cursor
            requests = state.get("pending_requests")
            require(isinstance(requests, list), "interactive pending request state is malformed")
            if pending and requests:
                return state, cursor, observed
            if not pending and state.get("closed") is True:
                return state, cursor, observed

    @staticmethod
    def interactive_request(state: dict[str, object]) -> tuple[str, str]:
        requests = state.get("pending_requests")
        require(isinstance(requests, list) and len(requests) == 1,
                "interactive acceptance did not expose one pending question")
        request = requests[0]
        require(isinstance(request, dict), "interactive pending question is malformed")
        request_id = request.get("id")
        turn_id = request.get("turn_id")
        require(_task_identity(request_id) and isinstance(turn_id, str) and bool(turn_id) and
                request.get("blocking") is True,
                "interactive pending question identity is invalid")
        questions = request.get("questions")
        require(isinstance(questions, list) and len(questions) == 1,
                "interactive acceptance did not expose one question")
        question = questions[0]
        require(isinstance(question, dict) and question.get("id") == "1" and
                question.get("text") == "Choose a color" and
                question.get("options") == ["Blue", "Green"] and
                question.get("multiple") is False,
                "interactive question contract changed")
        return request_id, turn_id

    def reply_interaction(self, task_id: str, request_id: str, turn_id: str) -> tuple[str, dict[str, object]]:
        message_id = secrets.token_hex(16)
        answers = self.briefs / "interactive-answer.json"
        write_json(answers, {"1": ["Blue"]})
        argv: list[object] = [
            self.delegate, "--root", self.state, "--pueue-config", self.config,
            "--runner", self.runner, "reply", task_id, "--request", request_id,
            "--answers", answers, "--id", message_id, "--json",
        ]
        response = parse_json_output(
            self.direct("interactive-reply", argv, expected=0, timeout=45),
            "interactive-reply")
        require(response.get("task_id") == task_id,
                "interactive reply targeted the wrong task")
        delivery = response.get("delivery")
        require(isinstance(delivery, dict) and delivery.get("message_id") == message_id and
                delivery.get("state") == "queued",
                "interactive reply was not durably queued")
        state = response.get("interaction")
        require(isinstance(state, dict) and state.get("turn_id") == turn_id,
                "interactive reply changed the interaction turn")
        write_json(self.output / "interactive-reply.json", response)
        return message_id, delivery

    def continue_timed_out_task(self, name: str, brief: Path, predecessor: str) -> str:
        argv = self.task_command_prefix()
        argv.extend(["continue", "--task", predecessor,
                     "--brief", brief, "--budget", TASK_BUDGET, "--json"])
        self.ops.root_id = self.root_id
        try:
            task_id, response = self.ops.continue_task(name, argv)
            root_id, numeric_id = dispatch_binding(response, task_id, name)
            require(response.get("command") == "continue" and
                    response.get("parent_task_id") == predecessor,
                    "continuation response did not retain its predecessor")
            require(self.root_id is not None and root_id == self.root_id,
                    "continuation changed root identity")
            self.numeric_ids[task_id] = numeric_id
            self.tasks[name] = task_id
            return task_id
        except AcceptanceFailure:
            self._sync_ops_owned_tasks()
            observation = self.ops.dispatch_observations[-1] if self.ops.dispatch_observations else {}
            refused = observation.get("response") if isinstance(observation, dict) else None
            message = str(refused.get("error", "")) if isinstance(refused, dict) else ""
            if authentication_blocked(refused, message):
                raise BlockedFailure(f"{self.provider} authentication is unavailable")
            raise

    def wait_done(self, name: str, task_id: str,
                  expected_result: str | frozenset[str] | None = None) -> dict[str, object]:
        require(self.root_id is not None, "root identity is unavailable")
        self.ops.root_id = self.root_id
        try:
            return self.ops.wait_task(name, task_id, expected_result)
        except (AcceptanceFailure, RuntimeError) as error:
            directory = self.state / "tasks" / task_id
            status_detail = "status=unavailable"
            try:
                status = self.ops.status(f"{name}-runner-failure-status", task_id,
                                         expected={0, 1})
                status_detail = _delegate_status_summary(status)
            except (AcceptanceFailure, RuntimeError, OSError, ValueError) as status_error:
                status_detail = f"status=unavailable ({type(status_error).__name__})"
            if authentication_unavailable(directory):
                raise BlockedFailure(f"{self.provider} authentication is unavailable") from error
            raise AcceptanceFailure(
                f"{name} runner completion failed: {error}; Delegate {status_detail}") from error

    def wait_timed_out(self, task_id: str, timeout: float = WATCH_SECONDS,
                       sleep_fn=time.sleep, monotonic_fn=time.monotonic) -> dict[str, object]:
        require(self.root_id is not None, "timeout source has no root identity")
        require(timeout > 0, "timeout observation deadline must be positive")
        self.ops.root_id = self.root_id
        deadline = monotonic_fn() + timeout
        attempt = 0
        while True:
            remaining = deadline - monotonic_fn()
            if remaining <= 0:
                break
            response = self.ops.status(f"timeout-status-{attempt}", task_id,
                                       expected={0, 1}, timeout=min(45, remaining))
            attempt += 1
            if monotonic_fn() >= deadline:
                break
            if response.get("status") != "timed_out":
                delay = min(0.25, max(0.0, deadline - monotonic_fn()))
                if delay <= 0:
                    break
                sleep_fn(delay)
                continue
            continuation = response.get("continuation")
            require(isinstance(continuation, dict) and continuation.get("resumable") is True and
                    continuation.get("mode") == "native" and
                    continuation.get("predecessor_task_id") == task_id and
                    isinstance(continuation.get("continue_command"), str) and
                    "continue --task " + task_id in continuation["continue_command"],
                    "timed-out task is not resumable through Delegate")
            return response
        raise AcceptanceFailure("timed-out task never published resumable continuation evidence")

    def terminal_private_queue(self, status: dict[str, object]) -> bool:
        try:
            status = self._without_model_discovery_queue(status)
            if self.root_id is None:
                return self._empty_private_queue(status)
            return self._terminal_private_queue(status)
        except (AcceptanceFailure, KeyError, OSError, RuntimeError, TypeError,
                UnicodeError, ValueError, RecursionError):
            return False

    def _without_model_discovery_queue(self, status: dict[str, object]) -> dict[str, object]:
        """Exclude only the distinct model-inspection group from task shutdown."""
        root_id = getattr(self, "models_root_id", None)
        if not isinstance(root_id, str):
            return status
        rows = status.get("tasks")
        groups = status.get("groups")
        require(isinstance(rows, dict) and isinstance(groups, dict),
                "private queue status is malformed")
        model_group = INSPECTION_GROUP_PREFIX + root_id
        if model_group not in groups:
            return status
        retained_rows: dict[object, object] = {}
        for numeric_id, row in rows.items():
            require(isinstance(row, dict), "private queue contains a malformed row")
            if row.get("group") == model_group:
                label = row.get("label")
                require(isinstance(label, str) and label.startswith(model_group + "-"),
                        "model discovery queue label is invalid")
                require(done_result(row) is not None,
                        "model discovery queue row is not terminal")
                continue
            retained_rows[numeric_id] = row
        if getattr(self, "discover_models_only", False):
            self._validate_model_discovery_evidence(status, model_group)
        retained_groups = {name: group for name, group in groups.items()
                           if name != model_group}
        return {"tasks": retained_rows, "groups": retained_groups}

    @staticmethod
    def _empty_private_queue(status: dict[str, object]) -> bool:
        # This provider harness passes an explicit external supervisor config;
        # external supervisors retain their owner's parallelism policy.
        rows = status.get("tasks")
        groups = status.get("groups")
        if not isinstance(rows, dict) or not isinstance(groups, dict) or rows:
            return False
        if not groups:
            return True
        if set(groups) != {"default"}:
            return False
        default = groups["default"]
        return (isinstance(default, dict) and default.get("status") == "Running" and
                type(default.get("parallel_tasks")) is int and
                default.get("parallel_tasks") == 1)

    def _inspection_queue_records(self) -> dict[str, dict[str, object]]:
        """Return complete inspection journals owned by every admitted task."""
        require(self.root_id is not None, "root identity is unavailable")
        task_ids = set(self.tasks.values())
        validator = NativeTaskOps(
            self.processes, self.delegate, self.runner, self.pueue,
            self.state.parent, self.state, self.output)
        validator.bind_supervisor(self.config, self.daemon)
        validator.root_id = self.root_id
        validator.dispatch_attempts = set(task_ids)
        validator.inspection_attempts = set(task_ids)
        records = validator.validate_inspection_journals(
            allow_failure=False, expected_binding_required=False)
        require(set(records) == task_ids,
                "inspection journal coverage is incomplete for admitted tasks")
        for task_id, record in records.items():
            meta = read_json(self.state / "tasks" / task_id / "meta.json")
            identity = self._record_provider_runtime_identity(task_id, meta)
            request = record.get("request")
            require(isinstance(request, dict), f"inspection request is absent for {task_id}")
            binding = request.get("binding")
            require(isinstance(binding, dict) and
                    binding.get("helper_executable") == identity["executable"] and
                    binding.get("helper_sha256") == identity["sha256"],
                    f"inspection helper binding does not match admitted runtime for {task_id}")
        return records

    def _terminal_private_queue(self, status: dict[str, object]) -> bool:
        rows = status.get("tasks")
        groups = status.get("groups")
        require(isinstance(rows, dict) and isinstance(groups, dict),
                "private queue status is malformed")
        require(self.root_id is not None, "root identity is unavailable")
        inspection_records = self._inspection_queue_records()
        inspection_group = INSPECTION_GROUP_PREFIX + self.root_id
        expected_groups = {"default"} | ({inspection_group} if inspection_records else set())
        require(set(groups) == expected_groups, "private queue contains an unknown supervisor group")
        for name in expected_groups:
            group = groups[name]
            # The native provider harness uses an explicit external config.
            # Private bundled-supervisor parallelism is asserted by the private
            # CLI acceptance below.
            require(isinstance(group, dict) and group.get("status") == "Running" and
                    type(group.get("parallel_tasks")) is int and
                    group.get("parallel_tasks") == 1,
                    f"private queue group is not running at parallelism one: {name}")

        expected: dict[str, tuple[str, str]] = {}
        ordinary_names: dict[str, str] = {}
        for name, task_id in self.tasks.items():
            require(task_id in self.numeric_ids, f"ordinary task has no numeric supervisor identity: {task_id}")
            label = f"delegate:{self.root_id}:{task_id}"
            require(label not in expected, "ordinary task labels are not unique")
            require(task_id not in ordinary_names, "ordinary task identities are not unique")
            expected[label] = ("ordinary", task_id)
            ordinary_names[task_id] = name
        for task_id, record in inspection_records.items():
            label = record["label"]
            require(isinstance(label, str) and label not in expected,
                    "inspection task labels are not unique")
            expected[label] = ("inspection", task_id)
        require(len(rows) == len(expected), "private queue membership is not exact")

        seen: set[str] = set()
        for row in rows.values():
            require(isinstance(row, dict), "private queue contains a malformed row")
            label = row.get("label")
            require(isinstance(label, str) and label in expected and label not in seen,
                    "private queue labels are not exact")
            seen.add(label)
            kind, task_id = expected[label]
            if kind == "ordinary":
                self.ops.validate_ordinary_queue_row(
                    row, label, task_id, self.numeric_ids[task_id], "default")
                expected_result = self.ops.expected_results.get(ordinary_names[task_id], "Success")
            else:
                record = inspection_records[task_id]
                require(row.get("group") == record["group"] and
                        row.get("id") == record["numeric_task_id"],
                        f"inspection queue identity is invalid for {task_id}")
                expected_command = [str(self.runner), "--inspection", "--guardian", "--supervised",
                                    "--root", str(self.state), task_id]
                commands = []
                for key in ("original_command", "command"):
                    value = row.get(key)
                    require(isinstance(value, str), f"inspection queue row missing {key}")
                    commands.append(shlex.split(value))
                require(commands == [expected_command, expected_command],
                        f"inspection queue argv is invalid for {task_id}")
                expected_result = "Success"
            require(done_result(row) == expected_result,
                    f"private queue task result changed for {task_id}")
        return seen == set(expected)

    def collect(self, name: str, task_id: str) -> dict[str, object]:
        self.ops.root_id = self.root_id
        directory = self.state / "tasks" / task_id
        try:
            response = self.ops.collect(name, task_id, expected={0, 4})
        except (AcceptanceFailure, RuntimeError) as error:
            raise AcceptanceFailure(f"{name} collection failed: {error}") from error
        rejected = response.get("publication") == "rejected"
        try:
            outcome, published_payload = verify_collected_outcome(
                response, directory, "rejected" if rejected else "committed")
        except AcceptanceFailure:
            raise
        except (OSError, ValueError, TypeError, RecursionError, RuntimeError) as error:
            raise AcceptanceFailure(f"{name} collection evidence is malformed") from error

        try:
            provider_exit = read_json(directory / "provider.exit")
        except (OSError, ValueError, TypeError, RecursionError, RuntimeError) as error:
            raise AcceptanceFailure(f"{name} provider evidence is malformed") from error
        require(isinstance(provider_exit, dict), f"{name} provider evidence is malformed")
        require(provider_exit.get("root_id") == self.root_id and
                provider_exit.get("task_id") == task_id and
                provider_exit.get("invocation_state") == "started" and
                isinstance(provider_exit.get("error"), str) and
                type(provider_exit.get("exit_code")) is int and
                isinstance(provider_exit.get("raw_manifest"), list) and
                isinstance(provider_exit.get("manifest_sha256"), str) and
                len(provider_exit["manifest_sha256"]) == 64 and
                all(character in "0123456789abcdef"
                    for character in provider_exit["manifest_sha256"]) and
                outcome.get("evidence_sha256") == provider_exit.get("manifest_sha256"),
                f"{name} lacks sealed provider evidence")
        if not rejected:
            require(provider_exit.get("exit_code") == 0 and provider_exit.get("error") == "",
                    f"{name} lacks sealed provider evidence")
        collection_payload = response.get("payload")
        require(isinstance(collection_payload, dict) and
                collection_payload == outcome.get("payload") and
                response.get("evidence_sha256") == outcome.get("evidence_sha256"),
                f"{name} collection receipt descriptors are inconsistent")
        if authentication_unavailable(directory):
            raise BlockedFailure(f"{self.provider} authentication is unavailable")
        if rejected:
            raise AcceptanceFailure(
                f"{name} provider result rejected: {_rejection_diagnostic_summary(outcome)}")
        require(published_payload, f"{name} committed an empty response")
        if self.scenario == "interactive":
            verify_interactive_answer(published_payload, name)
        elif self.mode == "read-only":
            if self.provider == "codex:review":
                verify_review_answer(published_payload, name)
            else:
                verify_read_only_answer(
                    published_payload, marker_answer(self.marker_text, continuation="resume" in name), name)
        self.records[name] = {"task_id": task_id, "directory": directory,
                              "outcome": deepcopy(outcome),
                              "payload": deepcopy(collection_payload),
                              "snapshot": task_digest_snapshot(directory)}
        return response

    def collect_timed_out_source(self, task_id: str) -> dict[str, object]:
        self.ops.root_id = self.root_id
        directory = self.state / "tasks" / task_id
        response = self.ops.collect("timeout-source-collect", task_id, expected={3, 4})
        if response.get("publication") == "rejected":
            verify_collected_outcome(response, directory, "rejected")
        else:
            require(response.get("publication") != "committed" and
                    response.get("outcome") is None and
                    not (directory / "outcome.json").exists(),
                    "timed-out source fabricated a terminal outcome")
        require(not (directory / "result.txt").exists(),
                "timed-out source published answer evidence")
        return response

    def _request_binding(self, task_id: str, require_launch_evidence: bool = False,
                         budget: str = CANONICAL_TASK_BUDGET,
                         budget_nanos: int = TASK_BUDGET_NANOS) -> tuple[
            Path, dict[str, object], dict[str, object], str, str, Path]:
        directory = self.state / "tasks" / task_id
        require(directory.is_dir() and not directory.is_symlink(),
                f"task {task_id} directory is absent")
        record = read_json(directory / "task.json")
        require(record.get("root_id") == self.root_id and record.get("task_id") == task_id and
                record.get("provider") == self.provider and record.get("mode") == self.mode and
                record.get("canonical_cwd") == str(self.workspace),
                f"task {task_id} changed its immutable binding")
        requested = record.get("requested_config")
        brief = directory / "brief.md"
        require(isinstance(requested, dict) and requested.get("permission") == self.mode and
                requested.get("budget") == budget and
                record.get("budget_nanos") == budget_nanos and
                brief.is_file() and not brief.is_symlink() and
                record.get("brief_length") == brief.stat().st_size and
                record.get("brief_sha256") == digest(brief),
                f"task {task_id} request binding is invalid")
        if self.scenario == "interactive":
            require(requested.get("interactive") is True,
                    f"task {task_id} did not persist interactive mode")
        meta = read_json(directory / "meta.json")
        meta_digest = digest(directory / "meta.json")
        spec_digest = meta.get("spec_sha256")
        launch_evidence = None
        if require_launch_evidence:
            launch_evidence = (
                read_json(directory / "provider.started.json"),
                read_json(directory / "provider.exit"),
            )
        identity = self._record_provider_runtime_identity(task_id, meta, launch_evidence)
        if self.provider_version is None:
            self.provider_version = identity["version"]
            self.write_binding(task_id, identity)
        return directory, record, meta, meta_digest, spec_digest, brief

    def timeout_source_binding(self, task_id: str) -> Path:
        directory, record, _meta, _meta_digest, _spec_digest, _brief = self._request_binding(
            task_id, budget=CANONICAL_TIMEOUT_RESUME_SOURCE_BUDGET,
            budget_nanos=TIMEOUT_RESUME_SOURCE_BUDGET_NANOS)
        requested = record.get("requested_config")
        args = getattr(self, "args", None)
        requested_model = getattr(args, "model", None)
        requested_effort = getattr(args, "effort", None)
        require(isinstance(requested, dict) and requested.get("permission") == self.mode and
                ((requested_model and requested.get("model") == requested_model) or
                 (not requested_model and requested.get("model") in (None, ""))) and
                ((requested_effort and requested.get("effort") == requested_effort) or
                 (not requested_effort and requested.get("effort") in (None, ""))) and
                record.get("prior_session") is None,
                "timed-out source request binding changed")
        reference = read_json(directory / "provider.ref.json")
        require(reference.get("root_id") == self.root_id and reference.get("task_id") == task_id and
                reference.get("provider") == self.provider and
                isinstance(reference.get("conversation_id"), str) and bool(reference["conversation_id"]),
                "timed-out source has no resumable provider session")
        require(not (directory / "outcome.json").exists() and not (directory / "result.txt").exists(),
                "timed-out source published terminal artifacts")
        return directory

    def _timeout_workspace_snapshot(self, resumed: bool) -> dict[str, dict[str, object]]:
        require(self.workspace_baseline is not None,
                "timeout-recovery workspace baseline is unavailable")
        expected = set(self.workspace_baseline)
        expected.add(self.timeout_marker_file.name)
        if resumed:
            expected.add(self.timeout_resume_file.name)
        actual = workspace_snapshot(self.workspace)
        require(set(actual) == expected,
                "timeout-recovery workspace contains an unexpected file")
        for name, snapshot_record in self.workspace_baseline.items():
            require(actual.get(name) == snapshot_record,
                    "timeout-recovery changed a harness-controlled workspace file")
        require(self.timeout_marker_file.read_bytes() == (self.timeout_marker + "\n").encode(),
                "timeout source did not create its exact marker")
        if resumed:
            require(self.timeout_resume_file.read_bytes() == (self.timeout_resume_marker + "\n").encode(),
                    "resumed timeout task did not create its exact marker")
        return actual

    def _workspace_write_snapshot(self, continuation: bool) -> dict[str, dict[str, object]]:
        require(self.nonce is not None, "write acceptance nonce is unavailable")
        require(self.workspace_baseline is not None,
                "write acceptance workspace baseline is unavailable")
        self._validate_pi_runtime_roots()
        generated = {
            "acceptance-write-target.txt", "acceptance-write-check.txt",
            "acceptance-write-continue-check.txt",
        }
        require(generated.isdisjoint(self.workspace_baseline),
                "write acceptance generated file was present in the baseline")
        expected = set(self.workspace_baseline)
        expected.update({"acceptance-write-target.txt", "acceptance-write-check.txt"})
        if continuation:
            expected.add("acceptance-write-continue-check.txt")
        actual = workspace_snapshot(self.workspace)
        expected.update(name for name in actual if self._is_pi_runtime_entry(name))
        git_directory = self.workspace / ".git"
        for path in self.workspace.rglob("*"):
            require(not path.is_symlink(), "write acceptance workspace contains a symlink")
            if path.is_dir():
                relative = str(path.relative_to(self.workspace))
                require(self.provider == "codex:exec" and
                        (path == git_directory or git_directory in path.parents) or
                        self._is_pi_runtime_entry(relative),
                        "write acceptance workspace contains an unexpected directory")
        require(set(actual) == expected,
                "write acceptance workspace contains unexpected files")
        fixture_name = str(self.fixture_file.relative_to(self.workspace))
        for name, record in self.workspace_baseline.items():
            if name == fixture_name:
                continue
            require(actual.get(name) == record,
                    "write acceptance changed a harness-controlled workspace record")
        require(self.workspace_marker.read_bytes() == self.marker_bytes and
                self.nonce_file.read_bytes() == (self.nonce + "\n").encode(),
                "write acceptance changed harness-controlled files")
        verify_write_fixture(self.nonce, self.fixture_file)
        return actual

    def _is_pi_runtime_entry(self, relative: str) -> bool:
        return (self.provider == "pi:json" and self.mode == "workspace-write" and
                (relative == ".pi" or relative == ".pi/tasks" or relative.startswith(".pi/tasks/")))

    def _validate_pi_runtime_roots(self) -> None:
        if self.provider != "pi:json" or self.mode != "workspace-write":
            return
        for relative in (".pi", ".pi/tasks"):
            path = self.workspace / relative
            if path.is_symlink() or path.exists():
                require(path.is_dir() and not path.is_symlink(),
                        f"Pi runtime root is not a directory: {relative}")

    def task_binding(self, task_id: str, predecessor: str | None = None,
                     predecessor_session: str | None = None) -> None:
        directory, record, meta, meta_digest, spec_digest, _ = self._request_binding(
            task_id, require_launch_evidence=True)
        requested = record.get("requested_config")
        args = getattr(self, "args", None)
        requested_model = getattr(args, "model", None)
        requested_effort = getattr(args, "effort", None)
        require(isinstance(requested, dict) and
                ((requested_model and requested.get("model") == requested_model) or
                 (not requested_model and requested.get("model") in (None, ""))) and
                ((requested_effort and requested.get("effort") == requested_effort) or
                 (not requested_effort and requested.get("effort") in (None, ""))),
                f"task {task_id} model or effort selection changed")
        if self.provider == "codex:review":
            require(requested.get("review_target") == {"kind": "uncommitted"},
                    f"task {task_id} Codex review target is not persisted")
        identity = self.provider_runtime_identities.get(task_id)
        require(isinstance(identity, dict),
                f"task {task_id} admitted provider runtime identity is unavailable")
        pending_runtime = "runtime_preflight_sha256" in meta
        require(meta.get("root_id") == self.root_id and meta.get("task_id") == task_id and
                meta.get("containment") == self.mode and
                isinstance(spec_digest, str) and len(spec_digest) == 64 and
                all(character in "0123456789abcdef" for character in spec_digest),
                f"task {task_id} metadata binding is invalid")
        effective = meta.get("effective_config")
        policy = effective.get("policy") if isinstance(effective, dict) else None
        require(isinstance(effective, dict) and effective.get("containment") == self.mode and
                isinstance(effective.get("digest"), str) and
                isinstance(policy, dict) and policy.get("workspace") == str(self.workspace) and
                (_hex_digest(policy.get("runtime_sha256")) if pending_runtime
                 else policy.get("runtime_sha256") == identity["sha256"]),
                f"task {task_id} effective policy binding is invalid")
        if self.provider == "antigravity:print":
            expected_approval = "plan" if self.mode == "read-only" else "always-proceed"
            require(effective.get("approval") == expected_approval,
                    f"task {task_id} AGY native approval mode is invalid")
        predicate = meta.get("predicate")
        require(isinstance(predicate, dict) and predicate.get("adapter") == self.provider and
                predicate.get("mode") == self.mode and isinstance(predicate.get("version"), str) and
                bool(predicate["version"]) and isinstance(predicate.get("sha256"), str) and
                len(predicate["sha256"]) == 64 and
                all(character in "0123456789abcdef" for character in predicate["sha256"]),
                f"task {task_id} predicate binding is invalid")
        submit = read_json(directory / "submit.json")
        label = f"delegate:{self.root_id}:{task_id}"
        supervisor = submit.get("supervisor")
        require(submit.get("root_id") == self.root_id and submit.get("task_id") == task_id and
                submit.get("spec_sha256") == spec_digest and submit.get("meta_sha256") == meta_digest and
                submit.get("label") == label and isinstance(supervisor, dict) and
                supervisor.get("client_executable") == str(self.pueue) and
                supervisor.get("config_path") == str(self.config) and
                supervisor.get("observed_version") == self.pueue_version,
                f"task {task_id} supervisor submission binding is invalid")
        for name in ("provider.start", "provider.started.json"):
            evidence = read_json(directory / name)
            require(evidence.get("root_id") == self.root_id and evidence.get("task_id") == task_id and
                    evidence.get("spec_sha256") == spec_digest and
                    evidence.get("meta_sha256") == meta_digest,
                    f"task {task_id} {name} binding is invalid")
        provider_exit = read_json(directory / "provider.exit")
        require(provider_exit.get("root_id") == self.root_id and
                provider_exit.get("task_id") == task_id and
                provider_exit.get("spec_sha256") == spec_digest and
                provider_exit.get("meta_sha256") == meta_digest and
                provider_exit.get("invocation_state") == "started" and
                provider_exit.get("exit_code") == 0 and provider_exit.get("error") == "" and
                provider_exit.get("predicate") == predicate and
                isinstance(provider_exit.get("raw_manifest"), list) and
                isinstance(provider_exit.get("manifest_sha256"), str) and
                len(provider_exit["manifest_sha256"]) == 64 and
                all(character in "0123456789abcdef" for character in provider_exit["manifest_sha256"]),
                f"task {task_id} provider seal binding is invalid")
        reference = read_json(directory / "provider.ref.json")
        session = reference.get("conversation_id")
        require(reference.get("root_id") == self.root_id and reference.get("task_id") == task_id and
                reference.get("spec_sha256") == spec_digest and
                reference.get("meta_sha256") == meta_digest and reference.get("provider") == self.provider and
                isinstance(session, str) and bool(session),
                f"task {task_id} provider session binding is invalid")
        outcome = read_json(directory / "outcome.json")
        require(outcome.get("root_id") == self.root_id and outcome.get("task_id") == task_id and
                outcome.get("spec_sha256") == spec_digest and outcome.get("meta_sha256") == meta_digest and
                outcome.get("verdict") == "committed" and outcome.get("predicate") == predicate and
                outcome.get("evidence_sha256") == provider_exit.get("manifest_sha256"),
                f"task {task_id} outcome authority binding is invalid")
        prior = record.get("prior_session")
        if predecessor is None:
            require(prior is None, f"task {task_id} unexpectedly has a predecessor")
            return
        require(isinstance(prior, dict) and prior.get("provider") == self.provider and
                prior.get("predecessor_task_id") == predecessor and
                prior.get("conversation_id") == predecessor_session,
                f"task {task_id} lacks the exact predecessor session binding")

    def session_ref(self, task_id: str) -> str:
        reference = read_json(self.state / "tasks" / task_id / "provider.ref.json")
        require(reference.get("root_id") == self.root_id and reference.get("task_id") == task_id and
                reference.get("provider") == self.provider,
                f"task {task_id} provider reference identity changed")
        session = reference.get("conversation_id")
        require(isinstance(session, str) and bool(session),
                f"task {task_id} has no provider session identity")
        return session

    def replay(self, records: dict[str, dict[str, object]] | None = None,
               names: tuple[str, ...] = ("fresh", "resume")) -> None:
        if records is None:
            records = {}
            for name, record_name in (("fresh", "fresh-collect"), ("resume", "resume-collect")):
                record = self.records[record_name]
                records[name] = {
                    "directory": record["directory"],
                    "outcome": record["outcome"],
                    "payload": record["payload"],
                }
        self.ops.root_id = self.root_id
        if names == ("fresh", "resume"):
            self.ops.replay(records)
        else:
            self.ops.replay(records, names=names)

    def shutdown(self) -> None:
        require(self.daemon is not None, "private daemon was not started")
        deadline = time.monotonic() + 30
        terminal = False
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            process = self.direct("final-status", pueue_command(self.pueue, self.config, "status", "--json"),
                                  timeout=min(20, remaining))
            status = status_jobs(process)
            if self.terminal_private_queue(status):
                terminal = True
                break
            delay = min(0.2, max(0.0, deadline - time.monotonic()))
            if delay <= 0:
                break
            time.sleep(delay)
        if not terminal:
            raise AcceptanceFailure("private queue did not reach a terminal state")
        self.direct("private-shutdown", pueue_command(self.pueue, self.config, "shutdown"), timeout=30)
        self.daemon.wait(30, expected=0)
        self.closed = True

    def safe_shutdown(self) -> bool:
        if self.closed:
            return True
        if self.daemon is None:
            processes = getattr(self, "processes", None)
            if processes is None:
                self.closed = True
                return True
            try:
                if not processes.finished_naturally():
                    return False
            except BaseException:
                return False
            self.closed = True
            return True
        try:
            if self.root_id is not None and self.ops.root_id is not None:
                require(self.root_id == self.ops.root_id,
                        "failure cleanup root identity changed")
            elif self.root_id is not None:
                self.ops.root_id = self.root_id
            self._sync_ops_owned_tasks()
            cleanup_ops: NativeTaskOps = self.ops
            if getattr(self, "discover_models_only", False):
                cleanup_ops = ModelDiscoveryCleanupOps(self)
            cleanup_ops.bind_supervisor(self.config, self.daemon)
            retained = cleanup_ops.retain_failure_ownership()
        except BaseException:
            return False
        if retained:
            self.closed = True
        return retained

    def run_interactive(self) -> int:
        before = workspace_snapshot(self.workspace)
        task_id = self.dispatch("interactive", self.interactive_brief("interactive"))
        pending_state, cursor, pending_events = self.wait_for_interaction(task_id, pending=True)
        request_id, turn_id = self.interactive_request(pending_state)
        message_id, queued_delivery = self.reply_interaction(task_id, request_id, turn_id)
        final_state, final_cursor, final_events = self.wait_for_interaction(task_id, cursor)
        deliveries = final_state.get("deliveries")
        require(isinstance(deliveries, list), "interactive final delivery state is malformed")
        delivery = next((item for item in deliveries
                         if isinstance(item, dict) and item.get("message_id") == message_id), None)
        require(isinstance(delivery, dict) and delivery.get("state") == "acknowledged",
                "interactive reply was not acknowledged by the provider")
        self.wait_done("interactive", task_id)
        self.collect("interactive-collect", task_id)
        self.task_binding(task_id)
        provider_session = self.session_ref(task_id)
        require(workspace_snapshot(self.workspace) == before,
                "Claude interactive acceptance changed the workspace")

        interaction_path = self.state / "tasks" / task_id / "raw" / "interaction"
        metadata = interaction_path.lstat()
        require(stat.S_ISREG(metadata.st_mode) and not stat.S_ISLNK(metadata.st_mode) and
                0 < metadata.st_size <= MAX_RAW_BYTES,
                "interactive task has no bounded sealed interaction receipt")
        interaction_receipt = {
            "schema_version": 1,
            "task_id": task_id,
            "request_id": request_id,
            "turn_id": turn_id,
            "message_id": message_id,
            "answer": "Blue",
            "queued_delivery": queued_delivery,
            "delivery": delivery,
            "initial_cursor": cursor,
            "final_cursor": final_cursor,
            "event_kinds": [event.get("kind") for event in pending_events + final_events
                            if isinstance(event.get("kind"), str)],
            "closed": final_state.get("closed") is True,
            "raw_interaction_sha256": digest(interaction_path),
        }
        require(interaction_receipt["closed"] is True,
                "interactive task did not publish a closed interaction state")
        write_json(self.output / "interaction.json", interaction_receipt)
        self.replay({"interactive": self.records["interactive-collect"]}, names=("interactive",))
        self.shutdown()
        self.success_receipt = {
            "gate": "acceptance-native", "status": "passed", "provider": self.provider,
            "mode": self.mode, "scenario": self.scenario,
            "task_budget": TASK_BUDGET,
            "canonical_task_budget": CANONICAL_TASK_BUDGET,
            "requested_model": getattr(self.args, "model", None),
            "requested_effort": getattr(self.args, "effort", None),
            "model_discovery": self.model_discovery_summary,
            "native_mode": self.native_mode_receipt(),
            **self.provider_runtime_receipt(),
            "tasks": {"interactive": task_id},
            "provider_sessions": {"interactive": provider_session},
            "interaction": interaction_receipt,
            "provider_launches": 1, "replay_launches": 0,
            "credentials_in_receipt": False,
            "natural_daemon_shutdown": self.closed,
        }
        return 0

    def run_timeout_resume(self) -> int:
        source = self.dispatch("timeout-source", self.timeout_brief("timeout-source"),
                               budget=TIMEOUT_RESUME_SOURCE_BUDGET)
        self.record_timeout_liveness(source)
        source_row = self.wait_done(
            "timeout-source", source, expected_result=TIMEOUT_SOURCE_RUNNER_RESULTS)
        source_runner_result = done_result(source_row)
        require(isinstance(source_runner_result, str) and
                source_runner_result in TIMEOUT_SOURCE_RUNNER_RESULTS,
                "timeout source has no accepted runner result")
        self.ops.expect_terminal_result("timeout-source", source_runner_result)
        timed_out = self.wait_timed_out(source)
        self.wait_timeout_leases_closed()
        source_collect = self.collect_timed_out_source(source)
        source_directory = self.timeout_source_binding(source)
        source_session = self.session_ref(source)
        self._timeout_workspace_snapshot(resumed=False)
        source_snapshot = task_digest_snapshot(source_directory)

        successor = self.continue_timed_out_task(
            "timeout-resume", self.timeout_brief("timeout-resume", resume=True), source)
        self.wait_done("timeout-resume", successor)
        self.collect("timeout-resume-collect", successor)
        self.task_binding(successor, source, source_session)
        successor_session = self.session_ref(successor)
        require_same_session(source_session, successor_session)
        require_replay_unchanged(source_snapshot, task_digest_snapshot(source_directory))
        require(not (source_directory / "result.txt").exists(),
                "continuation changed its timed-out predecessor into an answer")
        self._timeout_workspace_snapshot(resumed=True)
        self.replay({"timeout-resume": self.records["timeout-resume-collect"]},
                    names=("timeout-resume",))
        self.shutdown()
        continuation = timed_out.get("continuation")
        require(isinstance(continuation, dict), "timed-out continuation receipt is unavailable")
        self.success_receipt = {
            "gate": "acceptance-native", "status": "passed", "provider": self.provider,
            "mode": self.mode, "scenario": self.scenario,
            "task_budget": TASK_BUDGET,
            "timeout_source_budget": TIMEOUT_RESUME_SOURCE_BUDGET,
            "canonical_timeout_source_budget": CANONICAL_TIMEOUT_RESUME_SOURCE_BUDGET,
            "requested_model": getattr(self.args, "model", None),
            "requested_effort": getattr(self.args, "effort", None),
            **self.provider_runtime_receipt(),
            "tasks": {"timeout_source": source, "timeout_resume": successor},
            "provider_sessions": {
                "timeout_source": source_session,
                "timeout_resume": successor_session,
            },
            "source_runner_result": source_runner_result,
            "source_publication": source_collect.get("publication"),
            "continuation": continuation,
            "timeout_liveness": self.timeout_liveness,
            "same_session": True,
            "source_immutable": True,
            "replay_launches": 0,
            "credentials_in_receipt": False,
            "natural_daemon_shutdown": self.closed,
        }
        return 0

    def run(self) -> int:
        self.success_receipt = None
        self.setup()
        if self.discover_models_only:
            self.discover_models()
            self.shutdown()
            self.success_receipt = {
                "gate": "acceptance-native", "status":
                "passed" if self.model_discovery_exit_code == 0 else
                "BLOCKED" if self.model_discovery_exit_code == 2 else "failed",
                "provider": self.provider, "mode": self.mode,
                "task_budget": TASK_BUDGET,
                "canonical_task_budget": CANONICAL_TASK_BUDGET,
                "requested_model": getattr(self.args, "model", None),
                "requested_effort": getattr(self.args, "effort", None),
                "model_discovery": self.model_discovery_summary,
                "discovery_exit_code": self.model_discovery_exit_code,
                "native_mode": self.native_mode_receipt(),
                "credentials_in_receipt": False,
                "natural_daemon_shutdown": self.closed,
            }
            require(isinstance(self.model_discovery_exit_code, int),
                    "model discovery exit code is unavailable")
            return self.model_discovery_exit_code
        if self.scenario == "timeout-resume":
            return self.run_timeout_resume()
        if self.scenario == "interactive":
            return self.run_interactive()
        before = workspace_snapshot(self.workspace)
        if self.provider == "codex:review":
            first = self.dispatch("fresh", self.brief("fresh"))
            self.wait_done("fresh", first)
            self.collect("fresh-collect", first)
            self.task_binding(first)
            first_session = self.session_ref(first)
            require(workspace_snapshot(self.workspace) == before,
                    "Codex review changed the workspace")
            self.replay({"fresh": self.records["fresh-collect"]}, names=("fresh",))
            self.shutdown()
            self.success_receipt = {
                "gate": "acceptance-native", "status": "passed", "provider": self.provider,
                "mode": self.mode, "task_budget": TASK_BUDGET,
                "canonical_task_budget": CANONICAL_TASK_BUDGET,
                "requested_model": getattr(self.args, "model", None),
                "requested_effort": getattr(self.args, "effort", None),
                "model_discovery": self.model_discovery_summary,
                "native_mode": self.native_mode_receipt(),
                **self.provider_runtime_receipt(),
                "tasks": {"fresh": self.tasks["fresh"]},
                "provider_sessions": {"fresh": first_session},
                "scenario": self.scenario, "provider_launches": 1, "replay_launches": 0,
                "credentials_in_receipt": False,
                "natural_daemon_shutdown": self.closed,
            }
            return 0
        first = self.dispatch("fresh", self.brief("fresh"))
        self.wait_done("fresh", first)
        self.collect("fresh-collect", first)
        self.task_binding(first)
        first_session = self.session_ref(first)
        first_record = task_digest_snapshot(self.records["fresh-collect"]["directory"])
        first_check: bytes | None = None
        if self.mode == "workspace-write":
            require(self.nonce is not None, "write acceptance nonce is unavailable")
            verify_write_fixture(self.nonce, self.fixture_file)
            verify_write_artifacts(self.nonce, self.target_file, self.check_file)
            self._workspace_write_snapshot(continuation=False)
            first_check = self.check_file.read_bytes()
        second = self.continue_task("resume", self.brief("resume", continuation=True), first)
        self.wait_done("resume", second)
        self.collect("resume-collect", second)
        self.task_binding(second, first, first_session)
        second_session = self.session_ref(second)
        require_same_session(first_session, second_session)
        require_replay_unchanged(first_record,
                                 task_digest_snapshot(self.records["fresh-collect"]["directory"]))
        if self.mode == "workspace-write":
            require(self.nonce is not None and first_check is not None,
                    "write acceptance evidence is unavailable")
            verify_write_artifacts(self.nonce, self.target_file, self.continue_check_file,
                                   continuation=True)
            self._workspace_write_snapshot(continuation=True)
            require(self.check_file.read_bytes() == first_check,
                    "continuation changed the first shell digest artifact")
        else:
            require(workspace_snapshot(self.workspace) == before,
                    "read-only live acceptance changed the workspace")
        self.replay()
        self.shutdown()
        self.success_receipt = {
            "gate": "acceptance-native", "status": "passed", "provider": self.provider,
            "mode": self.mode, "task_budget": TASK_BUDGET,
            "canonical_task_budget": CANONICAL_TASK_BUDGET,
            "requested_model": getattr(self.args, "model", None),
            "requested_effort": getattr(self.args, "effort", None),
            "model_discovery": self.model_discovery_summary,
            "native_mode": self.native_mode_receipt(),
            **self.provider_runtime_receipt(),
            "tasks": {
                "fresh": self.tasks["fresh"],
                "resume": self.tasks["resume"],
            },
            "provider_sessions": {"fresh": first_session, "resume": second_session},
            "scenario": self.scenario, "provider_launches": 2, "replay_launches": 0,
            "credentials_in_receipt": False,
            "natural_daemon_shutdown": self.closed,
        }
        return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--provider", required=True, choices=sorted(PROFILES))
    parser.add_argument("--permission", "--mode", dest="mode",
                        choices=("read-only", "workspace-write"), default=None)
    parser.add_argument("--scenario", choices=("read-only", "write", "timeout-resume", "interactive"))
    parser.add_argument("--output")
    parser.add_argument("--delegate")
    parser.add_argument("--runner")
    parser.add_argument("--pueue")
    parser.add_argument("--pueued")
    parser.add_argument("--provider-executable")
    parser.add_argument("--model")
    parser.add_argument("--effort")
    parser.add_argument("--discover-models", action="store_true",
                        help="run advisory model discovery and stop without admitting a task")
    return parser


def _persist_failure_receipt(output: Path | None, value: dict[str, object]) -> bool:
    if output is None:
        return True
    try:
        write_json(output / "failure.json", value)
        return True
    except BaseException as error:
        print("FAIL acceptance-native: failure diagnostics could not be persisted: " + str(error),
              file=sys.stderr, flush=True)
        return False


def _finalize_run(run: NativeAcceptance | None, success: bool) -> bool:
    if run is None:
        return True
    try:
        finalized = run.finalize(success)
    except BaseException as error:
        print("FAIL acceptance-native: test-owned runtime cleanup raised: " + str(error),
              file=sys.stderr, flush=True)
        return False
    if not finalized:
        print("FAIL acceptance-native: test-owned runtime cleanup was not completed",
              file=sys.stderr, flush=True)
    return finalized


def _model_discovery_failure_receipt(args: argparse.Namespace, run: NativeAcceptance,
                                     code: int) -> dict[str, object]:
    summary = getattr(run, "model_discovery_summary", None)
    sanitized: dict[str, object] = {}
    if isinstance(summary, dict):
        for key in ("status", "reason_code", "complete", "model_count", "effort_count"):
            value = summary.get(key)
            if key in {"status", "reason_code"} and isinstance(value, str):
                sanitized[key] = value
            elif key == "complete" and type(value) is bool:
                sanitized[key] = value
            elif key in {"model_count", "effort_count"} and (value is None or type(value) is int):
                sanitized[key] = value
    return {
        "gate": "acceptance-native",
        "status": "BLOCKED" if code == 2 else "failed",
        "provider": args.provider,
        "mode": args.mode,
        "scenario": args.scenario,
        "task_budget": TASK_BUDGET,
        "requested_model": args.model,
        "requested_effort": args.effort,
        "discover_models": True,
        "discovery_exit_code": code,
        "model_discovery": sanitized or None,
        "no_retry": True,
        "credentials_in_receipt": False,
    }


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    run: NativeAcceptance | None = None
    output: Path | None = None
    try:
        output = choose_output(args.provider, args.output, args.mode, args.scenario)
        if args.provider == "ccr:print" and not args.model and not args.discover_models:
            raise BlockedFailure("CCR acceptance requires --model with a configured alias")
        if args.provider == "opencode:run" and not args.model and not args.discover_models:
            raise BlockedFailure("OpenCode acceptance requires --model with a tool-capable configured route")
        run = NativeAcceptance(argparse.Namespace(**{**vars(args), "output": str(output)}))
        code = run.run()
        if not _finalize_run(run, code == 0):
            return 1
        if code != 0:
            if args.discover_models:
                if not _persist_failure_receipt(output, _model_discovery_failure_receipt(args, run, code)):
                    return 1
            else:
                if not _persist_failure_receipt(output, {
                        "gate": "acceptance-native", "status": "BLOCKED" if code == 2 else "failed",
                        "provider": args.provider, "mode": args.mode, "scenario": args.scenario,
                        "task_budget": TASK_BUDGET, "requested_model": args.model,
                        "requested_effort": args.effort, "no_retry": True,
                        "credentials_in_receipt": False}):
                    return 1
        else:
            run.publish_success()
        status = "passed" if code == 0 else "BLOCKED" if code == 2 else "failed"
        print(json.dumps({"gate": "acceptance-native", "status": status, "provider": args.provider,
                          "evidence": str(output)}, sort_keys=True))
        return code
    except BlockedFailure as error:
        receipt_ok = _persist_failure_receipt(output, {"gate": "acceptance-native", "status": "BLOCKED",
                                                       "provider": args.provider, "mode": args.mode,
                                                       "scenario": args.scenario, "task_budget": TASK_BUDGET,
                                                       "requested_model": args.model,
                                                       "requested_effort": args.effort,
                                                       "discover_models": args.discover_models,
                                                       "error": str(error),
                                                       "no_retry": True, "credentials_in_receipt": False})
        cleanup_ok = _finalize_run(run, False)
        if not receipt_ok or not cleanup_ok:
            return 1
        print(f"BLOCKED acceptance-native: {error}", file=sys.stderr, flush=True)
        return 2
    except (AcceptanceFailure, OSError, ValueError, json.JSONDecodeError) as error:
        _persist_failure_receipt(output, {"gate": "acceptance-native", "status": "failed",
                                          "provider": args.provider, "mode": args.mode,
                                          "scenario": args.scenario, "task_budget": TASK_BUDGET,
                                          "requested_model": args.model,
                                          "requested_effort": args.effort,
                                          "discover_models": args.discover_models,
                                          "error": str(error),
                                          "no_retry": True, "credentials_in_receipt": False})
        _finalize_run(run, False)
        print(f"FAIL acceptance-native: {error}", file=sys.stderr, flush=True)
        return 1
    except BaseException as error:
        _persist_failure_receipt(output, {"gate": "acceptance-native", "status": "failed",
                                          "provider": args.provider, "mode": args.mode,
                                          "scenario": args.scenario, "task_budget": TASK_BUDGET,
                                          "requested_model": args.model,
                                          "requested_effort": args.effort,
                                          "discover_models": args.discover_models,
                                          "error": str(error),
                                          "traceback": traceback.format_exc(), "no_retry": True,
                                          "credentials_in_receipt": False})
        _finalize_run(run, False)
        print(f"FAIL acceptance-native: unexpected {error}", file=sys.stderr, flush=True)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
