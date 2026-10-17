"""Implementation methods for EndedTaskObservationTests; collected once by the acceptance_tests discovery."""

from pathlib import Path
import unittest
from unittest.mock import patch
from scripts import acceptance_private_cli as private_cli
from scripts.delegate_acceptance.private_cli import admission as admission_impl
from scripts.acceptance_tests.private_cli.fixtures import (
    set_up_private_capture_root,
)


class EndedTaskObservationTests(unittest.TestCase):
    def setUp(self):
        super().setUp()
        set_up_private_capture_root(self)

    def response(self):
        return {"task_id": "a" * 32, "root_id": "r" * 32}

    def test_running_status_is_polled_until_same_task_ends_with_unique_receipts(self):
        response = self.response()
        observations = [
            {**response, "liveness": "running", "stops": []},
            {**response, "liveness": "ended", "stops": []},
        ]
        with patch.object(admission_impl, "read_created_task_status",
                          side_effect=observations) as read, \
                patch.object(private_cli.time, "sleep") as sleep:
            status, evidence = private_cli.await_created_task_ended(
                Path("/fixture/delegate"), Path("/fixture/state"), {}, response,
                Path("/fixture/status.json"), timeout=5, capture_root=self.capture_root,
            )

        self.assertEqual(status["liveness"], "ended")
        self.assertEqual(evidence, {
            "kind": "queue-ended-observation",
            "final_receipt": "status-001.json",
            "poll_receipts": ["status-000.json", "status-001.json"],
            "poll_count": 2,
        })
        self.assertEqual(
            [call.args[3] for call in read.call_args_list],
            [response["task_id"], response["task_id"]],
        )
        self.assertEqual(
            [call.args[4].name for call in read.call_args_list],
            ["status-000.json", "status-001.json"],
        )
        self.assertTrue(all(call.kwargs["timeout"] <= 5 for call in read.call_args_list))
        sleep.assert_called_once()

    def test_running_status_times_out_with_fake_clock(self):
        response = self.response()
        now = [0.0]

        def monotonic():
            return now[0]

        def advance_to_deadline(_duration):
            now[0] = 5.0

        with patch.object(admission_impl, "read_created_task_status",
                          return_value={**response, "liveness": "running", "stops": []}) as read, \
                patch.object(private_cli.time, "monotonic", side_effect=monotonic), \
                patch.object(private_cli.time, "sleep", side_effect=advance_to_deadline) as sleep:
            with self.assertRaisesRegex(TimeoutError, "queue status remained non-ended"):
                private_cli.await_created_task_ended(
                    Path("/fixture/delegate"), Path("/fixture/state"), {}, response,
                    Path("/fixture/status.json"), timeout=5, capture_root=self.capture_root,
                )

        read.assert_called_once()
        sleep.assert_called_once()

    def test_identity_and_operational_errors_fail_without_another_poll(self):
        response = self.response()
        wrong_task = {
            "task_id": "b" * 32, "root_id": response["root_id"], "liveness": "running",
        }
        with patch.object(admission_impl, "run_json", return_value=wrong_task) as run, \
                patch.object(private_cli.time, "sleep") as sleep:
            with self.assertRaisesRegex(RuntimeError, "different task"):
                private_cli.await_created_task_ended(
                    Path("/fixture/delegate"), Path("/fixture/state"), {}, response,
                    Path("/fixture/status.json"), timeout=5, capture_root=self.capture_root,
                )
        run.assert_called_once()
        sleep.assert_not_called()

        cases = (
            (
                "root identity",
                {"task_id": response["task_id"], "root_id": "s" * 32, "liveness": "running"},
                "different root",
            ),
            (
                "status error",
                {**response, "liveness": "running", "error": "status unavailable"},
                "operational failure",
            ),
            (
                "status failure",
                {**response, "liveness": "running",
                 "failure": {"code": "supervisor_unavailable"}},
                "operational failure",
            ),
        )
        for label, observed, message in cases:
            with self.subTest(label=label), \
                    patch.object(admission_impl, "read_created_task_status",
                                 return_value=observed) as read, \
                    patch.object(private_cli.time, "sleep") as sleep:
                with self.assertRaisesRegex(RuntimeError, message):
                    private_cli.await_created_task_ended(
                        Path("/fixture/delegate"), Path("/fixture/state"), {}, response,
                        Path("/fixture/status.json"), timeout=5, capture_root=self.capture_root,
                    )
                read.assert_called_once()
                sleep.assert_not_called()

        with patch.object(admission_impl, "read_created_task_status",
                          side_effect=RuntimeError("delegate command failed: exit=2")) as read, \
                patch.object(private_cli.time, "sleep") as sleep:
            with self.assertRaisesRegex(RuntimeError, "exit=2"):
                private_cli.await_created_task_ended(
                    Path("/fixture/delegate"), Path("/fixture/state"), {}, response,
                    Path("/fixture/status.json"), timeout=5, capture_root=self.capture_root,
                )
        read.assert_called_once()
        sleep.assert_not_called()
