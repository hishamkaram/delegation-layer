"""Pure ownership-oracle tests for the native supervisor acceptance driver."""

from __future__ import annotations

from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import acceptance_supervisor_native as native


class NativeEventOracleTests(unittest.TestCase):
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

                def bind_supervisor(self, *_args):
                    pass

                def status(self):
                    return {"tasks": {"7": {
                        "id": 7,
                        "label": "delegate:" + self.suite.root_id + ":" + self.suite.task_id,
                        "status": {"Done": {"result": "Success"}},
                    }}}

                def failure_queue_finished(self, _status):
                    return True

                def safe_failure_shutdown(self):
                    if not self.failure_queue_finished(self.status()):
                        return False
                    calls.append("shutdown")
                    return True

                def retain_failure_ownership(self):
                    return self.safe_failure_shutdown()

            def events():
                calls.append("events")

            def terminal():
                calls.append("terminal")

            with patch.object(native, "NativeTaskOps", FreshOwner), \
                    patch.object(suite, "validate_events", side_effect=events), \
                    patch.object(suite, "validate_terminal", side_effect=terminal):
                suite.native_terminal_proven = False
                self.assertTrue(suite.retained_failure_cleanup())
            self.assertEqual(suite.native_job["id"], 7)
            self.assertEqual(calls, ["events", "terminal", "shutdown"])

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
