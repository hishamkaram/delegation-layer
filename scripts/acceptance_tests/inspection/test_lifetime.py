"""Hermetic inspection queue and journal checks for the native acceptance harness."""

from __future__ import annotations

from copy import deepcopy
from pathlib import Path
import json
import shutil
import unittest
from unittest.mock import patch

from scripts import acceptance_native as native_acceptance
from scripts import acceptance_provider_common as provider_common
from scripts.acceptance_provider_common import AcceptanceFailure
from scripts.acceptance_supervisor_common import digest, read_json, write_json
from scripts.delegate_acceptance.evidence.records import _read_control_record
from scripts.delegate_acceptance.evidence.timestamps import _canonical_timestamp
from scripts.delegate_acceptance.lifetime.policy import (
    _canonical_lifetime_launch,
    _validate_lifetime_registration,
    validate_custody_placement,
)


ROOT = "a" * 32
TASK = "b" * 32


from scripts.acceptance_tests.record_fixtures import object_at
from .fixtures import InspectionFixture, FixtureProcesses, ObservedDaemon, forbid_installed_content


class InspectionHarnessTests(unittest.TestCase):
    def setUp(self):
        self.enterContext(forbid_installed_content())
        self.fixture = InspectionFixture(self)

    def test_shutdown_requires_complete_success_proof_for_inspection(self):
        self.fixture.inspection_records()
        status = self.fixture.status(self.fixture.inspection_row())
        calls = []
        self.enterContext(patch.object(self.fixture.ops, "queue_status", return_value=status))
        self.fixture.ops.processes = FixtureProcesses()
        self.enterContext(patch.object(self.fixture.ops, "client", side_effect=lambda name, operation, **_kwargs: calls.append((name, operation))))
        self.fixture.ops.daemon = ObservedDaemon()

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
        directory = self.fixture.inspection_records(legacy_executable_identity=True)
        binding = read_json(directory / "request.json")["binding"]
        supervisor = binding["supervisor"]
        del supervisor["executable_identity"]
        supervisor["client_sha256"] = "c" * 64
        supervisor["daemon_executable"] = "/fixture/pueued"
        supervisor["daemon_sha256"] = "a" * 64

        provider_common._validate_inspection_binding(binding, "inspection binding")

        del supervisor["daemon_executable"]
        with self.assertRaisesRegex(AcceptanceFailure, "daemon provenance is incomplete"):
            provider_common._validate_inspection_binding(binding, "inspection binding")

    def test_model_discovery_closure_accepts_an_unhashed_installed_provider(self):
        directory = self.fixture.inspection_records(
            include_ordinary=False, revision="model-discovery-v1",
            managed_exit_proof=True, helper_sha256="", legacy_executable_identity=True)
        request_bytes = (directory / "request.json").read_bytes()
        request = read_json(directory / "request.json")
        self.assertEqual(object_at(request, "binding")["helper_sha256"], "")

        closure = provider_common.validate_managed_lifetime_closure(self.fixture.state)

        proof = object_at(closure, "inspections", TASK)
        self.assertEqual(object_at(proof, "registration")["owner_sha256"],
                         digest(directory / "request.json"))
        self.assertEqual(object_at(proof, "worker_wait")["result"], "success")
        self.assertTrue(object_at(proof, "termination")["scope_empty"])
        self.assertEqual((directory / "request.json").read_bytes(), request_bytes)

    def test_inspection_binding_rejects_malformed_optional_provider_diagnostics(self):
        directory = self.fixture.inspection_records(legacy_executable_identity=True)
        binding = read_json(directory / "request.json")["binding"]
        for value in (None, False, 0, " ", "invalid", "e" * 63, "E" * 64):
            with self.subTest(value=value), self.assertRaisesRegex(
                    AcceptanceFailure, "helper_sha256 is invalid"):
                provider_common._validate_inspection_binding(
                    dict(binding, helper_sha256=value), "inspection binding")
        del binding["helper_sha256"]
        with self.assertRaisesRegex(AcceptanceFailure, "missing fields"):
            provider_common._validate_inspection_binding(binding, "inspection binding")

    def test_inspection_binding_keeps_historical_definition_and_worker_digests_required(self):
        directory = self.fixture.inspection_records(legacy_executable_identity=True)
        binding = read_json(directory / "request.json")["binding"]
        for key in ("definition_sha256", "worker_sha256"):
            for value in ("", None, "invalid"):
                with self.subTest(key=key, value=value), self.assertRaisesRegex(
                        AcceptanceFailure, key + " is invalid"):
                    provider_common._validate_inspection_binding(
                        dict(binding, **{key: value}), "inspection binding")

    def test_model_discovery_closure_rejects_a_malformed_provider_digest(self):
        directory = self.fixture.inspection_records(
            include_ordinary=False, revision="model-discovery-v1",
            managed_exit_proof=True, legacy_executable_identity=True)
        request = read_json(directory / "request.json")
        object_at(request, "binding")["helper_sha256"] = "E" * 64
        write_json(directory / "request.json", request, replace=True)

        with self.assertRaisesRegex(AcceptanceFailure, "helper_sha256 is invalid"):
            provider_common.validate_managed_lifetime_closure(self.fixture.state)

    def test_installed_binding_matches_canonical_go_wire_semantics(self):
        directory = self.fixture.inspection_records()
        original = (directory / "request.json").read_bytes()
        binding = read_json(directory / "request.json")["binding"]
        self.assertEqual(binding["executable_identity"], "installed-path-v1")
        self.assertNotIn("worker_sha256", binding)
        self.assertNotIn("helper_sha256", binding)
        provider_common._validate_inspection_binding(binding, "installed binding")
        for field in ("helper_sha256", "worker_sha256"):
            with self.subTest(field=field):
                provider_common._validate_inspection_binding(
                    dict(binding, **{field: ""}), "installed binding")
                for value in (None, False, 0, "e" * 64, [], {}):
                    with self.subTest(value=value), self.assertRaisesRegex(
                            AcceptanceFailure, "installed identity contains executable SHA"):
                        provider_common._validate_inspection_binding(
                            dict(binding, **{field: value}), "installed binding")
        for marker in ("future-identity", None, False):
            with self.subTest(marker=marker), self.assertRaisesRegex(
                    AcceptanceFailure, "executable identity is unsupported"):
                provider_common._validate_inspection_binding(
                    dict(binding, executable_identity=marker), "installed binding")
        for marker in ("", "absent"):
            truncated = dict(binding)
            if marker == "absent":
                del truncated["executable_identity"]
            else:
                truncated["executable_identity"] = marker
            with self.subTest(marker=marker), self.assertRaisesRegex(AcceptanceFailure, "missing fields"):
                provider_common._validate_inspection_binding(truncated, "truncated legacy binding")
        for value in ("", None, "invalid"):
            with self.subTest(definition=value), self.assertRaisesRegex(
                    AcceptanceFailure, "definition_sha256 is invalid"):
                provider_common._validate_inspection_binding(
                    dict(binding, definition_sha256=value), "installed binding")
        self.assertEqual((directory / "request.json").read_bytes(), original)

    def test_managed_guardian_wait_replaces_queue_worker_observation(self):
        directory = self.fixture.inspection_records(managed_exit_proof=True)

        records = self.fixture.ops.validate_inspection_journals()

        self.assertFalse((directory / "worker-observation.json").exists())
        proof = object_at(records, TASK, "managed_lifetime")
        self.assertEqual(object_at(proof, "worker_wait")["result"], "success")
        self.assertTrue(object_at(proof, "termination")["scope_empty"])

    def test_root_lifetime_closure_rejects_inspection_only_unfinished_state(self):
        directory = self.fixture.inspection_records(include_ordinary=False, managed_exit_proof=True)
        (directory / "lifetime-worker-wait.json").unlink()

        with self.assertRaisesRegex(AcceptanceFailure, "inspection lifetime worker Wait"):
            provider_common.validate_managed_lifetime_closure(self.fixture.state)

    def test_root_lifetime_closure_accepts_expired_inspection_without_native_facts(self):
        directory = self.fixture.unlaunched_inspection()
        closure = provider_common.validate_managed_lifetime_closure(self.fixture.state)
        proof = object_at(closure, "inspections", TASK)
        self.assertEqual(proof["mode"], "unlaunched")
        self.assertEqual(proof["unlaunched_sha256"],
                         digest(directory / "lifetime-unlaunched.json"))
        self.assertNotIn("worker_wait", proof)
        self.assertNotIn("termination", proof)

    def test_root_lifetime_closure_uses_inspection_operation_identity(self):
        operation = "c" * 32
        self.fixture.unlaunched_inspection(operation)
        closure = provider_common.validate_managed_lifetime_closure(self.fixture.state)
        self.assertEqual(set(object_at(closure, "inspections")), {operation})
        self.assertEqual(object_at(closure, "inspections", operation, "unlaunched")["task_id"], operation)

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
            self.enterContext(patch.object(operation, "ops", self.fixture.ops, create=True))
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

        self.assertEqual(set(object_at(closure, "tasks")), {TASK})
        self.assertEqual(set(object_at(closure, "inspections")), {TASK})
        self.assertEqual(object_at(closure, "tasks", TASK, "worker_wait")["result"], "success")
        self.assertEqual(object_at(closure, "inspections", TASK, "worker_wait")["result"], "success")

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
                self.fixture.inspection_records(
                    custom_inspection=options.get("custom_inspection", False),
                    legacy_admission=options.get("legacy_admission", False), include_ordinary=False)
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
        self.enterContext(patch.object(self.fixture.ops, "queue_status", return_value=status))
        self.fixture.ops.processes = FixtureProcesses()
        self.enterContext(patch.object(self.fixture.ops, "client", side_effect=lambda name, operation, **_kwargs: calls.append((name, operation))))
        self.fixture.ops.daemon = ObservedDaemon()

        self.fixture.ops.shutdown()

        self.assertTrue(self.fixture.ops.closed)
        self.assertEqual(calls, [("private-shutdown", ["shutdown"])])

    def test_managed_ordinary_successful_shutdown_without_closure_is_rejected(self):
        self.fixture.ordinary_only_admission("managed")
        self.enterContext(patch.object(self.fixture.ops, "queue_status", return_value=self.fixture.ordinary_only_status()))

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


