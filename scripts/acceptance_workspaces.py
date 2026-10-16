#!/usr/bin/env python3
"""Native managed-workspace acceptance through the shipped Delegate CLI.

This driver is deliberately fixture-only.  It uses the rebuilt Delegate and
runner supplied by the caller, plus the real pueue pair, while the provider is
the existing finite private fixture.  Process ownership, command capture,
lease observation, and directory rollback remain in the shared acceptance
helpers.
"""

from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shlex
import shutil
import sys
import tempfile
import threading
import time
import uuid

import acceptance_private_cli as private_cli
from acceptance_provider_common import (
    validate_managed_workspace_operation_closure,
    verify_collected_outcome,
)
from acceptance_supervisor_common import (
    DirectoryOwner,
    OwnedDirectory,
    Processes, Process,
    digest,
    inherited_environment,
    read_json,
    require,
    write_json,
)


REPOSITORY = Path(__file__).resolve().parent.parent
TASK_ID_RE = re.compile(r"^[0-9a-f]{32}$")
OID_RE = re.compile(r"^[0-9a-f]{40}([0-9a-f]{24})?$")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
MAX_RECEIPT_BYTES = 512 * 1024
MAX_RESPONSE_BYTES = 8 * 1024 * 1024
DEFAULT_WORKSPACE_BUDGET = "5m"
DEFAULT_DISPATCH_WORKERS = 16
# Cover the default five-minute helper plus control overhead; burst latency
# is measured, not an acceptance deadline.
ADMISSION_COMMAND_TIMEOUT_SECONDS = 6 * 60
DEFAULT_CAPACITY_TASKS = 200
SUCCESS_TASK_BUDGET = "30s"
# Native startup and runtime probing are inside the task budget. Leave enough
# time to observe the provider's live lease before proving original expiry.
TIMEOUT_TASK_BUDGET = "30s"
TIMEOUT_CONTINUE_TASK_BUDGET = "30s"
CAPACITY_TASK_BUDGET = "30m"
WORKSPACE_OPERATION_BUDGET = "2m"
WAIT_SECONDS = ADMISSION_COMMAND_TIMEOUT_SECONDS
TIMEOUT_WAIT_SECONDS = 45
FIXTURE_SESSION = "00000000-0000-4000-8000-000000000001"
FIXTURE_FILE = "managed-workspace-fixture.txt"


class AcceptanceBlocked(RuntimeError):
    """A native prerequisite was unavailable or inconclusive."""


class AcceptanceSetupFailure(RuntimeError):
    """Carry partial exact-directory ownership to finalization."""

    def __init__(self, cause: BaseException, owner: DirectoryOwner, root_token: OwnedDirectory):
        super().__init__(str(cause))
        self.cause = cause
        self.owner = owner
        self.root_token = root_token

    def cleanup(self) -> dict[str, object]:
        entries = self.owner.transfer()
        paths = [str(entry.directory) for entry in entries]
        try:
            private_cli.remove_private_acceptance_root(self.root_token, entries)
        except (OSError, RuntimeError) as error:
            return {
                "authorized": False,
                "decision": "setup-cleanup-failed-evidence-retained",
                "error": str(error),
                "retained_owned_directories": paths,
            }
        return {
            "authorized": True,
            "decision": "setup-failure-exact-root-removed",
            "removed_owned_directories": paths,
        }


def _compact(value: object, depth: int = 0) -> object:
    """Keep receipt values finite and remove accidental raw environment data."""
    if depth > 5:
        return "<depth-limit>"
    if isinstance(value, Path):
        return value.name
    if isinstance(value, bytes):
        return {"bytes": len(value), "sha256": hashlib.sha256(value).hexdigest()}
    if isinstance(value, str):
        if len(value) > 512:
            return value[:256] + "...<bounded>..." + value[-128:]
        return value
    if isinstance(value, dict):
        items = sorted(value.items(), key=lambda item: str(item[0]))[:256]
        return {str(key): _compact(item, depth + 1) for key, item in items}
    if isinstance(value, (list, tuple)):
        values = list(value)
        result = [_compact(item, depth + 1) for item in values[:4096]]
        if len(values) > 4096:
            result.append("<items-bounded>")
        return result
    if isinstance(value, (int, float, bool)) or value is None:
        return value
    return str(value)[:512]


def file_fingerprint(path: Path) -> dict[str, object]:
    candidate = Path(path)
    try:
        resolved = candidate.resolve(strict=True)
        info = resolved.stat()
        if not resolved.is_file():
            raise OSError("not a regular file")
        return {
            "name": resolved.name,
            "bytes": info.st_size,
            "sha256": hashlib.sha256(resolved.read_bytes()).hexdigest(),
        }
    except OSError:
        return {"name": candidate.name, "status": "unavailable"}


def _require_executable(path: Path, label: str) -> Path:
    try:
        resolved = path.resolve(strict=True)
    except OSError as error:
        raise AcceptanceBlocked(label + "-unavailable") from error
    if not resolved.is_file() or not os.access(resolved, os.X_OK):
        raise AcceptanceBlocked(label + "-not-executable")
    return resolved


def _status_value(response: dict[str, object]) -> str:
    for candidate in (
        response.get("status"),
        response.get("state"),
        (response.get("task") or {}).get("status")
        if isinstance(response.get("task"), dict) else None,
        (response.get("job") or {}).get("status")
        if isinstance(response.get("job"), dict) else None,
    ):
        if isinstance(candidate, str):
            return candidate.lower()
        if isinstance(candidate, dict):
            for key in ("state", "status", "result"):
                nested = candidate.get(key)
                if isinstance(nested, str):
                    return nested.lower()
    return ""


def classify_control_response(exit_code: int, response: dict[str, object]) -> str:
    """Classify the public exit-3 protocol without treating it as success."""
    workspace = response.get("workspace")
    if not isinstance(workspace, dict):
        return "ready" if exit_code == 0 else "failed"
    if workspace.get("reason_code") == "requested_action_not_authorized":
        if (exit_code == 3
                and workspace.get("next_action") in {"retry_exec", "retry_release", "retry_forget"}
                and workspace.get("operation_id") in {None, ""}
                and workspace.get("operation") is None):
            return "not-authorized"
        return "failed"
    pending = (
        workspace.get("next_action") == "watch_workspace"
        or workspace.get("state") in {"provisioning", "busy", "cleanup_pending", "forgetting"}
        or (isinstance(workspace.get("operation"), dict)
            and workspace["operation"].get("status") == "pending")
    )
    if exit_code == 3 and pending:
        return "pending"
    if exit_code == 0 and not pending:
        return "ready"
    return "failed"


def classify_task_verdict(
    task_status: dict[str, object],
    outcome: dict[str, object] | None,
    scenario: str,
    *,
    lifetime_closed: bool,
    capture_status: str | None = None,
) -> dict[str, object]:
    """Apply provider-authority predicates separately from workspace evidence."""
    verdict = outcome.get("verdict") if outcome is not None else None
    if not lifetime_closed:
        return {"verdict": "failed", "reason": "native-lifetime-not-closed"}
    if scenario == "success":
        if not isinstance(verdict, str):
            return {"verdict": "failed", "reason": "outcome-verdict-missing"}
        if verdict != "committed":
            return {"verdict": "failed", "reason": "provider-outcome-not-committed"}
        if capture_status != "captured":
            return {"verdict": "failed", "reason": "workspace-capture-not-captured"}
        return {"verdict": "passed", "reason": "provider-outcome-committed"}
    if scenario == "timeout":
        if _status_value(task_status) != "timed_out":
            return {"verdict": "failed", "reason": "task-was-not-authoritatively-timed-out"}
        if outcome is None:
            continuation = task_status.get("continuation")
            if not (isinstance(continuation, dict)
                    and continuation.get("resumable") is True
                    and continuation.get("mode") == "native"
                    and _task_id_matches_continuation(task_status, continuation)):
                return {"verdict": "failed", "reason": "timeout-continuation-not-proven"}
        elif verdict not in {"committed", "rejected"}:
            return {"verdict": "failed", "reason": "provider-outcome-unsealed"}
        if capture_status != "captured":
            return {"verdict": "failed", "reason": "timeout-capture-not-captured"}
        return {
            "verdict": "passed",
            "reason": ("native-timeout-and-capture-proven-without-provider-publication"
                       if outcome is None else
                       "provider-outcome-retained-and-timeout-separated-from-capture"),
        }
    return {"verdict": "failed", "reason": "unknown-task-scenario"}


def _task_id_matches_continuation(
    status: dict[str, object], continuation: dict[str, object],
) -> bool:
    task_id = status.get("task_id")
    return (isinstance(task_id, str) and TASK_ID_RE.fullmatch(task_id) is not None
            and continuation.get("predecessor_task_id") == task_id)


def cleanup_decision(
    verdict: str,
    *,
    lifetime_closed: bool,
    daemon_closed: bool,
    task_count: int,
    pending_process_count: int,
) -> tuple[bool, str]:
    """Return whether exact owned-directory cleanup is authorized."""
    if pending_process_count:
        return False, "process-observation-pending"
    if verdict == "passed" and lifetime_closed and daemon_closed:
        return True, "passed-with-closed-managed-lifetimes"
    if verdict == "BLOCKED" and task_count == 0 and daemon_closed:
        return True, "blocked-before-task-or-daemon"
    if not lifetime_closed:
        return False, "managed-lifetime-closure-unproven"
    if not daemon_closed:
        return False, "private-daemon-closure-unproven"
    return False, "failure-evidence-retained"


def _validate_hex(value: object, pattern: re.Pattern[str], label: str) -> str:
    require(isinstance(value, str) and pattern.fullmatch(value) is not None,
            label + " is invalid")
    return value


