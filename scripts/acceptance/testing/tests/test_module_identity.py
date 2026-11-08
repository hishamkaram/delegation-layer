"""Prove supported entrypoints share canonical state owners in either import order."""

from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


CHECK_IMPORTS = """
import importlib
from pathlib import Path
import runpy
import sys

root = Path(sys.argv[1])
assert Path.cwd() != root
assert str(root) not in sys.path
name, symbol, owner, order = sys.argv[2:]
entry = root.joinpath(*name.split(".")).with_suffix(".py")

def load_direct_entry():
    sys.path.insert(0, str(entry.parent))
    return runpy.run_path(str(entry), run_name="direct_entry")

if order == "package-first":
    sys.path.insert(0, str(root))
    packaged = importlib.import_module(name)
    direct = load_direct_entry()
else:
    direct = load_direct_entry()
    packaged = importlib.import_module(name)
canonical = importlib.import_module(owner)
assert direct[symbol] is getattr(packaged, symbol) is getattr(canonical, symbol)
assert getattr(canonical, symbol).__module__ == owner
assert sys.path.count(str(root)) == 1
assert str(entry.parent) not in sys.path
importlib.reload(packaged)
reloaded_direct = load_direct_entry()
assert sys.path.count(str(root)) == 1
assert str(entry.parent) not in sys.path
assert reloaded_direct[symbol] is getattr(packaged, symbol) is getattr(canonical, symbol)
queue = importlib.import_module("queue")
assert queue.Queue.__module__ == "queue"
assert not Path(queue.__file__).is_relative_to(root)
print("IDENTITY_OK")
"""


class AcceptanceModuleIdentityTests(unittest.TestCase):
    def test_direct_and_package_entries_share_defining_objects(self):
        root = Path(__file__).resolve().parents[4]
        bridges = (
            ("scripts.acceptance.workspaces.cli", "Processes", "scripts.acceptance.processes.operations"),
            ("scripts.acceptance.workspaces.cli", "OwnedDirectory", "scripts.acceptance.directories.operations"),
            ("scripts.acceptance.workspaces.cli", "DirectoryOwner", "scripts.acceptance.directories.operations"),
            ("scripts.acceptance.inspections.cli", "NativeTaskOps", "scripts.acceptance.tasks.operations"),
            ("scripts.acceptance.inspections.cli", "AcceptanceFailure", "scripts.acceptance.evidence.validation"),
            ("scripts.acceptance.provider_drivers.agy.cli", "AcceptanceFailure", "scripts.acceptance.evidence.validation"),
        )
        for bridge, symbol, owner in bridges:
            for order in ("direct-first", "package-first"):
                with self.subTest(bridge=bridge, order=order), tempfile.TemporaryDirectory() as directory:
                    result = subprocess.run(
                        [sys.executable, "-I", "-B", "-c", CHECK_IMPORTS, str(root), bridge, symbol, owner, order],
                        cwd=directory, capture_output=True, text=True, timeout=30, check=False,
                    )
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(result.stdout.strip(), "IDENTITY_OK")
                    self.assertEqual(list(Path(directory).iterdir()), [])
