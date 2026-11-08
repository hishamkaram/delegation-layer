"""Prove supported entrypoints share canonical state owners in either import order."""

from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


CHECK_IMPORTS = """
import importlib
from pathlib import Path
import sys

scripts = Path(sys.argv[1])
root = str(scripts.parent)
assert root not in sys.path
sys.path.insert(0, str(scripts))
name, symbol, owner, order = sys.argv[2:]
if order == "package-first":
    sys.path.insert(0, root)
    packaged = importlib.import_module("scripts." + name)
    flat = importlib.import_module(name)
else:
    flat = importlib.import_module(name)
    packaged = importlib.import_module("scripts." + name)
canonical = importlib.import_module(owner)
assert getattr(flat, symbol) is getattr(packaged, symbol)
assert getattr(flat, symbol) is getattr(canonical, symbol)
assert sys.path.count(root) == 1
importlib.reload(flat)
importlib.reload(packaged)
assert sys.path.count(root) == 1
assert getattr(flat, symbol) is getattr(canonical, symbol)
print("IDENTITY_OK")
"""


class AcceptanceModuleIdentityTests(unittest.TestCase):
    def test_flat_and_package_entries_share_defining_objects(self):
        scripts = ((Path(__file__).resolve().parents[4] / 'scripts'))
        bridges = (
            ("scripts.acceptance.workspaces.cli", "Processes", "scripts.acceptance.processes.operations"),
            ("scripts.acceptance.workspaces.cli", "OwnedDirectory", "scripts.acceptance.directories.operations"),
            ("scripts.acceptance.workspaces.cli", "DirectoryOwner", "scripts.acceptance.directories.operations"),
            ("scripts.acceptance.inspections.cli", "NativeTaskOps", "scripts.acceptance.tasks.operations"),
            ("scripts.acceptance.inspections.cli", "AcceptanceFailure", "scripts.acceptance.evidence.validation"),
            ("scripts.acceptance.provider_drivers.agy.cli", "AcceptanceFailure", "scripts.acceptance.evidence.validation"),
        )
        for bridge, symbol, owner in bridges:
            for order in ("flat-first", "package-first"):
                with self.subTest(bridge=bridge, order=order), tempfile.TemporaryDirectory() as directory:
                    result = subprocess.run(
                        [sys.executable, "-I", "-B", "-c", CHECK_IMPORTS, str(scripts), bridge, symbol, owner, order],
                        cwd=directory, capture_output=True, text=True, timeout=30, check=False,
                    )
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(result.stdout.strip(), "IDENTITY_OK")
