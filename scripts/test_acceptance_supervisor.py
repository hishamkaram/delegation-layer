"""Supervisor gate failures must remain failures and expose their evidence."""
from contextlib import redirect_stderr, redirect_stdout
import io
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import acceptance_supervisor_common as supervisor_common
from acceptance_supervisor_hermetic import Case, HermeticSuite


class SupervisorDiagnosticsTests(unittest.TestCase):
    def test_process_start_receipt_failure_retains_spawned_process(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            cwd = root / "cwd"
            cwd.mkdir()

            class FakePopen:
                def __init__(self, *_args, **_kwargs):
                    self.pid = 701
                    self.returncode = None

                def poll(self):
                    return self.returncode

                def wait(self):
                    return self.returncode

            original_write_json = supervisor_common.write_json

            def fail_started(path, *args, **kwargs):
                if Path(path).name == "started.json":
                    raise OSError("injected started receipt failure")
                return original_write_json(path, *args, **kwargs)

            processes = supervisor_common.Processes(root / "processes", {"PATH": "/usr/bin"})
            with patch.object(supervisor_common.subprocess, "Popen", FakePopen), \
                    patch.object(supervisor_common, "write_json", side_effect=fail_started):
                with self.assertRaisesRegex(OSError, "started receipt"):
                    processes.start("caller", ["/bin/sh"], cwd)

            self.assertEqual(len(processes.entries), 1)
            process = processes.entries[0]
            self.assertEqual(process.pid, 701)
            self.assertEqual(processes.drain(timeout=0), [{
                "pid": 701, "argv": ["/bin/sh"],
                "directory": str(root / "processes" / "001-caller"),
            }])

            process.process.returncode = 0
            self.assertEqual(process.poll()["exit_code"], 0)
            self.assertEqual(processes.drain(timeout=0), [])

    def test_failed_case_snapshot_retains_logs_and_symlinks(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            case = object.__new__(Case)
            case.base = root / "private-base"
            case.base.mkdir()
            case.case_id = "failed-case"
            case.suite = SimpleNamespace(output=root / "evidence")
            case.processes = SimpleNamespace(drain=lambda **_kwargs: [{"pid": 123, "termination": "unknown"}])
            (case.base / "stderr").write_bytes(b"diagnostic bytes")
            (case.base / "unavailable-link").symlink_to(root / "not-present")
            case.retain_failure(RuntimeError("injected failure"))
            destination = case.suite.output / "cases" / case.case_id
            copied = destination / "private-base"
            self.assertEqual((copied / "stderr").read_bytes(), b"diagnostic bytes")
            self.assertTrue((copied / "unavailable-link").is_symlink())
            self.assertEqual((copied / "unavailable-link").readlink(), root / "not-present")
            self.assertTrue(case.base.exists())
            failure = json.loads((destination / "failure.json").read_text())
            self.assertTrue(failure["termination_unknown_preserved"])
            self.assertEqual(failure["unresolved_owned_processes"][0]["pid"], 123)

    def test_failed_case_is_reported_and_keeps_nonzero_exit(self):
        with tempfile.TemporaryDirectory() as temporary:
            suite = self.suite(temporary, "fail")
            stderr = io.StringIO()
            with patch("acceptance_supervisor_hermetic.digest", return_value="test-digest"), redirect_stderr(stderr):
                with self.assertRaises(SystemExit) as exit_result:
                    suite.run_targeted(["C07-locked-unknown"])
            self.assertEqual(exit_result.exception.code, 1)
            self.assertIn("FAIL C07-locked-unknown: injected assertion", stderr.getvalue())
            self.assertIn("injected traceback", stderr.getvalue())
            summary = json.loads((suite.output / "summary.json").read_text())
            self.assertEqual(summary["counts"], {"pass": 0, "fail": 1})
            self.assertEqual(summary["cases"], suite.results)

    def test_passing_case_does_not_emit_failure_diagnostics(self):
        with tempfile.TemporaryDirectory() as temporary:
            suite = self.suite(temporary, "pass")
            stderr, stdout = io.StringIO(), io.StringIO()
            with patch("acceptance_supervisor_hermetic.digest", return_value="test-digest"), redirect_stderr(stderr), redirect_stdout(stdout):
                suite.run_targeted(["C07-locked-unknown"])
            self.assertEqual(stderr.getvalue(), "")
            self.assertIn("PASS hermetic H targeted: 1 cases", stdout.getvalue())

    @staticmethod
    def suite(directory, status):
        suite = object.__new__(HermeticSuite)
        suite.tools = Path(directory)
        suite.output = Path(directory)
        suite.results = []

        def run_case(case_id, _function):
            suite.results.append({"case": case_id, "status": status,
                                  "error": "injected assertion", "traceback": "injected traceback"})

        suite.run_case = run_case
        return suite


if __name__ == "__main__":
    unittest.main()
