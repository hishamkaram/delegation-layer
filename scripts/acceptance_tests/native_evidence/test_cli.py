"""The evidence entrypoint works outside the repository and preserves exit codes."""

from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


class EvidenceCLITests(unittest.TestCase):
    def run_cli(self, directory: Path, *arguments: str) -> subprocess.CompletedProcess[str]:
        script = Path(__file__).resolve().parents[3] / "scripts/native_acceptance_evidence.py"
        return subprocess.run(
            [sys.executable, str(script), *arguments], cwd=directory,
            capture_output=True, text=True, timeout=20, check=False,
        )

    def test_list_cases_from_an_empty_directory(self):
        with tempfile.TemporaryDirectory(prefix="native-evidence-cli-") as directory:
            result = self.run_cli(Path(directory), "list-cases")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("codex:review\tread-only\tread-only", result.stdout)
        self.assertEqual(result.stderr, "")

    def test_failed_validation_is_a_nonzero_process_exit(self):
        with tempfile.TemporaryDirectory(prefix="native-evidence-cli-") as directory:
            result = self.run_cli(Path(directory), "validate", "--receipt", "missing.json")
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertEqual(result.stdout, "")
        self.assertIn("native acceptance evidence failed: evidence_file_unreadable", result.stderr)


if __name__ == "__main__":
    unittest.main()
