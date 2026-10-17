"""Scenario orchestration with real control children and explicit task-IO fixtures.

Provider lifetime/queue facts are supplied fixtures here. These tests do not
establish native Delegate or provider application acceptance.
"""

import hashlib
import json
import unittest
from unittest.mock import MagicMock, patch

from scripts.acceptance_tests.private_cli.modern_fixtures import ModernControlFixture
from scripts.delegate_acceptance.private_cli import contention_controls as controls
from scripts.delegate_acceptance.private_cli.contention_locks import probe_generation_lease
from scripts.delegate_acceptance.private_cli.leases import LiveFIFOLeaseCohort
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps


class ModernContentionScenarioTests(unittest.TestCase):
    def setUp(self):
        self.fixture = ModernControlFixture(self)
        self.tasks = NativeTaskOps(
            self.fixture.processes,
            self.fixture.baseline.delegate,
            self.fixture.baseline.runner,
            self.fixture.baseline.libexec / "pueue",
            self.fixture.root.directory,
            self.fixture.state,
            self.fixture.controls.directory,
            expected_inspection_binding_required=False,
            managed_runner=True,
        )
        self.brief = self.fixture.root.directory / "brief.md"
        self.brief.write_text("finite provider fixture\n")
        self.input = controls.ModernFixture(
            self.fixture.controls,
            self.brief,
            self.fixture.root.directory,
            self.fixture.root.directory / "provider-gate",
            self.fixture.root.directory,
            "test-nonce",
        )
        self.cohort = MagicMock(spec=LiveFIFOLeaseCohort)
        self.closure = {
            "root_id": self.fixture.root_id,
            "tasks": {},
            "workspace": {"operation": {"lifetime": {"registration": {"scope": {"lease_records": [1, 2, 3]}}}}},
        }
        self.owner_proof = {"counted_task_count": 2, "counted_tasks": [{"nested": {"lease_records": [1, 2]}}]}

    def close_tasks(self, state, task_ids, timeout):
        self.assertEqual(state, self.fixture.state)
        self.assertEqual(set(task_ids), self.tasks.dispatch_attempts)
        self.assertEqual(probe_generation_lease(self.input.gate, "unlocked")["owner_mode"], "unlocked")
        for task_id in self.tasks.tasks.values():
            directory = state / "tasks" / task_id
            payload = (task_id + " finite committed result\n").encode()
            (directory / "result.txt").write_bytes(payload)
            outcome = {
                "verdict": "committed",
                "payload": {
                    "basename": "result.txt",
                    "length": len(payload),
                    "sha256": hashlib.sha256(payload).hexdigest(),
                },
            }
            (directory / "outcome.json").write_text(json.dumps(outcome))
            self.closure["tasks"][task_id] = {
                "worker_wait": {"registration": {"nested": [task_id]}},
                "termination": {"cause": "completed", "native_empty": True},
            }
        self.fixture.write_options(active=False)
        return self.closure

    def test_two_binary_protocol_retains_one_real_task_ledger_and_full_nested_evidence(self):
        ledger = self.tasks.tasks
        with (
            patch.object(controls, "acquire_modern_cohort", return_value=self.cohort),
            patch.object(controls, "capacity_lease_specifications", return_value=[]),
            patch.object(controls, "prove_lifetime_owner_cohort", return_value=self.owner_proof),
            patch.object(controls, "await_managed_lifetime_closure", side_effect=self.close_tasks) as closure,
            patch.object(self.tasks, "wait_task") as wait_task,
        ):
            result = controls.run_modern_binary_contention(
                self.tasks, self.fixture.baseline, self.fixture.candidate, self.input
            )
        self.assertIs(self.tasks.tasks, ledger)
        self.assertEqual(set(ledger), {"modern-baseline", "modern-candidate"})
        self.assertEqual(len(self.tasks.dispatch_attempts), 2)
        self.assertEqual(set(result["task_ids"]), set(ledger.values()))
        self.assertIs(result["lifetime_closure"], self.closure)
        self.assertIs(result["live_owner_proof"], self.owner_proof)
        self.assertEqual(result["task_content_before_handoff"], result["task_content_after_handoff"])
        self.assertEqual(len(result["contention_rounds"]), 2)
        self.assertEqual(result["handoff"]["response"]["supervisor_control"]["state"], "migrated")
        self.assertEqual(wait_task.call_count, 2)
        closure.assert_called_once()
        self.cohort.remove_closed_fifos.assert_called_once()
        self.cohort.close.assert_called_once()
        self.assertTrue(self.fixture.processes.finished_naturally())
        self.assertEqual(len(result["source"]), 4)
        self.assertNotEqual(
            result["installations"]["baseline"]["delegate"]["sha256"],
            result["installations"]["candidate"]["delegate"]["sha256"],
        )

    def test_failed_dispatch_preserves_primary_error_and_reports_unproven_cleanup(self):
        self.fixture.write_options(fail_dispatch_role="candidate")
        with patch.object(
            controls, "await_managed_lifetime_closure", side_effect=RuntimeError("unproven closure")
        ) as closure:
            with self.assertRaisesRegex(RuntimeError, "cleanup failed") as raised:
                controls.run_modern_binary_contention(
                    self.tasks, self.fixture.baseline, self.fixture.candidate, self.input
                )
        self.assertIsNotNone(raised.exception.__cause__)
        self.assertIn("not admitted", str(raised.exception.__cause__))
        self.assertEqual(len(self.tasks.dispatch_attempts), 2)
        self.assertEqual(len(self.tasks.tasks), 1)
        self.assertEqual(set(closure.call_args.args[1]), self.tasks.dispatch_attempts)
        self.assertTrue(self.fixture.processes.finished_naturally())
        self.assertTrue(probe_generation_lease(self.input.gate, "unlocked")["probe_exclusive_acquired"])

    def test_unproven_closure_never_removes_fifo_but_closes_the_handle_once(self):
        release = MagicMock()
        with patch.object(controls, "await_managed_lifetime_closure", side_effect=RuntimeError("still live")):
            with self.assertRaisesRegex(RuntimeError, "still live"):
                controls.close_modern_cohort(self.tasks, self.input, release, self.cohort)
        release.release.assert_called_once()
        self.cohort.remove_closed_fifos.assert_not_called()
        self.cohort.close.assert_called_once()

    def test_candidate_admission_stages_inventory_without_replacing_active_generation(self):
        supervisor = self.fixture.state / ".supervisor"
        daemon_before = (supervisor / "daemon.identity.json").read_bytes()
        binding_before = (supervisor / "supervisor.identity.json").read_bytes()
        responses = controls.dispatch_modern_pair(
            self.tasks, self.fixture.baseline, self.fixture.candidate, self.input, self.brief,
        )
        self.assertEqual(len(responses), 2)
        self.assertEqual((supervisor / "daemon.identity.json").read_bytes(), daemon_before)
        self.assertEqual((supervisor / "supervisor.identity.json").read_bytes(), binding_before)
        controls.assert_state_supervisor_pair(self.fixture.state, self.fixture.candidate.libexec)

    def test_warm_admission_rejects_a_replaced_active_generation(self):
        original = controls.dispatch_modern_task

        def dispatch(tasks, installation, fixture, name, brief):
            response = original(tasks, installation, fixture, name, brief)
            if installation is self.fixture.candidate:
                (tasks.state / ".supervisor" / "supervisor.identity.json").write_text("replaced")
            return response

        with patch.object(controls, "dispatch_modern_task", side_effect=dispatch):
            with self.assertRaisesRegex(RuntimeError, "modern active generation"):
                controls.dispatch_modern_pair(
                    self.tasks, self.fixture.baseline, self.fixture.candidate, self.input, self.brief,
                )

    def test_rejects_same_binary_and_persisted_tmpdir_before_fixture_mutation(self):
        with self.assertRaisesRegex(RuntimeError, "two CLI binaries"):
            controls.run_modern_binary_contention(self.tasks, self.fixture.baseline, self.fixture.baseline, self.input)
        self.fixture.processes.environment["TMPDIR"] = str(self.fixture.root.directory)
        with self.assertRaisesRegex(RuntimeError, "omit TMPDIR"):
            controls.run_modern_binary_contention(self.tasks, self.fixture.baseline, self.fixture.candidate, self.input)
        self.assertFalse(self.input.gate.exists())
        self.assertFalse(self.brief.with_name("modern-contention-waiting.md").exists())
        self.assertFalse(self.tasks.dispatch_attempts)
