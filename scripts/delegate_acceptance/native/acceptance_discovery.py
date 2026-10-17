"""Composed native acceptance behavior: discovery."""

from __future__ import annotations


import hashlib
import os
import shutil
import time
from pathlib import Path
from scripts.delegate_acceptance.evidence.files import read_json, write_json
from scripts.delegate_acceptance.evidence.records import parse_json_output, status_jobs
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, _hex_digest, _task_identity, require
from scripts.delegate_acceptance.native.constants import NATIVE_DISCOVERY_ENVIRONMENT
from scripts.delegate_acceptance.native.errors import BlockedFailure
from scripts.delegate_acceptance.native.fixtures import bounded_text
from scripts.delegate_acceptance.native.provider_policy import model_discovery_exit_code, validate_model_discovery_response
from scripts.delegate_acceptance.processes.observation import inherited_environment
from scripts.delegate_acceptance.native.task_ops import ModelDiscoveryCleanupOps
from scripts.delegate_acceptance.native.workspace import has_git_ancestor, workspace_snapshot
from scripts.delegate_acceptance.supervisor.binding import observed_pueue_version
from scripts.delegate_acceptance.supervisor.configuration import pueue_command
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps
from scripts.delegate_acceptance.native.constants import CODEX_TIMEOUT_BASELINE_DIRECTORIES, MAX_CONTROL_BYTES, MODEL_DISCOVERY_TASK_BUDGET, MODEL_DISCOVERY_TASK_BUDGET_NANOS, MODEL_DISCOVERY_TIMEOUT_SECONDS, PROFILES


def native_environment() -> dict[str, str]:
    environment = inherited_environment()
    environment.update((key, os.environ[key]) for key in NATIVE_DISCOVERY_ENVIRONMENT
                       if key in os.environ)
    return environment


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


def _sync_model_cleanup(owner, ops: ModelDiscoveryCleanupOps) -> None:
    """Bind cleanup to the model inspection only after its identity exists."""
    acceptance = owner
    root_path = acceptance.models_state / "root.json"
    if not root_path.is_file() or root_path.is_symlink():
        return
    root = read_json(root_path)
    root_id = root.get("root_id")
    require(_task_identity(root_id), "model discovery root identity is invalid")
    acceptance.models_root_id = root_id
    ops.root_id = root_id

    inspections = acceptance.models_state / "inspections"
    if not inspections.is_dir() or inspections.is_symlink():
        return
    entries = list(inspections.iterdir())
    if not entries:
        return
    require(len(entries) == 1 and entries[0].is_dir() and not entries[0].is_symlink() and
            _task_identity(entries[0].name),
            "model discovery inspection journal coverage is not exact")
    task_id = entries[0].name
    if acceptance.model_discovery_task_id is None:
        acceptance.model_discovery_task_id = task_id
    require(acceptance.model_discovery_task_id == task_id,
            "model discovery task identity changed")
    ops.dispatch_attempts = {task_id}
    ops.inspection_attempts = {task_id}

def _prepare_codex_workspace(owner) -> None:
    acceptance = owner
    if acceptance.provider not in {"codex:exec", "codex:review"}:
        return
    if acceptance.scenario == "non-git":
        require(not has_git_ancestor(acceptance.workspace),
                "non-Git acceptance workspace acquired a Git repository")
        return
    require(acceptance.git is not None, "Codex git prerequisite is unavailable")
    process = acceptance.processes.run(
        "codex-git-init",
        [acceptance.git, "init", "--quiet", "--initial-branch=main", acceptance.workspace],
        acceptance.workspace.parent,
        timeout=20,
    )
    require((acceptance.workspace / ".git").is_dir() and not (acceptance.workspace / ".git").is_symlink() and
            process.result.get("exit_code") == 0,
            "Codex disposable workspace is not a git repository")
    if acceptance.provider == "codex:exec" and acceptance.scenario == "timeout-resume":
        for name in CODEX_TIMEOUT_BASELINE_DIRECTORIES:
            (acceptance.workspace / name).mkdir(mode=0o700)
    if acceptance.provider == "codex:review":
        for key, value in (("user.email", "delegation-acceptance@example.invalid"),
                           ("user.name", "Delegation Acceptance")):
            configured = acceptance.processes.run(
                "codex-review-git-config-" + key.replace(".", "-"),
                [acceptance.git, "-C", acceptance.workspace, "config", key, value],
                acceptance.workspace.parent, timeout=20)
            require(configured.result.get("exit_code") == 0,
                    "Codex review workspace Git identity could not be configured")
        staged = acceptance.processes.run(
            "codex-review-git-add",
            [acceptance.git, "-C", acceptance.workspace, "add", "acceptance-marker.txt"],
            acceptance.workspace.parent, timeout=20)
        require(staged.result.get("exit_code") == 0,
                "Codex review workspace target could not be staged")
        committed = acceptance.processes.run(
            "codex-review-git-commit",
            [acceptance.git, "-C", acceptance.workspace, "commit", "--quiet", "-m", "baseline"],
            acceptance.workspace.parent, timeout=20)
        require(committed.result.get("exit_code") == 0,
                "Codex review workspace baseline could not be committed")
        require(acceptance.workspace_marker.is_file() and not acceptance.workspace_marker.is_symlink(),
                "Codex review marker is not a regular file")
        with acceptance.workspace_marker.open("wb") as stream:
            stream.write(acceptance.marker_bytes + b"review-change\n")
            stream.flush()
            os.fsync(stream.fileno())

