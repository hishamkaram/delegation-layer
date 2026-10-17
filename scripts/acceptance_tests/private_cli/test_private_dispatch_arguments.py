"""Implementation methods for PrivateDispatchArgumentsTests; collected once by the acceptance_tests discovery."""

from contextlib import ExitStack, redirect_stderr, redirect_stdout
import hashlib
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch
from scripts import acceptance_private_cli as private_cli
from scripts.delegate_acceptance.private_cli import application as application_impl
from scripts.delegate_acceptance.private_cli import finalization as finalization_impl
from scripts.delegate_acceptance.private_cli import scenario_entry as scenario_entry_impl
from scripts.delegate_acceptance.private_cli import initial as initial_impl
from scripts.delegate_acceptance.private_cli import capacity_entry as capacity_entry_impl
from scripts.delegate_acceptance.private_cli import handoff_cleanup as handoff_cleanup_impl
from scripts.delegate_acceptance.processes import operations as process_owner_impl
from scripts import acceptance_supervisor_common as common
from scripts.acceptance_tests.private_cli.fixtures import (
    _release_live_directory_tokens,
)


class PrivateDispatchArgumentsTests(unittest.TestCase):
    def test_handoff_task_binding_accepts_exact_packaged_supervisor(self):
        binding = {
            "config_path": "/state/.supervisor/pueue.yml",
            "client_executable": "/package/libexec/pueue",
            "client_sha256": "a" * 64,
            "daemon_executable": "/package/libexec/pueued",
            "daemon_sha256": "b" * 64,
        }
        private_cli.require_handoff_task_binding(
            json.dumps({"supervisor_config": binding}).encode(), binding,
        )

    def test_handoff_task_binding_rejects_drift_and_unrelated_path_text(self):
        binding = {
            "config_path": "/state/.supervisor/pueue.yml",
            "client_executable": "/package/libexec/pueue",
            "client_sha256": "a" * 64,
            "daemon_executable": "/package/libexec/pueued",
            "daemon_sha256": "b" * 64,
        }
        for key in binding:
            with self.subTest(changed=key):
                changed = {**binding, key: "different"}
                metadata = {"supervisor_config": changed, "unrelated": binding}
                with self.assertRaisesRegex(RuntimeError, "inspection journal supervisor"):
                    private_cli.require_handoff_task_binding(json.dumps(metadata).encode(), binding)
        for metadata in ({}, [], {"supervisor_config": None}):
            with self.subTest(invalid=metadata):
                with self.assertRaises(RuntimeError):
                    private_cli.require_handoff_task_binding(json.dumps(metadata).encode(), binding)

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-dispatch-args-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.sources = {}
        for name in ("delegate", "runner", "pueue", "pueued"):
            source = self.root / name
            source.write_text("#!/bin/sh\nexit 0\n")
            source.chmod(0o700)
            self.sources[name] = source

    def handoff_journal(self):
        journal = self.root / "inspection-group"
        journal.mkdir()
        private_config = self.root / ".supervisor" / "pueue.yml"
        root_id = "a" * 32
        legacy = {"root_id": root_id, "supervisor": {"config_path": "/external/pueue.yml"}}
        successor = {"root_id": root_id, "supervisor": {"config_path": str(private_config)}}
        (journal / "request.json").write_text(json.dumps(legacy))
        (journal / "migration-complete.json").write_text(json.dumps({"successor": successor}))
        status = {
            "command": "supervisor", "root_id": root_id,
            "supervisor_control": {
                "action": "status", "state": "migrated",
                "migration_phase": "complete", "next_action": "none",
            },
        }
        return private_config, status

    def test_handoff_journal_uses_canonical_status_without_materialized_generation(self):
        private_config, status = self.handoff_journal()
        self.assertEqual(list(self.root.glob("inspection-group-*")), [])
        for state, phase in (("migrated", "complete"), ("ready", "adopted")):
            with self.subTest(state=state, phase=phase):
                observed = {
                    **status,
                    "supervisor_control": {
                        **status["supervisor_control"], "state": state, "migration_phase": phase,
                    },
                }
                evidence = private_cli.read_external_inspection_handoff_journal(
                    self.root, private_config, supervisor_status=observed,
                )
                self.assertEqual(evidence["successor"]["supervisor"]["config_path"], str(private_config))
                self.assertEqual(evidence["canonical_status"], observed["supervisor_control"])

    def test_handoff_journal_rejects_incomplete_or_unrelated_canonical_status(self):
        private_config, status = self.handoff_journal()
        invalid_statuses = [None, {}, {**status, "root_id": "b" * 32}]
        for field, value in (
                ("action", "migrate"), ("state", "external"),
                ("migration_phase", "pending"), ("next_action", "run_supervisor_migrate")):
            invalid_statuses.append({
                **status, "supervisor_control": {**status["supervisor_control"], field: value},
            })
        for invalid in invalid_statuses:
            with self.subTest(status=invalid):
                with self.assertRaisesRegex(RuntimeError, "canonical supervisor status|CLI did not validate"):
                    private_cli.read_external_inspection_handoff_journal(
                        self.root, private_config, supervisor_status=invalid,
                    )
        (self.root / "inspection-group" / "migration-complete.json").unlink()
        with self.assertRaisesRegex(RuntimeError, "completion marker is missing"):
            private_cli.read_external_inspection_handoff_journal(
                self.root, private_config, supervisor_status=status,
            )

    def arguments(self, total, workers, output=None, scenario="capacity"):
        output = output or (self.root / "receipt.json")
        args = ["acceptance_private_cli.py"]
        for name in ("delegate", "runner", "pueue", "pueued"):
            args.extend(["--" + name, str(self.sources[name])])
        args.extend(["--output", str(output), "--parallel-tasks", str(total)])
        if scenario != private_cli.EXTERNAL_INSPECTION_HANDOFF_SCENARIO:
            args.append("--runtime-only")
        if scenario != "capacity":
            args.extend(["--scenario", scenario])
        if workers is not None:
            args.extend(["--dispatch-workers", str(workers)])
        return args

    def test_dispatch_worker_default_override_and_independent_receipt(self):
        for supplied, chosen in ((None, 16), (2, 2), (20, 20)):
            with self.subTest(requested=supplied):
                receipt, upgrade = self.run_current_scenario(200, supplied)
                self.assertEqual(upgrade.call_count, 1)
                self.assertEqual(upgrade.call_args.args[11:13], (200, chosen))
                self.assertTrue(upgrade.call_args.args[13].name.endswith(".json.responses"))
                self.assertEqual(receipt["dispatch_workers"], chosen)
                self.assertEqual(receipt["concurrent_running_tasks"], 200)
                self.assertEqual(receipt["concurrent_status_bytes"], 1024 * 1024 + 1)
                self.assertEqual(receipt["capacity_proof"]["requested_task_count"], 200)
                self.assertEqual(receipt["capacity_proof"]["observed_running_task_count"], 200)
                self.assertEqual(receipt["capacity_proof"]["observed_live_lease_count"], 200)
                self.assertTrue(receipt["capacity_proof"]["queue_confirmed_while_leases_held"])
                self.assertTrue(receipt["capacity_proof"]["gate_opened_after_proof"])
                self.assertEqual(receipt["dispatch_batch"]["requested_caller_concurrency"], chosen)
                self.assertEqual(receipt["dispatch_batch"]["admitted_task_count"], 200)
                self.assertEqual(receipt["status_coalescing"], {
                    "concurrent_status_call_count": 16,
                    "pueue_status_snapshot_count": 1,
                    "shared_status_call_count": 15,
                })
                self.assertTrue(receipt["packaged_default_path"]["fresh_bootstrap"])
                self.assertEqual(receipt["result"], "PASS")
                self.assertEqual(receipt["status"], "passed")
                self.assertTrue(receipt["cleanup"]["private_root"]["verified"])

    def test_partial_failure_always_writes_sanitized_receipt(self):
        receipt, upgrade, error = self.execute_current_scenario(
            200, 8, upgrade_failure=True,
        )

        self.assertIsInstance(error, RuntimeError)
        self.assertEqual(upgrade.call_count, 1)
        self.assertEqual(receipt["result"], "FAIL")
        self.assertEqual(receipt["failure"]["phase"], "capacity_observation")
        self.assertEqual(receipt["counts"]["dispatch_completed_count"], 73)
        self.assertEqual(receipt["counts"]["admitted_task_count"], 71)
        self.assertEqual(receipt["counts"]["running_task_count"], 64)
        self.assertEqual(receipt["counts"]["live_lease_count"], 63)
        self.assertEqual(receipt["queue"], {
            "task_count": 73, "running_count": 64, "status_bytes": 4096,
        })
        self.assertEqual(receipt["cleanup"]["private_root"]["status"], "retained")
        self.assertEqual(
            set(receipt["platform"]), {"system", "kernel", "architecture"},
        )
        self.assertIn("soft_limit", receipt["file_descriptors"])
        self.assertIn("source", receipt["fingerprints"])
        serialized = json.dumps(receipt)
        self.assertNotIn("SECRET", serialized)
        self.assertNotIn("prompt credential", serialized)
        self.assertNotIn("environment", serialized)

    def test_cleanup_failure_overwrites_pending_success_with_fail_receipt(self):
        receipt, _, error = self.execute_current_scenario(
            200, 8, cleanup_failure=True,
        )

        self.assertIsInstance(error, RuntimeError)
        self.assertEqual(receipt["result"], "FAIL")
        self.assertEqual(receipt["failure"]["code"], "cleanup_failed")
        self.assertEqual(receipt["failure"]["phase"], "cleanup")
        self.assertEqual(receipt["cleanup"]["status"], "failed")
        self.assertEqual(receipt["cleanup"]["private_root"], {
            "status": "retained", "verified": True,
        })
        self.assertNotIn("SECRET cleanup diagnostic", json.dumps(receipt))

    def test_non_capacity_task_count_is_rejected_before_fixture_setup(self):
        for total in (0, 1, 199, 201):
            with self.subTest(total=total):
                errors = io.StringIO()
                with patch.object(sys, "argv", self.arguments(total, 8)), \
                        patch.object(private_cli.tempfile, "mkdtemp") as setup, \
                        redirect_stderr(errors):
                    with self.assertRaises(SystemExit) as raised:
                        private_cli.main()
                self.assertEqual(raised.exception.code, 2)
                self.assertIn("must be exactly 200", errors.getvalue())
                setup.assert_not_called()

    def test_default_stop_requires_exact_two_tasks_before_fixture_setup(self):
        for total in (0, 1, 3, 200):
            with self.subTest(total=total):
                errors = io.StringIO()
                with patch.object(
                        sys, "argv",
                        self.arguments(total, 2, scenario=private_cli.DEFAULT_STOP_SCENARIO)), \
                        patch.object(private_cli.tempfile, "mkdtemp") as setup, \
                        redirect_stderr(errors):
                    with self.assertRaises(SystemExit) as raised:
                        private_cli.main()
                self.assertEqual(raised.exception.code, 2)
                self.assertIn("must be exactly 2", errors.getvalue())
                setup.assert_not_called()

    def test_external_handoff_requires_exact_two_tasks_before_fixture_setup(self):
        for total in (0, 1, 3, 200):
            with self.subTest(total=total):
                errors = io.StringIO()
                with patch.object(
                        sys, "argv",
                        self.arguments(
                            total, 2,
                            scenario=private_cli.EXTERNAL_INSPECTION_HANDOFF_SCENARIO,
                        )), \
                        patch.object(private_cli.tempfile, "mkdtemp") as setup, \
                        redirect_stderr(errors):
                    with self.assertRaises(SystemExit) as raised:
                        private_cli.main()
                self.assertEqual(raised.exception.code, 2)
                self.assertIn("must be exactly 2", errors.getvalue())
                setup.assert_not_called()

    def test_external_handoff_rejects_runtime_only_before_fixture_setup(self):
        arguments = self.arguments(
            2, 2, scenario=private_cli.EXTERNAL_INSPECTION_HANDOFF_SCENARIO,
        ) + ["--runtime-only"]
        errors = io.StringIO()
        with patch.object(sys, "argv", arguments), \
                patch.object(private_cli.tempfile, "mkdtemp") as setup, \
                redirect_stderr(errors):
            with self.assertRaises(SystemExit) as raised:
                private_cli.main()
        self.assertEqual(raised.exception.code, 2)
        self.assertIn("cannot be runtime-only", errors.getvalue())
        setup.assert_not_called()

    def test_external_supervisor_start_has_configured_parent_directories(self):
        owner = private_cli.DirectoryOwner()
        started = []
        self.addCleanup(owner.rollback)

        for case_label in ("explicit-migrate", "automatic-models", "automatic-dispatch"):
            with self.subTest(case=case_label):
                case_root = owner.create(
                    "external handoff case " + case_label,
                    self.root / ("case-" + case_label), self.root,
                )
                external_root = owner.create(
                    "external supervisor " + case_label,
                    case_root.directory / "external-supervisor", case_root.directory,
                )
                private_cli.prepare_external_supervisor_directories(
                    owner, external_root, case_label,
                )
                config = private_cli.config_for(external_root.directory)
                configured = config["shared"]
                required_directories = tuple(
                    Path(configured[name])
                    for name in ("pueue_directory", "runtime_directory")
                )

                process_root = owner.create(
                    "external supervisor process capture " + case_label,
                    case_root.directory / "external-supervisor-processes", case_root.directory,
                )
                processes = common.Processes(
                    process_root.directory, environment={}, capture_root=process_root,
                    return_result=True,
                )
                test_case = self

                class FakePopen:
                    def __init__(self, argv, **_kwargs):
                        self.pid = 8000 + len(started)
                        self.returncode = 0
                        self.asserted_argv = tuple(argv)
                        test_case.assertTrue(all(path.is_dir() for path in required_directories))
                        started.append(case_label)

                    @staticmethod
                    def poll():
                        return 0

                    @staticmethod
                    def wait():
                        return 0

                with patch.object(common.subprocess, "Popen", FakePopen):
                    process = processes.start(
                        "external-daemon",
                        [str(self.sources["pueued"]), "-c",
                         str(external_root.directory / "pueue.yml")],
                        external_root.directory,
                    )
                    result = process.wait(timeout=1, expected=0)
                self.assertEqual(result["exit_code"], 0)

        self.assertEqual(started, [
            "explicit-migrate", "automatic-models", "automatic-dispatch",
        ])

    def test_external_handoff_success_writes_pass_receipt(self):
        receipt, error = self.execute_external_handoff_scenario()
        self.assertIsNone(error)
        self.assertEqual(receipt["result"], "PASS")
        self.assertEqual(receipt["status"], "passed")
        self.assertEqual(receipt["scenario"], private_cli.EXTERNAL_INSPECTION_HANDOFF_SCENARIO)
        cases = receipt["external_inspection_handoff"]
        self.assertEqual(set(cases), {
            "explicit-migrate", "automatic-models", "automatic-dispatch",
        })
        for label, case in cases.items():
            self.assertEqual(case["case"], label)
            self.assertEqual(case["handoff_action"], label)
            self.assertEqual(case["cleanup"], {
                "fixture_waiter_entries": "pending",
                "lifetime_closure": "canonical-managed-validator",
                "external_daemon": "pending",
                "private_daemon": "pending",
            })
        self.assertEqual(receipt["external_inspection_handoff_case_count"], 3)

    def test_external_handoff_closure_failure_writes_fail_receipt(self):
        receipt, error = self.execute_external_handoff_scenario(
            failure=RuntimeError("managed lifetime closure failed"),
        )
        self.assertIsInstance(error, RuntimeError)
        self.assertEqual(receipt["result"], "FAIL")
        self.assertEqual(receipt["status"], "failed")
        self.assertEqual(receipt["failure"]["phase"], "external_inspection_explicit_migrate")
        self.assertEqual(receipt["cleanup"]["private_root"]["status"], "retained")

    def test_external_handoff_closure_requires_empty_scope(self):
        closure = {
            "intent": {"deadline": "2030-01-01T00:00:00Z", "created_at": "2029-01-01T00:00:00Z"},
            "registration": {
                "created_at": "2029-01-01T00:00:01Z",
                "scope_kind": "process-group",
                "assurance": "fixture",
            },
            "worker_wait": {"child_pid": 123, "result": "exited", "exit_code": 0},
            "termination": {"cause": "completed", "scope_empty": True},
            "launches": [{"pid": 123}],
        }
        evidence = private_cli.external_handoff_closure_evidence(closure, "a" * 32)
        self.assertEqual(evidence["termination"]["scope_empty"], True)
        self.assertEqual(evidence["launch_count"], 1)
        invalid = dict(closure)
        invalid["termination"] = {"cause": "completed", "scope_empty": False}
        with self.assertRaises(RuntimeError):
            private_cli.external_handoff_closure_evidence(invalid, "a" * 32)

    def test_post_start_receipt_failure_retains_canonical_process_registry_entry(self):
        registered = []

        class FakeProcess:
            def __init__(self, directory, argv, _cwd, _environment, _capture_root, **kwargs):
                self.pid = 7101
                self.argv = [str(argument) for argument in argv]
                self.directory = directory
                self.result = None
                registered.append(self)
                kwargs["on_started"](self)

            def poll(self):
                return None

        with tempfile.TemporaryDirectory(prefix="delegate-start-receipt-") as directory:
            root = Path(directory)
            owner = private_cli.DirectoryOwner()
            capture_root = owner.create("capture root", root / "capture", root)
            try:
                processes = common.Processes(
                    capture_root.directory, environment={}, capture_root=capture_root,
                )
                with patch.object(process_owner_impl, "Process", FakeProcess), \
                        patch.object(process_owner_impl, "write_json", side_effect=OSError("receipt unavailable")):
                    with self.assertRaisesRegex(OSError, "receipt unavailable"):
                        processes.start(
                            "external-daemon", ["/fixture/pueued", "-c", "/fixture/pueue.yml"],
                            root,
                        )

                self.assertEqual(len(registered), 1)
                self.assertIs(private_cli.registered_process_handle(processes), registered[0])
                self.assertEqual(processes.pending(), [{
                    "pid": 7101,
                    "argv": ["/fixture/pueued", "-c", "/fixture/pueue.yml"],
                    "directory": str(capture_root.directory / "001-external-daemon"),
                }])
            finally:
                owner.rollback()

    def test_failed_lifetime_closure_retains_fixture_fifos(self):
        class Cohort:
            def __init__(self):
                self.removed = 0
                self.closed = 0

            def remove_closed_fifos(self):
                self.removed += 1

            def close(self):
                self.closed += 1

        cohort = Cohort()
        evidence = {"cleanup": {"fixture_waiter_entries": "pending"}}
        cleanup_errors = []
        with patch.object(
                handoff_cleanup_impl, "await_managed_lifetime_closure",
                side_effect=RuntimeError("closure is not proven")):
            closure_proven = private_cli.cleanup_external_handoff_cohorts(
                Path("/fixture/state"), ("a" * 32,), Path("/fixture/waiters"),
                (cohort,), evidence, cleanup_errors,
            )

        self.assertFalse(closure_proven)
        self.assertEqual(cohort.removed, 0)
        self.assertEqual(cohort.closed, 1)
        self.assertEqual(evidence["cleanup"]["fixture_waiter_entries"],
                         "retained-unproven-closure")
        self.assertEqual(cleanup_errors, ["managed lifetime closure failed: closure is not proven"])

    def test_admitted_task_before_cohort_acquisition_retains_unproven_work(self):
        evidence = {"cleanup": {"fixture_waiter_entries": "pending"}}
        cleanup_errors = []
        state = Path("/fixture/state")
        task_ids = ("a" * 32,)
        with patch.object(
                handoff_cleanup_impl, "await_managed_lifetime_closure",
                side_effect=RuntimeError("task closure is not proven")) as await_closure, \
                patch.object(handoff_cleanup_impl, "require_empty_fixture_waiters") as empty:
            closure_proven = private_cli.cleanup_external_handoff_cohorts(
                state, task_ids, Path("/fixture/waiters"), (None, None),
                evidence, cleanup_errors,
            )

        self.assertFalse(closure_proven)
        await_closure.assert_called_once_with(state, task_ids, timeout=45)
        empty.assert_not_called()
        self.assertEqual(evidence["cleanup"]["fixture_waiter_entries"],
                         "retained-unproven-closure")
        self.assertEqual(cleanup_errors, ["managed lifetime closure failed: task closure is not proven"])

    def test_post_start_failure_without_created_work_checks_empty_waiters(self):
        evidence = {"cleanup": {"fixture_waiter_entries": "pending"}}
        cleanup_errors = []
        with patch.object(handoff_cleanup_impl, "await_managed_lifetime_closure") as await_closure, \
                patch.object(handoff_cleanup_impl, "require_empty_fixture_waiters") as empty:
            closure_proven = private_cli.cleanup_external_handoff_cohorts(
                Path("/fixture/state"), (), Path("/fixture/waiters"), (None, None),
                evidence, cleanup_errors,
            )

        self.assertTrue(closure_proven)
        await_closure.assert_not_called()
        empty.assert_called_once_with(Path("/fixture/waiters"))
        self.assertEqual(cleanup_errors, [])

    def test_started_inspection_without_task_id_requires_lifetime_closure(self):
        evidence = {"cleanup": {"fixture_waiter_entries": "pending"}}
        cleanup_errors = []
        state = Path("/fixture/state")
        with patch.object(
                handoff_cleanup_impl, "await_managed_lifetime_closure",
                side_effect=RuntimeError("inspection closure is not proven"),
        ) as await_closure, patch.object(
                handoff_cleanup_impl, "require_empty_fixture_waiters",
        ) as empty:
            closure_proven = private_cli.cleanup_external_handoff_cohorts(
                state, (), Path("/fixture/waiters"), (None, None), evidence,
                cleanup_errors, delegate_control_started=True,
            )

        self.assertFalse(closure_proven)
        await_closure.assert_called_once_with(state, (), timeout=45)
        empty.assert_not_called()
        self.assertEqual(evidence["cleanup"]["fixture_waiter_entries"],
                         "retained-unproven-closure")
        self.assertEqual(
            cleanup_errors,
            ["managed lifetime closure failed: inspection closure is not proven"],
        )

    def test_successful_lifetime_closure_removes_fixture_fifos(self):
        class Cohort:
            def __init__(self):
                self.removed = 0
                self.closed = 0

            def remove_closed_fifos(self):
                self.removed += 1

            def close(self):
                self.closed += 1

        cohort = Cohort()
        evidence = {"cleanup": {"fixture_waiter_entries": "pending"}}
        cleanup_errors = []
        with patch.object(handoff_cleanup_impl, "await_managed_lifetime_closure", return_value={}), \
                patch.object(handoff_cleanup_impl, "require_empty_fixture_waiters") as empty:
            closure_proven = private_cli.cleanup_external_handoff_cohorts(
                Path("/fixture/state"), ("a" * 32,), Path("/fixture/waiters"),
                (cohort,), evidence, cleanup_errors,
            )

        self.assertTrue(closure_proven)
        self.assertEqual(cohort.removed, 1)
        self.assertEqual(cohort.closed, 1)
        empty.assert_called_once_with(Path("/fixture/waiters"))
        self.assertEqual(cleanup_errors, [])

    def execute_external_handoff_scenario(self, failure=None, failure_case=None,
                                          return_cleanup=False):
        base = Path(tempfile.mkdtemp(prefix=".dl-external-handoff-", dir=self.root)).resolve(strict=True)
        output_name = "failure" if failure else "success"
        if failure_case is not None:
            output_name += "-" + failure_case
        output = self.root / ("external-handoff-%s.json" % output_name)
        owners = []
        closure_calls = []
        shutdown_calls = []
        real_owner = private_cli.DirectoryOwner

        def tracked_owner(*args, **kwargs):
            owner = real_owner(*args, **kwargs)
            owners.append(owner)
            return owner

        run_handoff = _external_handoff_callback(failure, failure_case)
        def remove_root(_root_token, entries, **_kwargs):
            private_cli.rollback_owned_directories(entries)

        def record_closure(state, task_ids, **_kwargs):
            closure_calls.append((state, tuple(task_ids)))
            return {}

        def record_shutdown(client, config, _env, *, state, task_ids, **_kwargs):
            shutdown_calls.append((client, config, state, tuple(task_ids)))

        arguments = self.arguments(
            2, 2, output, scenario=private_cli.EXTERNAL_INSPECTION_HANDOFF_SCENARIO,
        )
        with ExitStack() as stack:
            stack.enter_context(patch.object(sys, "argv", arguments))
            stack.enter_context(patch.object(private_cli.tempfile, "mkdtemp",
                                             return_value=str(base)))
            stack.enter_context(patch.object(private_cli.Path, "home",
                                             return_value=self.root))
            stack.enter_context(patch.object(application_impl, "DirectoryOwner",
                                             side_effect=tracked_owner))
            stack.enter_context(patch.object(
                scenario_entry_impl, "run_external_inspection_handoff_acceptance",
                side_effect=run_handoff,
            ))
            stack.enter_context(patch.object(
                finalization_impl, "remove_private_acceptance_root", side_effect=remove_root,
            ))
            stack.enter_context(patch.object(
                finalization_impl, "await_managed_lifetime_closure", side_effect=record_closure,
            ))
            stack.enter_context(patch.object(
                finalization_impl, "shutdown_private_supervisor", side_effect=record_shutdown,
            ))
            stack.enter_context(redirect_stdout(io.StringIO()))
            error = None
            try:
                private_cli.main()
            except RuntimeError as raised:
                error = raised
            finally:
                for owner in reversed(owners):
                    if not getattr(owner, "_transferred", False):
                        owner.release()
        result = (json.loads(output.read_text()), error)
        if return_cleanup:
            return (*result, closure_calls, shutdown_calls)
        return result

    def test_later_case_failure_binds_cleanup_to_each_current_case(self):
        for failure_case in ("automatic-models", "automatic-dispatch"):
            with self.subTest(failure_case=failure_case):
                receipt, error, closure_calls, shutdown_calls = self.execute_external_handoff_scenario(
                    failure=RuntimeError("case failed"), failure_case=failure_case,
                    return_cleanup=True,
                )
                self.assertIsInstance(error, RuntimeError)
                self.assertEqual(receipt["result"], "FAIL")
                self.assertEqual(len(closure_calls), 1)
                self.assertEqual(len(shutdown_calls), 1)
                closure_state, closure_task_ids = closure_calls[0]
                shutdown_client, shutdown_config, shutdown_state, shutdown_task_ids = shutdown_calls[0]
                self.assertEqual(closure_state, shutdown_state)
                self.assertEqual(closure_task_ids, shutdown_task_ids)
                self.assertEqual(closure_state.name, "state")
                self.assertEqual(closure_state.parent.name, "external-handoff-" + failure_case)
                self.assertEqual(closure_task_ids, (
                    failure_case + "-task-a", failure_case + "-task-b",
                ))
                self.assertEqual(shutdown_config, closure_state / ".supervisor" / "pueue.yml")
                self.assertEqual(shutdown_client.name, "pueue")

    def test_invalid_workers_are_rejected_before_fixture_setup(self):
        for workers in (0, -1, "1.5", "not-an-integer"):
            with self.subTest(workers=workers):
                errors = io.StringIO()
                with patch.object(sys, "argv", self.arguments(200, workers)), \
                        patch.object(private_cli.tempfile, "mkdtemp") as setup, \
                        redirect_stderr(errors):
                    with self.assertRaises(SystemExit) as raised:
                        private_cli.main()
                self.assertEqual(raised.exception.code, 2)
                self.assertIn("--dispatch-workers", errors.getvalue())
                setup.assert_not_called()

    def test_missing_shutdown_probe_is_rejected_before_fixture_setup(self):
        output = self.root / "receipt.json"
        with patch.object(sys, "argv", self.arguments(200, 8, output)), \
                patch.object(private_cli.os, "access", return_value=False), \
                patch.object(private_cli.tempfile, "mkdtemp") as setup:
            with self.assertRaises(private_cli.AcceptanceBlocked):
                private_cli.main()
        setup.assert_not_called()
        receipt = json.loads(output.read_text())
        self.assertEqual(receipt["result"], "BLOCKED")
        self.assertEqual(receipt["failure"]["code"], "shutdown-probe-unavailable")
        self.assertEqual(receipt["counts"]["admitted_task_count"], 0)

    def test_existing_output_is_rejected_before_fixture_setup(self):
        for destination in ("receipt.json", "receipt.json.responses"):
            with self.subTest(destination=destination):
                output = self.root / "receipt.json"
                output.unlink(missing_ok=True)
                response_directory = self.root / "receipt.json.responses"
                if response_directory.exists():
                    response_directory.rmdir()
                occupied = self.root / destination
                if occupied.suffix == ".responses":
                    occupied.mkdir()
                else:
                    occupied.write_text("old receipt\n")
                with patch.object(sys, "argv", self.arguments(200, 8, output)), \
                        patch.object(private_cli.tempfile, "mkdtemp") as setup:
                    with self.assertRaisesRegex(RuntimeError, "already exists"):
                        private_cli.main()
                setup.assert_not_called()
                if occupied.is_dir():
                    occupied.rmdir()
                else:
                    occupied.unlink()

    def run_current_scenario(self, total, workers):
        receipt, upgrade, error = self.execute_current_scenario(total, workers)
        if error is not None:
            raise error
        return receipt, upgrade

    def execute_current_scenario(self, total, workers, *, upgrade_failure=False,
                                 cleanup_failure=False):
        scenario = "default" if workers is None else str(workers)
        if upgrade_failure:
            scenario += "-partial-failure"
        if cleanup_failure:
            scenario += "-cleanup-failure"
        base = Path(tempfile.mkdtemp(
            prefix=".dl-scenario-" + scenario + "-", dir=self.root,
        )).resolve(strict=True)
        output = self.root / ("receipt-" + scenario + ".json")
        _, _, old_libexec = private_cli.package_layout(base / "old", True)

        pending_old_layout = []

        run_json, upgrade, remove_root = _current_scenario_callbacks(base, cleanup_failure, old_libexec, pending_old_layout, self, total, upgrade_failure)
        owners = []
        real_owner = private_cli.DirectoryOwner

        def tracked_owner(*args, **kwargs):
            owner = real_owner(*args, **kwargs)
            owners.append(owner)
            return owner

        with ExitStack() as stack:
            stack.enter_context(patch.object(sys, "argv", self.arguments(total, workers, output)))
            stack.enter_context(patch.object(private_cli.tempfile, "mkdtemp",
                                            return_value=str(base)))
            stack.enter_context(patch.object(private_cli.Path, "home",
                                             return_value=self.root))
            stack.enter_context(patch.object(
                private_cli.shutil, "which",
                side_effect=AssertionError("private capacity acceptance must not require Node.js"),
            ))
            stack.enter_context(patch.object(initial_impl, "run_json", side_effect=run_json))
            stack.enter_context(patch.object(
                application_impl, "DirectoryOwner", side_effect=tracked_owner,
            ))
            shutdown = stack.enter_context(patch.object(capacity_entry_impl, "shutdown_private_supervisor"))
            stack.enter_context(patch.object(finalization_impl, "shutdown_private_supervisor", shutdown))
            stack.enter_context(patch.object(
                finalization_impl, "remove_private_acceptance_root",
                side_effect=remove_root,
            ))
            stack.enter_context(patch.object(initial_impl, "assert_state_supervisor_pair", return_value=old_libexec))
            stack.enter_context(patch.object(
                initial_impl, "verify_packaged_default_path",
                return_value={"fresh_bootstrap": True, "clean_shutdown": True},
            ))
            stack.enter_context(patch.object(capacity_entry_impl, "pueue_snapshot", return_value={
                "groups": {"default": {"parallel_tasks": 0}},
            }))
            observed_upgrade = stack.enter_context(patch.object(
                capacity_entry_impl, "verify_current_installation_upgrade", side_effect=upgrade,
            ))
            stack.enter_context(redirect_stdout(io.StringIO()))
            error = None
            try:
                private_cli.main()
            except RuntimeError as raised:
                error = raised
            finally:
                for owner in reversed(owners):
                    if not getattr(owner, "_transferred", False):
                        owner.release()
                _release_live_directory_tokens(pending_old_layout)
        return json.loads(output.read_text()), observed_upgrade, error


