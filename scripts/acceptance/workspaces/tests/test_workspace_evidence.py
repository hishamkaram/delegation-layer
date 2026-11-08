"""Deterministic on-disk proof tests for managed workspace lifetimes."""

import time
from copy import deepcopy
import fcntl
from pathlib import Path
import shutil
import tempfile
import unittest
import sys
from unittest import mock


repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.testing.record_fixtures import object_at

from scripts.acceptance.evidence import digests as evidence_digests
from scripts.acceptance.evidence import encoding as evidence_encoding
from scripts.acceptance.evidence import files as evidence_files
from scripts.acceptance.evidence import records as evidence_records
from scripts.acceptance.evidence import validation as evidence_validation
from scripts.acceptance.lifetime import closure as lifetime_closure
from scripts.acceptance.tasks import specification as tasks_specification
from scripts.acceptance.workspaces import specification as workspaces_specification

import scripts.acceptance.workspaces.cli as workspaces


ROOT = "a" * 32
WORKSPACE = "b" * 32
OPERATION = "c" * 32
REQUEST_SHA = "d" * 64
WORKER_SHA = "e" * 64
DEADLINE = "2026-09-15T00:01:00Z"
BOOT = "linux:00000000-0000-4000-8000-000000000001"
OTHER_BOOT = "linux:00000000-0000-4000-8000-000000000002"


def write_record(path: Path, value: object) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    path.write_bytes(evidence_encoding.canonical_go_json(value))
    path.chmod(0o600)


class WorkspaceEvidenceTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.state = Path(self.temporary.name) / "state"
        self.state.mkdir(mode=0o700)
        write_record(self.state / "root.json", {
            "schema_version": 1,
            "root_id": ROOT,
            "created_at": "2026-09-15T00:00:00Z",
        })

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_workspace_driver_waits_for_exact_capture_owner_and_wait(self) -> None:
        operation = self.write_native_records()
        wait_path = operation / "lifetime-worker-wait.json"
        wait_bytes = wait_path.read_bytes()
        wait_path.unlink()
        request_bytes = (operation / "request.json").read_bytes()
        context = mock.Mock(spec=workspaces.WorkspaceContext)
        context.state = self.state
        lock = operation / ".lifetime-owner.lock"
        lock.touch(mode=0o600)
        with lock.open("r+b") as held:
            fcntl.flock(held.fileno(), fcntl.LOCK_EX)

            def finish_capture(_delay: float) -> None:
                wait_path.write_bytes(wait_bytes)
                wait_path.chmod(0o600)
                fcntl.flock(held.fileno(), fcntl.LOCK_UN)

            with mock.patch.object(time, "sleep", side_effect=finish_capture) as wait:
                proof = workspaces._prove_lifetimes_closed(context, [])
        wait.assert_called_once()
        self.assertEqual(object_at(proof, "workspace", OPERATION, "managed_lifetime", "worker_wait")["result"], "success")
        self.assertEqual((operation / "request.json").read_bytes(), request_bytes)

    def test_workspace_driver_does_not_accept_persistent_capture_identity_fault(self) -> None:
        operation = self.write_native_records(owner_sha="f" * 64)
        request_bytes = (operation / "request.json").read_bytes()
        context = mock.Mock(spec=workspaces.WorkspaceContext)
        context.state = self.state
        with (
            mock.patch.object(time, "monotonic",
                              side_effect=[0, workspaces.WAIT_SECONDS]),
            mock.patch.object(time, "sleep") as wait,
        ):
            with self.assertRaisesRegex(RuntimeError, "closure remained incomplete.*intent binding mismatch for " + OPERATION):
                workspaces._prove_lifetimes_closed(context, [])
        wait.assert_not_called()
        self.assertEqual((operation / "request.json").read_bytes(), request_bytes)

    @staticmethod
    def add_artifact(operation: Path, name: str = "skip") -> Path:
        """Add one capture scratch file the Go owner creates and removes while active."""
        artifact = operation / "artifacts" / name
        artifact.parent.mkdir(mode=0o700, exist_ok=True)
        artifact.write_bytes(b"scratch")
        artifact.chmod(0o600)
        return artifact

    @staticmethod
    def vanish_before_lstat(target: Path) -> mock._patch:
        """Remove the target once after enumeration, then perform the real lstat."""
        real_lstat = Path.lstat
        removed = []

        def lstat(path: Path, *args: object, **kwargs: object) -> object:
            if path == target and not removed:
                removed.append(path)
                target.unlink()
            return real_lstat(path, *args, **kwargs)
        return mock.patch.object(Path, "lstat", autospec=True, side_effect=lstat)

    @staticmethod
    def refuse_iterdir(target: Path) -> mock._patch:
        """Deny enumerating the exact target directory, as an unreadable mode does."""
        real_iterdir = Path.iterdir

        def iterdir(path: Path) -> object:
            if path == target:
                raise PermissionError(13, "Permission denied", str(path))
            return real_iterdir(path)
        return mock.patch.object(Path, "iterdir", autospec=True, side_effect=iterdir)

    def test_scratch_removed_after_enumeration_is_retried_while_owner_is_active(self) -> None:
        operation = self.write_native_records()
        artifact = self.add_artifact(operation)
        scratch = "operation artifact " + OPERATION + "/artifacts/skip"
        with self.vanish_before_lstat(artifact), \
                self.assertRaisesRegex(evidence_validation.AcceptanceFailure, scratch) as raised:
            lifetime_closure.validate_managed_lifetime_closure(self.state)
        self.assertIsInstance(raised.exception.__cause__, FileNotFoundError)

        artifact = self.add_artifact(operation)
        wait_path = operation / "lifetime-worker-wait.json"
        wait_bytes = wait_path.read_bytes()
        wait_path.unlink()
        request_bytes = (operation / "request.json").read_bytes()
        context = mock.Mock(spec=workspaces.WorkspaceContext)
        context.state = self.state
        lock = operation / ".lifetime-owner.lock"
        lock.touch(mode=0o600)
        with lock.open("r+b") as held:
            fcntl.flock(held.fileno(), fcntl.LOCK_EX)

            def finish_capture(_delay: float) -> None:
                self.assertTrue((operation / "artifacts").is_dir())
                wait_path.write_bytes(wait_bytes)
                wait_path.chmod(0o600)
                fcntl.flock(held.fileno(), fcntl.LOCK_UN)

            with self.vanish_before_lstat(artifact), \
                    mock.patch.object(time, "sleep", side_effect=finish_capture) as wait:
                proof = workspaces._prove_lifetimes_closed(context, [])
        wait.assert_called_once()
        self.assertEqual(object_at(proof, "workspace", OPERATION, "managed_lifetime", "worker_wait")["result"], "success")
        self.assertEqual((operation / "request.json").read_bytes(), request_bytes)

    def test_unreadable_artifacts_fail_closed_instead_of_scanning_empty(self) -> None:
        operation = self.write_native_records()
        artifacts = self.add_artifact(operation).parent
        with self.refuse_iterdir(artifacts), \
                self.assertRaisesRegex(evidence_validation.AcceptanceFailure, "artifacts for " + OPERATION) as raised:
            lifetime_closure.validate_managed_lifetime_closure(self.state)
        self.assertIsInstance(raised.exception.__cause__, PermissionError)

        artifacts.chmod(0o000)
        try:
            try:
                tuple(artifacts.iterdir())
            except PermissionError:
                pass
            else:
                self.skipTest("this user can enumerate a mode-0 directory")
            with self.assertRaisesRegex(evidence_validation.AcceptanceFailure, "artifacts for " + OPERATION) as raised:
                lifetime_closure.validate_managed_lifetime_closure(self.state)
            self.assertIsInstance(raised.exception.__cause__, PermissionError)
        finally:
            artifacts.chmod(0o700)
        self.assertTrue((artifacts / "skip").is_file())

    def assert_closure_refused_at_deadline(self, message: str) -> None:
        operation = self.state / "workspace-operations" / OPERATION
        request_bytes = (operation / "request.json").read_bytes()
        context = mock.Mock(spec=workspaces.WorkspaceContext)
        context.state = self.state
        with (
            mock.patch.object(time, "monotonic",
                              side_effect=[0, workspaces.WAIT_SECONDS]),
            mock.patch.object(time, "sleep") as wait,
            self.assertRaisesRegex(RuntimeError, "closure remained incomplete.*" + message),
        ):
            workspaces._prove_lifetimes_closed(context, [])
        wait.assert_not_called()
        self.assertEqual((operation / "request.json").read_bytes(), request_bytes)

    def test_persistent_artifact_faults_are_refused_at_the_finite_deadline(self) -> None:
        operation = self.write_native_records()
        artifact = self.add_artifact(operation)
        with self.subTest("persistent permission"), self.refuse_iterdir(artifact.parent):
            self.assert_closure_refused_at_deadline("cannot inspect workspace operation artifacts")
        with self.subTest("nonregular artifact"):
            (artifact.parent / "delegate-overlay").mkdir(mode=0o700)
            self.assert_closure_refused_at_deadline("artifact is not a regular file")
            (artifact.parent / "delegate-overlay").rmdir()
        with self.subTest("missing worker wait"):
            (operation / "lifetime-worker-wait.json").unlink()
            self.assert_closure_refused_at_deadline("workspace operation lifetime worker Wait is missing")
        self.assertTrue(artifact.is_file())

    @staticmethod
    def supervisor() -> dict[str, object]:
        return {
            "client_executable": "/private/bin/pueue",
            "client_sha256": "1" * 64,
            "resolved_config_sha256": "2" * 64,
            "endpoint": "unix:/private/run/p.sock",
            "config_path": "/private/config/pueue.json",
            "config_digest": "3" * 64,
            "observed_version": "pueue fixture",
        }

    @classmethod
    def operation_request(cls, deadline: str = DEADLINE) -> dict[str, object]:
        return {
            "schema_version": 1,
            "root_id": ROOT,
            "operation_id": OPERATION,
            "workspace_id": WORKSPACE,
            "kind": "capture",
            "subject": {
                "kind": "task",
                "id": "1" * 32,
                "sha256": REQUEST_SHA,
            },
            "deadline": deadline,
            "worker_executable": "/private/bin/delegate-run",
            "worker_sha256": WORKER_SHA,
            "supervisor": cls.supervisor(),
        }

    def operation_directory(self, request: dict[str, object] | None = None) -> tuple[Path, str]:
        operation = self.state / "workspace-operations" / OPERATION
        operation.mkdir(mode=0o700, parents=True)
        request = request or self.operation_request()
        request_bytes = workspaces_specification._canonical_operation_request_bytes(
            request, ROOT, OPERATION)
        (operation / "request.json").write_bytes(request_bytes)
        (operation / "request.json").chmod(0o600)
        return operation, evidence_digests.sha(request_bytes)

    def write_owner_records(
            self, operation: Path, request_digest: str, owner_sha: str,
            deadline: str, boot_id: str) -> str:
        intent = {
            "schema_version": 1,
            "root_id": ROOT,
            "task_id": OPERATION,
            "owner_kind": "workspace",
            "owner_sha256": owner_sha,
            "scope_kind": "process-group",
            "assurance": "cooperative",
            "scope_plan": "workspace-fixture-plan",
            "boot_id": boot_id,
            "deadline": deadline,
            "created_at": "2026-09-15T00:00:01Z",
        }
        registration = {
            "schema_version": 1,
            "root_id": ROOT,
            "task_id": OPERATION,
            "owner_kind": "workspace",
            "owner_sha256": owner_sha,
            "scope_kind": "process-group",
            "assurance": "cooperative",
            "scope_id": "workspace-fixture-scope",
            "boot_id": boot_id,
            "deadline": deadline,
            "created_at": "2026-09-15T00:00:02Z",
        }
        write_record(operation / "lifetime-intent.json", intent)
        write_record(operation / "lifetime-registration.json", registration)
        write_record(operation / "start.json", {
            "schema_version": 1,
            "request_sha256": request_digest,
        })
        return evidence_digests.sha((operation / "lifetime-registration.json").read_bytes())

    def write_worker_and_launch_records(
            self, operation: Path, registration_digest: str,
            boot_id: str) -> tuple[str, dict[str, object]]:
        worker = {
            "schema_version": 1,
            "root_id": ROOT,
            "task_id": OPERATION,
            "registration_sha256": registration_digest,
            "pid": 2001,
            "process_start": "workspace-fixture-worker",
            "boot_id": boot_id,
            "started_at": "2026-09-15T00:00:03Z",
        }
        write_record(operation / "lifetime-worker.json", worker)
        worker_digest = evidence_digests.sha((operation / "lifetime-worker.json").read_bytes())
        launch = {
            "schema_version": 1,
            "root_id": ROOT,
            "task_id": OPERATION,
            "registration_sha256": registration_digest,
            "sequence": 1,
            "pid": 2002,
            "process_start": "workspace-fixture-launch",
            "pgid": 2002,
            "started_at": "2026-09-15T00:00:04Z",
        }
        write_record(operation / "lifetime-launch-00000000000000000001.json", launch)
        return worker_digest, launch

    def write_terminal_records(
            self, operation: Path, registration_digest: str, worker_digest: str,
            launch: dict[str, object], include_termination: bool,
            include_worker_wait: bool) -> None:
        if include_termination:
            write_record(operation / "lifetime-termination.json", {
                "schema_version": 1,
                "root_id": ROOT,
                "task_id": OPERATION,
                "registration_sha256": registration_digest,
                "launch_count": 1,
                "launch_set_sha256": evidence_digests.sha(evidence_encoding.canonical_go_json([launch])),
                "cause": "completed",
                "scope_empty": True,
                "observed_at": "2026-09-15T00:00:06Z",
            })
        if include_worker_wait:
            write_record(operation / "lifetime-worker-wait.json", {
                "schema_version": 1,
                "root_id": ROOT,
                "task_id": OPERATION,
                "registration_sha256": registration_digest,
                "worker_sha256": worker_digest,
                "child_pid": 2001,
                "result": "success",
                "exit_code": 0,
                "observed_at": "2026-09-15T00:00:05Z",
            })

    def write_native_records(
            self, *, include_termination: bool = True,
            include_worker_wait: bool = True,
            owner_sha: str | None = None,
            deadline: str = DEADLINE,
            boot_id: str = "workspace-fixture-boot",
            ) -> Path:
        request = self.operation_request(deadline)
        operation, request_digest = self.operation_directory(request)
        selected_owner_sha = request_digest if owner_sha is None else owner_sha
        registration_digest = self.write_owner_records(
            operation, request_digest, selected_owner_sha, deadline, boot_id)
        worker_digest, launch = self.write_worker_and_launch_records(
            operation, registration_digest, boot_id)
        self.write_terminal_records(
            operation, registration_digest, worker_digest, launch,
            include_termination, include_worker_wait)
        return operation

    def write_recovered_records(self, *, registered: bool, proof: str,
                                observed_boot: str = BOOT) -> Path:
        operation = self.write_native_records(include_worker_wait=False, boot_id=BOOT)
        if not registered:
            for name in ("lifetime-registration.json", "lifetime-worker.json",
                         "lifetime-launch-00000000000000000001.json",
                         "lifetime-termination.json", "start.json"):
                (operation / name).unlink()
        wait = {
            "schema_version": 1, "root_id": ROOT, "task_id": OPERATION,
            "registration_sha256": evidence_files.digest(operation / "lifetime-registration.json") if registered else "",
            "child_pid": 2001 if registered else 0, "result": "absent",
            "observed_at": "2026-09-14T00:00:00Z",
            "absence_proof": proof, "observed_boot_id": observed_boot,
        }
        if registered:
            wait["worker_sha256"] = evidence_files.digest(operation / "lifetime-worker.json")
        else:
            wait["intent_sha256"] = evidence_files.digest(operation / "lifetime-intent.json")
        write_record(operation / "lifetime-worker-wait.json", wait)
        return operation

    def test_exact_worker_absence_proves_recovery_without_success_or_clock_order(self) -> None:
        self.write_recovered_records(registered=True, proof="exact-worker")
        closure = object_at(lifetime_closure.validate_managed_lifetime_closure(self.state), "workspace", OPERATION)
        self.assertEqual(closure["mode"], "recovered")
        wait = object_at(closure, "managed_lifetime", "worker_wait")
        self.assertEqual(wait["result"], "absent")
        self.assertNotIn("exit_code", wait)

    def test_intent_only_recovery_requires_no_synthetic_native_facts(self) -> None:
        self.write_recovered_records(registered=False, proof="unregistered-disposed")
        native = object_at(lifetime_closure.validate_managed_lifetime_closure(self.state), "workspace", OPERATION, "managed_lifetime")
        self.assertIsNone(native["registration"])
        self.assertIsNone(native["worker"])
        self.assertIsNone(native["termination"])
        self.assertEqual(native["launches"], [])

    def test_intent_only_previous_boot_recovery_has_no_registration_or_termination(self) -> None:
        self.write_recovered_records(registered=False, proof="previous-boot", observed_boot=OTHER_BOOT)
        native = object_at(lifetime_closure.validate_managed_lifetime_closure(self.state), "workspace", OPERATION, "managed_lifetime")
        self.assertIsNone(native["registration"])
        self.assertIsNone(native["termination"])
        self.assertEqual(object_at(native, "worker_wait")["child_pid"], 0)

    def test_registered_previous_boot_recovery_allows_missing_worker_record(self) -> None:
        operation = self.write_recovered_records(registered=True, proof="previous-boot", observed_boot=OTHER_BOOT)
        (operation / "lifetime-worker.json").unlink()
        path = operation / "lifetime-worker-wait.json"
        wait = evidence_files.read_json(path)
        del wait["worker_sha256"]
        wait["child_pid"] = 0
        write_record(path, wait)
        native = object_at(lifetime_closure.validate_managed_lifetime_closure(self.state), "workspace", OPERATION, "managed_lifetime")
        self.assertIsNone(native["worker"])
        self.assertEqual(object_at(native, "worker_wait")["absence_proof"], "previous-boot")

    def test_legacy_previous_boot_termination_remains_readable_without_fake_wait(self) -> None:
        operation = self.write_native_records(include_worker_wait=False)
        path = operation / "lifetime-termination.json"
        termination = evidence_files.read_json(path)
        termination["cause"] = "previous-boot"
        write_record(path, termination)
        native = object_at(lifetime_closure.validate_managed_lifetime_closure(self.state), "workspace", OPERATION, "managed_lifetime")
        self.assertIsNone(native["worker_wait"])
        self.assertEqual(object_at(native, "termination")["cause"], "previous-boot")

    def test_captured_receipt_survives_previous_boot_completion_gap(self) -> None:
        operation = self.write_recovered_records(
            registered=True, proof="previous-boot", observed_boot=OTHER_BOOT)
        termination_path = operation / "lifetime-termination.json"
        termination = evidence_files.read_json(termination_path)
        termination["cause"] = "previous-boot"
        write_record(termination_path, termination)
        wait_path = operation / "lifetime-worker-wait.json"
        wait_bytes = wait_path.read_bytes()
        wait_path.unlink()
        artifacts = operation / "artifacts"
        artifacts.mkdir()
        patch = artifacts / "diff.patch"
        patch.write_bytes(b"diff --git a/example b/example\n")
        descriptors = [{"name": "diff.patch", "length": patch.stat().st_size,
                        "sha256": evidence_files.digest(patch)},
                       {"name": "name-status.z", "length": 0, "omitted": "limit"}]
        request = evidence_files.read_json(operation / "request.json")
        write_record(operation / "result.json", {
            "schema_version": 1, "operation_id": OPERATION,
            "request_sha256": evidence_files.digest(operation / "request.json"),
            "status": "succeeded", "artifacts": descriptors,
        })
        capture = {
            "status": "captured", "workspace_id": WORKSPACE, "operation_id": OPERATION,
            "lifetime_closure": "legacy-previous-boot",
            "termination_sha256": evidence_files.digest(termination_path),
            "candidate": {
                "workspace_id": WORKSPACE, "operation_id": OPERATION,
                "subject": request["subject"], "base_oid": "1" * 40,
                "observed_head": "2" * 40, "tree_oid": "3" * 40,
                "snapshot_ref": "refs/delegate/" + ROOT + "/workspaces/" + WORKSPACE + "/captures/" + OPERATION,
            },
            "artifacts": descriptors,
        }
        before = workspaces.verify_capture_receipt(self.state, capture)
        wait_path.write_bytes(wait_bytes)
        self.assertEqual(workspaces.verify_capture_receipt(self.state, capture), before)
        capture["termination_sha256"] = "f" * 64
        with self.assertRaisesRegex(RuntimeError, "termination does not match"):
            workspaces.verify_capture_receipt(self.state, capture)
        capture["termination_sha256"] = evidence_files.digest(termination_path)
        capture["worker_wait_sha256"] = evidence_files.digest(wait_path)
        with self.assertRaisesRegex(RuntimeError, "mixed closure"):
            workspaces.verify_capture_receipt(self.state, capture)

    def test_previous_boot_recovery_preserves_prior_completed_termination(self) -> None:
        self.write_recovered_records(registered=True, proof="previous-boot", observed_boot=OTHER_BOOT)
        native = object_at(lifetime_closure.validate_managed_lifetime_closure(self.state), "workspace", OPERATION, "managed_lifetime")
        self.assertEqual(object_at(native, "termination")["cause"], "completed")
        self.assertEqual(object_at(native, "worker_wait")["absence_proof"], "previous-boot")

    def test_previous_boot_cannot_be_inferred_from_unknown_or_mixed_tokens(self) -> None:
        operation = self.write_recovered_records(registered=True, proof="previous-boot")
        path = operation / "lifetime-worker-wait.json"
        wait = evidence_files.read_json(path)
        for observed in (BOOT, "different-boot", "darwin:2026-09-15T00:00:00Z",
                         "darwin:boot-session:00000000-0000-4000-8000-000000000002"):
            with self.subTest(observed=observed):
                wait["observed_boot_id"] = observed
                write_record(path, wait)
                with self.assertRaises(evidence_validation.AcceptanceFailure):
                    lifetime_closure.validate_managed_lifetime_closure(self.state)

    def test_recovered_completion_rejects_invented_exit_or_wrong_worker(self) -> None:
        operation = self.write_recovered_records(registered=True, proof="exact-worker")
        path = operation / "lifetime-worker-wait.json"
        original = evidence_files.read_json(path)
        for changes in ({"exit_code": 0}, {"child_pid": 7777}, {"worker_sha256": "f" * 64}):
            with self.subTest(changes=changes):
                write_record(path, {**original, **changes})
                with self.assertRaises(evidence_validation.AcceptanceFailure):
                    lifetime_closure.validate_managed_lifetime_closure(self.state)

    def test_intent_only_completion_rejects_native_start_guard(self) -> None:
        operation = self.write_recovered_records(registered=False, proof="unregistered-disposed")
        write_record(operation / "start.json", {
            "schema_version": 1, "request_sha256": evidence_files.digest(operation / "request.json"),
        })
        with self.assertRaises(evidence_validation.AcceptanceFailure):
            lifetime_closure.validate_managed_lifetime_closure(self.state)

    def add_operation_guards(self, operation: Path, request_digest: str) -> None:
        write_record(operation / "submission.json", {
            "schema_version": 1,
            "request_sha256": request_digest,
        })
        write_record(operation / "receipt.json", {
            "schema_version": 1,
            "request_sha256": request_digest,
            "numeric_task_id": 42,
        })
        write_record(operation / "result.json", {
            "schema_version": 1,
            "operation_id": OPERATION,
            "request_sha256": request_digest,
            "status": "succeeded",
        })

    def test_completed_workspace_helper_is_counted_without_ordinary_tasks(self) -> None:
        operation = self.write_native_records()
        self.add_operation_guards(operation, evidence_digests.sha((operation / "request.json").read_bytes()))

        closure = lifetime_closure.validate_managed_lifetime_closure(self.state)

        self.assertEqual(closure["tasks"], {})
        self.assertIn(OPERATION, object_at(closure, "workspace"))
        self.assertEqual(object_at(closure, "workspace", OPERATION)["mode"], "terminated")
        self.assertIsInstance(
            object_at(closure, "workspace", OPERATION, "managed_lifetime")["worker_wait"], dict)

    def test_active_owner_absent_termination_and_missing_wait_fail_closed(self) -> None:
        with self.subTest("active owner"):
            operation = self.write_native_records()
            lock = operation / ".lifetime-owner.lock"
            lock.write_bytes(b"")
            with lock.open("r+b") as held:
                fcntl.flock(held.fileno(), fcntl.LOCK_EX)
                with self.assertRaises(evidence_validation.AcceptanceFailure):
                    lifetime_closure.validate_managed_lifetime_closure(self.state)
                fcntl.flock(held.fileno(), fcntl.LOCK_UN)
            shutil.rmtree(operation)

        with self.subTest("absent termination"):
            operation = self.write_native_records()
            (operation / "lifetime-termination.json").unlink()
            with self.assertRaises(evidence_validation.AcceptanceFailure):
                lifetime_closure.validate_managed_lifetime_closure(self.state)
            shutil.rmtree(operation)

        with self.subTest("missing worker wait"):
            operation = self.write_native_records()
            (operation / "lifetime-worker-wait.json").unlink()
            with self.assertRaises(evidence_validation.AcceptanceFailure):
                lifetime_closure.validate_managed_lifetime_closure(self.state)

    def test_request_owner_and_deadline_mismatch_fail_closed(self) -> None:
        with self.subTest("owner digest"):
            operation = self.write_native_records(owner_sha="9" * 64)
            with self.assertRaises(evidence_validation.AcceptanceFailure):
                lifetime_closure.validate_managed_lifetime_closure(self.state)
            shutil.rmtree(operation)

        with self.subTest("deadline"):
            operation = self.write_native_records()
            intent = evidence_records._read_control_record(
                operation / "lifetime-intent.json", "fixture intent")
            intent["deadline"] = "2026-09-15T00:02:00Z"
            write_record(operation / "lifetime-intent.json", intent)
            with self.assertRaises(evidence_validation.AcceptanceFailure):
                lifetime_closure.validate_managed_lifetime_closure(self.state)
            shutil.rmtree(operation)

        with self.subTest("corrupt request"):
            operation = self.write_native_records()
            (operation / "request.json").write_bytes(b"{")
            with self.assertRaises(evidence_validation.AcceptanceFailure):
                lifetime_closure.validate_managed_lifetime_closure(self.state)

    def test_unlaunched_fence_is_distinct_and_rejects_native_facts(self) -> None:
        operation, request_digest = self.operation_directory()
        self.add_operation_guards(operation, request_digest)
        write_record(operation / "lifetime-unlaunched.json", {
            "schema_version": 1,
            "root_id": ROOT,
            "task_id": OPERATION,
            "owner_kind": "workspace",
            "owner_sha256": request_digest,
            "deadline": DEADLINE,
            "cause": "deadline-expired",
            "observed_at": "2026-09-15T00:01:01Z",
        })

        closure = lifetime_closure.validate_managed_lifetime_closure(self.state)
        evidence = object_at(closure, "workspace", OPERATION)
        self.assertEqual(evidence["mode"], "unlaunched")
        self.assertIn("unlaunched", evidence)
        self.assertNotIn("managed_lifetime", evidence)

        (operation / "lifetime-intent.json").write_bytes(b"{}")
        with self.assertRaises(evidence_validation.AcceptanceFailure):
            lifetime_closure.validate_managed_lifetime_closure(self.state)

    def test_active_unsubmitted_operation_is_not_treated_as_absent(self) -> None:
        operation, request_digest = self.operation_directory()
        lock = operation / ".lifetime-owner.lock"
        lock.write_bytes(b"")
        with lock.open("r+b") as held:
            fcntl.flock(held.fileno(), fcntl.LOCK_EX)
            with self.assertRaises(evidence_validation.AcceptanceFailure):
                lifetime_closure.validate_managed_lifetime_closure(self.state)
            fcntl.flock(held.fileno(), fcntl.LOCK_UN)
        write_record(operation / "submission.json", {
            "schema_version": 1,
            "request_sha256": request_digest,
        })
        with self.assertRaises(evidence_validation.AcceptanceFailure):
            lifetime_closure.validate_managed_lifetime_closure(self.state)

    def test_workspace_binding_uses_canonical_task_bytes_and_strict_values(self) -> None:
        task_id = "1" * 32
        task: dict[str, object] = {
            "schema_version": 1,
            "root_id": ROOT,
            "task_id": task_id,
            "provider": "fixture:test",
            "mode": "workspace-write",
            "canonical_cwd": "/private/workspace",
            "requested_config": {
                "permission": "workspace-write",
                "budget": "5m0s",
            },
            "budget_nanos": 300_000_000_000,
            "brief_sha256": "2" * 64,
            "brief_length": 1,
        }
        raw_without_binding = tasks_specification._canonical_task_bytes(task)
        self.assertNotIn(b'"workspace"', raw_without_binding)
        self.assertEqual(
            tasks_specification._validate_task_record(task, ROOT, task_id, "task"),
            evidence_digests.sha(raw_without_binding),
        )

        task["workspace"] = {
            "id": WORKSPACE,
            "reservation": 7,
            "request_sha256": "3" * 64,
            "budget_nanos": 300_000_000_000,
        }
        raw_with_binding = tasks_specification._canonical_task_bytes(task)
        self.assertIn(b'"workspace"', raw_with_binding)
        self.assertEqual(
            tasks_specification._validate_task_record(task, ROOT, task_id, "task"),
            evidence_digests.sha(raw_with_binding),
        )
        task_path = self.state / "tasks" / task_id / "task.json"
        write_record(task_path, task)
        stored_task = evidence_records._read_control_record(task_path, "fixture task")
        self.assertEqual(
            tasks_specification._validate_task_record(stored_task, ROOT, task_id, "task"),
            evidence_digests.sha(raw_with_binding),
        )

        for field, value in (
                ("budget_nanos", 0),
                ("budget_nanos", 1 << 63),
                ("reservation", 1 << 64),
                ("id", "not-an-id"),
                ("request_sha256", "not-a-digest"),
                ("budget_nanos", True),
        ):
            invalid = deepcopy(task)
            object_at(invalid, "workspace")[field] = value
            with self.subTest(field=field, value=value), self.assertRaises(
                    evidence_validation.AcceptanceFailure):
                tasks_specification._validate_task_record(invalid, ROOT, task_id, "task")

        invalid_task = deepcopy(task)
        invalid_task["budget_nanos"] = 1 << 63
        with self.assertRaises(evidence_validation.AcceptanceFailure):
            tasks_specification._validate_task_record(invalid_task, ROOT, task_id, "task")

    def test_operation_request_digest_is_the_canonical_on_disk_bytes(self) -> None:
        operation, request_digest = self.operation_directory()
        self.assertEqual(
            request_digest,
            evidence_digests.sha(workspaces_specification._canonical_operation_request_bytes(
                self.operation_request(), ROOT, OPERATION)),
        )
        request = self.operation_request()
        request["arguments"] = []
        with self.assertRaises(evidence_validation.AcceptanceFailure):
            workspaces_specification._canonical_operation_request_bytes(request, ROOT, OPERATION)
        request = self.operation_request()
        request["discard"] = False
        with self.assertRaises(evidence_validation.AcceptanceFailure):
            workspaces_specification._canonical_operation_request_bytes(request, ROOT, OPERATION)


if __name__ == "__main__":
    unittest.main()