def verify_capture_receipt(state: Path, capture: dict[str, object]) -> dict[str, object]:
    """Verify capture digests and artifact paths against retained state evidence."""
    require(capture.get("status") == "captured", "workspace capture is not captured")
    workspace_id = _validate_hex(capture.get("workspace_id"), TASK_ID_RE, "capture workspace ID")
    operation_id = _validate_hex(capture.get("operation_id"), TASK_ID_RE, "capture operation ID")
    _validate_hex(capture.get("termination_sha256"), SHA256_RE,
                  "capture termination_sha256")
    previous_boot = capture.get("lifetime_closure") == "legacy-previous-boot"
    if previous_boot:
        require(not capture.get("worker_wait_sha256") and not capture.get("unlaunched_sha256"),
                "previous-boot capture has mixed closure facts")
    else:
        _validate_hex(capture.get("worker_wait_sha256"), SHA256_RE,
                      "capture worker_wait_sha256")
    candidate = capture.get("candidate")
    require(isinstance(candidate, dict), "captured workspace omitted its candidate")
    require(candidate.get("workspace_id") == workspace_id, "capture candidate workspace mismatch")
    require(candidate.get("operation_id") == operation_id, "capture candidate operation mismatch")
    subject = candidate.get("subject")
    require(isinstance(subject, dict), "capture candidate omitted subject")
    _validate_hex(subject.get("id"), TASK_ID_RE, "capture subject ID")
    _validate_hex(subject.get("sha256"), SHA256_RE, "capture subject SHA256")
    base_oid = _validate_hex(candidate.get("base_oid"), OID_RE, "capture base OID")
    observed_head = _validate_hex(candidate.get("observed_head"), OID_RE, "capture observed HEAD")
    tree_oid = _validate_hex(candidate.get("tree_oid"), OID_RE, "capture tree OID")
    snapshot_ref = candidate.get("snapshot_ref")
    expected_ref = re.compile(
        r"^refs/delegate/([0-9a-f]{32})/workspaces/"
        + re.escape(workspace_id) + r"/captures/" + re.escape(operation_id) + r"$"
    )
    ref_match = expected_ref.fullmatch(snapshot_ref) if isinstance(snapshot_ref, str) else None
    require(
        isinstance(snapshot_ref, str)
        and ref_match is not None
        and "\x00" not in snapshot_ref
        and len(snapshot_ref) <= 256,
        "capture snapshot ref is not the exact owned workspace ref",
    )
    root_record_path = Path(state) / "root.json"
    if root_record_path.is_file() and not root_record_path.is_symlink():
        root_record = read_json(root_record_path)
        require(root_record.get("root_id") == ref_match.group(1),
                "capture snapshot ref root identity changed")
    if previous_boot:
        operation_path = Path(state) / "workspace-operations" / operation_id
        closure = validate_managed_workspace_operation_closure(operation_path, ref_match.group(1))
        native = closure.get("managed_lifetime")
        require(isinstance(native, dict), "previous-boot capture has no native closure")
        termination = native.get("termination")
        require(isinstance(termination, dict) and termination.get("cause") == "previous-boot" and
                digest(operation_path / "lifetime-termination.json") == capture["termination_sha256"],
                "previous-boot capture termination does not match retained closure")
    artifacts = capture.get("artifacts")
    require(isinstance(artifacts, list), "capture artifact inventory is not a list")
    verified: list[dict[str, object]] = []
    artifact_root = Path(state) / "workspace-operations" / operation_id / "artifacts"
    for artifact in artifacts:
        require(isinstance(artifact, dict), "capture artifact is not an object")
        name = artifact.get("name")
        require(
            isinstance(name, str) and name in {"diff.patch", "name-status.z"},
            "capture artifact name is not allowlisted",
        )
        omitted = artifact.get("omitted", "")
        if omitted:
            require(isinstance(omitted, str), "capture omitted reason is invalid")
            verified.append({"name": name, "omitted": omitted})
            continue
        length = artifact.get("length")
        require(type(length) is int and length >= 0, "capture artifact length is invalid")
        sha256 = _validate_hex(artifact.get("sha256"), SHA256_RE, "capture artifact SHA256")
        path = artifact_root / name
        require(path.parent == artifact_root and path.is_file() and not path.is_symlink(),
                "capture artifact path is not a retained regular file")
        data = path.read_bytes()
        require(len(data) == length and digest(path) == sha256,
                "capture artifact digest does not match retained bytes")
        verified.append({"name": name, "length": length, "sha256": sha256})
    require(any(item.get("name") == "diff.patch" and item.get("length", 0) > 0
                for item in verified),
            "capture did not retain a nonempty patch")
    return {
        "workspace_id": workspace_id,
        "operation_id": operation_id,
        "base_oid": base_oid,
        "observed_head": observed_head,
        "tree_oid": tree_oid,
        "subject": {
            "kind": subject.get("kind"),
            "id": subject.get("id"),
            "sha256": subject.get("sha256"),
        },
        "root_id": ref_match.group(1),
        "snapshot_ref": snapshot_ref,
        "artifacts": verified,
    }


class ReceiptFinalizer:
    """Write one bounded receipt before any owned-directory cleanup."""

    def __init__(self, output: Path, argv: list[str], binaries: dict[str, Path]):
        self.output = Path(output)
        self.argv = list(argv)
        self.binaries = dict(binaries)
        self.started = time.time_ns()
        self.source_manifest: dict[str, object] = {"status": "not-collected"}
        self.details: dict[str, object] = {}
        self.write_count = 0
        self.tool_fingerprints = {
            name: file_fingerprint(path) for name, path in sorted(self.binaries.items())
        }

    def refresh_tool_fingerprints(self) -> None:
        self.tool_fingerprints = {
            name: file_fingerprint(path) for name, path in sorted(self.binaries.items())
        }

    def set_source_manifest(self, manifest: dict[str, object]) -> None:
        self.source_manifest = manifest

    def set_details(self, **details: object) -> None:
        self.details.update(details)

    def _payload(
        self,
        verdict: str,
        reason: str,
        cleanup: dict[str, object],
    ) -> dict[str, object]:
        platform_evidence = {
            "system": platform.system(),
            "kernel": platform.release(),
            "architecture": platform.machine(),
            "python": file_fingerprint(Path(sys.executable)),
        }
        payload = {
            "schema_version": 1,
            "claim_class": "native-fixture-only",
            "verdict": verdict,
            "reason": reason,
            "started_unix_ns": self.started,
            "finished_unix_ns": time.time_ns(),
            "invocation": self.argv,
            "platform": platform_evidence,
            "tools": self.tool_fingerprints,
            "source_manifest": self.source_manifest,
            "details": self.details,
            "cleanup": cleanup,
            "assurance": "native fixture lifecycle only; no live provider claim",
        }
        encoded = json.dumps(_compact(payload), sort_keys=True, separators=(",", ":")).encode()
        require(len(encoded) <= MAX_RECEIPT_BYTES, "compact receipt exceeds bound")
        return _compact(payload)

    def finalize(self, verdict: str, reason: str, cleanup: dict[str, object]) -> None:
        write_json(self.output, self._payload(verdict, reason, cleanup), replace=True)
        self.write_count += 1


@dataclass
class WorkspaceContext:
    owner: DirectoryOwner
    root_token: OwnedDirectory
    base: Path
    state: Path
    source: Path
    brief_success: Path
    brief_success_reuse: Path
    brief_timeout: Path
    brief_timeout_resume: Path
    brief_capacity: Path
    delegate: Path
    pueue: Path
    pueued: Path
    private_config: Path
    gate_path: Path
    waiter_directory: Path
    fixture_session_marker: Path
    response_directory: Path
    capture_root: OwnedDirectory
    processes: Processes
    environment: dict[str, str]
    source_baseline: dict[str, str]
    lease_nonce: str
    workspace_budget: str
    task_ids: list[str] = field(default_factory=list)
    attempted_task_ids: list[str] = field(default_factory=list)
    workspace_ids: list[str] = field(default_factory=list)
    container_parent_token: OwnedDirectory | None = None
    container_token: OwnedDirectory | None = None
    gates: list[private_cli.LockedReleaseGate] = field(default_factory=list)
    cohorts: list[private_cli.LiveFIFOLeaseCohort] = field(default_factory=list)
    native_proofs: list[dict[str, object]] = field(default_factory=list)
    command_count: int = 0
    command_lock: threading.Lock = field(default_factory=threading.Lock)
    cleanup_done: bool = False
    retained_entries: tuple[OwnedDirectory, ...] = ()

    def command_receipt(self, task_id: str | None, label: str) -> Path:
        safe_task = task_id if task_id and TASK_ID_RE.fullmatch(task_id) else "control"
        with self.command_lock:
            directory = self.response_directory / safe_task
            directory.mkdir(mode=0o700, exist_ok=True)
            index = self.command_count
            self.command_count += 1
        return directory / ("%04d-" % index + label + ".json")

    def add_task(self, task_id: str) -> None:
        require(TASK_ID_RE.fullmatch(task_id) is not None, "task ID is not canonical")
        with self.command_lock:
            if task_id not in self.task_ids:
                self.task_ids.append(task_id)

    def add_workspace(self, workspace_id: str) -> None:
        require(TASK_ID_RE.fullmatch(workspace_id) is not None, "workspace ID is not canonical")
        with self.command_lock:
            if workspace_id not in self.workspace_ids:
                self.workspace_ids.append(workspace_id)

    def release_gates(self) -> None:
        for gate in tuple(self.gates):
            gate.release()
        self.gates.clear()

    def close_cohorts(self, remove_closed: bool) -> list[dict[str, object]]:
        evidence = []
        for cohort in tuple(self.cohorts):
            try:
                if remove_closed:
                    evidence.extend(cohort.remove_closed_fifos())
                cohort.close()
            except (OSError, RuntimeError) as error:
                evidence.append({"error": str(error)})
        self.cohorts.clear()
        return evidence

    def cleanup(self, verdict: str) -> dict[str, object]:
        """Release only fixture gates, then authorize exact cleanup if safe."""
        self.release_gates()
        cohort_evidence = self.close_cohorts(remove_closed=False)
        lifetime_closed = False
        lifetime_error = None
        daemon_closed = not self.private_config.exists()
        daemon_error = None
        if self.state.exists():
            try:
                private_cli.authorize_managed_lifetime_closure(
                    self.state, tuple(self.task_ids),
                )
                lifetime_closed = True
            except (RuntimeError, OSError) as error:
                lifetime_error = str(error)
        elif not self.attempted_task_ids:
            lifetime_closed = True
        if lifetime_closed and self.private_config.exists():
            try:
                private_cli.shutdown_private_supervisor(
                    self.pueue,
                    self.private_config,
                    self.environment,
                    state=self.state,
                    task_ids=tuple(self.task_ids),
                    capture_root=self.root_token,
                )
                daemon_closed = True
            except (RuntimeError, OSError) as error:
                daemon_closed = False
                daemon_error = str(error)
        process_pending = self.processes.pending()
        can_cleanup, decision = cleanup_decision(
            verdict,
            lifetime_closed=lifetime_closed,
            daemon_closed=daemon_closed,
            task_count=len(self.task_ids),
            pending_process_count=len(process_pending),
        )
        result: dict[str, object] = {
            "decision": decision,
            "authorized": can_cleanup,
            "lifetime_closed": lifetime_closed,
            "daemon_closed": daemon_closed,
            "pending_processes": process_pending,
            "cohort_closure": cohort_evidence,
        }
        if lifetime_error:
            result["lifetime_error"] = lifetime_error
        if daemon_error:
            result["daemon_error"] = daemon_error
        if not can_cleanup:
            entries = self.owner.transfer()
            self.retained_entries = entries
            result["retained_owned_directories"] = [str(entry.directory) for entry in entries]
            return result
        try:
            entries = self.owner.transfer()
            private_cli.remove_private_acceptance_root(
                self.root_token,
                entries,
                managed_state_roots=((self.state, tuple(self.task_ids)),)
                if self.state.exists() else (),
            )
            self.cleanup_done = True
            result["removed_owned_directories"] = [str(entry.directory) for entry in entries]
            return result
        except (RuntimeError, OSError) as error:
            result["authorized"] = False
            result["decision"] = "cleanup-failed-evidence-retained"
            result["error"] = str(error)
            return result


