"""Real finite fork/exec fixture controls through canonical process ownership."""

import hashlib
from pathlib import Path
import unittest
from unittest.mock import patch

from scripts.acceptance_tests.private_cli.modern_fixtures import ModernControlFixture
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.processes import operations as process_owner
from scripts.delegate_acceptance.private_cli import contention_controls as controls
from scripts.delegate_acceptance.private_cli.contention_locks import probe_generation_lease
from scripts.delegate_acceptance.private_cli.contention_policy import require_content_identity
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps


class ModernBinaryControlTests(unittest.TestCase):
    def setUp(self):
        self.fixture = ModernControlFixture(self)
        self.task = self.fixture.minimal_task()
        self.live_checks = 0

    def immutable(self):
        return {
            "root": hashlib.sha256((self.fixture.state / "root.json").read_bytes()).hexdigest(),
            "meta": hashlib.sha256((self.task / "meta.json").read_bytes()).hexdigest(),
        }

    def live(self):
        self.live_checks += 1

    def prove(self, *, reverse=False, label="round"):
        reader, writer = self.fixture.baseline, self.fixture.candidate
        if reverse:
            reader, writer = writer, reader
        return controls.prove_contention_round(
            processes=self.fixture.processes,
            reader=reader.delegate,
            writer=writer,
            state=self.fixture.state,
            controls=self.fixture.controls,
            label=label,
            task_id=self.fixture.task_id,
            immutable_content=self.immutable,
            assert_live=self.live,
        )

    def test_actual_two_fixture_binaries_share_root_then_exclude_writer_both_directions(self):
        initial = self.immutable()
        for reverse in (True, False):
            evidence = self.prove(reverse=reverse, label="reverse" if reverse else "forward")
            self.assertEqual(evidence["active_work_refused"], not reverse)
            expected = evidence["migration_expectation"]
            self.assertEqual((expected["exit_code"], expected["state"]), (0, "migrated") if reverse else (1, "blocked"))
            self.assertEqual(evidence["root_id"], self.fixture.root_id)
            self.assertEqual(evidence["task_id"], self.fixture.task_id)
            self.assertEqual(evidence["immutable_content_before"], evidence["immutable_content_after"])
            self.assertFalse(evidence["shared"]["lease"]["probe_exclusive_acquired"])
            self.assertFalse(evidence["exclusive"]["lease"]["probe_shared_acquired"])
            for role in ("shared", "exclusive"):
                receipt = evidence[role]["boundary"]
                self.assertEqual(len({receipt[key] for key in ("cli_pid", "control_owner_pid", "client_pid")}), 3)
            self.assertLessEqual(evidence["shared_release_ns"], evidence["exclusive"]["boundary"]["entered_ns"])
        self.assertEqual(self.live_checks, 6)
        self.assertEqual(len(self.fixture.processes.entries), 4)
        self.assertTrue(self.fixture.processes.finished_naturally())
        require_content_identity(initial, self.immutable(), "both directions")

    def test_releases_writer_before_waiting_for_reader_that_needs_a_second_shared_call(self):
        self.fixture.write_options(reader_repeat=2)
        evidence = self.prove()
        self.assertTrue(evidence["active_work_refused"])
        self.assertTrue(self.fixture.processes.finished_naturally())

    def test_rejects_missing_reader_or_writer_lock_and_keeps_natural_process_receipts(self):
        for option in ("no_reader_lock", "no_writer_lock"):
            self.fixture.write_options(**{"no_reader_lock": False, "no_writer_lock": False, option: True})
            with self.subTest(option=option), self.assertRaisesRegex(RuntimeError, "lease"):
                self.prove(label=option)
            self.assertTrue(self.fixture.processes.finished_naturally())
            self.assertTrue(probe_generation_lease(self.fixture.supervisor / "bootstrap.lock", "unlocked"))

    def test_rejects_wrong_lock_path_and_task_content_drift(self):
        self.fixture.write_options(wrong_lock=True)
        with self.assertRaisesRegex(RuntimeError, "lease"):
            self.prove(label="wrong-path")
        self.fixture.write_options(wrong_lock=False, mutate_task=self.fixture.task_id)
        with self.assertRaisesRegex(RuntimeError, "immutable"):
            self.prove(label="content-drift")
        self.assertTrue(self.fixture.processes.finished_naturally())

    def test_post_start_receipt_failure_cannot_lose_an_owned_child(self):
        def publication(path, value, replace=False):
            if Path(path).name == "started.json":
                raise OSError("injected started receipt failure")
            return write_json(path, value, replace=replace)

        with (
            patch.object(process_owner, "write_json", side_effect=publication),
            self.assertRaisesRegex(OSError, "injected"),
        ):
            self.prove()
        self.assertEqual(len(self.fixture.processes.entries), 1)
        self.assertTrue(self.fixture.processes.finished_naturally())
        self.assertTrue((self.fixture.processes.entries[0].directory / "completed.json").is_file())

    def test_final_candidate_handoff_checks_actual_pair_content_and_immutable_records(self):
        self.fixture.write_options(active=False)
        task_ops = NativeTaskOps(
            self.fixture.processes,
            self.fixture.baseline.delegate,
            self.fixture.baseline.runner,
            self.fixture.baseline.libexec / "pueue",
            self.fixture.root.directory,
            self.fixture.state,
            self.fixture.controls.directory,
            managed_runner=True,
        )
        task_ops.root_id = self.fixture.root_id
        evidence = controls.prove_exclusive_handoff(
            task_ops, self.fixture.candidate, self.fixture.controls, self.immutable
        )
        self.assertEqual(evidence["response"]["supervisor_control"]["state"], "migrated")
        self.assertFalse(evidence["boundary"]["lease"]["probe_shared_acquired"])
        self.assertIn("pueue-pair-", evidence["candidate_pair"])
        self.assertTrue(evidence["final_lock"]["probe_exclusive_acquired"])
        self.assertEqual(evidence["immutable_content_before"], evidence["immutable_content_after"])

    def test_response_root_drift_is_rejected_after_both_controls_are_reaped(self):
        self.fixture.write_options(response_root="c" * 32)
        with self.assertRaisesRegex(RuntimeError, "root identity"):
            self.prove()
        self.assertTrue(self.fixture.processes.finished_naturally())
