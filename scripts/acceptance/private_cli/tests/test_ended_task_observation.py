"""Implementation methods for EndedTaskObservationTests; collected once by the acceptance package discovery."""

from pathlib import Path
import unittest
from unittest.mock import patch
import sys
import time


repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)


from scripts.acceptance.private_cli import admission as admission_impl
from scripts.acceptance.private_cli.tests.fixtures import set_up_private_capture_root


USER_STOP_REQUEST = "ad2fb080d387694ece599c62defded6e"


def interrupted_user_stop_status(response):
    """Return the exact shape `status --json` reported after a native default user stop."""
    return {
        "schema_version": 1, "command": "status", **response,
        "status": "interrupted", "admission": "admitted", "liveness": "ended",
        "publication": "unknown",
        "supervisor": {"matched": True, "state": "ended", "numeric_task_id": 0},
        "stops": [{
            "request_id": USER_STOP_REQUEST, "cause": "user", "requested": True,
            "matched": False, "attempted": False, "numeric_task_id": 0,
            "in_flight": False, "observed_state": "ended", "terminated": True,
        }],
        "continuation": {
            "resumable": False, "mode": "native",
            "predecessor_task_id": response["task_id"],
            "reason": "task was cancelled by the user",
        },
        "failure": {"code": "task_interrupted", "stage": "status", "next_action": "stop_and_report"},
    }


def pending_user_stop_status(response):
    """Return the exact `status --json` shape a macOS CI run reported before the queue ended.

    Canonical closure is already proven, so the projection is interrupted, but
    the exact user stop is not yet terminated and the queue is still running.
    """
    status = interrupted_user_stop_status(response)
    return {
        **status, "liveness": "running",
        "supervisor": {"matched": True, "state": "running", "numeric_task_id": 0},
        "stops": [{**status["stops"][0], "observed_state": "", "terminated": False}],
    }


