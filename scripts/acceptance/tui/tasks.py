"""Real task operations submitted through the native terminal workbench.

The existing contributor adapter and private Pueue run the finite provider.
This proves product wiring, not compatibility with an external AI account.
"""

from __future__ import annotations

import json
from pathlib import Path
import re
import shlex
import time

from scripts.acceptance.evidence.files import read_json, write_json
from scripts.acceptance.lifetime.closure import validate_managed_lifetime_closure
from scripts.acceptance.processes.operations import Process, Processes
from scripts.acceptance.supervisor.config_policy import config_for
from scripts.acceptance.supervisor.observation import wait_runner_done
from scripts.acceptance.supervisor.queue_policy import done_result
from scripts.acceptance.tasks.publication import task_snapshot, verify_collected_outcome
from scripts.acceptance.tui.terminal import Terminal, plain_text


PROVIDER = "synthetic:contributor-proof"
_TASK_ID = re.compile(r"[0-9a-f]{32}")


class TaskClosureUnproven(RuntimeError):
    """The caller must retain task roots even if every direct CLI has exited."""

    closure_unproven = True


def require(condition: bool, message: str) -> None:
    if not condition:
        raise RuntimeError(message)


def preview_identity(data: bytes, label: str) -> str:
    matches = set(re.findall(r"\b" + re.escape(label) + r": ([0-9a-f]{32})\b", plain_text(data)))
    require(len(matches) == 1, "preview did not identify exactly one " + label)
    return next(iter(matches))


def _wait_path(path: Path, terminal: Terminal, timeout: float = 30) -> None:
    deadline = time.monotonic() + timeout
    while not path.exists():
        require(time.monotonic() < deadline, "native task fixture did not publish " + str(path))
        terminal.drain(0.025)


class Keyboard:
    """Own one terminal until the shared process registry observes exact Wait."""

    def __init__(self, proof: TaskProof, root: Path, name: str, *, width: int = 140, height: int = 60):
        self.terminal = Terminal()
        self.terminal.resize(width, height)
        self.process: Process | None = None
        try:
            self.process = proof.book.start(name, [str(proof.binary), "tui", "--root", str(root),
                                                    "--pueue-config", str(proof.config)],
                                            proof.base, terminal=self.terminal.stdio,
                                            environment=proof.environment)
            self.terminal.wait_text("Tasks")
        except BaseException:
            self.close()
            raise

    def form(self, index: int, label: str, values: list[str]) -> int:
        start = len(self.terminal.data)
        self.terminal.send(b"a")
        self.terminal.wait_text("Actions — Enter open", after=start)
        self.terminal.wait_text(label, after=start)
        self.terminal.send(b"\x1b[B" * index)
        start = len(self.terminal.data)
        self.terminal.send(b"\r")
        self.terminal.wait_text("Ctrl-S review", after=start)
        for i, value in enumerate(values):
            self.terminal.send(b"\x01\x0b" + value.encode())
            if i + 1 < len(values):
                self.terminal.send(b"\t")
        start = len(self.terminal.data)
        self.terminal.send(b"\x13")
        # The renderer may retain an unchanged caption suffix on screen;
        # these distinct footers establish edit -> reviewed form transitions.
        self.terminal.wait_text("Enter confirm", after=start)
        return start

    def confirm(self, *, double: bool = False) -> int:
        start = len(self.terminal.data)
        self.terminal.send(b"\r\r" if double else b"\r")
        return start

    def wait(self, expected: int | set[int], timeout: float = 15) -> None:
        if self.process is None:
            return
        deadline = time.monotonic() + timeout
        while self.process.poll() is None:
            require(time.monotonic() < deadline, "native task TUI did not exit: " + str(self.process.directory))
            self.terminal.drain(0.025)
        self.process.wait(timeout=1, expected=expected)
        self.terminal.assert_restored()
        (self.process.directory / "terminal.bin").write_bytes(self.terminal.data)

    def close(self) -> None:
        try:
            if self.process is not None and self.process.poll() is None:
                self.terminal.send(b"\x03")
                deadline = time.monotonic() + 2
                while self.process.poll() is None and time.monotonic() < deadline:
                    self.terminal.drain(0.025)
                if self.process.poll() is None:
                    # Explicit supported force exit; cancellation of admitted
                    # work remains the task owner's responsibility below.
                    self.terminal.send(b"f\r")
                self.wait({0, 3})
        finally:
            if self.process is not None:
                (self.process.directory / "terminal.bin").write_bytes(self.terminal.data)
            self.terminal.close()


