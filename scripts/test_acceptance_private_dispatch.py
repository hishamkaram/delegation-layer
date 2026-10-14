"""Dispatch arrival limits must not weaken the concurrent-running proof."""

from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack, redirect_stderr, redirect_stdout
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

import acceptance_private_cli as private_cli


class PrivateDispatchArgumentsTests(unittest.TestCase):
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

    def arguments(self, total, workers, output=None):
        output = output or (self.root / "receipt.json")
        args = ["acceptance_private_cli.py"]
        for name in ("delegate", "runner", "pueue", "pueued"):
            args.extend(["--" + name, str(self.sources[name])])
        args.extend(["--output", str(output),
                     "--parallel-tasks", str(total), "--runtime-only"])
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

        def upgrade(*args, **kwargs):
            self.assertIsInstance(args[4], private_cli.LockedReleaseGate)
            self.assertEqual(args[4].path, base / "blocker-gate")
            self.assertGreaterEqual(args[4].descriptor, 0, "gate opened before concurrency proof")
            self.assertEqual(args[5], base / "provider-waiters")
            self.assertEqual(kwargs["client_call_log"], base / "old-client-starts-calls")
            requested_workers = args[12]
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
            args[4].release()
            return ["upgraded", "restarted"], 1024 * 1024 + 1

        def remove_root(root_token, entries, **_kwargs):
            if cleanup_failure:
                raise RuntimeError("SECRET cleanup diagnostic")
            self.assertEqual(
                [entry[0] for entry in _kwargs["managed_state_roots"]],
                [base / "state"],
            )
            private_cli.rollback_owned_directories(entries)

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
            stack.enter_context(patch.object(private_cli, "run_json", side_effect=run_json))
            stack.enter_context(patch.object(private_cli, "shutdown_private_supervisor"))
            stack.enter_context(patch.object(
                private_cli, "remove_private_acceptance_root",
                side_effect=remove_root,
            ))
            stack.enter_context(patch.object(private_cli, "assert_state_supervisor_pair", return_value=old_libexec))
            stack.enter_context(patch.object(
                private_cli, "verify_packaged_default_path",
                return_value={"fresh_bootstrap": True, "clean_shutdown": True},
            ))
            stack.enter_context(patch.object(private_cli, "pueue_snapshot", return_value={
                "groups": {"default": {"parallel_tasks": 0}},
            }))
            observed_upgrade = stack.enter_context(patch.object(
                private_cli, "verify_current_installation_upgrade", side_effect=upgrade,
            ))
            stack.enter_context(redirect_stdout(io.StringIO()))
            error = None
            try:
                private_cli.main()
            except RuntimeError as raised:
                error = raised
        return json.loads(output.read_text()), observed_upgrade, error


class CreatedAdmissionTests(unittest.TestCase):
    def response(self):
        return {"task_id": "a" * 32, "task_record": "created", "admission": "unknown",
                "failure": {"code": "task_submission_uncertain", "next_action": "check_status"}}

    def observe(self, response):
        return private_cli.observe_created_admission(
            Path("/fixture/delegate"), Path("/fixture/state"), {}, response,
            Path("/fixture/dispatch-000.json"),
        )

    def test_admitted_submission_uses_dispatch_evidence_without_status(self):
        response = {"task_id": "a" * 32, "task_record": "created", "admission": "admitted"}
        with patch.object(private_cli, "run_json") as run:
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
        with patch.object(private_cli, "run_json", return_value=observed) as run:
            result = self.observe(response)
        self.assertIs(result, observed)
        run.assert_called_once()
        self.assertEqual(
            run.call_args.args[2],
            ["--root", "/fixture/state", "status", task_id, "--json"],
        )

    def test_uncertain_submission_only_observes_its_created_task(self):
        response = self.response()
        task_id = response["task_id"]
        observations = [{"task_id": task_id, "admission": "unknown", "error": "pueue observation is unknown"},
                        {"task_id": task_id, "admission": "admitted"}]
        with patch.object(private_cli, "run_json", side_effect=observations) as run, \
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
            with self.subTest(change=change), patch.object(private_cli, "run_json") as run:
                response = self.response()
                response.update(change)
                with self.assertRaises(RuntimeError):
                    self.observe(response)
                run.assert_not_called()

    def test_status_blocker_and_different_task_end_observation(self):
        for observed in ({"task_id": "a" * 32, "failure": {"next_action": "stop_and_report"}},
                         {"task_id": "b" * 32, "admission": "admitted"}):
            with self.subTest(observed=observed), patch.object(private_cli, "run_json", return_value=observed) as run:
                with self.assertRaises(RuntimeError):
                    self.observe(self.response())
                run.assert_called_once()

    def test_pending_observation_has_finite_deadline(self):
        pending = {"task_id": "a" * 32, "admission": "unknown", "pending": {"kind": "supervisor-command"}}
        with patch.object(private_cli, "run_json", return_value=pending) as run, \
                patch.object(private_cli.time, "monotonic", side_effect=[0, 0, 61, 61]), \
                patch.object(private_cli.time, "sleep"):
            with self.assertRaises(TimeoutError):
                self.observe(self.response())
        run.assert_called_once()

    def test_nonzero_status_cannot_confirm_admission(self):
        observed = {"task_id": "a" * 32, "admission": "admitted", "error": "receipt write failed"}
        captured = subprocess.CompletedProcess([], 1, json.dumps(observed).encode(), b"")
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(private_cli, "run_captured_command", return_value=captured) as run:
            receipt = Path(directory) / "dispatch-000.json"
            with self.assertRaisesRegex(RuntimeError, "exit=1"):
                private_cli.observe_created_admission(Path("/fixture/delegate"), Path("/fixture/state"), {},
                                                     self.response(), receipt)
            saved = json.loads((receipt.parent / "observation-dispatch-000-000.json").read_text())
            self.assertEqual(saved["exit_code"], 1)
            raw_stdout = json.dumps(observed).encode()
            self.assertEqual(saved["stdout_bytes"], len(raw_stdout))
            self.assertEqual(saved["stdout_sha256"], hashlib.sha256(raw_stdout).hexdigest())
            self.assertNotIn("stdout", saved)
        run.assert_called_once()


