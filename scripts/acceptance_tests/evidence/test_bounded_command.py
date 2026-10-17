"""The canonical process owner waits finite release commands and bounds output."""

import io
import os
import selectors
import subprocess
import sys
import unittest
from unittest.mock import Mock, patch

from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, CommandFailure
from scripts.delegate_acceptance.processes.operations import run_bounded_command
from scripts.delegate_acceptance.processes import operations


class BoundedCommandTests(unittest.TestCase):
    def run_python(self, source: str, *, maximum: int = 128, timeout: float = 5):
        return run_bounded_command(
            [sys.executable, "-c", source], dict(os.environ),
            capture=True, timeout=timeout, max_output=maximum,
        )

    def test_output_at_the_exact_limit_is_complete(self):
        result = self.run_python("import sys; sys.stdout.buffer.write(b'x' * 128)")
        self.assertEqual(result.exit_code, 0)
        self.assertEqual(result.output, b"x" * 128)

    def test_output_beyond_the_limit_is_explicitly_rejected(self):
        with self.assertRaises(BoundedCommandError) as captured:
            self.run_python("import sys; sys.stdout.buffer.write(b'x' * 129)")
        self.assertIs(captured.exception.reason, CommandFailure.OUTPUT_EXCEEDED)
        self.assertIsInstance(captured.exception.exit_code, int)

    def test_timeout_waits_for_the_owned_child_to_terminate(self):
        with self.assertRaises(BoundedCommandError) as captured:
            self.run_python("import signal; signal.pause()", timeout=1)
        self.assertIs(captured.exception.reason, CommandFailure.TIMEOUT)
        self.assertIsInstance(captured.exception.exit_code, int)
        self.assertLess(captured.exception.exit_code, 0)

    def test_nonzero_exit_remains_an_observation_for_the_consumer(self):
        result = self.run_python("import sys; sys.exit(7)")
        self.assertEqual(result.exit_code, 7)
        self.assertEqual(result.output, b"")


