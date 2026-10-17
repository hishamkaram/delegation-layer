"""Import-mode and canonical ownership identity checks for the workspace entry."""

from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


REPOSITORY_ROOT = Path(__file__).resolve().parents[3]
SCRIPTS_ROOT = REPOSITORY_ROOT / "scripts"


class WorkspaceEntryImportTests(unittest.TestCase):
    def _run_python(
        self, working_directory: Path, pythonpath: Path, arguments: list[str],
    ) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [sys.executable, *arguments],
            cwd=working_directory,
            env={"PYTHONPATH": str(pythonpath)},
            capture_output=True,
            check=False,
            text=True,
        )

    def test_flat_and_package_entries_share_canonical_classes_from_empty_cwd(self):
        code = "\n".join((
            "import acceptance_workspaces as flat_entry",
            "import scripts.acceptance_workspaces as package_entry",
            "import scripts.acceptance_supervisor_common as common",
            "import scripts.delegate_acceptance.directories.operations as directories",
            "import scripts.delegate_acceptance.processes.operations as processes",
            "import scripts.delegate_acceptance.workspaces.scenarios.context as context",
            "checks = {",
            "    'entry DirectoryOwner': flat_entry.DirectoryOwner is package_entry.DirectoryOwner,",
            "    'canonical DirectoryOwner': flat_entry.DirectoryOwner is directories.DirectoryOwner,",
            "    'entry OwnedDirectory': flat_entry.OwnedDirectory is package_entry.OwnedDirectory,",
            "    'canonical OwnedDirectory': flat_entry.OwnedDirectory is directories.OwnedDirectory,",
            "    'entry Processes': flat_entry.Processes is package_entry.Processes,",
            "    'canonical Processes': flat_entry.Processes is processes.Processes,",
            "    'entry WorkspaceContext': flat_entry.WorkspaceContext is package_entry.WorkspaceContext,",
            "    'canonical WorkspaceContext': flat_entry.WorkspaceContext is context.WorkspaceContext,",
            "    'shared DirectoryOwner': common.DirectoryOwner is directories.DirectoryOwner,",
            "    'shared OwnedDirectory': common.OwnedDirectory is directories.OwnedDirectory,",
            "    'shared Processes': common.Processes is processes.Processes,",
            "    'scenario module name': context.__name__ == 'scripts.delegate_acceptance.workspaces.scenarios.context',",
            "}",
            "failed = [name for name, matches in checks.items() if not matches]",
            "if failed: raise SystemExit('identity mismatch: ' + ', '.join(failed))",
        ))
        with tempfile.TemporaryDirectory() as temporary_directory:
            result = self._run_python(
                Path(temporary_directory), SCRIPTS_ROOT, ["-c", code],
            )
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_scripts_module_entry_runs_from_an_empty_cwd(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            result = self._run_python(
                Path(temporary_directory), REPOSITORY_ROOT,
                ["-m", "scripts.acceptance_workspaces", "--help"],
            )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("usage:", result.stdout.lower())

    def test_pure_workspace_model_and_policy_do_not_observe_filesystem_on_import(self):
        code = "\n".join((
            "from pathlib import Path",
            "from unittest import mock",
            "with mock.patch.object(Path, 'resolve', side_effect=AssertionError('filesystem observation')), "
            "mock.patch.object(Path, 'home', side_effect=AssertionError('home observation')):",
            "    from scripts.delegate_acceptance.workspaces.scenarios import model, policy",
            "    from scripts.delegate_acceptance.evidence import validation",
            "    assert model.DEFAULT_CAPACITY_TASKS == 200",
            "    assert policy.require is validation.require",
        ))
        with tempfile.TemporaryDirectory() as temporary_directory:
            result = self._run_python(
                Path(temporary_directory), REPOSITORY_ROOT, ["-c", code],
            )
        self.assertEqual(result.returncode, 0, result.stderr)
