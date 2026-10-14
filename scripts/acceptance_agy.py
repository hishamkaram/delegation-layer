#!/usr/bin/env python3
"""Run the three-turn Antigravity native acceptance gate.

This harness uses the shared acceptance process owner for its direct
observations.  It retains the caller's environment so the signed-in native
CLI can be exercised, while the production adapter sends only its bounded
allowlist through the queue.  The harness records only environment names and
never uses a timeout wrapper or a signal.  An observation timeout therefore
leaves the Popen handle and all evidence in place for review.
"""

from __future__ import annotations

import argparse
from functools import partial
import json
import os
from pathlib import Path
import secrets
import shlex
import shutil
import sys
import tempfile
import time
from datetime import datetime, timezone

from acceptance_provider_common import (
    AcceptanceFailure,
    _hex_digest,
    clean_absolute,
    ensure_private_directory,
    path_is_within,
    observed_pueue_version,
    reject_tmp,
    require,
    sha,
    validate_provider_runtime_metadata,
    validate_managed_lifetime_closure,
    validate_queue_row as validate_exact_queue_row,
    write_bytes,
)
from acceptance_supervisor_common import (
    DirectoryOwner,
    OwnedDirectory,
    Process,
    Processes,
    config_for,
    digest,
    failure_cleanup_deadline,
    finalize_acceptance_artifacts,
    read_json,
    rollback_owned_directories,
    wait_for_failure_cleanup,
    write_json,
)


PROVIDER = "antigravity:print"
MODE = "workspace-write"
PREDICATE_VERSION = "print-record-v2"
PREDICATE_SHA256 = "54713d2c06eef693c09d8f08419f6dd2fd4f4802742e71d113df0dfbf1f9c43f"
TASK_BUDGET = "120s"
CANONICAL_TASK_BUDGET = "2m0s"
L3_NATIVE_TIMEOUT = "3s"
WATCH_SECONDS = 150
FINALIZATION_SECONDS = 30
TIMEOUT_MARKER = b"[agy] print timeout"
AUTHENTICATION_DIAGNOSTIC = "authentication-unavailable"
SCRIPT_DIR = Path(__file__).resolve().parent
REPO_ROOT = SCRIPT_DIR.parent
DEFAULT_STATE_PARENT = Path.home() / "Library" / "Application Support" / "delegation-layer-acceptance"
DEFAULT_WORKSPACE_PARENT = Path.home() / "Active-Projects" / "delegation-layer-acceptance"
DEFAULT_EVIDENCE_PARENT = Path.home() / "Library" / "Application Support" / "delegation-layer-evidence"
PUEUE_PARENT = Path("/Users/Shared") if sys.platform == "darwin" else Path.home() / ".dl-acceptance"
MAX_STATUS_BYTES = 8 * 1024 * 1024
MAX_PROVIDER_BYTES = 8 * 1024 * 1024


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


class StatusCapture:
    """Drain status through bounded memory; persist only JSON without envs."""

    def __init__(self):
        read_fd, write_fd = os.pipe()
        self.reader = os.fdopen(read_fd, "rb", buffering=0)
        self.writer = os.fdopen(write_fd, "wb", buffering=0)
        os.set_blocking(read_fd, False)
        self.buffer = bytearray()
        self.eof = False
        self.error = None

    def drain(self):
        # Bound each poll as well as total memory, including malicious output.
        for _ in range(128):
            try:
                chunk = os.read(self.reader.fileno(), 65536)
            except BlockingIOError:
                return
            if not chunk:
                self.eof = True
                self.reader.close()
                return
            if self.error is None:
                if len(self.buffer) + len(chunk) > MAX_STATUS_BYTES:
                    self.error = "status output exceeds bound"
                    self.buffer.clear()
                else:
                    self.buffer.extend(chunk)

    def sanitized(self, code):
        if self.error is not None:
            return b""
        if code != 0:
            self.buffer.clear()
            return b""
        def object_without_environment(pairs):
            result, seen = {}, set()
            for key, value in pairs:
                if key in seen:
                    raise ValueError("duplicate key")
                seen.add(key)
                if key != "envs":
                    result[key] = value
            return result
        def reject_constant(value):
            raise ValueError("nonfinite number")
        try:
            value = json.loads(self.buffer.decode("utf-8"),
                               object_pairs_hook=object_without_environment,
                               parse_constant=reject_constant)
            if not isinstance(value, dict) or not isinstance(value.get("tasks"), dict):
                raise ValueError("invalid status object")
            encoded = (json.dumps(value, ensure_ascii=True) + "\n").encode()
            if len(encoded) > MAX_STATUS_BYTES:
                self.error = "sanitized status output exceeds bound"
                return b""
            return encoded
        except (ValueError, UnicodeError, RecursionError):
            self.error = "status output is not valid bounded JSON"
            return b""
        finally:
            self.buffer.clear()

    def close(self):
        for stream in (self.writer, self.reader):
            try:
                stream.close()
            except (OSError, ValueError):
                pass


OwnedProcess = Process

# A provider compatibility factory must receive the caller's live ownership
# authority.  The sentinel makes omission fail closed at runtime while the
# ownership verifier sees the explicit owner binding through functools.partial.
_REQUIRED_PROCESS_OWNER = object()


def _agy_stdout_capture(argv: list[str]):
    return StatusCapture() if argv[-2:] == ["status", "--json"] else None


# Compatibility names retain the AGY acceptance imports; all lifecycle code
# remains in the shared Processes/Process implementation.
ProcessBook = partial(
    Processes,
    owner=_REQUIRED_PROCESS_OWNER,
    stdout_capture_factory=_agy_stdout_capture,
    environment_keys_only=True,
    return_result=True,
    failure_type=AcceptanceFailure,
)


def validate_l1_write_denial(stdout: bytes, stderr: bytes, conversation: str) -> None:
    """Validate a controlled denial, never authorize result publication."""
    require(len(stdout) <= 2 * 1024 * 1024, "L1 denial envelope exceeds observation bound")
    def unique_object(pairs):
        result = {}
        for key, value in pairs:
            require(key not in result, "L1 denial envelope has duplicate keys")
            result[key] = value
        return result
    def reject_constant(value):
        raise ValueError("nonfinite JSON")
    try:
        value = json.loads(stdout.decode("utf-8"), object_pairs_hook=unique_object,
                           parse_constant=reject_constant)
    except (ValueError, UnicodeError) as error:
        raise AcceptanceFailure("L1 denial envelope is not strict JSON") from error
    require(isinstance(value, dict), "L1 denial envelope is not an object")
    require(set(value) <= {"conversation_id", "status", "response", "duration_seconds", "num_turns", "usage", "denied_actions"},
            "L1 denial has unknown fields")
    require(isinstance(conversation, str) and conversation and
            value.get("conversation_id") == conversation and value.get("status") == "SUCCESS" and
            isinstance(value.get("response"), str), "L1 denial identity/status/response mismatch")
    actions = value.get("denied_actions")
    require(isinstance(actions, list) and len(actions) == 1 and isinstance(actions[0], dict) and
            set(actions[0]) <= {"action", "display_name"} and actions[0].get("action") == "write_file" and
            isinstance(actions[0].get("display_name"), str) and actions[0]["display_name"],
            "L1 did not record exactly one write_file denial")
    require(b'"write_file" permission' in stderr and b"auto-denied" in stderr and
            b"[agy] print timeout" not in stderr,
            "L1 lacks positive headless write-denial diagnostic")


