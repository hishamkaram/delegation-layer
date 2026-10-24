"""Release control commands run in the supplied directory and within their capture bound."""

import contextlib
import io
import os
from pathlib import Path
import subprocess
import sys
import unittest
from unittest.mock import patch

from scripts.acceptance_tests.release_fixture import FixtureTree, open_descriptors
from scripts.delegate_acceptance.processes.contracts import CommandFailure
from scripts.delegate_acceptance.processes.operations import run_bounded_command
from scripts.delegate_acceptance.release_verification.commands import command
from scripts.delegate_acceptance.release_verification.contract import MAX_OUTPUT, VerificationError


def writer(size: int, *, exit_code: int = 0) -> list[str]:
    """A finite child that writes exactly ``size`` bytes to stdout, then exits."""
    return [sys.executable, "-c",
            f"import sys; sys.stdout.buffer.write(b'x' * {size}); sys.stdout.flush(); sys.exit({exit_code})"]


PRINT_CWD = [sys.executable, "-c", "import os; print(os.getcwd())"]


class ReleaseCommandDirectoryTests(unittest.TestCase):
    """Each child runs in the test's FixtureTree, which observes its original failure."""

    def setUp(self):
        self.baseline = open_descriptors()
        self.tree = FixtureTree.open(prefix="release-command-cwd", label="release command directory")
        self.addCleanup(self.tree.dispose)

    def forward(self, receipt: dict) -> bytes:
        return self.tree.observe_operation(lambda: command(
            receipt, "cwd-control", PRINT_CWD, dict(os.environ), capture=True, cwd=self.tree.root,
        ))

    def test_control_child_uses_explicit_directory(self):
        original = Path.cwd()
        expected = self.tree.root
        result = self.tree.observe_operation(lambda: run_bounded_command(
            PRINT_CWD, dict(os.environ), max_output=4096, capture=True, cwd=expected,
        ))
        self.assertEqual(result.exit_code, 0)
        self.assertEqual(Path(result.output.decode().strip()), expected)
        self.assertEqual(Path.cwd(), original)

    def test_release_wrapper_forwards_directory(self):
        receipt = {"commands": []}
        expected = self.tree.root
        data = self.forward(receipt)
        self.assertEqual(Path(data.decode().strip()), expected)
        self.assertEqual(receipt["commands"][0]["cwd"], str(expected))
        self.assertTrue(self.tree.dispose())
        self.assertFalse(expected.exists())
        self.assertEqual(open_descriptors(), self.baseline)
        self.assertEqual(receipt["commands"][0]["exit_code"], 0)

    def test_unknown_child_wait_retains_its_directory_until_closure_is_proven(self):
        refused = []

        def refuse(argv, **options):
            refused.append(argv)
            raise subprocess.TimeoutExpired(argv, 0)

        receipt = {"commands": []}
        with patch.object(subprocess, "Popen", side_effect=refuse), \
                self.assertRaises(VerificationError) as raised:
            self.forward(receipt)
        self.assertEqual(raised.exception.code, CommandFailure.TIMEOUT.value)
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertTrue(self.doCleanups())
        self.assertTrue(self.tree.root.is_dir(), "an unknown child Wait must retain its working directory")
        self.assertIs(self.tree.unresolved, raised.exception)
        self.assertEqual(open_descriptors(), self.baseline)
        # The proof: the one child start was refused, so no child can still write to the directory.
        self.assertEqual(refused, [PRINT_CWD])
        self.tree.remove_retained()
        self.assertFalse(self.tree.root.exists())


class ReleaseCommandBoundTests(unittest.TestCase):
    def run_command(self, argv, **kwargs):
        receipt = {"commands": []}
        return receipt, command(receipt, "bound-control", argv, dict(os.environ), **kwargs)

    def assertRefused(self, code, argv, **kwargs):
        receipt = {"commands": []}
        with self.assertRaises(VerificationError) as caught:
            command(receipt, "bound-control", argv, dict(os.environ), **kwargs)
        self.assertEqual((caught.exception.code, caught.exception.stage), (code, "bound-control"))
        self.assertEqual(len(receipt["commands"]), 1)
        return receipt["commands"][0]

    def test_default_bound_is_unchanged(self):
        receipt, data = self.run_command(writer(MAX_OUTPUT), capture=True)
        self.assertEqual(len(data), MAX_OUTPUT)
        self.assertEqual(receipt["commands"][0]["exit_code"], 0)
        record = self.assertRefused("command_output_exceeded", writer(MAX_OUTPUT + 1), capture=True)
        self.assertIsInstance(record["exit_code"], int)

    def test_explicit_bound_admits_complete_output_above_the_default(self):
        size = MAX_OUTPUT + 1
        try:
            receipt, data = self.run_command(writer(size), capture=True, max_output=size)
        except VerificationError as error:
            self.fail(f"explicit bound refused complete output: {error.code}")
        self.assertEqual(data, b"x" * size)
        self.assertEqual(receipt["commands"][0], {"name": "bound-control", "argv": writer(size), "exit_code": 0})

    def test_explicit_bound_overflow_refuses_without_truncation(self):
        record = self.assertRefused("command_output_exceeded", writer(11), capture=True, max_output=10)
        self.assertIsInstance(record["exit_code"], int)
        self.assertEqual(self.run_command(writer(10), capture=True, max_output=10)[1], b"x" * 10)

    def test_failure_identities_and_exit_codes_are_preserved(self):
        missing = [str(Path(sys.executable).parent / "no-such-release-control-child")]
        for capture in (True, False):
            with self.subTest(capture=capture):
                record = self.assertRefused("command_start_failed", missing, capture=capture, max_output=10)
                self.assertIsNone(record["exit_code"])
                record = self.assertRefused("command_failed", writer(3, exit_code=7), capture=capture,
                                            max_output=10)
                self.assertEqual(record["exit_code"], 7)

    def test_uncaptured_output_is_discarded_regardless_of_bound(self):
        receipt, data = self.run_command(writer(MAX_OUTPUT + 1), capture=False, max_output=0)
        self.assertEqual((data, receipt["commands"][0]["exit_code"]), (b"", 0))


if __name__ == "__main__":
    unittest.main()