class TaskProof:
    """One isolated queue and the exact source-built fixture task roots."""

    def __init__(self, book: Processes, binary: Path, pueue: Path, pueued: Path, base: Path):
        self.book, self.binary, self.pueue, self.pueued, self.base = book, binary, pueue, pueued, base
        self.root, self.force_root = base / "tui-tasks", base / "tui-force-admission"
        self.quit_root = base / "tui-normal-quit"
        self.runtime, self.workspace, self.queue = (base / name for name in ("tui-provider-runtime", "tui-workspace", "tui-queue"))
        for directory in (self.runtime, self.workspace, self.queue, self.queue / "state", self.queue / "run"):
            directory.mkdir(mode=0o700)
        self.environment = {**book.environment, "DELEGATE_TEST_PUEUE": str(pueue),
                            "DELEGATE_CONTRIBUTOR_RUNTIME": str(self.runtime)}
        self.config = self.queue / "pueue.json"
        write_json(self.config, config_for(self.queue))
        (self.queue / "aliases.yml").write_text("{}\n")
        (self.queue / "aliases.yml").chmod(0o600)
        self.daemon: Process | None = None
        self.closed = False

    def client(self, name: str, *arguments: str, expected: int | set[int] | None = 0,
               timeout: float = 15) -> Process:
        return self.book.run(name, [str(self.pueue), "-c", str(self.config), *arguments],
                             self.base, expected=expected, timeout=timeout, environment=self.environment)

    def control(self, name: str, root: Path, *arguments: str,
                expected: int | set[int] | None = 0, timeout: float = 70) -> Process:
        return self.book.run(name, [str(self.binary), "--root", str(root), "--pueue-config", str(self.config),
                                   *arguments, "--json"], self.base, expected=expected,
                             timeout=timeout, environment=self.environment)

    def start(self) -> None:
        for name, binary in (("pueue", self.pueue), ("pueued", self.pueued)):
            version = self.book.run("tui-" + name + "-version", [str(binary), "--version"], self.base,
                                    environment=self.environment)
            require(version.output().strip() == (name + " 4.0.4").encode(), "native task proof requires pinned Pueue4.0.4")
        self.daemon = self.book.start("tui-task-daemon", [str(self.pueued), "-c", str(self.config)],
                                      self.base, environment=self.environment)
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            require(self.daemon.poll() is None, "owned Pueue exited before readiness")
            status = self.client("tui-queue-ready", "status", "--json", expected={0, 1})
            if status.result is not None and status.result["exit_code"] == 0:
                require(status.json().get("tasks") == {}, "new fixture queue was not empty")
                return
            time.sleep(0.05)
        raise RuntimeError("owned Pueue did not become ready")

    def collect(self, task_id: str, expected: bytes, *, root: Path | None = None) -> dict[str, object]:
        target = self.root if root is None else root
        result = self.control("tui-collect", target, "collect", task_id, "--watch", "60s").json()
        require(isinstance(result, dict), "collection was not a control object")
        _outcome, payload = verify_collected_outcome(result, target / "tasks" / task_id, "committed")
        require(payload == expected, "UI task changed canonical result bytes")
        root_id = result.get("root_id")
        if not isinstance(root_id, str):
            raise RuntimeError("collected task lost its root identity")
        wait_runner_done(lambda name, arguments, *, timeout: self.client(name, *arguments, timeout=timeout),
                         root_id, task_id, timeout=60)
        return result

    def dispatch(self, keyboard: Keyboard, answer: str, nonce: str = "", *, double: bool = False) -> str:
        brief = json.dumps({"case": "present", "answer": answer, "nonce": nonce}, separators=(",", ":"))
        start = keyboard.form(0, "Dispatch local task", [PROVIDER, str(self.workspace), "", brief, "read-only", "60s"])
        task_id = preview_identity(bytes(keyboard.terminal.data[start:]), "Task")
        keyboard.confirm(double=double)
        _wait_path(self.root / "tasks" / task_id / "task.json", keyboard.terminal)
        return task_id

    def task_cases(self) -> list[dict[str, object]]:
        keyboard = Keyboard(self, self.root, "tui-real-tasks")
        try:
            first = self.dispatch(keyboard, "TUI canonical result", "TUI remembered continuation", double=True)
            self.collect(first, b"TUI canonical result")
            require(sorted(entry.name for entry in (self.root / "tasks").iterdir()) == [first],
                    "double Enter created more than one task")
            require(sorted(entry.name for entry in (self.runtime / "launches").iterdir()) == [first + ".json"],
                    "double Enter launched more than one provider turn")
            before = task_snapshot(self.root, first)
            start = keyboard.form(4, "Published result by ID", [first])
            keyboard.confirm()
            keyboard.terminal.wait_text("TUI canonical result", after=start)
            start = keyboard.form(7, "Continue local task", [first, "", '{"case":"resume"}', "60s"])
            successor = preview_identity(bytes(keyboard.terminal.data[start:]), "Successor")
            keyboard.confirm()
            _wait_path(self.root / "tasks" / successor / "task.json", keyboard.terminal)
            self.collect(successor, b"TUI remembered continuation")
            require(task_snapshot(self.root, first) == before, "continuation changed its immutable predecessor")
            self._assert_saved_scopes((first, successor))
            keyboard.terminal.send(b"q")
            keyboard.wait(0)
            require(keyboard.process is not None and keyboard.process.stderr_path.read_bytes() == b"",
                    "task workbench leaked output to stderr")
            return [{"case": "ui-real-dispatch-double-enter-result", "task_id": first,
                     "provider_launches": 1, "canonical_result": "committed"},
                    {"case": "ui-real-continuation", "task_id": successor,
                     "predecessor": first, "canonical_result": "committed", "predecessor_unchanged": True},
                    self.history_layout_case(first, successor)]
        finally:
            keyboard.close()

    def history_layout_case(self, task_id: str, successor: str) -> dict[str, object]:
        # Both committed tasks predate this TUI, so its first discovery pass can
        # find them without depending on the periodic scan's timing.
        before = {identity: task_snapshot(self.root, identity) for identity in (task_id, successor)}
        launches = sorted(entry.name for entry in (self.runtime / "launches").iterdir())
        for width, height in ((170, 40), (120, 35), (80, 24)):
            self._history_table_case(task_id, width, height)
        require(all(task_snapshot(self.root, identity) == evidence for identity, evidence in before.items()),
                "passive history layout changed committed task evidence")
        require(sorted(entry.name for entry in (self.runtime / "launches").iterdir()) == launches,
                "passive history layout launched another provider turn")
        return {"case": "ui-history-table-responsive", "task_id": task_id,
                "initial_sizes": [[170, 40], [120, 35], [80, 24]],
                "kernel_resize_case": "navigate-resize-paste",
                "full_identity_retained": True, "task_evidence_unchanged": True,
                "additional_provider_turns": 0, "termios_restored": True}

    def _history_table_case(self, task_id: str, width: int, height: int) -> None:
        # Keyboard borrows a PTY without claiming its controlling session.
        # Set size before launch; kernel resize delivery is proved separately
        # by navigate-resize-paste's existing controlling-terminal fixture.
        keyboard = Keyboard(self, self.root, "tui-history-" + str(width), width=width, height=height)
        terminal = keyboard.terminal
        try:
            terminal.wait_text("Task ID")
            terminal.wait_text("Admission")
            terminal.wait_text(task_id[:11])
            start = len(terminal.data)
            terminal.send(b"/" + task_id.encode() + b"\r")
            # Search's unchanged prefix may be retained by the diff renderer.
            # This suffix identifies the applied header, not an editor value
            # or the previous-results header's "(/ edit request)" caption.
            terminal.wait_text(task_id + "  (/ edit)", after=start)
            # These initial-size checks intentionally include the fresh
            # terminal's first table frame; task identity readback below uses
            # explicit input/repaint barriers.
            terminal.wait_text("Dated")
            if width >= 155:
                start = len(terminal.data)
                terminal.send(b"?")
                terminal.wait_text("Delegate terminal workbench", after=start)
                # The diff renderer can retain unchanged detail captions.
                # A different body establishes a fresh full-pane readback.
                start = len(terminal.data)
                terminal.send(b"\x1b")
                terminal.wait_text("Request/context", after=start)
                terminal.wait_text("Task: " + task_id, after=start)
            elif width >= 110:
                terminal.wait_text("Created")
            else:
                terminal.wait_text("▶")
                start = len(terminal.data)
                terminal.send(b"\x1b[C" * 4)
                terminal.wait_text("◀", after=start)
                terminal.wait_text("Created", after=start)
                start = len(terminal.data)
                terminal.send(b"\x1b[D" * 4)
                terminal.wait_text("Provider", after=start)
                start = len(terminal.data)
                terminal.send(b"\r")
                terminal.wait_text("Task: " + task_id, after=start)
                start = len(terminal.data)
                terminal.send(b"\x1b")
                terminal.wait_text("Task ID", after=start)
            terminal.send(b"q")
            keyboard.wait(0)
            require(keyboard.process is not None and keyboard.process.stderr_path.read_bytes() == b"",
                    "history layout workbench leaked output to stderr")
        finally:
            keyboard.close()

    def cancel_case(self) -> dict[str, object]:
        keyboard = Keyboard(self, self.root, "tui-real-cancel")
        try:
            (self.runtime / "hold-provider").touch(mode=0o600, exist_ok=False)
            cancelled = self.dispatch(keyboard, "must not complete before cancellation")
            _wait_path(self.runtime / "provider-entered", keyboard.terminal)
            start = keyboard.form(11, "Steer active turn by ID", [cancelled, "", "unsupported fixture steering"])
            keyboard.confirm()
            keyboard.terminal.wait_text("task interaction is unavailable", after=start)
            keyboard.form(6, "Cancel task by ID", [cancelled])
            keyboard.confirm()
            _wait_path(self.root / "tasks" / cancelled / "lifetime" / "terminated.json", keyboard.terminal)
            termination = read_json(self.root / "tasks" / cancelled / "lifetime" / "terminated.json")
            require(isinstance(termination, dict) and termination.get("cause") == "requested",
                    "task ended without the UI-requested cancellation")
            terminal = self.control("tui-cancelled-status", self.root, "status", cancelled).json()
            require(isinstance(terminal, dict) and terminal.get("status") == "interrupted",
                    "UI cancellation did not expose interrupted terminal evidence")
            self._assert_saved_scopes((cancelled,))
            keyboard.terminal.send(b"q")
            keyboard.wait(0)
            process = keyboard.process
            if process is None:
                raise RuntimeError("cancel process identity missing")
            report = process.stderr_path.read_text()
            if report:
                lines = report.splitlines()
                require(len(lines) == 2 and lines[0] == "Operation returned while closing: Cancel task by ID task=" + cancelled and
                        lines[1].startswith("Inspect: "), "cancel workbench emitted unexpected terminal-exit output")
                require(shlex.split(lines[1].removeprefix("Inspect: ")) ==
                        ["delegate", "--root", str(self.root), "status", cancelled, "--json"],
                        "completed cancel reference lost exact context")
            validate_managed_lifetime_closure(self.root, (cancelled,))
            (self.runtime / "hold-provider").unlink()
            (self.runtime / "provider-entered").unlink()
            return {"case": "ui-real-cancel-and-unsupported-interaction", "task_id": cancelled,
                    "status": "interrupted", "unsupported_interaction_refused": True}
        finally:
            keyboard.close()

    def _assert_saved_scopes(self, task_ids: tuple[str, ...], *, root: Path | None = None) -> None:
        target = self.root if root is None else root
        expected = ["DELEGATE_CONTRIBUTOR_RUNTIME=" + str(self.runtime)]
        requests = sorted((target / "inspections").glob("*/request.json"))
        require(bool(requests), "task proof did not run a supervised runtime inspection")
        for path in requests:
            request = read_json(path)
            require(isinstance(request, dict) and isinstance(request.get("binding"), dict),
                    "inspection request binding is unavailable")
            binding = request["binding"]
            require(binding.get("environment") == expected and binding.get("environment_recorded") is True,
                    "inspection lost exact isolated runtime or retained another environment value")
        for task_id in task_ids:
            meta = read_json(target / "tasks" / task_id / "meta.json")
            require(isinstance(meta, dict) and meta.get("environment") == expected and meta.get("environment_recorded") is True,
                    "provider plan lost exact isolated runtime or retained another environment value")

    def normal_quit_case(self) -> dict[str, object]:
        marker = self.runtime / "hold-admission"
        marker.touch(mode=0o600, exist_ok=False)
        keyboard = Keyboard(self, self.quit_root, "tui-normal-inflight-quit")
        try:
            brief = '{"case":"present","answer":"continues after normal UI quit"}'
            start = keyboard.form(0, "Dispatch local task", [PROVIDER, str(self.workspace), "", brief, "read-only", "60s"])
            task_id = preview_identity(bytes(keyboard.terminal.data[start:]), "Task")
            keyboard.confirm()
            entered = self.runtime / "admission-entered"
            _wait_path(entered, keyboard.terminal)
            require(entered.read_text().strip() == task_id, "in-flight quit lost admission identity")
            keyboard.terminal.send(b"q")
            keyboard.terminal.wait_text("force-exit confirmation", after=start)
            marker.unlink()
            keyboard.wait(0, timeout=45)
            process = keyboard.process
            if process is None:
                raise RuntimeError("normal quit process identity missing")
            report = process.stderr_path.read_text()
            require(task_id in report and str(self.quit_root) in report and
                    "Operation returned while closing:" in report and "Unresolved:" not in report,
                    "normal in-flight quit lost or misclassified its completed admission")
            self.collect(task_id, b"continues after normal UI quit", root=self.quit_root)
            self._assert_saved_scopes((task_id,), root=self.quit_root)
            # All preparation and worker owners have settled. Retire only this
            # case's handshake so the separate force-exit case can own its own.
            entered.unlink()
            return {"case": "ui-normal-inflight-quit", "task_id": task_id, "exit_code": 0,
                    "waited_for_admission": True, "admitted_work_committed_after_quit": True,
                    "completed_reference_reported": True}
        finally:
            keyboard.close()

    def force_case(self) -> dict[str, object]:
        marker = self.runtime / "hold-admission"
        marker.touch(mode=0o600, exist_ok=False)
        keyboard = Keyboard(self, self.force_root, "tui-force-real-admission")
        try:
            brief = '{"case":"present","answer":"must not launch after force exit"}'
            start = keyboard.form(0, "Dispatch local task", [PROVIDER, str(self.workspace), "", brief, "read-only", "60s"])
            task_id = preview_identity(bytes(keyboard.terminal.data[start:]), "Task")
            keyboard.confirm()
            _wait_path(self.runtime / "admission-entered", keyboard.terminal)
            require((self.runtime / "admission-entered").read_text().strip() == task_id, "admission barrier changed task identity")
            keyboard.terminal.send(b"q")
            keyboard.terminal.wait_text("force-exit confirmation", after=start)
            keyboard.terminal.send(b"f\r")
            keyboard.wait(3)
            process = keyboard.process
            if process is None:
                raise RuntimeError("force-exit process identity missing")
            report = process.stderr_path.read_text()
            require(task_id in report and str(self.force_root) in report, "forced exit lost exact task/root reconciliation")
            require(not (self.force_root / "tasks" / task_id).exists(), "held pre-admission unexpectedly created a task")
            marker.unlink()
            absent = self.control("tui-force-absent", self.force_root, "status", task_id, expected=1).json()
            require(isinstance(absent, dict) and absent.get("admission") == "unknown" and absent.get("publication") == "unknown",
                    "force-exit readback invented an admitted result")
            return {"case": "ui-force-real-admission", "task_id": task_id, "exit_code": 3,
                    "task_absent_after_exit": True, "exact_reconciliation": True}
        finally:
            keyboard.close()

    def close(self) -> dict[str, object]:
        if self.closed:
            return {}
        try:
            for root in (self.root, self.quit_root, self.force_root):
                tasks = root / "tasks"
                if tasks.exists():
                    for entry in tasks.iterdir():
                        require(_TASK_ID.fullmatch(entry.name) is not None, "unexpected private task entry")
                        if not (entry / "outcome.json").exists():
                            self.control("tui-cleanup-cancel", root, "cancel", entry.name, expected=None)
                            self.control("tui-cleanup-collect", root, "collect", entry.name, "--watch", "30s", expected=None)
            proof: dict[str, object] = {str(root): validate_managed_lifetime_closure(root)
                                        for root in (self.root, self.quit_root, self.force_root) if (root / "root.json").exists()}
            if self.daemon is not None:
                self._close_queue()
            self.closed = True
            return proof
        except BaseException as error:
            raise TaskClosureUnproven("native task/inspection closure unproven; retained " + str(self.base)) from error

    def capture_failure_logs(self, error: BaseException) -> None:
        daemon = self.daemon
        if daemon is None or daemon.poll() is not None:
            return
        try:
            # Only this private queue contains finite fixture output. Keep a
            # bounded tail; never request Pueue's unbounded --full log mode.
            self.client("tui-worker-failure-logs", "log", "--all", "--lines", "40", "--json")
        except BaseException as capture_error:
            error.add_note("private fixture log capture failed: " + str(capture_error)[:1024])

    def _close_queue(self) -> None:
        deadline = time.monotonic() + 30
        while True:
            status = self.client("tui-final-queue", "status", "--json").json()
            require(isinstance(status, dict) and isinstance(status.get("tasks"), dict), "private queue status is invalid")
            rows = status["tasks"]
            require(all(isinstance(row, dict) for row in rows.values()), "private queue contains invalid row data")
            if all(done_result(row) is not None for row in rows.values()):
                break
            require(time.monotonic() < deadline, "private queue still owns active or unknown work")
            time.sleep(0.05)
        self.client("tui-private-shutdown", "shutdown")
        daemon = self.daemon
        if daemon is None:
            raise RuntimeError("private daemon identity lost")
        daemon.wait(timeout=15, expected=0)


def run_task_cases(book: Processes, fixture_binary: Path, pueue: Path, pueued: Path,
                   base: Path) -> list[dict[str, object]]:
    proof = TaskProof(book, fixture_binary, pueue, pueued, base)
    try:
        proof.start()
        cases = proof.task_cases()
        cases.append(proof.cancel_case())
        cases.append(proof.normal_quit_case())
        cases.append(proof.force_case())
    except BaseException as error:
        proof.capture_failure_logs(error)
        closure = proof.close()
        write_json(book.directory / "task-failure.json", {"error": str(error)[:4096], "closure": closure})
        raise
    closure = proof.close()
    write_json(book.directory / "task-lifetime-closure.json", closure)
    cases.append({"case": "ui-fixture-task-cleanup", "canonical_lifetimes_closed": True,
                  "private_daemon_natural_shutdown": True,
                  "unverified": ["live native clarification: contributor fixture has no interaction capability",
                                 "external AI providers: this is a finite test-only provider"]})
    return cases