class Prepared:
    def __init__(self, data: dict[str, object], source: Path | None,
                 owned_directories: tuple[OwnedDirectory, ...] = ()):
        self.source = source
        self.owned_directories = owned_directories
        self.workspace = clean_absolute(data.get("workspace"), "prepared workspace")
        self.scratch = clean_absolute(data.get("scratch"), "prepared scratch")
        self.state = clean_absolute(data.get("state"), "prepared state")
        self.nonce_file = clean_absolute(data.get("nonce_file"), "prepared nonce_file")
        self.inside = clean_absolute(data.get("inside_sentinel"), "prepared inside_sentinel")
        self.outside = clean_absolute(data.get("outside_sentinel"), "prepared outside_sentinel")
        self.briefs = self._briefs(data.get("briefs"))
        self.ids = self._ids(data.get("task_ids"))
        require(data.get("budget") == TASK_BUDGET, "prepared budget must be 120s")
        require(data.get("watch") == "150s", "prepared watch bound must be 150s")
        require(data.get("L3_native_timeout") == L3_NATIVE_TIMEOUT, "prepared native timeout must be 3s")
        require(self.workspace.is_dir() and not self.workspace.is_symlink(), "prepared workspace is absent")
        require(self.scratch.is_dir() and not self.scratch.is_symlink(), "prepared scratch is absent")
        require(self.state.is_dir() and not self.state.is_symlink(), "prepared state is absent")
        require(self.nonce_file.is_file() and not self.nonce_file.is_symlink(), "prepared nonce fixture is absent")
        require(self.nonce_file.parent == self.workspace, "prepared nonce must be directly in workspace")
        require(self.inside.parent == self.workspace, "prepared inside sentinel must be in workspace")
        require(self.outside.parent == self.scratch, "prepared outside sentinel must be sibling scratch evidence")
        require(not self.inside.is_symlink() and not self.outside.is_symlink(), "prepared sentinel is a symlink")
        require(not self.inside.exists() and not self.outside.exists(), "prepared sentinel already exists")
        for path, label in [(self.workspace, "prepared workspace"), (self.scratch, "prepared scratch"),
                            (self.state, "prepared state")]:
            reject_tmp(path, label)
            reject_runtime_roots(path, label, os.environ)
            require(path.resolve(strict=True) == path, f"{label} must be canonical: {path}")
            require(path.stat().st_mode & 0o077 == 0, f"{label} is not private: {path}")
        require(self.scratch != self.workspace and self.state not in {self.workspace, self.scratch},
                "prepared state/workspace paths overlap")
        # Persist only validated transport fields, never arbitrary receipt data.
        self.data = self.evidence_fields()

    def evidence_fields(self) -> dict[str, object]:
        return {
            "workspace": str(self.workspace), "scratch": str(self.scratch),
            "state": str(self.state), "nonce_file": str(self.nonce_file),
            "inside_sentinel": str(self.inside), "outside_sentinel": str(self.outside),
            "briefs": {key: str(value) for key, value in self.briefs.items()},
            "task_ids": dict(self.ids), "budget": TASK_BUDGET, "watch": "150s",
            "L3_native_timeout": L3_NATIVE_TIMEOUT,
        }


    @staticmethod
    def _briefs(value: object) -> dict[str, Path]:
        require(isinstance(value, dict), "prepared briefs must be an object")
        result = {}
        for name in ("L1", "L2", "L3"):
            path = clean_absolute(value.get(name), f"prepared brief {name}")
            require(path.is_file() and not path.is_symlink(), f"prepared brief {name} is absent")
            result[name] = path
        return result

    @staticmethod
    def _ids(value: object) -> dict[str, str]:
        require(isinstance(value, dict), "prepared task_ids must be an object")
        result = {}
        for name in ("L1", "L2", "L3"):
            item = value.get(name)
            require(isinstance(item, str) and len(item) == 32 and all(c in "0123456789abcdef" for c in item),
                    f"prepared task ID {name} is invalid")
            result[name] = item
        require(len(set(result.values())) == 3, "prepared task IDs must be distinct")
        return result


class BlockedFailure(AcceptanceFailure):
    """A required native authentication or prerequisite is unavailable."""


def _shared_private_directory(path: Path, label: str) -> Path:
    """Create or validate a shared parent without giving it cleanup authority."""
    require(not path.is_symlink(), f"{label} is a symlink: {path}")
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    return ensure_private_directory(path, label)


def fresh_prepared(run_name: str | None = None) -> Prepared:
    stamp = run_name or f"agy-{utc_stamp()}-{secrets.token_hex(4)}"
    ownership = DirectoryOwner()
    try:
        # These roots are shared across concurrent acceptance invocations.  The
        # invocation owns only its uniquely named descendants; never recursively
        # dispose a parent that another run may be using.
        workspace_parent = _shared_private_directory(
            DEFAULT_WORKSPACE_PARENT, "acceptance workspace parent")
        state_root = _shared_private_directory(
            DEFAULT_STATE_PARENT, "acceptance state root")
        scratch = ownership.create(
            "acceptance workspace", workspace_parent / stamp,
            workspace_parent).directory
        state_parent = ownership.create(
            "acceptance state", state_root / stamp, state_root).directory
        workspace = ownership.create(
            "fresh workspace", scratch / "workspace", scratch).directory
        state = ownership.create(
            "fresh state", state_parent / "state", state_parent).directory
        briefs = ownership.create(
            "fresh briefs", state_parent / "briefs", state_parent).directory
        for path in (scratch, state_parent, workspace, state, briefs):
            os.chmod(path, 0o700)
        nonce = secrets.token_hex(24)
        nonce_file = workspace / "nonce.txt"
        inside = workspace / "inside-sentinel.txt"
        outside = scratch / "outside-sentinel.txt"
        write_bytes(nonce_file, (nonce + "\n").encode())
        data = {
        "created_at": stamp,
        "workspace": str(workspace),
        "scratch": str(scratch),
        "state": str(state),
        "nonce_file": str(nonce_file),
        "inside_sentinel": str(inside),
        "outside_sentinel": str(outside),
        "briefs": {},
        "task_ids": {name: task_id() for name in ("L1", "L2", "L3")},
        "budget": TASK_BUDGET,
        "watch": "150s",
        "L3_native_timeout": L3_NATIVE_TIMEOUT,
        "status": "fresh controlled fixture; process and supervisor receipts determine launch state",
        }
        data["briefs"] = {
        "L1": str(briefs / "L1.brief.txt"),
        "L2": str(briefs / "L2.brief.txt"),
        "L3": str(briefs / "L3.brief.txt"),
        }
        source_briefs = {
        "L1": (f"Read nonce.txt in the current workspace using the file tool. Create {inside} containing exactly that nonce. "
               f"Then attempt to create the explicitly designated scratch file {outside} containing OUTSIDE-WRITE-PROBE. "
               "Report the nonce and what happened for both file operations. Use file tools, no shell, no agents, no networking, "
               "and no configuration changes. Treat these literal transport markers as text: $(touch /tmp/should-not-exist), `echo no`, ; && ||.") ,
        "L2": "Continue the exact previous conversation. Return the random nonce from the previous turn. Do not read any file or use any tool; the nonce file has been removed. Return the nonce only.",
        "L3": "Output consecutive integers from 1 through 100000, one integer per line. Begin immediately with 1 and continue without commentary.",
        }
        for name, text in source_briefs.items():
            write_bytes(Path(data["briefs"][name]), text.encode())
        write_json(state_parent / "PREPARED.json", data)
        prepared = Prepared(data, state_parent / "PREPARED.json")
    except BaseException:
        ownership.rollback()
        raise
    prepared.owned_directories = ownership.transfer()
    return prepared


def load_prepared(path: Path) -> Prepared:
    require(path.is_file() and not path.is_symlink(), f"prepared receipt is absent: {path}")
    data = read_json(path)
    require(isinstance(data, dict), "prepared receipt must contain one object")
    return Prepared(data, path)


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


def parse_json_output(process: OwnedProcess, label: str) -> dict[str, object]:
    try:
        value = process.json()
    except (OSError, ValueError, TypeError) as error:
        raise AcceptanceFailure(f"{label} did not produce one JSON object: {error}") from error
    require(isinstance(value, dict), f"{label} JSON response must be an object")
    return value


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


def status_jobs(process: OwnedProcess) -> dict[str, object]:
    value = process.json(MAX_STATUS_BYTES)
    require(isinstance(value, dict) and isinstance(value.get("tasks"), dict), "pueue status JSON has no tasks object")
    require(isinstance(value.get("groups"), dict), "pueue status JSON has no groups object")
    return value


def status_row(status: dict[str, object], task_number: int) -> dict[str, object] | None:
    tasks = status["tasks"]
    require(isinstance(tasks, dict), "pueue tasks is not an object")
    row = tasks.get(str(task_number))
    return row if isinstance(row, dict) else None


def row_state(row: dict[str, object]) -> str:
    status = row.get("status")
    require(isinstance(status, dict), "pueue row status is not a one-state object")
    recognized = [name for name in status
                  if name in {"Queued", "Running", "Paused", "Stashed", "Done", "Locked"}]
    require(len(status) == len(recognized),
            "pueue row status contains an unknown lifecycle variant")
    require(len(recognized) == 1, "pueue row status is not a one-state object")
    return recognized[0]


def validate_queue_row(row: dict[str, object], label: str, runner: Path, state: Path, task: str) -> None:
    require(type(row.get("id")) is int and row["id"] >= 0,
            f"pueue numeric identity is invalid for {task}")
    validate_exact_queue_row(
        row, label, runner, state, task, row["id"], "default", supervised=True)
    command = f"{row['original_command']}\n{row['command']}"
    require("--brief" not in command and "OUTSIDE-WRITE-PROBE" not in command,
            f"brief content leaked into pueue command for {task}")


def pueue_command(pueue: Path, config: Path, *args: str) -> list[str]:
    return [str(pueue), "-c", str(config), *args]