def _git_executable() -> Path:
    found = shutil.which("git")
    if not found:
        raise AcceptanceBlocked("git-unavailable")
    return _require_executable(Path(found), "git")


def _git_process(
    context: WorkspaceContext,
    arguments: list[str],
    cwd: Path,
    *,
    label: str,
    expected: tuple[int, ...] = (0,),
    timeout: float = 30,
) -> Process:
    return context.processes.run(
        "git-" + label,
        [_git_executable(), *arguments],
        cwd,
        expected=expected,
        timeout=timeout,
    )


def _git(
    context: WorkspaceContext,
    arguments: list[str],
    cwd: Path,
    *,
    label: str,
    expected: tuple[int, ...] = (0,),
    timeout: float = 30,
) -> tuple[int, bytes]:
    process = _git_process(context, arguments, cwd, label=label, expected=expected, timeout=timeout)
    return process.result["exit_code"], process.output(MAX_RESPONSE_BYTES)


def _git_ok(context: WorkspaceContext, arguments: list[str], cwd: Path, *, label: str) -> bytes:
    code, output = _git(context, arguments, cwd, label=label)
    require(code == 0, "git command returned unexpected exit: " + label)
    return output


def _prepare_source(context: WorkspaceContext) -> None:
    source = context.source
    (source / "tracked.txt").write_text("managed workspace source\n", encoding="utf-8")
    (source / "README.md").write_text("managed workspace acceptance source\n", encoding="utf-8")
    _git_ok(context, ["init", "--quiet", "--initial-branch=main"], source, label="init")
    _git_ok(context, ["config", "user.name", "Acceptance Fixture"], source, label="config-name")
    _git_ok(context, ["config", "user.email", "fixture@example.invalid"], source, label="config-email")
    _git_ok(context, ["add", "--", "README.md", "tracked.txt"], source, label="add")
    _git_ok(context, ["commit", "--quiet", "-m", "fixture base"], source, label="commit")
    head = _git_ok(context, ["rev-parse", "--verify", "HEAD^{commit}"], source, label="baseline-head")
    tree = _git_ok(context, ["rev-parse", "--verify", "HEAD^{tree}"], source, label="baseline-tree")
    status = _git_ok(context, ["status", "--porcelain=v1", "--untracked-files=all"], source, label="baseline-status")
    require(not status.strip(), "fixture source is not clean after baseline commit")
    context.source_baseline = {
        "head": head.decode("ascii").strip(),
        "tree": tree.decode("ascii").strip(),
        "status_sha256": hashlib.sha256(status).hexdigest(),
    }


def _assert_source_untouched(context: WorkspaceContext) -> dict[str, object]:
    head = _git_ok(context, ["rev-parse", "--verify", "HEAD^{commit}"], context.source, label="source-final-head")
    tree = _git_ok(context, ["rev-parse", "--verify", "HEAD^{tree}"], context.source, label="source-final-tree")
    status = _git_ok(context, ["status", "--porcelain=v1", "--untracked-files=all"], context.source, label="source-final-status")
    observed = {
        "head": head.decode("ascii").strip(),
        "tree": tree.decode("ascii").strip(),
        "status_sha256": hashlib.sha256(status).hexdigest(),
        "status_empty": not status.strip(),
    }
    require(observed["head"] == context.source_baseline["head"], "source HEAD changed")
    require(observed["tree"] == context.source_baseline["tree"], "source tree changed")
    require(observed["status_empty"] is True, "source worktree is dirty")
    return observed


def _git_input_paths(context: WorkspaceContext) -> list[str]:
    output = _git_ok(
        context,
        ["-C", str(REPOSITORY), "ls-files", "--cached", "--others",
         "--exclude-standard", "-z"],
        REPOSITORY,
        label="source-manifest-paths",
    )
    names = output.decode("utf-8").split("\x00")
    paths = [name for name in names if name]
    require(len(paths) <= 8192, "source manifest exceeds entry bound")
    return paths


