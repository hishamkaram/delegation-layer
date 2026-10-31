"""The aggregate executes each expensive suite once and keeps strict auditing."""

from pathlib import Path
import shlex
import subprocess
import unittest


class CheckWorkflowTests(unittest.TestCase):
    def test_check_keeps_strict_audit_without_repeating_go_tests(self):
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
        audits = [command for command in commands
                  if command[:3] == ["go", "run", "./cmd/maintainability"]
                  and "--strict" in command]
        self.assertEqual(audits, [["go", "run", "./cmd/maintainability", "--strict"]])


if __name__ == "__main__":
    unittest.main()
