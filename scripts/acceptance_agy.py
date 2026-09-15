#!/usr/bin/env python3
"""Run the three-turn Antigravity native acceptance gate.

This harness deliberately has a small process supervisor of its own.  Its
direct observations retain the caller's environment so the signed-in native
CLI can be exercised, while the production adapter sends only its bounded
allowlist through the queue.  The harness records only environment names and
never uses a timeout wrapper or a signal.  An observation timeout therefore
leaves the Popen handle and all evidence in place for review.
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import secrets
import shlex
import shutil
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone

from acceptance_provider_common import (
    AcceptanceFailure,
    clean_absolute,
    ensure_private_directory,
    path_is_within,
    reject_tmp,
    require,
    sha,
    write_bytes,
)
from acceptance_supervisor_common import config_for, digest, read_json, write_json


PROVIDER = "antigravity:print"
MODE = "workspace-write"
PREDICATE_VERSION = "1.2.2-auth2"
PREDICATE_SHA256 = "53f1b5368023953447088ecbd4fc36c1b431b0fb376cf76511ed70d943d8138a"
TASK_BUDGET = "120s"
CANONICAL_TASK_BUDGET = "2m0s"
L3_NATIVE_TIMEOUT = "3s"
WATCH_SECONDS = 150
TIMEOUT_MARKER = b"[agy] print timeout"
PUEUE_VERSION = "4.0.4"
SCRIPT_DIR = Path(__file__).resolve().parent
REPO_ROOT = SCRIPT_DIR.parent
DEFAULT_STATE_PARENT = Path.home() / "Library" / "Application Support" / "delegation-layer-acceptance"
DEFAULT_WORKSPACE_PARENT = Path.home() / "Active-Projects" / "delegation-layer-acceptance"
PUEUE_PARENT = Path("/Users/Shared")
MAX_STATUS_BYTES = 8 * 1024 * 1024


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


class OwnedProcess:
    """One Popen with durable pre-start and post-Wait evidence."""

    def __init__(self, directory: Path, argv: list[str], cwd: Path, env: dict[str, str], stdin: bytes | None):
        self.directory = directory
        self.argv = [str(item) for item in argv]
        self.cwd = cwd
        self.env = dict(env)
        self.process: subprocess.Popen[bytes] | None = None
        self.result: dict[str, object] | None = None
        self.stdout_path = directory / "stdout"
        self.stderr_path = directory / "stderr"
        self._stdout = None
        self._stderr = None
        self._status_capture = None
        executable = Path(self.argv[0])
        require(executable.is_absolute(), "owned executable must be absolute")
        require(executable.is_file() and not executable.is_symlink(), f"owned executable unavailable: {executable}")
        directory.mkdir(mode=0o700, parents=False, exist_ok=False)
        invocation = {
            "argv": self.argv,
            "cwd": str(cwd),
            "executable_sha256": digest(executable),
            "environment_keys": sorted(self.env),
            "environment_values_in_record": False,
            "harness_environment_preserved": True,
            "stdin": "PIPE" if stdin is not None else "DEVNULL",
            "stdin_bytes": len(stdin) if stdin is not None else 0,
            "stdin_sha256": sha(stdin) if stdin is not None else None,
            "shell": False,
        }
        write_json(directory / "invocation.json", invocation)
        try:
            self._stdout = self.stdout_path.open("xb")
            self._stderr = self.stderr_path.open("xb")
            if self.argv[-2:] == ["status", "--json"]:
                self._status_capture = StatusCapture()
            self.process = subprocess.Popen(
                self.argv,
                cwd=str(cwd),
                env=self.env,
                stdin=subprocess.PIPE if stdin is not None else subprocess.DEVNULL,
                stdout=self._status_capture.writer if self._status_capture is not None else self._stdout,
                stderr=self._stderr,
                shell=False,
                close_fds=True,
            )
            if self._status_capture is not None:
                self._status_capture.writer.close()
            if stdin is not None:
                self.process.stdin.write(stdin)
                self.process.stdin.flush()
                self.process.stdin.close()
        except BaseException:
            if self.process is not None and self.process.stdin is not None:
                try:
                    self.process.stdin.close()
                except BrokenPipeError:
                    pass  # close still releases the parent fd after child EOF.
            if self._status_capture is not None:
                self._status_capture.writer.close()
                if self.process is None:
                    self._status_capture.reader.close()
            if self._stdout is not None:
                self._stdout.close()
            if self._stderr is not None:
                self._stderr.close()
            raise

    @property
    def pid(self) -> int:
        require(self.process is not None, "owned process was not started")
        return self.process.pid

    def poll(self) -> dict[str, object] | None:
        if self.result is not None:
            return self.result
        require(self.process is not None, "owned process was not started")
        if self._status_capture is not None and not self._status_capture.eof:
            self._status_capture.drain()
        if self.process.poll() is None:
            return None
        if self._status_capture is not None and not self._status_capture.eof:
            return None
        code = self.process.wait()
        if self._status_capture is not None:
            self._stdout.write(self._status_capture.sanitized(code))
        if self._stdout is not None:
            self._stdout.close()
        if self._stderr is not None:
            self._stderr.close()
        self.result = {
            "pid": self.pid,
            "exit_code": code,
            "natural_wait": code >= 0,
            "ended_ns": time.time_ns(),
            "stdout_sha256": digest(self.stdout_path),
            "stderr_sha256": digest(self.stderr_path),
            "stdout_bytes": self.stdout_path.stat().st_size,
            "stderr_bytes": self.stderr_path.stat().st_size,
        }
        if self._status_capture is not None:
            self.result["stdout_redaction"] = "pueue-status-envs"
            self.result["capture_error"] = self._status_capture.error
        write_json(self.directory / "completed.json", self.result)
        return self.result

    def wait(self, timeout: float, expected: int | set[int] | tuple[int, ...] | None = 0) -> dict[str, object]:
        deadline = time.monotonic() + timeout
        while self.poll() is None:
            if time.monotonic() >= deadline:
                marker = self.directory / "observation-expired.json"
                if not marker.exists():
                    write_json(marker, {
                        "pid": self.pid,
                        "timeout_seconds": timeout,
                        "termination": "unknown",
                        "signals_sent": 0,
                    })
                raise AcceptanceFailure(f"observation expired; process ownership retained: {self.directory}")
            time.sleep(0.025)
        require(self.result is not None and bool(self.result["natural_wait"]),
                f"process ended by signal: {self.directory}")
        require(not self.result.get("capture_error"), f"status capture failed: {self.directory}")
        if expected is not None:
            allowed = {expected} if isinstance(expected, int) else set(expected)
            require(int(self.result["exit_code"]) in allowed,
                    f"unexpected exit {self.result['exit_code']} for {self.directory}")
        return self.result

    def output(self, bound: int = MAX_STATUS_BYTES) -> bytes:
        require(self.result is not None, "process output requested before Wait")
        data = self.stdout_path.read_bytes()
        require(len(data) <= bound, f"owned stdout exceeds bound: {self.directory}")
        return data

    def json(self, bound: int = MAX_STATUS_BYTES) -> object:
        require(self.result is not None and not self.result.get("capture_error"),
                f"status capture incomplete or invalid: {self.directory}")
        return read_json(self.directory / "stdout", bound)


class ProcessBook:
    def __init__(self, directory: Path, env: dict[str, str]):
        self.directory = directory
        self.directory.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.env = dict(env)
        self.entries: list[OwnedProcess] = []

    def start(self, name: str, argv: list[str], cwd: Path, stdin: bytes | None = None) -> OwnedProcess:
        directory = self.directory / f"{len(self.entries) + 1:03d}-{name}"
        # Register before initialization can start a child and then fail.
        process = OwnedProcess.__new__(OwnedProcess)
        self.entries.append(process)
        process.__init__(directory, argv, cwd, self.env, stdin)
        write_json(directory / "started.json", {"pid": process.pid, "started_ns": time.time_ns()})
        return process

    def run(self, name: str, argv: list[str], cwd: Path, expected: int | set[int] | tuple[int, ...] | None = 0,
            timeout: float = 30, stdin: bytes | None = None) -> OwnedProcess:
        process = self.start(name, argv, cwd, stdin)
        process.wait(timeout, expected)
        return process

    def pending(self, exclude: tuple[OwnedProcess, ...] = ()) -> list[dict[str, object]]:
        result = []
        for entry in self.entries:
            if entry in exclude or getattr(entry, "process", None) is None:
                continue
            if entry.poll() is None:
                result.append({"pid": entry.pid, "argv": entry.argv, "directory": str(entry.directory)})
        return result



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
            b"[agy] print timeout" not in stderr and b"authentication required" not in stderr,
            "L1 lacks positive headless write-denial diagnostic")


class Prepared:
    def __init__(self, data: dict[str, object], source: Path | None):
        self.source = source
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


def fresh_prepared() -> Prepared:
    stamp = f"{utc_stamp()}-{secrets.token_hex(4)}"
    scratch = ensure_private_directory(DEFAULT_WORKSPACE_PARENT / f"agy-{stamp}", "fresh scratch", create=True)
    workspace = ensure_private_directory(scratch / "workspace", "fresh workspace", create=True)
    state_parent = ensure_private_directory(DEFAULT_STATE_PARENT / f"agy-{stamp}", "fresh state parent", create=True)
    state = ensure_private_directory(state_parent / "state", "fresh state", create=True)
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
        "L1": str(state_parent / "briefs" / "L1.brief.txt"),
        "L2": str(state_parent / "briefs" / "L2.brief.txt"),
        "L3": str(state_parent / "briefs" / "L3.brief.txt"),
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
    return Prepared(data, state_parent / "PREPARED.json")


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
    require(isinstance(status, dict) and len(status) == 1, "pueue row status is not a one-state object")
    return next(iter(status))


def validate_queue_row(row: dict[str, object], label: str, runner: Path, state: Path, task: str) -> None:
    require(row.get("label") == label, f"pueue label mismatch for {task}")
    for key in ("original_command", "command", "path"):
        require(isinstance(row.get(key), str), f"pueue row missing {key} for {task}")
    command = f"{row['original_command']}\n{row['command']}"
    require(str(runner) in command and str(state) in command and task in command,
            f"pueue row does not bind runner/state/task for {task}")
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
        self.delegate = resolve_executable(args.delegate, REPO_ROOT / "bin" / "delegate", "delegate")
        self.runner = resolve_executable(args.runner, REPO_ROOT / "bin" / "delegate-run", "delegate-run")
        self.agy = resolve_executable(args.agy, Path("/opt/homebrew/bin/agy"), "agy")
        self.pueue = selected_pueue(resolve_executable(args.pueue, Path("/opt/homebrew/bin/pueue"), "pueue"))
        self.pueued = selected_pueued(resolve_executable(args.pueued, Path("/opt/homebrew/bin/pueued"), "pueued"))
        self.probe = selected_tools(REPO_ROOT)
        require_discovery("agy", self.agy)
        require_discovery("pueue", self.pueue)
        self.provider_version: str | None = None
        self.provider_sha256 = digest(self.agy)
        self.profile_revision: str | None = None
        self.output.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.processes = ProcessBook(output / "processes", self.environment)

    def direct(self, name: str, argv: list[str], expected: int | set[int] | tuple[int, ...] | None = 0,
               timeout: float = 30) -> OwnedProcess:
        return self.processes.run(name, argv, REPO_ROOT, expected=expected, timeout=timeout)

    def setup(self) -> None:
        require(self.prepared.state.resolve() == self.prepared.state, "state path must be canonical")
        write_json(self.output / "prepared-input.json", self.prepared.data)
        base = Path(tempfile.mkdtemp(dir=str(PUEUE_PARENT), prefix="dl-agy-"))
        os.chmod(base, 0o700)
        self.pueue_base = ensure_private_directory(base, "private pueue base")
        ensure_private_directory(self.pueue_base / "state", "private pueue state", create=True)
        ensure_private_directory(self.pueue_base / "run", "private pueue runtime", create=True)
        self.pueue_config = self.pueue_base / "pueue.yml"
        write_json(self.pueue_config, config_for(self.pueue_base))
        self.processes.run("agy-version", [str(self.agy), "--version"], REPO_ROOT, timeout=15)
        version = self.processes.entries[-1].output().decode().strip()
        require(version, "agy returned an empty version")
        self.provider_version = version
        self.processes.run("pueue-version", pueue_command(self.pueue, self.pueue_config, "--version"), REPO_ROOT, timeout=15)
        self.processes.run("pueued-version", [str(self.pueued), "-c", str(self.pueue_config), "--version"], REPO_ROOT, timeout=15)
        pueue_version = self.processes.entries[-2].output().decode().strip()
        pueued_version = self.processes.entries[-1].output().decode().strip()
        require(pueue_version == f"pueue {PUEUE_VERSION}", f"unexpected pueue version: {pueue_version!r}")
        require(pueued_version == f"pueued {PUEUE_VERSION}", f"unexpected pueued version: {pueued_version!r}")
        self.processes.run("isolation-probe", [str(self.probe), "isolate", str(self.pueue_config), str(self.pueue_base)], REPO_ROOT, timeout=30)
        write_json(self.output / "binding.json", {
            "agy": str(self.agy), "agy_version": self.provider_version, "agy_sha256": self.provider_sha256,
            "pueue": str(self.pueue), "pueue_sha256": digest(self.pueue),
            "pueued": str(self.pueued), "pueued_sha256": digest(self.pueued),
            "pueue_version": PUEUE_VERSION, "pueued_version": PUEUE_VERSION,
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
                supervisor.get("observed_version") == PUEUE_VERSION and supervisor.get("client_executable") == str(self.pueue),
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
        require(meta.get("provider_executable") == str(self.agy), f"{name} provider executable drifted")
        require(meta.get("provider_version") == self.provider_version, f"{name} provider version drifted")
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
        require(isinstance(policy, dict) and policy.get("workspace") == str(self.prepared.workspace) and
                policy.get("runtime_sha256") == self.provider_sha256,
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
        sources = policy.get("sources")
        require(isinstance(sources, list) and all(isinstance(source, dict) for source in sources),
                f"{name} policy source inventory is absent")
        for source in sources:
            require(isinstance(source.get("path"), str) and isinstance(source.get("kind"), str) and
                    isinstance(source.get("present"), bool) and isinstance(source.get("sha256"), str),
                    f"{name} policy source digest is malformed")
        provider_exit = self.read_record(task, "provider.exit")
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
        require(not os.path.lexists(self.prepared.outside), "L1 outside sentinel was written")
        write_json(self.output / "L1-filesystem.json", {
            "expected_nonce_sha256": sha(nonce),
            "inside_sha256": sha(inside_bytes),
            "inside_matches": True, "outside_entry_absent": True,
        })
        if verdict == "committed":
            text = payload.decode(errors="replace").lower()
            attempted = any(item in text for item in ("outside-sentinel", "outside file", "scratch file", "outside"))
            refused = any(item in text for item in ("denied", "cannot", "unable", "refused", "blocked", "permission", "not allowed", "failed"))
            require(attempted and refused, "L1 outside-write denial was not reported (inconclusive)")
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
                   "inside_matches": True, "outside_entry_absent": True,
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
        self.final_queue = final
        pending = self.processes.pending(exclude=(self.daemon,) if self.daemon is not None else ())
        require(not pending, f"owned command remains pending at supervisor shutdown: {pending}")
        require(self.daemon is not None, "pueued was not started")
        process = self.direct("pueue-shutdown", pueue_command(self.pueue, self.pueue_config, "shutdown"), timeout=30)
        require(process.result is not None and process.result["exit_code"] == 0, "pueue shutdown was not acknowledged")
        self.daemon.wait(30, 0)
        self.daemon_done = True

    def failure_shutdown(self) -> None:
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
        while True:
            pending = self.processes.pending(exclude=(self.daemon,) if self.daemon else ())
            if pending:
                time.sleep(0.2)
                continue
            if self.daemon is None:
                require(not self.dispatch_attempts, "dispatch attempts exist without an owned supervisor")
                return
            if self.daemon.poll() is not None:
                if queue_finished or (self.final_queue is not None and
                                      self.failure_queue_finished(self.final_queue)):
                    return
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
                time.sleep(0.5)
                continue
            if shutdown is not None or self.pueue_config is None:
                time.sleep(0.2)
                continue
            try:
                if observation is None:
                    observation = self.processes.start(
                        "failure-status", pueue_command(self.pueue, self.pueue_config, "status", "--json"),
                        REPO_ROOT)
                result = observation.poll()
                if result is None:
                    time.sleep(0.2)
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
            time.sleep(0.5)

    def failure_queue_finished(self, value: dict[str, object]) -> bool:
        tasks = value["tasks"]
        if not isinstance(tasks, dict):
            return False
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
        return seen == self.dispatch_attempts

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
        expected = [str(self.runner), "--inspection", "--root", str(self.prepared.state), task]
        for key in ("original_command", "command"):
            command = row.get(key)
            require(isinstance(command, str), f"inspection queue row missing {key} for {task}")
            try:
                parsed = shlex.split(command)
            except ValueError as error:
                raise AcceptanceFailure(f"inspection queue argv is malformed for {task}") from error
            require(parsed == expected, f"inspection queue argv mismatch for {task}")

    def verify_queue_and_write_success(self) -> None:
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
        write_json(self.output / "success.json", {
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
            "raw_evidence_retained": True,
            "supervisor_status_environment_redacted": True,
        })

    def run(self) -> None:
        self.setup()
        self.run_policy_drift()
        self.run_l1()
        self.run_l2()
        self.run_l3()
        self.shutdown()
        self.verify_queue_and_write_success()


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--prepared", default=None, help="prepared Phase A JSON receipt")
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
    return prepared.state.parent / "evidence"


def failure_receipt(output: Path | None, error: BaseException, run: NativeRun | None) -> None:
    details: dict[str, object] = {
        "gate": "acceptance-agy",
        "status": "failed",
        "error": str(error),
        "no_retry": True,
        "signals_sent": 0,
    }
    if run is not None:
        details.update({
            "state": str(run.prepared.state),
            "private_pueue_base": str(run.pueue_base) if run.pueue_base else None,
            "admitted_tasks": run.processed_turns,
            "owned_pending": run.processes.pending(),
            "daemon_pid": run.daemon.pid if run.daemon is not None else None,
        })
    if output is not None and not (run is None and output.exists()):
        output.mkdir(mode=0o700, parents=True, exist_ok=True)
        write_json(output / "failure.json", details, replace=True)


def main(argv: list[str]) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    prepared: Prepared | None = None
    output: Path | None = None
    run: NativeRun | None = None
    try:
        prepared_path = args.prepared or os.environ.get("AGY_ACCEPTANCE_PREPARED")
        if prepared_path:
            prepared = load_prepared(clean_absolute(prepared_path, "prepared receipt"))
            output = choose_output(prepared, args.output)
            require(not output.exists(), f"acceptance output already exists: {output}")
        else:
            # Fresh inputs are created in the controlled acceptance tree.  The
            # output directory is selected after the unique state parent exists.
            parent = DEFAULT_STATE_PARENT
            parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            marker = f"agy-{utc_stamp()}-{secrets.token_hex(4)}"
            output = parent / marker / "evidence"
            # fresh_prepared creates its own unique parent; derive output from it
            # after construction rather than guessing the generated name.
            prepared = fresh_prepared()
            output = prepared.state.parent / "evidence"
            require(not output.exists(), f"fresh acceptance output unexpectedly exists: {output}")
        require(prepared is not None and output is not None, "acceptance inputs were not initialized")
        run = NativeRun(args, prepared, output)
        run.run()
        print(json.dumps({"gate": "acceptance-agy", "status": "passed", "evidence": str(output)}, sort_keys=True))
        return 0
    except (AcceptanceFailure, OSError, ValueError, json.JSONDecodeError) as error:
        failure_receipt(output, error, run)
        print(f"FAIL acceptance-agy: {error}", file=sys.stderr, flush=True)
        if run is not None:
            run.failure_shutdown()
            failure_receipt(output, error, run)
        return 1
    except BaseException as error:
        failure_receipt(output, error, run)
        print(f"FAIL acceptance-agy: unexpected {error}", file=sys.stderr, flush=True)
        if run is not None:
            run.failure_shutdown()
            failure_receipt(output, error, run)
        return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
