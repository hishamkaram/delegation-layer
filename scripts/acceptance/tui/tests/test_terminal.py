"""Native descriptor and output-mode contracts without launching Delegate."""

import json
import os
from pathlib import Path
import sys
import tempfile
import termios
import unittest
from unittest.mock import patch

from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.processes.contracts import TerminalIO
from scripts.acceptance.processes.operations import Processes
from scripts.acceptance.tui.terminal import (
    MAX_TERMIOS_DIAGNOSTIC_BYTES,
    Terminal,
    TerminalTermiosMismatch,
    plain_text,
    restored_modes,
)


class TerminalAcceptanceTests(unittest.TestCase):
    def test_control_sequences_are_removed_and_exit_modes_are_checked(self):
        output = b"\x1b[?1049h\x1b[?25l\x1b[?2004hhello\x1b]11;rgb:0000/0000/0000\x07"
        self.assertFalse(restored_modes(output))
        self.assertEqual(plain_text(output), "hello")
        self.assertTrue(restored_modes(output + b"\x1b[?1049l\x1b[?2004l\x1b[?25h"))

    def test_termios_observation_survives_unavailable_slave_descriptor(self):
        terminal = Terminal()
        try:
            terminal.data.extend(b"\x1b[?1049h\x1b[?1049l")
            os.close(terminal.slave)
            terminal.slave = -1
            terminal.assert_restored()
        finally:
            terminal.close()

    def test_live_observer_rejects_changed_termios_after_slave_closes(self):
        terminal = Terminal()
        try:
            changed = termios.tcgetattr(terminal.slave)
            changed[3] ^= termios.ECHO
            termios.tcsetattr(terminal.slave, termios.TCSANOW, changed)
            terminal.data.extend(b"\x1b[?1049h\x1b[?1049l")
            os.close(terminal.slave)
            terminal.slave = -1
            with self.assertRaises(TerminalTermiosMismatch):
                terminal.assert_restored()
        finally:
            terminal.close()

    def test_mismatch_preserves_exact_comparison_and_reports_numeric_slot_types(self):
        terminal = Terminal()
        before = [1, 2, 3, 4, 9600, 9600, [b"\x03", 1]]
        after = [1, 2, 3, 4, 9600, 9600, [3, b"\x01"]]
        try:
            terminal.original = before
            with patch("scripts.acceptance.tui.terminal.termios.tcgetattr", return_value=after) as read:
                with self.assertRaises(TerminalTermiosMismatch) as raised:
                    terminal.assert_restored()
            read.assert_called_once_with(terminal.master)
            evidence = json.loads(str(raised.exception).split("; ", 1)[1])
            self.assertEqual(evidence["before"]["cc"], [["bytes", 3], ["int", 1]])
            self.assertEqual(evidence["after"]["cc"], [["int", 3], ["bytes", 1]])
            self.assertEqual(evidence["before"]["lflag"], 4)
            self.assertEqual(evidence["after"]["ospeed"], 9600)
            self.assertEqual(before, [1, 2, 3, 4, 9600, 9600, [b"\x03", 1]])
            self.assertEqual(after, [1, 2, 3, 4, 9600, 9600, [3, b"\x01"]])
        finally:
            terminal.close()

    def test_termios_diagnostic_has_bounded_numeric_metadata(self):
        before = [(1 << 64) - 1] * 6 + [[b"\xff"] * 64]
        after = [-(1 << 63)] * 6 + [[255] * 64]
        error = TerminalTermiosMismatch(before, after)
        self.assertLessEqual(len(str(error).encode("ascii")), MAX_TERMIOS_DIAGNOSTIC_BYTES)
        evidence = json.loads(str(error).split("; ", 1)[1])
        self.assertEqual(len(evidence["before"]["cc"]), 64)
        self.assertEqual(evidence["after"]["cc"], [["int", 255]] * 64)
        with self.assertRaisesRegex(RuntimeError, "control slots exceeded bound"):
            TerminalTermiosMismatch(before[:6] + [[b"\xff"] * 65], after)
        with self.assertRaisesRegex(RuntimeError, "control attribute is invalid"):
            TerminalTermiosMismatch(before[:6] + [[b"private-input"]], after)

    def test_terminal_ioctl_error_still_propagates_without_mismatch(self):
        terminal = Terminal()
        error = termios.error(25, "native ioctl unavailable")
        try:
            with patch("scripts.acceptance.tui.terminal.termios.tcgetattr", side_effect=error):
                with self.assertRaises(termios.error) as raised:
                    terminal.assert_restored()
            self.assertIs(raised.exception, error)
        finally:
            terminal.close()

    def test_owned_input_flush_error_is_fatal_before_attribute_observation(self):
        terminal = Terminal()
        error = termios.error(25, "native input flush unavailable")
        try:
            terminal.data.extend(b"\x1b[?1049h\x1b[?1049l")
            with (
                patch("scripts.acceptance.tui.terminal.termios.tcflush", side_effect=error) as flush,
                patch("scripts.acceptance.tui.terminal.termios.tcgetattr", wraps=termios.tcgetattr) as read,
            ):
                with self.assertRaises(termios.error) as raised:
                    terminal.assert_restored()
            flush.assert_called_once_with(terminal.master, termios.TCIFLUSH)
            read.assert_not_called()
            self.assertIs(raised.exception, error)
        finally:
            terminal.close()

    def test_owned_input_flush_cannot_restore_raw_configuration(self):
        terminal = Terminal()
        try:
            raw = termios.tcgetattr(terminal.slave)
            raw[3] &= ~(termios.ICANON | termios.ECHO)
            termios.tcsetattr(terminal.slave, termios.TCSANOW, raw)
            terminal.data.extend(b"\x1b[?1049h\x1b[?1049l")
            with patch("scripts.acceptance.tui.terminal.termios.tcflush", wraps=termios.tcflush) as flush:
                with self.assertRaises(TerminalTermiosMismatch):
                    terminal.assert_restored()
            flush.assert_called_once_with(terminal.master, termios.TCIFLUSH)
            self.assertEqual(termios.tcgetattr(terminal.master)[3] & (termios.ICANON | termios.ECHO), 0)
        finally:
            terminal.close()

    def test_terminal_port_preserves_shared_process_wait_and_separate_stderr(self):
        owner = DirectoryOwner()
        root = Path(tempfile.mkdtemp(prefix="tui-terminal-port-")).resolve(strict=True)
        owner.adopt_created("terminal port test", root, root.parent)
        book = Processes(root / "captures", environment={}, owner=owner, environment_keys_only=True)
        terminal = Terminal()
        try:
            source = "import os,sys; assert os.isatty(0) and os.isatty(1); print('terminal-port'); print('separate-error',file=sys.stderr)"
            child = book.start("terminal", [str(Path(sys.executable).resolve()), "-c", source], root, terminal=terminal.stdio)
            terminal.wait_text("terminal-port")
            self.assertEqual(child.wait(timeout=5), 0)
            self.assertEqual(child.stderr_path.read_bytes(), b"separate-error\n")
            self.assertEqual(child.stdout_path.read_bytes(), b"")
            self.assertTrue(book.finished_naturally())
            with self.assertRaisesRegex(ValueError, "terminal stdio"):
                book.start("bad-stdin", [str(Path(sys.executable).resolve())], root, stdin=b"", terminal=terminal.stdio)
            with self.assertRaises(ValueError):
                book.start("bad-port", [str(Path(sys.executable).resolve())], root, terminal=TerminalIO(-1, -1))
            self.assertEqual(len(book.entries), 1)
        finally:
            terminal.close()
            if book.drain(timeout=5):
                owner.release()
                raise RuntimeError("terminal port child closure unproven; retained " + str(root))
            owner.rollback()