def _external_handoff_callback(failure, failure_case):
    def run_handoff(*args, **kwargs):
        case_label = kwargs["case_label"]
        case_config = args[7]
        case_task_ids = args[13]
        case_config.parent.mkdir(parents=True, exist_ok=True)
        case_config.write_text("fixture private config\n")
        case_task_ids.extend([case_label + "-task-a", case_label + "-task-b"])
        if failure is not None and (failure_case is None or case_label == failure_case):
            raise failure
        return {
            "scenario": private_cli.EXTERNAL_INSPECTION_HANDOFF_SCENARIO,
            "case": case_label,
            "handoff_action": kwargs["handoff_action"],
            "cleanup": {
                "fixture_waiter_entries": "pending",
                "lifetime_closure": "canonical-managed-validator",
                "external_daemon": "pending",
                "private_daemon": "pending",
            },
        }
    return run_handoff


def _current_scenario_callbacks(base, cleanup_failure, old_libexec, pending_old_layout, self, total, upgrade_failure):
    def run_json(_delegate, _env, args, **_options):
        command = args[2]
        if command == "dispatch":
            task_id = args[args.index("--id") + 1]
            task_dir = base / "state" / "tasks" / task_id
            task_dir.mkdir(parents=True)
            (task_dir / "meta.json").write_text(json.dumps({
                "client": str(old_libexec / "pueue"),
                "daemon": str(old_libexec / "pueued"),
            }))
            (task_dir / "outcome.json").write_text('{"verdict":"committed"}')
            supervisor = base / "state" / ".supervisor"
            supervisor.mkdir()
            (supervisor / "pueue.yml").write_text("fixture config\n")
            (supervisor / "daemon.identity.json").write_text(json.dumps({
                "executable": str(old_libexec / "pueued"),
                "sha256": hashlib.sha256((old_libexec / "pueued").read_bytes()).hexdigest(),
            }))
            (base / "old-daemon-starts").write_text("started\n")
            return {"task_id": task_id, "root_id": "b" * 32,
                    "task_record": "created", "admission": "admitted"}
        if command == "status":
            return {"task_id": args[3]}
        self.assertEqual(command, "collect")
        return {"outcome": {"verdict": "committed"}}

    upgrade = _capacity_upgrade_callback(base, pending_old_layout, self, total, upgrade_failure)
    def remove_root(root_token, entries, **_kwargs):
        if cleanup_failure:
            _release_live_directory_tokens(entries)
            raise RuntimeError("SECRET cleanup diagnostic")
        self.assertEqual(
            [entry[0] for entry in _kwargs["managed_state_roots"]],
            [base / "state"],
        )
        private_cli.rollback_owned_directories(entries)
    return run_json, upgrade, remove_root