def build_source_manifest(context: WorkspaceContext) -> dict[str, object]:
    files: list[dict[str, object]] = []
    for name in _git_input_paths(context):
        path = REPOSITORY / name
        require(path.is_file() and not path.is_symlink(), "source manifest input is not a regular file: " + name)
        data = path.read_bytes()
        files.append({"path": name, "bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()})
    manifest = {
        "status": "complete",
        "root": REPOSITORY.name,
        "tracked_and_new_input_count": len(files),
        "files": files,
    }
    manifest_bytes = json.dumps(manifest, sort_keys=True, separators=(",", ":")).encode()
    manifest["manifest_sha256"] = hashlib.sha256(manifest_bytes).hexdigest()
    return manifest


def _install_workspace_fixture(
    provider_bin: Path,
    gate: Path,
    waiters: Path,
    nonce: str,
    session_marker: Path,
) -> None:
    private_cli.install_pi_fixture(provider_bin, gate, waiters, nonce)
    fixture = provider_bin / "pi"
    original = fixture.read_text(encoding="utf-8")
    marker = "prompt=$(/bin/cat)\n"
    require(original.count(marker) == 1, "private fixture shape changed")
    session_line = '{"type":"session","id":"' + FIXTURE_SESSION + '"}\n'
    require(original.count(session_line) == 1, "private fixture session event shape changed")
    original = original.replace(session_line, "", 1)
    wait_program = private_cli.PI_WAIT_PROGRAM
    wait_session_line = '{"type":"session","id":"' + FIXTURE_SESSION + '"}\\n'
    require(wait_program.count(wait_session_line) == 1,
            "private fixture wait session event shape changed")
    original_wait_program = shlex.quote(wait_program)
    fixture_wait_program = shlex.quote(wait_program.replace(wait_session_line, "", 1))
    require(original.count(original_wait_program) == 1,
            "private fixture wait program shape changed")
    original = original.replace(original_wait_program, fixture_wait_program, 1)
    transformation = r'''prompt=$(/bin/cat)
session_id=__FIXTURE_SESSION_JSON__
session_marker=__FIXTURE_SESSION_MARKER_JSON__
requested_session=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --session)
      if [ "$#" -lt 2 ]; then
        printf '%s\n' 'fixture continuation omitted --session value' >&2
        exit 91
      fi
      requested_session=$2
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
if [ -n "$requested_session" ] && [ "$requested_session" != "$session_id" ]; then
  printf '%s\n' 'fixture continuation session mismatch' >&2
  exit 92
fi
resumed=false
if [ -n "$requested_session" ]; then
  resumed=true
fi
printf '{"schema_version":1,"session":"00000000-0000-4000-8000-000000000001","resumed":%s}\n' "$resumed" > "$session_marker"
printf '%s\n' '{"type":"session","id":"00000000-0000-4000-8000-000000000001"}'
case "$prompt" in
  *DELEGATE_WORKSPACE_TIMEOUT_RESUME*)
    printf '%s\n' 'fixture workspace timeout continuation' > managed-workspace-timeout-resumed.txt
    git add -N -- managed-workspace-timeout-resumed.txt
    ;;
  *DELEGATE_WORKSPACE_REUSE_FIXTURE*)
    printf '%s\n' 'fixture workspace reused edit' > managed-workspace-reused.txt
    git add -N -- managed-workspace-reused.txt
    ;;
  *DELEGATE_WORKSPACE_FIXTURE*|*DELEGATE_WORKSPACE_TIMEOUT*)
    printf '%s\n' 'fixture workspace edit' > managed-workspace-fixture.txt
    git add -N -- managed-workspace-fixture.txt
    ;;
esac
'''
    transformation = transformation.replace(
        "__FIXTURE_SESSION_JSON__", shlex.quote(FIXTURE_SESSION),
    ).replace(
        "__FIXTURE_SESSION_MARKER_JSON__", shlex.quote(str(session_marker)),
    )
    fixture.write_text(original.replace(marker, transformation, 1), encoding="utf-8")
    fixture.chmod(0o700)


def _write_briefs(context: WorkspaceContext) -> None:
    context.brief_success.write_text(
        "DELEGATE_WORKSPACE_FIXTURE\nReply with the fixture acceptance sentence.\n",
        encoding="utf-8",
    )
    context.brief_success_reuse.write_text(
        "DELEGATE_WORKSPACE_REUSE_FIXTURE\nReply with the fixture reuse sentence.\n",
        encoding="utf-8",
    )
    context.brief_timeout.write_text(
        "DELEGATE_WORKSPACE_TIMEOUT\nDELEGATE_PRIVATE_UPGRADE_WAIT\n",
        encoding="utf-8",
    )
    context.brief_timeout_resume.write_text(
        "DELEGATE_WORKSPACE_TIMEOUT_RESUME\nReply with the fixture continuation sentence.\n",
        encoding="utf-8",
    )
    context.brief_capacity.write_text(
        "DELEGATE_WORKSPACE_FIXTURE\nDELEGATE_PRIVATE_UPGRADE_WAIT\n",
        encoding="utf-8",
    )
    for brief in (
        context.brief_success,
        context.brief_success_reuse,
        context.brief_timeout,
        context.brief_timeout_resume,
        context.brief_capacity,
    ):
        brief.chmod(0o600)


def _control(
    context: WorkspaceContext,
    args: list[str],
    *,
    label: str,
    task_id: str | None = None,
    expected: tuple[int, ...] = (0,),
    timeout: float = 45,
    phase: str | None = None,
) -> tuple[int, dict[str, object]]:
    receipt = context.command_receipt(task_id, label)
    response = private_cli.run_json(
        context.delegate,
        context.environment,
        args,
        expected=expected,
        timeout=timeout,
        receipt=receipt,
        phase=phase,
        capture_root=context.capture_root,
        capture_response=True,
    )
    try:
        evidence = read_json(receipt)
    except (OSError, ValueError) as error:
        raise RuntimeError("shared control helper did not retain command response") from error
    code = evidence.get("exit_code")
    require(type(code) is int, "shared control helper omitted command exit")
    return code, response


def _workspace_from(response: dict[str, object]) -> dict[str, object]:
    workspace = response.get("workspace")
    require(isinstance(workspace, dict), "response omitted additive workspace block")
    return workspace


def _workspace_id(response: dict[str, object]) -> str:
    workspace = _workspace_from(response)
    return _validate_hex(workspace.get("id"), TASK_ID_RE, "workspace ID")


def _operation_id(response: dict[str, object]) -> str | None:
    workspace = response.get("workspace")
    if not isinstance(workspace, dict):
        return None
    operation = workspace.get("operation_id")
    if operation is None:
        operation_result = workspace.get("operation")
        if isinstance(operation_result, dict):
            operation = operation_result.get("operation_id")
    if operation is None:
        return None
    return _validate_hex(operation, TASK_ID_RE, "workspace operation ID")


def _wait_workspace(
    context: WorkspaceContext,
    workspace_id: str,
    *,
    operation_id: str | None = None,
    state: str | None = None,
    timeout: float = WAIT_SECONDS,
    task_id: str | None = None,
) -> dict[str, object]:
    deadline = time.monotonic() + timeout
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("workspace observation expired: " + workspace_id)
        args = [
            "--root", str(context.state), "workspace", "status", workspace_id,
            "--watch", str(min(5.0, remaining)) + "s", "--json",
        ]
        if operation_id is not None:
            args.extend(["--operation", operation_id])
        _code, response = _control(
            context, args, label="workspace-status", task_id=task_id,
            # Observation can finish an accepted scheduling transaction. Its
            # fixed private-control cutoff is independent of --watch; use the
            # same finite transport budget as admission rather than aborting
            # that transaction after the short observation window.
            expected=(0, 3), timeout=ADMISSION_COMMAND_TIMEOUT_SECONDS,
            phase="workspace-observation",
        )
        workspace = _workspace_from(response)
        if workspace.get("state") in {"unreadable", "failed"}:
            raise RuntimeError("workspace entered terminal failure: " + repr(_compact(workspace)))
        if state is None:
            done = workspace.get("path") and workspace.get("state") in {"retained", "busy"}
        else:
            done = workspace.get("state") == state
        if operation_id is not None:
            result = workspace.get("operation")
            if isinstance(result, dict):
                result_status = result.get("status")
                if result_status == "failed":
                    raise RuntimeError("workspace operation failed: " + repr(_compact(result)))
                if result_status == "succeeded" and (state is None or done):
                    return response
        if done and operation_id is None:
            return response
        if classify_control_response(_code, response) == "failed":
            raise RuntimeError("workspace observation failed: " + repr(_compact(response)))


def _adopt_workspace_container(context: WorkspaceContext, root_id: str, workspace_id: str) -> Path:
    _validate_hex(root_id, TASK_ID_RE, "workspace root ID")
    expected_parent = context.state.parent / ("." + context.state.name + ".workspaces")
    if context.container_parent_token is None:
        if not expected_parent.is_dir() or expected_parent.is_symlink():
            raise RuntimeError("managed workspace container parent was not created canonically")
        context.container_parent_token = context.owner.adopt_created(
            "managed workspace container parent", expected_parent, expected_parent.parent,
        )
    expected = expected_parent / root_id
    if context.container_token is None:
        if not expected.is_dir() or expected.is_symlink():
            raise RuntimeError("managed workspace root container was not provisioned canonically")
        context.container_token = context.owner.adopt_created(
            "managed workspace root container", expected, expected_parent,
        )
    else:
        require(context.container_token.directory == expected, "workspace root container changed")
        context.container_token.assert_live()
    request = read_json(context.state / "workspaces" / workspace_id / "request.json")
    root_record = read_json(context.state / "root.json")
    require(root_record.get("root_id") == root_id, "state root identity changed")
    require(request.get("root_id") == root_id, "workspace request root identity changed")
    require(request.get("container") == str(expected), "workspace request container identity changed")
    anchor = expected / workspace_id
    require(anchor.is_dir() and not anchor.is_symlink(), "workspace anchor is not canonical")
    marker = anchor / ".delegate-workspace"
    require(marker.is_symlink() and os.readlink(marker) == str(context.state / "workspaces" / workspace_id),
            "workspace anchor marker is not exact")
    return expected


def _admit_created_task(
    context: WorkspaceContext,
    task_id: str,
    response: dict[str, object],
) -> dict[str, object]:
    """Apply one admission protocol to dispatch and exact continuation."""
    if response.get("task_id") != task_id:
        raise RuntimeError("admission response task identity changed")
    if response.get("task_record") != "created":
        raise RuntimeError("admission did not durably create task: " + repr(_compact(response)))
    context.add_task(task_id)
    _control(
        context, ["--root", str(context.state), "status", task_id, "--json"],
        label="first-status", task_id=task_id, expected=(0, 3),
        phase="first-created-task-observation",
    )
    failure = response.get("failure")
    if isinstance(failure, dict) and failure.get("code") == "task_submission_uncertain":
        receipt = context.command_receipt(task_id, "admission-observation")
        response = private_cli.observe_created_admission(
            context.delegate, context.state, context.environment, response, receipt,
            timeout=WAIT_SECONDS, capture_root=context.capture_root,
            command_prefix=(),
        )
        write_json(receipt.with_name(receipt.stem + "-response.json"), {
            "arguments": ["status", task_id, "--json"],
            "exit_code": 0,
            "response": response,
        })
    if response.get("admission") not in {"admitted", None}:
        raise RuntimeError("task admission failed: " + repr(_compact(response)))
    return response


def _dispatch(
    context: WorkspaceContext,
    brief: Path,
    *,
    task_budget: str,
    fresh: bool,
    workspace_id: str | None = None,
) -> dict[str, object]:
    task_id = uuid.uuid4().hex
    with context.command_lock:
        context.attempted_task_ids.append(task_id)
    args = [
        "--root", str(context.state), "dispatch",
        "--provider", "pi:json", "--brief", str(brief),
        "--id", task_id, "--permission", "workspace-write",
        "--budget", task_budget, "--workspace-budget", context.workspace_budget,
        "--json",
    ]
    if fresh:
        args.extend([
            "--cwd", str(context.source), "--isolation", "git-worktree",
            "--base", context.source_baseline["head"],
        ])
    else:
        require(workspace_id is not None, "workspace reuse dispatch lacks ID")
        args.extend(["--workspace", workspace_id])
    code, response = _control(
        context, args, label="dispatch", task_id=task_id, expected=(0, 1, 2, 3),
        timeout=ADMISSION_COMMAND_TIMEOUT_SECONDS, phase="workspace-dispatch",
    )
    response = _admit_created_task(context, task_id, response)
    workspace_id = _workspace_id(response)
    context.add_workspace(workspace_id)
    root_id = response.get("root_id")
    require(isinstance(root_id, str), "dispatch omitted state root identity")
    return {
        "task_id": task_id,
        "workspace_id": workspace_id,
        "root_id": root_id,
        "dispatch": response,
        "workspace": response,
    }


def _continue(
    context: WorkspaceContext,
    predecessor: dict[str, object],
    *,
    brief: Path,
    task_budget: str,
) -> dict[str, object]:
    predecessor_id = _validate_hex(
        predecessor.get("task_id"), TASK_ID_RE, "continuation predecessor task ID",
    )
    predecessor_root_id = _validate_hex(
        predecessor.get("root_id"), TASK_ID_RE, "continuation predecessor root ID",
    )
    args = [
        "--root", str(context.state), "continue", "--task", predecessor_id,
        "--brief", str(brief), "--budget", task_budget, "--json",
    ]
    _code, response = _control(
        context, args, label="continue", expected=(0, 1, 2, 3),
        timeout=ADMISSION_COMMAND_TIMEOUT_SECONDS, phase="workspace-continuation",
    )
    task_id = _validate_hex(response.get("task_id"), TASK_ID_RE, "returned successor task ID")
    with context.command_lock:
        context.attempted_task_ids.append(task_id)
    response = _admit_created_task(context, task_id, response)
    require(response.get("parent_task_id") == predecessor_id,
            "continuation response omitted exact predecessor")
    workspace_id = _workspace_id(response)
    context.add_workspace(workspace_id)
    root_id = response.get("root_id")
    require(isinstance(root_id, str), "continuation omitted state root identity")
    require(root_id == predecessor_root_id, "continuation changed state root identity")
    return {
        "task_id": task_id,
        "workspace_id": workspace_id,
        "root_id": root_id,
        "parent_task_id": predecessor_id,
        "continue": response,
        "workspace": response,
    }


def _await_task(context: WorkspaceContext, task: dict[str, object], timeout: float) -> dict[str, object]:
    # A completed queue row may already have been reclaimed. Wait for the
    # validated provider publication; workspace capture and the canonical
    # root-level lifetime validator separately prove native closure below.
    return _collect_task(context, task, timeout)


def _collect_task(context: WorkspaceContext, task: dict[str, object], timeout: float) -> dict[str, object]:
    task_id = task["task_id"]
    _code, response = _control(
        context,
        ["--root", str(context.state), "collect", task_id, "--watch", str(timeout) + "s", "--json"],
        label="collect", task_id=task_id, expected=(0, 4),
        timeout=timeout + 10, phase="task-collection",
    )
    outcome = response.get("outcome")
    require(isinstance(outcome, dict), "collection omitted provider outcome")
    task_directory = context.state / "tasks" / task_id
    stored = read_json(task_directory / "outcome.json")
    require(stored == outcome, "collection is not bound to sole outcome authority")
    verify_collected_outcome(response, task_directory, stored["verdict"])
    return response


def _prove_lifetimes_closed(context: WorkspaceContext, task_ids: list[str]) -> dict[str, object]:
    try:
        proof = private_cli.authorize_managed_lifetime_closure(
            context.state, tuple(task_ids),
        )
    except (OSError, RuntimeError) as error:
        raise RuntimeError("managed task lifetime closure was not proven") from error
    require(isinstance(proof, dict), "managed lifetime closure proof is not an object")
    return proof


def _wait_capture(context: WorkspaceContext, task: dict[str, object]) -> tuple[dict[str, object], dict[str, object]]:
    workspace_id = task["workspace_id"]
    deadline = time.monotonic() + WAIT_SECONDS
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("workspace capture observation expired: " + workspace_id)
        response = _wait_workspace(
            context, workspace_id, timeout=min(15, remaining), task_id=task["task_id"],
        )
        workspace = _workspace_from(response)
        capture = workspace.get("capture")
        if isinstance(capture, dict):
            if capture.get("status") == "captured":
                return response, capture
            if capture.get("status") == "unavailable":
                raise RuntimeError("workspace capture became unavailable")
        time.sleep(0.1)


def _provider_session(context: WorkspaceContext, task_id: str) -> dict[str, object]:
    reference_path = context.state / "tasks" / task_id / "provider.ref.json"
    require(reference_path.is_file() and not reference_path.is_symlink(),
            "successful fixture task did not publish provider session reference")
    reference = read_json(reference_path)
    root_record = read_json(context.state / "root.json")
    require(reference.get("root_id") == root_record.get("root_id") and
            reference.get("task_id") == task_id and
            reference.get("provider") == "pi:json",
            "provider session reference has wrong task identity")
    require(reference.get("conversation_id") == FIXTURE_SESSION,
            "provider session reference has wrong fixture session")
    return {
        "provider": reference.get("provider"),
        "conversation_id": reference.get("conversation_id"),
    }


def _fixture_session_marker(
    context: WorkspaceContext,
    *,
    resumed: bool,
) -> dict[str, object]:
    marker_path = context.fixture_session_marker
    require(marker_path.is_file() and not marker_path.is_symlink(),
            "native fixture session marker is absent")
    marker = read_json(marker_path)
    expected = {
        "schema_version": 1,
        "session": FIXTURE_SESSION,
        "resumed": resumed,
    }
    require(marker == expected, "native fixture session marker is not exact")
    return marker


def _task_record(context: WorkspaceContext, task_id: str) -> dict[str, object]:
    record_path = context.state / "tasks" / task_id / "task.json"
    require(record_path.is_file() and not record_path.is_symlink(),
            "task request record is absent: " + task_id)
    record = read_json(record_path)
    require(record.get("task_id") == task_id, "task request identity changed: " + task_id)
    return record


def _immutable_task_snapshot(
    context: WorkspaceContext,
    task_id: str,
) -> dict[str, bytes | None]:
    directory = context.state / "tasks" / task_id
    names = (
        "task.json",
        "meta.json",
        "brief.md",
        "provider.started.json",
        "provider.exit",
        "provider.ref.json",
        "outcome.json",
    )
    snapshot: dict[str, bytes | None] = {}
    for name in names:
        path = directory / name
        if name in {"provider.exit", "outcome.json"} and not path.exists() and not path.is_symlink():
            snapshot[name] = None
            continue
        require(path.is_file() and not path.is_symlink(),
                "immutable predecessor record is absent: " + name)
        snapshot[name] = path.read_bytes()
    return snapshot


def _assert_immutable_task_snapshot(
    context: WorkspaceContext,
    task_id: str,
    snapshot: dict[str, bytes | None],
) -> None:
    directory = context.state / "tasks" / task_id
    for name, expected in snapshot.items():
        path = directory / name
        if expected is None:
            require(not path.exists() and not path.is_symlink(),
                    "continuation created an absent predecessor record: " + name)
            continue
        require(path.is_file() and not path.is_symlink(),
                "immutable predecessor record disappeared: " + name)
        require(path.read_bytes() == expected,
                "predecessor immutable record changed: " + name)


def _validate_continuation_binding(
    context: WorkspaceContext,
    predecessor: dict[str, object],
    successor: dict[str, object],
    session: str,
) -> dict[str, object]:
    predecessor_id = _validate_hex(
        predecessor.get("task_id"), TASK_ID_RE, "continuation predecessor task ID",
    )
    successor_id = _validate_hex(
        successor.get("task_id"), TASK_ID_RE, "continuation successor task ID",
    )
    expected_workspace_id = _validate_hex(
        predecessor.get("workspace_id"), TASK_ID_RE, "continuation workspace ID",
    )
    require(isinstance(session, str) and session == FIXTURE_SESSION,
            "continuation session identity is not the fixture session")
    predecessor_record = _task_record(context, predecessor_id)
    successor_record = _task_record(context, successor_id)
    root_id = _validate_hex(
        predecessor.get("root_id"), TASK_ID_RE, "continuation root ID",
    )
    require(predecessor_record.get("root_id") == root_id,
            "predecessor task root identity changed")
    require(successor_record.get("root_id") == root_id and
            successor.get("root_id") == root_id,
            "successor task root identity changed")
    require(predecessor_record.get("provider") == "pi:json" and
            successor_record.get("provider") == "pi:json",
            "continuation changed provider identity")
    predecessor_cwd = predecessor_record.get("canonical_cwd")
    successor_cwd = successor_record.get("canonical_cwd")
    require(isinstance(predecessor_cwd, str) and predecessor_cwd and
            successor_cwd == predecessor_cwd,
            "continuation changed canonical cwd")
    predecessor_workspace = predecessor_record.get("workspace")
    successor_workspace = successor_record.get("workspace")
    require(isinstance(predecessor_workspace, dict) and
            isinstance(successor_workspace, dict),
            "continuation omitted managed workspace binding")
    require(predecessor_workspace.get("id") == expected_workspace_id and
            successor_workspace.get("id") == expected_workspace_id and
            successor.get("workspace_id") == expected_workspace_id,
            "continuation changed managed workspace identity")
    prior = successor_record.get("prior_session")
    require(prior == {
        "provider": "pi:json",
        "conversation_id": session,
        "predecessor_task_id": predecessor_id,
    }, "successor prior_session binding is not exact")
    predecessor_reference = _provider_session(context, predecessor_id)
    successor_reference = _provider_session(context, successor_id)
    require(predecessor_reference["conversation_id"] == session and
            successor_reference["conversation_id"] == session,
            "continuation changed provider session identity")
    workspace_response = successor.get("workspace")
    workspace = (
        _workspace_from(workspace_response)
        if isinstance(workspace_response, dict)
        else {}
    )
    workspace_path = workspace.get("path")
    require(workspace_path == predecessor_cwd,
            "continuation workspace path does not match task cwd")
    return {
        "predecessor_task_id": predecessor_id,
        "successor_task_id": successor_id,
        "predecessor_session": predecessor_reference,
        "successor_session": successor_reference,
        "prior_session": prior,
        "canonical_cwd": predecessor_cwd,
        "workspace_id": expected_workspace_id,
    }


def _capture_roundtrip(
    context: WorkspaceContext,
    capture: dict[str, object],
    *,
    label: str = "capture-roundtrip",
) -> dict[str, object]:
    evidence = verify_capture_receipt(context.state, capture)
    ref = evidence["snapshot_ref"]
    code, _ = _git(
        context, ["-C", str(context.source), "cat-file", "-e", str(ref) + "^{tree}"],
        context.source, label="capture-ref-before-release",
    )
    require(code == 0, "capture snapshot ref does not resolve before release")
    patch_path = context.state / "workspace-operations" / evidence["operation_id"] / "artifacts" / "diff.patch"
    roundtrip = context.owner.create(
        "capture patch roundtrip", context.base / (label + "-" + evidence["operation_id"]), context.base,
    ).directory
    _git_ok(
        context, ["clone", "--no-local", str(context.source), str(roundtrip)],
        context.base, label="capture-roundtrip-clone",
    )
    _git_ok(
        context, ["-C", str(roundtrip), "apply", "--check", str(patch_path)],
        context.base, label="capture-roundtrip-check",
    )
    _git_ok(
        context, ["-C", str(roundtrip), "apply", str(patch_path)],
        context.base, label="capture-roundtrip-apply",
    )
    _git_ok(
        context, ["-C", str(roundtrip), "add", "--all"],
        context.base, label="capture-roundtrip-add",
    )
    roundtrip_tree = _git_ok(
        context, ["-C", str(roundtrip), "write-tree"],
        context.base, label="capture-roundtrip-tree",
    ).decode("ascii").strip()
    require(roundtrip_tree == evidence["tree_oid"],
            "capture patch roundtrip produced a different tree OID")
    edited = roundtrip / FIXTURE_FILE
    require(edited.is_file() and edited.read_text(encoding="utf-8") == "fixture workspace edit\n",
            "capture patch did not reproduce fixture edit")
    return evidence


def _workspace_exec(context: WorkspaceContext, task: dict[str, object]) -> dict[str, object]:
    workspace_id = task["workspace_id"]
    code, response = _workspace_mutation(
        context,
        [
            "--root", str(context.state), "workspace", "exec", workspace_id,
            "--budget", WORKSPACE_OPERATION_BUDGET, "--json", "--",
            "/bin/sh", "-c",
            "test -f managed-workspace-fixture.txt && "
            "printf '%s\\n' workspace-exec-ok",
        ],
        action="exec", workspace_id=workspace_id,
        label="workspace-exec", task_id=task["task_id"],
    )
    operation_id = _operation_id(response)
    require(operation_id is not None, "workspace exec omitted operation identity")
    observed = _wait_workspace(
        context, workspace_id, operation_id=operation_id, state="retained", timeout=WAIT_SECONDS,
        task_id=task["task_id"],
    )
    operation = _workspace_from(observed).get("operation")
    require(isinstance(operation, dict) and operation.get("status") == "succeeded",
            "workspace exec operation did not succeed")
    require(operation.get("command_exit") == 0, "workspace exec command did not exit zero")
    return {
        "exit_code": code,
        "operation_id": operation_id,
        "command_exit": operation.get("command_exit"),
    }


def _workspace_mutation(
    context: WorkspaceContext,
    arguments: list[str],
    *,
    action: str,
    workspace_id: str,
    label: str,
    task_id: str,
) -> tuple[int, dict[str, object]]:
    """Retry only a negative action acknowledgement, preserving exact argv."""
    deadline = time.monotonic() + WAIT_SECONDS
    while True:
        remaining = deadline - time.monotonic()
        require(remaining > 0, "workspace action admission expired: " + action)
        code, response = _control(
            context, arguments, label=label, task_id=task_id,
            expected=(0, 3), timeout=min(90, remaining), phase=label,
        )
        classification = classify_control_response(code, response)
        if classification != "not-authorized":
            require(classification != "failed", "workspace action failed: " + repr(_compact(response)))
            return code, response
        require(_workspace_from(response).get("next_action") == "retry_" + action,
                "workspace action returned a different retry instruction")
        remaining = deadline - time.monotonic()
        require(remaining > 0, "workspace action admission expired: " + action)
        _wait_workspace(
            context, workspace_id, state="released" if action == "forget" else "retained",
            timeout=remaining, task_id=task_id,
        )


def _verify_ref(context: WorkspaceContext, reference: str, label: str, expected: tuple[int, ...]) -> int:
    process = _git_process(
        context,
        ["-C", str(context.source), "for-each-ref", "--format=%(refname)", "--", reference],
        context.source, label=label,
    )
    references = process.output(MAX_RESPONSE_BYTES).decode("utf-8").splitlines()
    present = reference in references
    if not present:
        require(not process.stderr_path.read_bytes(),
                "ref lookup diagnostics prevent proving exact absence")
    code = 0 if present else 1
    require(code in expected, "ref presence did not match expected state: " + label)
    return code


def _release_and_forget(context: WorkspaceContext, task: dict[str, object]) -> dict[str, object]:
    workspace_id = task["workspace_id"]
    _code, released = _workspace_mutation(
        context,
        [
            "--root", str(context.state), "workspace", "release", workspace_id,
            "--budget", WORKSPACE_OPERATION_BUDGET, "--json",
        ],
        action="release", workspace_id=workspace_id,
        label="workspace-release", task_id=task["task_id"],
    )
    release_operation = _operation_id(released)
    released_observed = _wait_workspace(
        context, workspace_id, operation_id=release_operation,
        state="released", timeout=WAIT_SECONDS, task_id=task["task_id"],
    )
    return {
        "release_state": _workspace_from(released_observed).get("state"),
        "release_operation": release_operation,
    }


def _forget_workspace(context: WorkspaceContext, task: dict[str, object]) -> dict[str, object]:
    workspace_id = task["workspace_id"]
    _code, forgotten = _workspace_mutation(
        context,
        [
            "--root", str(context.state), "workspace", "forget", workspace_id,
            "--budget", WORKSPACE_OPERATION_BUDGET, "--json",
        ],
        action="forget", workspace_id=workspace_id,
        label="workspace-forget", task_id=task["task_id"],
    )
    forget_operation = _operation_id(forgotten)
    if _workspace_from(forgotten).get("state") == "forgotten":
        final = forgotten
    else:
        final = _wait_workspace(
            context, workspace_id, operation_id=forget_operation,
            state="forgotten", timeout=WAIT_SECONDS, task_id=task["task_id"],
        )
    require(_workspace_from(final).get("state") == "forgotten",
            "workspace forget did not reach forgotten state")
    return {
        "forget_state": _workspace_from(final).get("state"),
        "forget_operation": forget_operation,
    }


def _finish_cohort(context: WorkspaceContext, cohort: private_cli.LiveFIFOLeaseCohort) -> list[dict[str, object]]:
    evidence = cohort.remove_closed_fifos()
    cohort.close()
    if cohort in context.cohorts:
        context.cohorts.remove(cohort)
    private_cli.require_empty_fixture_waiters(context.waiter_directory)
    return evidence


def _run_success(context: WorkspaceContext) -> dict[str, object]:
    first = _dispatch(context, context.brief_success, task_budget=SUCCESS_TASK_BUDGET, fresh=True)
    _adopt_workspace_container(context, first["root_id"], first["workspace_id"])
    first_status = _await_task(context, first, WAIT_SECONDS)
    _collect_task(context, first, WAIT_SECONDS)
    first_workspace, capture = _wait_capture(context, first)
    outcome = read_json(context.state / "tasks" / first["task_id"] / "outcome.json")
    first_closure = _prove_lifetimes_closed(context, [first["task_id"]])
    classification = classify_task_verdict(
        first_status, outcome, "success", lifetime_closed=bool(first_closure),
        capture_status=capture.get("status"),
    )
    require(classification["verdict"] == "passed", str(classification))
    session = _provider_session(context, first["task_id"])
    capture_evidence = _capture_roundtrip(context, capture)
    first_subject = capture_evidence["subject"]
    require(isinstance(first_subject, dict) and
            first_subject.get("id") == first["task_id"],
            "first task capture is bound to a different subject")
    ref = capture_evidence["snapshot_ref"]

    reused = _dispatch(
        context, context.brief_success_reuse, task_budget=SUCCESS_TASK_BUDGET,
        fresh=False, workspace_id=first["workspace_id"],
    )
    _adopt_workspace_container(context, reused["root_id"], reused["workspace_id"])
    reused_status = _await_task(context, reused, WAIT_SECONDS)
    reused_collect = _collect_task(context, reused, WAIT_SECONDS)
    reused_workspace, reused_capture = _wait_capture(context, reused)
    reused_outcome = read_json(context.state / "tasks" / reused["task_id"] / "outcome.json")
    reused_capture_evidence = verify_capture_receipt(context.state, reused_capture)
    reused_subject = reused_capture_evidence["subject"]
    require(isinstance(reused_subject, dict) and
            reused_subject.get("id") == reused["task_id"],
            "reused task capture is bound to a different subject")
    reused_closure = _prove_lifetimes_closed(context, [reused["task_id"]])
    reused_capture_evidence = _capture_roundtrip(
        context, reused_capture, label="reused-capture-roundtrip",
    )
    reused_session = _provider_session(context, reused["task_id"])
    all_closure = _prove_lifetimes_closed(
        context, [first["task_id"], reused["task_id"]],
    )
    reused_classification = classify_task_verdict(
        reused_status, reused_outcome, "success", lifetime_closed=bool(reused_closure),
        capture_status=reused_capture.get("status"),
    )
    require(reused_classification["verdict"] == "passed", str(reused_classification))
    exec_evidence = _workspace_exec(context, first)
    _verify_ref(context, ref, "capture-ref-before-release", (0,))
    reused_ref = reused_capture_evidence["snapshot_ref"]
    _verify_ref(context, reused_ref, "reused-capture-ref-before-release", (0,))
    release_evidence = _release_and_forget(context, first)
    _verify_ref(context, ref, "capture-ref-after-release", (0,))
    _verify_ref(context, reused_ref, "reused-capture-ref-after-release", (0,))
    forget_evidence = _forget_workspace(context, first)
    after_forget = _verify_ref(context, ref, "capture-ref-after-forget", (1,))
    require(after_forget == 1, "forget did not remove the capture ref")
    require(
        _verify_ref(context, reused_ref, "reused-capture-ref-after-forget", (1,)) == 1,
        "forget did not remove the reused capture ref",
    )
    return {
        "task_ids": [first["task_id"], reused["task_id"]],
        "workspace_id": first["workspace_id"],
        "provider_outcome_authority": outcome.get("verdict"),
        "lifetime_closure": first_closure,
        "all_lifetime_closure": all_closure,
        "provider_session": session,
        "capture": capture_evidence,
        "reused_capture": reused_capture_evidence,
        "reused_lifetime_closure": reused_closure,
        "reused_provider_session": reused_session,
        "source_untouched_before_cleanup": True,
        "workspace_exec": exec_evidence,
        "capture_ref_after_release": "resolves",
        "capture_ref_after_forget": "absent",
        "release": release_evidence,
        "forget": forget_evidence,
        "reused_workspace_dispatch": {
            "task_id": reused["task_id"],
            "outcome": reused_outcome.get("verdict"),
            "collection": reused_collect.get("status"),
            "capture": _workspace_from(reused_workspace).get("state"),
        },
        "workspace_response_state": _workspace_from(first_workspace).get("state"),
    }


def _wait_for_timeout(context: WorkspaceContext, task: dict[str, object]) -> dict[str, object]:
    deadline = time.monotonic() + TIMEOUT_WAIT_SECONDS
    latest: dict[str, object] = {}
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("fixture task did not reach timed_out status")
        receipt = context.command_receipt(task["task_id"], "timeout-status")
        latest = private_cli.read_created_task_status(
            context.delegate,
            context.state,
            context.environment,
            task["task_id"],
            receipt,
            phase="timeout-observation",
            timeout=min(10, remaining),
            capture_root=context.capture_root,
        )
        if _status_value(latest) == "timed_out":
            return latest
        if _status_value(latest) in {"done", "finished", "completed"}:
            raise RuntimeError("timeout fixture ended before authoritative timeout")
        time.sleep(min(0.1, remaining))


def _run_timeout(context: WorkspaceContext) -> dict[str, object]:
    gate = private_cli.LockedReleaseGate.acquire(context.gate_path)
    context.gates.append(gate)
    timeout_task = _dispatch(
        context, context.brief_timeout, task_budget=TIMEOUT_TASK_BUDGET, fresh=True,
    )
    _adopt_workspace_container(context, timeout_task["root_id"], timeout_task["workspace_id"])
    specification, cohort, lease_observation = private_cli.default_stop_live_lease(
        context.pueue,
        context.private_config,
        context.environment,
        context.state,
        timeout_task,
        context.waiter_directory,
        context.lease_nonce,
        capture_root=context.capture_root,
    )
    context.cohorts.append(cohort)
    context.native_proofs.append({
        "scenario": "timeout",
        "scope_kind": specification["scope_kind"],
        "assurance": specification["assurance"],
        "counted_task_count": 1,
        "lease": private_cli.default_stop_lease_evidence(specification, lease_observation),
    })
    timeout_status = _wait_for_timeout(context, timeout_task)
    context.release_gates()
    timeout_closure = private_cli.await_managed_lifetime_closure(
        context.state, (timeout_task["task_id"],), timeout=TIMEOUT_WAIT_SECONDS)
    lease_cleanup = _finish_cohort(context, cohort)
    _timeout_workspace_response, capture = _wait_capture(context, timeout_task)
    timeout_status = _wait_for_timeout(context, timeout_task)
    outcome_path = context.state / "tasks" / timeout_task["task_id"] / "outcome.json"
    collected = (_collect_task(context, timeout_task, TIMEOUT_WAIT_SECONDS)
                 if outcome_path.exists() or outcome_path.is_symlink() else None)
    outcome = collected.get("outcome") if collected is not None else None
    classification = classify_task_verdict(
        timeout_status, outcome, "timeout", lifetime_closed=bool(timeout_closure),
        capture_status=capture.get("status"),
    )
    require(classification["verdict"] == "passed", str(classification))
    timeout_session = _provider_session(context, timeout_task["task_id"])
    timeout_session_id = timeout_session.get("conversation_id")
    require(isinstance(timeout_session_id, str) and timeout_session_id,
            "timeout fixture session identity is unavailable")
    timeout_marker = _fixture_session_marker(context, resumed=False)
    capture_evidence = _capture_roundtrip(context, capture)
    predecessor_subject = capture_evidence["subject"]
    require(isinstance(predecessor_subject, dict) and
            predecessor_subject.get("id") == timeout_task["task_id"],
            "timeout capture is bound to a different predecessor")
    predecessor_snapshot = _immutable_task_snapshot(context, timeout_task["task_id"])
    continuation = _continue(
        context,
        timeout_task,
        brief=context.brief_timeout_resume,
        task_budget=TIMEOUT_CONTINUE_TASK_BUDGET,
    )
    _adopt_workspace_container(context, continuation["root_id"], continuation["workspace_id"])
    continuation_status = _await_task(context, continuation, WAIT_SECONDS)
    continuation_collect = _collect_task(context, continuation, WAIT_SECONDS)
    continuation_workspace, continuation_capture = _wait_capture(context, continuation)
    continuation_outcome = read_json(
        context.state / "tasks" / continuation["task_id"] / "outcome.json",
    )
    continuation_capture_evidence = verify_capture_receipt(
        context.state, continuation_capture,
    )
    continuation_subject = continuation_capture_evidence["subject"]
    require(isinstance(continuation_subject, dict) and
            continuation_subject.get("id") == continuation["task_id"],
            "continuation capture is bound to a different successor")
    continuation_closure = _prove_lifetimes_closed(
        context, [continuation["task_id"]],
    )
    continuation_capture_evidence = _capture_roundtrip(
        context,
        continuation_capture,
        label="timeout-continuation-capture-roundtrip",
    )
    continuation_classification = classify_task_verdict(
        continuation_status,
        continuation_outcome,
        "success",
        lifetime_closed=bool(continuation_closure),
        capture_status=continuation_capture.get("status"),
    )
    require(continuation_classification["verdict"] == "passed",
            str(continuation_classification))
    continuation_binding = _validate_continuation_binding(
        context, timeout_task, continuation, timeout_session_id,
    )
    _assert_immutable_task_snapshot(
        context, timeout_task["task_id"], predecessor_snapshot,
    )
    continuation_session = _provider_session(context, continuation["task_id"])
    continuation_marker = _fixture_session_marker(context, resumed=True)
    require(capture_evidence["operation_id"] !=
            continuation_capture_evidence["operation_id"],
            "timeout predecessor and successor share a capture operation")
    require(capture_evidence["operation_id"] != timeout_task["task_id"] and
            continuation_capture_evidence["operation_id"] != continuation["task_id"],
            "capture operation was attributed to a task lifetime")
    ref = capture_evidence["snapshot_ref"]
    _verify_ref(context, ref, "timeout-capture-ref-before-release", (0,))
    continuation_ref = continuation_capture_evidence["snapshot_ref"]
    _verify_ref(context, continuation_ref, "timeout-continuation-ref-before-release", (0,))
    release = _release_and_forget(context, continuation)
    _verify_ref(context, ref, "timeout-capture-ref-after-release", (0,))
    _verify_ref(context, continuation_ref, "timeout-continuation-ref-after-release", (0,))
    forget = _forget_workspace(context, continuation)
    require(_verify_ref(context, ref, "timeout-capture-ref-after-forget", (1,)) == 1,
            "timeout forget did not remove capture ref")
    require(
        _verify_ref(context, continuation_ref, "timeout-continuation-ref-after-forget", (1,)) == 1,
        "timeout continuation forget did not remove capture ref",
    )
    return {
        "task_id": timeout_task["task_id"],
        "workspace_id": timeout_task["workspace_id"],
        "continuation_task_id": continuation["task_id"],
        "task_status": timeout_status.get("status"),
        "ended_status": timeout_status.get("status"),
        "provider_outcome_authority": outcome.get("verdict") if outcome is not None else None,
        "predecessor_publication": timeout_status.get("publication"),
        "lifetime_closure": timeout_closure,
        "native_session_proof": {
            "claim": "fixture-only",
            "predecessor": timeout_session,
            "continuation": continuation_session,
            "marker_before_gate": timeout_marker,
            "marker_after_continue": continuation_marker,
        },
        "capture": capture_evidence,
        "continuation_capture": continuation_capture_evidence,
        "continuation_lifetime_closure": continuation_closure,
        "continuation_binding": continuation_binding,
        "predecessor_outcome_immutable": True,
        "capture_scopes": {
            "predecessor": {
                "operation_id": capture_evidence["operation_id"],
                "subject_id": predecessor_subject.get("id"),
            },
            "continuation": {
                "operation_id": continuation_capture_evidence["operation_id"],
                "subject_id": continuation_subject.get("id"),
            },
        },
        "lease": private_cli.default_stop_lease_evidence(specification, lease_observation),
        "lease_cleanup": lease_cleanup,
        "capture_ref_after_release": "resolves",
        "capture_ref_after_forget": "absent",
        "release": release,
        "forget": forget,
        "continuation_workspace_state": _workspace_from(continuation_workspace).get("state"),
        "collection_status": (collected.get("status") if collected is not None
                              else "not_requested_without_outcome"),
        "continuation_collection_status": continuation_collect.get("status"),
    }


def _dispatch_capacity_one(context: WorkspaceContext) -> dict[str, object]:
    return _dispatch(
        context, context.brief_capacity, task_budget=CAPACITY_TASK_BUDGET, fresh=True,
    )


def _settle_capacity_one(context: WorkspaceContext, task: dict[str, object]) -> dict[str, object]:
    status = _await_task(context, task, WAIT_SECONDS)
    collected = _collect_task(context, task, WAIT_SECONDS)
    require(_status_value(status) != "timed_out", "capacity task timed out")
    outcome = collected.get("outcome")
    require(isinstance(outcome, dict) and outcome.get("verdict") == "committed",
            "capacity task provider outcome was not committed")
    return {
        "task_id": task["task_id"],
        "workspace_id": task["workspace_id"],
        "status": status.get("status"),
        "collection_status": collected.get("status"),
    }


def _release_forget_capacity_one(context: WorkspaceContext, task: dict[str, object]) -> dict[str, object]:
    release = _release_and_forget(context, task)
    forget = _forget_workspace(context, task)
    return {"task_id": task["task_id"], "release": release, "forget": forget}


def _run_capacity(context: WorkspaceContext, count: int, workers: int) -> dict[str, object]:
    gate = private_cli.LockedReleaseGate.acquire(context.gate_path)
    context.gates.append(gate)
    started = time.monotonic()
    tasks: list[dict[str, object]] = []
    with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="workspace-dispatch") as pool:
        futures = [pool.submit(_dispatch_capacity_one, context) for _ in range(count)]
        for future in as_completed(futures):
            tasks.append(future.result())
    burst_seconds = max(0.0, time.monotonic() - started)
    tasks.sort(key=lambda item: item["task_id"])
    require(len(tasks) == count, "capacity dispatch did not return every task")
    for task in tasks:
        _adopt_workspace_container(context, task["root_id"], task["workspace_id"])
    deadline = time.monotonic() + WAIT_SECONDS
    specifications: list[dict[str, object]] = []
    while len(specifications) < count:
        specifications = private_cli.lease_candidate_specifications(private_cli.capacity_lease_specifications(
            context.state, tasks, context.waiter_directory, context.lease_nonce,
        ))
        if len(specifications) == count:
            break
        if time.monotonic() >= deadline:
            raise TimeoutError("capacity cohort did not reach all native leases")
        time.sleep(0.1)
    cohort = private_cli.LiveFIFOLeaseCohort.acquire(specifications, context.lease_nonce)
    context.cohorts.append(cohort)
    proof = private_cli.prove_lifetime_owner_cohort(specifications, cohort)
    scope_pairs = sorted({(item["scope_kind"], item["assurance"]) for item in specifications})
    require(len(scope_pairs) == 1, "capacity cohort selected mixed native scope assurance")
    context.native_proofs.append({
        "scenario": "capacity",
        "backend": scope_pairs[0][0],
        "assurance": scope_pairs[0][1],
        "counted_task_count": proof["counted_task_count"],
        "lease_probe": proof["lease_probe"],
    })
    context.release_gates()
    with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="workspace-settle") as pool:
        futures = [pool.submit(_settle_capacity_one, context, task) for task in tasks]
        settled = [future.result() for future in as_completed(futures)]
    settled.sort(key=lambda item: item["task_id"])
    capacity_closure = _prove_lifetimes_closed(
        context, [task["task_id"] for task in tasks],
    )
    fifo_cleanup = _finish_cohort(context, cohort)
    with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="workspace-release") as pool:
        futures = [pool.submit(_release_forget_capacity_one, context, task) for task in tasks]
        released = [future.result() for future in as_completed(futures)]
    return {
        "requested_task_count": count,
        "dispatch_workers": workers,
        "dispatch_burst_seconds": round(burst_seconds, 6),
        "dispatch_latency_target": "none",
        "counted_task_count": proof["counted_task_count"],
        "native_scope": {
            "backend": scope_pairs[0][0],
            "assurance": scope_pairs[0][1],
        },
        "capacity_lifetime_proof": {
            "counted_task_count": proof["counted_task_count"],
            "lease_probe": proof["lease_probe"],
            "lifetime_closure": capacity_closure,
        },
        "fifo_cleanup": fifo_cleanup,
        "settled_task_count": len(settled),
        "released_workspace_count": len(released),
    }


