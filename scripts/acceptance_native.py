#!/usr/bin/env python3
"""Run a bounded live acceptance gate for the native provider matrix.

The gate uses the shipped dispatcher and runner for every provider turn.  It
does not copy credentials, call the provider directly, retry paid work, or
turn an authentication refusal into a passing result.
"""

from __future__ import annotations

import argparse
from copy import deepcopy
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import secrets
import shlex
import shutil
import stat
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
    require,
    snapshot,
    status_jobs,
    validate_queue_row,
    verify_collected_outcome,
    write_bytes,
)
from acceptance_supervisor_common import (
    Processes,
    config_for,
    digest,
    inherited_environment,
    read_json,
    write_json,
)


PROFILES = {
    "antigravity:print": {
        "executable": "agy", "modes": ("read-only", "workspace-write"),
        "default_mode": "workspace-write",
    },
    "claude:print": {
        "executable": "claude", "modes": ("read-only", "workspace-write"),
        "default_mode": "read-only",
    },
    "codex:exec": {
        "executable": "codex", "modes": ("read-only", "workspace-write"),
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
PUEUE_VERSION = "4.0.4"
TASK_BUDGET = "120s"
CANONICAL_TASK_BUDGET = "2m0s"
TASK_BUDGET_NANOS = 120_000_000_000
WATCH_SECONDS = 180
# model-discovery-v1 allows provider CLIs to populate a remote model/cache
# catalog on their first run; this outer wait adds supervisor bootstrap time.
MODEL_DISCOVERY_TASK_BUDGET = "5m0s"
MODEL_DISCOVERY_TASK_BUDGET_NANOS = 300_000_000_000
MODEL_DISCOVERY_TIMEOUT_SECONDS = 330
MAX_RAW_BYTES = 8 * 1024 * 1024
AUTHENTICATION_MARKERS = (
    "authentication required", "not authenticated", "please log in", "sign in",
    "unauthorized", "api key", "credentials", "login required", "no provider",
    "provider authentication unavailable", "persistent chatgpt authentication is required",
)
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


def choose_output(provider: str, explicit: str | None) -> Path:
    if explicit:
        path = clean_absolute(explicit, "live acceptance output")
    else:
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + secrets.token_hex(6)
        path = Path.home() / ".local" / "state" / "delegation-layer-evidence" / f"{provider.replace(':', '-')}-{stamp}"
    reject_tmp(path, "live acceptance output")
    if path.exists():
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
    for relative in ("raw/stderr", "raw/stdout", "publish.reject"):
        path = directory / relative
        if not path.is_file() or path.is_symlink():
            continue
        try:
            with path.open("rb") as stream:
                data = stream.read(MAX_RAW_BYTES).decode("utf-8", errors="replace").lower()
        except OSError:
            continue
        if any(marker in data for marker in AUTHENTICATION_MARKERS):
            return True
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


def authentication_blocked(response: object, diagnostic: str) -> bool:
    return (authentication_blocked_response(response) or
            any(marker in diagnostic.lower() for marker in AUTHENTICATION_MARKERS))


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


def bounded_text(path: Path, label: str) -> str:
    with path.open("rb") as stream:
        data = stream.read(MAX_RAW_BYTES + 1)
    require(len(data) <= MAX_RAW_BYTES, label + " exceeds the bounded output limit")
    return data.decode("utf-8", errors="replace").strip()


def marker_answer(marker: str, continuation: bool) -> str:
    return f"DELEGATION_LIVE_{'CONTINUED' if continuation else 'OK'}: {marker}"


def random_marker() -> bytes:
    return ("DELEGATION-LIVE-" + secrets.token_hex(24) + "\n").encode()


def verify_read_only_answer(payload: bytes, expected: str, label: str) -> None:
    try:
        answer = payload.decode("utf-8")
    except UnicodeDecodeError as error:
        raise AcceptanceFailure(f"{label} answer is not valid UTF-8") from error
    require(answer.strip() == expected,
            f"{label} did not return the exact marker-derived answer")


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
        if scenario not in {"read-only", "write"}:
            raise AcceptanceFailure(f"unsupported native acceptance scenario: {scenario}")
        if scenario == "read-only" and requested_mode != "read-only":
            raise AcceptanceFailure("read-only scenario requires --permission read-only")
        if scenario == "write" and requested_mode != "workspace-write":
            raise AcceptanceFailure("write scenario requires --permission workspace-write")
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
        if self.provider == "codex:exec":
            discovered_git = shutil.which("git", path=self.environment.get("PATH"))
            try:
                self.git = resolve_executable(discovered_git, Path("/usr/bin/git"), "git")
            except BlockedFailure as error:
                raise BlockedFailure("Codex git prerequisite is unavailable") from error

        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + secrets.token_hex(6)
        self.base = Path.home() / "delegation-layer-live" / f"{self.provider.replace(':', '-')}-{stamp}"
        reject_tmp(self.base, "live acceptance base")
        self.base.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.state = self.base / "state"
        self.workspace = self.base / "workspace"
        self.briefs = self.base / "briefs"
        self.pueue_base = self.base / "pueue"
        self.models_state = self.base / "models-state"
        for path in (self.state, self.workspace, self.briefs, self.pueue_base,
                     self.pueue_base / "state", self.pueue_base / "run",
                     self.models_state,):
            path.mkdir(mode=0o700, parents=False)
        self.config = self.pueue_base / "pueue.yml"
        write_json(self.config, config_for(self.pueue_base))
        self.processes = Processes(self.output / "processes", self.environment)
        self.ops = NativeTaskOps(self.processes, self.delegate, self.runner, self.pueue,
                                 self.base, self.state, self.output,
                                 watch_seconds=WATCH_SECONDS,
                                 expected_inspection_binding_required=False)
        self.daemon = None
        self.closed = False
        self.model_discovery: dict[str, object] | None = None
        self.model_discovery_summary: dict[str, object] | None = None
        self.model_discovery_exit_code: int | None = None
        self.models_root_id: str | None = None
        self.model_discovery_task_id: str | None = None
        self.root_id: str | None = None
        self.provider_version: str | None = None
        self.provider_sha256: str | None = None
        self.tasks: dict[str, str] = {}
        self.numeric_ids: dict[str, int] = {}
        self.records: dict[str, dict[str, object]] = {}
        self.workspace_baseline: dict[str, dict[str, object]] | None = None
        self.workspace_marker = self.workspace / "acceptance-marker.txt"
        self.marker_bytes = random_marker()
        write_bytes(self.workspace_marker, self.marker_bytes)
        self.marker_text = bounded_text(self.workspace_marker, "acceptance marker")
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

    def write_binding(self) -> None:
        binding = self.output / "binding.json"
        require(not binding.exists() and not binding.is_symlink(),
                "runtime binding was already persisted")
        require(isinstance(self.provider_version, str) and bool(self.provider_version),
                "runtime provider version is unavailable")
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
            "provider_executable": str(self.provider_executable),
            "provider_sha256": self.provider_sha256,
            "provider_version": self.provider_version,
            "delegate_sha256": digest(self.delegate),
            "runner_sha256": digest(self.runner),
            "pueue_sha256": digest(self.pueue),
            "pueued_sha256": digest(self.pueued),
            "pueue_version": PUEUE_VERSION,
            "credentials_in_receipt": False,
        })

    def native_mode_receipt(self) -> dict[str, object] | None:
        if self.provider != "antigravity:print":
            return None
        if self.mode == "read-only":
            return {"sandbox": "--sandbox", "mode": "plan", "bypass": False}
        return {"sandbox": "--sandbox", "mode": "accept-edits", "bypass": True}

    def _prepare_codex_workspace(self) -> None:
        if self.provider != "codex:exec":
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
        require(binding.get("definition_revision") == "model-discovery-v1" and
                binding.get("helper_executable") == str(self.provider_executable) and
                binding.get("helper_sha256") == self.provider_sha256,
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
        self.provider_sha256 = digest(self.provider_executable)
        pueue_version = self.direct("pueue-version", pueue_command(self.pueue, self.config, "--version"), timeout=20)
        pueued_version = self.direct("pueued-version", [self.pueued, "-c", self.config, "--version"], timeout=20)
        if bounded_text(pueue_version.directory / "stdout", "pueue version output") != f"pueue {PUEUE_VERSION}":
            raise BlockedFailure(f"pueue {PUEUE_VERSION} is unavailable")
        if bounded_text(pueued_version.directory / "stdout", "pueued version output") != f"pueued {PUEUE_VERSION}":
            raise BlockedFailure(f"pueued {PUEUE_VERSION} is unavailable")
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
        self.daemon = self.processes.start("pueued", [self.pueued, "-c", self.config], self.base)
        self.ops.bind_supervisor(self.config, self.daemon)
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            process = self.ops.client("daemon-status", ["status", "--json"], expected={0, 1}, timeout=20)
            if process.result.get("exit_code") == 0:
                status = status_jobs(process)
                if not status.get("tasks"):
                    self.workspace_baseline = workspace_snapshot(self.workspace)
                    return
            time.sleep(0.2)
        raise AcceptanceFailure("private pueue readiness observation expired")

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
            prefix = "DELEGATION_LIVE_CONTINUED:" if continuation else "DELEGATION_LIVE_OK:"
            text = "Continue the exact previous conversation. " if continuation else ""
            text += (f"Read the actual file `{self.workspace_marker}` using a file-read tool or a read-only "
                     "shell command. Do not infer its contents from examples or other repositories. "
                     f"Reply with exactly the literal prefix `{prefix}` followed by one space and the exact "
                     "contents read from that file, with no other text. "
                     "Do not modify files, use network, or access paths outside the current workspace.\n")
        write_bytes(path, text.encode())
        return path

    def dispatch_argv(self, task_id: str, brief: Path,
                      predecessor: str | None = None) -> list[object]:
        argv: list[object] = [self.delegate, "--root", self.state, "--pueue-config", self.config,
                              "--runner", self.runner, "dispatch", "--provider", self.provider,
                              "--brief", brief, "--cwd", self.workspace, "--id", task_id,
                              "--permission", self.mode, "--budget", TASK_BUDGET, "--json"]
        model = getattr(self.args, "model", None)
        effort = getattr(self.args, "effort", None)
        if model:
            argv.extend(["--model", model])
        if effort:
            argv.extend(["--effort", effort])
        if predecessor:
            argv.extend(["--resume-task", predecessor])
        return argv

    def dispatch(self, name: str, brief: Path, predecessor: str | None = None) -> str:
        task_id = secrets.token_hex(16)
        argv = self.dispatch_argv(task_id, brief, predecessor)
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

    def wait_done(self, name: str, task_id: str) -> None:
        require(self.root_id is not None, "root identity is unavailable")
        self.ops.root_id = self.root_id
        try:
            self.ops.wait_task(name, task_id)
        except (AcceptanceFailure, RuntimeError) as error:
            directory = self.state / "tasks" / task_id
            if authentication_unavailable(directory):
                raise BlockedFailure(f"{self.provider} authentication is unavailable") from error
            raise AcceptanceFailure(f"{name} runner completion failed: {error}") from error

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
        require(isinstance(self.provider_sha256, str) and self.provider_sha256,
                "provider executable digest is unavailable")
        for task_id, record in records.items():
            request = record.get("request")
            require(isinstance(request, dict), f"inspection request is absent for {task_id}")
            binding = request.get("binding")
            require(isinstance(binding, dict) and
                    binding.get("helper_executable") == str(self.provider_executable) and
                    binding.get("helper_sha256") == self.provider_sha256,
                    f"inspection helper binding is not the selected provider for {task_id}")
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
            require(isinstance(group, dict) and group.get("status") == "Running" and
                    type(group.get("parallel_tasks")) is int and
                    group.get("parallel_tasks") == 1,
                    f"private queue group is not running at parallelism one: {name}")

        expected: dict[str, tuple[str, str]] = {}
        for task_id in self.tasks.values():
            require(task_id in self.numeric_ids, f"ordinary task has no numeric supervisor identity: {task_id}")
            label = f"delegate:{self.root_id}:{task_id}"
            require(label not in expected, "ordinary task labels are not unique")
            expected[label] = ("ordinary", task_id)
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
                validate_queue_row(row, label, self.runner, self.state, task_id,
                                   self.numeric_ids[task_id], "default")
            else:
                record = inspection_records[task_id]
                require(row.get("group") == record["group"] and
                        row.get("id") == record["numeric_task_id"],
                        f"inspection queue identity is invalid for {task_id}")
                expected_command = [str(self.runner), "--inspection", "--root",
                                    str(self.state), task_id]
                commands = []
                for key in ("original_command", "command"):
                    value = row.get(key)
                    require(isinstance(value, str), f"inspection queue row missing {key}")
                    commands.append(shlex.split(value))
                require(commands == [expected_command, expected_command],
                        f"inspection queue argv is invalid for {task_id}")
            require(done_result(row) == "Success", f"private queue task is not successful: {task_id}")
        return seen == set(expected)

    def collect(self, name: str, task_id: str) -> dict[str, object]:
        self.ops.root_id = self.root_id
        directory = self.state / "tasks" / task_id
        try:
            response = self.ops.collect(name, task_id)
        except (AcceptanceFailure, RuntimeError) as error:
            if authentication_unavailable(directory):
                raise BlockedFailure(f"{self.provider} authentication is unavailable") from error
            raise AcceptanceFailure(f"{name} collection failed: {error}") from error
        if authentication_unavailable(directory):
            raise BlockedFailure(f"{self.provider} authentication is unavailable")
        outcome, published_payload = verify_collected_outcome(response, directory, "committed")
        require(published_payload, f"{name} committed an empty response")
        if self.mode == "read-only":
            verify_read_only_answer(
                published_payload, marker_answer(self.marker_text, continuation="resume" in name), name)
        provider_exit = read_json(directory / "provider.exit")
        require(provider_exit.get("root_id") == self.root_id and
                provider_exit.get("task_id") == task_id and
                provider_exit.get("invocation_state") == "started" and
                provider_exit.get("exit_code") == 0 and provider_exit.get("error") == "" and
                isinstance(provider_exit.get("raw_manifest"), list) and
                isinstance(provider_exit.get("manifest_sha256"), str) and
                outcome.get("evidence_sha256") == provider_exit.get("manifest_sha256"),
                f"{name} lacks sealed provider evidence")
        collection_payload = response.get("payload")
        require(isinstance(collection_payload, dict) and
                collection_payload == outcome.get("payload") and
                response.get("evidence_sha256") == outcome.get("evidence_sha256"),
                f"{name} collection receipt descriptors are inconsistent")
        self.records[name] = {"task_id": task_id, "directory": directory,
                              "outcome": deepcopy(outcome),
                              "payload": deepcopy(collection_payload),
                              "snapshot": task_digest_snapshot(directory)}
        return response

    def _request_binding(self, task_id: str) -> tuple[
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
                requested.get("budget") == CANONICAL_TASK_BUDGET and
                record.get("budget_nanos") == TASK_BUDGET_NANOS and
                brief.is_file() and not brief.is_symlink() and
                record.get("brief_length") == brief.stat().st_size and
                record.get("brief_sha256") == digest(brief),
                f"task {task_id} request binding is invalid")
        require(self.provider_sha256 is not None,
                "provider executable prerequisite evidence is unavailable")
        meta = read_json(directory / "meta.json")
        meta_digest = digest(directory / "meta.json")
        spec_digest = meta.get("spec_sha256")
        observed_version = meta.get("provider_version")
        require(isinstance(observed_version, str) and bool(observed_version),
                f"task {task_id} has no admitted provider version")
        if self.provider_version is None:
            self.provider_version = observed_version
            self.write_binding()
        else:
            require(observed_version == self.provider_version,
                    f"task {task_id} provider version drifted")
        return directory, record, meta, meta_digest, spec_digest, brief

    def _workspace_write_snapshot(self, continuation: bool) -> dict[str, dict[str, object]]:
        require(self.nonce is not None, "write acceptance nonce is unavailable")
        require(self.workspace_baseline is not None,
                "write acceptance workspace baseline is unavailable")
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
        git_directory = self.workspace / ".git"
        for path in self.workspace.rglob("*"):
            require(not path.is_symlink(), "write acceptance workspace contains a symlink")
            if path.is_dir():
                require(self.provider == "codex:exec" and
                        (path == git_directory or git_directory in path.parents),
                        "write acceptance workspace contains an unexpected directory")
        actual = workspace_snapshot(self.workspace)
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

    def task_binding(self, task_id: str, predecessor: str | None = None,
                     predecessor_session: str | None = None) -> None:
        directory, record, meta, meta_digest, spec_digest, _ = self._request_binding(task_id)
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
        require(meta.get("root_id") == self.root_id and meta.get("task_id") == task_id and
                meta.get("provider_executable") == str(self.provider_executable) and
                meta.get("provider_version") == self.provider_version and
                meta.get("containment") == self.mode and
                isinstance(spec_digest, str) and len(spec_digest) == 64 and
                all(character in "0123456789abcdef" for character in spec_digest),
                f"task {task_id} metadata binding is invalid")
        effective = meta.get("effective_config")
        policy = effective.get("policy") if isinstance(effective, dict) else None
        require(isinstance(effective, dict) and effective.get("containment") == self.mode and
                isinstance(effective.get("digest"), str) and
                isinstance(policy, dict) and policy.get("workspace") == str(self.workspace) and
                policy.get("runtime_sha256") == self.provider_sha256,
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
                supervisor.get("observed_version") == f"pueue {PUEUE_VERSION}",
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

    def replay(self) -> None:
        records: dict[str, dict[str, object]] = {}
        for name, record_name in (("fresh", "fresh-collect"), ("resume", "resume-collect")):
            record = self.records[record_name]
            records[name] = {
                "directory": record["directory"],
                "outcome": record["outcome"],
                "payload": record["payload"],
            }
        self.ops.root_id = self.root_id
        self.ops.replay(records)

    def shutdown(self) -> None:
        require(self.daemon is not None, "private daemon was not started")
        for _ in range(30):
            process = self.direct("final-status", pueue_command(self.pueue, self.config, "status", "--json"), timeout=20)
            status = status_jobs(process)
            if self.terminal_private_queue(status):
                break
            time.sleep(0.2)
        else:
            raise AcceptanceFailure("private queue did not reach a terminal state")
        self.direct("private-shutdown", pueue_command(self.pueue, self.config, "shutdown"), timeout=30)
        self.daemon.wait(30, expected=0)
        self.closed = True

    def safe_shutdown(self) -> bool:
        if self.closed:
            return True
        if self.daemon is None:
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

    def run(self) -> int:
        self.setup()
        if self.discover_models_only:
            self.discover_models()
            self.shutdown()
            write_json(self.output / "success.json", {
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
            })
            require(isinstance(self.model_discovery_exit_code, int),
                    "model discovery exit code is unavailable")
            return self.model_discovery_exit_code
        before = workspace_snapshot(self.workspace)
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
        write_json(self.output / "success.json", {
            "gate": "acceptance-native", "status": "passed", "provider": self.provider,
            "mode": self.mode, "task_budget": TASK_BUDGET,
            "canonical_task_budget": CANONICAL_TASK_BUDGET,
            "requested_model": getattr(self.args, "model", None),
            "requested_effort": getattr(self.args, "effort", None),
            "model_discovery": self.model_discovery_summary,
            "native_mode": self.native_mode_receipt(),
            "tasks": {
                "fresh": self.tasks["fresh"],
                "resume": self.tasks["resume"],
            },
            "scenario": self.scenario, "provider_launches": 2, "replay_launches": 0,
            "credentials_in_receipt": False,
            "natural_daemon_shutdown": self.closed,
        })
        return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--provider", required=True, choices=sorted(PROFILES))
    parser.add_argument("--permission", "--mode", dest="mode",
                        choices=("read-only", "workspace-write"), default=None)
    parser.add_argument("--scenario", choices=("read-only", "write"))
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


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    run: NativeAcceptance | None = None
    output: Path | None = None
    try:
        output = choose_output(args.provider, args.output)
        run = NativeAcceptance(argparse.Namespace(**{**vars(args), "output": str(output)}))
        code = run.run()
        status = "passed" if code == 0 else "BLOCKED" if code == 2 else "failed"
        print(json.dumps({"gate": "acceptance-native", "status": status, "provider": args.provider,
                          "evidence": str(output)}, sort_keys=True))
        return code
    except BlockedFailure as error:
        if output is not None:
            write_json(output / "failure.json", {"gate": "acceptance-native", "status": "BLOCKED",
                                                  "provider": args.provider, "mode": args.mode,
                                                  "scenario": args.scenario, "task_budget": TASK_BUDGET,
                                                  "requested_model": args.model,
                                                  "requested_effort": args.effort,
                                                  "discover_models": args.discover_models,
                                                  "error": str(error),
                                                  "no_retry": True, "credentials_in_receipt": False})
        if run is not None:
            run.safe_shutdown()
        print(f"BLOCKED acceptance-native: {error}", file=sys.stderr, flush=True)
        return 2
    except (AcceptanceFailure, OSError, ValueError, json.JSONDecodeError) as error:
        if output is not None:
            write_json(output / "failure.json", {"gate": "acceptance-native", "status": "failed",
                                                  "provider": args.provider, "mode": args.mode,
                                                  "scenario": args.scenario, "task_budget": TASK_BUDGET,
                                                  "requested_model": args.model,
                                                  "requested_effort": args.effort,
                                                  "discover_models": args.discover_models,
                                                  "error": str(error),
                                                  "no_retry": True, "credentials_in_receipt": False})
        if run is not None:
            run.safe_shutdown()
        print(f"FAIL acceptance-native: {error}", file=sys.stderr, flush=True)
        return 1
    except BaseException as error:
        if output is not None:
            write_json(output / "failure.json", {"gate": "acceptance-native", "status": "failed",
                                                  "provider": args.provider, "mode": args.mode,
                                                  "scenario": args.scenario, "task_budget": TASK_BUDGET,
                                                  "requested_model": args.model,
                                                  "requested_effort": args.effort,
                                                  "discover_models": args.discover_models,
                                                  "error": str(error),
                                                  "traceback": traceback.format_exc(), "no_retry": True,
                                                  "credentials_in_receipt": False})
        if run is not None:
            run.safe_shutdown()
        print(f"FAIL acceptance-native: unexpected {error}", file=sys.stderr, flush=True)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
