"""Hermetic inspection queue and journal checks for the native acceptance harness."""

from __future__ import annotations

from pathlib import Path
import shutil
import unittest
from unittest.mock import Mock, patch
import sys


repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.evidence import files as evidence_files
from scripts.acceptance.supervisor import queue_policy as supervisor_queue_policy


from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.evidence.files import read_json
from scripts.acceptance.evidence.files import write_json


ROOT = "a" * 32
TASK = "b" * 32


from scripts.acceptance.tasks import operations as task_operations
from scripts.acceptance.testing.record_fixtures import object_at
from scripts.acceptance.inspections.tests.fixtures import InspectionFixture
from scripts.acceptance.inspections.tests.fixtures import CapturedCommand
from scripts.acceptance.inspections.tests.fixtures import FixtureProcesses
from scripts.acceptance.inspections.tests.fixtures import ObservedDaemon


class InspectionHarnessTests(unittest.TestCase):
    def setUp(self):
        self.fixture = InspectionFixture(self)

    def test_dispatch_rejection_code_keeps_positive_admission_for_cleanup(self):
        directory = self.fixture.base / "dispatch-rejected"
        directory.mkdir(mode=0o700)
        response = self.fixture.admitted_response()
        write_json(directory / "stdout", response)
        process = CapturedCommand(directory=directory,
                                  result={"exit_code": 1, "natural_wait": True})
        self.fixture.ops.tasks = {}
        self.fixture.ops.labels = {}
        self.fixture.ops.numbers = {}
        self.fixture.ops.dispatch_attempts = set()
        self.fixture.ops.processes = FixtureProcesses(run=lambda *args, **kwargs: process)

        with self.assertRaisesRegex(AcceptanceFailure, "did not exit successfully"):
            self.fixture.ops.dispatch("rejected", TASK, ["delegate"])

        self.assertEqual(self.fixture.ops.tasks, {"rejected": TASK})
        self.assertEqual(self.fixture.ops.numbers, {"rejected": 7})
        self.assertTrue(self.fixture.ops.dispatch_observations[0]["receipt_written"])

    def test_dispatch_receipt_failure_retains_positive_admission(self):
        directory = self.fixture.base / "dispatch-receipt-failure"
        directory.mkdir(mode=0o700)
        response = self.fixture.admitted_response()
        write_json(directory / "stdout", response)
        process = CapturedCommand(directory=directory,
                                  result={"exit_code": 0, "natural_wait": True})
        self.fixture.ops.tasks = {}
        self.fixture.ops.labels = {}
        self.fixture.ops.numbers = {}
        self.fixture.ops.dispatch_attempts = set()
        self.fixture.ops.processes = FixtureProcesses(run=lambda *args, **kwargs: process)

        def fail_attempt_receipt(path, value, replace=False):
            if Path(path).name == "receipt-failure-dispatch-attempt.json":
                raise OSError("injected dispatch-attempt receipt failure")
            return evidence_files.write_json(path, value, replace)

        with patch.object(task_operations, "write_json", side_effect=fail_attempt_receipt):
            with self.assertRaisesRegex(OSError, "dispatch-attempt receipt failure"):
                self.fixture.ops.dispatch("receipt-failure", TASK, ["delegate"])

        self.assertEqual(self.fixture.ops.tasks, {"receipt-failure": TASK})
        self.assertFalse(self.fixture.ops.dispatch_observations[0]["receipt_written"])
        self.assertTrue((self.fixture.output / "receipt-failure-dispatch.json").is_file())

    def test_dispatch_records_static_refusal_before_admission_validation(self):
        directory = self.fixture.base / "dispatch"
        directory.mkdir(mode=0o700)
        response = {
            "schema_version": 1, "command": "dispatch", "root_id": ROOT,
            "task_id": TASK, "admission": "unknown", "liveness": "undetermined",
            "publication": "unknown",
            "error": "unsupported-effective-config: static native profile refusal",
        }
        write_json(directory / "stdout", response)
        process = CapturedCommand(directory=directory,
                                  result={"exit_code": 2, "natural_wait": True})
        calls = []
        self.fixture.ops.tasks = {}
        self.fixture.ops.labels = {}
        self.fixture.ops.dispatch_attempts = set()
        self.fixture.ops.inspection_attempts = {TASK}
        self.fixture.ops.processes = FixtureProcesses(
            run=lambda *args, **kwargs: calls.append((args, kwargs)) or process)

        with self.assertRaisesRegex(AcceptanceFailure, "was not admitted"):
            self.fixture.ops.dispatch("static-refusal", TASK, ["delegate"])

        self.assertEqual(calls[0][1]["expected"], None)
        self.assertEqual(len(self.fixture.ops.dispatch_observations), 1)
        observation = self.fixture.ops.dispatch_observations[0]
        self.assertEqual(observation["exit_code"], 2)
        self.assertTrue(observation["receipt_written"])
        self.assertEqual(read_json(self.fixture.output / "static-refusal-dispatch-attempt.json"), {
            **observation, "receipt_written": True,
        })

    def test_dispatch_rejects_duplicate_before_starting_process(self):
        process = Mock()
        self.fixture.ops.processes = Mock(run=process)

        with self.assertRaisesRegex(AcceptanceFailure, "duplicate dispatch attempt"):
            self.fixture.ops.dispatch("duplicate", TASK, ["delegate"])

        process.assert_not_called()

    def test_static_refusal_allows_only_exact_empty_private_queue(self):
        self.fixture.static_refusal()
        self.assertTrue(self.fixture.ops.failure_queue_finished({"tasks": {}, "groups": {}}))
        self.assertTrue(self.fixture.ops.failure_queue_finished({
            "tasks": {}, "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
        }))

    def test_preadmission_retained_cleanup_accepts_only_empty_or_implicit_default(self):
        (self.fixture.state / "root.json").unlink()
        for initialized in (False, True):
            if initialized:
                write_json(self.fixture.state / "root.json", {
                    "schema_version": 1, "root_id": ROOT, "created_at": "2026-09-15T00:00:00Z",
                })
                (self.fixture.state / ".maintenance.lock").write_bytes(b"")
            for groups in ({}, {"default": {"status": "Running", "parallel_tasks": 1}}):
                with self.subTest(initialized=initialized, groups=groups):
                    ops = self.fixture.preadmission_cleanup({"tasks": {}, "groups": groups})
                    with patch.object(task_operations.time, "sleep",
                               side_effect=AssertionError("empty owned queue must close")):
                        self.assertTrue(ops.retain_failure_ownership())
                    self.assertTrue(ops.closed)
                    self.assertEqual([call.args[:2] for call in ops.client.call_args_list], [
                        ("failure-queue", ["status", "--json"]),
                        ("failure-shutdown", ["shutdown"]),
                    ])
                    ops.daemon.wait_observation.assert_called_once_with(timeout=30, expected=0)
                    for call in ops.processes._drain.call_args_list:
                        self.assertEqual(call.kwargs, {"timeout": 0, "exclude": (ops.daemon,)})

    def test_preadmission_cleanup_refuses_unknown_rows_groups_and_pending_clients(self):
        default = {"default": {"status": "Running", "parallel_tasks": 1}}
        unknown_row = {
            "id": 99, "created_at": "2026-09-15T00:00:00Z", "label": "unknown",
            "original_command": "unknown", "command": "unknown", "path": str(self.fixture.state),
            "envs": {}, "group": "default", "dependencies": [], "priority": 0,
            "status": {"Queued": {"enqueued_at": "2026-09-15T00:00:00Z"}},
        }
        cases: tuple[tuple[dict[str, object], tuple[dict[str, object], ...]], ...] = (
            ({"tasks": {"99": unknown_row}, "groups": default}, ()),
            ({"tasks": {}, "groups": {**default, "unknown": default["default"]}}, ()),
            ({"tasks": {}, "groups": {"default": {"status": "Paused", "parallel_tasks": 1}}}, ()),
            ({"tasks": {}, "groups": {"default": {"status": "Running", "parallel_tasks": 2}}}, ()),
            ({"tasks": {}, "groups": default}, ({"pid": 17},)),
        )
        for status, pending in cases:
            with self.subTest(status=status, pending=pending):
                supervisor_queue_policy._validate_pueue_status(status)
                ops = self.fixture.preadmission_cleanup(status, pending)
                self.assertFalse(ops.safe_failure_shutdown())
                self.assertFalse(ops.closed)
                ops.daemon.wait_observation.assert_not_called()
                self.assertNotIn(["shutdown"], [call.args[1] for call in ops.client.call_args_list])

    def test_pueue_status_accepts_additive_fields(self):
        row: dict[str, object] = {
            "id": 99, "created_at": "2026-09-15T00:00:00Z", "label": "known",
            "original_command": "known", "command": "known", "path": str(self.fixture.state),
            "envs": {}, "group": "default", "dependencies": [], "priority": 0,
            "status": {"Queued": {"enqueued_at": "2026-09-15T00:00:00Z",
                                    "future_state_field": True}},
            "future_task_field": {"version": 2},
        }
        status: dict[str, object] = {
            "tasks": {"99": row},
            "groups": {"default": {"status": "Running", "parallel_tasks": 1,
                                     "future_group_field": "ignored"}},
            "future_status_field": ["ignored"],
        }

        supervisor_queue_policy._validate_pueue_status(status)
        self.assertEqual(supervisor_queue_policy.row_state(row), "Queued")

    def test_pueue_status_rejects_unknown_lifecycle_variant(self):
        row: dict[str, object] = {
            "id": 99, "created_at": "2026-09-15T00:00:00Z", "label": "known",
            "original_command": "known", "command": "known", "path": str(self.fixture.state),
            "envs": {}, "group": "default", "dependencies": [], "priority": 0,
            "status": {
                "Done": {"enqueued_at": "2026-09-15T00:00:00Z",
                          "start": "2026-09-15T00:00:00Z",
                          "end": "2026-09-15T00:00:00Z", "result": "Success"},
                "Aborted": {"end": "2026-09-15T00:00:00Z"},
            },
        }
        status: dict[str, object] = {"tasks": {"99": row},
                  "groups": {"default": {"status": "Running", "parallel_tasks": 1}}}

        with self.assertRaises(AcceptanceFailure):
            supervisor_queue_policy._validate_pueue_status(status)

    def test_preadmission_cleanup_refuses_unproven_durable_or_attempted_work(self):
        status: dict[str, object] = {"tasks": {}, "groups": {"default": {"status": "Running", "parallel_tasks": 1}}}
        for name in ("tasks", "inspections", "inspection-group", "unexpected"):
            with self.subTest(durable=name):
                (self.fixture.state / name).mkdir()
                ops = self.fixture.preadmission_cleanup(status)
                self.assertFalse(ops.safe_failure_shutdown())
                ops.daemon.wait_observation.assert_not_called()
                (self.fixture.state / name).rmdir()
        for field, value in (("dispatch_attempts", {TASK}), ("inspection_attempts", {TASK}),
                             ("tasks", {"fresh": TASK}), ("root_id", "c" * 32)):
            with self.subTest(field=field):
                ops = self.fixture.preadmission_cleanup(status)
                setattr(ops, field, value)
                self.assertFalse(ops.safe_failure_shutdown())
                ops.daemon.wait_observation.assert_not_called()

    def test_static_authentication_refusal_uses_exact_completed_cleanup(self):
        self.fixture.static_refusal()
        response = object_at(self.fixture.ops.dispatch_observations[0], "response")
        response["status"] = "blocked"
        response["error"] = ("provider authentication unavailable: unsupported-effective-config: "
                             "persistent ChatGPT authentication is required")
        ops = self.fixture.preadmission_cleanup({
            "tasks": {}, "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
        }, ops=self.fixture.ops)
        with patch.object(task_operations.time, "sleep",
                   side_effect=AssertionError("verified static refusal must close")):
            self.assertTrue(ops.retain_failure_ownership())
        ops.daemon.wait_observation.assert_called_once_with(timeout=30, expected=0)
        ops.closed = False
        response["status"] = "unknown"
        self.assertFalse(ops.safe_failure_shutdown())
        response["status"] = "blocked"
        response["error"] = "unrecognized failure: credentials unavailable"
        self.assertFalse(ops.safe_failure_shutdown())

    def test_static_refusal_rejects_durable_guards_and_unknown_rows(self):
        self.fixture.static_refusal()
        (self.fixture.state / "inspections").mkdir()
        self.assertFalse(self.fixture.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        shutil.rmtree(self.fixture.state / "inspections")
        unknown = {"id": 9, "label": "unknown", "group": "default",
                   "original_command": "unknown", "command": "unknown", "path": str(self.fixture.state),
                   "status": {"Done": {"result": "Success"}}}
        self.assertFalse(self.fixture.ops.failure_queue_finished({
            "tasks": {"9": unknown}, "groups": {},
        }))

    def test_static_refusal_requires_exact_integer_schema_version(self):
        for version in (True, 1.0, "1", None):
            with self.subTest(version=version):
                self.fixture.static_refusal()
                object_at(self.fixture.ops.dispatch_observations[0], "response")["schema_version"] = version
                self.assertFalse(self.fixture.ops.failure_queue_finished({"tasks": {}, "groups": {}}))
        self.fixture.static_refusal()
        self.assertTrue(self.fixture.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

    def test_static_refusal_rejects_uncertain_or_incomplete_attempts(self):
        self.fixture.static_refusal(exit_code=1)
        self.assertFalse(self.fixture.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        self.fixture.static_refusal(response=None)
        self.fixture.ops.dispatch_observations[0]["response"] = None
        self.assertFalse(self.fixture.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        self.fixture.static_refusal()
        self.fixture.ops.dispatch_attempts.add("c" * 32)
        self.assertFalse(self.fixture.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

    def test_static_refusal_safe_shutdown_requires_no_active_clients(self):
        self.fixture.static_refusal()
        self.fixture.ops.pueue_config = self.fixture.config
        daemon = ObservedDaemon()
        self.fixture.ops.daemon = daemon
        status_directory = self.fixture.base / "status"
        status_directory.mkdir(mode=0o700)
        write_json(status_directory / "stdout", {"tasks": {}, "groups": {}})
        status_process = CapturedCommand(directory=status_directory,
                                         result={"exit_code": 0, "natural_wait": True})
        calls = []
        self.fixture.ops.processes = FixtureProcesses()
        self.enterContext(patch.object(self.fixture.ops, "client", side_effect=lambda name, operation, **_kwargs: calls.append((name, operation)) or status_process))

        self.assertTrue(self.fixture.ops.safe_failure_shutdown())
        self.assertTrue(self.fixture.ops.closed)
        self.assertEqual(calls, [("failure-queue", ["status", "--json"]),
                                 ("failure-shutdown", ["shutdown"])])

        self.fixture.static_refusal()
        self.fixture.ops.closed = False
        self.fixture.ops.processes = FixtureProcesses(drain=Mock(return_value=[{"pid": 1}]))
        self.assertFalse(self.fixture.ops.safe_failure_shutdown())

    def test_static_refusal_requires_durable_completed_response_and_exit_two(self):
        self.fixture.static_refusal()
        self.fixture.ops.dispatch_observations[0]["receipt_written"] = False
        self.assertFalse(self.fixture.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        self.fixture.static_refusal()
        object_at(self.fixture.ops.dispatch_observations[0], "response")["root_id"] = "c" * 32
        self.assertFalse(self.fixture.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        self.fixture.static_refusal()
        object_at(self.fixture.ops.dispatch_observations[0], "response")["admission"] = "not-admitted"
        self.assertFalse(self.fixture.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        self.fixture.static_refusal()
        object_at(self.fixture.ops.dispatch_observations[0], "response")["supervisor"] = {"matched": False}
        self.assertFalse(self.fixture.ops.failure_queue_finished({"tasks": {}, "groups": {}}))


if __name__ == "__main__":
    unittest.main()
