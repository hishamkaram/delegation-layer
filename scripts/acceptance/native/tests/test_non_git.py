"""Regression tests for non-Git workspace proof and shared matrix execution."""

import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


import scripts.acceptance.native.cli as gate

repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.directories import operations as directories_operations


from scripts.acceptance.native import workflow as acceptance_impl
from scripts.acceptance.native.tests.support import adjacent_runner_bundle
from scripts.acceptance.native.tests.support import fake_native_python_script
from scripts.acceptance.native.tests.support import repository_root as fixture_repository_root


import scripts.acceptance.native_evidence.contract as native_evidence_contract


def matrix_stdin_python_script():
    # This shell-loop fixture owns only case planning, stdin and exit codes.
    # Full evidence construction remains covered by the aggregate cohort.
    plan = "".join("\t".join((case.case_id, case.provider, case.permission, case.scenario)) + "\n"
                   for case in native_evidence_contract.EXPECTED_CASES)
    program = f'''
import sys
from pathlib import Path

script, *arguments = sys.argv[1:]

def option(name):
    return arguments[arguments.index(name) + 1]

if script == "-c":
    print(3)
elif script.endswith("/acceptance/native_evidence/cli.py"):
    command = arguments[0]
    if command == "list-cases":
        print({plan!r}, end="")
    elif command == "prepare-case":
        (Path(option("--aggregate-root")) / "cases" / option("--case-id")).mkdir()
    elif command == "aggregate":
        Path(option("--receipt")).write_text("{{}}\\n")
    elif command == "validate":
        if Path(option("--receipt")).read_text() != "{{}}\\n":
            raise SystemExit(7)
    else:
        raise SystemExit(7)
elif script.endswith("/acceptance/native/cli.py"):
    sys.stdin.buffer.read()
    failed = (option("--provider"), option("--permission"), option("--scenario")) == (
        "codex:exec", "read-only", "non-git")
    raise SystemExit(1 if failed else 2)
else:
    raise SystemExit(7)
'''
    return '#!/bin/sh\nexec "$REAL_PYTHON" -c ' + shlex.quote(program) + ' "$@"\n'


