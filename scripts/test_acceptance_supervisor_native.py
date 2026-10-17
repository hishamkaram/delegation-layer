"""Pure ownership-oracle tests for the native supervisor acceptance driver."""

from __future__ import annotations

from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import acceptance_supervisor_native as native
from scripts.acceptance_tests.resources import track_directory_pins


class NativeEventOracleTests(unittest.TestCase):
    def test_add_capability_probe_is_not_counted_as_a_queue_mutation(self):
        prefix = ["/tmp/pueue", "-c", "/tmp/pueue.yml"]
        self.assertEqual(
            native.supervisor_operation([*prefix, "add", "--help"]),
            "add-probe",
        )
        self.assertEqual(
            native.supervisor_operation([*prefix, "add", "--", "/tmp/runner"]),
            "add",
        )

    def test_enqueue_capability_probe_is_not_counted_as_a_queue_mutation(self):
        prefix = ["/tmp/pueue", "-c", "/tmp/pueue.yml"]
        self.assertEqual(
            native.supervisor_operation([*prefix, "enqueue", "--help"]),
            "enqueue-probe",
        )
        self.assertEqual(
            native.supervisor_operation([*prefix, "enqueue", "7"]),
            "enqueue",
        )

    def fake_events(self, names=None):
        if names is None:
            names = list(native.EXPECTED_EXECUTION_EVENTS)
        return [{"name": name, "sequence": index + 1} for index, name in enumerate(names)]

    def test_current_runner_events_are_accepted(self):
        events = self.fake_events()
        native.validate_execution_event_sequence([event["name"] for event in events])

    def test_missing_event_is_rejected(self):
        events = self.fake_events()[:-1]
        with self.assertRaises(RuntimeError):
            native.validate_execution_event_sequence([event["name"] for event in events])

    def test_duplicate_event_is_rejected(self):
        events = self.fake_events()
        events[-1]["name"] = events[-2]["name"]
        with self.assertRaises(RuntimeError):
            native.validate_execution_event_sequence([event["name"] for event in events])

    def test_reordered_authorization_events_are_rejected(self):
        events = self.fake_events()
        first = native.EXPECTED_EXECUTION_EVENTS.index("preflight-authorized")
        second = native.EXPECTED_EXECUTION_EVENTS.index("start-authorized")
        events[first], events[second] = events[second], events[first]
        with self.assertRaises(RuntimeError):
            native.validate_execution_event_sequence([event["name"] for event in events])

    def test_capture_pair_may_interleave_but_close_order_is_required(self):
        names = list(native.EXPECTED_EXECUTION_EVENTS)
        stdout_eof = names.index("stdout-eof")
        stderr_eof = names.index("stderr-eof")
        names[stdout_eof], names[stderr_eof] = names[stderr_eof], names[stdout_eof]
        native.validate_execution_event_sequence(names)

        names = list(native.EXPECTED_EXECUTION_EVENTS)
        stdout_eof = names.index("stdout-eof")
        stdout_close = names.index("stdout-raw-closed")
        names[stdout_eof], names[stdout_close] = names[stdout_close], names[stdout_eof]
        with self.assertRaises(RuntimeError):
            native.validate_execution_event_sequence(names)

    def test_deadline_event_is_rejected_even_with_the_success_set(self):
        names = list(native.EXPECTED_EXECUTION_EVENTS)
        names[names.index("timer-disarmed")] = "deadline-observed"
        with self.assertRaises(RuntimeError):
            native.validate_execution_event_sequence(names)