class PrivateWrapperEvidenceTests(unittest.TestCase):
    def test_wrapper_calls_are_phase_role_bound_and_compact(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "calls"
            path.write_text(
                "managed-runner\tclient\t--config /owned/pueue.yml start --group default\n"
                "ordinary-dispatch\tclient\t--config /owned/pueue.yml add --print-task-id\n"
            )

            records = private_cli.wrapper_call_records(path, "client", "upgraded")

            self.assertEqual(records[0]["phase"], "managed-runner")
            self.assertEqual(records[0]["role"], "client")
            self.assertEqual(records[0]["command"], "start")
            self.assertNotIn("arguments", records[0])
            self.assertEqual(records[0]["arguments_bytes"],
                             len("--config /owned/pueue.yml start --group default"))
            with self.assertRaisesRegex(RuntimeError, "startup control command"):
                private_cli.require_no_startup_control_calls(records)

    def test_private_root_deletion_requires_the_exact_owned_parent_child(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve(strict=True)
            parent = root / "parent"
            owned = parent / (private_cli.PRIVATE_ACCEPTANCE_PREFIX + "test")
            owner = private_cli.DirectoryOwner()
            owned.mkdir(parents=True)
            token = owner.adopt_created("private root", owned, parent)
            entries = owner.transfer()
            private_cli.remove_private_acceptance_root(token, entries)
            self.assertFalse(owned.exists())


class PrivateCapturedProcessTests(unittest.TestCase):
    class Process:
        def __init__(self, responses, returncode=0, kill_error=None):
            self.responses = list(responses)
            self.returncode = returncode
            self.kill_error = kill_error
            self.killed = False

        def communicate(self, timeout):
            response = self.responses.pop(0)
            if isinstance(response, BaseException):
                raise response
            return response

        def kill(self):
            if self.kill_error is not None:
                raise self.kill_error
            self.killed = True

    def test_timeout_force_reaps_owned_fixture_process_with_diagnostics(self):
        process = self.Process([
            subprocess.TimeoutExpired(["fixture"], 45, output="partial stdout", stderr="partial stderr"),
            ("final stdout", "final stderr"),
        ], returncode=-9)

        stdout, stderr, reaped, error = private_cli.reap_captured_process(process, 45)

        self.assertTrue(reaped)
        self.assertTrue(process.killed)
        self.assertEqual((stdout, stderr), ("final stdout", "final stderr"))
        self.assertIn("exceeded its 45s observation bound", str(error))
        self.assertIn("final stdout", str(error))

    def test_unreapable_fixture_process_is_reported_for_root_retention(self):
        process = self.Process([
            subprocess.TimeoutExpired(["fixture"], 45, output="partial", stderr="blocked"),
        ], returncode=None, kill_error=OSError("kill failed"))

        _, _, reaped, error = private_cli.reap_captured_process(process, 45)

        self.assertFalse(reaped)
        self.assertIn("could not be reaped", str(error))
        self.assertIn("partial", str(error))


class PrivateQueueEvidenceTests(unittest.TestCase):
    def test_migration_label_lookup_rejects_duplicate_queue_rows(self):
        label = "delegate:" + "b" * 32 + ":" + "a" * 32
        snapshot = {"tasks": {
            "first": {"label": label, "status": {"Queued": {}}},
            "second": {"label": label, "status": {"Queued": {}}},
        }}
        with self.assertRaisesRegex(RuntimeError, "duplicate task label"):
            private_cli.task_row_by_label(snapshot, label)


class PrivateGuardianEvidenceTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-guardian-proof-")
        self.addCleanup(temporary.cleanup)
        self.state = Path(temporary.name) / "state"
        self.root_id = "b" * 32
        self.task_id = "a" * 32
        self.response = {"root_id": self.root_id, "task_id": self.task_id}
        self.lease_nonce = "c" * 32
        self.lifetime = self.state / "tasks" / self.task_id / "lifetime"
        self.lifetime.mkdir(parents=True)
        self.waiters = self.state.parent / "waiters"
        self.waiters.mkdir()

    def write_active_records(self, launch_count=1):
        common = {"schema_version": 1, "root_id": self.root_id, "task_id": self.task_id}
        task_bytes = b'{"schema_version":1,"task":"capacity"}\n'
        task_digest = hashlib.sha256(task_bytes).hexdigest()
        meta_bytes = b'{"schema_version":1,"runner_ownership":"managed"}\n'
        meta_digest = hashlib.sha256(meta_bytes).hexdigest()
        owner_digest = private_cli.ordinary_lifetime_owner_digest(
            task_digest, meta_digest,
        )
        intent = {
            **common, "boot_id": "boot-token", "owner_kind": "task",
            "owner_sha256": owner_digest, "scope_kind": "process-group",
            "assurance": "cooperative", "scope_plan": "process-group-plan",
        }
        registration = {
            **common, "boot_id": "boot-token", "owner_kind": "task",
            "owner_sha256": owner_digest, "scope_kind": "process-group",
            "assurance": "cooperative", "scope_id": "process-group-scope",
        }
        registration_bytes = (json.dumps(registration, sort_keys=True) + "\n").encode()
        registration_digest = hashlib.sha256(registration_bytes).hexdigest()
        worker = {
            **common, "boot_id": "boot-token", "registration_sha256": registration_digest,
            "pid": 101, "process_start": "worker-start",
        }
        (self.lifetime.parent / "task.json").write_bytes(task_bytes)
        (self.lifetime.parent / "meta.json").write_bytes(meta_bytes)
        (self.lifetime / "intent.json").write_text(json.dumps(intent) + "\n")
        (self.lifetime / "registration.json").write_bytes(registration_bytes)
        (self.lifetime / "worker.json").write_text(json.dumps(worker) + "\n")
        (self.lifetime / "owner.lock").touch()
        for sequence in range(1, launch_count + 1):
            launch = {
                **common, "registration_sha256": registration_digest, "sequence": sequence,
                "pid": 101 + sequence, "process_start": "provider-start-%d" % sequence,
            }
            (self.lifetime / ("%020d.launch.json" % sequence)).write_text(
                json.dumps(launch) + "\n"
            )

    def test_active_guardian_requires_consistent_nonterminal_launch_evidence(self):
        self.assertEqual(
            private_cli.capacity_lease_specifications(
                self.state, [self.response], self.waiters, self.lease_nonce,
            ), []
        )
        self.write_active_records(launch_count=3)
        specifications = private_cli.capacity_lease_specifications(
            self.state, [self.response], self.waiters, self.lease_nonce,
        )
        self.assertEqual(len(specifications), 1)
        self.assertEqual(specifications[0]["pid"], 104)
        self.assertEqual(
            specifications[0]["path"],
            self.waiters / (self.lease_nonce + "-104.fifo"),
        )
        self.assertEqual(specifications[0]["scope_kind"], "process-group")
        self.assertEqual(specifications[0]["assurance"], "cooperative")
        self.assertEqual(
            specifications[0]["owner_lock"], self.lifetime / "owner.lock",
        )
        (self.lifetime / "terminated.json").write_text("{}\n")
        with self.assertRaisesRegex(RuntimeError, "terminated before"):
            private_cli.capacity_lease_specifications(
                self.state, [self.response], self.waiters, self.lease_nonce,
            )

    def test_active_guardian_rejects_changed_identity(self):
        self.write_active_records()
        launch_path = self.lifetime / "00000000000000000001.launch.json"
        launch = json.loads(launch_path.read_text())
        launch["task_id"] = "c" * 32
        launch_path.write_text(json.dumps(launch) + "\n")
        with self.assertRaisesRegex(RuntimeError, "changed identity"):
            private_cli.capacity_lease_specifications(
                self.state, [self.response], self.waiters, self.lease_nonce,
            )

    def test_stale_pid_marker_is_not_a_capacity_lease(self):
        self.write_active_records()
        stale_marker = self.waiters / "102"
        stale_marker.touch()

        specifications = private_cli.capacity_lease_specifications(
            self.state, [self.response], self.waiters, self.lease_nonce,
        )

        self.assertEqual(len(specifications), 1)
        self.assertNotEqual(specifications[0]["path"], stale_marker)
        self.assertFalse(specifications[0]["path"].exists())


class PrivateLifetimeOwnerCohortTests(unittest.TestCase):
    class Cohort:
        def __init__(self):
            self.assertions = 0

        def assert_live(self):
            self.assertions += 1

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-owner-cohort-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.lock_path = self.root / "owner.lock"
        self.lock_path.touch(mode=0o600)

    def specification(self):
        return {
            "root_id": "f" * 32,
            "task_id": "a" * 32,
            "owner_lock": self.lock_path,
            "owner_sha256": "b" * 64,
            "registration_sha256": "c" * 64,
            "scope_kind": "process-group",
            "assurance": "cooperative",
            "scope_plan_sha256": "d" * 64,
            "scope_id_sha256": "e" * 64,
        }

    @staticmethod
    def stop_holder(holder):
        if holder.poll() is None:
            holder.terminate()
        try:
            holder.wait(timeout=3)
        except subprocess.TimeoutExpired:
            holder.kill()
            holder.wait(timeout=3)

    def start_holder(self):
        ready = self.root / "ready"
        holder = subprocess.Popen([
            sys.executable, "-c",
            "import fcntl,pathlib,sys,time; "
            "stream=open(sys.argv[1],'r+'); "
            "fcntl.flock(stream.fileno(),fcntl.LOCK_EX); "
            "pathlib.Path(sys.argv[2]).write_text('ready'); time.sleep(30)",
            str(self.lock_path), str(ready),
        ])
        self.addCleanup(self.stop_holder, holder)
        deadline = time.monotonic() + 5
        while not ready.is_file():
            if holder.poll() is not None:
                self.fail("lifetime-owner lock holder exited before readiness")
            if time.monotonic() >= deadline:
                self.fail("lifetime-owner lock holder did not become ready")
            time.sleep(0.01)
        return holder

    def test_live_owner_lock_binds_native_scope_evidence(self):
        self.start_holder()
        cohort = self.Cohort()

        proof = private_cli.prove_lifetime_owner_cohort(
            [self.specification()], cohort,
        )

        self.assertGreaterEqual(proof["started_unix_ns"], 1)
        self.assertGreaterEqual(proof["ended_unix_ns"], proof["started_unix_ns"])
        self.assertGreaterEqual(proof["duration_ns"], 0)
        self.assertEqual(proof["lease_probe"], "exclusive-flock-contended")
        self.assertEqual(proof["counted_task_count"], 1)
        self.assertGreaterEqual(cohort.assertions, 2)
        self.assertEqual(proof["counted_tasks"], [{
            "root_id": "f" * 32,
            "task_id": "a" * 32,
            "owner_sha256": "b" * 64,
            "registration_sha256": "c" * 64,
            "scope_kind": "process-group",
            "assurance": "cooperative",
            "scope_plan_sha256": "d" * 64,
            "scope_id_sha256": "e" * 64,
            "owner_lock": {
                "device": self.lock_path.stat().st_dev,
                "inode": self.lock_path.stat().st_ino,
            },
        }])

    def test_unlocked_owner_file_cannot_prove_live_ownership(self):
        with self.assertRaisesRegex(RuntimeError, "no live lifetime owner"):
            private_cli.prove_lifetime_owner_cohort(
                [self.specification()], self.Cohort(),
            )


class PrivateLiveLeaseTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-live-lease-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.nonce = "d" * 32

    def test_stale_fifo_without_writer_fails_and_closes_its_descriptor(self):
        fifo = self.root / "stale.fifo"
        os.mkfifo(fifo, 0o600)
        opened = []
        real_open = private_cli.os.open

        def recording_open(*args, **kwargs):
            descriptor = real_open(*args, **kwargs)
            opened.append(descriptor)
            return descriptor

        with patch.object(private_cli.os, "open", side_effect=recording_open):
            with self.assertRaisesRegex(RuntimeError, "no live writer"):
                private_cli.LiveFIFOLease.acquire(
                    "a" * 32, 101, fifo, self.nonce, timeout=0.03,
                )
        self.assertEqual(len(opened), 1)
        with self.assertRaises(OSError):
            os.fstat(opened[0])

    def test_pi_fixture_holds_nonce_bound_fifo_until_gate_release(self):
        install = self.root / "bin"
        install.mkdir()
        waiters = self.root / "waiters"
        waiters.mkdir()
        gate = self.root / "gate"
        release_gate = private_cli.LockedReleaseGate.acquire(gate)
        self.addCleanup(release_gate.release)
        private_cli.install_pi_fixture(install, gate, waiters, self.nonce)
        process = subprocess.Popen(
            [str(install / "pi"), "--mode", "json"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        self.addCleanup(self.reap, process)
        process.stdin.write(b"DELEGATE_PRIVATE_UPGRADE_WAIT\n")
        process.stdin.close()
        process.stdin = None
        fifo = private_cli.lease_fifo_path(waiters, self.nonce, process.pid)
        for _ in range(200):
            if fifo.exists():
                break
            private_cli.time.sleep(0.01)
        self.assertTrue(fifo.exists(), "fixture did not create its lease FIFO")

        lease = private_cli.LiveFIFOLease.acquire(
            "a" * 32, process.pid, fifo, self.nonce, timeout=2,
        )
        try:
            self.assertIsNone(process.poll())
            lease.assert_live()
            release_gate.release()
            stdout, stderr = process.communicate(timeout=3)
            self.assertEqual(process.returncode, 0, stderr.decode(errors="replace"))
            self.assertIn(b"private supervisor acceptance passed", stdout)
        finally:
            lease.close()
        self.assertFalse(fifo.exists())

    def test_pi_fixture_gate_releases_writer_before_lease_reader_exists(self):
        install = self.root / "bin"
        install.mkdir()
        waiters = self.root / "waiters"
        waiters.mkdir()
        gate = self.root / "gate"
        release_gate = private_cli.LockedReleaseGate.acquire(gate)
        self.addCleanup(release_gate.release)
        private_cli.install_pi_fixture(install, gate, waiters, self.nonce)
        process = subprocess.Popen(
            [str(install / "pi"), "--mode", "json"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        self.addCleanup(self.reap, process)
        process.stdin.write(b"DELEGATE_PRIVATE_UPGRADE_WAIT\n")
        process.stdin.close()
        process.stdin = None
        fifo = private_cli.lease_fifo_path(waiters, self.nonce, process.pid)
        for _ in range(200):
            if fifo.exists():
                break
            private_cli.time.sleep(0.01)
        self.assertTrue(fifo.exists(), "fixture did not create its lease FIFO")

        release_gate.release()
        stdout, stderr = process.communicate(timeout=3)
        self.assertEqual(process.returncode, 0, stderr.decode(errors="replace"))
        self.assertIn(b"private supervisor acceptance passed", stdout)
        self.assertFalse(fifo.exists())

    def test_child_death_during_cohort_formation_fails_and_closes_all_fds(self):
        first_fifo = self.root / "first.fifo"
        second_fifo = self.root / "second.fifo"
        os.mkfifo(first_fifo, 0o600)
        os.mkfifo(second_fifo, 0o600)
        script = (
            "import os,sys,time\n"
            "fifo,nonce,delay,hold=sys.argv[1:]\n"
            "fd=os.open(fifo,os.O_WRONLY)\n"
            "time.sleep(float(delay))\n"
            "os.write(fd,(nonce+'\\t'+str(os.getpid())+'\\n').encode())\n"
            "sys.stdin.buffer.read(1) if hold == 'hold' else time.sleep(float(hold))\n"
            "os.close(fd)\n"
        )
        first = subprocess.Popen(
            [sys.executable, "-c", script, str(first_fifo), self.nonce, "0", "hold"],
            stdin=subprocess.PIPE,
        )
        second = subprocess.Popen(
            [sys.executable, "-c", script, str(second_fifo), self.nonce, "0.2", "hold"],
            stdin=subprocess.PIPE,
        )
        self.addCleanup(self.reap, first)
        self.addCleanup(self.reap, second)
        opened = []
        real_open = private_cli.os.open

        def recording_open(*args, **kwargs):
            descriptor = real_open(*args, **kwargs)
            opened.append(descriptor)
            if len(opened) == 2:
                first.stdin.write(b"x")
                first.stdin.flush()
                first.stdin.close()
            return descriptor

        specifications = [
            {"task_id": "a" * 32, "pid": first.pid, "path": first_fifo},
            {"task_id": "b" * 32, "pid": second.pid, "path": second_fifo},
        ]
        with patch.object(private_cli.os, "open", side_effect=recording_open):
            with self.assertRaisesRegex(RuntimeError, "died during lease cohort formation"):
                private_cli.LiveFIFOLeaseCohort.acquire(
                    specifications, self.nonce, timeout=2,
                )
        self.assertEqual(len(opened), 2)
        for descriptor in opened:
            with self.assertRaises(OSError):
                os.fstat(descriptor)

    @staticmethod
    def reap(process):
        if process.stdin is not None and not process.stdin.closed:
            process.stdin.close()
        try:
            process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=3)


class PackagedDefaultPathTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-packaged-default-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def test_command_construction_omits_every_global_path_override(self):
        brief = self.root / "brief.md"
        workspace = self.root / "workspace"
        task_id = "a" * 32

        commands = private_cli.packaged_default_commands(brief, workspace, task_id)

        self.assertEqual(commands["dispatch"], [
            "dispatch", "--provider", "pi:json", "--brief", str(brief),
            "--cwd", str(workspace), "--id", task_id, "--permission", "read-only",
            "--budget", private_cli.TASK_BUDGET, "--json",
        ])
        self.assertEqual(commands["status"], ["status", task_id, "--json"])
        self.assertEqual(commands["collect"], [
            "collect", task_id, "--watch", private_cli.COLLECT_WATCH, "--json",
        ])
        for command in commands.values():
            self.assertNotIn("--root", command)
            self.assertNotIn("--runner", command)
            self.assertNotIn("--pueue-config", command)

    def test_isolated_home_maps_to_each_supported_default_state_path(self):
        with patch.object(private_cli.platform, "system", return_value="Linux"):
            self.assertEqual(
                private_cli.isolated_default_state_path(self.root),
                self.root / ".config" / "delegation-layer",
            )
        with patch.object(private_cli.platform, "system", return_value="Darwin"):
            self.assertEqual(
                private_cli.isolated_default_state_path(self.root),
                self.root / "Library" / "Application Support" / "delegation-layer",
            )

    def test_adjacent_package_copies_exact_regular_binary_identities(self):
        sources = {}
        for index, name in enumerate(("delegate", "delegate-run", "pueue", "pueued")):
            source = self.root / ("source-" + name)
            source.write_bytes((name + "-%d" % index).encode())
            source.chmod(0o700)
            sources[name] = source
        owner = private_cli.DirectoryOwner()

        install = private_cli.install_adjacent_package(
            owner, sources, self.root / "package",
        )

        for name, source in sources.items():
            copied = install / name
            self.assertTrue(copied.is_file())
            self.assertFalse(copied.is_symlink())
            self.assertNotEqual(copied.stat().st_ino, source.stat().st_ino)
            self.assertEqual(copied.read_bytes(), source.read_bytes())
            self.assertEqual(
                private_cli.file_fingerprint(copied)["sha256"],
                private_cli.file_fingerprint(source)["sha256"],
            )


class RecordingReleaseGate:
    def __init__(self, path):
        self.path = Path(path)

    def release(self):
        self.path.touch(mode=0o600, exist_ok=True)


class PrivateDispatchConcurrencyTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-dispatch-proof-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.state = self.root / "state"
        self.brief = self.root / "brief.md"
        self.brief.write_text("ordinary fixture turn\n")
        self.gate = self.root / "gate"
        self.release_gate = RecordingReleaseGate(self.gate)
        self.waiters = self.root / "waiters"
        self.waiters.mkdir()
        self.old_layout = self.root / "old"
        self.old_layout.mkdir()
        owner = private_cli.DirectoryOwner()
        self.old_layout_owned = (owner.adopt_created(
            "old package root", self.old_layout, self.root),)
        self.total = 200
        self.task_ids = ["%032x" % (index + 1) for index in range(self.total)]
        self.lease_candidates = [
            self.waiters / ("candidate-%03d.fifo" % index)
            for index in range(self.total)
        ]
        for candidate in self.lease_candidates:
            candidate.touch()
        self.restart_id = "f" * 32
        self.lock = threading.Lock()
        self.dispatched = set()
        self.observed = set()
        self.collected = set()
        self.restarts = 0
        self.snapshots = 0
        self.all_running = False
        self.shutdown = False
        self.status_bytes = 1024 * 1024 + 1
        self.worker_counts = []
        self.evidence = {}
        self.client_call_log = self.root / "client-calls"
        self.status_snapshot_owners = 1
        self.dispatch_burst_barrier = threading.Barrier(2)
        self.upgraded_liveness = "running"
        self.upgraded_error = ""
        self.upgraded_status = ""
        self.upgraded_admission = "admitted"
        self.duplicate_capacity_label = False
        self.cohort_closed = False
        self.owner_proof_count = self.total
        self.failed_dispatch_index = None
        self.attempted_task_ids = []

    def test_orchestration_records_all_200_capacity_members_before_release(self):
        # This unit isolates receipt/orchestration logic. Live FIFO handshakes,
        # owner-lock contention, and 200 native workers are separate executable
        # tests and the mandatory private CLI acceptance gate.
        task_ids, status_bytes = self.run_upgrade()
        self.assertEqual(self.worker_counts, [2, 2, 16, 16])
        self.assertEqual(task_ids, [*self.task_ids, self.restart_id])
        self.assertEqual(status_bytes, self.status_bytes)
        self.assertEqual(self.dispatched, set(range(self.total)))
        self.assertEqual(self.observed, set(self.task_ids[:16]))
        self.assertEqual(self.collected, set(self.task_ids))
        self.assertEqual(self.snapshots, 4)
        self.assertEqual(self.restarts, 1)
        dispatch_batch = self.evidence["dispatch_batch"]
        self.assertEqual(dispatch_batch["requested_caller_concurrency"], 2)
        self.assertEqual(dispatch_batch["observed_peak_in_flight_dispatch_calls"], 2)
        self.assertEqual(dispatch_batch["completed_dispatch_call_count"], self.total)
        self.assertEqual(dispatch_batch["admitted_task_count"], self.total)
        self.assertGreaterEqual(dispatch_batch["dispatch_elapsed_seconds"], 0)
        self.assertGreaterEqual(
            dispatch_batch["admission_elapsed_seconds"],
            dispatch_batch["dispatch_elapsed_seconds"],
        )
        capacity_proof = self.evidence["capacity_proof"]
        self.assertEqual(capacity_proof["requested_task_count"], self.total)
        self.assertEqual(capacity_proof["observed_running_task_count"], self.total)
        self.assertEqual(capacity_proof["peak_observed_running_task_count"], self.total)
        self.assertEqual(capacity_proof["observed_lease_candidate_count"], self.total)
        self.assertEqual(capacity_proof["peak_observed_lease_candidate_count"], self.total)
        self.assertEqual(capacity_proof["observed_live_lease_count"], self.total)
        self.assertEqual(
            capacity_proof["observed_lifetime_owner_lease_count"], self.total,
        )
        self.assertEqual(capacity_proof["scope_kind_counts"], {"process-group": self.total})
        self.assertEqual(capacity_proof["assurance_counts"], {"cooperative": self.total})
        self.assertEqual(
            len(capacity_proof["lifetime_owner_overlap"]["counted_tasks"]), self.total,
        )
        self.assertEqual(
            capacity_proof["lifetime_owner_overlap"]["counted_task_count"], self.total,
        )
        self.assertEqual(
            capacity_proof["lifetime_owner_overlap"]["lease_probe"],
            "exclusive-flock-contended",
        )
        self.assertTrue(capacity_proof["queue_confirmed_while_leases_held"])
        self.assertTrue(capacity_proof["proof_established_before_gate"])
        self.assertTrue(capacity_proof["gate_opened_after_proof"])
        self.assertTrue(self.cohort_closed)
        self.assertEqual(self.evidence["status_coalescing"], {
            "concurrent_status_call_count": 16,
            "pueue_status_snapshot_count": 1,
            "shared_status_call_count": 15,
        })
        receipts = list((self.root / "dispatch-responses").glob("dispatch-[0-9][0-9][0-9].json"))
        self.assertEqual(len(receipts), self.total)
        saved_ids = {json.loads(path.read_text())["task_id"] for path in receipts}
        self.assertEqual(saved_ids, set(self.task_ids))
        self.assertEqual(
            json.loads((self.root / "dispatch-responses" / "dispatch-batch.json").read_text()),
            dispatch_batch,
        )
        self.assertEqual(
            json.loads((self.root / "dispatch-responses" / "capacity-proof.json").read_text()),
            capacity_proof,
        )
        self.assertEqual(
            json.loads((self.root / "dispatch-responses" / "status-coalescing.json").read_text()),
            self.evidence["status_coalescing"],
        )
        self.assertTrue(self.gate.exists())
        self.assertFalse(self.old_layout.exists())

    def test_small_status_cannot_pass_the_200_task_proof(self):
        self.status_bytes = 1024 * 1024
        with self.assertRaisesRegex(RuntimeError, "former 1 MiB status limit"):
            self.run_upgrade()
        self.assertEqual(self.worker_counts, [2, 2])
        self.assertEqual(len(self.dispatched), self.total)
        self.assertEqual(self.observed, set())
        self.assertEqual(self.collected, set())
        self.assertEqual(self.restarts, 0)
        self.assertFalse(self.gate.exists())
        self.assertTrue(self.old_layout.exists())

    def test_admitted_but_unobserved_task_cannot_pass_upgrade(self):
        self.upgraded_liveness = "undetermined"
        with self.assertRaisesRegex(RuntimeError, "did not observe its gated task running"):
            self.run_upgrade()
        self.assertFalse(self.gate.exists())
        self.assertEqual(self.collected, set())
        self.assertEqual(self.restarts, 0)

    def test_admitted_running_with_diagnostic_cannot_pass_upgrade(self):
        self.upgraded_error = "pueue command remains in flight"
        with self.assertRaisesRegex(RuntimeError, "did not observe its gated task running"):
            self.run_upgrade()
        self.assertFalse(self.gate.exists())
        self.assertEqual(self.collected, set())

    def test_timed_out_running_task_cannot_pass_upgrade(self):
        self.upgraded_status = "timed_out"
        with self.assertRaisesRegex(RuntimeError, "did not observe its gated task running"):
            self.run_upgrade()
        self.assertFalse(self.gate.exists())
        self.assertEqual(self.collected, set())

    def test_status_without_admitted_admission_cannot_pass_upgrade(self):
        self.upgraded_admission = "unknown"
        with self.assertRaisesRegex(RuntimeError, "did not observe its gated task running"):
            self.run_upgrade()
        self.assertFalse(self.gate.exists())
        self.assertEqual(self.collected, set())

    def test_duplicate_capacity_queue_label_fails_before_gate_opens(self):
        self.duplicate_capacity_label = True
        with self.assertRaisesRegex(RuntimeError, "duplicate task label"):
            self.run_upgrade()
        self.assertFalse(self.gate.exists())
        self.assertEqual(self.observed, set())
        self.assertEqual(self.collected, set())
        self.assertTrue(self.old_layout.exists())

    def test_multiple_scheduled_status_cohorts_still_prove_coalescing(self):
        self.status_snapshot_owners = 2
        self.run_upgrade()
        self.assertEqual(self.evidence["status_coalescing"], {
            "concurrent_status_call_count": 16,
            "pueue_status_snapshot_count": 2,
            "shared_status_call_count": 14,
        })

    def test_uncoalesced_status_burst_fails_acceptance(self):
        self.status_snapshot_owners = 16
        with self.assertRaisesRegex(RuntimeError, "did not coalesce queue snapshots"):
            self.run_upgrade()
        self.assertFalse(self.gate.exists())
        self.assertEqual(self.collected, set())

    def test_missing_status_snapshot_fails_acceptance(self):
        self.status_snapshot_owners = 0
        with self.assertRaisesRegex(RuntimeError, "did not coalesce queue snapshots"):
            self.run_upgrade()
        self.assertFalse(self.gate.exists())
        self.assertEqual(self.collected, set())

    def test_incomplete_lifetime_owner_cohort_cannot_open_gate(self):
        self.owner_proof_count = self.total - 1
        with self.assertRaisesRegex(RuntimeError, "did not bind every admitted task"):
            self.run_upgrade()
        self.assertFalse(self.gate.exists())
        self.assertEqual(self.collected, set())

    def test_partial_dispatch_failure_retains_every_created_task_identity_once(self):
        self.failed_dispatch_index = 10

        with self.assertRaisesRegex(RuntimeError, "injected dispatch failure"):
            self.run_upgrade()

        expected = {self.task_ids[index] for index in self.dispatched}
        self.assertGreater(len(expected), 0)
        self.assertNotIn(self.task_ids[self.failed_dispatch_index], expected)
        self.assertEqual(set(self.attempted_task_ids), expected)
        self.assertEqual(len(self.attempted_task_ids), len(expected))
        self.assertFalse(self.gate.exists())

    def run_upgrade(self):
        def pool(max_workers):
            self.worker_counts.append(max_workers)
            return ThreadPoolExecutor(max_workers=max_workers)

        with patch.object(private_cli, "ThreadPoolExecutor", side_effect=pool), \
                patch.object(private_cli, "run_json", side_effect=self.run_json), \
                patch.object(private_cli, "pueue_snapshot", side_effect=self.snapshot), \
                patch.object(private_cli, "capacity_lease_specifications",
                             side_effect=self.lease_specifications), \
                patch.object(private_cli.LiveFIFOLeaseCohort, "acquire",
                             side_effect=self.acquire_cohort), \
                patch.object(private_cli, "prove_lifetime_owner_cohort",
                             side_effect=self.prove_owner_cohort), \
                patch.object(private_cli, "shutdown_private_supervisor", side_effect=self.stop), \
                patch.object(private_cli.time, "sleep"), redirect_stdout(io.StringIO()):
            return private_cli.verify_current_installation_upgrade(
                Path("/fixture/delegate"), self.state, self.root, self.brief, self.release_gate,
                self.waiters, {},
                self.old_layout, self.old_layout_owned,
                Path("/fixture/pueue"), Path("/fixture/pueue.yml"),
                self.total, 2, self.root / "dispatch-responses", self.evidence,
                lease_nonce="e" * 32,
                client_call_log=self.client_call_log,
                attempted_task_ids=self.attempted_task_ids,
            )

    def lease_specifications(self, state, responses, waiter_directory, nonce):
        self.assertEqual(state, self.state)
        self.assertEqual(waiter_directory, self.waiters)
        self.assertEqual(len(responses), self.total)
        self.assertEqual(nonce, "e" * 32)
        return [
            {
                "task_id": task_id,
                "root_id": "b" * 32,
                "pid": index + 1,
                "path": self.lease_candidates[index],
                "owner_lock": self.root / (task_id + ".lock"),
                "owner_sha256": "%064x" % (index + 1),
                "registration_sha256": "%064x" % (index + 201),
                "scope_kind": "process-group",
                "assurance": "cooperative",
                "scope_plan_sha256": "%064x" % (index + 401),
                "scope_id_sha256": "%064x" % (index + 601),
            }
            for index, task_id in enumerate(self.task_ids)
        ]

    def prove_owner_cohort(self, specifications, cohort):
        self.assertEqual(len(specifications), self.total)
        cohort.assert_live()
        counted = specifications[:self.owner_proof_count]
        return {
            "started_unix_ns": 1,
            "ended_unix_ns": 3,
            "duration_ns": 2,
            "lease_probe": "exclusive-flock-contended",
            "counted_task_count": len(counted),
            "counted_tasks": [{
                "root_id": specification["root_id"],
                "task_id": specification["task_id"],
                "owner_sha256": specification["owner_sha256"],
                "registration_sha256": specification["registration_sha256"],
                "scope_kind": specification["scope_kind"],
                "assurance": specification["assurance"],
                "scope_plan_sha256": specification["scope_plan_sha256"],
                "scope_id_sha256": specification["scope_id_sha256"],
                "owner_lock": {"device": 1, "inode": index + 1},
            } for index, specification in enumerate(counted)],
        }

    def acquire_cohort(self, specifications, nonce):
        self.assertEqual(len(specifications), self.total)
        self.assertEqual(nonce, "e" * 32)
        owner = self

        class Cohort:
            def __init__(self):
                self.leases = [object()] * owner.total

            def assert_live(self):
                return None

            def close(self):
                owner.cohort_closed = True
                self.leases = []

        return Cohort()

    def run_json(self, _delegate, _env, args, receipt=None, **_options):
        self.assertNotIn("--dispatch-workers", args)
        command = args[2]
        if command == "dispatch":
            response = self.dispatch(args, receipt)
        elif command == "status":
            task_id = args[3]
            upgraded = receipt is not None and receipt.name.startswith("status-after-upgrade-")
            if upgraded:
                self.assertTrue(self.all_running)
                self.assertFalse(self.gate.exists())
                self.assertFalse(self.old_layout.exists())
                with self.lock:
                    self.assertNotIn(task_id, self.observed)
                    self.observed.add(task_id)
                    if len(self.observed) <= self.status_snapshot_owners:
                        with self.client_call_log.open("a") as calls:
                            calls.write("managed-runner-observation\tclient\t"
                                        "-c /fixture/pueue.yml status --json\n")
            response = {"task_id": task_id,
                        "admission": self.upgraded_admission if upgraded else "admitted",
                        "liveness": "running", "supervisor": {
                            "matched": True,
                            "state": self.upgraded_liveness if upgraded else "running"}}
            if upgraded and self.upgraded_error:
                response["error"] = self.upgraded_error
            if upgraded and self.upgraded_status:
                response["status"] = self.upgraded_status
        else:
            self.assertEqual(command, "collect")
            self.assertTrue(self.gate.exists())
            self.assertEqual(self.observed, set(self.task_ids[:16]))
            if args[3] != self.restart_id:
                with self.lock:
                    self.assertNotIn(args[3], self.collected)
                    self.collected.add(args[3])
            response = {"outcome": {"verdict": "committed"}}
        if receipt is not None:
            receipt.write_text(json.dumps(response))
        return response

    def dispatch(self, args, receipt):
        if receipt is not None and receipt.stem == "dispatch-restarted":
            self.assertTrue(self.shutdown)
            self.assertTrue(self.gate.exists())
            self.assertEqual(self.collected, set(self.task_ids))
            self.restarts += 1
            task_id = self.restart_id
        else:
            self.assertFalse(self.gate.exists())
            self.assertEqual(args[args.index("--budget") + 1], private_cli.CAPACITY_TASK_BUDGET)
            waiting_brief = Path(args[args.index("--brief") + 1])
            self.assertEqual(waiting_brief.read_text(), "DELEGATE_PRIVATE_UPGRADE_WAIT\n")
            index = int(receipt.stem.removeprefix("dispatch-"))
            if index == self.failed_dispatch_index:
                raise RuntimeError("injected dispatch failure")
            with self.lock:
                self.assertNotIn(index, self.dispatched, "dispatch response was retried")
                self.dispatched.add(index)
                wait_for_burst = len(self.dispatched) <= 2
            if wait_for_burst:
                self.dispatch_burst_barrier.wait(timeout=5)
            task_id = self.task_ids[index]
        return {"task_id": task_id, "root_id": "b" * 32,
                "task_record": "created", "admission": "admitted"}

    def snapshot(self, _client, _config, _env, metrics, phase=None):
        self.assertEqual(len(self.dispatched), self.total)
        self.assertFalse(self.gate.exists())
        self.assertTrue(self.old_layout.exists())
        self.snapshots += 1
        self.all_running = self.snapshots > 1
        metrics["status_bytes"] = self.status_bytes
        tasks = {
            str(index): {
                "label": "delegate:" + "b" * 32 + ":" + task_id,
                "status": {"Running" if self.all_running or index < self.total - 1 else "Queued": {}},
            }
            for index, task_id in enumerate(self.task_ids)
        }
        if self.duplicate_capacity_label:
            tasks["duplicate"] = {
                "label": "delegate:" + "b" * 32 + ":" + self.task_ids[0],
                "status": {"Running": {}},
            }
        return {"tasks": tasks}

    def stop(self, _client, _config, _env, **_kwargs):
        self.assertEqual(self.collected, set(self.task_ids))
        self.assertEqual(_kwargs["state"], self.state)
        self.assertEqual(set(_kwargs["task_ids"]), set(self.task_ids))
        self.shutdown = True


if __name__ == "__main__":
    unittest.main()