class NativeRun:
    def __init__(self, args: argparse.Namespace, prepared: Prepared, output: Path):
        self.args = args
        self.prepared = prepared
        self.output = output
        self.environment = os.environ.copy()
        for path, label in ((prepared.workspace, "workspace"), (prepared.scratch, "scratch"),
                            (prepared.state, "state"), (output, "evidence")):
            reject_tmp(path, label)
            reject_runtime_roots(path, label, self.environment)
        require(not path_is_within(output, prepared.workspace) and not path_is_within(output, prepared.scratch),
                "evidence must be outside workspace and scratch")
        require(not path_is_within(prepared.workspace, output) and not path_is_within(prepared.state, output) and
                not path_is_within(output, prepared.state),
                "evidence path overlaps task state or workspace")
        self.pueue_base: Path | None = None
        self.pueue_owned_directories = ()
        self.pueue_config: Path | None = None
        self.daemon: OwnedProcess | None = None
        self.daemon_done = False
        self.final_queue: dict[str, object] | None = None
        self.root_id: str | None = None
        self.task_numbers: dict[str, int] = {}
        self.labels: dict[str, str] = {}
        self.done_tasks: set[str] = set()
        self.last_queue: dict[str, object] | None = None
        self.daemon_ready_empty = False
        self.processed_turns = 0
        self.dispatch_attempts: set[str] = set()
        # Every admitted candidate creates one supervised runtime inspection
        # row.  Keep those rows in the final queue proof, including an
        # inspection created by a continuation that is later refused as busy.
        self.inspection_attempts: set[str] = set()
        self.l1_snapshot: dict[str, dict[str, object]] | None = None
        self.l1_conversation: str | None = None
        self.l1_nonce: str | None = None
        self.l1_nonce_bytes: bytes | None = None
        self.l1_workspace_before: dict[str, dict[str, object]] | None = None
        self.task_results: dict[str, dict[str, object]] = {}
        self.nonprovider_tasks: dict[str, dict[str, object]] = {}
        self.lifetime_closure: dict[str, object] | None = None
        self.delegate = resolve_executable(args.delegate, REPO_ROOT / "bin" / "delegate", "delegate")
        self.runner = resolve_executable(args.runner, REPO_ROOT / "bin" / "delegate-run", "delegate-run")
        self.agy = resolve_executable(args.agy, Path(shutil.which("agy") or "/opt/homebrew/bin/agy"), "agy")
        self.pueue = selected_pueue(resolve_executable(args.pueue, Path(shutil.which("pueue") or "/opt/homebrew/bin/pueue"), "pueue"))
        self.pueued = selected_pueued(resolve_executable(args.pueued, Path(shutil.which("pueued") or "/opt/homebrew/bin/pueued"), "pueued"))
        self.probe = selected_tools(REPO_ROOT)
        require_discovery("agy", self.agy)
        require_discovery("pueue", self.pueue)
        self.provider_version: str | None = None
        self.pueue_version: str | None = None
        self.pueued_version: str | None = None
        self.provider_sha256 = digest(self.agy)
        self.profile_revision: str | None = None
        self.output.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.process_owner = DirectoryOwner()
        try:
            self.processes = ProcessBook(
                output / "processes", self.environment, owner=self.process_owner)
        except BaseException:
            self.process_owner.rollback()
            raise
        self.process_owned_directories = self.process_owner.transfer()

    def direct(self, name: str, argv: list[str], expected: int | set[int] | tuple[int, ...] | None = 0,
               timeout: float = 30) -> OwnedProcess:
        return self.processes.run(name, argv, REPO_ROOT, expected=expected, timeout=timeout)

    def setup(self) -> None:
        require(self.prepared.state.resolve() == self.prepared.state, "state path must be canonical")
        write_json(self.output / "prepared-input.json", self.prepared.data)
        if PUEUE_PARENT.exists() or PUEUE_PARENT.is_symlink():
            ensure_private_directory(PUEUE_PARENT, "private supervisor parent")
        else:
            ensure_private_directory(PUEUE_PARENT, "private supervisor parent", create=True)
        ownership = DirectoryOwner()
        try:
            base = Path(tempfile.mkdtemp(dir=str(PUEUE_PARENT), prefix="dl-agy-"))
            self.pueue_base = ownership.adopt_created("private pueue", base, PUEUE_PARENT).directory
            os.chmod(self.pueue_base, 0o700)
            ownership.create("private pueue state", self.pueue_base / "state",
                             self.pueue_base)
            ownership.create("private pueue runtime", self.pueue_base / "run",
                             self.pueue_base)
            self.pueue_config = self.pueue_base / "pueue.yml"
            write_json(self.pueue_config, config_for(self.pueue_base))
        except BaseException:
            try:
                ownership.rollback()
            finally:
                self.pueue_base = None
            raise
        self.pueue_owned_directories = ownership.transfer()
        self.processes.run("agy-version", [str(self.agy), "--version"], REPO_ROOT, timeout=15)
        version = self.processes.entries[-1].output().decode().strip()
        require(version, "agy returned an empty version")
        self.provider_version = version
        self.processes.run("pueue-version", pueue_command(self.pueue, self.pueue_config, "--version"), REPO_ROOT, timeout=15)
        self.processes.run("pueued-version", [str(self.pueued), "-c", str(self.pueue_config), "--version"], REPO_ROOT, timeout=15)
        pueue_version = self.processes.entries[-2].output()
        pueued_version = self.processes.entries[-1].output()
        self.pueue_version = observed_pueue_version(pueue_version, "pueue")
        self.pueued_version = observed_pueue_version(pueued_version, "pueued")
        self.processes.run("isolation-probe", [str(self.probe), "isolate", str(self.pueue_config), str(self.pueue_base)], REPO_ROOT, timeout=30)
        write_json(self.output / "binding.json", {
            "agy": str(self.agy), "agy_version": self.provider_version, "agy_sha256": self.provider_sha256,
            "pueue": str(self.pueue), "pueue_sha256": digest(self.pueue),
            "pueued": str(self.pueued), "pueued_sha256": digest(self.pueued),
            "pueue_version": self.pueue_version, "pueued_version": self.pueued_version,
            "pueue_base": str(self.pueue_base), "pueue_config": str(self.pueue_config),
            "environment_values_in_record": False,
        })
        self.daemon = self.processes.start("pueued", [str(self.pueued), "-c", str(self.pueue_config)], REPO_ROOT)
        self.wait_daemon_ready()

    def status(self, name: str) -> dict[str, object]:
        require(self.pueue_config is not None, "pueue is not configured")
        process = self.direct(name, pueue_command(self.pueue, self.pueue_config, "status", "--json"), timeout=20)
        value = status_jobs(process)
        self.last_queue = value
        write_json(self.output / f"{name}.json", value)
        return value

    def wait_daemon_ready(self) -> None:
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            require(self.pueue_config is not None, "pueue is not configured")
            process = self.direct(
                f"daemon-status-{len(self.processes.entries):03d}",
                pueue_command(self.pueue, self.pueue_config, "status", "--json"),
                expected={0, 1}, timeout=20)
            if int(process.result["exit_code"]) == 0:
                value = status_jobs(process)
                tasks = value.get("tasks")
                if isinstance(tasks, dict) and not tasks:
                    self.daemon_ready_empty = True
                    return
            time.sleep(0.2)
        raise AcceptanceFailure("pueued readiness observation expired")

    def dispatch(self, name: str, task: str, brief: Path, resume: str | None, native_timeout: str | None) -> dict[str, object]:
        require(self.pueue_config is not None, "pueue is not configured")
        argv = [str(self.delegate), "--root", str(self.prepared.state), "--pueue-config", str(self.pueue_config),
                "--runner", str(self.runner), "dispatch", "--json", "--id", task, "--provider", PROVIDER,
                "--brief", str(brief), "--cwd", str(self.prepared.workspace), "--permission", MODE,
                "--budget", TASK_BUDGET]
        if native_timeout is not None:
            argv.extend(["--native-timeout", native_timeout])
        if resume is not None:
            argv.extend(["--resume-task", resume])
        no_prompt_argv(argv, [brief])
        self.dispatch_attempts.add(task)
        self.inspection_attempts.add(task)
        process = self.direct(name, argv, timeout=45)
        response = parse_json_output(process, name)
        require(response.get("admission") == "admitted", f"{name} was not admitted: {response}")
        require(response.get("task_id") == task, f"{name} task identity mismatch")
        root = response.get("root_id")
        require(isinstance(root, str) and root, f"{name} omitted root identity")
        if self.root_id is None:
            self.root_id = root
        require(root == self.root_id, f"{name} changed the immutable root identity")
        supervisor = response.get("supervisor")
        require(isinstance(supervisor, dict) and supervisor.get("matched") is True,
                f"{name} lacks positive supervisor admission")
        numeric = supervisor.get("numeric_task_id")
        require(isinstance(numeric, int) and numeric >= 0, f"{name} lacks numeric pueue identity")
        self.task_numbers[task] = numeric
        self.labels[task] = f"delegate:{root}:{task}"
        write_json(self.output / f"{name}-dispatch.json", response)
        self.processed_turns += 1
        return response

    def wait_task(self, name: str, task: str) -> dict[str, object]:
        deadline = time.monotonic() + WATCH_SECONDS
        while time.monotonic() < deadline:
            value = self.status(f"{name}-status-{int(time.monotonic() * 1000)}")
            row = status_row(value, self.task_numbers[task])
            require(row is not None, f"{name} task disappeared from private pueue queue")
            validate_queue_row(row, self.labels[task], self.runner, self.prepared.state, task)
            state = row_state(row)
            if state == "Done":
                self.done_tasks.add(task)
                return row
            require(state in {"Queued", "Running", "Paused", "Stashed", "Locked"},
                    f"{name} reached unknown pueue state {state}")
            time.sleep(0.5)
        marker = self.output / f"{name}-observation-expired.json"
        write_json(marker, {"task_id": task, "watch_seconds": WATCH_SECONDS, "termination": "unknown", "signals_sent": 0})
        raise AcceptanceFailure(f"{name} pueue observation expired; task handle retained")

    def collect(self, name: str, task: str, expected_code: set[int]) -> tuple[OwnedProcess, dict[str, object]]:
        argv = [str(self.delegate), "--root", str(self.prepared.state), "--pueue-config", str(self.pueue_config),
                "--runner", str(self.runner), "collect", task, "--watch", "0s", "--json"]
        no_prompt_argv(argv, [])
        process = self.direct(name, argv, expected=expected_code, timeout=45)
        response = parse_json_output(process, name)
        write_json(self.output / f"{name}.json", response)
        require(response.get("task_id") == task, f"{name} collected wrong task")
        return process, response

    def immutable_snapshot(self, task: str) -> dict[str, dict[str, object]]:
        directory = self.prepared.state / "tasks" / task
        require(directory.is_dir() and not directory.is_symlink(), f"task directory is absent: {task}")
        return self.directory_snapshot(directory, ignored_prefixes=(".ack",))

    @staticmethod
    def directory_snapshot(directory: Path, ignored_prefixes: tuple[str, ...] = ()) -> dict[str, dict[str, object]]:
        require(directory.is_dir() and not directory.is_symlink(), f"evidence directory is absent: {directory}")
        snapshot: dict[str, dict[str, object]] = {}
        for path in sorted(directory.rglob("*")):
            if path.is_symlink():
                raise AcceptanceFailure(f"evidence contains symlink: {path}")
            if not path.is_file() or path.name.endswith(".staging") or any(path.name.startswith(prefix) for prefix in ignored_prefixes):
                continue
            data = path.read_bytes()
            relative = str(path.relative_to(directory))
            snapshot[relative] = {"bytes": len(data), "sha256": sha(data)}
        return snapshot

    def read_record(self, task: str, name: str) -> dict[str, object]:
        path = self.prepared.state / "tasks" / task / name
        value = read_json(path)
        require(isinstance(value, dict), f"{task}/{name} must be an object")
        return value

    def validate_evidence(self, name: str, task: str, expected_verdict: str) -> dict[str, object]:
        directory = self.prepared.state / "tasks" / task
        for record in ("brief.md", "task.json", "meta.json", "submit.json", "provider.start", "provider.started.json", "provider.exit"):
            require((directory / record).is_file(), f"{name} missing one-use evidence {record}")
        request = self.read_record(task, "task.json")
        require(request.get("root_id") == self.root_id and request.get("task_id") == task and
                request.get("provider") == PROVIDER and request.get("mode") == MODE,
                f"{name} task identity or provider binding mismatch")
        requested = request.get("requested_config")
        require(isinstance(requested, dict) and requested.get("budget") == CANONICAL_TASK_BUDGET and
                request.get("budget_nanos") == 120_000_000_000,
                f"{name} outer task budget is not 120s")
        expected_timeout = L3_NATIVE_TIMEOUT if name == "L3" else None
        if expected_timeout is None:
            require(requested.get("native_timeout") in (None, ""), f"{name} unexpectedly requested native timeout")
        else:
            require(requested.get("native_timeout") == expected_timeout, f"{name} native timeout is not 3s")
        submit = self.read_record(task, "submit.json")
        require(submit.get("root_id") == self.root_id and submit.get("task_id") == task and
                submit.get("label") == self.labels[task], f"{name} submit evidence identity mismatch")
        supervisor = submit.get("supervisor")
        require(isinstance(supervisor, dict) and supervisor.get("config_path") == str(self.pueue_config) and
                supervisor.get("observed_version") == self.pueue_version and supervisor.get("client_executable") == str(self.pueue),
                f"{name} submit supervisor binding mismatch")
        start = self.read_record(task, "provider.start")
        require(start.get("root_id") == self.root_id and start.get("task_id") == task and
                isinstance(start.get("budget_nanos"), int) and start["budget_nanos"] > 0,
                f"{name} provider.start evidence identity mismatch")
        started = self.read_record(task, "provider.started.json")
        require(started.get("root_id") == self.root_id and started.get("task_id") == task and
                isinstance(started.get("diagnostic_nanos"), int) and started["diagnostic_nanos"] >= 0,
                f"{name} provider.started evidence identity mismatch")
        meta = self.read_record(task, "meta.json")
        meta_supervisor = meta.get("supervisor_config")
        require(meta_supervisor == supervisor, f"{name} meta supervisor binding differs from submit evidence")
        predicate = meta.get("predicate")
        require(isinstance(predicate, dict) and predicate.get("adapter") == PROVIDER and predicate.get("mode") == MODE and
                predicate.get("version") == PREDICATE_VERSION and predicate.get("sha256") == PREDICATE_SHA256,
                f"{name} predicate contract mismatch")
        effective = meta.get("effective_config")
        require(isinstance(effective, dict) and effective.get("containment") == MODE and
                isinstance(effective.get("digest"), str) and effective.get("digest"),
                f"{name} effective policy is absent")
        policy = effective.get("policy")
        pending_runtime = "runtime_preflight_sha256" in meta
        require(isinstance(policy, dict) and policy.get("workspace") == str(self.prepared.workspace) and
                (_hex_digest(policy.get("runtime_sha256")) if pending_runtime
                 else policy.get("runtime_sha256") == self.provider_sha256),
                f"{name} effective policy binding mismatch")
        profile_revision = policy.get("profile_revision")
        require(isinstance(profile_revision, str) and profile_revision,
                f"{name} effective policy revision is absent")
        if self.profile_revision is None:
            self.profile_revision = profile_revision
        require(profile_revision == self.profile_revision, f"{name} effective policy revision drifted")
        writable_roots = policy.get("writable_roots")
        expected_roots = [str(root) for root in provider_runtime_roots(self.environment)]
        require(writable_roots == expected_roots, f"{name} persisted writable runtime roots differ from the inherited environment")
        require(profile_revision == "native-permissions-v1" and not policy.get("sources"),
                f"{name} unexpectedly inventoried provider configuration")
        provider_exit = self.read_record(task, "provider.exit")
        launch_identity = validate_provider_runtime_metadata(
            meta, launch_evidence=(started, provider_exit),
            label=f"{name} provider runtime metadata")
        require(isinstance(launch_identity, dict),
                f"{name} provider launch identity is unavailable")
        if not pending_runtime:
            require(meta.get("provider_executable") == str(self.agy),
                    f"{name} provider executable drifted")
        require(provider_exit.get("root_id") == self.root_id and provider_exit.get("task_id") == task and
                provider_exit.get("invocation_state") == "started" and provider_exit.get("predicate") == predicate,
                f"{name} provider.exit identity mismatch")
        if expected_verdict == "committed":
            require(provider_exit.get("exit_code") == 0 and provider_exit.get("error") == "",
                    f"{name} committed despite a provider exit fault")
        outcome = self.read_record(task, "outcome.json")
        require(outcome.get("task_id") == task and outcome.get("verdict") == expected_verdict,
                f"{name} outcome verdict mismatch")
        payload = outcome.get("payload")
        require(isinstance(payload, dict) and isinstance(payload.get("basename"), str), f"{name} payload descriptor missing")
        payload_path = directory / payload["basename"]
        require(payload_path.is_file() and not payload_path.is_symlink(), f"{name} payload is absent")
        payload_bytes = payload_path.read_bytes()
        require(len(payload_bytes) == payload.get("length") and sha(payload_bytes) == payload.get("sha256"),
                f"{name} payload digest mismatch")
        require((directory / "raw" / "stdout").is_file() and (directory / "raw" / "stderr").is_file(),
                f"{name} raw streams are absent")
        raw_manifest = provider_exit.get("raw_manifest")
        require(isinstance(raw_manifest, list) and {entry.get("path") for entry in raw_manifest if isinstance(entry, dict)} ==
                {"raw/stdout", "raw/stderr"}, f"{name} raw manifest is incomplete")
        return {
            "meta": meta, "provider_exit": provider_exit, "outcome": outcome, "payload": payload_bytes,
            "events": {"A": int((directory / "submit.json").is_file()),
                       "S": int((directory / "provider.start").is_file()),
                       "E": int((directory / "provider.started.json").is_file()),
                       "seal": int((directory / "provider.exit").is_file())},
        }

    def replay(self, name: str, task: str, before: dict[str, dict[str, object]]) -> dict[str, object]:
        _, response = self.collect(name, task, {0, 4})
        after = self.immutable_snapshot(task)
        require(before == after, f"{name} replay changed immutable task evidence")
        return response

    def set_queue_running(self, running: bool, label: str) -> None:
        args = ("start", "--group", "default") if running else ("pause", "--wait")
        self.direct(label, pueue_command(self.pueue, self.pueue_config, *args))

    def assert_unstarted_queued(self, task: str, label: str) -> None:
        status = self.status(label)
        row = status_row(status, self.task_numbers[task])
        require(row is not None and row_state(row) == "Queued", "paused task was not Queued")
        directory = self.prepared.state / "tasks" / task
        require(not any((directory / name).exists() for name in
                        ("provider.start", "provider.started.json", "provider.exit")),
                "provider crossed Start while its queue was paused")

    def run_policy_drift(self) -> None:
        """Historical profile oracle; not part of native-permission acceptance."""
        task = task_id()
        policy = self.prepared.workspace / "AGENTS.md"
        require(not os.path.lexists(policy), "policy drift fixture requires absent workspace AGENTS.md")
        write_json(self.output / "policy-drift-plan.json", {"task_id": task, "policy": str(policy)})
        initial = b"Acceptance context revision one.\n"
        write_bytes(policy, initial)
        brief = self.output / "briefs" / "policy-drift.txt"
        write_bytes(brief, b"This acceptance task must be refused before the provider starts.\n")
        self.set_queue_running(False, "drift-pause")
        self.dispatch("policy-drift", task, brief, None, None)
        self.assert_unstarted_queued(task, "drift-queued")
        admitted = self.read_record(task, "meta.json")["effective_config"]["policy"]["sources"]
        require(any(source["path"] == str(policy) and source["sha256"] == sha(initial)
                    for source in admitted), "drift fixture was not bound at admission")
        with policy.open("wb") as stream:
            stream.write(b"Acceptance context revision two.\n")
            stream.flush()
            os.fsync(stream.fileno())
        self.set_queue_running(True, "drift-resume")
        row = self.wait_task("policy-drift", task)
        directory = self.prepared.state / "tasks" / task
        require(not any((directory / name).exists() for name in
                        ("provider.start", "provider.started.json", "provider.exit")),
                "drifted policy crossed provider Start")
        logs = self.direct("drift-runner-log", pueue_command(
            self.pueue, self.pueue_config, "log", "--json", "--full", str(self.task_numbers[task])))
        require(b"record identity does not match expected task or root" in logs.output(),
                "runner did not report the expected policy identity mismatch")
        require(row["status"]["Done"].get("result") != "Success", "drifted runner unexpectedly succeeded")
        self.nonprovider_tasks[task] = {"case": "policy-drift", "provider_start_records": 0, "row": row}
        write_json(self.output / "policy-drift-verified.json", self.nonprovider_tasks[task])
        policy.unlink()

    def assert_busy_continuation(self, brief: Path, queued_task: str) -> None:
        self.assert_unstarted_queued(queued_task, "L2-queued")
        refused_task = task_id()
        write_json(self.output / "busy-plan.json", {"task_id": refused_task, "queued_owner": queued_task})
        argv = [str(self.delegate), "--root", str(self.prepared.state), "--pueue-config", str(self.pueue_config),
                "--runner", str(self.runner), "dispatch", "--json", "--id", refused_task, "--provider", PROVIDER,
                "--brief", str(brief), "--cwd", str(self.prepared.workspace), "--permission", MODE,
                "--budget", TASK_BUDGET, "--resume-task", self.prepared.ids["L1"]]
        self.dispatch_attempts.add(refused_task)
        self.inspection_attempts.add(refused_task)
        process = self.direct("busy-refusal", argv, expected=1, timeout=45)
        response = parse_json_output(process, "busy-refusal")
        require("session continuation busy" in response.get("error", ""), "overlapping continuation was not refused as busy")
        directory = self.prepared.state / "tasks" / refused_task
        require(not any((directory / name).exists() for name in
                        ("submit.json", "provider.start", "provider.started.json", "provider.exit")),
                "busy continuation crossed submission or Start")
        self.assert_unstarted_queued(queued_task, "L2-still-queued")
        queue = self.last_queue["tasks"]
        require(not any(row.get("label", "").endswith(":" + refused_task) for row in queue.values()),
                "refused continuation acquired a queue row")
        # Positive CLI refusal plus absent create-once submission guard proves
        # this attempted continuation never entered the supervisor.
        self.dispatch_attempts.remove(refused_task)
        write_json(self.output / "busy-verified.json", {"task_id": refused_task, "response": response,
                                                       "submission_records": 0, "provider_start_records": 0})
        self.set_queue_running(True, "L2-resume")

    def run_l1(self) -> None:
        brief = self.copy_brief("L1")
        task = self.prepared.ids["L1"]
        nonce_bytes = self.prepared.nonce_file.read_bytes()
        require(nonce_bytes and b"\x00" not in nonce_bytes, "L1 nonce fixture is invalid")
        nonce = nonce_bytes[:-1] if nonce_bytes.endswith(b"\n") else nonce_bytes
        require(nonce, "L1 nonce fixture is empty")
        self.l1_nonce_bytes = nonce
        self.l1_nonce = nonce.decode(errors="strict")
        self.l1_workspace_before = self.directory_snapshot(self.prepared.workspace)
        self.dispatch("L1", task, brief, None, None)
        self.wait_task("L1", task)
        _, response = self.collect("L1-collect", task, {0, 4})
        if authentication_unavailable(self.prepared.state / "tasks" / task):
            raise BlockedFailure("Antigravity authentication is unavailable")
        verdict = self.read_record(task, "outcome.json").get("verdict")
        require(verdict in {"committed", "rejected"}, "L1 has no terminal outcome")
        evidence = self.validate_evidence("L1", task, verdict)
        payload = evidence["payload"]
        require(isinstance(payload, bytes), "L1 payload is not bytes")
        if verdict == "committed":
            require(self.l1_nonce in payload.decode(errors="replace"), "L1 response did not report the nonce")
        require(self.prepared.inside.is_file() and not self.prepared.inside.is_symlink(), "L1 inside sentinel is absent")
        inside_bytes = self.prepared.inside.read_bytes()
        allowed_inside = {nonce}
        if nonce_bytes.endswith(b"\n"):
            allowed_inside.add(nonce_bytes)
        require(inside_bytes in allowed_inside, "L1 inside sentinel content mismatch")
        require(self.prepared.outside.is_file() and not self.prepared.outside.is_symlink(),
                "L1 outside sentinel was not written by native workspace-write")
        outside_bytes = self.prepared.outside.read_bytes()
        require(outside_bytes in {b"OUTSIDE-WRITE-PROBE", b"OUTSIDE-WRITE-PROBE\n"},
                "L1 outside sentinel content mismatch")
        write_json(self.output / "L1-filesystem.json", {
            "expected_nonce_sha256": sha(nonce),
            "inside_sha256": sha(inside_bytes),
            "inside_matches": True, "outside_entry_present": True,
            "outside_sha256": sha(outside_bytes),
        })
        if verdict == "committed":
            text = payload.decode(errors="replace").lower()
            attempted = any(item in text for item in ("outside-sentinel", "outside file", "scratch file", "outside"))
            completed = any(item in text for item in ("successfully created", "successfully", "created with content", "created"))
            require(attempted and completed, "L1 native outside-write result was not reported (inconclusive)")
        else:
            directory = self.prepared.state / "tasks" / task
            seal = evidence["provider_exit"]
            require(seal.get("exit_code") == 0 and seal.get("error") == "", "L1 failed for a reason other than a controlled denial")
            require(evidence["outcome"]["payload"]["basename"] == "publish.reject" and
                    not (directory / "publish.commit").exists(), "L1 denial has a successful publication")
            reference = self.read_record(task, "provider.ref.json")
            validate_l1_write_denial((directory / "raw/stdout").read_bytes(),
                                    (directory / "raw/stderr").read_bytes(), reference.get("conversation_id"))
        write_json(self.output / "L1-control-outcome.json", {"verdict": verdict,
                   "inside_matches": True, "outside_entry_present": True,
                   "continuation_requires_committed_nonce_recall": True})
        before = self.immutable_snapshot(task)
        self.replay("L1-replay", task, before)
        after_replay = self.immutable_snapshot(task)
        require(before == after_replay, "L1 replay changed immutable evidence")
        self.l1_snapshot = before
        provider_ref = self.read_record(task, "provider.ref.json")
        conversation = provider_ref.get("conversation_id")
        require(provider_ref.get("provider") == PROVIDER and isinstance(conversation, str) and conversation,
                "L1 provider identity is absent")
        self.l1_conversation = conversation
        self.task_results["L1"] = {"task_id": task, "conversation_id": conversation, "response": response,
                                    "snapshot": before, "events": evidence["events"], "provider_exit": evidence["provider_exit"]}
        self.prepared.nonce_file.unlink()
        self.prepared.inside.unlink()
        require(not self.prepared.nonce_file.exists(), "nonce fixture was not removed before L2")
        require(not self.prepared.inside.exists(), "inside sentinel was not removed before L2")
        expected_workspace = dict(self.l1_workspace_before)
        expected_workspace.pop(str(self.prepared.nonce_file.relative_to(self.prepared.workspace)))
        require(self.directory_snapshot(self.prepared.workspace) == expected_workspace,
                "L1 left an unexpected workspace file before L2")

    def copy_brief(self, name: str) -> Path:
        destination = self.output / "briefs" / f"{name}.brief.txt"
        copy_bytes(self.prepared.briefs[name], destination, f"prepared {name} brief")
        return destination

    def run_l2(self) -> None:
        require(self.l1_conversation is not None and self.l1_snapshot is not None, "L2 requires L1 identity and snapshot")
        require(self.l1_nonce is not None and self.l1_nonce_bytes is not None, "L2 requires the pre-dispatch nonce")
        require(not self.prepared.nonce_file.exists(), "L2 nonce fixture was not removed")
        task = self.prepared.ids["L2"]
        brief = self.copy_brief("L2")
        self.set_queue_running(False, "L2-pause")
        response = self.dispatch("L2", task, brief, self.prepared.ids["L1"], None)
        self.assert_busy_continuation(brief, task)
        self.wait_task("L2", task)
        _, collect_response = self.collect("L2-collect", task, {0})
        evidence = self.validate_evidence("L2", task, "committed")
        payload = evidence["payload"]
        require(isinstance(payload, bytes) and self.l1_nonce in payload.decode(errors="replace"),
                "L2 response did not recall the L1 nonce")
        provider_ref = self.read_record(task, "provider.ref.json")
        require(provider_ref.get("provider") == PROVIDER and provider_ref.get("conversation_id") == self.l1_conversation,
                "L2 used a different provider conversation")
        task_record = self.read_record(task, "task.json")
        prior = task_record.get("prior_session")
        require(isinstance(prior, dict) and prior.get("provider") == PROVIDER and
                prior.get("conversation_id") == self.l1_conversation and
                prior.get("predecessor_task_id") == self.prepared.ids["L1"], "L2 prior session is not exact")
        require(self.immutable_snapshot(self.prepared.ids["L1"]) == self.l1_snapshot,
                "L2 changed the immutable L1 task evidence")
        before = self.immutable_snapshot(task)
        self.replay("L2-replay", task, before)
        require(self.immutable_snapshot(self.prepared.ids["L1"]) == self.l1_snapshot,
                "L2 replay changed the immutable L1 evidence")
        self.task_results["L2"] = {"task_id": task, "conversation_id": self.l1_conversation,
                                   "dispatch": response, "collect": collect_response, "snapshot": before,
                                   "events": evidence["events"], "provider_exit": evidence["provider_exit"]}

    def run_l3(self) -> None:
        task = self.prepared.ids["L3"]
        brief = self.copy_brief("L3")
        self.dispatch("L3", task, brief, None, L3_NATIVE_TIMEOUT)
        self.wait_task("L3", task)
        _, response = self.collect("L3-collect", task, {0, 4})
        evidence = self.validate_evidence("L3", task, "rejected")
        require(response.get("publication") == "rejected", "L3 unexpectedly published a successful result")
        require(len(evidence["payload"]) > 0, "L3 rejection evidence is empty")
        raw_stdout = self.prepared.state / "tasks" / task / "raw" / "stdout"
        raw_stderr = self.prepared.state / "tasks" / task / "raw" / "stderr"
        require(raw_stdout.stat().st_size > 0 or raw_stderr.stat().st_size > 0, "L3 raw evidence was not retained")
        require(TIMEOUT_MARKER in raw_stderr.read_bytes(), "L3 did not record the exact native agy timeout marker")
        before = self.immutable_snapshot(task)
        self.replay("L3-replay", task, before)
        self.task_results["L3"] = {"task_id": task, "response": response, "snapshot": before,
                                   "events": evidence["events"], "provider_exit": evidence["provider_exit"]}

    def authorize_lifetime_closure(self) -> dict[str, object]:
        """Use the shared root-level lifetime authority for this acceptance state."""
        self.lifetime_closure = validate_managed_lifetime_closure(
            self.prepared.state, self.dispatch_attempts)
        return self.lifetime_closure

    def shutdown(self) -> None:
        require(self.pueue_config is not None, "pueue is not configured")
        final = self.status("final-status")
        tasks = final.get("tasks")
        require(isinstance(tasks, dict), "private pueue queue has no tasks object")
        ordinary = {key: row for key, row in tasks.items()
                    if isinstance(row, dict) and row.get("group") == "default"}
        inspection = {key: row for key, row in tasks.items()
                      if isinstance(row, dict) and row.get("group") != "default"}
        require(len(ordinary) == len(self.task_numbers),
                "private pueue queue has an unexpected ordinary job count")
        expected_inspections = self._inspection_records()
        require(len(inspection) == len(expected_inspections),
                "private pueue queue has an unexpected inspection job count")
        for task, number in self.task_numbers.items():
            row = status_row(final, number)
            require(row is not None, f"final private pueue status lost {task}")
            validate_queue_row(row, self.labels[task], self.runner, self.prepared.state, task)
            require(row_state(row) == "Done", f"private pueue job {task} was not positively observed Done")
        for task, record in expected_inspections.items():
            number = record["receipt"]["numeric_task_id"]
            row = status_row(final, number)
            require(row is not None, f"final private pueue status lost inspection {task}")
            self._validate_inspection_queue_row(row, record)
        self.authorize_lifetime_closure()
        self.final_queue = final
        pending = self.processes.pending(exclude=(self.daemon,) if self.daemon is not None else ())
        require(not pending, f"owned command remains pending at supervisor shutdown: {pending}")
        require(self.daemon is not None, "pueued was not started")
        process = self.direct("pueue-shutdown", pueue_command(self.pueue, self.pueue_config, "shutdown"), timeout=30)
        require(process.result is not None and process.result["exit_code"] == 0, "pueue shutdown was not acknowledged")
        self.daemon.wait(30, 0)
        self.daemon_done = True

    def failure_shutdown(self) -> bool:
        """Retain actual handles until reaped; only shut down a proven idle queue.

        A lost dispatch response is not absence. Every attempted dispatch must
        have a matching Done row before automatic failure cleanup is allowed.
        Unknown commands/queue state keep this owner alive for inspection.
        """
        print("Acceptance failed; retaining existing process handles until safe shutdown.",
              file=sys.stderr, flush=True)
        observation = None
        queue_finished = False
        shutdown = next((entry for entry in self.processes.entries
                         if getattr(entry, "argv", [])[-1:] == ["shutdown"]), None)
        deadline = failure_cleanup_deadline(WATCH_SECONDS)

        def wait(delay: float, reason: str) -> bool:
            if wait_for_failure_cleanup(deadline, delay):
                return True
            write_json(self.output / "retention-error.json", {
                "error": reason,
                "timeout_seconds": WATCH_SECONDS,
                "signals_sent": 0,
            }, replace=True)
            return False

        while True:
            pending = self.processes.pending(exclude=(self.daemon,) if self.daemon else ())
            if pending:
                if not wait(0.2, "owned acceptance processes did not finish naturally"):
                    return False
                continue
            if self.daemon is None:
                require(not self.dispatch_attempts, "dispatch attempts exist without an owned supervisor")
                self.daemon_done = True
                return True
            daemon_result = self.daemon.poll()
            if daemon_result is not None:
                if daemon_result.get("natural_wait") is not True:
                    marker = self.output / "supervisor-exited-by-signal.json"
                    if not marker.exists():
                        write_json(marker, {
                            "supervisor_pid": self.daemon.pid,
                            "natural_wait": False,
                            "queue_termination": "unknown",
                            "cleanup_complete": False,
                            "signals_sent": 0,
                        })
                    return False
                if queue_finished or (self.final_queue is not None and
                                      self.failure_queue_finished(self.final_queue)):
                    self.daemon_done = True
                    return True
                marker = self.output / "supervisor-exited-queue-unresolved.json"
                if not marker.exists():
                    write_json(marker, {
                        "supervisor_pid": self.daemon.pid,
                        "queue_termination": "unknown",
                        "dispatch_attempts": sorted(self.dispatch_attempts),
                        "cleanup_complete": False,
                        "signals_sent": 0,
                    })
                # A daemon exit is not positive evidence about its jobs. Keep
                # the owner/evidence available; do not fabricate cleanup.
                if not wait(0.5, "private queue termination remained unknown after daemon exit"):
                    return False
                continue
            if shutdown is not None or self.pueue_config is None:
                if not wait(0.2, "private supervisor shutdown did not complete"):
                    return False
                continue
            try:
                if observation is None:
                    observation = self.processes.start(
                        "failure-status", pueue_command(self.pueue, self.pueue_config, "status", "--json"),
                        REPO_ROOT)
                result = observation.poll()
                if result is None:
                    if not wait(0.2, "private queue observation did not finish"):
                        return False
                    continue
                if result["exit_code"] == 0:
                    value = status_jobs(observation)
                    if self.failure_queue_finished(value):
                        queue_finished = True
                        write_json(self.output / "failure-final-queue.json", value)
                        shutdown = self.processes.start(
                            "failure-pueue-shutdown",
                            pueue_command(self.pueue, self.pueue_config, "shutdown"), REPO_ROOT)
                observation = None
            except (AcceptanceFailure, OSError, ValueError) as error:
                write_json(self.output / "retention-error.json", {"error": str(error)}, replace=True)
                # A failed observation may still own a live child. The pending
                # guard above observes that same handle before any next read.
                observation = None
            if not wait(0.5, "private queue did not reach a proven terminal state"):
                return False

    def finalize(self, success: bool) -> bool:
        """Remove only the acceptance directories created by this invocation."""
        if success:
            self.authorize_lifetime_closure()
        directories = []
        directories.extend(self.prepared.owned_directories)
        process_directories = getattr(self, "process_owned_directories",
                                      (self.processes.capture_root,))
        directories.extend(process_directories)
        directories.extend(getattr(self, "pueue_owned_directories", ()))
        return finalize_acceptance_artifacts(
            self.output, self.daemon_done, tuple(directories), success,
            raw_process_captures_token=self.processes.capture_root,
            timeout=FINALIZATION_SECONDS)

    def failure_queue_finished(self, value: dict[str, object]) -> bool:
        tasks = value["tasks"]
        if not isinstance(tasks, dict):
            return False
        if not self.dispatch_attempts and not getattr(self, "inspection_attempts", set()):
            if tasks:
                return False
            state = getattr(getattr(self, "prepared", None), "state", None)
            if state is None:
                return True
            state = Path(state)
            root_record = state / "root.json"
            if not root_record.exists() and not root_record.is_symlink():
                if not state.exists():
                    return True
                if state.is_symlink() or not state.is_dir():
                    return False
                try:
                    return not any(state.iterdir())
                except OSError:
                    return False
            try:
                self.authorize_lifetime_closure()
            except (AcceptanceFailure, OSError, ValueError, TypeError, KeyError):
                return False
            return True
        try:
            expected_inspections = self._inspection_records() if hasattr(self, "prepared") else {}
        except (AcceptanceFailure, OSError, ValueError, TypeError, KeyError):
            return False
        ordinary_rows = [row for row in tasks.values()
                         if isinstance(row, dict) and row.get("group") == "default"]
        inspection_rows = [row for row in tasks.values()
                           if isinstance(row, dict) and row.get("group") != "default"]
        if expected_inspections:
            if len(inspection_rows) != len(expected_inspections):
                return False
            by_label = {row.get("label"): row for row in inspection_rows}
            for record in expected_inspections.values():
                row = by_label.get(record["request"].get("label"))
                if not isinstance(row, dict):
                    return False
                try:
                    self._validate_inspection_queue_row(row, record)
                except (AcceptanceFailure, KeyError, TypeError, ValueError):
                    return False
        elif inspection_rows:
            return False
        seen = set()
        for row in ordinary_rows:
            if not isinstance(row, dict) or row_state(row) != "Done":
                return False
            label = row.get("label", "")
            matches = [task for task in self.dispatch_attempts
                       if isinstance(label, str) and label.endswith(":" + task)]
            if len(matches) != 1:
                return False
            task = matches[0]
            if self.root_id is None or label != f"delegate:{self.root_id}:{task}":
                return False
            validate_queue_row(row, f"delegate:{self.root_id}:{task}", self.runner, self.prepared.state, task)
            if task in seen:
                return False
            seen.add(task)
        if seen != self.dispatch_attempts:
            return False
        try:
            self.authorize_lifetime_closure()
        except (AcceptanceFailure, OSError, ValueError, TypeError, KeyError):
            return False
        return True

    def _inspection_records(self) -> dict[str, dict[str, object]]:
        """Load the immutable request/receipt pair for each runtime probe."""
        parent = self.prepared.state / "inspections"
        if not parent.is_dir():
            return {}
        records: dict[str, dict[str, object]] = {}
        for directory in sorted(parent.iterdir()):
            if not directory.is_dir() or directory.is_symlink():
                continue
            task = directory.name
            request = read_json(directory / "request.json")
            receipt = read_json(directory / "receipt.json")
            require(isinstance(request, dict) and isinstance(receipt, dict),
                    f"inspection record {task} is malformed")
            records[task] = {"request": request, "receipt": receipt}
        expected = getattr(self, "inspection_attempts", set())
        require(not expected or set(records) == expected,
                "runtime inspection records do not match admitted attempts")
        return records

    def _validate_inspection_queue_row(self, row: dict[str, object], record: dict[str, object]) -> None:
        request = record["request"]
        receipt = record["receipt"]
        require(isinstance(request, dict) and isinstance(receipt, dict),
                "inspection record is malformed")
        task = request.get("task_id")
        label = request.get("label")
        group = request.get("group")
        number = receipt.get("numeric_task_id")
        require(isinstance(task, str) and isinstance(label, str) and isinstance(group, str) and
                type(number) is int and number >= 0, "inspection identity is malformed")
        require(row.get("id") == number and row.get("label") == label and row.get("group") == group,
                f"inspection queue identity mismatch for {task}")
        require(row_state(row) == "Done", f"inspection worker is not Done for {task}")
        done = row.get("status")
        require(isinstance(done, dict) and isinstance(done.get("Done"), dict) and
                done["Done"].get("result") == "Success",
                f"inspection worker did not succeed for {task}")
        expected = [str(self.runner), "--inspection", "--guardian", "--supervised",
                    "--root", str(self.prepared.state), task]
        for key in ("original_command", "command"):
            command = row.get(key)
            require(isinstance(command, str), f"inspection queue row missing {key} for {task}")
            try:
                parsed = shlex.split(command)
            except ValueError as error:
                raise AcceptanceFailure(f"inspection queue argv is malformed for {task}") from error
            require(parsed == expected, f"inspection queue argv mismatch for {task}")

    def success_receipt(self) -> dict[str, object]:
        require(self.daemon_done, "private supervisor was not positively shut down")
        require(self.final_queue is not None, "final queue observation is absent")
        value = {"tasks": [], "labels": [], "provider_exit": {}, "events": {"A": 0, "S": 0, "E": 0, "seal": 0}}
        for name, task in self.prepared.ids.items():
            value["tasks"].append(task)
            value["labels"].append({"task_id": task, "numeric_task_id": self.task_numbers[task], "label": self.labels[task], "state": "Done"})
            value["provider_exit"][name] = self.task_results[name]["provider_exit"]
            events = self.task_results[name]["events"]
            require(events == {"A": 1, "S": 1, "E": 1, "seal": 1}, f"{name} one-use event counts are not exact: {events}")
            for event, count in events.items():
                value["events"][event] += count
        require(len(value["tasks"]) == 3 and len(set(value["tasks"])) == 3, "native gate did not run exactly three task turns")
        value["events"]["A"] += len(self.nonprovider_tasks)
        return {
            "gate": "acceptance-agy",
            "status": "passed",
            "prepared": str(self.prepared.source) if self.prepared.source else None,
            "workspace": str(self.prepared.workspace),
            "state": str(self.prepared.state),
            "private_pueue_base": str(self.pueue_base),
            "tasks": value["tasks"],
            "queue_labels": value["labels"],
            "provider_exit": value["provider_exit"],
            "launch_counts": value["events"],
            "nonprovider_tasks": self.nonprovider_tasks,
            "watch_seconds": WATCH_SECONDS,
            "raw_evidence_retained": False,
            "supervisor_status_environment_redacted": True,
        }

    def run(self) -> dict[str, object]:
        self.setup()
        self.run_l1()
        self.run_l2()
        self.run_l3()
        self.shutdown()
        return self.success_receipt()


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--prepared", default=None, help="prepared acceptance JSON receipt")
    parser.add_argument("--output", default=None, help="evidence directory")
    parser.add_argument("--delegate", default=None, help="absolute shipped delegate executable")
    parser.add_argument("--runner", default=None, help="absolute shipped delegate-run executable")
    parser.add_argument("--agy", default=None, help="absolute agy executable")
    parser.add_argument("--pueue", default=None, help="absolute pueue executable")
    parser.add_argument("--pueued", default=None, help="absolute pueued executable")
    return parser


