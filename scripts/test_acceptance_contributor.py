"""Hermetic checks that the contributor gate cannot hide runner failures."""
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from acceptance_contributor import ContributorAcceptance


ROOT_ID = "a" * 32
TASK_ID = "b" * 32
LABEL = "delegate:" + ROOT_ID + ":" + TASK_ID


def gate_with_statuses(statuses):
    gate = object.__new__(ContributorAcceptance)

    def client(_name, _operation):
        status = statuses.pop(0)
        return SimpleNamespace(json=lambda: {"tasks": {"0": {"label": LABEL, "status": status}}})

    gate.client = client
    return gate


class ContributorHarnessTests(unittest.TestCase):
    def test_published_outcome_cannot_mask_failed_runner(self):
        for result in ({"Failed": 1}, "Killed", None):
            with self.subTest(result=result):
                gate = gate_with_statuses([{"Done": {"result": result}}])
                with self.assertRaisesRegex(RuntimeError, "failed runner"):
                    gate.wait_runner_done(ROOT_ID, TASK_ID)

    def test_waits_for_successful_runner_completion(self):
        statuses = ["Running", {"Done": {"result": "Success"}}]
        gate = gate_with_statuses(statuses)
        with patch("acceptance_contributor.time.sleep") as sleep:
            gate.wait_runner_done(ROOT_ID, TASK_ID)
        self.assertEqual(statuses, [])
        sleep.assert_called_once()

    def test_rejects_an_unrelated_supervisor_row(self):
        gate = gate_with_statuses([{"Done": {"result": "Success"}}])
        with self.assertRaisesRegex(RuntimeError, "no unique supervisor row"):
            gate.wait_runner_done("c" * 32, TASK_ID)

    def test_completion_wait_has_a_finite_deadline(self):
        gate = gate_with_statuses([])
        with patch("acceptance_contributor.time.monotonic", side_effect=[0, 151]):
            with self.assertRaisesRegex(RuntimeError, "within acceptance deadline"):
                gate.wait_runner_done(ROOT_ID, TASK_ID)


if __name__ == "__main__":
    unittest.main()
