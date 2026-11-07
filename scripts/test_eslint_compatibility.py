"""Exercise the locked glob override through real ESLint consumers and rules."""

import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

from scripts import static_analysis as analysis


class ESLintCompatibilityTests(unittest.TestCase):
    def setUp(self):
        self.directory = Path(tempfile.mkdtemp(prefix="eslint compatibility-", dir=analysis.ROOT / "bin"))
        self.child: subprocess.Popen[str] | None = None
        self.addCleanup(self.close_fixture)

    def close_fixture(self):
        # This fixture owns the finite Node process and retains its ancestor if joining fails.
        try:
            if self.child is not None and self.child.returncode is None:
                self.child.kill()
                self.child.communicate(timeout=10)
        except (OSError, subprocess.SubprocessError) as error:
            raise RuntimeError(f"ESLint closure unverified; retained {self.directory}") from error
        shutil.rmtree(self.directory)

    def write(self, name: str, content: str) -> Path:
        path = self.directory / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)
        return path

    def lint(self, command: list[str], paths: list[str], *, cwd: Path) -> tuple[int, dict[str, list[str | None]]]:
        self.child = subprocess.Popen(command + ["--format", "json", *paths], cwd=cwd,
                                      text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        stdout, stderr = self.child.communicate(timeout=30)
        self.assertIn(self.child.returncode, (0, 1), stderr)
        reports: object = json.loads(stdout)
        if not isinstance(reports, list):
            raise AssertionError("ESLint must report a JSON array")
        rules: dict[str, list[str | None]] = {}
        for report in reports:
            if not isinstance(report, dict):
                raise AssertionError("ESLint file result must be an object")
            filename = report.get("filePath")
            messages = report.get("messages")
            if not isinstance(filename, str) or not isinstance(messages, list):
                raise AssertionError("ESLint file result lacks a path or messages")
            relative = Path(filename).relative_to(self.directory).as_posix()
            self.assertNotIn(relative, rules, "ESLint reported the same file twice")
            rules[relative] = []
            for message in messages:
                if not isinstance(message, dict):
                    raise AssertionError("ESLint message must be an object")
                rule = message.get("ruleId")
                if rule is not None and not isinstance(rule, str):
                    raise AssertionError("ESLint rule ID must be a string or null")
                rules[relative].append(rule)
        if self.child.returncode is None:
            raise AssertionError("ESLint process has no observed exit")
        return self.child.returncode, rules

    def test_repository_rules_reject_and_accept_nested_fixture_with_spaces(self):
        path = self.write("nested directory/a file.mjs",
                          "function example() { try { return 1; } finally { return 2; } }\nexample();\n")
        command, _ = analysis.commands("javascript")
        code, rules = self.lint(command, [str(path)], cwd=analysis.ROOT)
        self.assertEqual(code, 1)
        self.assertEqual(rules, {"nested directory/a file.mjs": ["no-unsafe-finally"]})

        path.write_text("function example() { return 1; }\nexample();\n")
        code, rules = self.lint(command, [str(path)], cwd=analysis.ROOT)
        self.assertEqual(code, 0)
        self.assertEqual(rules, {"nested directory/a file.mjs": []})

    def test_globs_select_nested_files_and_apply_positive_and_negative_ignores(self):
        repository_config = json.dumps((analysis.ROOT / "eslint.config.mjs").as_uri())
        config = self.write("eslint.config.mjs", f"import repository from {repository_config};\n" + """
export default [
  { ignores: ["ignored/**/*", "!ignored/**/*/", "!ignored/**/keep.mjs"] },
  ...repository,
  { files: ["positive/**/*.mjs"], ignores: ["**/skip-*.mjs"], rules: { "no-console": "error" } },
];
""")
        self.write("positive/root.mjs", "console.log('selected');\n")
        self.write("positive/nested directory/a file.mjs", "console.log('selected');\n")
        self.write("positive/nested directory/skip-file.mjs", "console.log('excluded from rule');\n")
        self.write("outside/a file.mjs", "console.log('outside selected glob');\n")
        self.write("ignored/nested directory/discard.mjs", "invalid syntax sentinel\n")
        self.write("ignored/nested directory/keep.mjs", "missing();\n")
        command, _ = analysis.commands("javascript")
        command[command.index("--config") + 1] = str(config)
        code, rules = self.lint(command, ["positive/**/*.mjs", "outside/**/*.mjs", "ignored/**/*.mjs"],
                                cwd=self.directory)
        self.assertEqual(code, 1)
        self.assertEqual(rules, {
            "positive/root.mjs": ["no-console"],
            "positive/nested directory/a file.mjs": ["no-console"],
            "positive/nested directory/skip-file.mjs": [],
            "outside/a file.mjs": [],
            "ignored/nested directory/keep.mjs": ["no-undef"],
        })


if __name__ == "__main__":
    unittest.main()
