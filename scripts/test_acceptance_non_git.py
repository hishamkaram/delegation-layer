"""Regression tests for non-Git workspace proof and shared matrix execution."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import acceptance_native as gate
import acceptance_supervisor_common as common
import native_acceptance_evidence as evidence
from test_acceptance_native import fake_native_python_script


def isolate_git_ancestors(root):
    native_lstat = Path.lstat

    def lstat(path, *args, **kwargs):
        # Keep fixture entries real while excluding unrelated host Git ancestors.
        if path.name == ".git" and not path.is_relative_to(root):
            raise FileNotFoundError(path)
        return native_lstat(path, *args, **kwargs)

    return patch.object(Path, "lstat", autospec=True, side_effect=lstat)


class NonGitAcceptanceTests(unittest.TestCase):
    def test_cleanup_cannot_remove_a_sibling_invocation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            home = root / "home"
            runs = []
            with patch.object(gate.Path, "home", return_value=home), \
                    patch.object(gate, "reject_tmp"), patch.object(gate, "require_discovery"):
                for index in range(2):
                    output = root / f"evidence-{index}"
                    output.mkdir()
                    args = gate.build_parser().parse_args([
                        "--provider", "codex:exec", "--permission", "read-only",
                        "--output", str(output), "--delegate", "/bin/sh", "--runner", "/bin/sh",
                        "--pueue", "/bin/sh", "--pueued", "/bin/sh", "--provider-executable", "/bin/sh",
                    ])
                    runs.append(gate.NativeAcceptance(args))
            try:
                self.assertTrue(common.rollback_owned_directories(runs[0].owned_directories))
                self.assertFalse(runs[0].base.exists())
                self.assertTrue(runs[1].workspace.is_dir())
                self.assertTrue(runs[1].config.is_file())
                self.assertTrue(runs[1].processes.directory.is_dir())
            finally:
                self.assertTrue(common.rollback_owned_directories(runs[1].owned_directories))
            self.assertTrue((home / "delegation-layer-live").is_dir())
            self.assertEqual(list((home / "delegation-layer-live").iterdir()), [])

    def test_ancestor_detection_handles_git_files_directories_and_symlinks(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            nested = root / "one" / "two"
            nested.mkdir(parents=True)
            with isolate_git_ancestors(root):
                self.assertFalse(gate.has_git_ancestor(nested))
            marker = root / ".git"
            marker.write_text("gitdir: elsewhere\n")
            self.assertTrue(gate.has_git_ancestor(nested))
            marker.unlink()
            marker.mkdir()
            self.assertTrue(gate.has_git_ancestor(nested))
            marker.rmdir()
            marker.symlink_to(root / "absent")
            self.assertTrue(gate.has_git_ancestor(nested))

    def test_ancestor_inspection_errors_are_not_absence(self):
        with patch.object(gate.Path, "lstat", side_effect=PermissionError("fixture")):
            with self.assertRaises(PermissionError):
                gate.has_git_ancestor(Path("/fixture"))

    def test_ancestor_repository_blocks_before_capture_or_base_creation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            home = root / "home"
            home.mkdir()
            (home / ".git").mkdir()
            output = root / "evidence"
            output.mkdir()
            with patch.object(gate.Path, "home", return_value=home), \
                    patch.object(gate, "choose_output", return_value=output), \
                    patch.object(gate, "resolve_executable", return_value=Path("/bin/sh")), \
                    patch.object(gate, "require_discovery"), \
                    patch.object(gate, "Processes") as processes:
                code = gate.main(["--provider", "codex:exec", "--permission", "read-only",
                                  "--scenario", "non-git", "--output", str(output)])
            self.assertEqual(code, 2)
            processes.assert_not_called()
            self.assertFalse((home / "delegation-layer-live").exists())
            self.assertEqual({p.name for p in output.iterdir()}, {"failure.json"})
            self.assertEqual(json.loads((output / "failure.json").read_text())["status"], "BLOCKED")

    def test_non_git_modes_do_not_initialize_git(self):
        for mode in ("read-only", "workspace-write"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                root = Path(directory).resolve()
                home = root / "home"
                output = root / "evidence"
                output.mkdir()
                args = gate.build_parser().parse_args([
                    "--provider", "codex:exec", "--permission", mode, "--scenario", "non-git",
                    "--output", str(output), "--delegate", "/bin/sh", "--runner", "/bin/sh",
                    "--pueue", "/bin/sh", "--pueued", "/bin/sh", "--provider-executable", "/bin/sh",
                ])
                with patch.object(gate.Path, "home", return_value=home), \
                        patch.object(gate, "reject_tmp"), patch.object(gate, "require_discovery"), \
                        isolate_git_ancestors(root):
                    operation = gate.NativeAcceptance(args)
                try:
                    with patch.object(operation.processes, "run") as run, isolate_git_ancestors(root):
                        operation._prepare_codex_workspace()
                        run.assert_not_called()
                        self.assertFalse(gate.has_git_ancestor(operation.workspace))
                    (operation.workspace / ".git").mkdir()
                    with self.assertRaises(gate.AcceptanceFailure):
                        operation._prepare_codex_workspace()
                finally:
                    self.assertTrue(common.rollback_owned_directories(operation.owned_directories))

    def test_matrix_has_both_non_git_modes_after_stable_existing_cases(self):
        self.assertEqual(
            [(case.case_id, case.permission, case.scenario) for case in evidence.EXPECTED_CASES[-2:]],
            [("case-15-codex-exec-read-only-non-git", "read-only", "non-git"),
             ("case-16-codex-exec-workspace-write-non-git", "workspace-write", "non-git")],
        )

    def test_matrix_child_stdin_cannot_hide_a_case_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            fake_python = root / "python3"
            fake_python.write_text(fake_native_python_script())
            fake_python.chmod(0o700)
            environment = {**os.environ, "PATH": str(root) + os.pathsep + os.environ["PATH"],
                           "REAL_PYTHON": sys.executable, "FAKE_NATIVE_RESULT": "one-failure",
                           "FAKE_READ_STDIN": "1", "DELEGATE_ACCEPTANCE_OUTPUT_ROOT": str(root / "aggregate")}
            result = subprocess.run(["sh", str(Path(__file__).with_name("acceptance_native_all.sh"))],
                                    env=environment, capture_output=True, text=True, check=False, timeout=60)
            self.assertEqual(result.returncode, 1, result.stderr)
            self.assertIn(f"passed=0 blocked={len(evidence.EXPECTED_CASES) - 1} failed=1", result.stdout)
            rows = (root / "aggregate" / "case-matrix.tsv").read_text().splitlines()[1:]
            self.assertEqual(len(rows), len(evidence.EXPECTED_CASES))
            self.assertEqual([row.split("\t")[0] for row in rows],
                             [case.case_id for case in evidence.EXPECTED_CASES])

    def test_matrix_helper_failure_runs_no_case(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            fake_python = root / "python3"
            fake_python.write_text(fake_native_python_script())
            fake_python.chmod(0o700)
            environment = {**os.environ, "PATH": str(root) + os.pathsep + os.environ["PATH"],
                           "REAL_PYTHON": sys.executable, "FAKE_LIST_EXIT": "7",
                           "DELEGATE_ACCEPTANCE_OUTPUT_ROOT": str(root / "aggregate")}
            result = subprocess.run(["sh", str(Path(__file__).with_name("acceptance_native_all.sh"))],
                                    env=environment, capture_output=True, text=True, check=False, timeout=20)
            self.assertEqual(result.returncode, 7, result.stderr)
            self.assertEqual((root / "aggregate" / "case-matrix.tsv").read_text().count("\n"), 1)


if __name__ == "__main__":
    unittest.main()
