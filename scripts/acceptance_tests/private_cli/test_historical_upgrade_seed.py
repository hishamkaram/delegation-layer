"""Read-only upgrade controls preserve real installed identities and accepted proposals."""

import hashlib
import io
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from scripts.delegate_acceptance.evidence.encoding import canonical_go_json
from scripts.delegate_acceptance.evidence.files import read_json, write_json, write_bytes
from scripts.delegate_acceptance.private_cli import upgrade
from scripts.acceptance_tests.private_cli.fixtures import materialize_candidate_pair, materialize_supervisor_pair
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, CommandFailure


class InstalledUpgradeRecordTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-upgrade-records-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.state = self.root / "state"
        self.supervisor = self.state / ".supervisor"
        self.libexec = self.root / "libexec"
        self.supervisor.mkdir(parents=True)
        self.libexec.mkdir()
        self.binding = {"executable_identity": "installed-path-v1"}
        for name, field in (("pueue", "client"), ("pueued", "daemon")):
            executable = self.libexec / name
            executable.write_text("original fixture program source\n")
            executable.chmod(0o700)
            self.binding[field + "_executable"] = str(executable)
        write_json(self.supervisor / "supervisor.identity.json", {"schema_version": 1, "binding": self.binding})
        write_json(self.supervisor / "daemon.identity.json", {
            "executable_identity": "installed-path-v1", "executable": str(self.libexec / "pueued")})
        for name in ("transition.lock", "bootstrap.lock"):
            (self.supervisor / name).touch()

    def record_bytes(self):
        return {name: (self.supervisor / name).read_bytes() for name in
                ("supervisor.identity.json", "daemon.identity.json")}

    def test_observation_preserves_both_records_without_opening_installed_programs(self):
        before = self.record_bytes()
        real_open = Path.open

        def open_record(path, *args, **kwargs):
            if path.parent == self.libexec:
                self.fail("installed program contents were opened")
            return real_open(path, *args, **kwargs)

        with patch.object(Path, "open", open_record):
            result = upgrade.observe_installed_package(self.state, self.libexec)
        self.assertEqual(result, self.binding)
        self.assertEqual(self.record_bytes(), before)

    def test_missing_or_wrong_installed_role_and_pending_transition_preserve_records(self):
        before = self.record_bytes()
        for name in ("pueue", "pueued"):
            executable = self.libexec / name
            executable.chmod(0o600)
            with self.subTest(role=name), self.assertRaisesRegex(RuntimeError, "unexpected installed supervisor"):
                upgrade.observe_installed_package(self.state, self.libexec)
            self.assertEqual(self.record_bytes(), before)
            executable.chmod(0o700)
        (self.supervisor / "migration.json").write_text("{}")
        with self.assertRaisesRegex(RuntimeError, "pending migration"):
            upgrade.observe_installed_package(self.state, self.libexec)
        self.assertEqual(self.record_bytes(), before)

    def test_no_fabricated_historical_or_mixed_executable_identity(self):
        path = self.supervisor / "supervisor.identity.json"
        for binding in ({key: value for key, value in self.binding.items() if key != "executable_identity"},
                        {**self.binding, "daemon_sha256": "historical-value"}):
            write_json(path, {"schema_version": 1, "binding": binding}, replace=True)
            before = self.record_bytes()
            with self.subTest(binding=binding), self.assertRaises(RuntimeError):
                upgrade.observe_installed_package(self.state, self.libexec)
            self.assertEqual(self.record_bytes(), before)

    def pending(self, *, binding=None, shutdown=False, proposal_digest=None):
        group_directory = self.state / "inspection-group"
        group_directory.mkdir(exist_ok=True)
        successor = {"root_id": "r" * 32, "supervisor": binding or self.binding}
        group = {"successor": successor, "successor_sha256": proposal_digest or
                 hashlib.sha256(canonical_go_json(successor)).hexdigest()}
        write_bytes(group_directory / "migration.json", canonical_go_json(group), replace=True)
        write_json(self.supervisor / "migration.json", {
            "legacy_shutdown_requested": shutdown,
            "successor": {"executable": str(self.libexec / "pueued")},
        }, replace=True)
        return group_directory / "migration.json", self.supervisor / "migration.json"

    def test_pending_proposal_is_validated_without_rewriting_identity_or_sealed_evidence(self):
        paths = self.pending()
        before = {path: path.read_bytes() for path in paths}
        upgrade.require_pending_successor(self.libexec, self.state, self.supervisor)
        self.assertEqual({path: path.read_bytes() for path in paths}, before)
        self.assertEqual(read_json(paths[0])["successor"]["supervisor"], self.binding)

    def test_pending_proposal_rejects_foreign_role_digest_drift_and_shutdown_progress(self):
        invalid = (
            {"binding": {**self.binding, "client_executable": str(self.root / "pueue")}},
            {"binding": {**self.binding, "daemon_sha256": "historical-value"}},
            {"proposal_digest": "0" * 64}, {"shutdown": True},
        )
        for options in invalid:
            with self.subTest(options=options):
                paths = self.pending(**options)
                before = {path: path.read_bytes() for path in paths}
                with self.assertRaises(RuntimeError):
                    upgrade.require_pending_successor(self.libexec, self.state, self.supervisor)
                self.assertEqual({path: path.read_bytes() for path in paths}, before)

    def run_removed_successor(self, *, rewrite=False):
        config = self.supervisor / "pueue.yml"
        config.write_text("fixture configuration\n")
        handle = io.BytesIO(b"fixture configuration\n")
        self.addCleanup(handle.close)
        predecessor = SimpleNamespace(backup_content=handle.getvalue(), backup_handle=handle)
        cleanups = []
        observed = {}
        test = self

        def interrupt(*, delegate, package_owner, pending_libexec, **_options):
            test.assertEqual(delegate, self.root / "pending-package/install/delegate")
            test.assertTrue(delegate.is_file() and (pending_libexec / "pueued").is_file())
            observed["selected"] = delegate
            self.pending(binding={**self.binding,
                                 "client_executable": str(pending_libexec / "pueue"),
                                 "daemon_executable": str(pending_libexec / "pueued")})
            package_owner.rollback()
            return "/unused/socket"

        def retry(_delegate, _environment, _arguments, **options):
            test.assertFalse(observed["selected"].exists())
            test.assertEqual(options["expected"], (1,))
            if rewrite:
                (self.supervisor / "migration.json").write_text("rewritten accepted target\n")
            return {"supervisor_control": {"state": "blocked"}}

        with patch.object(upgrade, "install_package_layout", wraps=upgrade.install_package_layout) as install, \
                patch("scripts.delegate_acceptance.private_cli.installation.build_candidate_pair",
                      side_effect=materialize_candidate_pair), \
                patch.object(upgrade, "install_supervisor_pair", side_effect=materialize_supervisor_pair), \
                patch.object(upgrade, "capture_predecessor_shutdown_target", return_value=predecessor), \
                patch.object(upgrade, "interrupt_pending_migration", side_effect=interrupt), \
                patch.object(upgrade, "run_json", side_effect=retry), \
                patch.object(upgrade, "shutdown_predecessor_supervisor") as shutdown, \
                patch.object(upgrade, "await_pending_predecessor"):
            if rewrite:
                with self.assertRaisesRegex(RuntimeError, "rewrote accepted migration evidence"):
                    upgrade.run_pending_package_upgrade(
                        delegate=self.root / "current/delegate", client=self.libexec / "pueue",
                        real_client=self.libexec / "pueue", state=self.state, config=config, env={},
                        receipts=self.root, supervisor_cleanups=cleanups, attempted_task_ids=[], capture_root=None,
                        fixture_base=self.root, source_root=Path("/explicit/source"),
                    )
                shutdown.assert_not_called()
                self.assertFalse(handle.closed)
                self.assertEqual(len(cleanups), 1)
            else:
                evidence = upgrade.run_pending_package_upgrade(
                    delegate=self.root / "current/delegate", client=self.libexec / "pueue",
                    real_client=self.libexec / "pueue", state=self.state, config=config, env={},
                    receipts=self.root, supervisor_cleanups=cleanups, attempted_task_ids=[], capture_root=None,
                    fixture_base=self.root, source_root=Path("/explicit/source"),
                )
                self.assertTrue(evidence["missing_successor_refused"])
                self.assertTrue(evidence["accepted_migration_preserved"])
                self.assertEqual(cleanups, [])
                self.assertTrue(handle.closed)
                shutdown.assert_called_once()
            self.assertEqual(install.call_args.args[1:],
                             (Path("/explicit/source"), self.root / "pending-package", False))
        self.assertFalse((self.root / "pending-package").exists())

    def test_removed_accepted_successor_refuses_without_retarget_and_closes_predecessor_handle(self):
        self.run_removed_successor()

    def test_rewritten_accepted_target_fails_and_keeps_predecessor_cleanup_owned(self):
        self.run_removed_successor(rewrite=True)

    def test_unproven_build_child_closure_retains_the_pending_installation_namespace(self):
        token = None

        def fail_stage(*, package_owner, base, **_options):
            nonlocal token
            token = package_owner.create("pending package", base / "pending-package", base)
            raise RuntimeError("candidate installation failed") from BoundedCommandError(CommandFailure.TERMINATION_TIMEOUT)

        with patch.object(upgrade, "stage_pending_package", side_effect=fail_stage):
            with self.assertRaisesRegex(RuntimeError, "candidate installation failed") as error:
                upgrade.run_pending_package_upgrade(
                    delegate=self.root / "current/delegate", client=self.libexec / "pueue",
                    real_client=self.libexec / "pueue", state=self.state, config=self.supervisor / "pueue.yml",
                    env={}, receipts=self.root, supervisor_cleanups=[], attempted_task_ids=[], capture_root=None,
                    fixture_base=self.root, source_root=Path("/explicit/source"),
                )
        self.assertTrue((self.root / "pending-package").is_dir())
        if token is None:
            self.fail("the failed stage did not register its owned pending package")
        self.assertTrue(token.consumed)
        self.assertTrue(any(str(self.root / "pending-package") in note for note in error.exception.__notes__))


if __name__ == "__main__":
    unittest.main()
