"""Boundary checks for native task proof evidence, without launching providers."""

import fcntl
from pathlib import Path
import struct
import tempfile
import termios
import unittest
from unittest.mock import MagicMock, patch

from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.processes.operations import Processes
from scripts.acceptance.tui.tasks import Keyboard, TaskClosureUnproven, TaskProof, preview_identity


class NativeTaskProofTests(unittest.TestCase):
    def test_history_readback_waits_for_applied_query_and_fresh_body(self):
        with tempfile.TemporaryDirectory(prefix="tui-history-phase-") as temporary:
            root = Path(temporary)
            owner = DirectoryOwner()
            book = Processes(root / "captures", environment={}, owner=owner, environment_keys_only=True)
            proof = TaskProof(book, root / "delegate", root / "pueue", root / "pueued", root)
            task_id = "a" * 32
            keyboard = MagicMock(spec=Keyboard)
            terminal = keyboard.terminal = MagicMock()
            # The initial frame already contains a detail caption. It cannot
            # prove that the later applied query selected this exact task.
            terminal.data = bytearray(b"Task ID Admission Request/context old task")
            phase = {"state": "initial"}

            def send(data):
                terminal.data.extend(data)
                if data == b"?":
                    self.assertEqual(phase["state"], "applied", "readback advanced before query acknowledgement")
                elif data == b"\x1b":
                    self.assertEqual(phase["state"], "help", "readback returned before a distinct body was observed")
                    phase["state"] = "returned"

            def observe(text, *, after=0, **_kwargs):
                if text == task_id + "  (/ edit)":
                    self.assertGreater(after, 0)
                    phase["state"] = "applied"
                elif text == "Delegate terminal workbench":
                    phase["state"] = "help"
                elif text in ("Request/context", "Task: " + task_id):
                    self.assertGreater(after, 0)
                    self.assertEqual(phase["state"], "returned", "retained caption was accepted as fresh task detail")

            terminal.send.side_effect = send
            terminal.wait_text.side_effect = observe
            keyboard.process = MagicMock()
            keyboard.process.stderr_path = root / "stderr"
            keyboard.process.stderr_path.write_bytes(b"")
            with patch("scripts.acceptance.tui.tasks.Keyboard", return_value=keyboard):
                proof._history_table_case(task_id, 170, 40)
            self.assertEqual(phase["state"], "returned")
            keyboard.close.assert_called_once_with()

    def test_keyboard_sets_requested_dimensions_before_child_start(self):
        owner = DirectoryOwner()
        base = Path(tempfile.mkdtemp(prefix="tui-keyboard-size-")).resolve(strict=True)
        owner.adopt_created("unlaunched terminal size fixture", base, base.parent)
        sizes = []

        def capture_start(*_args, terminal, **_kwargs):
            dimensions = fcntl.ioctl(terminal.input_fd, termios.TIOCGWINSZ, b"\0" * 8)
            sizes.append(struct.unpack("HHHH", dimensions)[:2])
            raise RuntimeError("controlled pre-start stop")

        try:
            book = Processes(base / "captures", environment={}, owner=owner, environment_keys_only=True)
            proof = TaskProof(book, base / "delegate", base / "pueue", base / "pueued", base)
            with patch.object(Processes, "start", side_effect=capture_start), \
                    self.assertRaisesRegex(RuntimeError, "controlled pre-start stop"):
                Keyboard(proof, proof.root, "size", width=170, height=40)
            self.assertEqual(sizes, [(40, 170)])
            self.assertEqual(book.entries, [])
        finally:
            owner.rollback()

    def test_preview_identity_requires_one_exact_ui_resource(self):
        task_id = "a" * 32
        self.assertEqual(preview_identity(b"\x1b[32mTask: " + task_id.encode() + b"\x1b[0m\n", "Task"), task_id)
        self.assertEqual(preview_identity(("Successor: " + task_id + "\nSuccessor: " + task_id).encode(), "Successor"), task_id)
        for data in (b"Task: unknown", b"Task: " + b"a" * 31,
                     ("Task: " + task_id + "\nTask: " + "b" * 32).encode()):
            with self.subTest(data=data), self.assertRaisesRegex(RuntimeError, "exactly one"):
                preview_identity(data, "Task")

    def test_direct_process_completion_cannot_replace_canonical_task_closure(self):
        owner = DirectoryOwner()
        base = Path(tempfile.mkdtemp(prefix="tui-task-closure-")).resolve(strict=True)
        owner.adopt_created("unlaunched task proof fixture", base, base.parent)
        try:
            book = Processes(base / "captures", environment={}, owner=owner, environment_keys_only=True)
            proof = TaskProof(book, base / "delegate", base / "pueue", base / "pueued", base)
            proof.root.mkdir(mode=0o700)
            (proof.root / "root.json").write_bytes(b"{")
            self.assertTrue(book.finished_naturally())
            with self.assertRaises(TaskClosureUnproven) as caught:
                proof.close()
            self.assertTrue(caught.exception.closure_unproven)
            self.assertFalse(proof.closed)
            self.assertTrue(proof.root.exists())
            self.assertEqual(book.entries, [])
        finally:
            # This malformed record was never an admitted task. The creator
            # proved that no command started, so its synthetic data is removable.
            owner.rollback()
