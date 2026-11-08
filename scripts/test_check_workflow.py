"""The aggregate executes each expensive suite once and keeps focused checks."""

from pathlib import Path
import shlex
import subprocess
import unittest


class CheckWorkflowTests(unittest.TestCase):
    def test_content_target_keeps_required_checks_without_full_suites(self):
        root = Path(__file__).resolve().parent.parent
        result = subprocess.run(
            ["make", "--no-print-directory", "-n", "check-docs"], cwd=root,
            capture_output=True, text=True, timeout=20, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        commands = [shlex.split(line) for line in result.stdout.splitlines()
                    if line.startswith(("go ", "python3 "))]
        suites = [command for command in commands if command[:2] == ["go", "test"]]
        self.assertEqual(suites, [["go", "test", "-count=1", "./internal/app", "-run",
                                   "^TestProviderGuideMatchesNativeCatalog$"]])
        python = [command for command in commands if command[:3] == ["python3", "-m", "unittest"]]
        self.assertEqual(len(python), 4)
        self.assertEqual(len({tuple(command) for command in python}), 4)
        self.assertTrue(all("-p" in command for command in python))
        self.assertIn(["python3", "-m", "unittest", "discover", "-s", "scripts/acceptance/native/tests",
                       "-t", "scripts", "-p", "test_aggregate.py", "-k",
                       "test_public_option_docs_cover_ccr_required_model_and_effort", "-v"], python)
        self.assertNotIn("install-tools.sh", result.stdout)
        self.assertNotIn("acceptance/native/run_all.sh", result.stdout)

    def test_check_runs_race_suite_once(self):
        root = Path(__file__).resolve().parent.parent
        result = subprocess.run(
            ["make", "--no-print-directory", "-n", "check"], cwd=root,
            capture_output=True, text=True, timeout=20, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        commands = [shlex.split(line.removesuffix("\\")) for line in result.stdout.splitlines()
                    if line.startswith("go ")]
        race_suites = [command for command in commands
                       if command[:2] == ["go", "test"] and "-race" in command]
        self.assertEqual(len(race_suites), 1, race_suites)
        self.assertIn("./...", race_suites[0])


if __name__ == "__main__":
    unittest.main()