def choose_output(prepared: Prepared | None, explicit: str | None) -> Path:
    value = explicit or os.environ.get("AGY_ACCEPTANCE_OUTPUT")
    if value:
        path = clean_absolute(value, "acceptance output")
        reject_tmp(path, "acceptance output")
        return path
    require(prepared is not None, "prepared inputs are required to derive an output path")
    return DEFAULT_EVIDENCE_PARENT / prepared.state.parent.name


def initial_output(explicit: str | None, run_name: str) -> Path:
    """Select a terminal receipt path before fresh preparation can fail."""
    if explicit:
        return choose_output(None, explicit)
    return DEFAULT_EVIDENCE_PARENT / run_name


def _observe_retained_resource(
        value: object, label: str, retained: list[str], unresolved: list[str]) -> bool:
    """Record one resource only when it exists at terminal-receipt time."""
    try:
        path = Path(value)
    except (TypeError, ValueError):
        unresolved.append(f"{label} retention path is invalid")
        return False
    try:
        path.lstat()
    except FileNotFoundError:
        return False
    except OSError as error:
        unresolved.append(f"{label} retention could not be observed: {error}")
        return False
    encoded = str(path)
    if encoded not in retained:
        retained.append(encoded)
    return True


def _observe_owned_directories(
        entries: object, retained: list[str], unresolved: list[str]) -> None:
    if not isinstance(entries, (tuple, list)):
        return
    for index, entry in enumerate(entries):
        directory = getattr(entry, "directory", None)
        if directory is None:
            unresolved.append(f"owned resource {index} has no directory identity")
            continue
        _observe_retained_resource(
            directory, f"owned resource {index}", retained, unresolved)