POOL_ID = "c" * 64
PLACEMENT = {"version": 1, "pool_root": "/nonexistent/delegate/pool", "pool_id": POOL_ID}
REGISTRATION_DIGEST = "d" * 64


def registration_for(owner_kind, **extra):
    return {
        "schema_version": 1, "root_id": ROOT, "task_id": TASK, "owner_kind": owner_kind,
        "owner_sha256": REGISTRATION_DIGEST, "scope_kind": "process-group", "assurance": "cooperative",
        "scope_id": "scope:fixture", "boot_id": "fixture-boot", "deadline": "2026-09-15T00:01:00Z",
        "created_at": "2026-09-15T00:00:05Z", **extra,
    }


def validate_registration(registration):
    return _validate_lifetime_registration(
        assurance="cooperative", evidence=registration["owner_kind"], intent={
            "boot_id": "fixture-boot", "deadline": "2026-09-15T00:01:00Z",
        }, intent_created=_canonical_timestamp("2026-09-15T00:00:04Z", "intent"),
        owner_digest=REGISTRATION_DIGEST, owner_kind=registration["owner_kind"], recovered=False,
        registration=registration, root=ROOT, scope_kind="process-group", task=TASK, window_deadline=None)


def rebind_registration(lifetime, change):
    """Rewrite the registration and re-derive every record that binds its digest."""
    path = lifetime / "registration.json"
    registration = read_json(path)
    change(registration)
    write_json(path, registration, replace=True)
    registration_sha = digest(path)
    for name in ("worker.json", "00000000000000000001.launch.json", "terminated.json", "worker-wait.json"):
        record = read_json(lifetime / name)
        record["registration_sha256"] = registration_sha
        if name == "worker-wait.json":
            record["worker_sha256"] = digest(lifetime / "worker.json")
        if name == "terminated.json":
            launch = read_json(lifetime / "00000000000000000001.launch.json")
            record["launch_set_sha256"] = provider_common.sha(
                provider_common.canonical_go_json([_canonical_lifetime_launch(launch, label="launch")]))
        write_json(lifetime / name, record, replace=True)


