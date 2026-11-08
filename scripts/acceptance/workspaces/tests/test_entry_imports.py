"""Import-mode and canonical ownership identity checks for the workspace entry."""

from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


REPOSITORY_ROOT = Path(__file__).resolve().parents[4]


class WorkspaceEntryImportTests(unittest.TestCase):
    def _run_python(
        self, working_directory: Path, pythonpath: Path | None, arguments: list[str],
    ) -> subprocess.CompletedProcess[str]:
        environment = {"PYTHONNOUSERSITE": "1"}
        if pythonpath is not None:
            environment["PYTHONPATH"] = str(pythonpath)
        return subprocess.run(
            [sys.executable, *arguments],
            cwd=working_directory,
            env=environment,
            capture_output=True,
            check=False,
            text=True,
            timeout=30,
        )

    def test_flat_and_package_entries_share_canonical_classes_from_empty_cwd(self):
        code = "\n".join((
            "import importlib, runpy, sys",
            "from pathlib import Path",
            "root = Path(sys.argv[1]).resolve()",
            "assert Path.cwd() != root",
            "assert all(Path(item or '.').resolve() != root for item in sys.path)",
            "direct_entry = runpy.run_path(str(root / 'scripts/acceptance/workspaces/cli.py'), run_name='direct_entry')",
            "package_entry = importlib.import_module('scripts.acceptance.workspaces.cli')",
            "import scripts.acceptance.directories.operations as directories",
            "import scripts.acceptance.processes.operations as processes",
            "import scripts.acceptance.workspaces.scenarios.context as context",
            "checks = {",
            "    'entry DirectoryOwner': direct_entry['DirectoryOwner'] is package_entry.DirectoryOwner,",
            "    'canonical DirectoryOwner': direct_entry['DirectoryOwner'] is directories.DirectoryOwner,",
            "    'entry OwnedDirectory': direct_entry['OwnedDirectory'] is package_entry.OwnedDirectory,",
            "    'canonical OwnedDirectory': direct_entry['OwnedDirectory'] is directories.OwnedDirectory,",
            "    'entry Processes': direct_entry['Processes'] is package_entry.Processes,",
            "    'canonical Processes': direct_entry['Processes'] is processes.Processes,",
            "    'entry WorkspaceContext': direct_entry['WorkspaceContext'] is package_entry.WorkspaceContext,",
            "    'canonical WorkspaceContext': direct_entry['WorkspaceContext'] is context.WorkspaceContext,",
            "    'shared DirectoryOwner': context.DirectoryOwner is directories.DirectoryOwner,",
            "    'shared OwnedDirectory': context.OwnedDirectory is directories.OwnedDirectory,",
            "    'shared Processes': context.Processes is processes.Processes,",
            "    'scenario module name': context.__name__ == 'scripts.acceptance.workspaces.scenarios.context',",
            "}",
            "failed = [name for name, matches in checks.items() if not matches]",
            "if failed: raise SystemExit('identity mismatch: ' + ', '.join(failed))",
        ))
        with tempfile.TemporaryDirectory() as temporary_directory:
            result = self._run_python(
                Path(temporary_directory), None, ["-c", code, str(REPOSITORY_ROOT)],
            )
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_scripts_module_entry_runs_from_an_empty_cwd(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            result = self._run_python(
                Path(temporary_directory), REPOSITORY_ROOT,
                ["-m", "scripts.acceptance.workspaces.cli", "--help"],
            )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("usage:", result.stdout.lower())

    def test_pure_workspace_model_and_policy_do_not_observe_filesystem_on_import(self):
        code = "\n".join((
            "from pathlib import Path",
            "from unittest import mock",
            "with mock.patch.object(Path, 'resolve', side_effect=AssertionError('filesystem observation')), "
            "mock.patch.object(Path, 'home', side_effect=AssertionError('home observation')):",
            "    from scripts.acceptance.workspaces.scenarios import model, policy",
            "    from scripts.acceptance.evidence import validation",
            "    assert model.DEFAULT_CAPACITY_TASKS == 200",
            "    assert policy.require is validation.require",
        ))
        with tempfile.TemporaryDirectory() as temporary_directory:
            result = self._run_python(
                Path(temporary_directory), REPOSITORY_ROOT, ["-c", code],
            )
        self.assertEqual(result.returncode, 0, result.stderr)
