"""Implementation methods for PackagedDefaultPathTests; collected once by the acceptance_tests discovery."""

from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from scripts import acceptance_private_cli as private_cli


class PackagedDefaultPathTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-packaged-default-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def test_command_construction_omits_every_global_path_override(self):
        brief = self.root / "brief.md"
        workspace = self.root / "workspace"
        task_id = "a" * 32

        commands = private_cli.packaged_default_commands(brief, workspace, task_id)

        self.assertEqual(commands["dispatch"], [
            "dispatch", "--provider", "pi:json", "--brief", str(brief),
            "--cwd", str(workspace), "--id", task_id, "--permission", "read-only",
            "--budget", private_cli.TASK_BUDGET, "--json",
        ])
        self.assertEqual(commands["status"], ["status", task_id, "--json"])
        self.assertEqual(commands["collect"], [
            "collect", task_id, "--watch", private_cli.COLLECT_WATCH, "--json",
        ])
        for command in commands.values():
            self.assertNotIn("--root", command)
            self.assertNotIn("--runner", command)
            self.assertNotIn("--pueue-config", command)

    def test_isolated_home_maps_to_each_supported_default_state_path(self):
        with patch.object(private_cli.platform, "system", return_value="Linux"):
            self.assertEqual(
                private_cli.isolated_default_state_path(self.root),
                self.root / ".config" / "delegation-layer",
            )
        with patch.object(private_cli.platform, "system", return_value="Darwin"):
            self.assertEqual(
                private_cli.isolated_default_state_path(self.root),
                self.root / "Library" / "Application Support" / "delegation-layer",
            )

    def test_adjacent_package_copies_exact_regular_binary_identities(self):
        sources = {}
        for index, name in enumerate(("delegate", "delegate-run", "pueue", "pueued")):
            source = self.root / ("source-" + name)
            source.write_bytes((name + "-%d" % index).encode())
            source.chmod(0o700)
            sources[name] = source
        owner = private_cli.DirectoryOwner()
        self.addCleanup(owner.release)

        install = private_cli.install_adjacent_package(
            owner, sources, self.root / "package",
        )

        for name, source in sources.items():
            copied = install / name
            self.assertTrue(copied.is_file())
            self.assertFalse(copied.is_symlink())
            self.assertNotEqual(copied.stat().st_ino, source.stat().st_ino)
            self.assertEqual(copied.read_bytes(), source.read_bytes())
            self.assertEqual(
                private_cli.file_fingerprint(copied)["sha256"],
                private_cli.file_fingerprint(source)["sha256"],
            )
