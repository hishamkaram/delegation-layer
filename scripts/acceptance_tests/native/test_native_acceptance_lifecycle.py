"""Hermetic oracles for a native acceptance behavior cohort."""
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import acceptance_supervisor_common as supervisor_common
from scripts import acceptance_native as entry
from scripts.acceptance_tests.resources import track_directory_pins
from scripts.delegate_acceptance.native import (
    acceptance as gate,
    acceptance_lifecycle as lifecycle_impl,
)
from scripts.delegate_acceptance.processes import operations as processes_impl


class NativeAcceptanceOracleTests(unittest.TestCase):
    def test_failure_cleanup_delegates_retention_with_owned_state(self):
        root = "a" * 32
        task = "b" * 32
        config = Path("/fixture/pueue.yml")
        daemon = object()
        calls = []
        operation = object.__new__(gate.NativeAcceptance)
        operation.closed = False
        operation.root_id = None
        operation.config = config
        operation.daemon = daemon
        operation.tasks = {}
        operation.numeric_ids = {}

        def bind_supervisor(bound_config, bound_daemon):
            calls.append(("bind", bound_config, bound_daemon))

        def retain_failure_ownership():
            calls.append(("retain", operation.ops.root_id, dict(operation.tasks),
                          dict(operation.numeric_ids)))
            return True

        operation.ops = SimpleNamespace(
            root_id=root,
            tasks={"fresh": task},
            labels={"fresh": f"delegate:{root}:{task}"},
            numbers={"fresh": 7},
            dispatch_attempts={task},
            bind_supervisor=bind_supervisor,
            retain_failure_ownership=retain_failure_ownership,
        )

        self.assertTrue(operation.safe_shutdown())
        self.assertTrue(operation.closed)
        self.assertEqual(calls, [("bind", config, daemon),
                                 ("retain", root, {"fresh": task}, {task: 7})])

    def test_failure_cleanup_refuses_unknown_queue_when_shared_retention_refuses(self):
        operation = object.__new__(gate.NativeAcceptance)
        operation.closed = False
        operation.root_id = None
        operation.config = Path("/fixture/pueue.yml")
        operation.daemon = object()
        operation.tasks = {}
        operation.numeric_ids = {}
        retained = []
        operation.ops = SimpleNamespace(
            root_id=None,
            tasks={}, labels={}, numbers={}, dispatch_attempts=set(),
            bind_supervisor=lambda *_args: None,
            retain_failure_ownership=lambda: retained.append(True) or False,
        )

        self.assertFalse(operation.safe_shutdown())
        self.assertEqual(retained, [True])
        self.assertFalse(operation.closed)

    def test_native_lifecycle_owner_adopts_daemon_before_started_receipt_failure(self):
        with tempfile.TemporaryDirectory(prefix="native-owner-receipt-unit-") as directory:
            root = Path(directory)
            cwd = root / "cwd"
            cwd.mkdir()

            class FakePopen:
                def __init__(self, *_args, **_kwargs):
                    self.pid = 702
                    self.returncode = None

                def poll(self):
                    return self.returncode

                def wait(self):
                    return self.returncode

            original_write_json = processes_impl.write_json

            def fail_started(path, *args, **kwargs):
                if Path(path).name == "started.json":
                    raise OSError("injected started receipt failure")
                return original_write_json(path, *args, **kwargs)

            operation = object.__new__(gate.NativeAcceptance)
            operation.daemon = None
            processes = gate.Processes(root / "processes", {"PATH": "/usr/bin"})
            track_directory_pins(self, (processes.capture_root,))
            with patch.object(processes_impl.subprocess, "Popen", FakePopen), \
                    patch.object(processes_impl, "write_json", side_effect=fail_started):
                with self.assertRaisesRegex(OSError, "started receipt"):
                    processes.start("pueued", ["/bin/sh"], cwd,
                                    on_spawned=operation._adopt_daemon)

            self.assertIs(operation.daemon, processes.entries[0])
            self.assertEqual(operation.daemon.pid, 702)
            operation.daemon.process.returncode = 0
            operation.daemon.poll()

    def test_cleanup_stays_open_until_every_owned_process_exits_naturally(self):
        with tempfile.TemporaryDirectory(prefix="native-natural-exit-unit-") as directory:
            root = Path(directory)
            cwd = root / "cwd"
            cwd.mkdir()

            class FakePopen:
                def __init__(self, *_args, **_kwargs):
                    self.pid = 703
                    self.returncode = None

                def poll(self):
                    return self.returncode

                def wait(self):
                    return self.returncode

            operation = object.__new__(gate.NativeAcceptance)
            operation.daemon = None
            operation.closed = False
            operation.processes = gate.Processes(root / "processes", {"PATH": "/usr/bin"})
            track_directory_pins(self, (operation.processes.capture_root,))
            with patch.object(processes_impl.subprocess, "Popen", FakePopen):
                process = operation.processes.start("pueued", ["/bin/sh"], cwd)

            self.assertFalse(operation.safe_shutdown())
            self.assertFalse(operation.closed)
            process.process.returncode = 0
            self.assertTrue(operation.safe_shutdown())
            self.assertTrue(operation.closed)

    def test_constructor_rolls_back_only_the_roots_created_before_failure(self):
        with tempfile.TemporaryDirectory(prefix="native-constructor-rollback-unit-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            home = root / "home"
            live_parent = home / "delegation-layer-live"
            live_parent.mkdir(parents=True)
            sentinel = live_parent / "preexisting"
            sentinel.write_text("keep")
            args = SimpleNamespace(
                provider="pi:json", mode="read-only", scenario="read-only",
                discover_models=False, output=str(output), delegate="/bin/sh",
                runner="/bin/sh", pueue="/bin/sh", pueued="/bin/sh",
                provider_executable="/bin/sh", model=None, effort=None,
            )
            original_mkdir = gate.Path.mkdir

            def fail_workspace(path, *mkdir_args, **mkdir_kwargs):
                if path.name == "workspace":
                    raise OSError("injected constructor failure")
                return original_mkdir(path, *mkdir_args, **mkdir_kwargs)

            with patch.object(gate.Path, "home", return_value=home), \
                    patch.object(gate, "reject_tmp"), \
                    patch.object(gate, "require_discovery"), \
                    patch.object(gate.Path, "mkdir", new=fail_workspace):
                with self.assertRaisesRegex(OSError, "constructor failure"):
                    gate.NativeAcceptance(args)

            self.assertEqual(sentinel.read_text(), "keep")
            self.assertEqual(list(live_parent.iterdir()), [sentinel])

    def test_constructor_rolls_back_base_when_mkdir_fails_after_creation(self):
        with tempfile.TemporaryDirectory(prefix="native-constructor-base-rollback-unit-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            home = root / "home"
            args = SimpleNamespace(
                provider="pi:json", mode="read-only", scenario="read-only",
                discover_models=False, output=str(output), delegate="/bin/sh",
                runner="/bin/sh", pueue="/bin/sh", pueued="/bin/sh",
                provider_executable="/bin/sh", model=None, effort=None,
            )
            with patch.object(gate.Path, "home", return_value=home), \
                    patch.object(gate, "reject_tmp"), \
                    patch.object(gate, "require_discovery"), \
                    patch.object(gate.NativeAcceptance, "_initialize_runtime_roots",
                                 side_effect=OSError("injected runtime setup failure")):
                with self.assertRaisesRegex(OSError, "runtime setup failure"):
                    gate.NativeAcceptance(args)

            self.assertTrue((home / "delegation-layer-live").is_dir())
            self.assertEqual(list((home / "delegation-layer-live").iterdir()), [])

    def test_failure_diagnostics_do_not_skip_shutdown_or_finalization(self):
        with tempfile.TemporaryDirectory(prefix="native-finalize-call-unit-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = supervisor_common.DirectoryOwner()
            base = owner.create("acceptance root", root / "private", root)
            raw = owner.create("raw process captures", output / "processes", output)
            operation = object.__new__(gate.NativeAcceptance)
            operation.closed = False
            operation.output = output
            operation.base = base.directory
            operation.base_parent = root
            operation.state = base.directory / "state"
            operation.workspace = base.directory / "workspace"
            operation.owned_directories = (base, raw)
            track_directory_pins(self, operation.owned_directories)
            operation.processes = SimpleNamespace(capture_root=raw)
            calls = []
            operation._write_failure_diagnostics = lambda: calls.append("diagnostics") or False
            operation.safe_shutdown = lambda: calls.append("shutdown") or False
            with patch.object(lifecycle_impl, "finalize_acceptance_artifacts", return_value=False) as finalize:
                self.assertFalse(operation.finalize(False))
            self.assertEqual(calls, ["diagnostics", "shutdown"])
            finalize.assert_called_once_with(
                operation.output, False, (base, raw), False,
                remove_failed_process_captures=False,
                raw_process_captures_token=raw,
                retained_owned_directories=(base,),
            )

    def test_cleanup_removes_closed_owned_base_and_preserves_evidence(self):
        with tempfile.TemporaryDirectory(prefix="native-cleanup-unit-") as directory:
            root = Path(directory)
            operation = object.__new__(gate.NativeAcceptance)
            operation.base_parent = root / "delegation-layer-live"
            operation.base = operation.base_parent / "run"
            operation.state = operation.base / "state"
            operation.workspace = operation.base / "workspace"
            operation.output = root / "evidence"
            operation.output.mkdir()
            owner = supervisor_common.DirectoryOwner()
            base_parent = owner.create("acceptance base parent", operation.base_parent, root)
            base_token = owner.create("acceptance base", operation.base, operation.base_parent)
            raw_token = owner.create("raw process captures", operation.output / "processes", operation.output)
            operation.owned_directories = (base_parent, base_token, raw_token)
            track_directory_pins(self, operation.owned_directories)
            operation.processes = SimpleNamespace(capture_root=raw_token)
            (operation.base / "state").mkdir()
            (operation.base / "state" / "record").write_text("disposable")
            operation.closed = True

            self.assertTrue(operation.finalize(True))

            self.assertFalse(operation.base.exists())
            self.assertFalse(operation.base_parent.exists())
            receipt = gate.read_json(operation.output / "cleanup.json")
            self.assertEqual(receipt["status"], "removed")
            self.assertTrue(receipt["supervisor_shutdown_observed"])

    def test_cleanup_retains_unclosed_owned_base(self):
        with tempfile.TemporaryDirectory(prefix="native-cleanup-unit-") as directory:
            root = Path(directory)
            operation = object.__new__(gate.NativeAcceptance)
            operation.base_parent = root / "delegation-layer-live"
            operation.base = operation.base_parent / "run"
            operation.state = operation.base / "state"
            operation.workspace = operation.base / "workspace"
            operation.output = root / "evidence"
            operation.output.mkdir()
            owner = supervisor_common.DirectoryOwner()
            base_parent = owner.create("acceptance base parent", operation.base_parent, root)
            base_token = owner.create("acceptance base", operation.base, operation.base_parent)
            raw_token = owner.create("raw process captures", operation.output / "processes", operation.output)
            operation.owned_directories = (base_parent, base_token, raw_token)
            track_directory_pins(self, operation.owned_directories)
            operation.processes = SimpleNamespace(capture_root=raw_token)
            operation.closed = False

            self.assertFalse(operation.finalize(False))

            self.assertTrue(operation.base.is_dir())
            receipt = gate.read_json(operation.output / "cleanup.json")
            self.assertEqual(receipt["status"], "retained")
            self.assertFalse(receipt["supervisor_shutdown_observed"])

    @staticmethod
    def _failure_operation(root: Path) -> tuple[object, Path, str]:
        operation = object.__new__(gate.NativeAcceptance)
        operation.base_parent = root / "delegation-layer-live"
        operation.base = operation.base_parent / "run"
        operation.output = root / "evidence"
        operation.state = operation.base / "state"
        operation.workspace = operation.base / "workspace"
        operation.briefs = operation.base / "briefs"
        operation.pueue_base = operation.base / "pueue"
        operation.models_state = operation.base / "models-state"
        operation.output.mkdir()
        owner = supervisor_common.DirectoryOwner()
        owner.create("acceptance base parent", operation.base_parent, root)
        owner.create("acceptance base", operation.base, operation.base_parent)
        owner.create("acceptance state", operation.state, operation.base)
        owner.create("acceptance workspace", operation.workspace, operation.base)
        owner.create("acceptance briefs", operation.briefs, operation.base)
        owner.create("private pueue", operation.pueue_base, operation.base)
        owner.create("private pueue state", operation.pueue_base / "state", operation.pueue_base)
        owner.create("private pueue runtime", operation.pueue_base / "run", operation.pueue_base)
        owner.create("model discovery state", operation.models_state, operation.base)
        raw_token = owner.create("raw process captures", operation.output / "processes", operation.output)
        operation.owned_directories = owner.transfer()
        operation.processes = SimpleNamespace(capture_root=raw_token)
        (operation.state / "tasks").mkdir()
        operation.closed = True
        operation.provider = "pi:json"
        operation.mode = "workspace-write"
        operation.scenario = "write"
        operation.root_id = "a" * 32
        task_id = "b" * 32
        operation.tasks = {"fresh": task_id}
        operation.numeric_ids = {task_id: 7}
        operation.model_discovery_task_id = None
        task = operation.state / "tasks" / task_id
        task.mkdir()
        gate.write_json(operation.output / "failure.json", {
            "gate": "acceptance-native", "status": "failed", "provider": operation.provider,
            "mode": operation.mode, "scenario": operation.scenario, "task_budget": "5m",
            "requested_model": None, "requested_effort": None, "discover_models": False,
            "no_retry": True, "credentials_in_receipt": False,
            "error": "write task did not leave the exact edited target",
            "traceback": "RAW TRACEBACK MUST NOT APPEAR",
        })
        gate.write_json(task / "outcome.json", {
            "verdict": "committed", "predicate": {"adapter": "pi:json", "mode": operation.mode,
                                                        "version": "v1", "sha256": "c" * 64},
            "payload": {"basename": "result.txt", "length": 18, "sha256": "d" * 64},
            "evidence_sha256": "e" * 64,
        })
        gate.write_json(task / "provider.exit", {
            "invocation_state": "started", "exit_code": 0,
            "error": "RAW PROVIDER OUTPUT MUST NOT APPEAR",
            "raw_manifest": [{"path": "raw/stdout", "bytes": 44, "sha256": "f" * 64}],
            "manifest_sha256": "1" * 64,
        })
        (task / "result.txt").write_text("RAW RESULT TEXT MUST NOT APPEAR")
        (operation.workspace / "acceptance-write-target.txt").write_text("edited:fixture")
        (operation.output / "processes" / "001-provider").mkdir()
        (operation.output / "processes" / "001-provider" / "stdout").write_text(
            "RAW PROCESS CAPTURE MUST NOT APPEAR")
        return operation, task, task_id

    def test_failure_snapshot_retains_state_and_cleans_process_captures(self):
        with tempfile.TemporaryDirectory(prefix="native-diagnostic-unit-") as directory:
            operation, _task, task_id = self._failure_operation(Path(directory))

            self.assertTrue(operation.finalize(False))

            snapshot = operation.output / "failure-diagnostics.json"
            data = snapshot.read_bytes()
            self.assertLessEqual(len(data), gate.FAILURE_DIAGNOSTICS_BYTES)
            value = json.loads(data)
            self.assertEqual(value["provider"], "pi:json")
            self.assertEqual(value["mode"], "workspace-write")
            self.assertEqual(value["scenario"], "write")
            self.assertEqual(value["task_ids"], [task_id])
            self.assertEqual(value["status"]["partial"], False)
            self.assertNotIn("RAW", data.decode())
            self.assertTrue(operation.base.exists())
            self.assertTrue(operation.base_parent.exists())
            self.assertTrue(operation.state.exists())
            self.assertTrue(operation.workspace.exists())
            self.assertFalse(operation.briefs.exists())
            self.assertFalse(operation.pueue_base.exists())
            self.assertFalse(operation.models_state.exists())
            self.assertFalse((operation.output / "processes").exists())
            receipt = gate.read_json(operation.output / "cleanup.json")
            self.assertEqual(receipt["raw_process_captures"], "removed")

            self.assertFalse(operation.finalize(False))
            self.assertEqual(snapshot.read_bytes(), data)

    def test_failure_snapshot_preserves_sanitized_rejection_and_payload_descriptor(self):
        with tempfile.TemporaryDirectory(prefix="native-diagnostic-rejected-unit-") as directory:
            operation, task, task_id = self._failure_operation(Path(directory))
            refusal = b"UNRESTRICTED_REFUSAL_PAYLOAD"
            gate.write_json(task / "outcome.json", {
                "verdict": "rejected",
                "payload": {
                    "basename": "publish.reject", "length": len(refusal),
                    "sha256": hashlib.sha256(refusal).hexdigest(),
                },
                "evidence_sha256": "e" * 64,
                "diagnostics": [{
                    "code": "incomplete_output", "field": "result.text",
                    "message": "provider output did not contain a complete answer",
                    "raw": "MUST NOT APPEAR",
                }],
            }, replace=True)
            (task / "publish.reject").write_bytes(refusal)

            self.assertTrue(operation.finalize(False))

            value = gate.read_json(operation.output / "failure-diagnostics.json")
            diagnostic_task = next(item for item in value["tasks"] if item["task_id"] == task_id)
            self.assertEqual(diagnostic_task["outcome"]["diagnostics"], [{
                "code": "incomplete_output", "field": "result.text",
                "message": "provider output did not contain a complete answer",
            }])
            descriptor = diagnostic_task["files"]["publish.reject"]
            self.assertEqual(descriptor["bytes"], len(refusal))
            self.assertEqual(descriptor["sha256"], hashlib.sha256(refusal).hexdigest())
            snapshot = (operation.output / "failure-diagnostics.json").read_text()
            self.assertNotIn("UNRESTRICTED_REFUSAL_PAYLOAD", snapshot)
            self.assertNotIn("MUST NOT APPEAR", snapshot)
            self.assertTrue(operation.base.exists())
            self.assertFalse((operation.output / "processes").exists())

    def test_failure_snapshot_is_bounded_and_marks_truncation(self):
        with tempfile.TemporaryDirectory(prefix="native-diagnostic-bound-unit-") as directory:
            operation, _task, _task_id = self._failure_operation(Path(directory))
            gate.write_json(_task / "provider.exit", {
                "invocation_state": "started", "exit_code": 1,
                "raw_manifest": [
                    {"path": "raw/" + ("m" * 220), "bytes": index, "sha256": "f" * 64}
                    for index in range(3000)
                ],
            }, replace=True)
            for index in range(1500):
                name = f"{index:04d}-" + ("x" * 230)
                (operation.workspace / name).write_bytes(b"workspace metadata only")

            self.assertTrue(operation.finalize(False))

            snapshot = operation.output / "failure-diagnostics.json"
            self.assertLessEqual(snapshot.stat().st_size, gate.FAILURE_DIAGNOSTICS_BYTES)
            value = gate.read_json(snapshot)
            self.assertTrue(value["status"]["partial"])
            self.assertTrue(value["status"]["truncated"])
            self.assertTrue(operation.base.exists())
            self.assertFalse((operation.output / "processes").exists())

    def test_failure_snapshot_error_retains_state_and_raw_captures(self):
        with tempfile.TemporaryDirectory(prefix="native-diagnostic-failure-unit-") as directory:
            operation, _task, _task_id = self._failure_operation(Path(directory))
            with patch.object(operation, "_failure_diagnostics",
                              side_effect=OSError("injected snapshot failure")):
                self.assertTrue(operation.finalize(False))

            self.assertTrue(operation.base.exists())
            self.assertTrue((operation.output / "processes").is_dir())
            self.assertFalse((operation.output / "failure-diagnostics.json").exists())
            receipt = gate.read_json(operation.output / "cleanup.json")
            self.assertEqual(receipt["raw_process_captures"], "retained")

    def test_blocked_snapshot_error_retains_raw_captures_after_state_cleanup(self):
        with tempfile.TemporaryDirectory(prefix="native-blocked-diagnostic-failure-unit-") as directory:
            operation, _task, _task_id = self._failure_operation(Path(directory))
            with patch.object(operation, "_failure_diagnostics",
                              side_effect=OSError("injected snapshot failure")):
                self.assertTrue(operation.finalize(False, blocked=True))

            self.assertFalse(operation.base.exists())
            self.assertTrue((operation.output / "processes").is_dir())
            self.assertFalse((operation.output / "failure-diagnostics.json").exists())
            receipt = gate.read_json(operation.output / "cleanup.json")
            self.assertEqual(receipt["raw_process_captures"], "retained")

    def test_blocked_cleanup_removes_closed_state_and_raw_captures(self):
        with tempfile.TemporaryDirectory(prefix="native-blocked-cleanup-unit-") as directory:
            operation, _task, _task_id = self._failure_operation(Path(directory))

            self.assertTrue(operation.finalize(False, blocked=True))

            self.assertFalse(operation.base.exists())
            self.assertFalse(operation.output.joinpath("processes").exists())
            receipt = gate.read_json(operation.output / "cleanup.json")
            self.assertEqual(receipt["status"], "removed")
            self.assertEqual(receipt["raw_process_captures"], "removed")

    def test_main_finalizes_the_owned_acceptance_root_before_passing(self):
        with tempfile.TemporaryDirectory(prefix="native-main-cleanup-unit-") as directory:
            output = Path(directory) / "evidence"
            output.mkdir()
            calls = []

            class Run:
                def run(self):
                    calls.append("run")
                    return 0

                def finalize(self, success, *, blocked=False):
                    calls.append(("finalize", success, blocked))
                    return True

                def publish_success(self):
                    calls.append("publish")

            with patch.object(entry, "choose_output", return_value=output), \
                    patch.object(entry, "NativeAcceptance", return_value=Run()):
                code = entry.main([
                    "--provider", "pi:json", "--permission", "read-only",
                    "--output", str(output),
                ])

            self.assertEqual(code, 0)
            self.assertEqual(calls, ["run", ("finalize", True, False), "publish"])

    def test_main_treats_cleanup_failure_as_fatal(self):
        with tempfile.TemporaryDirectory(prefix="native-main-cleanup-failure-unit-") as directory:
            output = Path(directory) / "evidence"
            output.mkdir()

            class Run:
                def run(self):
                    return 0

                def finalize(self, _success, *, blocked=False):
                    return False

                def publish_success(self):
                    raise AssertionError("success was published before cleanup")

            with patch.object(entry, "choose_output", return_value=output), \
                    patch.object(entry, "NativeAcceptance", return_value=Run()):
                self.assertEqual(entry.main([
                    "--provider", "pi:json", "--permission", "read-only",
                    "--output", str(output),
                ]), 1)
            self.assertFalse((output / "success.json").exists())

    def test_model_discovery_main_persists_sanitized_failure_without_success(self):
        for exit_code, status in ((1, "failed"), (2, "BLOCKED")):
            with self.subTest(exit_code=exit_code), tempfile.TemporaryDirectory(
                    prefix="native-model-main-failure-") as directory:
                output = Path(directory) / "evidence"
                output.mkdir()
                calls = []

                class Run:
                    model_discovery_summary = {
                        "status": "blocked" if exit_code == 2 else "failed",
                        "reason_code": "authentication_unavailable" if exit_code == 2 else "provider_error",
                        "complete": False, "model_count": 0, "effort_count": None,
                        "raw_error": "must not be copied",
                    }

                    def run(self):
                        return exit_code

                    def finalize(self, success, *, blocked=False):
                        calls.append(("finalize", success, blocked))
                        return True

                    def publish_success(self):
                        raise AssertionError("model discovery failure published success")

                with patch.object(entry, "choose_output", return_value=output), \
                        patch.object(entry, "NativeAcceptance", return_value=Run()):
                    code = entry.main([
                        "--provider", "codex:exec", "--permission", "read-only",
                        "--discover-models", "--output", str(output),
                    ])

                self.assertEqual(code, exit_code)
                self.assertEqual(calls, [("finalize", False, exit_code == 2)])
                self.assertFalse((output / "success.json").exists())
                receipt = gate.read_json(output / "failure.json")
                self.assertEqual(receipt["status"], status)
                self.assertEqual(receipt["discovery_exit_code"], exit_code)
                self.assertEqual(receipt["model_discovery"]["reason_code"],
                                 Run.model_discovery_summary["reason_code"])
                self.assertNotIn("raw_error", receipt["model_discovery"])

    def test_main_exception_disposition_keeps_blocked_neutral_and_failures_failed(self):
        cases = (
            (gate.BlockedFailure("authentication unavailable"), 2, True, "BLOCKED"),
            (gate.AcceptanceFailure("provider rejected the task"), 1, False, "failed"),
            (RuntimeError("unexpected harness failure"), 1, False, "failed"),
        )
        for error, expected_code, expected_blocked, expected_status in cases:
            with self.subTest(error=type(error).__name__), tempfile.TemporaryDirectory(
                    prefix="native-main-disposition-") as directory:
                output = Path(directory) / "evidence"
                output.mkdir()
                calls = []

                class Run:
                    def run(self):
                        raise error

                    def finalize(self, success, *, blocked=False):
                        calls.append(("finalize", success, blocked))
                        return True

                    def publish_success(self):
                        raise AssertionError("failure published success")

                with patch.object(entry, "choose_output", return_value=output), \
                        patch.object(entry, "NativeAcceptance", return_value=Run()):
                    code = entry.main([
                        "--provider", "pi:json", "--permission", "read-only",
                        "--output", str(output),
                    ])

                self.assertEqual(code, expected_code)
                self.assertEqual(calls, [("finalize", False, expected_blocked)])
                self.assertEqual(gate.read_json(output / "failure.json")["status"],
                                 expected_status)

    def test_cleanup_preserves_preexisting_raw_process_captures(self):
        with tempfile.TemporaryDirectory(prefix="native-preexisting-captures-unit-") as directory:
            root = Path(directory)
            operation = object.__new__(gate.NativeAcceptance)
            operation.base_parent = root / "delegation-layer-live"
            operation.base = operation.base_parent / "run"
            operation.output = root / "evidence"
            operation.base_parent.mkdir()
            operation.output.mkdir()
            owner = supervisor_common.DirectoryOwner()
            base_token = owner.create("acceptance base", operation.base, operation.base_parent)
            track_directory_pins(self, (base_token,))
            captures = operation.output / "processes"
            captures.mkdir()
            sentinel = captures / "keep.txt"
            sentinel.write_text("pre-existing capture")
            operation.owned_directories = (base_token,)
            operation.processes = SimpleNamespace(capture_root=None)
            operation.closed = True

            self.assertFalse(operation.finalize(True))

            self.assertTrue(sentinel.is_file())
            self.assertEqual(sentinel.read_text(), "pre-existing capture")
            self.assertFalse((operation.output / "cleanup.json").exists())

    def test_model_discovery_failure_cleanup_tracks_its_inspection_root(self):
        with tempfile.TemporaryDirectory(prefix="native-model-cleanup-unit-") as directory:
            root = Path(directory)
            models_state = root / "models-state"
            models_state.mkdir()
            gate.write_json(models_state / "root.json", {"root_id": "c" * 32})
            task_id = "d" * 32
            (models_state / "inspections" / task_id).mkdir(parents=True)
            operation = object.__new__(gate.NativeAcceptance)
            operation.processes = SimpleNamespace()
            operation.delegate = root / "delegate"
            operation.runner = root / "runner"
            operation.pueue = root / "pueue"
            operation.models_state = models_state
            operation.output = root / "output"
            operation.model_discovery_task_id = None
            operation.models_root_id = None
            cleanup = gate.ModelDiscoveryCleanupOps(operation)
            status = {"tasks": {}, "groups": {}}
            with patch.object(gate.NativeTaskOps, "failure_queue_finished", return_value=True):
                self.assertTrue(cleanup.failure_queue_finished(status))
            self.assertEqual(cleanup.root_id, "c" * 32)
            self.assertEqual(cleanup.dispatch_attempts, {task_id})
            self.assertEqual(cleanup.inspection_attempts, {task_id})

    def test_model_discovery_failure_cleanup_uses_model_owner(self):
        with tempfile.TemporaryDirectory(prefix="native-model-owner-unit-") as directory:
            root = Path(directory)
            operation = object.__new__(gate.NativeAcceptance)
            operation.closed = False
            operation.discover_models_only = True
            operation.root_id = None
            operation.config = root / "pueue.yml"
            operation.daemon = object()
            operation.models_state = root / "models-state"
            operation.output = root / "output"
            operation.delegate = root / "delegate"
            operation.runner = root / "runner"
            operation.pueue = root / "pueue"
            operation.processes = SimpleNamespace()
            operation.tasks = {}
            operation.numeric_ids = {}

            def unexpected_owner(*_args):
                raise AssertionError("ordinary cleanup owner was selected")

            operation.ops = SimpleNamespace(
                root_id=None, tasks={}, labels={}, numbers={}, dispatch_attempts=set(),
                bind_supervisor=unexpected_owner, retain_failure_ownership=unexpected_owner,
            )
            with patch.object(gate.ModelDiscoveryCleanupOps, "retain_failure_ownership",
                              return_value=True) as retain:
                self.assertTrue(operation.safe_shutdown())
            retain.assert_called_once()
            self.assertTrue(operation.closed)

    def test_runtime_binding_receipt_is_create_once(self):
        with tempfile.TemporaryDirectory(prefix="native-binding-unit-") as directory:
            root = Path(directory)
            output = root / "output"
            output.mkdir()
            files = {}
            for name in ("provider", "delegate", "runner", "pueue", "pueued"):
                path = root / name
                path.write_text(name + "\n")
                files[name] = path

            operation = object.__new__(gate.NativeAcceptance)
            operation.output = output
            operation.provider = "pi:json"
            operation.mode = "read-only"
            operation.scenario = "read-only"
            operation.provider_executable = files["provider"]
            operation.provider_version = "pi-runtime-v1"
            first_task = "a" * 32
            first_identity = {
                "executable": str(files["provider"]),
                "version": operation.provider_version,
                "sha256": "a" * 64,
            }
            operation.delegate = files["delegate"]
            operation.runner = files["runner"]
            operation.pueue = files["pueue"]
            operation.pueued = files["pueued"]
            operation.pueue_version = "pueue 4.0.4"
            operation.pueued_version = "pueued 4.0.4"
            operation._record_provider_runtime_identity(first_task, {
                "provider_executable": first_identity["executable"],
                "provider_version": first_identity["version"],
                "effective_config": {"policy": {
                    "runtime_sha256": first_identity["sha256"],
                }},
            })
            operation.write_binding(first_task, first_identity)
            receipt = gate.read_json(output / "binding.json")
            self.assertEqual(receipt["task_budget"], gate.TASK_BUDGET)
            self.assertEqual(receipt["canonical_task_budget"], gate.CANONICAL_TASK_BUDGET)
            self.assertIsNone(receipt["requested_model"])
            self.assertIsNone(receipt["requested_effort"])
            self.assertIsNone(receipt["model_discovery"])
            self.assertIsNone(receipt["native_mode"])
            self.assertEqual(receipt["provider_identity_scope"], "first-admitted-task")
            self.assertEqual(receipt["provider_task_id"], first_task)
            self.assertEqual(receipt["provider_sha256"], first_identity["sha256"])
            original = (output / "binding.json").read_bytes()

            second_task = "b" * 32
            second_identity = {
                "executable": str(files["provider"]),
                "version": "pi-runtime-v2",
                "sha256": "b" * 64,
            }
            operation._record_provider_runtime_identity(second_task, {
                "provider_executable": second_identity["executable"],
                "provider_version": second_identity["version"],
                "effective_config": {"policy": {
                    "runtime_sha256": second_identity["sha256"],
                }},
            })
            operation.tasks = {"fresh": first_task, "resume": second_task}
            runtime_receipt = operation.provider_runtime_receipt()
            self.assertEqual(runtime_receipt["provider_versions"], {
                first_task: "pi-runtime-v1", second_task: "pi-runtime-v2",
            })
            self.assertEqual(runtime_receipt["provider_runtime_identities"], {
                first_task: first_identity, second_task: second_identity,
            })
            with self.assertRaises(gate.AcceptanceFailure):
                operation.write_binding(second_task, second_identity)
            self.assertEqual((output / "binding.json").read_bytes(), original)

    def test_codex_git_baseline_is_allowed_but_immutable(self):
        with tempfile.TemporaryDirectory(prefix="native-codex-git-unit-") as directory:
            workspace = Path(directory) / "workspace"
            workspace.mkdir()
            git_directory = workspace / ".git"
            git_directory.mkdir()
            (git_directory / "config").write_text("[core]\n")
            marker = workspace / "acceptance-marker.txt"
            nonce_file = workspace / "acceptance-write-nonce.txt"
            fixture = workspace / "acceptance-write-fixture.txt"
            ack = workspace / ".ack-proof"
            staging = workspace / "proof.staging"
            target = workspace / "acceptance-write-target.txt"
            check = workspace / "acceptance-write-check.txt"
            marker.write_bytes(b"DELEGATION-LIVE-MARKER\n")
            nonce_file.write_bytes(b"nonce\n")
            fixture.write_bytes(b"preexisting:nonce\n")
            ack.write_text("stable\n")
            staging.write_text("stable\n")

            operation = object.__new__(gate.NativeAcceptance)
            operation.provider = "codex:exec"
            operation.workspace = workspace
            operation.workspace_marker = marker
            operation.marker_bytes = marker.read_bytes()
            operation.nonce_file = nonce_file
            operation.fixture_file = fixture
            operation.target_file = target
            operation.check_file = check
            operation.nonce = "nonce"
            operation.workspace_baseline = gate.workspace_snapshot(workspace)
            fixture.write_bytes(b"edited-fixture:nonce")
            target.write_bytes(b"edited:nonce\n")
            check.write_text(f"{hashlib.sha256(target.read_bytes()).hexdigest()}  {target.name}\n")
            operation._workspace_write_snapshot(continuation=False)

            ack.write_text("changed\n")
            with self.assertRaises(gate.AcceptanceFailure):
                operation._workspace_write_snapshot(continuation=False)
            ack.write_text("stable\n")
            (git_directory / "config").write_text("[core]\nrepositoryformatversion = 1\n")
            with self.assertRaises(gate.AcceptanceFailure):
                operation._workspace_write_snapshot(continuation=False)

    def test_write_oracle_requires_filesystem_digest_not_provider_prose(self):
        with tempfile.TemporaryDirectory(prefix="native-write-unit-") as directory:
            root = Path(directory)
            fixture = root / "acceptance-write-fixture.txt"
            target = root / "acceptance-write-target.txt"
            artifact = root / "acceptance-write-check.txt"
            fixture.write_bytes(b"preexisting:nonce\n")
            fixture.write_bytes(b"edited-fixture:nonce")
            gate.verify_write_fixture("nonce", fixture)
            target.write_bytes(b"edited:nonce\n")
            digest = hashlib.sha256(target.read_bytes()).hexdigest()
            artifact.write_text(f"{digest}  {target.name}\n")
            gate.verify_write_artifacts("nonce", target, artifact)

            artifact.write_text("I created and checked the target successfully.\n")
            with self.assertRaises(gate.AcceptanceFailure):
                gate.verify_write_artifacts("nonce", target, artifact)