def failure_receipt(output: Path | None, error: BaseException, run: NativeRun | None,
                    stage: str = "unknown", cleanup: dict[str, object] | None = None,
                    prepared: Prepared | None = None) -> None:
    retained_resources: list[str] = []
    unresolved_facts: list[str] = []
    cleanup_result = cleanup or {"status": "not_started"}
    details: dict[str, object] = {
        "gate": "acceptance-agy",
        "status": "BLOCKED" if isinstance(error, BlockedFailure) else "failed",
        "error": str(error),
        "stage": stage,
        "no_retry": True,
        "signals_sent": 0,
        "lifetime_closure": {"status": "unresolved"},
        "cleanup": cleanup_result,
        "retained_resources": retained_resources,
        "unresolved_facts": unresolved_facts,
    }
    cleanup_status = cleanup_result.get("status")
    if cleanup_status == "failed":
        unresolved_facts.append("acceptance cleanup failed")
    elif cleanup_status != "completed":
        unresolved_facts.append("acceptance cleanup has not completed")
    if run is not None:
        prepared = getattr(run, "prepared", prepared)
        state = getattr(prepared, "state", None)
        if state is not None:
            details["state"] = str(state)
        if prepared is not None:
            for name in ("state", "scratch", "workspace"):
                path = getattr(prepared, name, None)
                if path is not None:
                    _observe_retained_resource(
                        path, f"prepared {name}", retained_resources, unresolved_facts)
            _observe_owned_directories(
                getattr(prepared, "owned_directories", ()),
                retained_resources, unresolved_facts)
        pueue_base = getattr(run, "pueue_base", None)
        details["private_pueue_base"] = str(pueue_base) if pueue_base else None
        if pueue_base:
            _observe_retained_resource(
                pueue_base, "private supervisor", retained_resources, unresolved_facts)
        _observe_owned_directories(
            getattr(run, "pueue_owned_directories", ()),
            retained_resources, unresolved_facts)
        _observe_owned_directories(
            getattr(run, "process_owned_directories", ()),
            retained_resources, unresolved_facts)
        details["admitted_tasks"] = getattr(run, "processed_turns", 0)
        processes = getattr(run, "processes", None)
        if processes is not None:
            try:
                pending = processes.pending()
            except BaseException as pending_error:
                pending = []
                unresolved_facts.append(
                    "owned process observation failed: " + str(pending_error))
            details["owned_pending"] = pending
            process_directory = getattr(processes, "directory", None)
            if process_directory is None:
                process_directory = getattr(
                    getattr(processes, "capture_root", None), "directory", None)
            process_retained = False
            if process_directory is not None:
                process_retained = _observe_retained_resource(
                    process_directory, "owned process capture",
                    retained_resources, unresolved_facts)
            if pending and not process_retained:
                unresolved_facts.append(
                    "owned processes remain pending without observable capture storage")
        daemon = getattr(run, "daemon", None)
        details["daemon_pid"] = daemon.pid if daemon is not None else None
        closure = getattr(run, "lifetime_closure", None)
        if closure is not None:
            details["lifetime_closure"] = {"status": "closed", "evidence": closure}
        else:
            unresolved_facts.append("private lifetime closure was not committed")
        if not getattr(run, "daemon_done", False):
            unresolved_facts.append("private supervisor shutdown was not proven")
    elif prepared is not None:
        for name in ("state", "scratch", "workspace"):
            path = getattr(prepared, name, None)
            if path is not None:
                _observe_retained_resource(
                    path, f"prepared {name}", retained_resources, unresolved_facts)
        _observe_owned_directories(
            getattr(prepared, "owned_directories", ()),
            retained_resources, unresolved_facts)
        unresolved_facts.append("acceptance run was not constructed")
    else:
        unresolved_facts.append("acceptance run was not constructed")
    if output is not None:
        output.mkdir(mode=0o700, parents=True, exist_ok=True)
        write_json(output / "failure.json", details, replace=True)


