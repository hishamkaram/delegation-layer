"""Upgrade controls preserve immutable proposals across installed retargets."""

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
        self.binding = {"executable_identity": "installed-path-v1", "observed_version": "4.0.4", "config_path": "/fixture/old-config.yml",
                        "config_digest": "a" * 64, "resolved_config_sha256": "d" * 64,
                        "endpoint": "unix:/fixture/a.sock"}
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

    def pending(self, *, binding=None, shutdown=False, proposal_digest=None, source=None, group_directory=None,
                legacy_pid=0, legacy_process_start=""):
        source = self.binding if source is None else source
        group_directory = self.state / "inspection-group" if group_directory is None else group_directory
        group_directory.mkdir(exist_ok=True)
        root_id = "a" * 32
        legacy = {"schema_version": 1, "root_id": root_id, "supervisor": source,
                  "name": "delegation-inspection-" + root_id, "parallel_tasks": 1}
        successor = {"schema_version": 1, "root_id": root_id, "supervisor": binding or self.binding,
                     "name": "delegation-inspection-" + root_id, "parallel_tasks": 1}
        group = {"schema_version": 1, "root_id": root_id,
                 "legacy_sha256": hashlib.sha256(canonical_go_json(legacy)).hexdigest(),
                 "successor_sha256": proposal_digest or hashlib.sha256(canonical_go_json(successor)).hexdigest(),
                 "successor": successor}
        write_bytes(group_directory / "request.json", canonical_go_json(legacy), replace=True)
        write_bytes(group_directory / "migration.json", canonical_go_json(group), replace=True)
        write_json(self.supervisor / "migration.json", {
            "legacy": {name: source[name] for name in
                       ("config_path", "config_digest", "resolved_config_sha256", "endpoint")},
            "legacy_shutdown_requested": shutdown, "legacy_pid": legacy_pid,
            "legacy_process_start": legacy_process_start,
            "successor": {"executable": (binding or self.binding)["daemon_executable"]},
        }, replace=True)
        return group_directory / "migration.json", self.supervisor / "migration.json"

    def generation_history(self):
        active = {**self.binding, "config_path": "/fixture/config<&>.yml", "config_digest": "b" * 64,
                  "resolved_config_sha256": "c" * 64, "endpoint": "unix:/fixture/b.sock"}
        group_path, _private = self.pending(binding=active)
        write_bytes(group_path.parent / "migration-complete.json", group_path.read_bytes(), replace=True)
        write_json(self.supervisor / "supervisor.identity.json", {"schema_version": 1, "binding": active}, replace=True)
        # Independently authored Go struct bytes: exact field order, HTML escapes,
        # and trailing newline from GroupGeneration + task.MarshalCanonical.
        expected = (b'{"config_path":"/fixture/config\\u003c\\u0026\\u003e.yml","config_digest":"' + b'b' * 64
                    + b'","resolved_config_sha256":"' + b'c' * 64 + b'","endpoint":"unix:/fixture/b.sock"}\n')
        directory = self.state / ("inspection-group-" + hashlib.sha256(expected).hexdigest()[:16])
        return active, directory

    def successive_pending(self):
        active, directory = self.generation_history()
        pending = self.root / "pending-libexec"
        pending.mkdir(exist_ok=True)
        target = {**active, "config_digest": "e" * 64, "resolved_config_sha256": "f" * 64,
                  "endpoint": "unix:/fixture/c.sock"}
        for name, field in (("pueue", "client"), ("pueued", "daemon")):
            program = pending / name
            program.write_text("original finite fixture program source\n")
            program.chmod(0o700)
            target[field + "_executable"] = str(program)
        paths = self.pending(binding=target, source=active, group_directory=directory)
        return pending, active, directory, paths

    def test_successive_pending_selects_current_generation_instead_of_completed_root_history(self):
        pending, active, directory, paths = self.successive_pending()
        self.assertEqual(read_json(self.state / "inspection-group/request.json")["supervisor"], self.binding)
        request = read_json(directory / "request.json")
        self.assertEqual(request["supervisor"], active)
        self.assertEqual(tuple(request), ("schema_version", "root_id", "supervisor", "name", "parallel_tasks"))
        self.assertEqual(tuple(read_json(paths[0])),
                         ("schema_version", "root_id", "legacy_sha256", "successor_sha256", "successor"))
        protected = (*paths, self.state / "inspection-group/migration.json",
                     self.state / "inspection-group/migration-complete.json")
        before = {path: path.read_bytes() for path in protected}
        upgrade.require_pending_successor(pending, self.state, self.supervisor)
        self.assertEqual({path: path.read_bytes() for path in protected}, before)

    def test_successive_pending_rejects_missing_source_request_journal_and_invalid_proposal(self):
        for fault in ("missing source", "empty coordinate", "wrong coordinate type", "missing root request",
                      "missing generation request", "wrong generation request", "invalid root request", "missing generation journal",
                      "proposal digest", "wrong installed role", "missing installed role", "wrong private target"):
            with self.subTest(fault=fault):
                pending, _active, directory, paths = self.successive_pending()
                if fault in ("missing source", "empty coordinate", "wrong coordinate type"):
                    record = read_json(paths[1])
                    if fault == "missing source":
                        record.pop("legacy")
                    else:
                        record["legacy"]["endpoint"] = "" if fault == "empty coordinate" else 7
                    write_json(paths[1], record, replace=True)
                elif fault in ("missing root request", "missing generation request", "missing generation journal"):
                    path = {"missing root request": self.state / "inspection-group/request.json",
                            "missing generation request": directory / "request.json",
                            "missing generation journal": directory / "migration.json"}[fault]
                    path.unlink()
                elif fault == "wrong generation request":
                    write_json(directory / "request.json", {"supervisor": self.binding}, replace=True)
                elif fault == "invalid root request":
                    write_json(self.state / "inspection-group/request.json", {"supervisor": {"endpoint": "unix:/bad"}}, replace=True)
                elif fault == "wrong private target":
                    private = read_json(paths[1])
                    private["successor"]["executable"] = str(self.libexec / "pueued")
                    write_json(paths[1], private, replace=True)
                elif fault == "missing installed role":
                    (pending / "pueue").unlink()
                else:
                    proposal = read_json(paths[0])
                    if fault == "proposal digest":
                        proposal["successor_sha256"] = "0" * 64
                    else:
                        proposal["successor"]["supervisor"]["client_executable"] = str(self.libexec / "pueue")
                        proposal["successor_sha256"] = hashlib.sha256(canonical_go_json(proposal["successor"])).hexdigest()
                    write_bytes(paths[0], canonical_go_json(proposal), replace=True)
                before = {path: path.read_bytes() for path in (*paths, directory / "request.json") if path.exists()}
                with self.assertRaises((RuntimeError, FileNotFoundError)):
                    upgrade.require_pending_successor(pending, self.state, self.supervisor)
                self.assertEqual({path: path.read_bytes() for path in before}, before)


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
            {"legacy_pid": 1}, {"legacy_process_start": "recorded-process"},
        )
        for options in invalid:
            with self.subTest(options=options):
                paths = self.pending(**options)
                before = {path: path.read_bytes() for path in paths}
                with self.assertRaises(RuntimeError):
                    upgrade.require_pending_successor(self.libexec, self.state, self.supervisor)
                self.assertEqual({path: path.read_bytes() for path in paths}, before)

    def run_reversible_retarget(self, *, fault=None, generation=False, wrong_source=False):
        active, group_directory = self.generation_history() if generation else (self.binding, self.state / "inspection-group")
        history = {path: path.read_bytes() for path in (self.state / "inspection-group/migration.json",
                   self.state / "inspection-group/migration-complete.json")} if generation else {}
        config = self.supervisor / "pueue.yml"
        config.write_text("fixture configuration\n")
        handle = io.BytesIO(b"fixture configuration\n")
        self.addCleanup(handle.close)
        predecessor = SimpleNamespace(backup_content=handle.getvalue(), backup_handle=handle)
        cleanups = []
        observed = {}
        current = {**active, "config_path": str(config), "config_digest": "e" * 64,
                   "resolved_config_sha256": "f" * 64, "endpoint": "unix:" + str(config.parent / "run/upgrade.sock")}

        def interrupt(*, delegate, package_owner, pending_libexec, **_options):
            self.assertEqual(delegate, self.root / "pending-package/install/delegate")
            self.assertTrue(delegate.is_file() and (pending_libexec / "pueued").is_file())
            observed["selected"] = delegate
            self.pending(binding={**current, "client_executable": str(pending_libexec / "pueue"),
                                  "daemon_executable": str(pending_libexec / "pueued")},
                         source=active, group_directory=group_directory)
            if wrong_source:
                path = self.supervisor / "migration.json"
                private = read_json(path)
                private["legacy"] = {name: self.binding[name] for name in
                                     ("config_path", "config_digest", "resolved_config_sha256", "endpoint")}
                write_json(path, private, replace=True)
            package_owner.rollback()
            return "/unused/socket"

        def retry(_delegate, _environment, _arguments, **options):
            self.assertFalse(observed["selected"].exists())
            observed["retry_options"] = options
            if fault == "nonzero":
                raise RuntimeError("delegate command failed")
            selected = {**current}
            if fault == "wrong pair":
                selected["daemon_executable"] = str(self.root / "other/pueued")
            if fault == "endpoint":
                selected["endpoint"] = "unix:/wrong.sock"
            write_json(self.supervisor / "supervisor.identity.json", {"schema_version": 1, "binding": selected}, replace=True)
            write_json(self.supervisor / "daemon.identity.json",
                       {"executable_identity": "installed-path-v1", "executable": selected["daemon_executable"]}, replace=True)
            primary = read_json(group_directory / "migration.json")
            successor = {"schema_version": 1, "root_id": "a" * 32, "supervisor": selected,
                         "name": "delegation-inspection-" + "a" * 32, "parallel_tasks": 1}
            completion = {"schema_version": 1, "root_id": "a" * 32, "legacy_sha256": primary["legacy_sha256"],
                          "successor_sha256": hashlib.sha256(canonical_go_json(successor)).hexdigest(),
                          "successor": successor}
            alternate = group_directory / ("migration-" + completion["successor_sha256"][:32] + ".json")
            write_bytes(alternate, canonical_go_json(completion), replace=True)
            if fault == "completion digest":
                completion["successor_sha256"] = "0" * 64
            elif fault == "completion binding":
                completion = {**completion, "successor": {**successor, "supervisor": active}}
            elif fault == "completion source":
                completion["legacy_sha256"] = "0" * 64
            write_bytes(group_directory / "migration-complete.json", canonical_go_json(completion), replace=True)
            if fault == "alternate":
                write_bytes(alternate, canonical_go_json({**completion, "legacy_sha256": "0" * 64}), replace=True)
            if fault != "private pending":
                (self.supervisor / "migration.json").unlink()
            if fault == "proposal":
                (group_directory / "migration.json").write_text("changed original C proposal\n")
            elif fault == "root history":
                (self.state / "inspection-group/migration-complete.json").write_text("changed root history\n")
            control = {"state": "migrated", "next_action": "retry_dispatch"}
            if fault == "blocked":
                control["state"] = "blocked"
            elif fault == "wrong action":
                control["next_action"] = "stop_and_report"
            return {"supervisor_control": control}

        rows = {"7": {"id": 7, "label": "original ended row", "status": {"Done": {"result": "Success"}}}}
        snapshots = [{"tasks": rows}, {"tasks": {**rows, "8": {"id": 8}} if fault == "queue" else rows}]
        with patch.object(upgrade, "install_package_layout", wraps=upgrade.install_package_layout) as install, \
                patch("scripts.delegate_acceptance.private_cli.installation.build_candidate_pair",
                      side_effect=materialize_candidate_pair), \
                patch.object(upgrade, "install_supervisor_pair", side_effect=materialize_supervisor_pair), \
                patch.object(upgrade, "capture_predecessor_shutdown_target", return_value=predecessor), \
                patch.object(upgrade, "interrupt_pending_migration", side_effect=interrupt), \
                patch.object(upgrade, "run_json", side_effect=retry), \
                patch.object(upgrade, "pueue_snapshot", side_effect=snapshots), \
                patch.object(upgrade, "await_pending_predecessor") as awaited:
            options = dict(delegate=self.root / "current/delegate", client=self.libexec / "pueue",
                           real_client=self.libexec / "pueue", state=self.state, config=config, env={},
                           receipts=self.root, supervisor_cleanups=cleanups, attempted_task_ids=[], capture_root=None,
                           fixture_base=self.root, source_root=Path("/explicit/source"))
            if fault or wrong_source:
                expected = {
                    "proposal": "rewrote immutable inspection", "root history": "rewrote immutable inspection",
                    "private pending": "pending migration.json", "wrong pair": "supervisor selection is unavailable",
                    "endpoint": "upgrade endpoint", "queue": "queue rows", "nonzero": "delegate command failed",
                    "blocked": "did not complete reversible", "wrong action": "did not complete reversible",
                }.get(fault, "source disagrees" if wrong_source else "retarget completion")
                with self.assertRaisesRegex(RuntimeError, expected):
                    upgrade.run_pending_package_upgrade(**options)
                awaited.assert_not_called()
                self.assertFalse(handle.closed)
                self.assertEqual(len(cleanups), 1)
            else:
                evidence = upgrade.run_pending_package_upgrade(**options)
                self.assertTrue(evidence["reversible_retarget_completed"])
                self.assertTrue(evidence["inspection_proposals_preserved"])
                self.assertTrue(evidence["queue_rows_preserved"])
                self.assertEqual(evidence["active_binding"], current)
                self.assertEqual(observed["retry_options"]["expected"], (0,))
                self.assertEqual(cleanups, [])
                self.assertTrue(handle.closed)
                awaited.assert_called_once_with(None, predecessor)
            self.assertEqual(install.call_args.args[1:],
                             (Path("/explicit/source"), self.root / "pending-package", False))
        self.assertFalse((self.root / "pending-package").exists())
        if fault != "root history":
            self.assertEqual({path: path.read_bytes() for path in history}, history)

    def test_removed_pre_shutdown_successor_retargets_and_closes_predecessor_handle(self):
        self.run_reversible_retarget()

    def test_unconsumed_private_journal_retains_predecessor_cleanup_obligation(self):
        self.run_reversible_retarget(fault="private pending")

    def test_retarget_preserves_current_source_proposal_and_completed_root_history(self):
        self.run_reversible_retarget(generation=True)

    def test_retarget_rejects_immutable_proposal_and_root_history_rewrite(self):
        for fault in ("proposal", "root history"):
            with self.subTest(fault=fault):
                self.run_reversible_retarget(generation=True, fault=fault)

    def test_retarget_rejects_invalid_completion_pair_queue_and_public_result(self):
        for fault in ("completion digest", "completion binding", "completion source", "alternate",
                      "wrong pair", "endpoint", "queue", "blocked", "wrong action", "nonzero"):
            with self.subTest(fault=fault):
                self.run_reversible_retarget(generation=True, fault=fault)

    def test_retarget_rejects_private_source_disagreement(self):
        self.run_reversible_retarget(generation=True, wrong_source=True)

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