def _capacity_upgrade_callback(base, pending_old_layout, self, total, upgrade_failure):
    def upgrade(*args, **kwargs):
        self.assertIsInstance(args[4], private_cli.LockedReleaseGate)
        self.assertEqual(args[4].path, base / "blocker-gate")
        self.assertGreaterEqual(args[4].descriptor, 0, "gate opened before concurrency proof")
        self.assertEqual(args[5], base / "provider-waiters")
        self.assertEqual(kwargs["client_call_log"], base / "old-client-starts-calls")
        requested_workers = args[12]
        old_layout_entries = tuple(args[8])
        args[14].update({
            "capacity_proof": {
                "requested_task_count": total,
                "observed_running_task_count": total,
                "peak_observed_running_task_count": total,
                "observed_lease_candidate_count": total,
                "peak_observed_lease_candidate_count": total,
                "observed_live_lease_count": total,
                "lease_protocol": "fifo-nonce-v1",
                "lease_nonce_sha256": "a" * 64,
                "queue_confirmed_while_leases_held": True,
                "status_bytes": 1024 * 1024 + 1,
                "proof_established_before_gate": True,
                "gate_opened_after_proof": True,
            },
            "dispatch_batch": {
                "requested_caller_concurrency": requested_workers,
                "observed_peak_in_flight_dispatch_calls": min(total, requested_workers),
                "completed_dispatch_call_count": total,
                "dispatch_elapsed_seconds": 0.001,
                "admission_elapsed_seconds": 0.001,
                "admitted_task_count": total,
            },
            "status_coalescing": {
                "concurrent_status_call_count": 16,
                "pueue_status_snapshot_count": 1,
                "shared_status_call_count": 15,
            },
        })
        if upgrade_failure:
            pending_old_layout.extend(old_layout_entries)
            tracker = kwargs["run_receipt"]
            tracker.phase("capacity_observation")
            tracker.update_counts(
                dispatch_completed_count=73,
                admitted_task_count=71,
                running_task_count=64,
                live_lease_count=63,
            )
            tracker.update_queue(
                task_count=73, running_count=64, status_bytes=4096,
            )
            raise RuntimeError("SECRET prompt credential unrestricted log")
        private_cli.rollback_owned_directories(old_layout_entries)
        args[4].release()
        return ["upgraded", "restarted"], 1024 * 1024 + 1
    return upgrade
