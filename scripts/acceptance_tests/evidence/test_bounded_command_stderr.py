"""The canonical bounded command owner captures stderr as a separate, equally bounded stream."""

import io
import os
import selectors
import subprocess
import sys
import unittest
from unittest.mock import Mock, patch

from scripts.acceptance_tests.release_fixture import open_descriptors
from scripts.delegate_acceptance.processes import operations
from scripts.delegate_acceptance.processes.contracts import (
    BoundedCommandError, BoundedCommandResult, CommandFailure,
)
from scripts.delegate_acceptance.processes.operations import run_bounded_command

LIMIT = 128
TIMEOUT_SECONDS = 5
LARGE_STREAM_BYTES = 1024 * 1024
STDOUT_FD, STDERR_FD = 10, 11
BOTH_ORDERS = ((STDOUT_FD, STDERR_FD), (STDERR_FD, STDOUT_FD))
EMIT = ("import sys; sys.stdout.buffer.write({out}); sys.stdout.buffer.flush(); "
        "sys.stderr.buffer.write({err}); sys.stderr.buffer.flush(); sys.exit({code})")
# Captured at import: the fakes patch Popen on the one shared subprocess module.
CHILD_SPEC = subprocess.Popen


class StderrCaptureTests(unittest.TestCase):
    """Real finite children: exact bytes, one shared bound, and every descriptor returned."""

    def run_child(self, *, out="b''", err="b''", code=0, maximum=LIMIT, **options):
        source = EMIT.format(out=out, err=err, code=code)
        return run_bounded_command([sys.executable, "-c", source], dict(os.environ), capture=True,
                                   timeout=TIMEOUT_SECONDS, max_output=maximum, **options)

    def test_default_capture_discards_stderr_outside_the_bound(self):
        result = self.run_child(out="b'OUT'", err="b'e' * 4096")
        self.assertEqual(result, BoundedCommandResult(0, b"OUT"))
        self.assertEqual(result.stderr, b"")

    def test_streams_are_returned_separately_with_exact_bytes_and_exit_code(self):
        result = self.run_child(out="b'OUT\\n'", err="b'ERR\\n'", code=3, capture_stderr=True)
        self.assertEqual(result, BoundedCommandResult(3, b"OUT\n", b"ERR\n"))

    def test_sink_receives_only_stdout_and_stderr_is_returned(self):
        chunks = []
        result = self.run_child(out="b'OUT'", err="b'ERR'", capture_stderr=True, sink=chunks.append)
        self.assertEqual(result, BoundedCommandResult(0, b"", b"ERR"))
        self.assertEqual(b"".join(chunks), b"OUT")

    def test_both_streams_share_the_one_output_bound(self):
        exact = self.run_child(out="b'o' * 100", err="b'e' * 28", capture_stderr=True)
        self.assertEqual(exact, BoundedCommandResult(0, b"o" * 100, b"e" * 28))
        for out, err in (("b'o' * 100", "b'e' * 29"), ("b''", "b'e' * 129")):
            with self.subTest(out=out, err=err), self.assertRaises(BoundedCommandError) as caught:
                self.run_child(out=out, err=err, capture_stderr=True)
            self.assertIs(caught.exception.reason, CommandFailure.OUTPUT_EXCEEDED)
            self.assertIsInstance(caught.exception.exit_code, int)

    def test_sink_delivered_bytes_count_against_the_shared_bound(self):
        chunks = []
        with self.assertRaises(BoundedCommandError) as caught:
            self.run_child(out="b'o' * 100", err="b'e' * 29", capture_stderr=True, sink=chunks.append)
        self.assertIs(caught.exception.reason, CommandFailure.OUTPUT_EXCEEDED)
        self.assertLessEqual(sum(map(len, chunks)), 100)

    def test_zero_bound_accepts_silence_and_rejects_one_stderr_byte(self):
        self.assertEqual(self.run_child(maximum=0, capture_stderr=True), BoundedCommandResult(0, b"", b""))
        with self.assertRaises(BoundedCommandError) as caught:
            self.run_child(err="b'e'", maximum=0, capture_stderr=True)
        self.assertIs(caught.exception.reason, CommandFailure.OUTPUT_EXCEEDED)

    def test_large_streams_are_drained_together_without_blocking_the_child(self):
        # A child that fills stderr before writing stdout blocks unless both pipes drain.
        result = self.run_child(out=f"b'o' * {LARGE_STREAM_BYTES}", err=f"b'e' * {LARGE_STREAM_BYTES}",
                                maximum=2 * LARGE_STREAM_BYTES, capture_stderr=True)
        self.assertEqual(result, BoundedCommandResult(0, b"o" * LARGE_STREAM_BYTES, b"e" * LARGE_STREAM_BYTES))

    def test_two_stream_capture_returns_every_descriptor(self):
        before = open_descriptors()
        self.run_child(out="b'OUT'", err="b'ERR'", capture_stderr=True)
        with self.assertRaises(BoundedCommandError):
            self.run_child(err="b'e' * 129", capture_stderr=True)
        self.assertEqual(open_descriptors(), before)


