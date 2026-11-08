"""The canonical release gate retains, reports and releases when its action is interrupted.

Each action is synthetic and launches no child; the owned workspace is a real
directory pinned by a real ``DirectoryOwner`` inside a test-owned ``FixtureTree``.
"""

from __future__ import annotations

import errno
import io
import json
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.release_tests.release_fixture import FixtureTree
from scripts.acceptance.testing.resources import open_descriptors
from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.release_verification import cleanup as cleanup
from scripts.release_verification import filesystem as filesystem
from scripts.release_verification.assets import remember_directory
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.contract import new_receipt


SECRET = "private-canary-path"


class InterruptedGateTests(unittest.TestCase):
    """Each test, or each subtest, creates exactly the one owner its gate consumes."""

    def fresh(self) -> None:
        """One new tree, receipt and owner holding one real pinned workspace."""
        self.tree = FixtureTree.open(prefix="release-cleanup", label="release cleanup fixture")
        self.addCleanup(self.tree.dispose)
        # The tree's own root pin stays open; only the gate owner's pins must be consumed.
        self.descriptors = open_descriptors()
        self.receipt_path = self.tree.root / "receipt.json"
        self.workspace = self.tree.root / "workspace"
        self.receipt = new_receipt("published-npm", "v1.2.3", "a" * 40)
        self.receipt["stage"] = "verify-npm"
        self.owner = DirectoryOwner()
        remember_directory(self.receipt, self.owner.create("gate workspace", self.workspace, self.tree.root))
        self.locations = {"assets": self.workspace}
        self.destructive = [patch.object(cleanup, name, side_effect=AssertionError(name))
                            for name in ("prepare_directory_cleanup", "resume_directory_cleanup")]

    def gate(self, fault: BaseException) -> BaseException:
        def action() -> None:
            raise fault

        with self.destructive[0] as prepare, self.destructive[1] as resume, \
                patch("sys.stderr"), self.assertRaises(BaseException) as caught:
            cleanup.finish_gate(self.receipt_path, self.receipt, self.owner, action, "remove", self.locations)
        self.assertEqual((prepare.call_count, resume.call_count), (0, 0))
        return caught.exception

    def assertPinsConsumed(self) -> None:
        self.assertEqual(open_descriptors(), self.descriptors)
        with self.assertRaisesRegex(RuntimeError, "already transferred"):
            self.owner.release()

    def test_an_interrupt_or_exit_escapes_unchanged_after_retaining_and_reporting(self) -> None:
        for fault in (KeyboardInterrupt(), SystemExit(3)):
            with self.subTest(fault=type(fault).__name__):
                self.fresh()
                escaped = self.gate(fault)
                self.assertIs(escaped, fault)
                self.assertFalse(hasattr(escaped, "__notes__"))
                receipt = json.loads(self.receipt_path.read_text())
                self.assertEqual((receipt["verdict"], receipt["failure"]),
                                 ("fail", {"code": "verification_interrupted", "stage": "verify-npm"}))
                self.assertEqual(receipt["cleanup"]["status"], "retained-for-investigation")
                self.assertEqual(receipt["cleanup"]["retained_paths"], [str(self.workspace.resolve())])
                self.assertNotIn("completion", receipt)
                self.assertTrue(self.workspace.is_dir())
                self.assertPinsConsumed()
        assert isinstance(fault, SystemExit)
        self.assertEqual(fault.code, 3)

    def test_a_receipt_write_fault_keeps_the_interrupt_with_an_errno_only_note(self) -> None:
        self.fresh()
        interrupt = KeyboardInterrupt()
        with patch.object(filesystem, "write_receipt", side_effect=OSError(errno.EIO, SECRET)):
            escaped = self.gate(interrupt)
        self.assertIs(escaped, interrupt)
        self.assertEqual(escaped.__notes__, ["interrupted gate retention incomplete: EIO"])
        self.assertFalse(self.receipt_path.exists())
        self.assertTrue(self.workspace.is_dir())
        self.assertPinsConsumed()

    def test_a_release_fault_is_a_type_or_code_note_and_never_hides_the_interrupt(self) -> None:
        for secondary, label in ((RuntimeError(SECRET), "RuntimeError"),
                                 (VerificationError("forced_release_fault", SECRET), "forced_release_fault")):
            with self.subTest(label=label):
                self.fresh()
                interrupt = KeyboardInterrupt()
                real_release = self.owner.release

                def release(real=real_release, secondary=secondary) -> bool:
                    real()
                    raise secondary

                with patch.object(self.owner, "release", side_effect=release), \
                        patch.object(filesystem, "write_receipt", side_effect=secondary):
                    escaped = self.gate(interrupt)
                self.assertIs(escaped, interrupt)
                self.assertEqual(escaped.__notes__, [f"interrupted gate retention incomplete: {label}"])
                self.assertNotIn(SECRET, "".join(escaped.__notes__))
                self.assertTrue(self.workspace.is_dir())
                self.assertEqual(open_descriptors(), self.descriptors)

    def test_ordinary_failures_still_return_one_without_raising(self) -> None:
        for fault, code in ((VerificationError("forced_gate_failure", "verify-npm"), "forced_gate_failure"),
                            (ValueError(SECRET), "verification_internal_error")):
            with self.subTest(code=code):
                self.fresh()

                def action(fault=fault) -> None:
                    raise fault

                with patch("sys.stderr"):
                    self.assertEqual(cleanup.finish_gate(self.receipt_path, self.receipt, self.owner, action,
                                                         "remove", self.locations), 1)
                receipt = json.loads(self.receipt_path.read_text())
                self.assertEqual(receipt["failure"]["code"], code)
                self.assertTrue(self.workspace.is_dir())
                self.assertPinsConsumed()


class InterruptedGateDescriptorTests(unittest.TestCase):
    """The whole gate cohort, cleanups included, leaves the process's descriptor count unchanged."""

    def test_the_interrupted_gate_cohort_releases_every_descriptor_it_opens(self) -> None:
        suite = unittest.defaultTestLoader.loadTestsFromTestCase(InterruptedGateTests)
        before = open_descriptors()
        result = unittest.TextTestRunner(stream=io.StringIO()).run(suite)
        after = open_descriptors()
        self.assertEqual((result.testsRun, result.failures, result.errors), (4, [], []))
        self.assertTrue(result.wasSuccessful())
        self.assertEqual(after, before)


if __name__ == "__main__":
    unittest.main()