class EndedTaskObservationTests(unittest.TestCase):
    def setUp(self):
        super().setUp()
        self.capture_root = set_up_private_capture_root(self)

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
                patch.object(time, "sleep") as sleep:
            status, evidence = admission_impl.await_created_task_ended(
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
                patch.object(time, "monotonic", side_effect=monotonic), \
                patch.object(time, "sleep", side_effect=advance_to_deadline) as sleep:
            with self.assertRaisesRegex(TimeoutError, "queue status remained non-ended"):
                admission_impl.await_created_task_ended(
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
                patch.object(time, "sleep") as sleep:
            with self.assertRaisesRegex(RuntimeError, "different task"):
                admission_impl.await_created_task_ended(
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
                    patch.object(time, "sleep") as sleep:
                with self.assertRaisesRegex(RuntimeError, message):
                    admission_impl.await_created_task_ended(
                        Path("/fixture/delegate"), Path("/fixture/state"), {}, response,
                        Path("/fixture/status.json"), timeout=5, capture_root=self.capture_root,
                    )
                read.assert_called_once()
                sleep.assert_not_called()

        with patch.object(admission_impl, "read_created_task_status",
                          side_effect=RuntimeError("delegate command failed: exit=2")) as read, \
                patch.object(time, "sleep") as sleep:
            with self.assertRaisesRegex(RuntimeError, "exit=2"):
                admission_impl.await_created_task_ended(
                    Path("/fixture/delegate"), Path("/fixture/state"), {}, response,
                    Path("/fixture/status.json"), timeout=5, capture_root=self.capture_root,
                )
        read.assert_called_once()
        sleep.assert_not_called()

    def await_ended(self, response, **options):
        return admission_impl.await_created_task_ended(
            Path("/fixture/delegate"), Path("/fixture/state"), {}, response,
            Path("/fixture/status.json"), timeout=5, capture_root=self.capture_root, **options,
        )

    def test_documented_user_interruption_ends_observation_only_when_expected(self):
        response = self.response()
        status = interrupted_user_stop_status(response)
        pending = {**status, "pending": {"kind": "supervisor-command"}}
        for label, observed in (("documented envelope", status), ("supervisor command pending", pending)):
            with self.subTest(label=label), \
                    patch.object(admission_impl, "read_created_task_status", return_value=observed) as read, \
                    patch.object(time, "sleep") as sleep:
                returned, evidence = self.await_ended(response, expected_user_stop=USER_STOP_REQUEST)
                self.assertIs(returned, observed)
                self.assertEqual(evidence, {
                    "kind": "queue-ended-observation",
                    "final_receipt": "status-000.json",
                    "poll_receipts": ["status-000.json"],
                    "poll_count": 1,
                })
                read.assert_called_once()
                sleep.assert_not_called()

        # The same observed state stays an operational failure for every caller
        # that did not request this exact user stop, including the budget proof.
        with patch.object(admission_impl, "read_created_task_status", return_value=status) as read, \
                patch.object(time, "sleep") as sleep:
            with self.assertRaisesRegex(RuntimeError, "operational failure"):
                self.await_ended(response)
        read.assert_called_once()
        sleep.assert_not_called()

        ended = {**response, "liveness": "ended", "stops": []}
        with patch.object(admission_impl, "read_created_task_status", return_value=ended):
            returned, evidence = self.await_ended(response, expected_user_stop=USER_STOP_REQUEST)
        self.assertIs(returned, ended)
        self.assertEqual(evidence["poll_count"], 1)

    def test_user_interruption_mismatches_fail_without_another_poll(self):
        response = self.response()
        status = interrupted_user_stop_status(response)
        row = status["stops"][0]
        continuation = status["continuation"]
        failure = status["failure"]
        cases = (
            ("other request", {"stops": [{**row, "request_id": "f" * 32}]}),
            ("budget cause", {"stops": [{**row, "cause": "budget"}]}),
            ("not terminated", {"stops": [{**row, "terminated": False}]}),
            ("not requested", {"stops": [{**row, "requested": False}]}),
            ("resumable", {"continuation": {**continuation, "resumable": True}}),
            ("continue command", {"continuation": {**continuation, "continue_command": "delegate continue"}}),
            ("other predecessor", {"continuation": {**continuation, "predecessor_task_id": "b" * 32}}),
            ("no continuation", {"continuation": None}),
            ("budget raced", {"status": "timed_out", "failure": {**failure, "code": "task_cancelled"}}),
            ("timed out status", {"status": "timed_out"}),
            ("cancelled code", {"failure": {**failure, "code": "task_cancelled"}}),
            ("continue guidance", {"failure": {**failure, "next_action": "continue_task"}}),
            ("other stage", {"failure": {**failure, "stage": "collect"}}),
            ("committed publication", {"publication": "committed"}),
            ("outcome", {"outcome": {"state": "committed"}}),
            ("pending publication", {"pending": {"kind": "publication"}}),
            ("operational error", {"error": "supervisor unavailable"}),
            ("running", {"liveness": "running"}),
        )
        for label, changes in cases:
            observed = {**status, **changes}
            with self.subTest(label=label), \
                    patch.object(admission_impl, "read_created_task_status", return_value=observed) as read, \
                    patch.object(time, "sleep") as sleep:
                with self.assertRaisesRegex(RuntimeError, "operational failure"):
                    self.await_ended(response, expected_user_stop=USER_STOP_REQUEST)
                read.assert_called_once()
                sleep.assert_not_called()

        identities = (
            ("other root", "read_created_task_status", {**status, "root_id": "s" * 32}, "different root"),
            ("other task", "run_json", {**status, "task_id": "b" * 32}, "different task"),
        )
        for label, target, observed, message in identities:
            with self.subTest(label=label), \
                    patch.object(admission_impl, target, return_value=observed) as read, \
                    patch.object(time, "sleep") as sleep:
                with self.assertRaisesRegex(RuntimeError, message):
                    self.await_ended(response, expected_user_stop=USER_STOP_REQUEST)
                read.assert_called_once()
                sleep.assert_not_called()

        with patch.object(admission_impl, "read_created_task_status",
                          side_effect=RuntimeError("delegate command failed: exit=1")) as read, \
                patch.object(time, "sleep") as sleep:
            with self.assertRaisesRegex(RuntimeError, "exit=1"):
                self.await_ended(response, expected_user_stop=USER_STOP_REQUEST)
        read.assert_called_once()
        sleep.assert_not_called()

    def test_invalid_user_stop_expectation_is_rejected_before_observation(self):
        for expected in ("", 7, b"request"):
            with self.subTest(expected=expected), \
                    patch.object(admission_impl, "read_created_task_status") as read:
                with self.assertRaisesRegex(ValueError, "expected user stop request"):
                    self.await_ended(self.response(), expected_user_stop=expected)
                read.assert_not_called()

    def test_pending_user_interruption_is_polled_until_its_documented_end(self):
        response = self.response()
        pending = pending_user_stop_status(response)
        ended = interrupted_user_stop_status(response)
        with patch.object(admission_impl, "read_created_task_status", side_effect=[pending, ended]) as read, \
                patch.object(time, "sleep") as sleep:
            returned, evidence = self.await_ended(response, expected_user_stop=USER_STOP_REQUEST)
        self.assertIs(returned, ended)
        self.assertEqual(evidence, {
            "kind": "queue-ended-observation",
            "final_receipt": "status-001.json",
            "poll_receipts": ["status-000.json", "status-001.json"],
            "poll_count": 2,
        })
        self.assertEqual(
            [call.args[4].name for call in read.call_args_list],
            ["status-000.json", "status-001.json"],
        )
        sleep.assert_called_once()

        # A caller that did not request this exact user stop, including the
        # budget proof, still treats the pending interruption as a failure.
        with patch.object(admission_impl, "read_created_task_status", return_value=pending) as read, \
                patch.object(time, "sleep") as sleep:
            with self.assertRaisesRegex(RuntimeError, "operational failure"):
                self.await_ended(response)
        read.assert_called_once()
        sleep.assert_not_called()

    def test_persistent_pending_user_interruption_times_out_with_fake_clock(self):
        response = self.response()
        now = [0.0]

        def monotonic():
            return now[0]

        def advance_to_deadline(_duration):
            now[0] = 5.0

        with patch.object(admission_impl, "read_created_task_status",
                          return_value=pending_user_stop_status(response)) as read, \
                patch.object(time, "monotonic", side_effect=monotonic), \
                patch.object(time, "sleep", side_effect=advance_to_deadline) as sleep:
            with self.assertRaisesRegex(TimeoutError, "queue status remained non-ended"):
                self.await_ended(response, expected_user_stop=USER_STOP_REQUEST)
        read.assert_called_once()
        sleep.assert_called_once()

    def test_pending_user_interruption_fails_unless_the_queue_ends_as_documented(self):
        response = self.response()
        pending = pending_user_stop_status(response)
        ended = interrupted_user_stop_status(response)
        failure = ended["failure"]
        cases = (
            ("ended not terminated", {"stops": [{**ended["stops"][0], "terminated": False}]}),
            ("budget raced", {"status": "timed_out", "failure": {**failure, "code": "task_cancelled"}}),
            ("outcome", {"outcome": {"state": "committed"}}),
            ("pending publication", {"pending": {"kind": "publication"}}),
        )
        for label, changes in cases:
            with self.subTest(label=label), \
                    patch.object(admission_impl, "read_created_task_status",
                                 side_effect=[pending, {**ended, **changes}]) as read, \
                    patch.object(time, "sleep") as sleep:
                with self.assertRaisesRegex(RuntimeError, "operational failure"):
                    self.await_ended(response, expected_user_stop=USER_STOP_REQUEST)
                self.assertEqual(read.call_count, 2)
                sleep.assert_called_once()

    def test_user_interruption_binding_faults_fail_pending_and_ended_without_another_poll(self):
        response = self.response()
        for state, status in (
            ("pending", pending_user_stop_status(response)),
            ("ended", interrupted_user_stop_status(response)),
        ):
            row = status["stops"][0]
            continuation = status["continuation"]
            budget_row = {
                "request_id": "budget", "cause": "budget", "requested": True,
                "observed_state": row["observed_state"], "terminated": row["terminated"],
            }
            cases = (
                ("checkpoint mode", {"continuation": {**continuation, "mode": "checkpoint"}}),
                ("no mode", {"continuation": {k: v for k, v in continuation.items() if k != "mode"}}),
                ("additional budget row", {"stops": [row, budget_row]}),
                ("duplicate exact row", {"stops": [row, dict(row)]}),
                ("malformed stop row", {"stops": [row, "stop"]}),
                ("malformed stops", {"stops": {"0": row}}),
                ("other predecessor", {"continuation": {**continuation, "predecessor_task_id": "b" * 32}}),
                ("operational error", {"error": "supervisor unavailable"}),
            )
            for label, changes in cases:
                with self.subTest(state=state, label=label), \
                        patch.object(admission_impl, "read_created_task_status",
                                     return_value={**status, **changes}) as read, \
                        patch.object(time, "sleep") as sleep:
                    with self.assertRaisesRegex(RuntimeError, "operational failure"):
                        self.await_ended(response, expected_user_stop=USER_STOP_REQUEST)
                    read.assert_called_once()
                    sleep.assert_not_called()

            identities = (
                ("other root", "read_created_task_status", {**status, "root_id": "s" * 32}, "different root"),
                ("other task", "run_json", {**status, "task_id": "b" * 32}, "different task"),
            )
            for label, target, observed, message in identities:
                with self.subTest(state=state, label=label), \
                        patch.object(admission_impl, target, return_value=observed) as read, \
                        patch.object(time, "sleep") as sleep:
                    with self.assertRaisesRegex(RuntimeError, message):
                        self.await_ended(response, expected_user_stop=USER_STOP_REQUEST)
                    read.assert_called_once()
                    sleep.assert_not_called()
