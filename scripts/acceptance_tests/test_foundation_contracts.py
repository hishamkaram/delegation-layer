"""Canonical state ownership and import behavior of the shared foundation."""

from pathlib import Path
import subprocess
import sys
import unittest

from scripts import acceptance_provider_common as provider
from scripts import acceptance_supervisor_common as supervisor
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.processes.operations import Process, Processes
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps


class FoundationContractTests(unittest.TestCase):
    def test_facades_alias_the_same_canonical_state_owners(self):
        self.assertIs(supervisor.Process, Process)
        self.assertIs(supervisor.Processes, Processes)
        self.assertIs(supervisor.DirectoryOwner, DirectoryOwner)
        self.assertIs(provider.NativeTaskOps, NativeTaskOps)

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
import scripts.delegate_acceptance as foundation
for module in pkgutil.walk_packages(foundation.__path__, 'scripts.delegate_acceptance.'):
    importlib.import_module(module.name)
import scripts.acceptance_supervisor_common
import scripts.acceptance_provider_common
"""
        scripts = str(Path(__file__).resolve().parents[1])
        result = subprocess.run(
            [sys.executable, "-I", "-B", "-c", program, scripts],
            capture_output=True, text=True, timeout=20, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