class CustodyPlacementRegistrationTests(unittest.TestCase):
    def setUp(self):
        self.fixture = InspectionFixture(self)

    def placed_lifetime(self, placement=PLACEMENT):
        directory = self.fixture.ordinary_only_admission("managed")
        lifetime = self.fixture.managed_ordinary_lifetime(directory, wait_result="exit-error")
        rebind_registration(lifetime, lambda record: record.update(custody_placement=deepcopy(placement)))
        return lifetime

    def closes(self):
        return self.fixture.ops.failure_queue_finished(self.fixture.ordinary_only_status({"Failed": 17}))

    def test_legacy_registrations_without_placement_keep_their_bytes_and_digests(self):
        for owner_kind in ("task", "inspection", "workspace"):
            with self.subTest(owner_kind=owner_kind):
                registration = registration_for(owner_kind)
                before = provider_common.canonical_go_json(registration)
                validate_registration(registration)
                self.assertEqual(provider_common.canonical_go_json(registration), before)
                self.assertNotIn("custody_placement", registration)
        directory = self.fixture.ordinary_only_admission("managed")
        lifetime = self.fixture.managed_ordinary_lifetime(directory, wait_result="exit-error")
        self.assertNotIn("custody_placement", read_json(lifetime / "registration.json"))
        self.assertTrue(self.closes())

    def test_task_and_inspection_placements_are_accepted_for_a_pool_that_does_not_exist(self):
        for owner_kind in ("task", "inspection"):
            with self.subTest(owner_kind=owner_kind):
                validate_registration(registration_for(owner_kind, custody_placement=deepcopy(PLACEMENT)))
        validate_custody_placement(deepcopy(PLACEMENT), "placement")
        validate_custody_placement({**PLACEMENT, "pool_root": "/"}, "root placement")

    def test_a_workspace_registration_with_a_valid_placement_is_accepted_and_unchanged(self):
        registration = registration_for("workspace", custody_placement=deepcopy(PLACEMENT))
        before = provider_common.canonical_go_json(registration)
        validate_registration(registration)
        self.assertEqual(provider_common.canonical_go_json(registration), before)
        for bad in (None, {**PLACEMENT, "pool_root": "/a/./b"}, {**PLACEMENT, "extra": 1}):
            with self.assertRaises(AcceptanceFailure):
                validate_registration(registration_for("workspace", custody_placement=bad))

    def test_invalid_placements_are_refused_by_type_version_path_id_and_keys(self):
        cases = {"null": None, "list": [], "text": "x", "number": 1}
        for name, value in cases.items():
            with self.subTest(placement=name):
                with self.assertRaisesRegex(AcceptanceFailure, "not an object"):
                    validate_registration(registration_for("task", custody_placement=value))
        for name, version in {"zero": 0, "two": 2, "bool": True, "text": "1", "float": 1.0, "null": None}.items():
            self.assert_refused({"version": version}, "version is unsupported", "version " + name)
        for name, root in {
            "empty": "", "blank": " ", "relative": "pool", "dot segment": "/a/./b", "parent segment": "/a/../b",
            "trailing slash": "/a/", "double slash": "/a//b", "leading double slash": "//a", "NUL": "/a\x00b",
            "parent of root": "/..",
        }.items():
            self.assert_refused({"pool_root": root}, "not a clean absolute path", "root " + name)
        for name, root in {"number": 7, "null": None, "list": ["/a"]}.items():
            self.assert_refused({"pool_root": root}, "pool root is not text", "root " + name)
        for name, pool_id in {
            "uppercase": POOL_ID.upper(), "short": "c" * 63, "long": "c" * 65, "empty": "", "non-hex": "g" * 64,
            "number": 7, "null": None,
        }.items():
            self.assert_refused({"pool_id": pool_id}, "pool id is not 64 lowercase hex", "id " + name)
        for key in PLACEMENT:
            missing = {name: value for name, value in PLACEMENT.items() if name != key}
            with self.subTest(missing=key), self.assertRaisesRegex(AcceptanceFailure, "unknown or missing fields"):
                validate_registration(registration_for("task", custody_placement=missing))
        self.assert_refused({"extra": 1}, "unknown or missing fields", "extra key")

    def assert_refused(self, change, pattern, name):
        with self.subTest(case=name), self.assertRaisesRegex(AcceptanceFailure, pattern):
            validate_registration(registration_for("task", custody_placement={**PLACEMENT, **change}))

    def test_unknown_registration_fields_stay_strict_beside_a_placement(self):
        with self.assertRaisesRegex(AcceptanceFailure, "unknown or missing fields"):
            validate_registration(registration_for("task", custody_placement=deepcopy(PLACEMENT), pool_root="/x"))

    def test_a_placed_registration_closes_through_the_canonical_chain(self):
        self.placed_lifetime()
        self.assertTrue(self.closes())

    def test_a_malformed_placement_in_the_chain_is_refused_even_with_rebound_digests(self):
        self.placed_lifetime({**PLACEMENT, "pool_root": "/a/./b"})
        self.assertFalse(self.closes())

    def test_tampering_the_placement_after_the_chain_was_bound_is_detected(self):
        lifetime = self.placed_lifetime()
        self.assertTrue(self.closes())
        path = lifetime / "registration.json"
        record = read_json(path)
        record["custody_placement"]["pool_id"] = "e" * 64
        write_json(path, record, replace=True)
        self.assertFalse(self.closes())

    def test_dropping_or_adding_a_placement_after_binding_changes_the_registration_digest(self):
        lifetime = self.placed_lifetime()
        placed_digest = digest(lifetime / "registration.json")
        record = read_json(lifetime / "registration.json")
        del record["custody_placement"]
        write_json(lifetime / "registration.json", record, replace=True)
        self.assertNotEqual(digest(lifetime / "registration.json"), placed_digest)
        self.assertFalse(self.closes())

    def test_a_duplicate_placement_key_is_refused_by_the_canonical_record_reader(self):
        lifetime = self.placed_lifetime()
        path = lifetime / "registration.json"
        placement = json.dumps(PLACEMENT)
        text = path.read_text()
        path.write_text(text.rstrip().rstrip("}") + ', "custody_placement": ' + placement + "}")
        with self.assertRaisesRegex(AcceptanceFailure, "duplicate|JSON"):
            _read_control_record(path, "registration")
        self.assertFalse(self.closes())


if __name__ == "__main__":
    unittest.main()
