"""Boundary checks for native task proof evidence, without launching providers."""

from pathlib import Path
import tempfile
import unittest

from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.processes.operations import Processes
from scripts.acceptance.tui.tasks import TaskClosureUnproven, TaskProof, preview_identity


class NativeTaskProofTests(unittest.TestCase):
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