def discover_models(owner) -> None:
    """Record advisory model facts through the already-running supervisor."""
    acceptance = owner
    before = workspace_snapshot(acceptance.workspace)
    argv = acceptance.model_discovery_argv()
    process = acceptance.processes.run(
        "model-discovery", argv, acceptance.base,
        expected={0, 1, 2}, timeout=MODEL_DISCOVERY_TIMEOUT_SECONDS)
    response = parse_json_output(process, "model discovery")
    summary = validate_model_discovery_response(response, acceptance.provider)
    expected_exit = model_discovery_exit_code(str(response["status"]))
    require(process.result.get("exit_code") == expected_exit,
            "model discovery exit status does not match its status")
    acceptance.model_discovery = response
    acceptance.model_discovery_summary = summary
    acceptance.model_discovery_exit_code = expected_exit
    root_path = acceptance.models_state / "root.json"
    if root_path.is_file() and not root_path.is_symlink():
        root = read_json(root_path)
        root_id = root.get("root_id") if isinstance(root, dict) else None
        require(isinstance(root_id, str) and len(root_id) == 32 and
                all(character in "0123456789abcdef" for character in root_id),
                "model discovery root identity is invalid")
        acceptance.models_root_id = root_id
    write_json(acceptance.output / "models.json", response)
    require(workspace_snapshot(acceptance.workspace) == before,
            "model discovery changed the acceptance workspace")

def model_discovery_argv(owner) -> list[object]:
    acceptance = owner
    return [
        acceptance.delegate, "--root", acceptance.models_state,
        "--pueue-config", acceptance.config, "--runner", acceptance.runner,
        "models", "--provider", acceptance.provider, "--cwd", acceptance.workspace, "--json",
    ]

