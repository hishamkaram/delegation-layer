"""Hermetic inspection queue and journal checks for the native acceptance harness."""

from __future__ import annotations

from copy import deepcopy
from pathlib import Path
from types import SimpleNamespace
import shutil
import unittest

from scripts import acceptance_native as native_acceptance
from scripts import acceptance_provider_common as provider_common
from scripts.acceptance_provider_common import AcceptanceFailure
from scripts.acceptance_supervisor_common import digest, read_json, write_json


ROOT = "a" * 32
TASK = "b" * 32


from .fixtures import InspectionFixture


class InspectionHarnessTests(unittest.TestCase):
    def setUp(self):
        self.fixture = InspectionFixture(self)

    def test_shutdown_requires_complete_success_proof_for_inspection(self):
        self.fixture.inspection_records()
        status = self.fixture.status(self.fixture.inspection_row())
        calls = []
        self.fixture.ops.queue_status = lambda _name: status
        self.fixture.ops.processes = SimpleNamespace(drain=lambda **_kwargs: [])
        self.fixture.ops.client = lambda name, operation, **_kwargs: calls.append((name, operation))
        self.fixture.ops.daemon = SimpleNamespace(wait=lambda **_kwargs: None)

        self.fixture.ops.shutdown()

        self.assertTrue(self.fixture.ops.closed)
        self.assertEqual(calls, [("private-shutdown", ["shutdown"])])

    def test_successful_shutdown_rejects_missing_worker_observation(self):
        directory = self.fixture.inspection_records()
        (directory / "worker-observation.json").unlink()
        with self.assertRaisesRegex(AcceptanceFailure, "proof"):
            self.fixture.ops._validate_queue_membership(self.fixture.status(self.fixture.inspection_row()), False)

    def test_inspection_expectation_requires_full_binding_before_dispatch(self):
        self.fixture.ops.dispatch_attempts.clear()

        with self.assertRaisesRegex(AcceptanceFailure, "binding is required"):
            self.fixture.ops.expect_inspection(TASK)

    def test_inspection_binding_accepts_only_the_guardian_exit_proof(self):
        directory = self.fixture.inspection_records()
        binding = read_json(directory / "request.json")["binding"]
        binding["worker_exit_proof"] = "guardian-wait-v1"

        provider_common._validate_inspection_binding(binding, "inspection binding")

        binding["worker_exit_proof"] = "untrusted-wait"
        with self.assertRaisesRegex(AcceptanceFailure, "worker exit proof is invalid"):
            provider_common._validate_inspection_binding(binding, "inspection binding")

    def test_inspection_binding_validates_paired_daemon_provenance(self):
        directory = self.fixture.inspection_records()
        binding = read_json(directory / "request.json")["binding"]
        supervisor = binding["supervisor"]
        supervisor["daemon_executable"] = "/fixture/pueued"
        supervisor["daemon_sha256"] = "a" * 64

        provider_common._validate_inspection_binding(binding, "inspection binding")

        del supervisor["daemon_sha256"]
        with self.assertRaisesRegex(AcceptanceFailure, "daemon provenance is incomplete"):
            provider_common._validate_inspection_binding(binding, "inspection binding")

    def test_managed_guardian_wait_replaces_queue_worker_observation(self):
        directory = self.fixture.inspection_records(managed_exit_proof=True)

        records = self.fixture.ops.validate_inspection_journals()

        self.assertFalse((directory / "worker-observation.json").exists())
        proof = records[TASK]["managed_lifetime"]
        self.assertEqual(proof["worker_wait"]["result"], "success")
        self.assertTrue(proof["termination"]["scope_empty"])

    def test_root_lifetime_closure_rejects_inspection_only_unfinished_state(self):
        directory = self.fixture.inspection_records(include_ordinary=False, managed_exit_proof=True)
        (directory / "lifetime-worker-wait.json").unlink()

        with self.assertRaisesRegex(AcceptanceFailure, "inspection lifetime worker Wait"):
            provider_common.validate_managed_lifetime_closure(self.fixture.state)

    def test_root_lifetime_closure_accepts_expired_inspection_without_native_facts(self):
        directory = self.fixture.unlaunched_inspection()
        closure = provider_common.validate_managed_lifetime_closure(self.fixture.state)
        proof = closure["inspections"][TASK]
        self.assertEqual(proof["mode"], "unlaunched")
        self.assertEqual(proof["unlaunched_sha256"],
                         digest(directory / "lifetime-unlaunched.json"))
        self.assertNotIn("worker_wait", proof)
        self.assertNotIn("termination", proof)

    def test_root_lifetime_closure_uses_inspection_operation_identity(self):
        operation = "c" * 32
        self.fixture.unlaunched_inspection(operation)
        closure = provider_common.validate_managed_lifetime_closure(self.fixture.state)
        self.assertEqual(set(closure["inspections"]), {operation})
        self.assertEqual(closure["inspections"][operation]["unlaunched"]["task_id"], operation)

    def test_root_lifetime_closure_rejects_unlaunched_native_contradiction(self):
        directory = self.fixture.unlaunched_inspection()
        write_json(directory / "lifetime-intent.json", {})
        with self.assertRaises(AcceptanceFailure):
            provider_common.validate_managed_lifetime_closure(self.fixture.state)

    def test_root_lifetime_closure_rejects_logical_id_as_attempt_fence(self):
        directory = self.fixture.unlaunched_inspection("c" * 32)
        fence = read_json(directory / "lifetime-unlaunched.json")
        fence["task_id"] = TASK
        (directory / "lifetime-unlaunched.json").write_bytes(
            provider_common.canonical_go_json(fence))
        with self.assertRaises(AcceptanceFailure):
            provider_common.validate_managed_lifetime_closure(self.fixture.state)

    def test_root_lifetime_closure_ignores_unsubmitted_managed_proof_reference(self):
        directory = self.fixture.inspection_records(include_ordinary=False, managed_exit_proof=True)
        for entry in tuple(directory.iterdir()):
            if entry.name != "request.json":
                self.assertTrue(entry.is_file())
                entry.unlink()
        write_json(directory / "proof-reference.json", {})

        closure = provider_common.validate_managed_lifetime_closure(self.fixture.state)

        self.assertEqual(closure["inspections"], {})

    def test_root_lifetime_closure_rejects_unsubmitted_unexpected_record(self):
        directory = self.fixture.inspection_records(include_ordinary=False, managed_exit_proof=True)
        for entry in tuple(directory.iterdir()):
            if entry.name != "request.json":
                entry.unlink()
        write_json(directory / "unexpected.json", {})

        with self.assertRaisesRegex(AcceptanceFailure, "unexpected records"):
            provider_common.validate_managed_lifetime_closure(self.fixture.state)

    def test_root_lifetime_closure_rejects_submitted_managed_inspection_without_lifetime(self):
        directory = self.fixture.inspection_records(include_ordinary=False, managed_exit_proof=True)
        for entry in tuple(directory.iterdir()):
            if entry.name.startswith("lifetime-"):
                entry.unlink()

        with self.assertRaisesRegex(AcceptanceFailure, "managed inspection lifetime evidence is absent"):
            provider_common.validate_managed_lifetime_closure(self.fixture.state)

    def test_native_terminal_queue_correlates_helper_with_admitted_runtime(self):
        self.fixture.inspection_records()
        ordinary = self.fixture.durable_ordinary_admission()
        meta_path = ordinary / "meta.json"
        meta = read_json(meta_path)
        meta["effective_config"]["policy"] = {"runtime_sha256": "e" * 64}
        write_json(meta_path, meta, replace=True)
        self.fixture.rebind_ordinary_metadata(ordinary)

        def operation_for(metadata):
            operation = object.__new__(native_acceptance.NativeAcceptance)
            operation.config = self.fixture.config
            operation.daemon = None
            operation.root_id = ROOT
            operation.state = self.fixture.state
            operation.tasks = {"fresh": TASK}
            operation.numeric_ids = {TASK: 7}
            operation.provider_executable = Path(metadata["provider_executable"])
            operation.provider_version = "fixture-v1"
            operation.ops = self.fixture.ops
            return operation

        operation = operation_for(meta)
        operation._record_provider_runtime_identity(TASK, meta)
        status = self.fixture.status(self.fixture.inspection_row())
        self.assertFalse(operation.terminal_private_queue(status))

        meta["provider_executable"] = str(self.fixture.base / "helper")
        write_json(meta_path, meta, replace=True)
        self.fixture.rebind_ordinary_metadata(ordinary)
        operation.provider_executable = Path(meta["provider_executable"])
        with self.assertRaisesRegex(AcceptanceFailure, "changed its admitted provider runtime identity"):
            operation._record_provider_runtime_identity(TASK, meta)
        operation = operation_for(meta)
        operation._record_provider_runtime_identity(TASK, meta)
        self.assertTrue(operation.terminal_private_queue(status))

    def test_root_lifetime_closure_rejects_complete_task_with_incomplete_inspection(self):
        self.fixture.inspection_records(managed_exit_proof=True)
        ordinary = self.fixture.durable_ordinary_admission(runner_ownership="managed")
        self.fixture.managed_ordinary_lifetime(ordinary)
        (self.fixture.state / "inspections" / TASK / "lifetime-termination.json").unlink()

        with self.assertRaisesRegex(AcceptanceFailure, "inspection lifetime termination"):
            provider_common.validate_managed_lifetime_closure(self.fixture.state)

    def test_root_lifetime_closure_accepts_complete_mixed_state(self):
        self.fixture.inspection_records(managed_exit_proof=True)
        ordinary = self.fixture.durable_ordinary_admission(runner_ownership="managed")
        self.fixture.managed_ordinary_lifetime(ordinary)

        closure = provider_common.validate_managed_lifetime_closure(self.fixture.state)

        self.assertEqual(set(closure["tasks"]), {TASK})
        self.assertEqual(set(closure["inspections"]), {TASK})
        self.assertEqual(closure["tasks"][TASK]["worker_wait"]["result"], "success")
        self.assertEqual(closure["inspections"][TASK]["worker_wait"]["result"], "success")

    def test_root_lifetime_closure_ignores_managed_plan_without_supervisor_handoff(self):
        self.fixture.inspection_records()
        ordinary = self.fixture.durable_ordinary_admission(runner_ownership="managed")
        (ordinary / "supervisor.ref.json").unlink()
        shutil.rmtree(self.fixture.state / "inspections")

        closure = provider_common.validate_managed_lifetime_closure(self.fixture.state)

        self.assertEqual(closure["tasks"], {})
        self.assertEqual(closure["inspections"], {})

    def test_root_lifetime_closure_rejects_handoff_without_managed_lifetime(self):
        self.fixture.inspection_records()
        self.fixture.durable_ordinary_admission(runner_ownership="managed")
        shutil.rmtree(self.fixture.state / "inspections")

        with self.assertRaisesRegex(AcceptanceFailure, "ordinary task lifetime"):
            provider_common.validate_managed_lifetime_closure(self.fixture.state)

    def test_root_lifetime_closure_preserves_custom_and_historical_inspections(self):
        for index, options in enumerate((
                {"custom_inspection": True, "include_ordinary": False},
                {"legacy_admission": True, "include_ordinary": False})):
            with self.subTest(options=options):
                if index:
                    shutil.rmtree(self.fixture.state / "inspections")
                    self.fixture.ops.inspection_attempts.clear()
                    self.fixture.ops.inspection_bindings.clear()
                self.fixture.inspection_records(**options)
                closure = provider_common.validate_managed_lifetime_closure(self.fixture.state)
                self.assertEqual(closure["tasks"], {})
                self.assertEqual(closure["inspections"], {})

    def test_unstarted_guardian_requires_a_failed_terminal_queue_row(self):
        self.fixture.inspection_records(result=None, include_ordinary=False,
                                managed_exit_proof=True)
        self.fixture.ops.tasks = {}
        self.fixture.ops.labels = {}
        self.fixture.ops.numbers = {}

        failed = self.fixture.status(self.fixture.inspection_row({"Failed": 1}), include_ordinary=False)
        self.assertTrue(self.fixture.ops.failure_queue_finished(failed))

        succeeded = self.fixture.status(self.fixture.inspection_row(), include_ordinary=False)
        self.assertFalse(self.fixture.ops.failure_queue_finished(succeeded))

    def test_inspection_queue_row_requires_the_control_working_directory(self):
        self.fixture.inspection_records(result=None, include_ordinary=False,
                                managed_exit_proof=True)
        self.fixture.ops.tasks = {}
        self.fixture.ops.labels = {}
        self.fixture.ops.numbers = {}
        row = self.fixture.inspection_row({"Failed": 1})
        row["path"] = str(self.fixture.state)

        self.assertFalse(self.fixture.ops.failure_queue_finished(
            self.fixture.status(row, include_ordinary=False)))

    def test_managed_guardian_wait_rejects_changed_worker_identity(self):
        directory = self.fixture.inspection_records(managed_exit_proof=True)
        wait_path = directory / "lifetime-worker-wait.json"
        wait = read_json(wait_path)
        wait["worker_sha256"] = "0" * 64
        write_json(wait_path, wait, replace=True)

        with self.assertRaisesRegex(AcceptanceFailure, "worker Wait binding mismatch"):
            self.fixture.ops.validate_inspection_journals()

    def test_managed_guardian_proof_survives_failed_or_removed_queue_row(self):
        self.fixture.inspection_records(managed_exit_proof=True)
        self.fixture.durable_ordinary_admission()
        ordinary = self.fixture.ordinary_row()
        self.fixture.ops.tasks = {}
        self.fixture.ops.labels = {}
        self.fixture.ops.numbers = {}

        failed_row = self.fixture.status(self.fixture.inspection_row({"Failed": 17}), ordinary=ordinary)
        self.assertTrue(self.fixture.ops.failure_queue_finished(failed_row))

        removed_row = self.fixture.status(None, ordinary=ordinary)
        self.assertTrue(self.fixture.ops.failure_queue_finished(removed_row))

    def test_partial_managed_guardian_evidence_cannot_fallback_to_queue(self):
        directory = self.fixture.inspection_records(managed_exit_proof=True)
        (directory / "lifetime-worker-wait.json").unlink()

        self.assertFalse(self.fixture.ops.failure_queue_finished(self.fixture.status(self.fixture.inspection_row())))

    def test_custom_inspection_uses_exact_terminal_queue_fallback(self):
        self.fixture.inspection_records(custom_inspection=True)
        self.fixture.durable_ordinary_admission()
        ordinary = self.fixture.ordinary_row()
        self.fixture.ops.tasks = {}
        self.fixture.ops.labels = {}
        self.fixture.ops.numbers = {}

        self.assertTrue(self.fixture.ops.failure_queue_finished(
            self.fixture.status(self.fixture.inspection_row(), ordinary=ordinary)))

    def test_historical_inspection_uses_exact_terminal_queue_fallback(self):
        self.fixture.inspection_records(legacy_admission=True)
        self.fixture.durable_ordinary_admission()
        ordinary = self.fixture.ordinary_row()
        self.fixture.ops.tasks = {}
        self.fixture.ops.labels = {}
        self.fixture.ops.numbers = {}

        self.assertTrue(self.fixture.ops.failure_queue_finished(
            self.fixture.status(self.fixture.inspection_row(), ordinary=ordinary)))

    def test_queue_fallback_rejects_wrong_supervisor_state_identity(self):
        self.fixture.inspection_records()
        status = self.fixture.status(self.fixture.inspection_row())
        status["tasks"]["42"]["path"] = str(self.fixture.base / "foreign-state")

        self.assertFalse(self.fixture.ops.failure_queue_finished(status))

    def test_queue_fallback_rejects_duplicate_terminal_rows(self):
        self.fixture.inspection_records()
        status = self.fixture.status(self.fixture.inspection_row())
        duplicate = deepcopy(status["tasks"]["42"])
        duplicate["id"] = 43
        status["tasks"]["43"] = duplicate

        self.assertFalse(self.fixture.ops.failure_queue_finished(status))

    def test_managed_ordinary_done_failures_without_lifetime_closure_are_retained(self):
        self.fixture.ordinary_only_admission("managed")

        for result in ({"Failed": 17}, "Killed"):
            with self.subTest(result=result):
                self.assertFalse(
                    self.fixture.ops.failure_queue_finished(self.fixture.ordinary_only_status(result)))

    def test_managed_ordinary_missing_mismatched_or_unreadable_wait_is_retained(self):
        directory = self.fixture.ordinary_only_admission("managed")
        lifetime = self.fixture.managed_ordinary_lifetime(directory, wait_result="exit-error")
        wait_path = lifetime / "worker-wait.json"
        valid_wait = read_json(wait_path)
        status = self.fixture.ordinary_only_status({"Failed": 17})

        wait_path.unlink()
        self.assertFalse(self.fixture.ops.failure_queue_finished(status))

        write_json(wait_path, valid_wait)
        mismatched = deepcopy(valid_wait)
        mismatched["worker_sha256"] = "0" * 64
        write_json(wait_path, mismatched, replace=True)
        self.assertFalse(self.fixture.ops.failure_queue_finished(status))

        wait_path.write_text("{", encoding="utf-8")
        self.assertFalse(self.fixture.ops.failure_queue_finished(status))

    def test_managed_ordinary_valid_closure_allows_failed_cleanup(self):
        directory = self.fixture.ordinary_only_admission("managed")
        self.fixture.managed_ordinary_lifetime(directory, wait_result="exit-error")

        self.assertTrue(self.fixture.ops.failure_queue_finished(
            self.fixture.ordinary_only_status({"Failed": 17})))

    def test_managed_ordinary_valid_closure_allows_successful_shutdown(self):
        directory = self.fixture.ordinary_only_admission("managed")
        self.fixture.managed_ordinary_lifetime(directory)
        status = self.fixture.ordinary_only_status()
        calls = []
        self.fixture.ops.queue_status = lambda _name: status
        self.fixture.ops.processes = SimpleNamespace(drain=lambda **_kwargs: [])
        self.fixture.ops.client = lambda name, operation, **_kwargs: calls.append((name, operation))
        self.fixture.ops.daemon = SimpleNamespace(wait=lambda **_kwargs: None)

        self.fixture.ops.shutdown()

        self.assertTrue(self.fixture.ops.closed)
        self.assertEqual(calls, [("private-shutdown", ["shutdown"])])

    def test_managed_ordinary_successful_shutdown_without_closure_is_rejected(self):
        self.fixture.ordinary_only_admission("managed")
        self.fixture.ops.queue_status = lambda _name: self.fixture.ordinary_only_status()

        with self.assertRaisesRegex(AcceptanceFailure, "lifetime"):
            self.fixture.ops.shutdown()

        self.assertFalse(self.fixture.ops.closed)

    def test_managed_ordinary_not_started_proof_allows_failed_cleanup(self):
        directory = self.fixture.ordinary_only_admission("managed")
        self.fixture.managed_ordinary_lifetime(
            directory, wait_result="not-started", with_worker=False)

        self.assertTrue(self.fixture.ops.failure_queue_finished(
            self.fixture.ordinary_only_status({"FailedToSpawn": "fixture start failure"})))

    def test_managed_ordinary_exact_unqualified_failure_is_valid(self):
        directory = self.fixture.ordinary_only_admission("managed")
        self.fixture.managed_ordinary_lifetime(
            directory, wait_result="exit-error", with_worker=False,
            scope_kind="cgroup-v2")

        self.assertTrue(self.fixture.ops.failure_queue_finished(
            self.fixture.ordinary_only_status({"Failed": 17})))

    def test_managed_ordinary_previous_boot_termination_does_not_require_wait(self):
        directory = self.fixture.ordinary_only_admission("managed")
        self.fixture.managed_ordinary_lifetime(
            directory, include_wait=False, termination_cause="previous-boot")

        self.assertTrue(
            self.fixture.ops.failure_queue_finished(self.fixture.ordinary_only_status("Killed")))

    def test_managed_ordinary_lifetime_requires_domain_separated_owner_digest(self):
        directory = self.fixture.ordinary_only_admission("managed")
        spec_sha = digest(directory / "task.json")
        meta_sha = digest(directory / "meta.json")
        self.assertEqual(
            provider_common.ordinary_lifetime_owner_digest(spec_sha, meta_sha),
            provider_common.sha(
                ("delegation-layer:lifetime-owner:task:v1\n" + spec_sha +
                 "\n" + meta_sha).encode("ascii")),
        )
        lifetime = self.fixture.managed_ordinary_lifetime(directory, wait_result="exit-error")
        for name in ("intent.json", "registration.json"):
            path = lifetime / name
            record = read_json(path)
            record["owner_sha256"] = spec_sha
            write_json(path, record, replace=True)

        self.assertFalse(self.fixture.ops.failure_queue_finished(
            self.fixture.ordinary_only_status({"Failed": 17})))

    def test_managed_ordinary_lifetime_requires_exact_launch_set(self):
        directory = self.fixture.ordinary_only_admission("managed")
        lifetime = self.fixture.managed_ordinary_lifetime(directory, wait_result="exit-error")
        termination_path = lifetime / "terminated.json"
        termination = read_json(termination_path)
        termination["launch_set_sha256"] = provider_common.sha(
            provider_common.canonical_go_json([]))
        write_json(termination_path, termination, replace=True)

        self.assertFalse(self.fixture.ops.failure_queue_finished(
            self.fixture.ordinary_only_status({"Failed": 17})))

    def test_custom_ordinary_task_without_lifetime_keeps_queue_fallback(self):
        self.fixture.ordinary_only_admission("custom")

        self.assertTrue(
            self.fixture.ops.failure_queue_finished(self.fixture.ordinary_only_status("Killed")))


if __name__ == "__main__":
    unittest.main()
