"""Implementation methods for HistoricalUpgradeSeedTests; collected once by the acceptance_tests discovery."""

import hashlib
from pathlib import Path
import tempfile
import unittest
from scripts import acceptance_private_cli as private_cli
from scripts import acceptance_supervisor_common as common


class HistoricalUpgradeSeedTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-upgrade-seed-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.state = self.root / "state"
        self.supervisor = self.state / ".supervisor"
        self.libexec = self.root / "libexec"
        self.supervisor.mkdir(parents=True)
        self.libexec.mkdir()
        self.binding = {}
        for name, field in (("pueue", "client"), ("pueued", "daemon")):
            data = name.encode()
            (self.libexec / name).write_bytes(data)
            self.binding[field + "_sha256"] = hashlib.sha256(data).hexdigest()
            self.binding[field + "_executable"] = "/retained/" + name
        common.write_json(self.supervisor / "supervisor.identity.json", {"schema_version": 1, "binding": self.binding})
        common.write_json(self.supervisor / "daemon.identity.json", {"executable": "/retained/pueued", "sha256": self.binding["daemon_sha256"]})
        for name in ("transition.lock", "bootstrap.lock"):
            (self.supervisor / name).touch()

    def test_split_seed_preserves_daemon_witness(self):
        before = (self.supervisor / "daemon.identity.json").read_bytes()
        result = private_cli.seed_historical_private_paths(self.state, self.libexec, split=True)
        self.assertEqual(result["daemon_executable"], str(self.libexec / "pueued"))
        self.assertEqual((self.supervisor / "daemon.identity.json").read_bytes(), before)

    def test_coherent_seed_names_same_installation(self):
        result = private_cli.seed_historical_private_paths(self.state, self.libexec, split=False)
        identity = common.read_json(self.supervisor / "daemon.identity.json")
        self.assertEqual(identity["executable"], result["daemon_executable"])

    def test_different_bytes_and_pending_transitions_leave_records_unchanged(self):
        before = (self.supervisor / "supervisor.identity.json").read_bytes()
        (self.libexec / "pueued").write_bytes(b"changed daemon")
        with self.assertRaisesRegex(RuntimeError, "changed executable bytes"):
            private_cli.seed_historical_private_paths(self.state, self.libexec, split=False)
        self.assertEqual((self.supervisor / "supervisor.identity.json").read_bytes(), before)
        (self.supervisor / "migration.json").write_text("{}")
        with self.assertRaisesRegex(RuntimeError, "pending migration"):
            private_cli.seed_historical_private_paths(self.state, self.libexec, split=True)
        self.assertEqual((self.supervisor / "supervisor.identity.json").read_bytes(), before)