class BoundedCommandFailureTests(unittest.TestCase):
    def setUp(self):
        self.child = Mock(spec=subprocess.Popen)
        self.child.stdout = Mock(spec=io.BufferedReader)
        self.child.stdout.fileno.return_value = 10
        self.child.returncode = None
        self.child.poll.return_value = None
        self.child.wait.side_effect = self.wait_child
        self.selector = Mock(spec=selectors.BaseSelector)
        self.selector.select.return_value = [(None, selectors.EVENT_READ)]
        self.order = Mock()
        self.order.attach_mock(self.child.kill, "kill")
        self.order.attach_mock(self.child.wait, "wait")
        self.order.attach_mock(self.child.stdout.close, "pipe_close")
        self.order.attach_mock(self.selector.close, "selector_close")
        self.spawn = self.patch_operation(operations.subprocess, "Popen", return_value=self.child)
        self.construct = self.patch_operation(operations.selectors, "DefaultSelector", return_value=self.selector)
        self.read = self.patch_operation(operations.os, "read", return_value=b"")
        self.patch_operation(operations.time, "monotonic", return_value=100)

    def patch_operation(self, target, name, **options):
        replacement = patch.object(target, name, **options)
        result = replacement.start()
        self.addCleanup(replacement.stop)
        return result

    def reset_faults(self):
        self.child.reset_mock(side_effect=True)
        self.selector.reset_mock(side_effect=True)
        self.construct.reset_mock(side_effect=True)
        self.read.reset_mock(side_effect=True)
        self.order.reset_mock()
        self.child.returncode = None
        self.child.wait.side_effect = self.wait_child

    def wait_child(self, *, timeout):
        self.child.returncode = -9 if self.child.kill.called else 0
        return self.child.returncode

    def invoke(self, **options):
        return run_bounded_command(["finite-control-fixture"], {}, max_output=128, capture=True, **options)

    def assert_closed(self, *, selector=True):
        self.child.kill.assert_called()
        self.child.wait.assert_called()
        self.assertEqual(self.child.returncode, -9)
        self.child.stdout.close.assert_called_once()
        calls = [call[0] for call in self.order.mock_calls]
        self.assertLess(calls.index("wait"), calls.index("pipe_close"))
        if selector:
            self.selector.close.assert_called_once()
        else:
            self.selector.close.assert_not_called()

    def test_capture_io_faults_are_sanitized_and_close_every_acquired_resource(self):
        for stage in ("constructor", "register", "select", "read"):
            with self.subTest(stage=stage):
                self.reset_faults()
                failure = OSError("private-capture-detail")
                target = {"constructor": self.construct, "register": self.selector.register,
                          "select": self.selector.select, "read": self.read}[stage]
                target.side_effect = failure
                with self.assertRaises(BoundedCommandError) as caught:
                    self.invoke()
                self.assertIs(caught.exception.reason, CommandFailure.CAPTURE_UNAVAILABLE)
                self.assertEqual(str(caught.exception), CommandFailure.CAPTURE_UNAVAILABLE.value)
                self.assertEqual(caught.exception.exit_code, -9)
                self.assert_closed(selector=stage != "constructor")

    def test_unknown_errors_and_cancellation_keep_the_primary_exception_after_wait(self):
        for failure in (RuntimeError("unexpected fixture failure"), KeyboardInterrupt(), SystemExit(17)):
            with self.subTest(kind=type(failure).__name__):
                self.reset_faults()
                self.read.side_effect = failure
                with self.assertRaises(type(failure)) as caught:
                    self.invoke()
                self.assertIs(caught.exception, failure)
                self.assert_closed()

    def test_selector_close_failure_cannot_bypass_child_wait_or_pipe_close(self):
        self.selector.close.side_effect = OSError("private-selector-detail")
        with self.assertRaises(BoundedCommandError) as caught:
            self.invoke()
        self.assertIs(caught.exception.reason, CommandFailure.CAPTURE_UNAVAILABLE)
        self.assert_closed()

    def test_read_failure_keeps_primary_when_selector_close_also_fails(self):
        primary = RuntimeError("primary read failure")
        secondary = OSError("secondary selector close failure")
        self.read.side_effect = primary
        self.selector.close.side_effect = secondary
        with self.assertRaises(RuntimeError) as caught:
            self.invoke()
        self.assertIs(caught.exception, primary)
        self.assertIs(caught.exception.__cause__, secondary)
        self.assert_closed()

    def test_typed_primary_retains_selector_cleanup_evidence_and_exact_wait_code(self):
        primary = BoundedCommandError(CommandFailure.TIMEOUT)
        secondary = OSError("secondary selector close failure")
        self.read.side_effect = primary
        self.selector.close.side_effect = secondary
        with self.assertRaises(BoundedCommandError) as caught:
            self.invoke()
        self.assertIs(caught.exception, primary)
        self.assertIs(caught.exception.__cause__, secondary)
        self.assertEqual(caught.exception.exit_code, -9)
        self.assert_closed()

    def test_kill_error_cannot_skip_wait_and_preserves_the_primary_failure(self):
        primary = RuntimeError("primary capture failure")
        self.read.side_effect = primary
        self.child.kill.side_effect = OSError("private-kill-detail")
        with self.assertRaises(RuntimeError) as caught:
            self.invoke()
        self.assertIs(caught.exception, primary)
        self.assertIsInstance(caught.exception.__cause__, OSError)
        self.assert_closed()

    def test_pipe_close_error_is_reported_after_the_primary_child_was_waited(self):
        primary = RuntimeError("primary capture failure")
        self.read.side_effect = primary
        self.child.stdout.close.side_effect = OSError("private-pipe-detail")
        with self.assertRaises(BoundedCommandError) as caught:
            self.invoke()
        self.assertIs(caught.exception.reason, CommandFailure.CAPTURE_UNAVAILABLE)
        self.assertIs(caught.exception.__cause__, primary)
        self.assert_closed()

    def test_unproven_wait_timeout_reports_closure_failure_and_keeps_primary(self):
        primary = RuntimeError("primary capture failure")
        self.read.side_effect = primary
        self.child.wait.side_effect = subprocess.TimeoutExpired("fixture", 1)
        with self.assertRaises(BoundedCommandError) as caught:
            self.invoke()
        self.assertIs(caught.exception.reason, CommandFailure.TERMINATION_TIMEOUT)
        self.assertIsNone(caught.exception.exit_code)
        self.assertIs(caught.exception.__cause__, primary)
        self.assertEqual(self.child.wait.call_count, 2)
        self.assertEqual(self.child.kill.call_count, 2)
        self.child.stdout.close.assert_called_once()
        self.selector.close.assert_called_once()

    def test_non_timeout_wait_failure_reports_unproven_closure_with_safe_code(self):
        primary = RuntimeError("primary capture failure")
        self.read.side_effect = primary
        self.child.wait.side_effect = OSError("private-wait-detail")
        with self.assertRaises(BoundedCommandError) as caught:
            self.invoke()
        self.assertEqual(caught.exception.reason.value, "command_termination_failed")
        self.assertIsNone(caught.exception.exit_code)
        self.assertIs(caught.exception.__cause__, primary)
        self.assertNotIn("private-wait-detail", str(caught.exception))
        self.child.wait.assert_called_once()
        self.child.stdout.close.assert_called_once()

    def test_wait_failure_and_pipe_failure_retain_the_full_failure_chain(self):
        primary = RuntimeError("primary capture failure")
        self.read.side_effect = primary
        self.child.wait.side_effect = subprocess.TimeoutExpired("fixture", 1)
        self.child.stdout.close.side_effect = OSError("private-pipe-detail")
        with self.assertRaises(BoundedCommandError) as caught:
            self.invoke()
        self.assertIs(caught.exception.reason, CommandFailure.CAPTURE_UNAVAILABLE)
        self.assertIsNone(caught.exception.exit_code)
        closure = caught.exception.__cause__
        self.assertIsInstance(closure, BoundedCommandError)
        self.assertIs(closure.reason, CommandFailure.TERMINATION_TIMEOUT)
        self.assertIs(closure.__cause__, primary)
        self.assertEqual(self.child.wait.call_count, 2)
        self.child.stdout.close.assert_called_once()

    def test_failed_kill_with_timed_out_wait_still_performs_the_second_wait(self):
        primary = RuntimeError("primary capture failure")
        self.read.side_effect = primary
        self.child.kill.side_effect = OSError("private-kill-detail")

        def wait(*, timeout):
            if self.child.wait.call_count == 1:
                raise subprocess.TimeoutExpired("fixture", timeout)
            return self.wait_child(timeout=timeout)

        self.child.wait.side_effect = wait
        with self.assertRaises(RuntimeError) as caught:
            self.invoke()
        self.assertIs(caught.exception, primary)
        self.assertEqual(self.child.wait.call_count, 2)
        self.assertEqual(self.child.kill.call_count, 2)
        self.assert_closed()

    def test_missing_capture_pipe_still_waits_for_its_owned_child(self):
        self.child.stdout = None
        with self.assertRaises(BoundedCommandError) as caught:
            self.invoke()
        self.assertIs(caught.exception.reason, CommandFailure.CAPTURE_UNAVAILABLE)
        self.child.kill.assert_called_once()
        self.child.wait.assert_called_once()
        self.construct.assert_not_called()

    def test_non_capture_unknown_wait_failure_also_closes_the_child(self):
        primary = RuntimeError("primary wait failure")
        def wait(*, timeout):
            if self.child.wait.call_count == 1:
                raise primary
            return self.wait_child(timeout=timeout)

        self.child.wait.side_effect = wait
        self.child.stdout = None
        with self.assertRaises(RuntimeError) as caught:
            run_bounded_command(["finite-control-fixture"], {}, max_output=0)
        self.assertIs(caught.exception, primary)
        self.child.kill.assert_called_once()
        self.assertEqual(self.child.wait.call_count, 2)

    def test_invalid_bounds_fail_before_starting_a_child(self):
        cases = [{"timeout": value, "max_output": 0} for value in (0, -1, float("nan"), float("inf"))]
        cases.extend({"timeout": 1, "max_output": value} for value in (-1, 1.5))
        for options in cases:
            with self.subTest(options=options), self.assertRaises(ValueError):
                run_bounded_command(["finite-control-fixture"], {}, **options)
        self.spawn.assert_not_called()

    def test_start_failure_keeps_its_code_and_does_not_close_an_unowned_child(self):
        self.spawn.side_effect = OSError("private-start-detail")
        with self.assertRaises(BoundedCommandError) as caught:
            self.invoke()
        self.assertIs(caught.exception.reason, CommandFailure.START_FAILED)
        self.assertIsNone(caught.exception.exit_code)
        self.child.kill.assert_not_called()
        self.child.wait.assert_not_called()
        self.child.stdout.close.assert_not_called()
        self.construct.assert_not_called()

    def test_zero_capture_limit_and_positive_finite_timeout_remain_valid(self):
        self.child.poll.return_value = 0
        with patch.object(operations, "_bounded_command_pending_bytes", return_value=0):
            result = run_bounded_command(["finite-control-fixture"], {}, max_output=0, capture=True, timeout=0.5)
        self.assertEqual(result.output, b"")
        self.assertEqual(result.exit_code, 0)
        self.child.kill.assert_not_called()
        self.child.wait.assert_called_once()
        self.child.stdout.close.assert_called_once()


class BoundedCommandFiniteChildFailureTests(unittest.TestCase):
    def test_real_finite_control_child_is_waited_after_selector_construction_fails(self):
        original_spawn = subprocess.Popen
        children = []

        def spawn(*args, **kwargs):
            child = original_spawn(*args, **kwargs)
            children.append(child)
            return child

        try:
            with (
                patch.object(operations.subprocess, "Popen", side_effect=spawn),
                patch.object(operations.selectors, "DefaultSelector", side_effect=OSError("private-selector-detail")),
            ):
                with self.assertRaises(BoundedCommandError) as caught:
                    run_bounded_command(
                        [sys.executable, "-c", "pass"], dict(os.environ),
                        capture=True, max_output=128, timeout=5,
                    )
            self.assertIs(caught.exception.reason, CommandFailure.CAPTURE_UNAVAILABLE)
            self.assertEqual(len(children), 1)
            self.assertIsInstance(children[0].returncode, int)
            self.assertEqual(caught.exception.exit_code, children[0].returncode)
            self.assertTrue(children[0].stdout.closed)
        finally:
            for child in children:
                try:
                    child.wait(timeout=5)
                finally:
                    child.stdout.close()


if __name__ == "__main__":
    unittest.main()