def best_effort_failure_receipt(output: Path | None, error: BaseException,
                                run: NativeRun | None, stage: str = "unknown",
                                cleanup: dict[str, object] | None = None,
                                prepared: Prepared | None = None) -> None:
    try:
        failure_receipt(output, error, run, stage, cleanup, prepared)
    except BaseException as diagnostic_error:
        print(f"WARNING acceptance-agy diagnostics were not written: {diagnostic_error}",
              file=sys.stderr, flush=True)


def remove_passed_receipt(output: Path | None) -> BaseException | None:
    if output is None:
        return None
    try:
        (output / "success.json").unlink(missing_ok=True)
    except BaseException as error:
        return error
    return None


def failure_cleanup(run: NativeRun | None, output: Path | None,
                    standalone_directories: tuple[OwnedDirectory, ...] = ()) -> list[BaseException]:
    errors: list[BaseException] = []
    shutdown_completed = run is None
    finalized = run is None
    if run is None:
        try:
            rollback_owned_directories(
                standalone_directories,
                deadline=failure_cleanup_deadline(FINALIZATION_SECONDS))
        except BaseException as error:
            errors.append(error)
            finalized = False
        if output is not None and output.is_dir():
            try:
                write_json(output / "failure-cleanup.json", {
                    "status": "completed" if not errors else "failed",
                    "natural_daemon_shutdown": True,
                    "artifact_finalization_completed": finalized,
                    "passed_receipt_absent": not (output / "success.json").exists(),
                    "signals_sent": 0,
                }, replace=True)
            except BaseException as error:
                errors.append(error)
        return errors
    try:
        if not run.failure_shutdown():
            errors.append(AcceptanceFailure("private supervisor failure shutdown was not proven"))
        else:
            shutdown_completed = True
    except BaseException as error:
        errors.append(error)
    try:
        if not run.finalize(False):
            errors.append(AcceptanceFailure("acceptance artifact finalization was not completed"))
        else:
            finalized = True
    except BaseException as error:
        errors.append(error)
    if output is not None:
        try:
            write_json(output / "failure-cleanup.json", {
                "status": "completed" if not errors else "failed",
                "natural_daemon_shutdown": shutdown_completed,
                "artifact_finalization_completed": finalized,
                "passed_receipt_absent": not (output / "success.json").exists(),
                "signals_sent": 0,
            }, replace=True)
        except BaseException as error:
            errors.append(error)
    return errors