def run_matrix_stdin_fixture(root, repository):
    fake_python = root / "python3"
    fake_python.write_text(matrix_stdin_python_script())
    fake_python.chmod(0o700)
    environment = {**os.environ, "PATH": str(root) + os.pathsep + os.environ["PATH"],
                   "REAL_PYTHON": sys.executable,
                   "DELEGATE_ACCEPTANCE_OUTPUT_ROOT": str(root / "aggregate")}
    return subprocess.run(["sh", str(repository / "scripts/acceptance/native/run_all.sh")],
                          cwd=repository, env=environment, capture_output=True,
                          text=True, check=False, timeout=20)


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
            delegate, runner = adjacent_runner_bundle(root)
            runs = []
            with patch.object(gate.Path, "home", return_value=home), \
                    patch.object(acceptance_impl, "reject_tmp"), \
                    patch.object(acceptance_impl, "require_discovery"):
                for index in range(2):
                    output = root / f"evidence-{index}"
                    output.mkdir()
                    args = gate.build_parser().parse_args([
                        "--provider", "codex:exec", "--permission", "read-only",
                        "--output", str(output), "--delegate", str(delegate), "--runner", str(runner),
                        "--pueue", "/bin/sh", "--pueued", "/bin/sh", "--provider-executable", "/bin/sh",
                    ])
                    runs.append(gate.NativeAcceptance(args))
                    self.assertTrue(runs[-1].ops.managed_runner)
            try:
                self.assertTrue(directories_operations.rollback_owned_directories(runs[0].owned_directories))
                self.assertFalse(runs[0].base.exists())
                self.assertTrue(runs[1].workspace.is_dir())
                self.assertTrue(runs[1].config.is_file())
                self.assertTrue(runs[1].processes.directory.is_dir())
            finally:
                self.assertTrue(directories_operations.rollback_owned_directories(runs[1].owned_directories))
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
                    patch.object(acceptance_impl, "resolve_executable", return_value=Path("/bin/sh")), \
                    patch.object(acceptance_impl, "require_discovery"), \
                    patch.object(acceptance_impl, "Processes") as processes:
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
                delegate, runner = adjacent_runner_bundle(root)
                args = gate.build_parser().parse_args([
                    "--provider", "codex:exec", "--permission", mode, "--scenario", "non-git",
                    "--output", str(output), "--delegate", str(delegate), "--runner", str(runner),
                    "--pueue", "/bin/sh", "--pueued", "/bin/sh", "--provider-executable", "/bin/sh",
                ])
                with patch.object(gate.Path, "home", return_value=home), \
                        patch.object(acceptance_impl, "reject_tmp"), \
                        patch.object(acceptance_impl, "require_discovery"), \
                        isolate_git_ancestors(root):
                    operation = gate.NativeAcceptance(args)
                try:
                    self.assertTrue(operation.ops.managed_runner)
                    with patch.object(operation.processes, "run") as run, isolate_git_ancestors(root):
                        operation._prepare_codex_workspace()
                        run.assert_not_called()
                        self.assertFalse(gate.has_git_ancestor(operation.workspace))
                    (operation.workspace / ".git").mkdir()
                    with self.assertRaises(gate.AcceptanceFailure):
                        operation._prepare_codex_workspace()
                finally:
                    self.assertTrue(directories_operations.rollback_owned_directories(operation.owned_directories))

    def test_matrix_has_both_non_git_modes_after_stable_existing_cases(self):
        self.assertEqual(
            [(case.case_id, case.permission, case.scenario) for case in native_evidence_contract.EXPECTED_CASES[14:16]],
            [("case-15-codex-exec-read-only-non-git", "read-only", "non-git"),
             ("case-16-codex-exec-workspace-write-non-git", "workspace-write", "non-git")],
        )

    def test_matrix_child_stdin_cannot_hide_a_case_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            result = run_matrix_stdin_fixture(root, fixture_repository_root())
            self.assertEqual(result.returncode, 1, result.stderr)
            self.assertIn(f"passed=0 blocked={len(native_evidence_contract.EXPECTED_CASES) - 1} failed=1", result.stdout)
            rows = (root / "aggregate" / "case-matrix.tsv").read_text().splitlines()[1:]
            self.assertEqual(len(rows), len(native_evidence_contract.EXPECTED_CASES))
            self.assertEqual([row.split("\t")[0] for row in rows],
                             [case.case_id for case in native_evidence_contract.EXPECTED_CASES])

    def test_matrix_stdin_fixture_detects_a_child_consuming_remaining_cases(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            repository = root / "repository"
            wrapper = repository / "scripts/acceptance/native/run_all.sh"
            wrapper.parent.mkdir(parents=True)
            source = (fixture_repository_root() / "scripts/acceptance/native/run_all.sh").read_text()
            self.assertEqual(source.count(" </dev/null; then"), 1)
            wrapper.write_text(source.replace(" </dev/null; then", "; then"))
            fixture = root / "fixture"
            fixture.mkdir()
            result = run_matrix_stdin_fixture(fixture, repository)
            self.assertEqual(result.returncode, 1, result.stderr)
            self.assertIn("case plan was not completely consumed", result.stderr)
            rows = (fixture / "aggregate/case-matrix.tsv").read_text().splitlines()[1:]
            self.assertEqual(len(rows), 1)
            self.assertEqual(rows[0].split("\t")[0], native_evidence_contract.EXPECTED_CASES[0].case_id)

    def test_matrix_helper_failure_runs_no_case(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            fake_python = root / "python3"
            fake_python.write_text(fake_native_python_script())
            fake_python.chmod(0o700)
            environment = {**os.environ, "PATH": str(root) + os.pathsep + os.environ["PATH"],
                           "REAL_PYTHON": sys.executable, "FAKE_LIST_EXIT": "7",
                           "DELEGATE_ACCEPTANCE_OUTPUT_ROOT": str(root / "aggregate")}
            result = subprocess.run(["sh", str(fixture_repository_root() / "scripts/acceptance/native/run_all.sh")],
                                    cwd=fixture_repository_root(), env=environment, capture_output=True,
                                    text=True, check=False, timeout=20)
            self.assertEqual(result.returncode, 7, result.stderr)
            self.assertEqual((root / "aggregate" / "case-matrix.tsv").read_text().count("\n"), 1)


if __name__ == "__main__":
    unittest.main()