def _make_context(
    args: argparse.Namespace,
    binaries: dict[str, Path],
) -> tuple[WorkspaceContext, dict[str, Path]]:
    owner = DirectoryOwner()
    home = Path.home().resolve(strict=True)
    try:
        base_path = Path(tempfile.mkdtemp(prefix=".dl-workspaces-", dir=str(home))).resolve(strict=True)
    except OSError as error:
        raise AcceptanceBlocked("private-acceptance-root-unavailable") from error
    root_token = owner.adopt_created("managed workspace acceptance root", base_path, home)
    try:
        return _make_context_unchecked(args, binaries, owner, root_token)
    except BaseException as error:
        raise AcceptanceSetupFailure(error, owner, root_token) from error


def _make_context_unchecked(
    args: argparse.Namespace,
    binaries: dict[str, Path],
    owner: DirectoryOwner,
    root_token: OwnedDirectory,
) -> tuple[WorkspaceContext, dict[str, Path]]:
    base = root_token.directory
    state = owner.create("managed workspace state", base / "state", base).directory
    source = owner.create("managed workspace source", base / "source", base).directory
    provider_bin = owner.create("provider fixture bin", base / "provider-bin", base).directory
    waiter_directory = owner.create("provider fixture waiters", base / "provider-waiters", base).directory
    response_directory = owner.create("task command responses", base / "responses", base).directory
    home_dir = owner.create("private CLI home", base / "home", base).directory
    package_root = owner.create("private package root", base / "package", base).directory
    install, _canonical_bin, libexec = private_cli.install_package_layout(
        owner, binaries["delegate"], binaries["runner"], package_root, False,
    )
    private_cli.install_fixture(binaries["pueue"], libexec / "pueue")
    private_cli.install_fixture(binaries["pueued"], libexec / "pueued")
    gate_placeholder = base / "fixture-gate"
    fixture_session_marker = base / "native-fixture-session.json"
    lease_nonce = uuid.uuid4().hex
    _install_workspace_fixture(
        provider_bin, gate_placeholder, waiter_directory, lease_nonce,
        fixture_session_marker,
    )
    environment = inherited_environment()
    for key in tuple(environment):
        if key.startswith("GIT_"):
            environment.pop(key, None)
    environment.update({
        "HOME": str(home_dir),
        "PATH": os.pathsep.join((str(provider_bin), str(install), str(libexec), "/usr/bin", "/bin")),
        "XDG_CONFIG_HOME": str(home_dir / ".config"),
        "XDG_DATA_HOME": str(home_dir / ".local" / "share"),
        "LANG": "C.UTF-8",
        "LC_ALL": "C.UTF-8",
        "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_CONFIG_GLOBAL": os.devnull,
        "GIT_TERMINAL_PROMPT": "0",
    })
    git_path = _git_executable()
    binaries_for_receipt = {
        "delegate": binaries["delegate"],
        "runner": binaries["runner"],
        "pueue": binaries["pueue"],
        "pueued": binaries["pueued"],
        "git": git_path,
        "fixture": provider_bin / "pi",
    }
    processes = Processes(
        base,
        environment=environment,
        capture_root=root_token,
        environment_keys_only=True,
    )
    context = WorkspaceContext(
        owner=owner,
        root_token=root_token,
        base=base,
        state=state,
        source=source,
        brief_success=base / "brief-success.md",
        brief_success_reuse=base / "brief-success-reuse.md",
        brief_timeout=base / "brief-timeout.md",
        brief_timeout_resume=base / "brief-timeout-resume.md",
        brief_capacity=base / "brief-capacity.md",
        delegate=install / "delegate",
        pueue=libexec / "pueue",
        pueued=libexec / "pueued",
        private_config=state / ".supervisor" / "pueue.yml",
        gate_path=gate_placeholder,
        waiter_directory=waiter_directory,
        fixture_session_marker=fixture_session_marker,
        response_directory=response_directory,
        capture_root=root_token,
        processes=processes,
        environment=environment,
        source_baseline={},
        lease_nonce=lease_nonce,
        workspace_budget=args.workspace_budget,
    )
    _prepare_source(context)
    _write_briefs(context)
    return context, binaries_for_receipt