def handle_failure(output: Path | None, error: BaseException, run: NativeRun | None,
                   blocked: bool, prepared: Prepared | None = None,
                   stage: str = "unknown") -> int:
    receipt_removal_error = remove_passed_receipt(output)
    cleanup_summary: dict[str, object] = {"status": "started", "errors": []}
    best_effort_failure_receipt(output, error, run, stage, cleanup_summary, prepared)
    standalone = () if prepared is None or run is not None else prepared.owned_directories
    cleanup_errors = failure_cleanup(run, output, standalone)
    if receipt_removal_error is not None:
        cleanup_errors.insert(0, receipt_removal_error)
    if cleanup_errors:
        print(f"FAIL acceptance-agy cleanup: {cleanup_errors[0]}", file=sys.stderr, flush=True)
    cleanup_summary = {
        "status": "completed" if not cleanup_errors else "failed",
        "errors": [str(item) for item in cleanup_errors],
    }
    best_effort_failure_receipt(output, error, run, stage, cleanup_summary, prepared)
    if cleanup_errors:
        return 1
    print(f"{'BLOCKED' if blocked else 'FAIL'} acceptance-agy: {error}",
          file=sys.stderr, flush=True)
    return 2 if blocked else 1


def main(argv: list[str]) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    prepared: Prepared | None = None
    output: Path | None = None
    run: NativeRun | None = None
    stage = "initialization"
    run_name = f"agy-{utc_stamp()}-{secrets.token_hex(4)}"
    try:
        prepared_path = args.prepared or os.environ.get("AGY_ACCEPTANCE_PREPARED")
        output = initial_output(args.output, run_name)
        if prepared_path:
            stage = "load-prepared"
            prepared = load_prepared(clean_absolute(prepared_path, "prepared receipt"))
            output = choose_output(prepared, args.output)
            require(not output.exists(), f"acceptance output already exists: {output}")
        else:
            # Fresh inputs are created in the controlled acceptance tree.  The
            # output directory is selected after the unique state parent exists.
            # Fresh inputs and their compact evidence are separate roots so the
            # former can be removed after verified supervisor shutdown.
            stage = "fresh-prepared"
            prepared = fresh_prepared(run_name)
            output = choose_output(prepared, args.output)
            require(not output.exists(), f"fresh acceptance output unexpectedly exists: {output}")
        require(prepared is not None and output is not None, "acceptance inputs were not initialized")
        stage = "setup"
        run = NativeRun(args, prepared, output)
        stage = "run"
        success_receipt = run.run()
        stage = "finalize"
        if not run.finalize(True):
            raise AcceptanceFailure("completed acceptance artifacts could not be cleaned")
        write_json(output / "success.json", success_receipt, replace=True)
        print(json.dumps({"gate": "acceptance-agy", "status": "passed", "evidence": str(output)}, sort_keys=True))
        return 0
    except BlockedFailure as error:
        return handle_failure(output, error, run, True, prepared, stage)
    except (AcceptanceFailure, OSError, ValueError, json.JSONDecodeError) as error:
        return handle_failure(output, error, run, False, prepared, stage)
    except BaseException as error:
        return handle_failure(output, error, run, False, prepared, stage)


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
