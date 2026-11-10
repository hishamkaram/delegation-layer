"""Exercise the installed source-built CLI with real native PTY descriptors."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import sys
import tempfile
import termios
import time

from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.evidence.files import read_json, write_json
from scripts.acceptance.processes.operations import Process, Processes
from scripts.acceptance.tui.terminal import Terminal, TerminalTermiosMismatch


def require(condition: bool, message: str) -> None:
    if not condition:
        raise RuntimeError(message)


class ClosureUnproven(RuntimeError):
    """Retain fixtures if the guardian cannot prove its exact child's closure."""


def wait_child(process: Process, terminal: Terminal, timeout: float = 10) -> None:
    deadline = time.monotonic() + timeout
    while process.poll() is None:
        if time.monotonic() >= deadline:
            raise RuntimeError("TUI process did not close; retained " + str(process.directory))
        terminal.drain(0.025)
    process.wait(timeout=1)


def terminal_case_start(book: Processes, binary: Path, helper: Path, base: Path,
                        name: str, mode: str, terminal: Terminal) -> tuple[Process, Path, Path, Path]:
    root, trigger, receipt = (base / (name + suffix) for suffix in ("-state", "-trigger", "-receipt.json"))
    process = book.start(name, [str(helper), "--test-tui-terminal", mode, str(binary), str(root), str(trigger), str(receipt)],
                         base, terminal=terminal.stdio)
    return process, root, trigger, receipt


def verify_terminal_child(process: Process, receipt_path: Path, expected: int) -> None:
    try:
        state = read_json(receipt_path, bound=4096)
    except (OSError, RuntimeError, ValueError) as error:
        raise ClosureUnproven("native terminal closure receipt unavailable; retained " + str(receipt_path.parent)) from error
    if not isinstance(state, dict) or state.get("exact_wait") is not True or state.get("scope_empty") is not True:
        raise ClosureUnproven("native terminal exact Wait or scope closure unproven; retained " + str(receipt_path.parent))
    write_json(process.directory / "guardian-receipt.json", state)
    require(state.get("child_exit_code") == expected, "native TUI returned an unexpected exit")
    require(state.get("failure_cleanup") is False, "native TUI required failure cleanup")


def unrestored_termios_case(book: Processes, helper: Path, base: Path) -> dict[str, object]:
    """A completed controlling child must leave observable RAW evidence."""
    terminal = Terminal()
    process = None
    receipt_path = base / "unrestored-termios-rejected-receipt.json"
    try:
        flags = termios.ICANON | termios.ECHO
        baseline = terminal.original[3]
        require(isinstance(baseline, int) and baseline & flags == flags,
                "negative control requires canonical input and echo initially")
        process, root, _trigger, receipt_path = terminal_case_start(
            book, helper, helper, base, "unrestored-termios-rejected", "raw-exit", terminal)
        wait_child(process, terminal)
        verify_terminal_child(process, receipt_path, 0)
        observed = termios.tcgetattr(terminal.master)[3]
        require(isinstance(observed, int) and observed & flags == 0,
                "completed RAW child lost its canonical/echo counterexample")
        try:
            terminal.assert_restored()
        except TerminalTermiosMismatch:
            pass
        else:
            raise RuntimeError("unrestored RAW terminal incorrectly passed restoration")
        require(process.stderr_path.read_bytes() == b"", "unexpected RAW fixture stderr")
        require(not root.exists(), "RAW terminal control initialized execution state")
        return {"case": "unrestored-termios-rejected", "exit_code": 0,
                "exact_wait": True, "scope_empty": True, "raw_flags_observed": True,
                "termios_mismatch_rejected": True, "execution_root_absent": True}
    finally:
        try:
            if process is not None:
                if process.poll() is None:
                    terminal.hangup()
                    wait_child(process, terminal, timeout=20)
                (process.directory / "terminal.bin").write_bytes(terminal.data)
                if not (process.directory / "guardian-receipt.json").exists():
                    verify_terminal_child(process, receipt_path, 0)
        finally:
            terminal.close()


