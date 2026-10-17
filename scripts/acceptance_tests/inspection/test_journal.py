"""Hermetic inspection queue and journal checks for the native acceptance harness."""

from __future__ import annotations

from copy import deepcopy
from pathlib import Path
from types import SimpleNamespace
import shlex
import shutil
import unittest
from unittest.mock import Mock, patch

from scripts import acceptance_inspection as inspection_acceptance
from scripts import acceptance_provider_common as provider_common
from scripts import acceptance_supervisor_common as supervisor_common
from scripts.acceptance_provider_common import AcceptanceFailure, NativeTaskOps, _inspection_group
from scripts.acceptance_supervisor_common import digest, read_json, write_json


ROOT = "a" * 32
TASK = "b" * 32


from scripts.delegate_acceptance.tasks import operations as task_operations
from scripts.delegate_acceptance.processes import operations as process_operations
from .fixtures import InspectionFixture


class InspectionHarnessTests(unittest.TestCase):
    def setUp(self):
        self.fixture = InspectionFixture(self)

    def test_inspection_expectation_rejects_late_and_conflicting_registration(self):
        directory = self.fixture.inspection_records()
        binding = read_json(directory / "request.json")["binding"]

        self.fixture.ops.dispatch_attempts.discard(TASK)
        conflicting = deepcopy(binding)
        conflicting["helper_executable"] = str(self.fixture.base / "other-helper")
        with self.assertRaisesRegex(AcceptanceFailure, "different source"):
            self.fixture.ops.expect_inspection(TASK, conflicting)

        self.fixture.ops.dispatch_attempts.add(TASK)
        with self.assertRaisesRegex(AcceptanceFailure, "precede dispatch"):
            self.fixture.ops.expect_inspection(TASK, binding)

    def test_failed_journal_recomputes_embedded_task_digest_without_ordinary_task(self):
        directory = self.fixture.inspection_records(result=None, include_ordinary=False)
        request = read_json(directory / "request.json")
        request["task_sha256"] = "d" * 64
        write_json(directory / "request.json", request, replace=True)
        self.fixture.refresh_request_links(directory)
        self.fixture.ops.tasks = {}
        status = self.fixture.status(self.fixture.inspection_row({"Failed": 17}), include_ordinary=False)
        self.assertFalse(self.fixture.ops.failure_queue_finished(status))

    def test_model_discovery_journal_requires_complete_standalone_proof(self):
        directory = self.fixture.inspection_records(include_ordinary=False,
                                            revision="model-discovery-v1")
        records = self.fixture.ops.validate_inspection_journals()
        self.assertEqual(set(records), {TASK})
        self.assertFalse((self.fixture.state / "tasks").exists())
        (directory / "worker-observation.json").unlink()
        with self.assertRaisesRegex(AcceptanceFailure, "proof"):
            self.fixture.ops.validate_inspection_journals()

    def test_model_discovery_rejects_an_ordinary_task(self):
        self.fixture.inspection_records(revision="model-discovery-v1")
        with self.assertRaisesRegex(AcceptanceFailure, "ordinary task"):
            self.fixture.ops.validate_inspection_journals()

    def test_model_discovery_requires_its_exact_deadline(self):
        directory = self.fixture.inspection_records(include_ordinary=False,
                                            revision="model-discovery-v1")
        request = read_json(directory / "request.json")
        request["deadline"] = "2026-09-15T00:00:20Z"
        write_json(directory / "request.json", request, replace=True)
        self.fixture.refresh_request_links(directory)
        with self.assertRaisesRegex(AcceptanceFailure, "300 seconds"):
            self.fixture.ops.validate_inspection_journals()

    def test_model_discovery_accepts_legacy_one_minute_journal(self):
        self.fixture.inspection_records(include_ordinary=False, revision="model-discovery-v1",
                                legacy_model_discovery=True)
        records = self.fixture.ops.validate_inspection_journals()
        self.assertEqual(set(records), {TASK})

    def test_inspection_request_requires_exact_deadline_window(self):
        directory = self.fixture.inspection_records(result=None, include_ordinary=False)
        request = read_json(directory / "request.json")
        request["deadline"] = "2026-09-15T00:00:40Z"
        write_json(directory / "request.json", request, replace=True)
        self.fixture.refresh_request_links(directory)
        self.fixture.ops.tasks = {}
        status = self.fixture.status(self.fixture.inspection_row({"Failed": 17}), include_ordinary=False)
        self.assertFalse(self.fixture.ops.failure_queue_finished(status))

    def test_inspection_request_accepts_legacy_twenty_second_deadline(self):
        self.fixture.inspection_records(legacy_admission=True)
        records = self.fixture.ops.validate_inspection_journals()
        self.assertEqual(set(records), {TASK})

    def test_inspection_request_cannot_predate_root_creation(self):
        self.fixture.inspection_records()
        root = read_json(self.fixture.state / "root.json")
        root["created_at"] = "2026-09-15T00:00:01Z"
        write_json(self.fixture.state / "root.json", root, replace=True)

        self.assertFalse(self.fixture.ops.failure_queue_finished(self.fixture.status(self.fixture.inspection_row())))

    def test_root_creation_timestamp_remains_pinned_across_observations(self):
        self.fixture.inspection_records()
        self.fixture.durable_ordinary_admission()
        status = self.fixture.status(self.fixture.inspection_row())
        self.assertTrue(self.fixture.ops.failure_queue_finished(status))
        root = read_json(self.fixture.state / "root.json")
        original = root["created_at"]
        for changed in ("2026-09-14T23:59:59Z", "2026-09-15T00:00:01Z"):
            with self.subTest(created_at=changed):
                root["created_at"] = changed
                write_json(self.fixture.state / "root.json", root, replace=True)
                self.assertFalse(self.fixture.ops.failure_queue_finished(status))
        root["created_at"] = original
        write_json(self.fixture.state / "root.json", root, replace=True)
        self.assertTrue(self.fixture.ops.failure_queue_finished(status))

    def test_inspection_claim_timestamps_follow_request_and_each_other(self):
        directory = self.fixture.inspection_records()
        submission = read_json(directory / "submission.json")
        submission["created_at"] = "2026-09-15T00:00:03Z"
        write_json(directory / "submission.json", submission, replace=True)
        self.assertFalse(self.fixture.ops.failure_queue_finished(self.fixture.status(self.fixture.inspection_row())))

        submission["created_at"] = "2026-09-15T00:00:01Z"
        write_json(directory / "submission.json", submission, replace=True)
        completion = read_json(directory / "completion.json")
        completion["completed_at"] = "2026-09-15T00:00:01Z"
        write_json(directory / "completion.json", completion, replace=True)
        observation = read_json(directory / "worker-observation.json")
        observation["completion_sha256"] = digest(directory / "completion.json")
        write_json(directory / "worker-observation.json", observation, replace=True)
        self.assertFalse(self.fixture.ops.failure_queue_finished(self.fixture.status(self.fixture.inspection_row())))

    def test_inspection_cleanup_requires_submission_and_conditional_start_guards(self):
        directory = self.fixture.inspection_records()
        (directory / "submission.json").unlink()
        self.assertFalse(self.fixture.ops.failure_queue_finished(self.fixture.status(self.fixture.inspection_row())))

        shutil.rmtree(directory)
        shutil.rmtree(self.fixture.state / "tasks" / TASK)
        directory = self.fixture.inspection_records()
        (directory / "start.json").unlink()
        self.assertFalse(self.fixture.ops.failure_queue_finished(self.fixture.status(self.fixture.inspection_row())))

    def test_inspection_binding_must_match_expected_source(self):
        directory = self.fixture.inspection_records()
        request = read_json(directory / "request.json")
        self.fixture.ops.inspection_bindings[TASK] = deepcopy(request["binding"])
        self.fixture.ops._validate_queue_membership(self.fixture.status(self.fixture.inspection_row()), False)

        request["binding"]["helper_executable"] = str(self.fixture.base / "foreign-helper")
        write_json(directory / "request.json", request, replace=True)
        self.fixture.refresh_request_links(directory)
        with self.assertRaisesRegex(AcceptanceFailure, "binding"):
            self.fixture.ops._validate_queue_membership(self.fixture.status(self.fixture.inspection_row()), False)

    def test_native_binding_mode_reaches_failure_cleanup_and_replay(self):
        self.fixture.inspection_records()
        self.fixture.durable_ordinary_admission()
        self.fixture.ops.inspection_bindings.clear()
        status = self.fixture.status(self.fixture.inspection_row())
        self.assertFalse(self.fixture.ops.failure_queue_finished(status))

        self.fixture.ops = NativeTaskOps(
            SimpleNamespace(), self.fixture.base / "delegate", self.fixture.runner, self.fixture.pueue,
            self.fixture.state_parent, self.fixture.state, self.fixture.output,
            expected_inspection_binding_required=False)
        self.fixture.ops.bind_supervisor(self.fixture.config)
        self.fixture.ops.root_id = ROOT
        self.fixture.ops.tasks = {"fresh": TASK}
        self.fixture.ops.labels = {"fresh": f"delegate:{ROOT}:{TASK}"}
        self.fixture.ops.numbers = {"fresh": 7}
        self.fixture.ops.dispatch_attempts = {TASK}
        self.fixture.ops.inspection_attempts = {TASK}
        self.assertTrue(self.fixture.ops.failure_queue_finished(status))

        ordinary = self.fixture.state / "tasks" / TASK
        result = b"replay proof\n"
        payload = {"basename": "result.txt", "length": len(result),
                   "sha256": provider_common.sha(result)}
        outcome = {"verdict": "committed", "payload": payload,
                   "evidence_sha256": "a" * 64}
        (ordinary / "result.txt").write_bytes(result)
        write_json(ordinary / "outcome.json", outcome)
        response = {"task_id": TASK, "outcome": outcome, "payload": payload,
                    "evidence_sha256": outcome["evidence_sha256"]}
        self.fixture.ops.queue_status = lambda _name: status
        self.fixture.ops.collect = lambda _name, task: dict(response, task_id=task)

        self.fixture.ops.replay({
            "fresh": {"directory": ordinary, "outcome": outcome, "payload": payload},
        }, names=("fresh",))

    def test_fixture_binding_uses_static_definition_and_supervisor_sources(self):
        helper = self.fixture.base / "inspection-helper"
        helper_config = self.fixture.base / "helper.json"
        helper.write_bytes(b"fixture helper")
        helper.chmod(0o700)
        helper_config.write_bytes(b"{\"schema_version\":1}\n")
        helper_config.chmod(0o600)
        environment = {"FIXTURE": "yes", "LANG": "C"}
        supervisor = provider_common.supervisor_binding(
            self.fixture.pueue, self.fixture.config, self.fixture.base, digest(self.fixture.config), "pueue fixture")
        binding = inspection_acceptance.fixture_inspection_binding(
            helper, helper_config, self.fixture.base / "workspace", environment,
            self.fixture.runner, supervisor)

        self.assertEqual(binding["definition_revision"], "inspection-fixture-v1")
        self.assertEqual(binding["helper_executable"], str(helper.resolve()))
        self.assertEqual(binding["worker_executable"], str(self.fixture.runner.resolve()))
        self.assertEqual(binding["supervisor"], supervisor)
        self.assertEqual(binding["supervisor"]["endpoint"],
                         "unix:" + str((self.fixture.base / "run" / "p.sock").resolve()))
        self.assertEqual(binding["supervisor"]["config_digest"], digest(self.fixture.config))

        changed_environment = dict(environment, FIXTURE="changed")
        changed = inspection_acceptance.fixture_inspection_binding(
            helper, helper_config, self.fixture.base / "workspace", changed_environment,
            self.fixture.runner, supervisor)
        self.assertNotEqual(binding["definition_sha256"], changed["definition_sha256"])

        changed_helper_config = self.fixture.base / "other-helper.json"
        changed_helper_config.write_bytes(helper_config.read_bytes())
        changed_helper_config.chmod(0o600)
        changed_path = inspection_acceptance.fixture_inspection_binding(
            helper, changed_helper_config, self.fixture.base / "workspace", environment,
            self.fixture.runner, supervisor)
        self.assertNotEqual(binding["definition_sha256"], changed_path["definition_sha256"])

    def test_legacy_supervisor_binding_accepts_cwd_only_resolution(self):
        binding = provider_common.supervisor_binding(
            self.fixture.pueue, self.fixture.config, self.fixture.base, digest(self.fixture.config), "pueue fixture")
        binding["observed_version"] = "pueue 99.1.0"
        provider_common._validate_supervisor_binding(binding, "future supervisor")
        for key in ("resolution_os", "resolution_home", "resolution_data_local",
                    "resolution_config", "resolution_runtime", "resolution_username"):
            binding.pop(key, None)
        binding["resolution_cwd"] = str(self.fixture.base)

        provider_common._validate_supervisor_binding(binding, "legacy supervisor")

    def test_noneligible_inspection_facts_and_exit_must_be_empty_and_unavailable(self):
        directory = self.fixture.inspection_records(result="unavailable")
        result_path = directory / "result.json"
        result = read_json(result_path)
        result["facts"] = {"eligible": False}
        write_json(result_path, result, replace=True)
        completion = read_json(directory / "completion.json")
        completion["result_sha256"] = digest(result_path)
        write_json(directory / "completion.json", completion, replace=True)
        self.assertFalse(self.fixture.ops.failure_queue_finished(self.fixture.status(self.fixture.inspection_row())))

        shutil.rmtree(directory)
        shutil.rmtree(self.fixture.state / "tasks" / TASK)
        directory = self.fixture.inspection_records(result="unavailable", include_ordinary=False)
        completion = read_json(directory / "completion.json")
        completion["native_exit"] = "successful-exit"
        write_json(directory / "completion.json", completion, replace=True)
        self.assertFalse(self.fixture.ops.failure_queue_finished(
            self.fixture.status(self.fixture.inspection_row({"Failed": 17}), include_ordinary=False)))

    def test_inspection_result_requires_completion_record(self):
        directory = self.fixture.inspection_records(result="unavailable")
        (directory / "completion.json").unlink()
        self.assertFalse(self.fixture.ops.failure_queue_finished(self.fixture.status(self.fixture.inspection_row())))

    def test_ordinary_attempt_without_inspection_journal_is_allowed(self):
        self.fixture.state.joinpath("inspections").mkdir(mode=0o700)
        self.fixture.ops._validate_queue_membership({
            "tasks": {"7": self.fixture.ordinary_row()},
            "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
        }, False)

    def test_declared_inspection_without_journal_is_rejected(self):
        directory = self.fixture.inspection_records(result=None)
        shutil.rmtree(directory)
        self.assertFalse(self.fixture.ops.failure_queue_finished({
            "tasks": {"7": self.fixture.ordinary_row()},
            "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
        }))

    def test_mixed_attempts_allow_ordinary_task_without_inspection(self):
        other = "c" * 32
        self.fixture.ops.tasks["resume"] = other
        self.fixture.ops.labels["resume"] = f"delegate:{ROOT}:{other}"
        self.fixture.ops.numbers["resume"] = 8
        self.fixture.ops.dispatch_attempts.add(other)
        self.fixture.inspection_records()
        status = self.fixture.status(self.fixture.inspection_row())
        status["tasks"]["8"] = self.fixture.ordinary_row(name="resume", task=other, number=8)

        self.fixture.ops._validate_queue_membership(status, False)

    def test_queue_rejects_unknown_inspection_row_and_wrong_argv(self):
        self.fixture.inspection_records()
        status = self.fixture.status(self.fixture.inspection_row())
        unknown = deepcopy(status)
        unknown["tasks"]["99"] = {
            "id": 99, "label": "delegation-inspection-" + ROOT + "-" + "c" * 32,
            "group": _inspection_group(ROOT), "original_command": "unknown",
            "command": "unknown", "path": str(self.fixture.state),
            "status": {"Done": {"result": "Success"}},
        }
        with self.assertRaisesRegex(AcceptanceFailure, "membership|labels"):
            self.fixture.ops._validate_queue_membership(unknown, False)

        wrong_argv = deepcopy(status)
        wrong_argv["tasks"]["42"]["command"] = shlex.join(
            [str(self.fixture.runner), "--root", str(self.fixture.state), TASK])
        with self.assertRaisesRegex(AcceptanceFailure, "argv"):
            self.fixture.ops._validate_queue_membership(wrong_argv, False)

    def test_queue_rejects_wrong_ordinary_id_group_and_argv(self):
        for mutation, message in (
                (lambda row: row.update(id=8), "numeric"),
                (lambda row: row.update(group="foreign"), "group"),
                (lambda row: row.update(command="/bin/true"), "exact")):
            with self.subTest(message=message):
                row = self.fixture.ordinary_row()
                mutation(row)
                with self.assertRaisesRegex(AcceptanceFailure, "identity|group|exact|runner"):
                    self.fixture.ops._validate_queue_membership({
                        "tasks": {"7": row},
                        "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
                    }, False)

    def test_queue_rejects_foreign_groups_even_without_inspection_rows(self):
        self.fixture.state.joinpath("inspections").mkdir(mode=0o700)
        status = {
            "tasks": {"7": self.fixture.ordinary_row()},
            "groups": {
                "default": {"status": "Running", "parallel_tasks": 1},
                "foreign": {"status": "Running", "parallel_tasks": 1},
            },
        }
        with self.assertRaisesRegex(AcceptanceFailure, "unknown supervisor group"):
            self.fixture.ops._validate_queue_membership(status, False)

    def test_ordinary_failure_cleanup_binds_exact_id_label_and_default_group(self):
        self.fixture.inspection_records()
        self.fixture.durable_ordinary_admission()
        shutil.rmtree(self.fixture.state / "inspections")
        self.fixture.ops.inspection_attempts.clear()
        self.fixture.ops.inspection_bindings.clear()
        status = {
            "tasks": {"7": self.fixture.ordinary_row()},
            "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
        }
        self.assertTrue(self.fixture.ops.failure_queue_finished(status))

        wrong_id = deepcopy(status)
        wrong_id["tasks"]["7"]["id"] = 8
        self.assertFalse(self.fixture.ops.failure_queue_finished(wrong_id))

        foreign_group = deepcopy(status)
        foreign_group["tasks"]["7"]["group"] = "foreign"
        self.assertFalse(self.fixture.ops.failure_queue_finished(foreign_group))

        duplicate_label = deepcopy(status)
        duplicate_label["tasks"]["8"] = deepcopy(duplicate_label["tasks"]["7"])
        duplicate_label["tasks"]["8"]["id"] = 8
        self.assertFalse(self.fixture.ops.failure_queue_finished(duplicate_label))

    def test_status_observation_requires_successful_natural_exit_and_strict_schema(self):
        directory = self.fixture.base / "status-strict"
        directory.mkdir(mode=0o700)
        write_json(directory / "stdout", {"tasks": {}, "groups": {}})
        process = SimpleNamespace(directory=directory,
                                  result={"exit_code": 1, "natural_wait": True})
        with self.assertRaisesRegex(AcceptanceFailure, "did not exit successfully"):
            provider_common.status_jobs(process)

        process.result = {"exit_code": 0, "natural_wait": True}
        write_json(directory / "stdout", {"Tasks": {}, "groups": {}}, replace=True)
        with self.assertRaisesRegex(AcceptanceFailure, "missing fields"):
            provider_common.status_jobs(process)

    def test_status_observation_rejects_nonstandard_json_constants(self):
        directory = self.fixture.base / "status-constants"
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
        directory = self.fixture.base / "status-exit-type"
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
        self.fixture.ops.processes = Mock(run=process)
        self.fixture.ops.closed = True

        with self.assertRaisesRegex(AcceptanceFailure, "supervisor is closed"):
            self.fixture.ops.direct("late-direct", ["delegate"])
        with self.assertRaisesRegex(AcceptanceFailure, "supervisor is closed"):
            self.fixture.ops.dispatch("late-dispatch", "c" * 32, ["delegate"])

        process.assert_not_called()

    def test_control_decoder_rejects_simple_fold_alias(self):
        directory = self.fixture.base / "alias"
        directory.mkdir(mode=0o700)
        (directory / "stdout").write_text('{"tasks":{},"Tasks":{}}')
        process = SimpleNamespace(directory=directory,
                                  result={"exit_code": 0, "natural_wait": True})
        with self.assertRaisesRegex(AcceptanceFailure, "duplicate JSON object key"):
            provider_common.status_jobs(process)

    def test_replay_preserves_inspection_journal_and_queue(self):
        self.fixture.inspection_records()
        status = self.fixture.status(self.fixture.inspection_row())
        self.fixture.ops.queue_status = lambda _name: deepcopy(status)

        self.fixture.ops.replay({}, names=())

        self.assertEqual(self.fixture.ops.queue_before_replay, {
            "tasks": status["tasks"], "groups": status["groups"]})

    def test_failure_cleanup_accepts_only_positive_failed_inspection(self):
        self.fixture.inspection_records(result=None, include_ordinary=False)
        self.fixture.ops.tasks = {}
        failed = self.fixture.status(self.fixture.inspection_row({"Failed": 17}), include_ordinary=False)

        self.assertTrue(self.fixture.ops.failure_queue_finished(failed))

        empty = {"tasks": {}, "groups": {}}
        self.assertFalse(self.fixture.ops.failure_queue_finished(empty))

        lost_response = deepcopy(failed)
        shutil.rmtree(self.fixture.state / "inspections" / TASK)
        self.assertFalse(self.fixture.ops.failure_queue_finished(lost_response))

    def test_failure_cleanup_recovers_durable_admission_after_response_loss(self):
        self.fixture.inspection_records()
        self.fixture.durable_ordinary_admission()
        status = self.fixture.status(self.fixture.inspection_row(), ordinary=self.fixture.ordinary_row())
        self.fixture.ops.tasks = {}
        self.fixture.ops.labels = {}
        self.fixture.ops.numbers = {}

        self.assertTrue(self.fixture.ops.failure_queue_finished(status))

    def test_failure_cleanup_requires_exact_durable_supervisor_receipt(self):
        self.fixture.inspection_records()
        self.fixture.durable_ordinary_admission()
        status = self.fixture.status(self.fixture.inspection_row(), ordinary=self.fixture.ordinary_row())

        self.assertTrue(self.fixture.ops.failure_queue_finished(status))
        receipt = self.fixture.state / "tasks" / TASK / "supervisor.ref.json"
        receipt_record = read_json(receipt)
        receipt_record["resolution_cwd"] = str(self.fixture.base)
        write_json(receipt, receipt_record, replace=True)
        self.assertFalse(self.fixture.ops.failure_queue_finished(status))

        del receipt_record["resolution_cwd"]
        write_json(receipt, receipt_record, replace=True)
        self.assertTrue(self.fixture.ops.failure_queue_finished(status))
        receipt.unlink()
        self.assertFalse(self.fixture.ops.failure_queue_finished(status))

    def test_failure_cleanup_rejects_task_hash_drift(self):
        self.fixture.inspection_records()
        self.fixture.durable_ordinary_admission()
        status = self.fixture.status(self.fixture.inspection_row(), ordinary=self.fixture.ordinary_row())
        self.fixture.ops.tasks = {}
        self.fixture.ops.labels = {}
        self.fixture.ops.numbers = {}
        task_path = self.fixture.state / "tasks" / TASK / "task.json"
        task_record = read_json(task_path)
        task_record["requested_config"]["budget"] = "2m0s"
        task_path.write_bytes(provider_common._canonical_task_bytes(task_record))

        self.assertFalse(self.fixture.ops.failure_queue_finished(status))

    def test_failure_cleanup_recovers_durable_admission_without_inspection(self):
        self.fixture.inspection_records()
        self.fixture.durable_ordinary_admission()
        shutil.rmtree(self.fixture.state / "inspections")
        self.fixture.ops.inspection_attempts.clear()
        self.fixture.ops.inspection_bindings.clear()
        status = {
            "tasks": {"7": self.fixture.ordinary_row()},
            "groups": {"default": {"status": "Running", "parallel_tasks": 1}},
        }
        self.fixture.ops.tasks = {}
        self.fixture.ops.labels = {}
        self.fixture.ops.numbers = {}

        self.assertTrue(self.fixture.ops.failure_queue_finished(status))

        task_path = self.fixture.state / "tasks" / TASK / "task.json"
        task_record = read_json(task_path)
        task_record["requested_config"]["budget"] = "2m0s"
        task_path.write_bytes(provider_common._canonical_task_bytes(task_record))
        self.assertFalse(self.fixture.ops.failure_queue_finished(status))

    def test_failure_cleanup_rejects_unrecognized_inspection_result(self):
        self.fixture.inspection_records(result="unavailable")
        status = self.fixture.status(self.fixture.inspection_row("Success"),
                             ordinary=self.fixture.ordinary_row("Errored"))

        self.assertFalse(self.fixture.ops.failure_queue_finished(status))

    def test_failed_inspection_requires_receipt_and_complete_membership(self):
        directory = self.fixture.inspection_records(result=None, include_ordinary=False)
        self.fixture.ops.tasks = {}
        status = self.fixture.status(self.fixture.inspection_row({"Failed": 17}), include_ordinary=False)
        (directory / "receipt.json").unlink()
        self.assertFalse(self.fixture.ops.failure_queue_finished(status))

        request_sha = digest(directory / "request.json")
        write_json(directory / "receipt.json", {
            "schema_version": 1, "request_sha256": request_sha, "numeric_task_id": 42,
        })
        incomplete = {"tasks": {}, "groups": status["groups"]}
        self.assertFalse(self.fixture.ops.failure_queue_finished(incomplete))

        unknown = self.fixture.status(self.fixture.inspection_row({"Failed": 17}), include_ordinary=False)
        unknown["tasks"]["99"] = {
            "id": 99, "label": "unknown", "group": "default",
            "original_command": "unknown", "command": "unknown", "path": str(self.fixture.state),
            "status": {"Done": {"result": "Success"}},
        }
        self.assertFalse(self.fixture.ops.failure_queue_finished(unknown))

    def test_failed_inspection_cannot_coexist_with_ordinary_task(self):
        self.fixture.inspection_records(result="unavailable")
        status = self.fixture.status(self.fixture.inspection_row({"Failed": 17}),
                             ordinary=self.fixture.ordinary_row("Errored"))
        self.assertFalse(self.fixture.ops.failure_queue_finished(status))

    def test_retain_failure_ownership_waits_for_callers_then_observes_shutdown(self):
        pending = [{"pid": 17, "argv": ["caller"], "directory": "/owned/caller"}]
        drain_calls = []
        self.fixture.ops.processes = SimpleNamespace(
            drain=lambda **kwargs: drain_calls.append(kwargs) or
            (pending if len(drain_calls) == 1 else []))
        daemon = SimpleNamespace(poll=lambda: None)
        self.fixture.ops.daemon = daemon
        shutdown_calls = []
        self.fixture.ops.safe_failure_shutdown = lambda: shutdown_calls.append(True) or True

        with patch.object(task_operations.time, "sleep") as sleep:
            self.assertTrue(self.fixture.ops.retain_failure_ownership())

        self.assertEqual(len(drain_calls), 2)
        self.assertEqual(drain_calls[0]["exclude"], (daemon,))
        self.assertEqual(shutdown_calls, [True])
        sleep.assert_called_once_with(0.25)

    def test_retain_failure_ownership_stops_at_finite_observation_deadline(self):
        pending = [{"pid": 17, "argv": ["caller"], "directory": "/owned/caller"}]
        self.fixture.ops.processes = SimpleNamespace(drain=lambda **_kwargs: pending)
        self.fixture.ops.daemon = SimpleNamespace(poll=lambda: None)

        with patch.object(task_operations, "failure_cleanup_deadline", return_value=10.0), \
                patch.object(task_operations, "wait_for_failure_cleanup", return_value=False):
            self.assertFalse(self.fixture.ops.retain_failure_ownership())

        receipt = read_json(self.fixture.output / "failure-cleanup-expired.json")
        self.assertEqual(receipt["reason"],
                         "owned processes or private queue did not finish naturally")
        self.assertEqual(receipt["signals_sent"], 0)

    def test_retain_failure_ownership_does_not_pass_unknown_queue(self):
        self.fixture.ops.processes = SimpleNamespace(drain=lambda **_kwargs: [])
        states = iter((None, 0))
        self.fixture.ops.daemon = SimpleNamespace(poll=lambda: next(states))
        self.fixture.ops.safe_failure_shutdown = lambda: False

        with patch.object(task_operations.time, "sleep") as sleep, \
                patch.object(task_operations.time, "monotonic",
                      side_effect=(10.0, 10.1, 10.2, 10.3, 10.4)):
            self.assertFalse(self.fixture.ops.retain_failure_ownership())

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

        original_write_json = process_operations.write_json
        failed = True

        def fail_first_completion(path, *args, **kwargs):
            nonlocal failed
            if Path(path).name == "completed.json" and failed:
                failed = False
                raise OSError("injected completion receipt failure")
            return original_write_json(path, *args, **kwargs)

        processes = supervisor_common.Processes(self.fixture.base / "processes", {"PATH": "/usr/bin"})
        daemon = SimpleNamespace(poll=lambda: None)
        ops = NativeTaskOps(processes, self.fixture.base / "delegate", self.fixture.runner, self.fixture.pueue,
                            self.fixture.state_parent, self.fixture.state, self.fixture.output)
        ops.bind_supervisor(self.fixture.config, daemon)
        ops.safe_failure_shutdown = lambda: True

        with patch.object(process_operations.subprocess, "Popen", FakePopen), \
                patch.object(process_operations, "write_json", side_effect=fail_first_completion):
            process = processes.start("caller", ["/bin/sh"], self.fixture.base)
            process.process.returncode = 0
            with patch.object(task_operations.time, "sleep") as sleep:
                self.assertTrue(ops.retain_failure_ownership())

        self.assertFalse(failed)
        self.assertTrue((process.directory / "completed.json").is_file())
        sleep.assert_called_once_with(0.25)


if __name__ == "__main__":
    unittest.main()