def _run(args: argparse.Namespace, finalizer: ReceiptFinalizer, context: WorkspaceContext) -> tuple[str, str]:
    manifest = build_source_manifest(context)
    finalizer.set_source_manifest(manifest)
    if context.source_baseline:
        finalizer.set_details(source_baseline=context.source_baseline)
    # The context owns this append-only proof list. Keep its receipt view even
    # when a later scenario phase fails; the final verdict still stays failed.
    finalizer.set_details(native_proofs=context.native_proofs)
    context.container_parent_token = None
    container_parent = context.state.parent / ("." + context.state.name + ".workspaces")
    if container_parent.exists() or container_parent.is_symlink():
        raise AcceptanceBlocked("managed-workspace-container-parent-already-exists")
    if args.scenario == "lifecycle":
        finalizer.set_details(scenario="lifecycle")
        lifecycle = _run_success(context)
        finalizer.set_details(lifecycle=lifecycle)
        timeout = _run_timeout(context)
        source_final = _assert_source_untouched(context)
        finalizer.set_details(
            scenario="lifecycle",
            lifecycle=lifecycle,
            timeout=timeout,
            native_proofs=context.native_proofs,
            source_final=source_final,
            task_count=len(context.task_ids),
            workspace_count=len(context.workspace_ids),
            command_response_count=context.command_count,
        )
    else:
        capacity = _run_capacity(context, args.parallel_tasks, args.dispatch_workers)
        source_final = _assert_source_untouched(context)
        finalizer.set_details(
            scenario="capacity",
            capacity=capacity,
            native_proofs=context.native_proofs,
            source_final=source_final,
            task_count=len(context.task_ids),
            workspace_count=len(context.workspace_ids),
            command_response_count=context.command_count,
        )
    return "passed", "managed workspace fixture acceptance passed"


