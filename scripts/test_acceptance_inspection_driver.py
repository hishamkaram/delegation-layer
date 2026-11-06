"""Deterministic checks for the real-supervisor inspection fixture oracle."""
from argparse import Namespace
from pathlib import Path
from types import SimpleNamespace
import json
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

import acceptance_inspection as inspection_acceptance

from acceptance_inspection import (
    InspectionAcceptance, no_secret_bytes, validate_blocker_row, validate_concurrent_response,
    validate_expired_inspection,
)
import acceptance_provider_common as provider_common
from acceptance_provider_common import AcceptanceFailure, NativeTaskOps
import acceptance_supervisor_common as supervisor_common
from acceptance_supervisor_common import digest, write_json
from scripts.acceptance_tests.record_fixtures import object_at
from scripts.acceptance_tests.inspection.fixtures import FixtureProcesses
from scripts.acceptance_tests.resources import DirectoryPinOwner
from scripts.acceptance_tests.inspection.fixtures import forbid_installed_content
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, BoundedCommandResult, CommandFailure


class InspectionInstallationTests(unittest.TestCase):
    COMMANDS = (
        ("delegate", "internal/testutil/inspectionfixture/cmd/delegate"),
        ("delegate-run", "internal/testutil/inspectionfixture/cmd/delegate-run"),
        ("inspection-helper", "internal/testutil/inspectionfixture/cmd/inspection-helper"),
        ("provider", "internal/testutil/fakeprovider/cmd/provider"),
    )

    def setUp(self):
        self.enterContext(forbid_installed_content())
        temporary = tempfile.TemporaryDirectory(prefix="inspection-installation-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.source = self.root / "source"
        self.source.mkdir()
        (self.source / "go.mod").write_text("module example.invalid/inspection\n")
        for _name, package in self.COMMANDS:
            directory = self.source / package
            directory.mkdir(parents=True)
            (directory / "main.go").write_text("package main\nfunc main() {}\n")
        for name in ("pueue", "pueued"):
            path = self.root / name
            path.write_text("#!/bin/sh\nexit 0\n")
            path.chmod(0o700)
        self.args = Namespace(source_root=str(self.source), tools=None,
                                    pueue=str(self.root / "pueue"), pueued=str(self.root / "pueued"),
                                    output=str(self.root / "evidence"))
        self.enterContext(patch("scripts.delegate_acceptance.paths.operations.reject_tmp"))
        self.launch = self.enterContext(patch("subprocess.Popen", side_effect=AssertionError("actor launched")))

    @staticmethod
    def materialize_build(arguments, _environment, **_options):
        target = Path(arguments[arguments.index("-o") + 1])
        target.write_text("#!/bin/sh\nexit 0\n")  # Original source-build output; no image copy.
        target.chmod(0o700)
        return BoundedCommandResult(0, b"")

    def construct(self):
        operation = InspectionAcceptance(self.args)
        self.addCleanup(supervisor_common.rollback_owned_directories, operation.owned_directories)
        return operation

    def test_builds_four_selected_source_commands_directly_into_owned_installation(self):
        with patch.object(inspection_acceptance, "run_bounded_command", side_effect=self.materialize_build) as build:
            operation = self.construct()
        self.assertEqual(build.call_count, 4)
        for call, (name, package) in zip(build.call_args_list, self.COMMANDS):
            self.assertEqual(call.args[0], ["go", "build", "-buildvcs=false", "-o",
                                           str(operation.bindir / name), "./" + package])
            self.assertEqual(call.kwargs["cwd"], self.source)
            self.assertEqual(call.args[1]["GOTOOLCHAIN"], "local")
            self.assertEqual(call.args[1]["CGO_ENABLED"], "0")
            self.assertEqual(call.kwargs["timeout"], 900)
            self.assertEqual(call.kwargs["max_output"], 1024 * 1024)
            self.assertTrue(call.kwargs["capture_stderr"])
        self.assertEqual(operation.delegate, operation.bindir / "delegate")
        self.assertEqual(operation.runner, operation.bindir / "delegate-run")
        self.launch.assert_not_called()

    def test_retired_tools_refuses_before_reading_or_copying_installed_programs(self):
        tools = self.root / "legacy-tools"
        tools.mkdir()
        for name, _package in self.COMMANDS:
            (tools / name).write_text("#!/bin/sh\nexit 0\n")
            (tools / name).chmod(0o700)
        self.args.tools = str(tools)
        with patch.object(inspection_acceptance, "run_bounded_command") as build:
            with self.assertRaisesRegex(AcceptanceFailure, "--tools is retired.*--source-root"):
                InspectionAcceptance(self.args)
        build.assert_not_called()
        self.launch.assert_not_called()
        self.assertFalse(Path(self.args.output).exists())

    def test_build_and_data_configuration_keep_programs_in_place_without_sha_fields(self):
        with patch.object(inspection_acceptance, "run_bounded_command", side_effect=self.materialize_build):
            operation = self.construct()
        _task, _brief, _case, helper_config = operation.configure("finite", 0)
        provider_record = supervisor_common.read_json(operation.bindir / "provider-fixture.json")
        helper_record = supervisor_common.read_json(operation.bindir / "inspection-fixture.json")
        self.assertNotIn("provider_sha256", provider_record)
        self.assertNotIn("helper_sha256", helper_record)
        self.assertEqual(provider_record["provider_executable"], str(operation.provider))
        self.assertEqual(helper_record["helper_executable"], str(operation.helper))
        self.assertEqual(provider_record["provider_config_sha256"], digest(Path(provider_record["provider_config"])))
        self.assertEqual(helper_record["helper_config_sha256"], digest(helper_config))
        self.launch.assert_not_called()

    def test_completed_build_failure_rolls_back_without_private_output(self):
        with patch.object(inspection_acceptance, "run_bounded_command", return_value=BoundedCommandResult(
                9, b"private compiler stdout", b"private compiler stderr")) as build:
            with self.assertRaisesRegex(AcceptanceFailure, "build failed with exit 9") as stopped:
                InspectionAcceptance(self.args)
        self.assertNotIn("private compiler", str(stopped.exception))
        self.assertEqual(build.call_count, 1)
        self.assertEqual(list(Path(self.args.output).iterdir()), [])
        self.launch.assert_not_called()

    def test_unproven_build_wait_retains_owned_installation_before_cleanup(self):
        failure = BoundedCommandError(CommandFailure.TERMINATION_FAILED)
        with patch.object(inspection_acceptance, "run_bounded_command", side_effect=failure):
            with self.assertRaisesRegex(AcceptanceFailure, "command_termination_failed") as stopped:
                InspectionAcceptance(self.args)
        self.assertIn("retained " + str(Path(self.args.output)), str(stopped.exception))
        self.assertTrue((Path(self.args.output) / "fixture/bin").is_dir())
        self.launch.assert_not_called()
        # The Wait failure was simulated. No child exists; TemporaryDirectory
        # owns final cleanup after verifying that constructor retention worked.

    def test_missing_fixture_source_blocks_before_any_build(self):
        (self.source / self.COMMANDS[2][1] / "main.go").unlink()
        with patch.object(inspection_acceptance, "run_bounded_command") as build:
            with self.assertRaisesRegex(AcceptanceFailure, "all four inspection fixture commands"):
                InspectionAcceptance(self.args)
        build.assert_not_called()
        self.assertEqual(list(Path(self.args.output).iterdir()), [])


class InspectionDriverTests(unittest.TestCase):
    def test_constructor_rolls_back_disposable_roots_after_partial_setup(self):
        with tempfile.TemporaryDirectory(prefix="inspection-constructor-") as temporary:
            root = Path(temporary)
            pueue = root / "pueue"
            pueued = root / "pueued"
            pueue.write_bytes(b"pueue")
            pueued.write_bytes(b"pueued")
            output = root / "evidence"
            args = Namespace(source_root=str(inspection_acceptance.REPO), pueue=str(pueue),
                                   pueued=str(pueued), output=str(output))

            with patch("scripts.delegate_acceptance.paths.operations.reject_tmp"), \
                    patch.object(inspection_acceptance, "build_inspection_tools",
                                 side_effect=OSError("injected build failure")):
                with self.assertRaisesRegex(OSError, "build failure"):
                    InspectionAcceptance(args)

            self.assertTrue(output.is_dir())
            self.assertEqual(list(output.iterdir()), [])

    def test_finalize_before_setup_is_a_noop(self):
        with tempfile.TemporaryDirectory() as temporary:
            driver = InspectionAcceptance.__new__(InspectionAcceptance)
            driver.output = Path(temporary) / "not-created"
            driver.ops = NativeTaskOps(FixtureProcesses(), Path("/delegate"), Path("/runner"),
                                       Path("/pueue"), Path("/base"), Path("/state"), driver.output)

            self.assertFalse(driver.finalize(False))
            self.assertFalse(driver.output.exists())

    def test_runner_terminal_accepts_explicit_budget_stop_only_when_expected(self):
        root = "a" * 32
        task = "b" * 32
        row = {
            "label": f"delegate:{root}:{task}",
            "status": {"Done": {"result": "Killed"}},
        }

        def client(_name, _argv, timeout):
            self.assertGreater(timeout, 0)
            return SimpleNamespace(json=lambda: {"tasks": {"7": row}})

        self.assertEqual(
            provider_common.wait_runner_terminal(client, root, task, "Killed",
                                                 timeout=1, monotonic_fn=lambda: 0),
            row,
        )
        with self.assertRaisesRegex(
                AcceptanceFailure, "supervisor result 'Killed', expected 'Success'"):
            provider_common.wait_runner_done(client, root, task,
                                             timeout=1, monotonic_fn=lambda: 0)

    def test_runner_terminal_normalizes_failed_result(self):
        root = "a" * 32
        task = "b" * 32
        row = {
            "label": f"delegate:{root}:{task}",
            "status": {"Done": {"result": {"Failed": 1}}},
        }

        def client(_name, _argv, timeout):
            self.assertGreater(timeout, 0)
            return SimpleNamespace(json=lambda: {"tasks": {"7": row}})

        self.assertEqual(
            provider_common.wait_runner_terminal(client, root, task, "Failed",
                                                 timeout=1, monotonic_fn=lambda: 0),
            row,
        )

    def test_runner_terminal_accepts_one_of_controlled_results(self):
        root = "a" * 32
        task = "b" * 32
        row = {
            "label": f"delegate:{root}:{task}",
            "status": {"Done": {"result": "Success"}},
        }

        def client(_name, _argv, timeout):
            self.assertGreater(timeout, 0)
            return SimpleNamespace(json=lambda: {"tasks": {"7": row}})

        self.assertEqual(
            provider_common.wait_runner_terminal(
                client, root, task, frozenset({"Success", "Failed"}),
                timeout=1, monotonic_fn=lambda: 0),
            row,
        )

    def test_concurrent_admission_is_retained_before_receipt_write(self):
        ops = NativeTaskOps(Mock(), Path("/delegate"), Path("/runner"), Path("/pueue"),
                            Path("/base"), Path("/state"), Path("/evidence"))
        task = "a" * 32
        root = "b" * 32
        response: dict[str, object] = {"task_id": task, "root_id": root, "admission": "admitted",
                    "supervisor": {"matched": True, "numeric_task_id": 0}}
        with self.assertRaises(AcceptanceFailure):
            ops.record_admission("success", task, response)
        ops.dispatch_attempts.add(task)
        with patch("scripts.delegate_acceptance.tasks.operations.write_json", side_effect=OSError("receipt failure")):
            with self.assertRaises(OSError):
                ops.record_admission("success", task, response)
        self.assertEqual(ops.tasks, {"success": task})
        self.assertEqual(ops.numbers, {"success": 0})
        self.assertEqual(ops.labels, {"success": "delegate:" + root + ":" + task})

    def test_diagnostic_write_failure_still_retains_owned_processes(self):
        driver = InspectionAcceptance.__new__(InspectionAcceptance)
        driver.output = Path("/fixture-evidence")
        driver.state = Path("/fixture-state")
        driver.config = None
        driver.case_records = {}
        driver.ops = Mock(closed=False)
        driver.ops.retain_failure_ownership.return_value = True
        driver.setup = Mock(side_effect=RuntimeError("fixture assertion failed"))
        driver.cleanup_blocker_after_failure = Mock(return_value=True)
        with patch("acceptance_inspection.write_json", side_effect=[OSError("receipt unavailable"), None]):
            with self.assertRaisesRegex(OSError, "receipt unavailable"):
                driver.run()
        driver.cleanup_blocker_after_failure.assert_called_once_with()
        driver.ops.retain_failure_ownership.assert_called_once_with()

    def test_behavioral_success_is_not_published_when_finalization_fails(self):
        for failure in (False, OSError("injected finalization failure")):
            with self.subTest(failure=repr(failure)), \
                    tempfile.TemporaryDirectory(prefix="inspection-finalize-unit-") as temporary:
                root = Path(temporary)
                output = root / "evidence"
                output.mkdir()
                driver = InspectionAcceptance.__new__(InspectionAcceptance)
                driver.output = output
                driver.workspace = root / "workspace"
                driver.state = root / "state"
                driver.events = root / "events"
                driver.cases = root / "cases"
                driver.pueue_base = root / "pueue"
                driver.sentinel = b"fixture-sentinel"
                driver.config = root / "pueue.json"
                driver.case_records = {"behavior": {"status": "passed"}}
                driver.ops = Mock(closed=True)
                driver.ops.safe_failure_shutdown.return_value = True
                driver.ops.retain_failure_ownership.return_value = True
                driver.setup = Mock()
                driver.success = Mock()
                driver.failed_inspection = Mock()
                driver.queued_expiry = Mock()
                driver.cleanup_blocker_after_failure = Mock(return_value=True)
                calls = []

                def finalize(success):
                    calls.append(success)
                    if success and isinstance(failure, BaseException):
                        raise failure
                    return not success or failure is not False

                self.enterContext(patch.object(driver, "finalize", side_effect=finalize))
                argv = ["--source-root", str(root), "--pueue", "/pueue", "--pueued", "/pueued",
                        "--output", str(output)]
                with patch.object(inspection_acceptance, "InspectionAcceptance", return_value=driver), \
                        patch.object(inspection_acceptance, "no_secret_bytes"), \
                        patch.object(sys, "argv", ["acceptance_inspection.py", *argv]):
                    code = inspection_acceptance.main()

                self.assertEqual(code, 1)
                self.assertEqual(calls, [True, False])
                self.assertFalse((output / "success.json").exists())
                self.assertTrue((output / "failure.json").is_file())
                cleanup = json.loads((output / "failure-cleanup.json").read_text())
                self.assertTrue(cleanup["passed_receipt_absent"])
                self.assertTrue(cleanup["artifact_finalization_completed"])

    @staticmethod
    def _write_expiry_journal(root: Path, started: bool = True) -> tuple[Path, str, dict[str, object]]:
        task = "a" * 32
        state = root / "state"
        directory = state / "inspections" / task
        directory.mkdir(parents=True)
        request_path = directory / "request.json"
        supervisor = {"client_executable": "/usr/bin/pueue", "config_path": "/private/pueue.json"}
        request = {"schema_version": 1, "task_id": task,
                   "deadline": "2026-09-15T00:00:20Z",
                   "binding": {"supervisor": supervisor}}
        write_json(request_path, request)
        request_digest = digest(request_path)
        write_json(directory / "receipt.json", {"schema_version": 1,
                                                 "request_sha256": request_digest,
                                                 "numeric_task_id": 42})
        if started:
            write_json(directory / "start.json", {"schema_version": 1,
                                                   "request_sha256": request_digest,
                                                   "created_at": "2026-09-15T00:00:01Z"})
            result_path = directory / "result.json"
            write_json(result_path, {"schema_version": 1, "request_sha256": request_digest,
                                     "reason": "deadline-expired", "facts": {}})
            write_json(directory / "completion.json", {
                "schema_version": 1, "request_sha256": request_digest,
                "result_sha256": digest(result_path),
                "native_exit": "unavailable", "completed_at": "2026-09-15T00:00:21Z",
            })
            stop_path = directory / "stop-request.json"
            write_json(stop_path, {"schema_version": 1, "request_sha256": request_digest,
                                   "numeric_task_id": 42,
                                   "supervisor": supervisor,
                                   "deadline": request["deadline"]})
            stop_digest = digest(stop_path)
            write_json(directory / "stop-reply.json", {
                "schema_version": 1, "stop_sha256": stop_digest,
                "numeric_task_id": 42, "action": "kill", "acknowledged": False,
            })
            write_json(directory / "stop-observation.json", {
                "schema_version": 1, "stop_sha256": stop_digest,
                "numeric_task_id": 42, "state": "ended",
            })
        return state, task, {"id": 42}

    def test_secret_crossing_capture_scan_boundary_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "stdout").write_bytes(b"a" * 65532 + b"private-sentinel" + b"z" * 100)
            with self.assertRaisesRegex(AcceptanceFailure, "sentinel escaped"):
                no_secret_bytes([root], b"private-sentinel")

    def test_unrelated_output_passes_and_single_byte_secret_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "stdout").write_bytes(b"safe-output" * 10000)
            no_secret_bytes([root], b"not-present")
            with self.assertRaises(AcceptanceFailure):
                no_secret_bytes([root], b"f")

    def test_symlink_cannot_hide_output_from_scan(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "stdout").symlink_to(root / "missing")
            with self.assertRaisesRegex(AcceptanceFailure, "symlink"):
                no_secret_bytes([root], b"sentinel")

    def test_concurrent_unknown_is_not_treated_as_admitted(self):
        task = "a" * 32
        root = "b" * 32
        unknown: dict[str, object] = {
            "task_id": task, "root_id": root, "task_record": "unknown",
            "admission": "unknown", "liveness": "undetermined", "publication": "unknown",
            "failure": {"code": "dispatch_failed", "stage": "create-task-record",
                         "next_action": "stop_and_report"},
            "error": "Delegate could not complete the operation.",
        }
        self.assertEqual(validate_concurrent_response(unknown, 1, task), root)
        with self.assertRaises(AcceptanceFailure):
            validate_concurrent_response(unknown, 0, task)
        with self.assertRaises(AcceptanceFailure):
            validate_concurrent_response({**unknown, "error": "unsupported policy"}, 1, task)
        with self.assertRaises(AcceptanceFailure):
            validate_concurrent_response({**unknown, "root_id": ""}, 1, task)
        with self.assertRaises(AcceptanceFailure):
            validate_concurrent_response(unknown, 1, "c" * 32)

    def test_empty_sentinel_is_refused(self):
        with self.assertRaises(AcceptanceFailure):
            no_secret_bytes([], b"")

    def test_blocker_identity_state_and_argv_are_exact(self):
        blocker = " ".join(inspection_acceptance.BLOCKER_ARGV)
        running: dict[str, object] = {"id": 7, "group": "g", "label": "l", "status": {"Running": {}},
                   "command": blocker, "original_command": blocker}
        validate_blocker_row(running, 7, "g", "l", "Running")
        with self.assertRaises(AcceptanceFailure):
            validate_blocker_row({**running, "id": 8}, 7, "g", "l", "Running")
        with self.assertRaises(AcceptanceFailure):
            validate_blocker_row({**running, "command": "/bin/sleep 1"}, 7, "g", "l", "Running")
        done = {**running, "status": {"Done": {"result": "Success"}}}
        validate_blocker_row(done, 7, "g", "l", "Done")

    def test_expiry_oracle_accepts_exact_running_stop_records(self):
        with tempfile.TemporaryDirectory() as directory:
            state, task, row = self._write_expiry_journal(Path(directory))
            validate_expired_inspection(state, task, row, True)

    def test_expiry_oracle_rejects_queued_start_or_stop_records(self):
        with tempfile.TemporaryDirectory() as directory:
            state, task, row = self._write_expiry_journal(Path(directory), started=False)
            validate_expired_inspection(state, task, row, False)
            (state / "inspections" / task / "start.json").write_text("{}\n")
            with self.assertRaises(AcceptanceFailure):
                validate_expired_inspection(state, task, row, False)

    def test_expiry_oracle_rejects_wrong_stop_action(self):
        with tempfile.TemporaryDirectory() as directory:
            state, task, row = self._write_expiry_journal(Path(directory))
            reply = state / "inspections" / task / "stop-reply.json"
            value = json.loads(reply.read_text())
            value["action"] = "remove"
            write_json(reply, value, replace=True)
            with self.assertRaises(AcceptanceFailure):
                validate_expired_inspection(state, task, row, True)

    def test_expiry_oracle_does_not_fabricate_missing_self_stop_reply(self):
        with tempfile.TemporaryDirectory() as directory:
            state, task, row = self._write_expiry_journal(Path(directory))
            inspection_dir = state / "inspections" / task
            (inspection_dir / "stop-reply.json").unlink()
            (inspection_dir / "stop-observation.json").unlink()
            validate_expired_inspection(state, task, row, True)


