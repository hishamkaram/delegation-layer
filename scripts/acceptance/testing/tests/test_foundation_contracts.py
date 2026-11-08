"""Canonical state ownership and import behavior of the shared foundation."""

from pathlib import Path
import subprocess
import sys
import unittest


from scripts.acceptance.directories import operations as directories
from scripts.acceptance.processes import operations as processes
from scripts.acceptance.tasks import operations as tasks
from scripts.acceptance.provider_drivers.claude import runner as claude
from scripts.acceptance.workspaces.scenarios import context as workspaces


class FoundationContractTests(unittest.TestCase):
    def test_consumers_share_canonical_mutable_state_owners(self):
        self.assertIs(workspaces.DirectoryOwner, directories.DirectoryOwner)
        self.assertIs(workspaces.Processes, processes.Processes)
        self.assertIs(claude.DirectoryOwner, directories.DirectoryOwner)
        self.assertIs(claude.Processes, processes.Processes)
        self.assertIs(claude.NativeTaskOps, tasks.NativeTaskOps)

    def test_importing_every_foundation_module_starts_no_fixture(self):
        program = """
import importlib
import os
import pkgutil
from pathlib import Path
import sys

def forbid_effects(event, arguments):
    forbidden = {
        'subprocess.Popen', 'os.system', 'os.fork', 'os.posix_spawn',
        'os.mkdir', 'os.remove', 'os.rmdir', 'os.rename', 'socket.connect',
    }
    if event in forbidden:
        raise RuntimeError('import-time effect: ' + event)
    if event == 'open' and arguments[2] & (os.O_WRONLY | os.O_RDWR | os.O_CREAT):
        raise RuntimeError('import-time write')

sys.addaudithook(forbid_effects)
sys.path.insert(0, str(Path(sys.argv[1]).parent))
import scripts.acceptance as foundation
for module in pkgutil.walk_packages(foundation.__path__, 'scripts.acceptance.'):
    importlib.import_module(module.name)
import scripts.acceptance.private_cli.cli
"""
        scripts = str((Path(__file__).resolve().parents[4] / 'scripts'))
        result = subprocess.run(
            [sys.executable, "-I", "-B", "-c", program, scripts],
            capture_output=True, text=True, timeout=20, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