def _validate_model_discovery_evidence(owner, status: dict[str, object],
                                       model_group: str) -> None:
    """Validate the standalone inspection before excluding its queue row."""
    acceptance = owner
    require(acceptance.models_root_id is not None, "model discovery root identity is unavailable")
    if getattr(acceptance, "discover_models_only", True):
        require(acceptance.root_id is None and not acceptance.tasks and not acceptance.numeric_ids and
                not acceptance.ops.dispatch_attempts and not acceptance.ops.inspection_attempts,
                "model discovery admitted an ordinary provider task")
        require(not (acceptance.state / "tasks").exists() and
                not (acceptance.state / "inspections").exists(),
                "model discovery created ordinary provider state")
    model_tasks = acceptance.models_state / "tasks"
    require(not model_tasks.exists() and not model_tasks.is_symlink(),
            "model discovery created an ordinary task record")

    inspections = acceptance.models_state / "inspections"
    require(inspections.is_dir() and not inspections.is_symlink(),
            "model discovery inspection journal is absent")
    entries = list(inspections.iterdir())
    require(len(entries) == 1 and entries[0].is_dir() and not entries[0].is_symlink() and
            _task_identity(entries[0].name),
            "model discovery inspection journal coverage is not exact")
    task_id = entries[0].name
    if acceptance.model_discovery_task_id is None:
        acceptance.model_discovery_task_id = task_id
    require(acceptance.model_discovery_task_id == task_id,
            "model discovery task identity changed")
    validator = NativeTaskOps(acceptance.processes, acceptance.delegate, acceptance.runner, acceptance.pueue,
                              acceptance.models_state.parent, acceptance.models_state, acceptance.output)
    validator.bind_supervisor(acceptance.config, acceptance.daemon)
    validator.root_id = acceptance.models_root_id
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
            helper_executable == str(acceptance.provider_executable) and
            _hex_digest(binding.get("helper_sha256")),
            "model discovery provider binding changed")
    task = request["task"]
    discovery_mode = PROFILES[acceptance.provider]["modes"][0]
    require(task.get("provider") == acceptance.provider and
            task.get("mode") == discovery_mode and
            task.get("canonical_cwd") == str(acceptance.workspace) and
            task.get("requested_config") == {"permission": discovery_mode, "budget": MODEL_DISCOVERY_TASK_BUDGET} and
            task.get("budget_nanos") == MODEL_DISCOVERY_TASK_BUDGET_NANOS and
            task.get("brief_length") == len(b"model discovery") and
            task.get("brief_sha256") == hashlib.sha256(b"model discovery").hexdigest(),
            "model discovery request is not the bounded metadata operation")
    result = journal["result"]
    require(isinstance(result, dict), "model discovery has no completed result")
    if result.get("reason") == "eligible":
        require(isinstance(acceptance.model_discovery, dict), "model discovery response is absent")
        expected = {key: acceptance.model_discovery.get(key)
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

def setup(owner) -> None:
    acceptance = owner
    acceptance.require_authentication_prerequisite()
    pueue_version = acceptance.direct("pueue-version", pueue_command(acceptance.pueue, acceptance.config, "--version"), timeout=20)
    pueued_version = acceptance.direct("pueued-version", [acceptance.pueued, "-c", acceptance.config, "--version"], timeout=20)
    acceptance.pueue_version = observed_pueue_version(
        bounded_text(pueue_version.directory / "stdout", "pueue version output", MAX_CONTROL_BYTES), "pueue")
    acceptance.pueued_version = observed_pueue_version(
        bounded_text(pueued_version.directory / "stdout", "pueued version output", MAX_CONTROL_BYTES), "pueued")
    discovery = parse_json_output(
        acceptance.direct("discovery", [acceptance.delegate, "providers", "--json"], timeout=30), "provider discovery")
    providers = discovery.get("providers")
    require(isinstance(providers, list), "compiled discovery has no provider list")
    selected = next((item for item in providers
                     if isinstance(item, dict) and item.get("id") == acceptance.provider), None)
    require(isinstance(selected, dict), "selected provider is absent from compiled discovery")
    advertised = selected.get("supported_modes")
    require(isinstance(advertised, list) and acceptance.mode in advertised,
            f"compiled discovery does not advertise {acceptance.provider} {acceptance.mode}")
    acceptance._prepare_codex_workspace()
    acceptance.daemon = acceptance.processes.start("pueued", [acceptance.pueued, "-c", acceptance.config], acceptance.base,
                                       on_spawned=acceptance._adopt_daemon)
    acceptance.ops.bind_supervisor(acceptance.config, acceptance.daemon)
    deadline = time.monotonic() + 30
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            break
        process = acceptance.ops.client("daemon-status", ["status", "--json"], expected={0, 1},
                                  timeout=min(20, remaining))
        if process.result.get("exit_code") == 0:
            status = status_jobs(process)
            if not status.get("tasks"):
                acceptance.workspace_baseline = workspace_snapshot(acceptance.workspace)
                return
        delay = min(0.2, max(0.0, deadline - time.monotonic()))
        if delay <= 0:
            break
        time.sleep(delay)
    raise AcceptanceFailure("private pueue readiness observation expired")

def require_authentication_prerequisite(owner) -> None:
    """Turn an explicit native signed-out status into a neutral live gate."""
    acceptance = owner
    arguments = acceptance.profile.get("authentication_status")
    if arguments is None:
        return
    require(isinstance(arguments, tuple) and arguments and
            all(isinstance(value, str) and value for value in arguments),
            f"{acceptance.provider} authentication status command is invalid")
    process = acceptance.direct(
        "provider-authentication-status", [acceptance.provider_executable, *arguments],
        expected={0, 1}, timeout=20)
    response = parse_json_output(process, f"{acceptance.provider} authentication status")
    logged_in = response.get("loggedIn")
    require(type(logged_in) is bool,
            f"{acceptance.provider} authentication status has no loggedIn boolean")
    if not logged_in:
        raise BlockedFailure(f"{acceptance.provider} authentication is unavailable")
    require(process.result.get("exit_code") == 0,
            f"{acceptance.provider} authenticated status exited unsuccessfully")

def require_default_model_prerequisite(owner) -> None:
    """Gate Pi's default control on its native locally available catalog."""
    acceptance = owner
    if acceptance.provider != "pi:json" or getattr(acceptance.args, "model", None) is not None:
        return
    acceptance.discover_models()
    response = acceptance.model_discovery
    require(isinstance(response, dict), "Pi model discovery response is unavailable")
    status, reason = response.get("status"), response.get("reason_code")
    if status == "blocked":
        if reason == "authentication_unavailable":
            raise BlockedFailure("Pi model discovery authentication is unavailable")
        raise AcceptanceFailure(f"Pi default-model discovery is blocked: {reason}")
    if status == "unavailable" and reason == "models_unavailable":
        raise BlockedFailure("Pi has no locally available model after native authentication and filtering")
    require(status != "failed", f"Pi default-model discovery failed: {reason}")

def _adopt_daemon(owner, process: object) -> None:
    acceptance = owner
    require(acceptance.daemon is None, "private daemon handle was assigned twice")
    acceptance.daemon = process