class SetupBoundary(RuntimeError):
    """Raised after setup has validated its sources, before daemon creation."""


class InspectionDriverSetupTests(unittest.TestCase):
    def setUp(self):
        self.enterContext(forbid_installed_content())

    def test_setup_validates_config_source_and_binding_before_daemon_start(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            pueue = root / "pueue"
            pueued = root / "pueued"
            for path in (pueue, pueued):
                path.write_bytes(path.name.encode("ascii"))
                path.chmod(0o700)

            output = root / "output"
            args = Namespace(source_root=str(inspection_acceptance.REPO), pueue=str(pueue),
                                   pueued=str(pueued), output=str(output))
            private_prefix = (Path("/Users/Shared") if sys.platform == "darwin"
                              else Path.home() / ".dl-acceptance")
            setup_calls: list[tuple[str, list[object], Path]] = []

            def private_directory(path: Path, _label: str, create: bool = False) -> Path:
                path = Path(path)
                if path.parent == private_prefix and path.name.startswith("dl-inspect-"):
                    path = root / "private-supervisor"
                if create:
                    path.mkdir(mode=0o700, parents=True, exist_ok=False)
                self.assertTrue(path.is_dir())
                self.assertFalse(path.is_symlink())
                return path.resolve()

            def direct(name: str, _argv: list[object], _expected=0, _timeout=30):
                self.assertIn(name, {"pueue-version", "pueued-version"})
                directory = root / (name + "-receipt")
                directory.mkdir(mode=0o700)
                (directory / "stdout").write_text(name.removesuffix("-version") + " 4.0.4\n")
                return SimpleNamespace(directory=directory)

            def before_daemon(name: str, argv: list[object], cwd: Path):
                setup_calls.append((name, argv, Path(cwd)))
                self.assertEqual(name, "daemon")
                self.assertIsNotNone(driver.config)
                self.assertIsNotNone(driver.pueue_base)
                self.assertIsNotNone(driver.supervisor_config_digest)
                if driver.config is None or driver.pueue_base is None:
                    raise AssertionError("setup supervisor paths are absent")
                config = driver.config
                base = driver.pueue_base
                self.assertEqual(supervisor_common.read_json(config),
                                 supervisor_common.config_for(base))
                helper_config = root / "trusted-helper.json"
                expected = driver.expected_inspection_binding(helper_config)
                provider_common._validate_inspection_binding(expected,
                                                              "setup expected binding")
                self.assertEqual(object_at(expected, "supervisor")["config_digest"],
                                 supervisor_common.digest(config))
                self.assertEqual(object_at(expected, "supervisor")["resolved_config_sha256"],
                                 provider_common.resolved_supervisor_config_digest(base))
                self.assertEqual(object_at(expected, "supervisor")["endpoint"],
                                 "unix:" + str(base / "run" / "p.sock"))
                raise SetupBoundary("daemon creation boundary reached")

            with patch.object(inspection_acceptance, "run_bounded_command",
                              side_effect=InspectionInstallationTests.materialize_build), \
                    patch.object(inspection_acceptance, "ensure_private_directory",
                              side_effect=private_directory), \
                    patch.object(inspection_acceptance.secrets, "token_hex",
                                 side_effect=lambda count: "a" * (count * 2)), \
                    patch.object(inspection_acceptance, "DirectoryOwner",
                                 side_effect=lambda: DirectoryPinOwner(self)):
                driver = inspection_acceptance.InspectionAcceptance(args)
                self.enterContext(patch.object(driver.ops, "direct", side_effect=direct))
                self.enterContext(patch.object(driver.processes, "start", side_effect=before_daemon))
                with self.assertRaises(SetupBoundary):
                    driver.setup()

            self.assertEqual(len(setup_calls), 1)
            self.assertEqual(setup_calls[0][0], "daemon")
            self.assertFalse((root / "private-supervisor" / "run" / "p.sock").exists())


if __name__ == "__main__":
    unittest.main()
