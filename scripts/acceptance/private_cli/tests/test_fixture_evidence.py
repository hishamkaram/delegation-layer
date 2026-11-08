"""Installed observations never open executable contents; source and config evidence stays hashed."""

import hashlib
import json
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch


from scripts.acceptance.private_cli import fingerprints as fingerprints
from scripts.acceptance.private_cli.handoff import workflow as handoff
from scripts.acceptance.private_cli import stop as stop
from scripts.acceptance.private_cli import initial as initial
from scripts.acceptance.private_cli.receipt import AcceptanceReceipt
from scripts.acceptance.workspaces.scenarios.evidence import ReceiptFinalizer


class FixtureEvidenceTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-evidence-observation-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.executable = self.root / "fixture"
        self.executable.write_text("original executable fixture source\n")
        self.executable.chmod(0o700)
        self.config = self.root / "config.yml"
        self.config.write_text("fixture configuration\n")

    def forbid_executable_reads(self):
        selected = {self.executable.resolve(), Path(sys.executable).resolve()}
        real_read, real_open = Path.read_bytes, Path.open

        def read(path):
            self.assertNotIn(path.resolve(), selected, "installed executable was read")
            return real_read(path)

        def open_input(path, *args, **kwargs):
            self.assertNotIn(path.resolve(), selected, "installed executable was opened")
            return real_open(path, *args, **kwargs)

        self.enterContext(patch.object(Path, "read_bytes", read))
        self.enterContext(patch.object(Path, "open", open_input))

    def test_observation_uses_metadata_through_symlink_and_reports_unavailable_files(self):
        self.forbid_executable_reads()
        link = self.root / "selected"
        link.symlink_to(self.executable.name)
        observed = fingerprints.installed_executable_observation(link)
        self.assertEqual(observed, {
            "representation": "installed-paths", "path": str(self.executable.resolve()),
            "name": "fixture", "bytes": self.executable.stat().st_size,
        })
        for path in (self.config, self.root, self.root / "missing"):
            with self.subTest(path=path):
                self.assertEqual(fingerprints.installed_executable_observation(path),
                                 {"name": path.name, "status": "unavailable"})

    def test_private_receipt_and_stop_fixture_keep_only_installed_metadata(self):
        self.forbid_executable_reads()
        receipt = AcceptanceReceipt(self.root / "receipt.json", 1, 1, {"delegate": self.executable})
        self.assertEqual(receipt.fingerprints["binaries"]["delegate"]["path"], str(self.executable))
        self.assertNotIn("sha256", receipt.fingerprints["binaries"]["delegate"])
        observed = stop.initial_stop_evidence(self.executable, {})
        self.assertEqual(observed["fingerprints"]["provider_fixture"],
                         fingerprints.installed_executable_observation(self.executable))
        self.assertIn("sha256", receipt.fingerprints["source"]["acceptance/private_cli/cli.py"])

    def test_handoff_preserves_configuration_integrity_with_installed_fixture_observation(self):
        self.forbid_executable_reads()
        case = SimpleNamespace(case_label="case", handoff_action="explicit-migrate",
                               provider_fixture=self.executable, private_config=self.root / "private.yml")
        observed = handoff._initial_handoff_evidence(case, self.config, self.root / "socket", self.root / "pid")
        self.assertNotIn("sha256", observed["provider_fixture"])
        self.assertEqual(observed["supervisors"]["external"]["config_fingerprint"]["sha256"],
                         hashlib.sha256(b"fixture configuration\n").hexdigest())
        self.assertTrue(all("sha256" in value for value in observed["source_fingerprints"].values()))
        self.assertIn("acceptance/directories/operations.py", observed["source_fingerprints"])
        self.assertIn("acceptance/private_cli/handoff/workflow.py", observed["source_fingerprints"])

    def test_workspace_receipt_observes_selected_tools_and_python_without_contents(self):
        self.forbid_executable_reads()
        output = self.root / "workspace-receipt.json"
        finalizer = ReceiptFinalizer(output, [], {"delegate": self.executable, "acceptance-driver": self.config})
        finalizer.refresh_tool_fingerprints()
        finalizer.finalize("failed", "record-only fixture", {})
        payload = json.loads(output.read_text())
        self.assertNotIn("sha256", payload["tools"]["delegate"])
        self.assertIn("sha256", payload["tools"]["acceptance-driver"])
        self.assertNotIn("sha256", payload["platform"]["python"])

    def test_initial_provenance_preserves_daemon_record_and_accepts_independent_equal_programs(self):
        old = self.root / "old"
        upgraded = self.root / "upgraded"
        old.mkdir()
        upgraded.mkdir()
        programs = set()
        for directory in (old, upgraded):
            for role in ("pueue", "pueued"):
                program = directory / role
                program.write_text("original identical fixture program source\n")
                program.chmod(0o700)
                programs.add(program)
        state = self.root / "state"
        supervisor = state / ".supervisor"
        task = state / "tasks" / "task"
        supervisor.mkdir(parents=True)
        task.mkdir(parents=True)
        config = supervisor / "pueue.yml"
        config.write_text("fixture config\n")
        identity = supervisor / "daemon.identity.json"
        identity.write_text(json.dumps({"executable": str(old / "pueued"),
                                        "executable_identity": "installed-path-v1"}))
        (task / "meta.json").write_text(json.dumps({"supervisor_config": {
            "client_executable": str(old / "pueue"), "daemon_executable": str(old / "pueued")}}))
        (task / "outcome.json").write_text('{"verdict":"committed"}')
        marker = self.root / "starts"
        marker.write_text("started\n")
        fixture = SimpleNamespace(state=state, old_daemon_marker=marker)
        lifetime = SimpleNamespace(private_config=config, old_libexec=old, upgraded_libexec=upgraded)
        record = identity.read_bytes()
        real_open = Path.open

        def open_input(path, *args, **kwargs):
            self.assertNotIn(path, programs, "installed executable was opened for provenance")
            return real_open(path, *args, **kwargs)

        with patch.object(Path, "open", open_input), \
                patch.object(initial, "assert_state_supervisor_pair", return_value=old):
            observed = initial.read_initial_provenance(fixture, False, lifetime, "task")
        self.assertEqual(observed, (old, old / "pueued", identity, record, upgraded / "pueued"))


if __name__ == "__main__":
    unittest.main()
