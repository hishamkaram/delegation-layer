"""Hermetic inspection queue and journal checks for the native acceptance harness."""

from __future__ import annotations

from copy import deepcopy
from pathlib import Path
from types import SimpleNamespace
import shlex
import shutil
import tempfile
import unittest
from unittest.mock import Mock, patch

import acceptance_inspection as inspection_acceptance
import acceptance_provider_common as provider_common
import acceptance_supervisor_common as supervisor_common
from acceptance_provider_common import AcceptanceFailure, NativeTaskOps, _inspection_group
from acceptance_supervisor_common import digest, read_json, write_json


ROOT = "a" * 32
TASK = "b" * 32


class InspectionHarnessTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.base = Path(self.temporary.name)
        self.state = self.base / "state"
        self.state_parent = self.base / "state-parent"
        self.output = self.base / "output"
        for directory in (self.state, self.state_parent, self.output):
            directory.mkdir(mode=0o700)

        self.runner = self.base / "delegate-run"
        self.pueue = self.base / "pueue"
        self.config = self.base / "pueue.yml"
        self.runner.write_bytes(b"runner")
        self.pueue.write_bytes(b"pueue")
        self.config.write_bytes(b"config")
        self.runner.chmod(0o700)
        self.pueue.chmod(0o700)
        self.config.chmod(0o600)

        self.ops = NativeTaskOps(SimpleNamespace(), self.base / "delegate", self.runner,
                                 self.pueue, self.state_parent, self.state, self.output)
        self.ops.bind_supervisor(self.config)
        self.ops.root_id = ROOT
        self.ops.tasks = {"fresh": TASK}
        self.ops.labels = {"fresh": f"delegate:{ROOT}:{TASK}"}
        self.ops.numbers = {"fresh": 7}
        self.ops.dispatch_attempts = {TASK}
        write_json(self.state / "root.json", {
            "schema_version": 1, "root_id": ROOT, "created_at": "2026-09-15T00:00:00Z",
        })

    def tearDown(self):
        self.temporary.cleanup()

    def inspection_records(self, result: str | None = "eligible",
                           include_ordinary: bool = True) -> Path:
        # Declare the expectation before restoring the seeded dispatch
        # attempt, matching the production pre-dispatch ordering.
        self.ops.dispatch_attempts.discard(TASK)
        directory = self.state / "inspections" / TASK
        directory.mkdir(mode=0o700, parents=True)
        task_record = {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "provider": "fixture:test", "mode": "read-only",
            "canonical_cwd": str(self.base),
            "requested_config": {"permission": "read-only", "budget": "1m0s"},
            "budget_nanos": 60_000_000_000,
            "brief_sha256": "c" * 64, "brief_length": 31,
        }
        if include_ordinary:
            ordinary = self.state / "tasks" / TASK
            ordinary.mkdir(mode=0o700, parents=True)
            task_bytes = provider_common._canonical_task_bytes(task_record)
            (ordinary / "task.json").write_bytes(task_bytes)
            (ordinary / "task.json").chmod(0o600)
        request = {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "task": task_record,
            "task_sha256": provider_common.sha(provider_common._canonical_task_bytes(task_record)),
            "binding": {
                "definition_revision": "inspection-v1",
                "definition_sha256": "d" * 64,
                "helper_executable": str(self.base / "helper"),
                "helper_sha256": "e" * 64,
                "worker_executable": str(self.runner),
                "worker_sha256": digest(self.runner),
                "supervisor": {
                    "client_executable": str(self.pueue),
                    "client_sha256": digest(self.pueue),
                    "resolved_config_sha256": "f" * 64,
                    "endpoint": str(self.base / "pueue.sock"),
                    "config_path": str(self.config),
                    "config_digest": digest(self.config),
                    "observed_version": "pueue 4.0.4",
                },
            },
            "group": _inspection_group(ROOT),
            "label": f"{_inspection_group(ROOT)}-{TASK}",
            "created_at": "2026-09-15T00:00:00Z",
            "deadline": "2026-09-15T00:00:20Z",
        }
        write_json(directory / "request.json", request)
        self.ops.expect_inspection(TASK, request["binding"])
        self.ops.dispatch_attempts.add(TASK)
        request_sha = digest(directory / "request.json")
        write_json(directory / "receipt.json", {
            "schema_version": 1, "request_sha256": request_sha, "numeric_task_id": 42,
        })
        write_json(directory / "submission.json", {
            "schema_version": 1, "request_sha256": request_sha,
            "created_at": "2026-09-15T00:00:01Z",
        })
        if result is not None:
            write_json(directory / "start.json", {
                "schema_version": 1, "request_sha256": request_sha,
                "created_at": "2026-09-15T00:00:02Z",
            })
            write_json(directory / "result.json", {
                "schema_version": 1, "request_sha256": request_sha, "reason": result,
                "facts": {"eligible": True} if result == "eligible" else {},
            })
            result_sha = digest(directory / "result.json")
            write_json(directory / "completion.json", {
                "schema_version": 1, "request_sha256": request_sha,
                "result_sha256": result_sha,
                "completed_at": "2026-09-15T00:00:02Z",
                "native_exit": "successful-exit" if result == "eligible" else "unavailable",
            })
            if result == "eligible":
                write_json(directory / "worker-observation.json", {
                    "schema_version": 1, "request_sha256": request_sha,
                    "completion_sha256": digest(directory / "completion.json"),
                    "numeric_task_id": 42, "state": "succeeded",
                })
        return directory

    @staticmethod
    def refresh_request_links(directory: Path) -> None:
        request_sha = digest(directory / "request.json")
        for name in ("receipt.json", "submission.json", "start.json"):
            path = directory / name
            if path.exists():
                record = read_json(path)
                record["request_sha256"] = request_sha
                write_json(path, record, replace=True)

    def ordinary_row(self, result: str = "Success", name: str = "fresh",
                     task: str = TASK, number: int = 7) -> dict[str, object]:
        command = shlex.join([str(self.runner), "--root", str(self.state), task])
        return {
            "id": number, "label": self.ops.labels[name], "group": "default",
            "original_command": command, "command": command, "path": str(self.state),
            "status": {"Done": {"result": result}},
        }

    def inspection_row(self, result: object = "Success") -> dict[str, object]:
        command = shlex.join([str(self.runner), "--inspection", "--root", str(self.state), TASK])
        return {
            "id": 42, "label": f"{_inspection_group(ROOT)}-{TASK}",
            "group": _inspection_group(ROOT), "original_command": command,
            "command": command, "path": str(self.state),
            "status": {"Done": {"result": result}},
        }

    def status(self, inspection: dict[str, object] | None = None,
               ordinary: dict[str, object] | None = None,
               include_ordinary: bool = True):
        rows = {}
        if include_ordinary:
            rows["7"] = ordinary or self.ordinary_row()
        if inspection is not None:
            rows["42"] = inspection
        return {"tasks": rows, "groups": {
            "default": {"status": "Running", "parallel_tasks": 1},
            _inspection_group(ROOT): {"status": "Running", "parallel_tasks": 1},
        }}

    def static_refusal(self, exit_code: int = 2,
                       response: dict[str, object] | None = None) -> None:
        self.state.joinpath(".maintenance.lock").write_bytes(b"")
        self.ops.tasks = {}
        self.ops.labels = {}
        self.ops.dispatch_attempts = {TASK}
        self.ops.inspection_attempts = {TASK}
        self.ops.dispatch_observations = [{
            "name": "static-refusal",
            "task_id": TASK,
            "exit_code": exit_code,
            "natural_wait": True,
            "response": response or {
                "schema_version": 1, "command": "dispatch", "root_id": ROOT,
                "task_id": TASK, "admission": "unknown", "liveness": "undetermined",
                "publication": "unknown",
                "error": "unsupported-effective-config: static native profile refusal",
            },
            "receipt_written": True,
        }]

    def admitted_response(self) -> dict[str, object]:
        return {
            "schema_version": 1, "command": "dispatch", "root_id": ROOT,
            "task_id": TASK, "admission": "admitted", "liveness": "running",
            "publication": "unknown",
            "supervisor": {"matched": True, "state": "Queued", "numeric_task_id": 7},
        }

    def test_dispatch_rejection_code_keeps_positive_admission_for_cleanup(self):
        directory = self.base / "dispatch-rejected"
        directory.mkdir(mode=0o700)
        response = self.admitted_response()
        write_json(directory / "stdout", response)
        process = SimpleNamespace(directory=directory,
                                  result={"exit_code": 1, "natural_wait": True})
        self.ops.tasks = {}
        self.ops.labels = {}
        self.ops.numbers = {}
        self.ops.dispatch_attempts = set()
        self.ops.processes = SimpleNamespace(run=lambda *args, **kwargs: process)

        with self.assertRaisesRegex(AcceptanceFailure, "did not exit successfully"):
            self.ops.dispatch("rejected", TASK, ["delegate"])

        self.assertEqual(self.ops.tasks, {"rejected": TASK})
        self.assertEqual(self.ops.numbers, {"rejected": 7})
        self.assertTrue(self.ops.dispatch_observations[0]["receipt_written"])

    def test_dispatch_receipt_failure_retains_positive_admission(self):
        directory = self.base / "dispatch-receipt-failure"
        directory.mkdir(mode=0o700)
        response = self.admitted_response()
        write_json(directory / "stdout", response)
        process = SimpleNamespace(directory=directory,
                                  result={"exit_code": 0, "natural_wait": True})
        self.ops.tasks = {}
        self.ops.labels = {}
        self.ops.numbers = {}
        self.ops.dispatch_attempts = set()
        self.ops.processes = SimpleNamespace(run=lambda *args, **kwargs: process)

        def fail_attempt_receipt(path, value, replace=False):
            if Path(path).name == "receipt-failure-dispatch-attempt.json":
                raise OSError("injected dispatch-attempt receipt failure")
            return supervisor_common.write_json(path, value, replace)

        with patch.object(provider_common, "write_json", side_effect=fail_attempt_receipt):
            with self.assertRaisesRegex(OSError, "dispatch-attempt receipt failure"):
                self.ops.dispatch("receipt-failure", TASK, ["delegate"])

        self.assertEqual(self.ops.tasks, {"receipt-failure": TASK})
        self.assertFalse(self.ops.dispatch_observations[0]["receipt_written"])
        self.assertTrue((self.output / "receipt-failure-dispatch.json").is_file())

    def test_dispatch_records_static_refusal_before_admission_validation(self):
        directory = self.base / "dispatch"
        directory.mkdir(mode=0o700)
        response = {
            "schema_version": 1, "command": "dispatch", "root_id": ROOT,
            "task_id": TASK, "admission": "unknown", "liveness": "undetermined",
            "publication": "unknown",
            "error": "unsupported-effective-config: static native profile refusal",
        }
        write_json(directory / "stdout", response)
        process = SimpleNamespace(directory=directory,
                                  result={"exit_code": 2, "natural_wait": True})
        calls = []
        self.ops.tasks = {}
        self.ops.labels = {}
        self.ops.dispatch_attempts = set()
        self.ops.inspection_attempts = {TASK}
        self.ops.processes = SimpleNamespace(
            run=lambda *args, **kwargs: calls.append((args, kwargs)) or process)

        with self.assertRaisesRegex(AcceptanceFailure, "was not admitted"):
            self.ops.dispatch("static-refusal", TASK, ["delegate"])

        self.assertEqual(calls[0][1]["expected"], None)
        self.assertEqual(len(self.ops.dispatch_observations), 1)
        observation = self.ops.dispatch_observations[0]
        self.assertEqual(observation["exit_code"], 2)
        self.assertTrue(observation["receipt_written"])
        self.assertEqual(read_json(self.output / "static-refusal-dispatch-attempt.json"), {
            **observation, "receipt_written": True,
        })

    def test_dispatch_rejects_duplicate_before_starting_process(self):
        process = Mock()
        self.ops.processes = Mock(run=process)

        with self.assertRaisesRegex(AcceptanceFailure, "duplicate dispatch attempt"):
            self.ops.dispatch("duplicate", TASK, ["delegate"])

        process.assert_not_called()

    def test_static_refusal_allows_only_exact_empty_private_queue(self):
        self.static_refusal()
        self.assertTrue(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))
        self.assertTrue(self.ops.failure_queue_finished({
            "tasks": {}, "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
        }))

    def test_static_refusal_rejects_durable_guards_and_unknown_rows(self):
        self.static_refusal()
        (self.state / "inspections").mkdir()
        self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        shutil.rmtree(self.state / "inspections")
        unknown = {"id": 9, "label": "unknown", "group": "default",
                   "original_command": "unknown", "command": "unknown", "path": str(self.state),
                   "status": {"Done": {"result": "Success"}}}
        self.assertFalse(self.ops.failure_queue_finished({
            "tasks": {"9": unknown}, "groups": {},
        }))

    def test_static_refusal_requires_exact_integer_schema_version(self):
        for version in (True, 1.0, "1", None):
            with self.subTest(version=version):
                self.static_refusal()
                self.ops.dispatch_observations[0]["response"]["schema_version"] = version
                self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))
        self.static_refusal()
        self.assertTrue(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

    def test_static_refusal_rejects_uncertain_or_incomplete_attempts(self):
        self.static_refusal(exit_code=1)
        self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        self.static_refusal(response=None)
        self.ops.dispatch_observations[0]["response"] = None
        self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        self.static_refusal()
        self.ops.dispatch_attempts.add("c" * 32)
        self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

    def test_static_refusal_safe_shutdown_requires_no_active_clients(self):
        self.static_refusal()
        self.ops.pueue_config = self.config
        daemon = SimpleNamespace(poll=lambda: None, wait=lambda **_kwargs: None)
        self.ops.daemon = daemon
        status_directory = self.base / "status"
        status_directory.mkdir(mode=0o700)
        write_json(status_directory / "stdout", {"tasks": {}, "groups": {}})
        status_process = SimpleNamespace(directory=status_directory,
                                         result={"exit_code": 0, "natural_wait": True})
        calls = []
        self.ops.processes = SimpleNamespace(drain=lambda **_kwargs: [])
        self.ops.client = lambda name, operation, **_kwargs: calls.append((name, operation)) or status_process

        self.assertTrue(self.ops.safe_failure_shutdown())
        self.assertTrue(self.ops.closed)
        self.assertEqual(calls, [("failure-queue", ["status", "--json"]),
                                 ("failure-shutdown", ["shutdown"])])

        self.static_refusal()
        self.ops.closed = False
        self.ops.processes = SimpleNamespace(drain=lambda **_kwargs: [{"pid": 1}])
        self.assertFalse(self.ops.safe_failure_shutdown())

    def test_static_refusal_requires_durable_completed_response_and_exit_two(self):
        self.static_refusal()
        self.ops.dispatch_observations[0]["receipt_written"] = False
        self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        self.static_refusal()
        self.ops.dispatch_observations[0]["response"]["root_id"] = "c" * 32
        self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        self.static_refusal()
        self.ops.dispatch_observations[0]["response"]["admission"] = "not-admitted"
        self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

        self.static_refusal()
        self.ops.dispatch_observations[0]["response"]["supervisor"] = {"matched": False}
        self.assertFalse(self.ops.failure_queue_finished({"tasks": {}, "groups": {}}))

    def test_shutdown_requires_complete_success_proof_for_inspection(self):
        self.inspection_records()
        status = self.status(self.inspection_row())
        calls = []
        self.ops.queue_status = lambda _name: status
        self.ops.processes = SimpleNamespace(drain=lambda **_kwargs: [])
        self.ops.client = lambda name, operation, **_kwargs: calls.append((name, operation))
        self.ops.daemon = SimpleNamespace(wait=lambda **_kwargs: None)

        self.ops.shutdown()

        self.assertTrue(self.ops.closed)
        self.assertEqual(calls, [("private-shutdown", ["shutdown"])])

    def test_successful_shutdown_rejects_missing_worker_observation(self):
        directory = self.inspection_records()
        (directory / "worker-observation.json").unlink()
        with self.assertRaisesRegex(AcceptanceFailure, "proof"):
            self.ops._validate_queue_membership(self.status(self.inspection_row()), False)

    def test_inspection_expectation_requires_full_binding_before_dispatch(self):
        self.ops.dispatch_attempts.clear()

        with self.assertRaisesRegex(AcceptanceFailure, "binding is required"):
            self.ops.expect_inspection(TASK)

    def test_inspection_expectation_rejects_late_and_conflicting_registration(self):
        directory = self.inspection_records()
        binding = read_json(directory / "request.json")["binding"]

        self.ops.dispatch_attempts.discard(TASK)
        conflicting = deepcopy(binding)
        conflicting["helper_executable"] = str(self.base / "other-helper")
        with self.assertRaisesRegex(AcceptanceFailure, "different source"):
            self.ops.expect_inspection(TASK, conflicting)

        self.ops.dispatch_attempts.add(TASK)
        with self.assertRaisesRegex(AcceptanceFailure, "precede dispatch"):
            self.ops.expect_inspection(TASK, binding)

    def test_failed_journal_recomputes_embedded_task_digest_without_ordinary_task(self):
        directory = self.inspection_records(result=None, include_ordinary=False)
        request = read_json(directory / "request.json")
        request["task_sha256"] = "d" * 64
        write_json(directory / "request.json", request, replace=True)
        self.refresh_request_links(directory)
        self.ops.tasks = {}
        status = self.status(self.inspection_row({"Failed": 17}), include_ordinary=False)
        self.assertFalse(self.ops.failure_queue_finished(status))

    def test_inspection_request_requires_exact_deadline_window(self):
        directory = self.inspection_records(result=None, include_ordinary=False)
        request = read_json(directory / "request.json")
        request["deadline"] = "2026-09-15T00:00:40Z"
        write_json(directory / "request.json", request, replace=True)
        self.refresh_request_links(directory)
        self.ops.tasks = {}
        status = self.status(self.inspection_row({"Failed": 17}), include_ordinary=False)
        self.assertFalse(self.ops.failure_queue_finished(status))

    def test_inspection_request_cannot_predate_root_creation(self):
        self.inspection_records()
        root = read_json(self.state / "root.json")
        root["created_at"] = "2026-09-15T00:00:01Z"
        write_json(self.state / "root.json", root, replace=True)

        self.assertFalse(self.ops.failure_queue_finished(self.status(self.inspection_row())))

    def test_root_creation_timestamp_remains_pinned_across_observations(self):
        self.inspection_records()
        status = self.status(self.inspection_row())
        self.assertTrue(self.ops.failure_queue_finished(status))
        root = read_json(self.state / "root.json")
        original = root["created_at"]
        for changed in ("2026-09-14T23:59:59Z", "2026-09-15T00:00:01Z"):
            with self.subTest(created_at=changed):
                root["created_at"] = changed
                write_json(self.state / "root.json", root, replace=True)
                self.assertFalse(self.ops.failure_queue_finished(status))
        root["created_at"] = original
        write_json(self.state / "root.json", root, replace=True)
        self.assertTrue(self.ops.failure_queue_finished(status))

    def test_inspection_claim_timestamps_follow_request_and_each_other(self):
        directory = self.inspection_records()
        submission = read_json(directory / "submission.json")
        submission["created_at"] = "2026-09-15T00:00:03Z"
        write_json(directory / "submission.json", submission, replace=True)
        self.assertFalse(self.ops.failure_queue_finished(self.status(self.inspection_row())))

        submission["created_at"] = "2026-09-15T00:00:01Z"
        write_json(directory / "submission.json", submission, replace=True)
        completion = read_json(directory / "completion.json")
        completion["completed_at"] = "2026-09-15T00:00:01Z"
        write_json(directory / "completion.json", completion, replace=True)
        observation = read_json(directory / "worker-observation.json")
        observation["completion_sha256"] = digest(directory / "completion.json")
        write_json(directory / "worker-observation.json", observation, replace=True)
        self.assertFalse(self.ops.failure_queue_finished(self.status(self.inspection_row())))

    def test_inspection_cleanup_requires_submission_and_conditional_start_guards(self):
        directory = self.inspection_records()
        (directory / "submission.json").unlink()
        self.assertFalse(self.ops.failure_queue_finished(self.status(self.inspection_row())))

        shutil.rmtree(directory)
        shutil.rmtree(self.state / "tasks" / TASK)
        directory = self.inspection_records()
        (directory / "start.json").unlink()
        self.assertFalse(self.ops.failure_queue_finished(self.status(self.inspection_row())))

    def test_inspection_binding_must_match_expected_source(self):
        directory = self.inspection_records()
        request = read_json(directory / "request.json")
        self.ops.inspection_bindings[TASK] = deepcopy(request["binding"])
        self.ops._validate_queue_membership(self.status(self.inspection_row()), False)

        request["binding"]["helper_executable"] = str(self.base / "foreign-helper")
        write_json(directory / "request.json", request, replace=True)
        self.refresh_request_links(directory)
        with self.assertRaisesRegex(AcceptanceFailure, "binding"):
            self.ops._validate_queue_membership(self.status(self.inspection_row()), False)

    def test_fixture_binding_uses_static_definition_and_supervisor_sources(self):
        helper = self.base / "inspection-helper"
        helper_config = self.base / "helper.json"
        helper.write_bytes(b"fixture helper")
        helper.chmod(0o700)
        helper_config.write_bytes(b"{\"schema_version\":1}\n")
        helper_config.chmod(0o600)
        environment = {"FIXTURE": "yes", "LANG": "C"}
        supervisor = provider_common.supervisor_binding(
            self.pueue, self.config, self.base, digest(self.config))
        binding = inspection_acceptance.fixture_inspection_binding(
            helper, helper_config, self.base / "workspace", environment,
            self.runner, supervisor)

        self.assertEqual(binding["definition_revision"], "inspection-fixture-v1")
        self.assertEqual(binding["helper_executable"], str(helper.resolve()))
        self.assertEqual(binding["worker_executable"], str(self.runner.resolve()))
        self.assertEqual(binding["supervisor"], supervisor)
        self.assertEqual(binding["supervisor"]["endpoint"],
                         "unix:" + str((self.base / "run" / "p.sock").resolve()))
        self.assertEqual(binding["supervisor"]["config_digest"], digest(self.config))

        changed_environment = dict(environment, FIXTURE="changed")
        changed = inspection_acceptance.fixture_inspection_binding(
            helper, helper_config, self.base / "workspace", changed_environment,
            self.runner, supervisor)
        self.assertNotEqual(binding["definition_sha256"], changed["definition_sha256"])

        changed_helper_config = self.base / "other-helper.json"
        changed_helper_config.write_bytes(helper_config.read_bytes())
        changed_helper_config.chmod(0o600)
        changed_path = inspection_acceptance.fixture_inspection_binding(
            helper, changed_helper_config, self.base / "workspace", environment,
            self.runner, supervisor)
        self.assertNotEqual(binding["definition_sha256"], changed_path["definition_sha256"])

    def test_noneligible_inspection_facts_and_exit_must_be_empty_and_unavailable(self):
        directory = self.inspection_records(result="unavailable")
        result_path = directory / "result.json"
        result = read_json(result_path)
        result["facts"] = {"eligible": False}
        write_json(result_path, result, replace=True)
        completion = read_json(directory / "completion.json")
        completion["result_sha256"] = digest(result_path)
        write_json(directory / "completion.json", completion, replace=True)
        self.assertFalse(self.ops.failure_queue_finished(self.status(self.inspection_row())))

        shutil.rmtree(directory)
        shutil.rmtree(self.state / "tasks" / TASK)
        directory = self.inspection_records(result="unavailable", include_ordinary=False)
        completion = read_json(directory / "completion.json")
        completion["native_exit"] = "successful-exit"
        write_json(directory / "completion.json", completion, replace=True)
        self.assertFalse(self.ops.failure_queue_finished(
            self.status(self.inspection_row({"Failed": 17}), include_ordinary=False)))

    def test_inspection_result_requires_completion_record(self):
        directory = self.inspection_records(result="unavailable")
        (directory / "completion.json").unlink()
        self.assertFalse(self.ops.failure_queue_finished(self.status(self.inspection_row())))

    def test_ordinary_attempt_without_inspection_journal_is_allowed(self):
        self.state.joinpath("inspections").mkdir(mode=0o700)
        self.ops._validate_queue_membership({
            "tasks": {"7": self.ordinary_row()},
            "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
        }, False)

    def test_declared_inspection_without_journal_is_rejected(self):
        directory = self.inspection_records(result=None)
        shutil.rmtree(directory)
        self.assertFalse(self.ops.failure_queue_finished({
            "tasks": {"7": self.ordinary_row()},
            "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
        }))

    def test_mixed_attempts_allow_ordinary_task_without_inspection(self):
        other = "c" * 32
        self.ops.tasks["resume"] = other
        self.ops.labels["resume"] = f"delegate:{ROOT}:{other}"
        self.ops.numbers["resume"] = 8
        self.ops.dispatch_attempts.add(other)
        self.inspection_records()
        status = self.status(self.inspection_row())
        status["tasks"]["8"] = self.ordinary_row(name="resume", task=other, number=8)

        self.ops._validate_queue_membership(status, False)

    def test_queue_rejects_unknown_inspection_row_and_wrong_argv(self):
        self.inspection_records()
        status = self.status(self.inspection_row())
        unknown = deepcopy(status)
        unknown["tasks"]["99"] = {
            "id": 99, "label": "delegation-inspection-" + ROOT + "-" + "c" * 32,
            "group": _inspection_group(ROOT), "original_command": "unknown",
            "command": "unknown", "path": str(self.state),
            "status": {"Done": {"result": "Success"}},
        }
        with self.assertRaisesRegex(AcceptanceFailure, "membership|labels"):
            self.ops._validate_queue_membership(unknown, False)

        wrong_argv = deepcopy(status)
        wrong_argv["tasks"]["42"]["command"] = shlex.join(
            [str(self.runner), "--root", str(self.state), TASK])
        with self.assertRaisesRegex(AcceptanceFailure, "argv"):
            self.ops._validate_queue_membership(wrong_argv, False)

    def test_queue_rejects_wrong_ordinary_id_group_and_argv(self):
        for mutation, message in (
                (lambda row: row.update(id=8), "numeric"),
                (lambda row: row.update(group="foreign"), "group"),
                (lambda row: row.update(command="/bin/true"), "exact")):
            with self.subTest(message=message):
                row = self.ordinary_row()
                mutation(row)
                with self.assertRaisesRegex(AcceptanceFailure, "identity|group|exact|runner"):
                    self.ops._validate_queue_membership({
                        "tasks": {"7": row},
                        "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
                    }, False)

    def test_queue_rejects_foreign_groups_even_without_inspection_rows(self):
        self.state.joinpath("inspections").mkdir(mode=0o700)
        status = {
            "tasks": {"7": self.ordinary_row()},
            "groups": {
                "default": {"status": "Running", "parallel_tasks": 1},
                "foreign": {"status": "Running", "parallel_tasks": 1},
            },
        }
        with self.assertRaisesRegex(AcceptanceFailure, "unknown supervisor group"):
            self.ops._validate_queue_membership(status, False)

    def test_ordinary_failure_cleanup_binds_exact_id_label_and_default_group(self):
        status = {
            "tasks": {"7": self.ordinary_row()},
            "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
        }
        self.assertTrue(self.ops.failure_queue_finished(status))

        wrong_id = deepcopy(status)
        wrong_id["tasks"]["7"]["id"] = 8
        self.assertFalse(self.ops.failure_queue_finished(wrong_id))

        foreign_group = deepcopy(status)
        foreign_group["tasks"]["7"]["group"] = "foreign"
        self.assertFalse(self.ops.failure_queue_finished(foreign_group))

        duplicate_label = deepcopy(status)
        duplicate_label["tasks"]["8"] = deepcopy(duplicate_label["tasks"]["7"])
        duplicate_label["tasks"]["8"]["id"] = 8
        self.assertFalse(self.ops.failure_queue_finished(duplicate_label))

    def test_status_observation_requires_successful_natural_exit_and_strict_schema(self):
        directory = self.base / "status-strict"
        directory.mkdir(mode=0o700)
        write_json(directory / "stdout", {"tasks": {}, "groups": {}})
        process = SimpleNamespace(directory=directory,
                                  result={"exit_code": 1, "natural_wait": True})
        with self.assertRaisesRegex(AcceptanceFailure, "did not exit successfully"):
            provider_common.status_jobs(process)

        process.result = {"exit_code": 0, "natural_wait": True}
        write_json(directory / "stdout", {"Tasks": {}, "groups": {}}, replace=True)
        with self.assertRaisesRegex(AcceptanceFailure, "unknown or missing"):
            provider_common.status_jobs(process)

    def test_status_observation_rejects_nonstandard_json_constants(self):
        directory = self.base / "status-constants"
        directory.mkdir(mode=0o700)
        process = SimpleNamespace(directory=directory,
                                  result={"exit_code": 0, "natural_wait": True})

        for constant in ("NaN", "Infinity", "-Infinity"):
            with self.subTest(constant=constant):
                (directory / "stdout").write_text(
                    '{"tasks":' + constant + ',"groups":{}}')
                with self.assertRaisesRegex(AcceptanceFailure, "non-standard JSON constant"):
                    provider_common.status_jobs(process)

    def test_status_observation_requires_exact_integer_zero_exit(self):
        directory = self.base / "status-exit-type"
        directory.mkdir(mode=0o700)
        write_json(directory / "stdout", {"tasks": {}, "groups": {}})
        process = SimpleNamespace(directory=directory, result={"natural_wait": True})

        for exit_code in (False, 0.0, "0", None):
            with self.subTest(exit_code=exit_code):
                process.result["exit_code"] = exit_code
                with self.assertRaisesRegex(AcceptanceFailure, "did not exit successfully"):
                    provider_common.status_jobs(process)

    def test_closed_supervisor_rejects_all_new_process_launches(self):
        process = Mock()
        self.ops.processes = Mock(run=process)
        self.ops.closed = True

        with self.assertRaisesRegex(AcceptanceFailure, "supervisor is closed"):
            self.ops.direct("late-direct", ["delegate"])
        with self.assertRaisesRegex(AcceptanceFailure, "supervisor is closed"):
            self.ops.dispatch("late-dispatch", "c" * 32, ["delegate"])

        process.assert_not_called()

    def test_control_decoder_rejects_simple_fold_alias(self):
        directory = self.base / "alias"
        directory.mkdir(mode=0o700)
        (directory / "stdout").write_text('{"tasks":{},"Tasks":{}}')
        process = SimpleNamespace(directory=directory,
                                  result={"exit_code": 0, "natural_wait": True})
        with self.assertRaisesRegex(AcceptanceFailure, "duplicate JSON object key"):
            provider_common.status_jobs(process)

    def test_replay_preserves_inspection_journal_and_queue(self):
        self.inspection_records()
        status = self.status(self.inspection_row())
        self.ops.queue_status = lambda _name: deepcopy(status)

        self.ops.replay({}, names=())

        self.assertEqual(self.ops.queue_before_replay, {
            "tasks": status["tasks"], "groups": status["groups"]})

    def test_failure_cleanup_accepts_only_positive_failed_inspection(self):
        self.inspection_records(result=None, include_ordinary=False)
        self.ops.tasks = {}
        failed = self.status(self.inspection_row({"Failed": 17}), include_ordinary=False)

        self.assertTrue(self.ops.failure_queue_finished(failed))

        empty = {"tasks": {}, "groups": {}}
        self.assertFalse(self.ops.failure_queue_finished(empty))

        lost_response = deepcopy(failed)
        shutil.rmtree(self.state / "inspections" / TASK)
        self.assertFalse(self.ops.failure_queue_finished(lost_response))

    def test_failure_cleanup_rejects_unrecognized_inspection_result(self):
        self.inspection_records(result="unavailable")
        status = self.status(self.inspection_row("Success"),
                             ordinary=self.ordinary_row("Errored"))

        self.assertFalse(self.ops.failure_queue_finished(status))

    def test_failed_inspection_requires_receipt_and_complete_membership(self):
        directory = self.inspection_records(result=None, include_ordinary=False)
        self.ops.tasks = {}
        status = self.status(self.inspection_row({"Failed": 17}), include_ordinary=False)
        (directory / "receipt.json").unlink()
        self.assertFalse(self.ops.failure_queue_finished(status))

        request_sha = digest(directory / "request.json")
        write_json(directory / "receipt.json", {
            "schema_version": 1, "request_sha256": request_sha, "numeric_task_id": 42,
        })
        incomplete = {"tasks": {}, "groups": status["groups"]}
        self.assertFalse(self.ops.failure_queue_finished(incomplete))

        unknown = self.status(self.inspection_row({"Failed": 17}), include_ordinary=False)
        unknown["tasks"]["99"] = {
            "id": 99, "label": "unknown", "group": "default",
            "original_command": "unknown", "command": "unknown", "path": str(self.state),
            "status": {"Done": {"result": "Success"}},
        }
        self.assertFalse(self.ops.failure_queue_finished(unknown))

    def test_failed_inspection_cannot_coexist_with_ordinary_task(self):
        self.inspection_records(result="unavailable")
        status = self.status(self.inspection_row({"Failed": 17}),
                             ordinary=self.ordinary_row("Errored"))
        self.assertFalse(self.ops.failure_queue_finished(status))

    def test_retain_failure_ownership_waits_for_callers_then_observes_shutdown(self):
        pending = [{"pid": 17, "argv": ["caller"], "directory": "/owned/caller"}]
        drain_calls = []
        self.ops.processes = SimpleNamespace(
            drain=lambda **kwargs: drain_calls.append(kwargs) or
            (pending if len(drain_calls) == 1 else []))
        daemon = SimpleNamespace(poll=lambda: None)
        self.ops.daemon = daemon
        shutdown_calls = []
        self.ops.safe_failure_shutdown = lambda: shutdown_calls.append(True) or True

        with patch("acceptance_provider_common.time.sleep") as sleep:
            self.assertTrue(self.ops.retain_failure_ownership())

        self.assertEqual(len(drain_calls), 2)
        self.assertEqual(drain_calls[0]["exclude"], (daemon,))
        self.assertEqual(shutdown_calls, [True])
        sleep.assert_called_once_with(0.25)

    def test_retain_failure_ownership_does_not_pass_unknown_queue(self):
        self.ops.processes = SimpleNamespace(drain=lambda **_kwargs: [])
        states = iter((None, 0))
        self.ops.daemon = SimpleNamespace(poll=lambda: next(states))
        self.ops.safe_failure_shutdown = lambda: False

        with patch("acceptance_provider_common.time.sleep") as sleep, \
                patch("acceptance_provider_common.time.monotonic",
                      side_effect=(10.0, 10.1, 10.2)):
            self.assertFalse(self.ops.retain_failure_ownership())

        self.assertEqual(sleep.call_count, 1)
        self.assertGreater(sleep.call_args.args[0], 0.0)
        self.assertAlmostEqual(sleep.call_args.args[0], 0.15, places=3)

    def test_retain_failure_ownership_retries_failed_completion_receipt(self):
        class FakePopen:
            def __init__(self, *_args, **_kwargs):
                self.pid = 702
                self.returncode = None

            def poll(self):
                return self.returncode

            def wait(self):
                return self.returncode

        original_write_json = supervisor_common.write_json
        failed = True

        def fail_first_completion(path, *args, **kwargs):
            nonlocal failed
            if Path(path).name == "completed.json" and failed:
                failed = False
                raise OSError("injected completion receipt failure")
            return original_write_json(path, *args, **kwargs)

        processes = supervisor_common.Processes(self.base / "processes", {"PATH": "/usr/bin"})
        daemon = SimpleNamespace(poll=lambda: None)
        ops = NativeTaskOps(processes, self.base / "delegate", self.runner, self.pueue,
                            self.state_parent, self.state, self.output)
        ops.bind_supervisor(self.config, daemon)
        ops.safe_failure_shutdown = lambda: True

        with patch.object(supervisor_common.subprocess, "Popen", FakePopen), \
                patch.object(supervisor_common, "write_json", side_effect=fail_first_completion):
            process = processes.start("caller", ["/bin/sh"], self.base)
            process.process.returncode = 0
            with patch("acceptance_provider_common.time.sleep") as sleep:
                self.assertTrue(ops.retain_failure_ownership())

        self.assertFalse(failed)
        self.assertTrue((process.directory / "completed.json").is_file())
        sleep.assert_called_once_with(0.25)


if __name__ == "__main__":
    unittest.main()