class StderrCaptureFaultTests(unittest.TestCase):
    """Deterministic two-pipe fakes: readiness order, EOF order, faults and closure."""

    def setUp(self):
        self.spawn = self.patch_operation(operations.subprocess, "Popen", side_effect=lambda *_a, **_k: self.child)
        self.construct = self.patch_operation(operations.selectors, "DefaultSelector",
                                              side_effect=lambda: self.selector)
        self.patch_operation(operations.os, "read", side_effect=self.read)
        self.patch_operation(operations, "_bounded_command_pending_bytes",
                             side_effect=lambda pipe: len(self.pending[pipe.fileno()]))
        self.patch_operation(operations.time, "monotonic", return_value=100)
        self.build()

    def patch_operation(self, target, name, **options):
        replacement = patch.object(target, name, **options)
        result = replacement.start()
        self.addCleanup(replacement.stop)
        return result

    def build(self, *, stdout=b"", stderr=b"", readiness=BOTH_ORDERS[0]):
        """A fresh child, pipes and selector; ``readiness`` orders every select result."""
        self.child = Mock(spec=CHILD_SPEC)
        self.child.returncode = None
        self.child.poll.return_value = None
        self.child.wait.side_effect = self.wait_child
        self.child.stdout, self.child.stderr = self.pipe(STDOUT_FD), self.pipe(STDERR_FD)
        self.pipes = {STDOUT_FD: self.child.stdout, STDERR_FD: self.child.stderr}
        self.pending = {STDOUT_FD: bytearray(stdout), STDERR_FD: bytearray(stderr)}
        self.readiness = readiness
        self.registered = []
        self.reads = []
        self.faults = {}
        self.selector = Mock(spec=selectors.BaseSelector)
        self.selector.register.side_effect = self.register
        self.selector.unregister.side_effect = self.unregister
        self.selector.select.side_effect = self.ready
        self.order = Mock()
        for name in ("kill", "wait"):
            self.order.attach_mock(getattr(self.child, name), name)
        self.order.attach_mock(self.child.stdout.close, "stdout_close")
        self.order.attach_mock(self.child.stderr.close, "stderr_close")

    @staticmethod
    def pipe(fd):
        pipe = Mock(spec=io.BufferedReader)
        pipe.fileno.return_value = fd
        return pipe

    def raise_fault(self, stage, fd):
        error = self.faults.get((stage, fd))
        if error is not None:
            raise error

    def register(self, pipe, _events):
        self.raise_fault("register", pipe.fileno())
        self.registered.append(pipe)

    def unregister(self, pipe):
        self.raise_fault("unregister", pipe.fileno())
        self.registered.remove(pipe)

    def ready(self, _timeout):
        return [(selectors.SelectorKey(self.pipes[fd], fd, selectors.EVENT_READ, None), selectors.EVENT_READ)
                for fd in self.readiness if self.pipes[fd] in self.registered]

    def read(self, fd, size):
        self.raise_fault("read", fd)
        self.reads.append(fd)
        chunk = bytes(self.pending[fd][:size])
        del self.pending[fd][:size]
        return chunk

    def wait_child(self, *, timeout):
        self.child.returncode = -9 if self.child.kill.called else 0
        return self.child.returncode

    def invoke(self, *, maximum=LIMIT, **options):
        return run_bounded_command(["finite-control-fixture"], {}, max_output=maximum, capture=True,
                                   capture_stderr=True, **options)

    def assert_pipes_closed_after_wait(self):
        self.child.stdout.close.assert_called_once()
        self.child.stderr.close.assert_called_once()
        calls = [call[0] for call in self.order.mock_calls]
        last_wait = len(calls) - 1 - calls[::-1].index("wait")
        self.assertLess(last_wait, calls.index("stdout_close"))
        self.assertLess(last_wait, calls.index("stderr_close"))

    def assert_clean(self):
        self.child.kill.assert_not_called()
        self.child.wait.assert_called_once()
        self.selector.close.assert_called_once()
        self.assertEqual(self.registered, [])
        self.assert_pipes_closed_after_wait()

    def assert_killed(self, *, selector=True):
        self.child.kill.assert_called()
        self.assertEqual(self.child.returncode, -9)
        self.assertEqual(self.selector.close.call_count, 1 if selector else 0)
        self.assert_pipes_closed_after_wait()

    def test_spawn_requests_a_separate_stderr_pipe_only_when_asked(self):
        self.invoke()
        self.assertEqual(self.spawn.call_args.kwargs["stderr"], subprocess.PIPE)
        self.assertEqual(self.spawn.call_args.kwargs["stdout"], subprocess.PIPE)
        self.build(stdout=b"OUT")
        result = run_bounded_command(["finite-control-fixture"], {}, max_output=LIMIT, capture=True)
        self.assertEqual(result, BoundedCommandResult(0, b"OUT"))
        self.assertEqual(self.spawn.call_args.kwargs["stderr"], subprocess.DEVNULL)
        self.child.stderr.fileno.assert_not_called()
        self.child.stderr.close.assert_not_called()

    def test_stderr_capture_requires_capture_and_starts_no_child(self):
        for options in ({}, {"sink": lambda _chunk: None}):
            with self.subTest(options=options), self.assertRaises(ValueError):
                run_bounded_command(["finite-control-fixture"], {}, max_output=LIMIT, capture_stderr=True,
                                    **options)
        self.spawn.assert_not_called()

    def test_both_readiness_orders_return_the_same_separate_bytes(self):
        for order in BOTH_ORDERS:
            with self.subTest(order=order):
                self.build(stdout=b"OUT", stderr=b"ERR", readiness=order)
                self.assertEqual(self.invoke(), BoundedCommandResult(0, b"OUT", b"ERR"))
                self.assertEqual(self.reads[:2], list(order))
                self.assert_clean()

    def test_either_stream_may_reach_eof_first(self):
        for order in BOTH_ORDERS:
            for silent in (STDOUT_FD, STDERR_FD):
                with self.subTest(order=order, silent=silent):
                    loud = STDERR_FD if silent == STDOUT_FD else STDOUT_FD
                    payload = {silent: b"", loud: b"x" * 3}
                    self.build(stdout=payload[STDOUT_FD], stderr=payload[STDERR_FD], readiness=order)
                    result = self.invoke()
                    self.assertEqual((result.output, result.stderr), (payload[STDOUT_FD], payload[STDERR_FD]))
                    self.assertEqual(self.selector.unregister.call_args_list[0].args[0], self.pipes[silent])
                    self.assert_clean()

    def test_both_streams_share_one_bound_in_either_readiness_order(self):
        cases = ((LIMIT, b"o" * 100, b"e" * 28, None), (LIMIT, b"o" * 100, b"e" * 29, "exceeded"),
                 (LIMIT, b"", b"e" * (LIMIT + 1), "exceeded"), (0, b"", b"", None), (0, b"", b"e", "exceeded"),
                 (0, b"o", b"", "exceeded"))
        for order in BOTH_ORDERS:
            for maximum, stdout, stderr, outcome in cases:
                with self.subTest(order=order, maximum=maximum, stdout=len(stdout), stderr=len(stderr)):
                    self.build(stdout=stdout, stderr=stderr, readiness=order)
                    if outcome is None:
                        self.assertEqual(self.invoke(maximum=maximum), BoundedCommandResult(0, stdout, stderr))
                        self.assert_clean()
                        continue
                    with self.assertRaises(BoundedCommandError) as caught:
                        self.invoke(maximum=maximum)
                    self.assertIs(caught.exception.reason, CommandFailure.OUTPUT_EXCEEDED)
                    self.assertEqual(caught.exception.exit_code, -9)
                    self.assert_killed()

    def test_sink_bytes_count_against_the_shared_bound(self):
        for stderr, exceeded in ((b"e" * 28, False), (b"e" * 29, True)):
            with self.subTest(stderr=len(stderr)):
                chunks = []
                self.build(stdout=b"o" * 100, stderr=stderr)
                if not exceeded:
                    self.assertEqual(self.invoke(sink=chunks.append), BoundedCommandResult(0, b"", stderr))
                    self.assertEqual(b"".join(chunks), b"o" * 100)
                    continue
                with self.assertRaises(BoundedCommandError) as caught:
                    self.invoke(sink=chunks.append)
                self.assertIs(caught.exception.reason, CommandFailure.OUTPUT_EXCEEDED)
                self.assertEqual(b"".join(chunks), b"o" * 100)

    def test_shared_deadline_closes_the_child_and_both_pipes(self):
        self.build(stdout=b"OUT")
        self.selector.select.side_effect = lambda timeout: []
        with self.assertRaises(BoundedCommandError) as caught:
            self.invoke(timeout=2)
        self.assertIs(caught.exception.reason, CommandFailure.TIMEOUT)
        self.selector.select.assert_called_once_with(2.0)
        self.assert_killed()

    def test_capture_io_faults_on_either_pipe_report_unavailable_capture(self):
        for stage, fd in (("register", STDERR_FD), ("read", STDOUT_FD), ("read", STDERR_FD),
                          ("unregister", STDOUT_FD), ("unregister", STDERR_FD)):
            with self.subTest(stage=stage, fd=fd):
                self.build(stdout=b"OUT", stderr=b"ERR")
                self.faults[(stage, fd)] = OSError("private-capture-detail")
                with self.assertRaises(BoundedCommandError) as caught:
                    self.invoke()
                self.assertIs(caught.exception.reason, CommandFailure.CAPTURE_UNAVAILABLE)
                self.assertEqual(str(caught.exception), CommandFailure.CAPTURE_UNAVAILABLE.value)
                self.assertEqual(caught.exception.exit_code, -9)
                self.assert_killed()

    def test_selector_close_fault_after_a_complete_drain_still_closes_everything(self):
        self.build(stdout=b"OUT", stderr=b"ERR")
        self.selector.close.side_effect = OSError("private-selector-detail")
        with self.assertRaises(BoundedCommandError) as caught:
            self.invoke()
        self.assertIs(caught.exception.reason, CommandFailure.CAPTURE_UNAVAILABLE)
        self.assert_killed()

    def test_missing_stderr_pipe_waits_for_the_child_without_a_selector(self):
        self.build()
        self.child.stderr = None
        with self.assertRaises(BoundedCommandError) as caught:
            self.invoke()
        self.assertIs(caught.exception.reason, CommandFailure.CAPTURE_UNAVAILABLE)
        self.child.kill.assert_called_once()
        self.child.wait.assert_called_once()
        self.child.stdout.close.assert_called_once()
        self.construct.assert_not_called()

    def test_each_pipe_close_fault_attempts_both_closes_and_keeps_the_primary(self):
        for failing in (("stdout",), ("stderr",), ("stdout", "stderr")):
            with self.subTest(failing=failing):
                self.build(stdout=b"OUT")
                primary = RuntimeError("primary capture failure")
                self.faults[("read", STDOUT_FD)] = primary
                for name in failing:
                    getattr(self.child, name).close.side_effect = OSError(f"private-{name}-close")
                with self.assertRaises(BoundedCommandError) as caught:
                    self.invoke()
                self.assertIs(caught.exception.reason, CommandFailure.CAPTURE_UNAVAILABLE)
                self.assertIs(caught.exception.__cause__, primary)
                self.assertEqual(str(caught.exception), CommandFailure.CAPTURE_UNAVAILABLE.value)
                notes = getattr(caught.exception, "__notes__", [])
                self.assertEqual([note.split(":")[0] for note in notes],
                                 ["stderr pipe close failed"] if len(failing) == 2 else [])
                self.assert_killed()

    def test_pipe_close_fault_after_natural_exit_keeps_the_exact_code(self):
        self.build(stdout=b"OUT", stderr=b"ERR")
        closure = OSError("private-stderr-close")
        self.child.stderr.close.side_effect = closure
        with self.assertRaises(BoundedCommandError) as caught:
            self.invoke()
        self.assertIs(caught.exception.reason, CommandFailure.CAPTURE_UNAVAILABLE)
        self.assertEqual(caught.exception.exit_code, 0)
        self.assertIs(caught.exception.__cause__, closure)
        self.assert_clean()

    def test_kill_and_wait_failures_keep_closure_classification(self):
        cases = (("wait", subprocess.TimeoutExpired("fixture", 1), CommandFailure.TERMINATION_TIMEOUT),
                 ("wait", OSError("private-wait-detail"), CommandFailure.TERMINATION_FAILED),
                 ("kill", OSError("private-kill-detail"), None))
        for target, fault, reason in cases:
            with self.subTest(target=target, fault=type(fault).__name__):
                self.build(stdout=b"OUT", stderr=b"ERR")
                primary = RuntimeError("primary capture failure")
                self.faults[("read", STDERR_FD)] = primary
                getattr(self.child, target).side_effect = fault
                expected = RuntimeError if reason is None else BoundedCommandError
                with self.assertRaises(expected) as caught:
                    self.invoke()
                self.child.stdout.close.assert_called_once()
                self.child.stderr.close.assert_called_once()
                self.selector.close.assert_called_once()
                if reason is None:
                    self.assertIs(caught.exception, primary)
                    self.assertIs(caught.exception.__cause__, fault)
                    self.assertEqual(self.child.returncode, -9)
                    continue
                self.assertIs(caught.exception.reason, reason)
                self.assertIsNone(caught.exception.exit_code)
                self.assertIs(caught.exception.__cause__, primary)


if __name__ == "__main__":
    unittest.main()
