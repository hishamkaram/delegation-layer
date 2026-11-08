"""The extracted harness keeps one class identity and both command entry forms."""

from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[4]
ENTRY = ROOT / "scripts" / "acceptance/hermetic/cli.py"


class HermeticEntryTests(unittest.TestCase):
    def test_file_entry_runs_from_an_empty_directory(self):
        with tempfile.TemporaryDirectory() as temporary:
            result = subprocess.run(
                [sys.executable, str(ENTRY), "--help"], cwd=temporary,
                capture_output=True, text=True, timeout=10, check=False,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("--tools", result.stdout)
            self.assertIn("--source-root", result.stdout)
            self.assertIn("--case", result.stdout)
            self.assertEqual(list(Path(temporary).iterdir()), [])

    def test_module_entry_preserves_argument_errors(self):
        result = subprocess.run(
            [sys.executable, "-m", "scripts.acceptance.hermetic.cli", "--case", "unknown"],
            cwd=ROOT, capture_output=True, text=True, timeout=10, check=False,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("invalid choice", result.stderr)

    def test_direct_and_package_entries_share_one_case_and_suite(self):
        program = "\n".join([
            "import importlib, runpy, sys",
            "from pathlib import Path",
            "root = Path(sys.argv[1])",
            "order = sys.argv[2]",
            "entry = root / 'scripts/acceptance/hermetic/cli.py'",
            "assert Path.cwd() != root",
            "assert str(root) not in sys.path",
            "name = 'scripts.acceptance.hermetic.cli'",
            "if order == 'package-first':",
            "    sys.path.insert(0, str(root))",
            "    packaged = importlib.import_module(name)",
            "sys.path.insert(0, str(entry.parent))",
            "direct = runpy.run_path(str(entry), run_name='direct_entry')",
            "if order == 'direct-first':",
            "    packaged = importlib.import_module(name)",
            "case = importlib.import_module('scripts.acceptance.hermetic.case')",
            "suite = importlib.import_module('scripts.acceptance.hermetic.suite')",
            "assert direct['HermeticSuite'] is packaged.HermeticSuite is suite.HermeticSuite",
            "assert suite.Case is case.Case",
            "assert case.Case.__module__ == 'scripts.acceptance.hermetic.case'",
            "assert suite.HermeticSuite.__module__ == 'scripts.acceptance.hermetic.suite'",
            "assert sys.path.count(str(root)) == 1",
            "assert str(entry.parent) not in sys.path",
            "queue = importlib.import_module('queue')",
            "assert queue.Queue.__module__ == 'queue'",
            "assert not Path(queue.__file__).is_relative_to(root)",
        ])
        for order in ("direct-first", "package-first"):
            with self.subTest(order=order), tempfile.TemporaryDirectory() as temporary:
                result = subprocess.run(
                    [sys.executable, "-I", "-B", "-c", program, str(ROOT), order],
                    cwd=temporary, capture_output=True, text=True, timeout=10, check=False,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(list(Path(temporary).iterdir()), [])
