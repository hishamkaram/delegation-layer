"""Own native timeout FIFO leases and their readiness/closure receipt."""

from __future__ import annotations

import os
from pathlib import Path
import select
import secrets
import shlex
import stat
import sys
import time
from typing import Protocol

from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require
from scripts.delegate_acceptance.paths.operations import write_bytes
from scripts.delegate_acceptance.processes.observation import wait_until
from scripts.delegate_acceptance.native.diagnostics import _delegate_status_summary
from scripts.delegate_acceptance.native.errors import BlockedFailure


TIMEOUT_LEASE_MODE = 0o600
TIMEOUT_LEASE_MAX_BYTES = 512
TIMEOUT_LEASE_SETUP_SECONDS = 330
TIMEOUT_LEASE_STATUS_INTERVAL_SECONDS = 5
TIMEOUT_LEASE_OBSERVATION_SECONDS = 30


class TimeoutStatusSource(Protocol):
    root_id: str | None

    def status(self, name: str, task_id: str, *, expected: set[int],
               timeout: float) -> dict[str, object]: ...


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
            raise BlockedFailure(
                "timeout liveness FIFO is unavailable on this filesystem") from error

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
                    require(self.ready, f"{self.label} FIFO closed before exact readiness")
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


class TimeoutLeaseOwner:
    """Own lease paths, readiness facts, and the published liveness receipt."""

    def __init__(self, workspace: Path, enabled: bool,
                 directory_owner: DirectoryOwner) -> None:
        self.marker_file = workspace / "acceptance-timeout-marker.txt"
        self.resume_file = workspace / "acceptance-timeout-resumed.txt"
        self.marker = "timeout-marker:" + secrets.token_hex(24)
        self.resume_marker = "timeout-resumed:" + secrets.token_hex(24)
        self.directory = workspace / "acceptance-timeout-leases"
        self.parent_fifo = self.directory / "parent.fifo"
        self.child_fifo = self.directory / "child.fifo"
        self.parent_ready_marker = "timeout-parent-ready:" + secrets.token_hex(24)
        self.child_ready_marker = "timeout-child-ready:" + secrets.token_hex(24)
        self.leases: dict[str, TimeoutFifoLease] = {}
        self.liveness: dict[str, object] | None = None
        if enabled:
            directory_owner.create("timeout liveness leases", self.directory, workspace)
            self.leases = {
                "parent": TimeoutFifoLease(
                    self.parent_fifo, "timeout parent", self.parent_ready_marker),
                "child": TimeoutFifoLease(
                    self.child_fifo, "timeout child", self.child_ready_marker),
            }

    def cleanup(self) -> bool:
        failures: list[BaseException] = []
        for lease in self.leases.values():
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
        for path in (self.marker_file, self.resume_file):
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
        try:
            metadata = self.directory.lstat()
        except FileNotFoundError:
            metadata = None
        except OSError as error:
            failures.append(error)
            metadata = None
        if metadata is not None:
            if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISDIR(metadata.st_mode):
                failures.append(AcceptanceFailure(
                    "timeout liveness lease directory is unsafe"))
            elif any(self.directory.iterdir()):
                failures.append(AcceptanceFailure(
                    "timeout liveness lease directory retained unexpected entries"))
        return not failures

    def observe(self) -> None:
        require(set(self.leases) == {"parent", "child"},
                "timeout liveness leases are unavailable")
        for lease in self.leases.values():
            lease.observe()

    def ready(self) -> bool:
        self.observe()
        for lease in self.leases.values():
            require(not lease.eof,
                    f"{lease.label} FIFO closed before the task was observed running")
        return all(lease.ready and not lease.eof for lease in self.leases.values())

    def receipt(self, closed: bool) -> dict[str, object]:
        resources: dict[str, dict[str, object]] = {}
        for name in ("parent", "child"):
            lease = self.leases.get(name)
            require(lease is not None and type(lease.device) is int and
                    type(lease.inode) is int,
                    f"timeout {name} FIFO identity is unavailable")
            resources[name] = {
                "path": str(lease.path), "device": lease.device, "inode": lease.inode,
                "type": "fifo", "ready": lease.ready, "closed": lease.eof,
            }
        return {
            "schema_version": 1, "resources": resources,
            "ready": all(resource["ready"] is True for resource in resources.values()),
            "closed": closed,
        }

    def capture(self, output: Path) -> bool:
        if not self.ready():
            return False
        self.liveness = self.receipt(closed=False)
        write_json(output / "timeout-liveness.json", self.liveness)
        return True

    def record_liveness(
            self, *, task_id: str, root_id: str, operations: TimeoutStatusSource,
            output: Path, timeout: float = TIMEOUT_LEASE_SETUP_SECONDS,
            sleep_fn=time.sleep, monotonic_fn=time.monotonic) -> None:
        require(root_id is not None, "timeout source has no root identity")
        operations.root_id = root_id
        require(timeout > 0, "timeout liveness setup deadline must be positive")
        deadline = monotonic_fn() + timeout
        next_status = monotonic_fn()
        attempt = 0
        last_status: dict[str, object] | None = None
        while True:
            remaining = deadline - monotonic_fn()
            if remaining <= 0:
                break
            if monotonic_fn() >= next_status:
                last_status = operations.status(
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
                        last_status.get("status") == "timed_out" or terminal_failure):
                    raise AcceptanceFailure(
                        "timeout source ended before both liveness leases were ready; Delegate " +
                        _delegate_status_summary(last_status))
                if last_status.get("liveness") == "running" and self.capture(output):
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

    def closed(self) -> bool:
        self.observe()
        return all(lease.ready and lease.eof for lease in self.leases.values())

    def wait_closed(self, output: Path) -> None:
        require(isinstance(self.liveness, dict),
                "timeout liveness readiness was not recorded")
        wait_until(self.closed, timeout=TIMEOUT_LEASE_OBSERVATION_SECONDS,
                   description="both timeout liveness FIFO leases to close")
        receipt = self.receipt(closed=True)
        receipt["closed_observed_ns"] = time.time_ns()
        write_json(output / "timeout-liveness.json", receipt, replace=True)
        self.liveness = receipt

    def write_brief(self, briefs: Path, name: str, resume: bool = False) -> Path:
        path = briefs / f"{name}.txt"
        if resume:
            text = (
                "Continue the exact previous Codex session. The prior turn intentionally timed out after "
                "creating its marker. Verify acceptance-timeout-marker.txt, then create "
                "acceptance-timeout-resumed.txt containing exactly "
                f"`{self.resume_marker}` followed by one newline. Reply exactly "
                "`DELEGATION_TIMEOUT_RESUMED` after the file is written. Do not modify any other file, "
                "configuration, or path outside the current workspace.\n"
            )
        else:
            command = (
                "set -eu; "
                f"exec 3>{shlex.quote(str(self.parent_fifo))}; "
                f"printf '%s\\n' {shlex.quote(self.parent_ready_marker)} >&3; "
                f"(exec 3>&-; exec 4>{shlex.quote(str(self.child_fifo))}; "
                f"printf '%s\\n' {shlex.quote(self.child_ready_marker)} >&4; "
                "exec sleep 600) & "
                f"printf '%s\\n' {shlex.quote(self.marker)} > "
                f"{shlex.quote(self.marker_file.name)}; wait")
            text = (
                "This is a timeout-recovery integration test. Use the native shell tool to run exactly this "
                f"command: `{command}`. The long sleep is intentional: do not stop it, do not reply before "
                "the supervisor budget ends the turn, and do not modify any other file, configuration, or "
                "path outside the current workspace.\n"
            )
        write_bytes(path, text.encode())
        return path
