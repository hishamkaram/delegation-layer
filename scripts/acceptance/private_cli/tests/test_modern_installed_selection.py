"""Actorless installed selection, current/legacy record shapes and warm-generation controls."""

from copy import deepcopy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts.acceptance.private_cli.contention import controls as controls
from scripts.acceptance.private_cli.contention.policy import ModernInstallation
from scripts.acceptance.private_cli.contention.policy import migration_expectation
from scripts.acceptance.private_cli.contention.policy import require_migration_response
from scripts.acceptance.private_cli.tests.modern_fixtures import ModernControlFixture
from scripts.acceptance.inspections.tests.fixtures import FixtureProcesses
from scripts.acceptance.testing.resources import DirectoryPinOwner
from scripts.acceptance.tasks.operations import NativeTaskOps
from scripts.acceptance.private_cli.tests.modern_application_fixture import publish_pair


class ModernInstalledSelectionTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="modern-installed-selection-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.state = self.root / "state"
        self.supervisor = self.state / ".supervisor"
        self.supervisor.mkdir(parents=True)
        (self.supervisor / "pueue.yml").write_text("fixture config\n")
        self.installations = []
        self.programs = set()
        for name in ("baseline", "candidate"):
            directory = self.root / name
            libexec = directory / "libexec"
            libexec.mkdir(parents=True)
            for path in (directory / "delegate", directory / "delegate-run", libexec / "pueue", libexec / "pueued"):
                path.write_text("original fixture program source\n")
                path.chmod(0o700)
                self.programs.add(path)
            self.installations.append(ModernInstallation(directory / "delegate", directory / "delegate-run", libexec))
        self.baseline, self.candidate = self.installations
        self.logical = {"config_path": str(self.supervisor / "pueue.yml"), "config_digest": "a" * 64,
                        "resolved_config_sha256": "b" * 64, "endpoint": "unix:/fixture.sock"}
        self.binding = {**self.logical, "executable_identity": "installed-path-v1",
                        "client_executable": str(self.baseline.libexec / "pueue"),
                        "daemon_executable": str(self.baseline.libexec / "pueued")}
        self.daemon = {**self.logical, "executable_identity": "installed-path-v1",
                       "executable": self.binding["daemon_executable"]}
        self.write_records()
        original_open = Path.open

        def installed_read_guard(path, mode="r", *args, **kwargs):
            if "r" in mode:
                self.assertNotIn(path, self.programs, "installed executable contents were opened")
            return original_open(path, mode, *args, **kwargs)

        self.enterContext(patch.object(Path, "open", installed_read_guard))

    def write_records(self):
        (self.supervisor / "supervisor.identity.json").write_text(json.dumps({"schema_version": 1, "binding": self.binding}))
        (self.supervisor / "daemon.identity.json").write_text(json.dumps(self.daemon))

    def facts(self, installation=None):
        return controls.observe_migration(self.state, installation or self.candidate)

    def test_observation_reads_only_records_and_keeps_actual_selected_installation_paths(self):
        original_open = Path.open
        before = {name: (self.supervisor / name).read_bytes() for name in
                  ("supervisor.identity.json", "daemon.identity.json")}

        def open_record(path, *args, **kwargs):
            self.assertNotIn(path, self.programs, "installed executable contents were opened")
            return original_open(path, *args, **kwargs)

        with patch.object(Path, "open", open_record):
            facts = self.facts()
        self.assertEqual(facts["writer"]["executable"], str(self.candidate.libexec / "pueued"))
        self.assertEqual(facts["writer"]["executable_identity"], "installed-path-v1")
        self.assertNotIn("sha256", facts["writer"])
        self.assertEqual(before, {name: (self.supervisor / name).read_bytes() for name in before})
        self.assertFalse(any(path.name.startswith("pueue-pair") for path in self.supervisor.iterdir()))

    def test_current_same_path_compatible_update_is_noop_and_distinct_installation_blocks_active_work(self):
        before = migration_expectation(self.facts(self.baseline), active=True)
        (self.baseline.libexec / "pueued").write_text("original compatible replacement fixture source\n")
        self.assertEqual(migration_expectation(self.facts(self.baseline), active=True), before)
        self.assertEqual(before.case, "healthy_same_generation")
        changed = migration_expectation(self.facts(), active=True)
        self.assertEqual((changed.state, changed.reason_code), ("blocked", "active_work"))
        self.assertEqual(migration_expectation(self.facts(), active=False).state, "migrated")

    def test_missing_nonexecutable_and_foreign_role_paths_are_refused_before_any_control_launch(self):
        for role in ("pueue", "pueued"):
            selected = self.candidate.libexec / role
            selected.chmod(0o600)
            with self.subTest(role=role), self.assertRaises(RuntimeError):
                self.facts()
            selected.chmod(0o700)
        (self.candidate.libexec / "pueued").unlink()
        with self.assertRaises(RuntimeError):
            self.facts()
        self.binding["client_executable"] = str(self.baseline.libexec / "pueued")
        self.write_records()
        with self.assertRaises(RuntimeError):
            self.facts(self.baseline)

    def test_markers_mixed_hashes_and_truncated_legacy_shapes_refuse(self):
        original = self.facts()
        for location, field, value in (
            ("published", "executable_identity", "unknown"), ("daemon", "executable_identity", "unknown"),
            ("published", "client_sha256", "c" * 64), ("daemon", "sha256", "c" * 64),
            ("writer", "sha256", "c" * 64), ("published", "executable_identity", None),
        ):
            facts = deepcopy(original)
            facts[location][field] = value
            with self.subTest(location=location, field=field), self.assertRaises(RuntimeError):
                migration_expectation(facts, active=True)

    def test_historical_record_digests_are_decoded_without_executable_reads_or_fresh_compatibility_pins(self):
        self.binding.pop("executable_identity")
        self.daemon.pop("executable_identity")
        self.binding.update(client_sha256="c" * 64, daemon_sha256="d" * 64)
        self.daemon["sha256"] = "d" * 64
        self.write_records()
        original_open = Path.open

        def open_record(path, *args, **kwargs):
            self.assertNotIn(path, self.programs)
            return original_open(path, *args, **kwargs)

        with patch.object(Path, "open", open_record):
            facts = self.facts(self.baseline)
        self.assertEqual(facts["published"], self.binding)
        self.assertEqual(facts["daemon"], self.daemon)
        expected = migration_expectation(facts, active=True)
        facts["published"]["daemon_sha256"] = "e" * 64
        facts["daemon"]["sha256"] = "e" * 64
        self.assertEqual(migration_expectation(facts, active=True), expected)
        self.assertEqual(expected.state, "blocked")

    def test_generated_fixture_publishes_original_installation_paths_without_program_reads_or_inventory(self):
        original_open = Path.open

        def guard(path, mode="r", *args, **kwargs):
            if "r" in mode:
                self.assertNotIn(path.name, ("delegate", "delegate-run", "pueue", "pueued"))
            return original_open(path, mode, *args, **kwargs)

        with patch.object(Path, "open", guard):
            fixture = ModernControlFixture(self)
            before = (fixture.supervisor / "supervisor.identity.json").read_bytes()
            selected = publish_pair(fixture.state, fixture.candidate.libexec, activate=False)
            self.assertEqual(selected, fixture.candidate.libexec)
            self.assertEqual((fixture.supervisor / "supervisor.identity.json").read_bytes(), before)
            self.assertFalse(list(fixture.supervisor.glob("pueue-pair*")))
            fixture.prepare_migration()
            migration = json.loads((fixture.supervisor / "migration.json").read_text())
            self.assertEqual(migration["successor"]["executable"], str(selected / "pueued"))
            self.assertNotIn("sha256", migration["successor"])
        self.assertEqual(fixture.processes.entries, [])

    def test_warm_candidate_admission_keeps_the_canonical_baseline_pair_and_exact_records(self):
        runtime = self.supervisor / "run"
        runtime.mkdir()
        (runtime / "pueue.pid").write_text("424242\n")
        before = controls.active_supervisor_content(self.state)
        responses = [{"task_id": "baseline-task"}, {"task_id": "candidate-task"}]
        tasks = NativeTaskOps(
            FixtureProcesses(), self.baseline.delegate, self.baseline.runner, self.baseline.libexec / "pueue",
            self.root, self.state, self.root)
        ports = DirectoryPinOwner(self).create("modern control fixture", self.root / "controls", self.root)
        fixture = controls.ModernFixture(ports, self.root / "brief.md", self.root, self.root / "gate",
                                         self.root, "fixture-nonce")
        with patch.object(controls, "dispatch_modern_task", side_effect=responses) as dispatch:
            observed = controls.dispatch_modern_pair(tasks, self.baseline, self.candidate,
                                                     fixture, self.root / "brief.md")
        self.assertEqual(observed, responses)
        self.assertEqual(dispatch.call_count, 2)
        self.assertEqual(controls.active_supervisor_content(self.state), before)

    def test_invalid_role_representation_and_daemon_marker_mismatch_are_refused(self):
        for value in ("/selected/pueue/../pueued", "/selected/bad\n/pueued", "/selected/bad\x00/pueued"):
            facts = self.facts()
            facts["writer"]["executable"] = value
            with self.subTest(path=value), self.assertRaises(RuntimeError):
                migration_expectation(facts, active=True)
        facts = self.facts()
        facts["daemon"].pop("executable_identity")
        facts["daemon"]["sha256"] = "d" * 64
        with self.assertRaisesRegex(RuntimeError, "provenance"):
            migration_expectation(facts, active=True)

    def test_accepted_historical_successor_is_read_without_fresh_hash_comparison_or_retarget(self):
        self.binding.pop("executable_identity")
        self.daemon.pop("executable_identity")
        self.binding.update(client_sha256="c" * 64, daemon_sha256="d" * 64)
        self.daemon["sha256"] = "d" * 64
        self.write_records()
        facts = self.facts()
        facts["migration"] = {"schema_version": 1, "legacy": dict(self.daemon),
                              "successor": {**self.logical, "executable": facts["writer"]["executable"],
                                            "sha256": "f" * 64}}
        before = deepcopy(facts)
        self.assertEqual(migration_expectation(facts, active=True).state, "blocked")
        self.assertEqual(facts, before)
        facts["writer"]["executable"] = "/another-install/pueued"
        with self.assertRaisesRegex(RuntimeError, "recorded prepared successor"):
            migration_expectation(facts, active=False)

    def test_compatible_path_selection_never_waives_an_incompatible_runtime_response(self):
        expected = migration_expectation(self.facts(self.baseline), active=True)
        response = {"schema_version": 1, "command": "supervisor", "root_id": "root",
                    "supervisor_control": {"action": "migrate", "state": "migrated", "active_work": "present",
                                           "migration_phase": "complete", "next_action": "retry_dispatch"},
                    "error": "installed CLI required protocol is unavailable"}
        with self.assertRaisesRegex(RuntimeError, "unexpected error"):
            require_migration_response(response, "root", expected)


if __name__ == "__main__":
    unittest.main()