def nonleader_unrestored_termios_case(book: Processes, helper: Path, base: Path) -> dict[str, object]:
    """Match Keyboard's direct child; the fixed RAW role launches no children."""
    terminal = Terminal()
    process = None
    try:
        flags = termios.ICANON | termios.ECHO
        baseline = terminal.original[3]
        require(isinstance(baseline, int) and baseline & flags == flags,
                "nonleader control requires canonical input and echo initially")
        process = book.start("unrestored-nonleader-termios-rejected",
                             [str(helper), "--test-tui-terminal-raw-exit"], base,
                             terminal=terminal.stdio, start_new_session=False)
        wait_child(process, terminal)
        observed = termios.tcgetattr(terminal.master)[3]
        require(isinstance(observed, int) and observed & flags == 0,
                "completed nonleader RAW child lost its canonical/echo counterexample")
        try:
            terminal.assert_restored()
        except TerminalTermiosMismatch:
            pass
        else:
            raise RuntimeError("unrestored nonleader RAW terminal incorrectly passed restoration")
        require(process.stderr_path.read_bytes() == b"", "unexpected nonleader RAW fixture stderr")
        return {"case": "unrestored-nonleader-termios-rejected", "exit_code": 0,
                "exact_wait": True, "fixed_role_launches_no_children": True,
                "raw_flags_observed": True, "termios_mismatch_rejected": True}
    finally:
        try:
            if process is not None:
                if process.poll() is None:
                    terminal.hangup()
                    wait_child(process, terminal, timeout=20)
                (process.directory / "terminal.bin").write_bytes(terminal.data)
        finally:
            terminal.close()


def interactive_case(book: Processes, binary: Path, helper: Path, base: Path, name: str,
                     *, navigation: bool = False, ctrl_c: bool = False) -> dict[str, object]:
    terminal = Terminal()
    process = None
    receipt_path = base / (name + "-receipt.json")
    try:
        process, root, _trigger, receipt_path = terminal_case_start(book, binary, helper, base, name, "interactive", terminal)
        terminal.wait_text("Tasks")
        if navigation:
            start = len(terminal.data)
            terminal.send(b"\t")
            terminal.wait_text("Team", after=start)
            terminal.send(b"\t\t\t")
            terminal.send(b"a")
            terminal.wait_text("Dispatch local task")
            terminal.send(b"\r")
            terminal.wait_text("Provider")
            terminal.send(b"\x1b[200~claude:print\xe4\xb8\x96\xe7\x95\x8c\x1b[201~")
            terminal.wait_text("世界")
            terminal.send(b"\x0f")
            # Advanced fields remain reached by native keyboard traversal;
            # they need not fit in the current visible form viewport.
            for _ in range(7):
                terminal.send(b"\t")
            terminal.wait_text("Requested model")
            start = len(terminal.data)
            terminal.send(b"\x1b")
            terminal.wait_text("No records in this view.", after=start)
            # The lifetime fixture gives this exact child its own controlling
            # PTY. Native TIOCSWINSZ delivers kernel SIGWINCH; no signal API.
            start = len(terminal.data)
            terminal.resize(24, 8)
            terminal.wait_text("Terminal too small", after=start)
            start = len(terminal.data)
            terminal.resize(110, 35)
            terminal.wait_text("Tasks", after=start)
        terminal.send(b"\x03" if ctrl_c else b"q")
        wait_child(process, terminal)
        verify_terminal_child(process, receipt_path, 0)
        terminal.assert_restored()
        require(process.stderr_path.read_bytes() == b"", "unexpected TUI stderr: " + str(process.stderr_path))
        require(not root.exists(), "passive TUI navigation initialized execution state")
        transcript = process.directory / "terminal.bin"
        transcript.write_bytes(terminal.data)
        return {"case": name, "exit_code": 0, "termios_restored": True,
                "terminal_modes_restored": True, "transcript_bytes": len(terminal.data),
                "execution_root_absent": True, "kernel_resize": navigation}
    finally:
        try:
            if process is not None and process.poll() is None:
                # Cooperative UI shutdown only. Failure retains the exact
                # process and captures, never unproven directory removal.
                terminal.send(b"\x03q\r")
                try:
                    wait_child(process, terminal, timeout=5)
                finally:
                    if process.poll() is None:
                        (process.directory / "terminal-incomplete.bin").write_bytes(terminal.data)
            if process is not None and process.poll() is not None and not (process.directory / "guardian-receipt.json").exists():
                verify_terminal_child(process, receipt_path, 0)
        finally:
            try:
                if process is not None:
                    (process.directory / "terminal.bin").write_bytes(terminal.data)
            finally:
                terminal.close()


