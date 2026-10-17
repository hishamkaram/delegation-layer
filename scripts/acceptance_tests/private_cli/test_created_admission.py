"""Implementation methods for CreatedAdmissionTests; collected once by the acceptance_tests discovery."""

import hashlib
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
from scripts import acceptance_private_cli as private_cli
from scripts.delegate_acceptance.private_cli import admission as admission_impl
from scripts.delegate_acceptance.private_cli import commands as commands_impl
from scripts.acceptance_tests.private_cli.fixtures import (
    set_up_private_capture_root,
)


class CreatedAdmissionTests(unittest.TestCase):
    def setUp(self):
        super().setUp()
        set_up_private_capture_root(self)

    def response(self):
        return {"task_id": "a" * 32, "task_record": "created", "admission": "unknown",
                "failure": {"code": "task_submission_uncertain", "next_action": "check_status"}}

    def observe(self, response):
        return private_cli.observe_created_admission(
            Path("/fixture/delegate"), Path("/fixture/state"), {}, response,
            Path("/fixture/dispatch-000.json"), capture_root=self.capture_root,
        )

    def test_admitted_submission_uses_dispatch_evidence_without_status(self):
        response = {"task_id": "a" * 32, "task_record": "created", "admission": "admitted"}
        with patch.object(admission_impl, "run_json") as run:
            observed = self.observe(response)
        self.assertIs(observed, response)
        run.assert_not_called()

    def test_admitted_operational_failure_observes_the_same_task(self):
        task_id = "a" * 32
        response = {
            "task_id": task_id,
            "task_record": "created",
            "admission": "admitted",
            "error": "dispatch operation failed",
            "failure": {"code": "task_operation_failed", "next_action": "check_status"},
        }
        observed = {"task_id": task_id, "admission": "admitted"}
        with patch.object(admission_impl, "run_json", return_value=observed) as run:
            result = self.observe(response)
        self.assertIs(result, observed)
        run.assert_called_once()
        self.assertEqual(
            run.call_args.args[2],
            ["--root", "/fixture/state", "status", task_id, "--json"],
        )

    def test_enqueue_uncertainty_observes_exact_stashed_task_without_redispatch(self):
        root_id = "b" * 32
        task_id = "c" * 32
        numeric_task_id = 37
        dispatch = {
            "root_id": root_id,
            "task_id": task_id,
            "task_record": "created",
            "admission": "admitted",
            "supervisor": {
                "matched": True,
                "state": "stashed",
                "numeric_task_id": numeric_task_id,
            },
            "pending": {"kind": "supervisor-command"},
            "failure": {
                "code": "task_launch_uncertain",
                "stage": "enqueue-task",
                "next_action": "check_status",
            },
            "error": "Delegate recorded the exact supervisor task but could not confirm that it was enqueued.",
        }
        status = {
            "root_id": root_id,
            "task_id": task_id,
            "admission": "admitted",
            "liveness": "running",
            "supervisor": {
                "matched": True,
                "state": "running",
                "numeric_task_id": numeric_task_id,
            },
        }

        def scripted_control(_delegate, _env, args, **_options):
            self.assertEqual(args[2], "status", "uncertain dispatch must not be redispatched")
            self.assertEqual(args[3], task_id)
            return status

        with patch.object(admission_impl, "run_json", side_effect=scripted_control) as run:
            observed = self.observe(dispatch)

        self.assertEqual(run.call_count, 1)
        self.assertIs(observed, status)
        self.assertEqual(observed["root_id"], dispatch["root_id"])
        self.assertEqual(observed["task_id"], dispatch["task_id"])
        self.assertEqual(observed["supervisor"]["numeric_task_id"], numeric_task_id)
        self.assertEqual(dispatch["failure"]["code"], "task_launch_uncertain")
        self.assertEqual(dispatch["admission"], "admitted")

    def test_uncertain_submission_only_observes_its_created_task(self):
        response = self.response()
        task_id = response["task_id"]
        observations = [{"task_id": task_id, "admission": "unknown", "error": "pueue observation is unknown"},
                        {"task_id": task_id, "admission": "admitted"}]
        with patch.object(admission_impl, "run_json", side_effect=observations) as run, \
                patch.object(private_cli.time, "sleep"):
            self.observe(response)
        self.assertEqual(run.call_count, 2)
        self.assertEqual(response["admission"], "unknown", "original dispatch evidence was rewritten")
        self.assertEqual(len({call.kwargs["receipt"] for call in run.call_args_list}), 2)
        for call in run.call_args_list:
            self.assertEqual(call.args[2], ["--root", "/fixture/state", "status", task_id, "--json"])

    def test_not_created_or_blocked_dispatch_is_not_observed(self):
        for change in ({"task_record": "not_created"},
                       {"failure": {"code": "supervisor_setup_failed", "next_action": "stop_and_report"}}):
            with self.subTest(change=change), patch.object(admission_impl, "run_json") as run:
                response = self.response()
                response.update(change)
                with self.assertRaises(RuntimeError):
                    self.observe(response)
                run.assert_not_called()

    def test_status_blocker_and_different_task_end_observation(self):
        for observed in ({"task_id": "a" * 32, "failure": {"next_action": "stop_and_report"}},
                         {"task_id": "b" * 32, "admission": "admitted"}):
            with self.subTest(observed=observed), patch.object(admission_impl, "run_json", return_value=observed) as run:
                with self.assertRaises(RuntimeError):
                    self.observe(self.response())
                run.assert_called_once()

    def test_pending_observation_has_finite_deadline(self):
        pending = {"task_id": "a" * 32, "admission": "unknown", "pending": {"kind": "supervisor-command"}}
        with patch.object(admission_impl, "run_json", return_value=pending) as run, \
                patch.object(private_cli.time, "monotonic", side_effect=[0, 0, 61, 61]), \
                patch.object(private_cli.time, "sleep"):
            with self.assertRaises(TimeoutError):
                self.observe(self.response())
        run.assert_called_once()

    def test_nonzero_status_cannot_confirm_admission(self):
        observed = {"task_id": "a" * 32, "admission": "admitted", "error": "receipt write failed"}
        captured = subprocess.CompletedProcess([], 1, json.dumps(observed).encode(), b"")
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(commands_impl, "run_captured_command", return_value=captured) as run:
            receipt = Path(directory) / "dispatch-000.json"
            with self.assertRaisesRegex(RuntimeError, "exit=1"):
                private_cli.observe_created_admission(Path("/fixture/delegate"), Path("/fixture/state"), {},
                                                     self.response(), receipt,
                                                     capture_root=self.capture_root)
            saved = json.loads((receipt.parent / "observation-dispatch-000-000.json").read_text())
            self.assertEqual(saved["exit_code"], 1)
            raw_stdout = json.dumps(observed).encode()
            self.assertEqual(saved["stdout_bytes"], len(raw_stdout))
            self.assertEqual(saved["stdout_sha256"], hashlib.sha256(raw_stdout).hexdigest())
            self.assertNotIn("stdout", saved)
        run.assert_called_once()
