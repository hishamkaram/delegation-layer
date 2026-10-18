"""Release control commands run in the supplied directory, never global chdir."""

import os
from pathlib import Path
import sys
import tempfile
import unittest

from scripts.delegate_acceptance.processes.operations import run_bounded_command
from scripts.delegate_acceptance.release_verification.commands import command


class ReleaseCommandDirectoryTests(unittest.TestCase):
    def test_control_child_uses_explicit_directory(self):
        original = Path.cwd()
        with tempfile.TemporaryDirectory() as temporary:
            expected = Path(temporary).resolve()
            result = run_bounded_command(
                [sys.executable, "-c", "import os; print(os.getcwd())"],
                dict(os.environ), max_output=4096, capture=True, cwd=expected,
            )
            self.assertEqual(result.exit_code, 0)
            self.assertEqual(Path(result.output.decode().strip()), expected)
        self.assertEqual(Path.cwd(), original)

    def test_release_wrapper_forwards_directory(self):
        receipt = {"commands": []}
        with tempfile.TemporaryDirectory() as temporary:
            expected = Path(temporary).resolve()
            data = command(
                receipt, "cwd-control", [sys.executable, "-c", "import os; print(os.getcwd())"],
                dict(os.environ), capture=True, cwd=expected,
            )
            self.assertEqual(Path(data.decode().strip()), expected)
            self.assertEqual(receipt["commands"][0]["cwd"], str(expected))
        self.assertEqual(receipt["commands"][0]["exit_code"], 0)


if __name__ == "__main__":
    unittest.main()