def guardian_case(book: Processes, binary: Path, helper: Path, base: Path, mode: str) -> dict[str, object]:
    """The test-only lifetime owner requests one identity-verified signal."""
    terminal = Terminal()
    process = None
    name = "guardian-" + mode
    trigger, receipt_path = (base / (name + suffix) for suffix in ("-trigger", "-receipt.json"))
    expected = 143 if mode == "term" else 129
    try:
        process, root, trigger, receipt_path = terminal_case_start(book, binary, helper, base, name, mode, terminal)
        terminal.wait_text("Tasks")
        if mode == "hangup":
            terminal.hangup()
        else:
            trigger.touch(exist_ok=False)
        wait_child(process, terminal, timeout=20)
        verify_terminal_child(process, receipt_path, expected)
        if mode != "hangup":
            terminal.assert_restored()
        require(process.stderr_path.read_bytes() == b"", "unexpected guardian/TUI stderr: " + str(process.stderr_path))
        require(not root.exists(), "guardian TUI initialized execution state")
        return {"case": name, "exit_code": expected,
                "terminal_restoration": "master disappeared" if mode == "hangup" else "termios-and-modes-restored",
                "execution_root_absent": True}
    finally:
        try:
            if process is not None:
                if process.poll() is None:
                    if mode == "hangup":
                        terminal.hangup()
                    elif not trigger.exists():
                        trigger.touch(exist_ok=False)
                    wait_child(process, terminal, timeout=20)
                (process.directory / "terminal.bin").write_bytes(terminal.data)
                if not (process.directory / "guardian-receipt.json").exists():
                    verify_terminal_child(process, receipt_path, expected)
        finally:
            terminal.close()


def run(binary: Path, helper: Path, output: Path, *, task_binary: Path | None = None,
        pueue: Path | None = None, pueued: Path | None = None) -> dict[str, object]:
    require(binary.is_absolute() and binary.is_file(), "an installed source-built binary is required")
    require(helper.is_absolute() and helper.is_file(), "the installed guardian fixture helper is required")
    require(all(path is None for path in (task_binary, pueue, pueued)) or
            all(path is not None and path.is_absolute() and path.is_file() for path in (task_binary, pueue, pueued)),
            "task acceptance requires installed --task-binary, --pueue and --pueued paths together")
    require(not output.exists() and not output.is_symlink(), "acceptance output must be new")
    output.mkdir(mode=0o700, parents=True)
    owner = DirectoryOwner()
    base = Path(tempfile.mkdtemp(prefix="delegate-tui-")).resolve(strict=True)
    owner.adopt_created("native TUI fixture", base, base.parent)
    captures = DirectoryOwner()
    book = None
    cases = []
    try:
        home = owner.create("isolated user home", base / "home", base)
        environment = {"PATH": os.environ.get("PATH", os.defpath), "HOME": str(home.directory),
                       "XDG_CONFIG_HOME": str(home.directory / "config"),
                       "XDG_CACHE_HOME": str(home.directory / "cache"),
                       "TERM": "xterm-256color", "LANG": "C.UTF-8", "NO_COLOR": "1"}
        book = Processes(output / "processes", environment=environment, owner=captures, environment_keys_only=True)
        cases.append(nonleader_unrestored_termios_case(book, helper, base))
        cases.append(unrestored_termios_case(book, helper, base))
        cases.append(interactive_case(book, binary, helper, base, "navigate-resize-paste", navigation=True))
        cases.append(interactive_case(book, binary, helper, base, "control-c", ctrl_c=True))
        cases.extend(guardian_case(book, binary, helper, base, mode) for mode in ("term", "hup", "hangup"))
        root = base / "non-tty-state"
        process = book.run("non-tty", [str(binary), "tui", "--root", str(root)], base, expected=2)
        require(b"\x1b" not in process.output(), "non-TTY command emitted terminal escapes")
        require(b"requires terminal" in process.stderr_path.read_bytes(), "non-TTY refusal is not actionable")
        require(not root.exists(), "non-TTY refusal initialized state")
        cases.append({"case": "non-tty", "exit_code": 2, "execution_root_absent": True})
        if task_binary is not None and pueue is not None and pueued is not None:
            from scripts.acceptance.tui.tasks import run_task_cases
            cases.extend(run_task_cases(book, task_binary, pueue, pueued, base))
        require(book.finished_naturally(), "native TUI child closure is unproven")
        owner.rollback()
        receipt = {"platform": sys.platform, "binary": str(binary), "cases": cases,
                   "fixture_cleanup": "closed-and-removed", "real_provider_tasks_launched": 0,
                   "unverified": (["interactive fixture-provider dispatch (no task fixture supplied)"]
                                  if task_binary is None else [])}
        write_json(output / "receipt.json", receipt)
        return receipt
    except BaseException as error:
        if isinstance(error, ClosureUnproven) or getattr(error, "closure_unproven", False) is True or book is not None and book.drain(timeout=5):
            owner.release()
        else:
            owner.rollback()
        raise
    finally:
        captures.release()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--control-helper", type=Path, required=True)
    parser.add_argument("--task-binary", type=Path)
    parser.add_argument("--pueue", type=Path)
    parser.add_argument("--pueued", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    print(json.dumps(run(args.binary.resolve(strict=True), args.control_helper.resolve(strict=True), args.output.resolve(),
                         task_binary=args.task_binary.resolve(strict=True) if args.task_binary else None,
                         pueue=args.pueue.resolve(strict=True) if args.pueue else None,
                         pueued=args.pueued.resolve(strict=True) if args.pueued else None), sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