class FailureCleanupTests(unittest.TestCase):
    def test_constructor_rolls_back_allocated_private_base(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            tools = root / "tools"
            tools.mkdir()
            pueue = root / "pueue"
            pueued = root / "pueued"
            pueue.write_bytes(b"pueue")
            pueued.write_bytes(b"pueued")
            output = root / "output"
            base = root / "private"

            def allocate(**_kwargs):
                base.mkdir()
                return str(base)

            with patch.object(native.tempfile, "mkdtemp", side_effect=allocate), \
                    patch.object(native, "Processes",
                                 side_effect=OSError("injected process owner failure")):
                with self.assertRaisesRegex(OSError, "process owner failure"):
                    native.NativeSuite(tools, pueue, pueued, output)
            self.assertFalse(base.exists())

    def suite(self):
        suite = native.NativeSuite.__new__(native.NativeSuite)
        suite.processes = object()
        suite.delegate = Path("/tmp/delegate")
        suite.runner = Path("/tmp/delegate-run")
        suite.pueue = Path("/tmp/pueue")
        suite.base = Path("/tmp/native-base")
        suite.root = Path("/tmp/native-root")
        suite.output = Path("/tmp/native-output")
        suite.config_path = Path("/tmp/native-config")
        suite.daemon = object()
        suite.task_id = "b" * 32
        suite.dispatch_attempted = True
        suite.admitted = True
        suite.root_id = "a" * 32
        suite.task_dir = Path("/tmp/native-root") / "tasks" / suite.task_id
        return suite

    def test_shared_cleanup_cannot_shutdown_without_terminal_capture_proof(self):
        suite = self.suite()
        calls = []

        class FakeOwner:
            def __init__(self, *_args):
                self.suite = suite

            def bind_supervisor(self, *_args):
                pass

            def safe_failure_shutdown(self):
                if not self.failure_queue_finished(self.status()):
                    return False
                calls.append("shutdown")
                return True

            def failure_queue_finished(self, _status):
                return True

            def status(self):
                return {"tasks": {"1": {
                    "label": "delegate:" + self.suite.root_id + ":" + self.suite.task_id,
                }}}

            def retain_failure_ownership(self):
                return self.safe_failure_shutdown()

        with patch.object(native, "NativeTaskOps", FakeOwner), \
                patch.object(suite, "supervisor_numeric_task_id", return_value=7), \
                patch.object(suite, "failure_terminal_proof", return_value=False):
            self.assertFalse(suite.retained_failure_cleanup())
        self.assertEqual(calls, [])

    def test_shared_cleanup_uses_shutdown_only_after_terminal_capture_proof(self):
        suite = self.suite()
        calls = []

        class FakeOwner:
            def __init__(self, *_args):
                self.suite = suite
                self.dispatch_attempts = set()

            def bind_supervisor(self, *_args):
                pass

            def safe_failure_shutdown(self):
                if not self.failure_queue_finished(self.status()):
                    return False
                calls.append("shutdown")
                return True

            def failure_queue_finished(self, _status):
                return True

            def status(self):
                return {"tasks": {"1": {
                    "label": "delegate:" + self.suite.root_id + ":" + self.suite.task_id,
                }}}

            def retain_failure_ownership(self):
                return self.safe_failure_shutdown()

        with patch.object(native, "NativeTaskOps", FakeOwner), \
                patch.object(suite, "supervisor_numeric_task_id", return_value=7), \
                patch.object(suite, "failure_terminal_proof", return_value=True):
            self.assertTrue(suite.retained_failure_cleanup())
        self.assertEqual(calls, ["shutdown"])

    def test_early_finish_failure_refreshes_exact_done_row_before_proof(self):
        suite = self.suite()
        with tempfile.TemporaryDirectory() as directory:
            suite.task_dir = Path(directory)
            (suite.task_dir / "provider.exit").write_text("{}")
            (suite.task_dir / "supervisor.ref.json").write_text(
                '{"numeric_task_id":7}')
            calls = []

            class FreshOwner:
                def __init__(self, *_args):
                    self.suite = suite
                    self.numbers = {}

                def bind_supervisor(self, *_args):
                    pass

                def status(self):
                    return {"tasks": {"7": {
                        "id": 7,
                        "label": "delegate:" + self.suite.root_id + ":" + self.suite.task_id,
                        "status": {"Done": {"result": "Success"}},
                    }}}

                def failure_queue_finished(self, _status):
                    return self.numbers.get("native") == 7

                def safe_failure_shutdown(self):
                    if not self.failure_queue_finished(self.status()):
                        return False
                    calls.append("shutdown")
                    return True

                def retain_failure_ownership(self):
                    return self.safe_failure_shutdown()

            def terminal():
                calls.append("terminal")

            with patch.object(native, "NativeTaskOps", FreshOwner), \
                    patch.object(suite, "validate_events",
                                 side_effect=AssertionError("unrelated event oracle failure")), \
                    patch.object(suite, "validate_terminal", side_effect=terminal):
                suite.native_terminal_proven = False
                self.assertTrue(suite.retained_failure_cleanup())
            self.assertEqual(suite.native_job["id"], 7)
            self.assertEqual(calls, ["terminal", "shutdown"])

    def test_finish_does_not_publish_success_when_private_root_deletion_fails(self):
        with tempfile.TemporaryDirectory(prefix="supervisor-finish-delete-error-") as directory:
            root = Path(directory)
            suite = native.NativeSuite.__new__(native.NativeSuite)
            suite.base = root / "private"
            suite.base.mkdir()
            owner = native.DirectoryOwner()
            suite.owned_directories = (owner.adopt_created(
                "native supervisor", suite.base, root),)
            track_directory_pins(self, suite.owned_directories)
            suite.output = root / "output"
            suite.output.mkdir()
            suite.root = root / "task-root"
            suite.task_id = "b" * 32
            suite.root_id = "a" * 32
            suite.task_dir = root / "task"
            suite.task_dir.mkdir()
            (suite.task_dir / "supervisor.ref.json").write_text('{"numeric_task_id": 7}')
            (suite.task_dir / "outcome.json").write_text('{"verdict": "committed"}')
            (suite.task_dir / "provider.exit").write_text('{}')
            suite.profile = root / "profile"
            suite.provider_config = root / "provider-config"
            suite.provider = root / "provider"
            suite.work = root / "work"
            for path in (suite.profile, suite.provider_config, suite.provider, suite.work):
                path.write_text("fixture")
            suite.daemon = SimpleNamespace(
                pid=17,
                result={"exit_code": 0},
                poll=lambda: None,
                wait=lambda *_args, **_kwargs: None,
            )
            suite.processes = SimpleNamespace(drain=lambda **_kwargs: [])
            suite.case_ids = []
            suite.native_terminal_proven = False
            suite.shutdown_gate = False

            class JsonResult:
                def __init__(self, value):
                    self.value = value

                def json(self):
                    return self.value

            job = {
                "id": 7,
                "label": "delegate:" + suite.root_id + ":" + suite.task_id,
                "status": {"Done": {"result": "Success"}},
            }

            def client(_name, operation):
                if operation == ["status", "--json"]:
                    return JsonResult({"tasks": {"7": job}})
                if operation == ["shutdown"]:
                    return JsonResult({})
                raise AssertionError("unexpected client operation")

            suite.client = client
            suite.validate_events = lambda: None
            suite.cli = lambda *_args: JsonResult({"outcome": {"verdict": "committed"}})
            suite.take_provider_workspace_offline = lambda: None

            with patch.object(native.shutil, "copytree"), \
                    patch.object(native, "rollback_owned_directories",
                                 side_effect=OSError("injected private root deletion failure")):
                with self.assertRaisesRegex(OSError, "private root deletion failure"):
                    suite.finish()

            self.assertFalse((suite.output / "success.json").exists())

    def test_taking_workspace_offline_consumes_its_owner_token(self):
        with tempfile.TemporaryDirectory(prefix="supervisor-workspace-offline-") as directory:
            root = Path(directory)
            suite = native.NativeSuite.__new__(native.NativeSuite)
            suite.work = root / "work"
            owner = native.DirectoryOwner()
            workspace = owner.create("workspace", suite.work, root)
            retained = owner.create("retained", root / "retained", root)
            suite.owned_directories = owner.transfer()

            suite.take_provider_workspace_offline()

            self.assertFalse(suite.work.exists())
            self.assertEqual(suite.owned_directories, (retained,))
            native.rollback_owned_directories(suite.owned_directories)

    def test_failure_cleanup_exceptions_still_observe_processes_and_write_evidence(self):
        with tempfile.TemporaryDirectory(prefix="supervisor-failure-cleanup-error-") as directory:
            root = Path(directory)
            suite = self.suite()
            suite.base = root / "private"
            suite.output = root / "output"
            suite.output.mkdir()
            suite.daemon = SimpleNamespace(
                pid=17, result={"exit_code": None}, poll=lambda: None,
            )
            drain = Mock(return_value=[])
            suite.processes = SimpleNamespace(drain=drain)
            suite.shutdown_gate = False
            suite.setup = lambda: None
            suite.start_daemon = lambda: None

            def fail_dispatch():
                raise RuntimeError("primary supervisor failure")

            suite.dispatch_and_observe = fail_dispatch
            suite.retained_failure_cleanup = Mock(
                side_effect=RuntimeError("retained cleanup failed"))
            suite.retain_unknown_failure_ownership = Mock(
                side_effect=RuntimeError("unknown retention failed"))

            with self.assertRaisesRegex(RuntimeError, "primary supervisor failure"):
                suite.run()

            suite.retained_failure_cleanup.assert_called_once_with()
            suite.retain_unknown_failure_ownership.assert_called_once_with()
            drain.assert_called_once_with(timeout=0, exclude=(suite.daemon,))
            receipt = native.read_json(suite.output / "failure.json")
            self.assertFalse(receipt["natural_failure_cleanup"])
            self.assertIn("retained cleanup failed", receipt["failure_cleanup_error"])
            self.assertIn("unknown retention failed", receipt["failure_cleanup_error"])

    def test_failure_proof_rejects_unknown_or_incomplete_dispatch(self):
        suite = self.suite()
        suite.admitted = False
        self.assertFalse(suite.failure_terminal_proof())

        suite.admitted = True
        with tempfile.TemporaryDirectory() as directory:
            suite.task_dir = Path(directory)
            self.assertFalse(suite.failure_terminal_proof())

    def test_failure_proof_accepts_no_dispatch_without_task_records(self):
        suite = self.suite()
        suite.dispatch_attempted = False
        self.assertTrue(suite.failure_terminal_proof())


if __name__ == "__main__":
    unittest.main()
