"""The extracted harness keeps one class identity and both command entry forms."""

from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[3]
ENTRY = ROOT / "scripts" / "acceptance_supervisor_hermetic.py"


class HermeticEntryTests(unittest.TestCase):
    def test_file_entry_runs_from_an_empty_directory(self):
        with tempfile.TemporaryDirectory() as temporary:
            result = subprocess.run(
                [sys.executable, str(ENTRY), "--help"], cwd=temporary,
                capture_output=True, text=True, timeout=10, check=False,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("--tools", result.stdout)
            self.assertIn("--case", result.stdout)
            self.assertEqual(list(Path(temporary).iterdir()), [])

    def test_module_entry_preserves_argument_errors(self):
        result = subprocess.run(
            [sys.executable, "-m", "scripts.acceptance_supervisor_hermetic", "--case", "unknown"],
            cwd=ROOT, capture_output=True, text=True, timeout=10, check=False,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("invalid choice", result.stderr)

    def test_flat_and_script_imports_share_one_case_and_suite(self):
        program = "\n".join([
            "import sys",
            "sys.path[:0] = [sys.argv[1], sys.argv[2]]",
            "import acceptance_supervisor_hermetic as flat",
            "import scripts.acceptance_supervisor_hermetic as packaged",
            "from scripts.delegate_acceptance.hermetic.case import Case",
            "from scripts.delegate_acceptance.hermetic.suite import HermeticSuite",
            "assert flat.Case is packaged.Case is Case",
            "assert flat.HermeticSuite is packaged.HermeticSuite is HermeticSuite",
        ])
        with tempfile.TemporaryDirectory() as temporary:
            result = subprocess.run(
                [sys.executable, "-c", program, str(ROOT / "scripts"), str(ROOT)],
                cwd=temporary, capture_output=True, text=True, timeout=10, check=False,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(list(Path(temporary).iterdir()), [])
