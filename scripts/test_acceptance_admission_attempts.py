"""Focused admission-attempt identity tests for the acceptance oracle."""

from __future__ import annotations

from copy import deepcopy
from pathlib import Path
from types import SimpleNamespace
import shlex
import tempfile
import unittest

import acceptance_provider_common as provider_common
import test_acceptance_inspection as inspection_fixtures
from acceptance_provider_common import AcceptanceFailure, NativeTaskOps, _inspection_group
from acceptance_supervisor_common import read_json, write_json


ROOT = "a" * 32
TASK = "b" * 32
WORKSPACE = "c" * 32
INITIAL_REQUEST = "d" * 64
REPLAY_REQUEST = "e" * 64


class AdmissionAttemptOracleTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="admission-attempt-oracle-")
        base = Path(self.temporary.name)
        self.base = base
        self.state = base / "state"
        self.state_parent = base / "state-parent"
        self.state.mkdir(mode=0o700)
        self.state_parent.mkdir(mode=0o700)
        write_json(self.state / "root.json", {
            "schema_version": 1,
            "root_id": ROOT,
            "created_at": "2026-09-15T00:00:00Z",
        })
        self.runner = base / "delegate-run"
        self.pueue = base / "pueue"
        self.config = base / "pueue.yml"
        self.runner.write_bytes(b"runner")
        self.pueue.write_bytes(b"pueue")
        self.config.write_bytes(b"config")
        self.runner.chmod(0o700)
        self.pueue.chmod(0o700)
        self.config.chmod(0o600)

    def tearDown(self):
        self.temporary.cleanup()

    @staticmethod
    def task_record(reservation: int = 1, request_sha256: str = INITIAL_REQUEST,
                    workspace: str = WORKSPACE, budget_nanos: int = 1) -> dict[str, object]:
        return {
            "schema_version": 1,
            "root_id": ROOT,
            "task_id": TASK,
            "provider": "fixture:test",
            "mode": "read-only",
            "canonical_cwd": "/workspace",
            "requested_config": {},
            "budget_nanos": budget_nanos,
            "brief_sha256": "0" * 64,
            "brief_length": 1,
            "workspace": {
                "id": workspace,
                "reservation": reservation,
                "request_sha256": request_sha256,
                "budget_nanos": budget_nanos,
            },
        }

    def write_workspace(self, sequence: int = 1, request_sha256: str = INITIAL_REQUEST,
                        closure: str | None = None, reservation_subject: dict[str, object] | None = None,
                        workspace: str = WORKSPACE) -> None:
        directory = self.state / "workspaces" / workspace
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        request = {
            "schema_version": 1,
            "root_id": ROOT,
            "workspace_id": workspace,
            "task_id": "f" * 32,
            "request_sha256": INITIAL_REQUEST,
            "source_cwd": "/workspace",
            "base_ref": "main",
            "container": "/workspace/container",
            "budget_nanos": 1,
        }
        (directory / "request.json").write_bytes(
            provider_common._canonical_workspace_resource_request_bytes(
                request, ROOT, workspace, "workspace request"))
        subject = reservation_subject or {
            "kind": "admission", "id": TASK, "sha256": request_sha256,
        }
        reservation = {
            "schema_version": 1,
            "workspace_id": workspace,
            "sequence": sequence,
            "subject": subject,
        }
        (directory / f"use-{sequence:020d}.json").write_bytes(
            provider_common.canonical_go_json(reservation))
        closure_path = directory / f"use-{sequence:020d}-closed.json"
        if closure is not None:
            closed = {
                "schema_version": 1,
                "workspace_id": workspace,
                "sequence": sequence,
                "disposition": closure,
            }
            closure_path.write_bytes(provider_common.canonical_go_json(closed))
        elif closure_path.exists():
            closure_path.unlink()

    def inspection_request(self, task_record: dict[str, object], operation_id: str | None) -> dict[str, object]:
        request = {
            "schema_version": 1,
            "root_id": ROOT,
            "task_id": TASK,
            "task": task_record,
            "task_sha256": provider_common.sha(provider_common._canonical_task_bytes(task_record)),
            "binding": {},
            "group": _inspection_group(ROOT),
            "label": _inspection_group(ROOT) + "-" + (operation_id or TASK),
            "created_at": "2026-09-15T00:00:00Z",
            "deadline": "2026-09-15T00:00:20Z",
        }
        if operation_id is not None:
            request["operation_id"] = operation_id
        return request

    def inspection_binding(self, config: Path | None = None,
                           concurrent: bool = False) -> dict[str, object]:
        config = config or self.config
        return {
            "definition_revision": (
                "runtime-capability-v1" if concurrent else "inspection-v1"),
            "definition_sha256": "1" * 64,
            "helper_executable": str(self.base / "helper"),
            "helper_sha256": "2" * 64,
            "worker_executable": str(self.runner),
            "worker_sha256": provider_common.sha(self.runner.read_bytes()),
            "supervisor": provider_common.supervisor_binding(
                self.pueue, config, config.parent,
                provider_common.sha(config.read_bytes()), "pueue fixture"),
            **({"capability_sha256": "3" * 64,
                "runner_ownership": "managed",
                "worker_exit_proof": "guardian-wait-v1"} if concurrent else {}),
        }

    def write_inspection(self, task_record: dict[str, object],
                         operation_id: str, binding: dict[str, object],
                         include_scheduling: bool = True) -> Path:
        directory = self.state / "inspections" / operation_id
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        request = self.inspection_request(task_record, operation_id)
        request["binding"] = binding
        request["group"] = ("default" if binding["definition_revision"] ==
                            "runtime-capability-v1" else _inspection_group(ROOT))
        request["label"] = provider_common._inspection_label(ROOT, operation_id)
        write_json(directory / "request.json", request)
        if include_scheduling:
            request_sha = provider_common.sha((directory / "request.json").read_bytes())
            write_json(directory / "receipt.json", {
                "schema_version": 1, "request_sha256": request_sha,
                "numeric_task_id": 42,
            })
            write_json(directory / "submission.json", {
                "schema_version": 1, "request_sha256": request_sha,
                "created_at": "2026-09-15T00:00:01Z",
            })
        return directory

    def write_durable_ordinary(self, task_record: dict[str, object],
                               binding: dict[str, object], numeric: int = 7) -> Path:
        directory = self.state / "tasks" / TASK
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        brief = b"x"
        task_record["brief_sha256"] = provider_common.sha(brief)
        (directory / "task.json").write_bytes(provider_common._canonical_task_bytes(task_record))
        (directory / "brief.md").write_bytes(brief)
        spec_sha = provider_common.sha((directory / "task.json").read_bytes())
        meta = {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "spec_sha256": spec_sha,
            "requested_config": task_record["requested_config"],
            "effective_config": {
                "containment": "fixture-local", "approval": "never",
                "digest": "4" * 64,
            },
            "containment": "fixture-local", "approval": "never",
            "provider_executable": str(self.base / "provider"),
            "provider_version": "fixture-v1",
            "publisher_build": "fixture-build", "publisher_version": "1",
            "predicate": {
                "adapter": task_record["provider"], "mode": task_record["mode"],
                "version": "fixture-v1", "sha256": "5" * 64,
            },
            "supervisor_config": binding["supervisor"],
            "created_at": "2026-09-15T00:00:02Z",
        }
        write_json(directory / "meta.json", meta)
        meta_sha = provider_common.sha((directory / "meta.json").read_bytes())
        label = f"delegate:{ROOT}:{TASK}"
        write_json(directory / "submit.json", {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "spec_sha256": spec_sha, "meta_sha256": meta_sha, "label": label,
            "supervisor": binding["supervisor"],
            "created_at": "2026-09-15T00:00:03Z",
        })
        write_json(directory / "supervisor.ref.json", {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "spec_sha256": spec_sha, "meta_sha256": meta_sha,
            "numeric_task_id": numeric, "label": label,
            **binding["supervisor"],
        })
        return directory

    def queue_ops(self, binding: dict[str, object]) -> NativeTaskOps:
        ops = NativeTaskOps(
            SimpleNamespace(), self.state_parent / "delegate", self.runner, self.pueue,
            self.state_parent, self.state, self.state_parent)
        ops.bind_supervisor(Path(binding["supervisor"]["config_path"]))
        ops.tasks = {"fresh": TASK}
        ops.labels = {"fresh": f"delegate:{ROOT}:{TASK}"}
        ops.numbers = {"fresh": 7}
        ops.expected_results = {"fresh": "Success"}
        ops.dispatch_attempts = {TASK}
        ops.inspection_attempts = {TASK}
        ops.inspection_bindings = {TASK: binding}
        return ops

    def queue_status(self, ops: NativeTaskOps, include_inspection: bool = False,
                     inspection_group: str | None = None,
                     inspection_operation: str | None = None) -> dict[str, object]:
        ordinary_label = f"delegate:{ROOT}:{TASK}"
        ordinary_argv = shlex.join([str(self.runner), "--root", str(self.state), TASK])
        rows: dict[str, object] = {
            "7": {
                "id": 7, "label": ordinary_label,
                "group": "default", "original_command": ordinary_argv,
                "command": ordinary_argv, "path": str(self.state),
                "status": {"Done": {"result": "Success"}},
            },
        }
        groups: dict[str, object] = {
            "default": {"status": "Running", "parallel_tasks": 1},
            _inspection_group(ROOT): {"status": "Running", "parallel_tasks": 1},
        }
        if inspection_group == "default":
            groups["default"] = {"status": "Running", "parallel_tasks": 0}
            groups.pop(_inspection_group(ROOT), None)
        if include_inspection:
            require_operation = inspection_operation
            self.assertIsNotNone(require_operation)
            label = provider_common._inspection_label(ROOT, require_operation)
            argv = shlex.join([
                str(self.runner), "--inspection", "--guardian", "--supervised",
                "--root", str(self.state), require_operation,
            ])
            rows["42"] = {
                "id": 42, "label": label, "group": inspection_group,
                "original_command": argv, "command": argv,
                "path": str(self.state_parent),
                "status": {"Done": {"result": "Success"}},
            }
        return {"tasks": rows, "groups": groups}

    @staticmethod
    def rewrite_lifetime_identity(directory: Path, operation_id: str) -> None:
        for path in directory.glob("lifetime-*.json"):
            record = read_json(path)
            record["task_id"] = operation_id
            path.write_bytes(provider_common.canonical_go_json(record))
        registration_path = directory / "lifetime-registration.json"
        registration = read_json(registration_path)
        registration_sha = provider_common.sha(registration_path.read_bytes())
        worker_path = directory / "lifetime-worker.json"
        worker = read_json(worker_path)
        worker["registration_sha256"] = registration_sha
        worker_path.write_bytes(provider_common.canonical_go_json(worker))
        worker_sha = provider_common.sha(worker_path.read_bytes())
        launch_paths = sorted(directory.glob("lifetime-launch-*.json"))
        launches = []
        for path in launch_paths:
            launch = read_json(path)
            launch["registration_sha256"] = registration_sha
            path.write_bytes(provider_common.canonical_go_json(launch))
            canonical_launch = {
                "schema_version": launch["schema_version"],
                "root_id": launch["root_id"],
                "task_id": launch["task_id"],
                "registration_sha256": launch["registration_sha256"],
                "sequence": launch["sequence"],
                "pid": launch["pid"],
                "process_start": launch["process_start"],
            }
            if launch.get("pgid", 0) != 0:
                canonical_launch["pgid"] = launch["pgid"]
            canonical_launch["started_at"] = launch["started_at"]
            launches.append(canonical_launch)
        termination_path = directory / "lifetime-termination.json"
        termination = read_json(termination_path)
        termination["registration_sha256"] = registration_sha
        termination["launch_set_sha256"] = provider_common.sha(
            provider_common.canonical_go_json(launches))
        termination_path.write_bytes(provider_common.canonical_go_json(termination))
        wait_path = directory / "lifetime-worker-wait.json"
        wait = read_json(wait_path)
        wait["registration_sha256"] = registration_sha
        wait["worker_sha256"] = worker_sha
        wait_path.write_bytes(provider_common.canonical_go_json(wait))

    @staticmethod
    def write_unlaunched_fence(directory: Path, operation_id: str) -> None:
        fence = {
            "schema_version": 1, "root_id": ROOT,
            "task_id": operation_id, "owner_kind": "inspection",
            "owner_sha256": provider_common.sha(
                (directory / "request.json").read_bytes()),
            "deadline": "2026-09-15T00:00:20Z",
            "cause": "deadline-expired",
            "observed_at": "2026-09-15T00:00:21Z",
        }
        (directory / "lifetime-unlaunched.json").write_bytes(
            provider_common.canonical_go_json(fence))

    def test_derived_id_matches_canonical_projection_and_rejects_wrong_id(self):
        self.write_workspace()
        task = self.task_record()
        attempt = provider_common._workspace_admission_attempt(
            self.state, ROOT, task, "ordinary admission")
        self.assertEqual(attempt["operation_id"], "2d299e0d7e6f79ed6ea4bfed37314c67")
        request = self.inspection_request(task, attempt["operation_id"])
        identity = provider_common._inspection_request_identity(
            self.state, ROOT, request, attempt["operation_id"], "inspection request")
        self.assertEqual(identity["task_id"], TASK)
        wrong = deepcopy(request)
        wrong["operation_id"] = "1" * 32
        with self.assertRaises(AcceptanceFailure):
            provider_common._inspection_request_identity(
                self.state, ROOT, wrong, wrong["operation_id"], "inspection request")

    def test_unknown_logical_task_is_not_an_attempt(self):
        other = "1" * 32
        directory = self.state / "inspections" / other
        directory.mkdir(mode=0o700, parents=True)
        task = deepcopy(self.task_record())
        task["task_id"] = other
        task.pop("workspace")
        request = self.inspection_request(task, None)
        request["task_id"] = other
        write_json(directory / "request.json", request)
        ops = NativeTaskOps(
            SimpleNamespace(), self.state_parent / "delegate", self.runner, self.pueue,
            self.state_parent, self.state, self.state_parent)
        ops.dispatch_attempts = {TASK}
        with self.assertRaisesRegex(AcceptanceFailure, "unattempted task"):
            ops._inspection_journal_paths()

    def test_conflicting_reservation_sequence_workspace_and_request_are_rejected(self):
        cases = (
            ("reservation subject", {"reservation": 1}),
            ("sequence", {"reservation": 2}),
            ("workspace", {"workspace": "1" * 32}),
            ("request digest", {"request_sha256": REPLAY_REQUEST}),
        )
        for name, changes in cases:
            with self.subTest(name=name):
                self.write_workspace()
                task = self.task_record()
                if name == "reservation subject":
                    self.write_workspace(reservation_subject={
                        "kind": "admission", "id": TASK, "sha256": REPLAY_REQUEST,
                    })
                else:
                    binding = task["workspace"]
                    self.assertIsInstance(binding, dict)
                    binding = deepcopy(binding)
                    binding.update(changes)
                    task["workspace"] = binding
                with self.assertRaises(AcceptanceFailure):
                    provider_common._workspace_admission_attempt(
                        self.state, ROOT, task, f"conflicting {name}")

    def test_stored_reservation_and_closure_sequences_require_canonical_uint64_ints(self):
        for malformed in (True, 1.0):
            with self.subTest(record="reservation", value=malformed):
                self.write_workspace()
                reservation_path = (
                    self.state / "workspaces" / WORKSPACE / "use-00000000000000000001.json")
                reservation = {
                    "schema_version": 1,
                    "workspace_id": WORKSPACE,
                    "sequence": malformed,
                    "subject": {"kind": "admission", "id": TASK, "sha256": INITIAL_REQUEST},
                }
                reservation_path.write_bytes(provider_common.canonical_go_json(reservation))
                with self.assertRaises(AcceptanceFailure):
                    provider_common._workspace_admission_attempt(
                        self.state, ROOT, self.task_record(), "malformed reservation")

            with self.subTest(record="closure", value=malformed):
                self.write_workspace(closure="aborted-not-launched")
                closure_path = (
                    self.state / "workspaces" / WORKSPACE /
                    "use-00000000000000000001-closed.json")
                closure = {
                    "schema_version": 1,
                    "workspace_id": WORKSPACE,
                    "sequence": malformed,
                    "disposition": "aborted-not-launched",
                }
                closure_path.write_bytes(provider_common.canonical_go_json(closure))
                with self.assertRaises(AcceptanceFailure):
                    provider_common._workspace_admission_attempt(
                        self.state, ROOT, self.task_record(), "malformed closure")

    def test_queue_oracle_uses_effective_id_for_label_and_argv(self):
        operation_id = "2d299e0d7e6f79ed6ea4bfed37314c67"
        ops = NativeTaskOps(
            SimpleNamespace(), self.state_parent / "delegate", self.runner, self.pueue,
            self.state_parent, self.state, self.state_parent)
        ops.bind_supervisor(self.config)
        journal = {
            "task_id": TASK,
            "operation_id": operation_id,
            "label": _inspection_group(ROOT) + "-" + operation_id,
            "group": _inspection_group(ROOT),
            "numeric_task_id": 42,
            "result": {"reason": "eligible"},
        }
        argv = [str(self.runner), "--inspection", "--guardian", "--supervised",
                "--root", str(self.state), operation_id]
        row = {
            "id": 42,
            "label": journal["label"],
            "group": journal["group"],
            "original_command": shlex.join(argv),
            "command": shlex.join(argv),
            "path": str(self.state_parent),
            "status": {"Done": {"result": "Success"}},
        }
        ops._validate_inspection_queue_row(row, journal, allow_failure=False)
        wrong_argv = deepcopy(row)
        wrong_argv["command"] = shlex.join(argv[:-1] + [TASK])
        with self.assertRaises(AcceptanceFailure):
            ops._validate_inspection_queue_row(wrong_argv, journal, allow_failure=False)
        wrong_label = deepcopy(row)
        wrong_label["label"] = _inspection_group(ROOT) + "-" + TASK
        with self.assertRaises(AcceptanceFailure):
            ops._validate_inspection_queue_row(wrong_label, journal, allow_failure=False)

    def test_failure_queue_requires_historical_guardian_closure_or_terminal_row(self):
        binding = self.inspection_binding()
        old_task = self.task_record(reservation=1, request_sha256=INITIAL_REQUEST)
        old_task["brief_sha256"] = provider_common.sha(b"x")
        self.write_workspace(sequence=1, request_sha256=INITIAL_REQUEST,
                             closure="aborted-not-launched")
        old_attempt = provider_common._workspace_admission_attempt(
            self.state, ROOT, old_task, "old admission")
        old_directory = self.write_inspection(old_task, old_attempt["operation_id"], binding)

        current_task = self.task_record(reservation=2, request_sha256=INITIAL_REQUEST)
        current_task["brief_sha256"] = provider_common.sha(b"x")
        self.write_workspace(sequence=2, request_sha256=INITIAL_REQUEST,
                             closure="operation-completed")
        self.write_durable_ordinary(current_task, binding)
        ops = self.queue_ops(binding)
        status = self.queue_status(ops)

        # The old reservation closure proves only workspace admission abort;
        # it is not an inspection-helper closure. A guardian-unstarted
        # historical journal still needs its terminal queue row.
        self.assertFalse(ops.failure_queue_finished(status))

        self.write_unlaunched_fence(old_directory, old_attempt["operation_id"])

        self.assertTrue(ops.failure_queue_finished(status))

    def managed_history(self) -> tuple[dict[str, object], Path, NativeTaskOps]:
        supervisor = self.state / ".supervisor"
        supervisor.mkdir(mode=0o700)
        config = supervisor / "pueue.yml"
        config.write_bytes(b"private config")
        config.chmod(0o600)
        binding = self.inspection_binding(config, concurrent=True)
        old = self.task_record(reservation=1)
        old["brief_sha256"] = provider_common.sha(b"x")
        self.write_workspace(sequence=1, closure="aborted-not-launched")
        attempt = provider_common._workspace_admission_attempt(
            self.state, ROOT, old, "historical admission")
        directory = self.write_inspection(old, attempt["operation_id"], binding)
        current = self.task_record(reservation=2)
        self.write_workspace(sequence=2, closure="operation-completed")
        self.write_durable_ordinary(current, binding)
        return binding, directory, self.queue_ops(binding)

    def write_helper_result(self, directory: Path, reason: str | None) -> None:
        request = read_json(directory / "request.json")
        request_sha = provider_common.sha((directory / "request.json").read_bytes())
        write_json(directory / "start.json", {
            "schema_version": 1, "request_sha256": request_sha,
            "created_at": "2026-09-15T00:00:02Z",
        })
        if reason is not None:
            write_json(directory / "result.json", {
                "schema_version": 1, "request_sha256": request_sha,
                "reason": reason, "facts": {"eligible": True} if reason == "eligible" else {},
            })
            write_json(directory / "completion.json", {
                "schema_version": 1, "request_sha256": request_sha,
                "result_sha256": provider_common.sha((directory / "result.json").read_bytes()),
                "completed_at": "2026-09-15T00:00:08Z",
                "native_exit": "successful-exit" if reason == "eligible" else "unavailable",
            })
        inspection_fixtures.InspectionHarnessTests.managed_inspection_lifetime(
            directory, request, request_sha, reason or "unavailable")
        self.rewrite_lifetime_identity(directory, directory.name)

    @staticmethod
    def replace_request_fixture(directory: Path, request: dict[str, object]) -> None:
        request_bytes = provider_common.canonical_go_json(request)
        (directory / "request.json").write_bytes(request_bytes)
        for name in ("submission.json", "receipt.json"):
            path = directory / name
            if path.exists():
                record = read_json(path)
                record["request_sha256"] = provider_common.sha(request_bytes)
                path.write_bytes(provider_common.canonical_go_json(record))

    def test_managed_history_cannot_use_a_terminal_queue_row_as_native_closure(self):
        _, directory, ops = self.managed_history()
        status = self.queue_status(
            ops, include_inspection=True, inspection_group="default",
            inspection_operation=directory.name)
        status["tasks"]["42"]["status"] = {"Done": {"result": {"Failed": 1}}}
        self.assertFalse(ops.failure_queue_finished(status))
        with self.assertRaisesRegex(AcceptanceFailure, "historical inspection closure is absent"):
            ops.validate_inspection_journals(allow_failure=True)

    def test_submission_only_fenced_history_is_valid_but_receipt_only_is_not(self):
        _, directory, ops = self.managed_history()
        self.write_unlaunched_fence(directory, directory.name)
        receipt = (directory / "receipt.json").read_bytes()
        (directory / "receipt.json").unlink()
        journals = ops.validate_inspection_journals(allow_failure=True)
        self.assertTrue(journals[TASK]["historical_aborted"])
        self.assertIsNone(journals[TASK]["receipt"])
        self.assertIsNotNone(journals[TASK]["submission"])
        (directory / "receipt.json").write_bytes(receipt)
        (directory / "submission.json").unlink()
        with self.assertRaisesRegex(AcceptanceFailure, "scheduling records are incomplete"):
            ops.validate_inspection_journals(allow_failure=True)

    def test_repeated_aborted_attempts_without_an_ordinary_task_are_history(self):
        binding = self.inspection_binding()
        directories = []
        for sequence in (1, 2):
            self.write_workspace(sequence=sequence, closure="aborted-not-launched")
            record = self.task_record(reservation=sequence)
            attempt = provider_common._workspace_admission_attempt(
                self.state, ROOT, record, "aborted admission")
            directory = self.write_inspection(record, attempt["operation_id"], binding)
            self.write_unlaunched_fence(directory, directory.name)
            directories.append(directory)
        ops = self.queue_ops(binding)
        journals = ops.validate_inspection_journals(allow_failure=True)
        selected = journals[TASK]
        self.assertEqual(selected["operation_id"], directories[-1].name)
        self.assertTrue(selected["historical_aborted"])
        self.assertFalse(selected["ordinary_task_present"])
        self.assertEqual(len(selected["historical_attempts"]), 1)
        request = read_json(directories[0] / "request.json")
        request["task"]["budget_nanos"] = 2
        request["task_sha256"] = provider_common.sha(
            provider_common._canonical_task_bytes(request["task"]))
        self.replace_request_fixture(directories[0], request)
        self.write_unlaunched_fence(directories[0], directories[0].name)
        with self.assertRaisesRegex(AcceptanceFailure, "differs from aborted admission"):
            ops.validate_inspection_journals(allow_failure=True)

    def test_aborted_admission_preserves_independently_successful_inspection(self):
        _, directory, ops = self.managed_history()
        self.write_helper_result(directory, "eligible")
        journal = ops.validate_inspection_journals(allow_failure=True)[TASK]
        self.assertTrue(journal["historical_aborted"])
        self.assertEqual(journal["result"]["reason"], "eligible")
        self.assertEqual(journal["managed_lifetime"]["worker_wait"]["result"], "success")

    def test_aborted_admission_preserves_started_helper_without_result(self):
        _, directory, ops = self.managed_history()
        self.write_helper_result(directory, None)
        journal = ops.validate_inspection_journals(allow_failure=True)[TASK]
        self.assertTrue(journal["historical_aborted"])
        self.assertIsNotNone(journal["start"])
        self.assertIsNone(journal["result"])
        self.assertEqual(journal["managed_lifetime"]["worker_wait"]["result"], "exit-error")

    def test_aborted_admission_keeps_eligible_candidate_before_completion_crash(self):
        _, directory, ops = self.managed_history()
        self.write_helper_result(directory, "eligible")
        (directory / "completion.json").unlink()
        path = directory / "lifetime-worker-wait.json"
        wait = read_json(path)
        wait["result"], wait["exit_code"] = "exit-error", 1
        path.write_bytes(provider_common.canonical_go_json(wait))
        journal = ops.validate_inspection_journals(allow_failure=True)[TASK]
        self.assertTrue(journal["historical_aborted"])
        self.assertEqual(journal["result"]["reason"], "eligible")
        self.assertIsNone(journal["completion"])
        self.assertEqual(journal["managed_lifetime"]["worker_wait"]["result"], "exit-error")

    def test_aborted_admission_keeps_previous_boot_closure_without_native_wait(self):
        _, directory, ops = self.managed_history()
        self.write_helper_result(directory, "eligible")
        (directory / "lifetime-worker-wait.json").unlink()
        path = directory / "lifetime-termination.json"
        termination = read_json(path)
        termination["cause"] = "previous-boot"
        path.write_bytes(provider_common.canonical_go_json(termination))
        journal = ops.validate_inspection_journals(allow_failure=True)[TASK]
        self.assertTrue(journal["historical_aborted"])
        self.assertEqual(journal["result"]["reason"], "eligible")
        self.assertIsNone(journal["managed_lifetime"]["worker_wait"])

    def test_completed_scope_without_wait_is_still_invalid_history(self):
        _, directory, ops = self.managed_history()
        self.write_helper_result(directory, "eligible")
        (directory / "lifetime-worker-wait.json").unlink()
        with self.assertRaisesRegex(AcceptanceFailure, "worker Wait is missing"):
            ops.validate_inspection_journals(allow_failure=True)

    def test_saved_serial_runtime_inspection_keeps_its_validated_group(self):
        _, directory, ops = self.managed_history()
        request = read_json(directory / "request.json")
        request["group"] = _inspection_group(ROOT)
        self.replace_request_fixture(directory, request)
        self.write_helper_result(directory, "eligible")
        journal = ops.validate_inspection_journals(allow_failure=True)[TASK]
        self.assertEqual(journal["group"], _inspection_group(ROOT))
        self.assertEqual(journal["group_parallelism"], 1)

    def test_successful_retry_keeps_positive_fenced_history_and_current_queue_proof(self):
        binding = self.inspection_binding()
        old_first = self.task_record(reservation=1, request_sha256=INITIAL_REQUEST)
        old_second = self.task_record(reservation=2, request_sha256=INITIAL_REQUEST)
        current = self.task_record(reservation=3, request_sha256=INITIAL_REQUEST)
        for task in (old_first, old_second, current):
            task["brief_sha256"] = provider_common.sha(b"x")
        self.write_workspace(sequence=1, request_sha256=INITIAL_REQUEST,
                             closure="aborted-not-launched")
        self.write_workspace(sequence=2, request_sha256=INITIAL_REQUEST,
                             closure="aborted-not-launched")
        self.write_workspace(sequence=3, request_sha256=INITIAL_REQUEST,
                             closure="operation-completed")
        first_attempt = provider_common._workspace_admission_attempt(
            self.state, ROOT, old_first, "first historical admission")
        second_attempt = provider_common._workspace_admission_attempt(
            self.state, ROOT, old_second, "second historical admission")
        current_attempt = provider_common._workspace_admission_attempt(
            self.state, ROOT, current, "current admission")

        first_directory = self.write_inspection(
            old_first, first_attempt["operation_id"], binding,
            include_scheduling=False)
        self.write_unlaunched_fence(first_directory, first_attempt["operation_id"])
        second_directory = self.write_inspection(
            old_second, second_attempt["operation_id"], binding)
        self.write_unlaunched_fence(second_directory, second_attempt["operation_id"])

        ordinary = self.state / "tasks" / TASK
        ordinary.mkdir(mode=0o700, parents=True)
        (ordinary / "task.json").write_bytes(provider_common._canonical_task_bytes(current))
        (ordinary / "brief.md").write_bytes(b"x")
        current_directory = self.write_inspection(
            current, current_attempt["operation_id"], binding)
        current_request_sha = provider_common.sha(
            (current_directory / "request.json").read_bytes())
        write_json(current_directory / "start.json", {
            "schema_version": 1, "request_sha256": current_request_sha,
            "created_at": "2026-09-15T00:00:02Z",
        })
        write_json(current_directory / "result.json", {
            "schema_version": 1, "request_sha256": current_request_sha,
            "reason": "eligible", "facts": {"eligible": True},
        })
        result_sha = provider_common.sha((current_directory / "result.json").read_bytes())
        write_json(current_directory / "completion.json", {
            "schema_version": 1, "request_sha256": current_request_sha,
            "result_sha256": result_sha,
            "completed_at": "2026-09-15T00:00:02Z",
            "native_exit": "successful-exit",
        })
        write_json(current_directory / "worker-observation.json", {
            "schema_version": 1, "request_sha256": current_request_sha,
            "completion_sha256": provider_common.sha(
                (current_directory / "completion.json").read_bytes()),
            "numeric_task_id": 42, "state": "succeeded",
        })

        ops = self.queue_ops(binding)
        journals = ops.validate_inspection_journals(allow_failure=False)
        self.assertEqual(journals[TASK]["operation_id"], current_attempt["operation_id"])
        history = journals[TASK]["historical_attempts"]
        self.assertEqual(
            {entry["operation_id"] for entry in history},
            {first_attempt["operation_id"], second_attempt["operation_id"]})
        self.assertTrue(all(entry["historical_aborted"] for entry in history))
        history_by_id = {entry["operation_id"]: entry for entry in history}
        self.assertIsNone(history_by_id[first_attempt["operation_id"]]["receipt"])
        self.assertIsNotNone(history_by_id[second_attempt["operation_id"]]["receipt"])

        status = self.queue_status(
            ops, include_inspection=True,
            inspection_group=_inspection_group(ROOT),
            inspection_operation=current_attempt["operation_id"])
        ops._validate_queue_membership(status, allow_failure=False)

    def test_accepts_current_and_exact_aborted_history_attempts(self):
        self.write_workspace(sequence=1, request_sha256=INITIAL_REQUEST,
                             closure="aborted-not-launched")
        old_task = self.task_record(reservation=1, request_sha256=INITIAL_REQUEST)
        old_attempt = provider_common._workspace_admission_attempt(
            self.state, ROOT, old_task, "old admission")

        self.write_workspace(sequence=2, request_sha256=INITIAL_REQUEST,
                             closure="operation-completed")
        current_task = self.task_record(reservation=2, request_sha256=INITIAL_REQUEST)
        current_attempt = provider_common._workspace_admission_attempt(
            self.state, ROOT, current_task, "current admission")
        self.assertNotEqual(old_attempt["operation_id"], current_attempt["operation_id"])
        self.assertEqual(current_attempt["sequence"], 2)
        accepted = provider_common._validate_historical_workspace_attempt(
            self.state, old_attempt, current_task, ROOT, "history")
        self.assertEqual(accepted["operation_id"], current_attempt["operation_id"])

        # Give the changed request its own later reservation so the admission
        # resolver itself succeeds; history must then reject the digest and
        # budget drift rather than hiding it behind reservation normalization.
        self.write_workspace(sequence=3, request_sha256=REPLAY_REQUEST,
                             closure="operation-completed")
        changed = self.task_record(
            reservation=3, request_sha256=REPLAY_REQUEST, budget_nanos=2)
        with self.assertRaises(AcceptanceFailure):
            provider_common._validate_historical_workspace_attempt(
                self.state, old_attempt, changed, ROOT, "history")

    def test_full_journal_accepts_existing_ordinary_workspace_attempt(self):
        self.write_workspace()
        task = self.task_record()
        attempt = provider_common._workspace_admission_attempt(
            self.state, ROOT, task, "ordinary admission")
        operation_id = attempt["operation_id"]
        ordinary = self.state / "tasks" / TASK
        ordinary.mkdir(mode=0o700, parents=True)
        (ordinary / "task.json").write_bytes(provider_common._canonical_task_bytes(task))
        (ordinary / "brief.md").write_bytes(b"x")

        binding = {
            "definition_revision": "inspection-v1",
            "definition_sha256": "1" * 64,
            "helper_executable": str(self.base / "helper"),
            "helper_sha256": "2" * 64,
            "worker_executable": str(self.runner),
            "worker_sha256": provider_common.sha(self.runner.read_bytes()),
            "supervisor": provider_common.supervisor_binding(
                self.pueue, self.config, self.config.parent,
                provider_common.sha(self.config.read_bytes()), "pueue fixture"),
        }
        directory = self.state / "inspections" / operation_id
        directory.mkdir(mode=0o700, parents=True)
        request = self.inspection_request(task, operation_id)
        request["binding"] = binding
        write_json(directory / "request.json", request)
        request_sha = provider_common.sha((directory / "request.json").read_bytes())
        write_json(directory / "receipt.json", {
            "schema_version": 1, "request_sha256": request_sha, "numeric_task_id": 42,
        })
        write_json(directory / "submission.json", {
            "schema_version": 1, "request_sha256": request_sha,
            "created_at": "2026-09-15T00:00:01Z",
        })
        write_json(directory / "start.json", {
            "schema_version": 1, "request_sha256": request_sha,
            "created_at": "2026-09-15T00:00:02Z",
        })
        write_json(directory / "result.json", {
            "schema_version": 1, "request_sha256": request_sha,
            "reason": "eligible", "facts": {"eligible": True},
        })
        result_sha = provider_common.sha((directory / "result.json").read_bytes())
        write_json(directory / "completion.json", {
            "schema_version": 1, "request_sha256": request_sha,
            "result_sha256": result_sha, "completed_at": "2026-09-15T00:00:02Z",
            "native_exit": "successful-exit",
        })
        write_json(directory / "worker-observation.json", {
            "schema_version": 1, "request_sha256": request_sha,
            "completion_sha256": provider_common.sha(
                (directory / "completion.json").read_bytes()),
            "numeric_task_id": 42, "state": "succeeded",
        })

        ops = NativeTaskOps(
            SimpleNamespace(), self.state_parent / "delegate", self.runner, self.pueue,
            self.state_parent, self.state, self.state_parent)
        ops.bind_supervisor(self.config)
        ops.dispatch_attempts = {TASK}
        ops.inspection_attempts = {TASK}
        ops.inspection_bindings = {TASK: binding}
        journals = ops.validate_inspection_journals(allow_failure=False)
        self.assertEqual(journals[TASK]["operation_id"], operation_id)
        self.assertTrue(journals[TASK]["ordinary_task_matches"])

    def test_managed_workspace_journal_accepts_private_default_group_and_zero_parallelism(self):
        private_supervisor = self.state / ".supervisor"
        private_supervisor.mkdir(mode=0o700)
        private_config = private_supervisor / "pueue.yml"
        private_config.write_bytes(b"private config")
        private_config.chmod(0o600)

        task = self.task_record()
        task["brief_sha256"] = provider_common.sha(b"x")
        self.write_workspace()
        attempt = provider_common._workspace_admission_attempt(
            self.state, ROOT, task, "managed workspace admission")
        operation_id = attempt["operation_id"]
        ordinary = self.state / "tasks" / TASK
        ordinary.mkdir(mode=0o700, parents=True)
        (ordinary / "task.json").write_bytes(provider_common._canonical_task_bytes(task))
        (ordinary / "brief.md").write_bytes(b"x")

        binding = self.inspection_binding(private_config, concurrent=True)
        directory = self.write_inspection(task, operation_id, binding)
        request = read_json(directory / "request.json")
        request_sha = provider_common.sha((directory / "request.json").read_bytes())
        write_json(directory / "start.json", {
            "schema_version": 1, "request_sha256": request_sha,
            "created_at": "2026-09-15T00:00:02Z",
        })
        write_json(directory / "result.json", {
            "schema_version": 1, "request_sha256": request_sha,
            "reason": "eligible", "facts": {"eligible": True},
        })
        result_sha = provider_common.sha((directory / "result.json").read_bytes())
        write_json(directory / "completion.json", {
            "schema_version": 1, "request_sha256": request_sha,
            "result_sha256": result_sha,
            "completed_at": "2026-09-15T00:00:02Z",
            "native_exit": "successful-exit",
        })
        inspection_fixtures.InspectionHarnessTests.managed_inspection_lifetime(
            directory, request, request_sha, "eligible")
        self.rewrite_lifetime_identity(directory, operation_id)

        ops = self.queue_ops(binding)
        journals = ops.validate_inspection_journals(allow_failure=False)
        self.assertEqual(journals[TASK]["group"], "default")
        self.assertEqual(journals[TASK]["group_parallelism"], 0)
        self.assertEqual(journals[TASK]["operation_id"], operation_id)

        status = self.queue_status(
            ops, include_inspection=True, inspection_group="default",
            inspection_operation=operation_id)
        ops._validate_queue_membership(status, allow_failure=False)
        wrong_parallelism = deepcopy(status)
        wrong_parallelism["groups"]["default"]["parallel_tasks"] = 1
        with self.assertRaises(AcceptanceFailure):
            ops._validate_queue_membership(wrong_parallelism, allow_failure=False)


if __name__ == "__main__":
    unittest.main()