def _parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--delegate", type=Path, required=True)
    parser.add_argument("--runner", type=Path, required=True)
    parser.add_argument("--pueue", type=Path, required=True)
    parser.add_argument("--pueued", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--scenario", choices=("lifecycle", "capacity"), default="lifecycle")
    parser.add_argument("--parallel-tasks", type=int, default=DEFAULT_CAPACITY_TASKS)
    parser.add_argument("--dispatch-workers", type=int, default=DEFAULT_DISPATCH_WORKERS)
    parser.add_argument("--workspace-budget", default=DEFAULT_WORKSPACE_BUDGET)
    args = parser.parse_args(argv)
    if args.parallel_tasks <= 0:
        parser.error("--parallel-tasks must be positive")
    if args.dispatch_workers <= 0:
        parser.error("--dispatch-workers must be positive")
    if args.scenario == "lifecycle" and args.parallel_tasks != DEFAULT_CAPACITY_TASKS:
        parser.error("--parallel-tasks is only configurable for --scenario capacity")
    if args.scenario == "capacity":
        args.dispatch_workers = min(args.parallel_tasks, args.dispatch_workers)
    output = args.output.resolve()
    if output.exists() or output.is_symlink():
        parser.error("--output already exists: " + str(output))
    output.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    args.output = output
    return args


def main(argv: list[str] | None = None) -> int:
    args = _parse_args(sys.argv[1:] if argv is None else argv)
    finalizer = ReceiptFinalizer(
        args.output,
        [sys.argv[0], *(sys.argv[1:] if argv is None else argv)],
        {
            "acceptance-driver": Path(__file__),
            "private-fixture": Path(private_cli.__file__),
            "delegate": args.delegate,
            "runner": args.runner,
            "pueue": args.pueue,
            "pueued": args.pueued,
        },
    )
    context: WorkspaceContext | None = None
    setup_failure: AcceptanceSetupFailure | None = None
    verdict = "failed"
    reason = "acceptance did not reach a verdict"
    cleanup: dict[str, object] = {"status": "not-started"}
    try:
        binaries = {
            "delegate": _require_executable(args.delegate, "delegate"),
            "runner": _require_executable(args.runner, "runner"),
            "pueue": _require_executable(args.pueue, "pueue"),
            "pueued": _require_executable(args.pueued, "pueued"),
        }
        context, receipt_binaries = _make_context(args, binaries)
        finalizer.binaries.update(receipt_binaries)
        finalizer.refresh_tool_fingerprints()
        verdict, reason = _run(args, finalizer, context)
    except AcceptanceSetupFailure as error:
        setup_failure = error
        if isinstance(error.cause, AcceptanceBlocked):
            verdict = "BLOCKED"
        else:
            verdict = "failed"
        reason = type(error.cause).__name__ + ": " + str(error.cause)
        finalizer.set_details(failure={"type": type(error.cause).__name__,
                                       "message": str(error.cause)})
    except AcceptanceBlocked as error:
        verdict = "BLOCKED"
        reason = str(error)
    except BaseException as error:
        verdict = "failed"
        reason = type(error).__name__ + ": " + str(error)
        finalizer.set_details(failure={"type": type(error).__name__, "message": str(error)})
    finally:
        if context is None:
            finalizer.finalize(verdict, reason, cleanup)
            if setup_failure is not None:
                try:
                    cleanup = setup_failure.cleanup()
                except BaseException as error:
                    verdict = "failed"
                    reason = "setup-cleanup-error: " + type(error).__name__ + ": " + str(error)
                    cleanup = {
                        "authorized": False,
                        "decision": "setup-cleanup-failed-evidence-retained",
                        "error": str(error),
                    }
                finalizer.finalize(verdict, reason, cleanup)
        else:
            finalizer.finalize(
                verdict,
                reason,
                {"status": "committed-before-cleanup", "retained_until": "compact-receipt"},
            )
            try:
                cleanup = context.cleanup(verdict)
            except BaseException as error:
                verdict = "failed"
                reason = "cleanup-or-closure-error: " + type(error).__name__ + ": " + str(error)
                cleanup = {
                    "authorized": False,
                    "decision": "cleanup-failed-evidence-retained",
                    "error": str(error),
                    "retained_owned_directories": [str(entry.directory)
                                                    for entry in context.owner.transfer()],
                }
            finalizer.finalize(verdict, reason, cleanup)
    summary = {"verdict": verdict, "reason": reason, "receipt": str(args.output)}
    sys.stdout.write(json.dumps(summary, sort_keys=True) + "\n")
    if verdict == "passed":
        return 0
    if verdict == "BLOCKED":
        return 3
    return 1


if __name__ == "__main__":
    raise SystemExit(main())

