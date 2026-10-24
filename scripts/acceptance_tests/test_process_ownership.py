"""Start ownership and finalization progress of the canonical Process owner.

Every child is a finite local Python program with finite stdin; tests send no
signals and observe each exit through the exact natural Wait.  Run natively with
``python3 -m unittest scripts.acceptance_tests.test_process_ownership -v``.
"""

from contextlib import nullcontext
import os
from pathlib import Path
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.directories import operations as directories
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.evidence.files import read_json
from scripts.delegate_acceptance.processes import operations as processes

PYTHON = str(Path(sys.executable).resolve())
STATUS_CHILD = (
    "import sys\n"
    "data = sys.stdin.buffer.read()\n"
    "sys.stdout.buffer.write(b'{\"tasks\": {}, \"stdin\": %d}\\n' % len(data))\n"
    "sys.stderr.buffer.write(b'finite stderr\\n')\n"
)
ECHO_CHILD = "import sys; sys.stdout.buffer.write(sys.stdin.buffer.read())"


class RecordingCapture:
    """Finite pipe capture whose sanitized() consumes its buffer like AGY status."""

    def __init__(self, close_failures=0):
        read_fd, write_fd = os.pipe()
        self.reader = os.fdopen(read_fd, "rb", buffering=0)
        self.writer = os.fdopen(write_fd, "wb", buffering=0)
        os.set_blocking(read_fd, False)
        self.buffer = bytearray()
        self.eof = False
        self.error = None
        self.sanitized_calls = 0
        self.close_failures = close_failures

    def drain(self):
        while True:
            try:
                chunk = os.read(self.reader.fileno(), 65536)
            except BlockingIOError:
                return
            if not chunk:
                self.eof = True
                return
            self.buffer.extend(chunk)

    def sanitized(self, code):
        self.sanitized_calls += 1
        data = bytes(self.buffer)
        self.buffer.clear()
        return data

    def close(self):
        if self.close_failures:
            self.close_failures -= 1
            raise OSError("capture close fault")
        self.writer.close()
        self.reader.close()


class CloseFault:
    """Delegate one owned stream; its first close fails without closing."""

    def __init__(self, stream):
        self.stream = stream
        self.failures = 1

    def __getattr__(self, name):
        return getattr(self.stream, name)

    def close(self):
        if self.failures:
            self.failures -= 1
            raise OSError("stream close fault")
        self.stream.close()


def fail_once(original, matches, error):
    state = {"failed": False}

    def call(*args, **kwargs):
        if not state["failed"] and matches(*args):
            state["failed"] = True
            raise error
        return original(*args, **kwargs)
    return call


def close_capture(capture):
    capture.close_failures = 0
    capture.close()


def settle_children(book):
    """Test cleanup: give each finite child EOF, take its exact Wait, close streams."""
    for process in book.entries:
        if process.process.stdin is not None:
            process.process.stdin.close()
        process.process.wait(timeout=10)
        for stream in (process.stdout, process.stderr):
            getattr(stream, "stream", stream).close()


def descriptor_errno(descriptor):
    try:
        os.fstat(descriptor)
    except OSError as error:
        return error.errno
    return None


# fstat(-1) reports the platform's EBADF without opening any descriptor.
BAD_DESCRIPTOR = descriptor_errno(-1)


def consume_if_live(token):
    """Test cleanup: close a capture token's pins if the code under test did not."""
    try:
        token.consume()
    except RuntimeError as error:
        if str(error) != "owned directory authority was already consumed: " + str(token.directory):
            raise


class ProcessOwnershipTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="process-ownership-")
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name).resolve()
        self.owner = DirectoryOwner()
        self.addCleanup(self.owner.rollback)

    def book(self, name, capture=None):
        factory = (lambda argv: capture) if capture is not None else None
        if capture is not None:
            self.addCleanup(close_capture, capture)
        book = processes.Processes(self.root / name, {}, owner=self.owner,
                                   stdout_capture_factory=factory)
        self.addCleanup(settle_children, book)
        return book

    def test_post_start_setup_failure_keeps_registered_child_and_capture(self):
        stages = (
            ("set_blocking", patch.object(processes.os, "set_blocking",
                                          side_effect=OSError("setup fault")), b"payload"),
            ("transfer", patch.object(processes.DirectoryOwner, "transfer_child_to",
                                      side_effect=OSError("setup fault")), None),
        )
        expected = b'{"tasks": {}, "stdin": 0}\n'
        for stage, fault, stdin in stages:
            with self.subTest(stage=stage):
                capture = RecordingCapture()
                book = self.book(stage, capture)
                with fault, self.assertRaisesRegex(OSError, "setup fault"):
                    book.start(stage, [PYTHON, "-c", STATUS_CHILD], self.root, stdin=stdin)
                self.assertEqual(len(book.entries), 1)
                process = book.entries[0]
                self.assertTrue(process._stdin_closed)
                self.assertTrue(process.process.stdin is None or process.process.stdin.closed)
                self.assertTrue(capture.writer.closed)
                self.assertFalse(capture.reader.closed)
                self.assertFalse(process.stdout.closed)
                self.assertTrue(process.directory.is_dir())
                self.assertFalse((process.directory / "started.json").exists())
                if stdin is None:
                    self.assertEqual(process.wait(10), 0)
                else:
                    with self.assertRaisesRegex(RuntimeError, "stdin write failed"):
                        process.wait(10)
                    self.assertEqual(process.result["stdin_error"],
                                     "stdin closed after post-start setup failure")
                self.assertTrue(process.result["natural_wait"])
                self.assertEqual(process.result["exit_code"], 0)
                self.assertIsNone(process.result["capture_error"])
                self.assertEqual(process.stdout_path.read_bytes(), expected)
                self.assertEqual(capture.sanitized_calls, 1)
                self.assertTrue(capture.reader.closed)
                self.assertTrue(book.finished_naturally())

    def test_post_start_transfer_failure_registers_and_adopts_once_before_raising(self):
        book = self.book("adopted")
        events = []

        def adopt(process):
            # Registration must already hold the exact handle being adopted.
            events.append(("adopt", process, tuple(book.entries)))

        def fail_transfer(*_args, **_kwargs):
            events.append(("transfer",))
            raise OSError("transfer fault")
        with patch.object(processes.DirectoryOwner, "transfer_child_to", side_effect=fail_transfer), \
                self.assertRaisesRegex(OSError, "transfer fault"):
            book.start("adopted", [PYTHON, "-c", STATUS_CHILD], self.root, on_spawned=adopt)
        self.assertEqual(len(book.entries), 1)
        process = book.entries[0]
        self.assertEqual(events, [("adopt", process, (process,)), ("transfer",)])
        self.assertIs(events[0][1], process)
        self.assertFalse((process.directory / "started.json").exists())
        self.assertEqual(process.wait(10), 0)
        self.assertTrue(process.result["natural_wait"])
        self.assertEqual(book.pending(), [])

    def test_pre_start_failures_roll_back_without_starting_or_registering(self):
        capture = RecordingCapture()
        book = self.book("unstarted", capture)
        adopted = []
        stderr_fault = fail_once(Path.open, lambda path, *rest: path.name == "stderr",
                                 OSError("setup fault"))
        with patch.object(processes.Path, "open", new=stderr_fault), \
                patch.object(processes.subprocess, "Popen") as popen, \
                self.assertRaisesRegex(OSError, "setup fault"):
            book.start("unstarted", [PYTHON, "-c", "pass"], self.root, on_spawned=adopted.append)
        popen.assert_not_called()
        self.assertEqual(book.entries, [])
        self.assertEqual(book.pending(), [])
        self.assertEqual(book.drain(timeout=0), [])
        self.assertTrue(book.finished_naturally())
        self.assertEqual(adopted, [])
        self.assertTrue(capture.writer.closed and capture.reader.closed)
        self.assertEqual(list(book.directory.iterdir()), [])

        child = book.directory / "invalid-owner"
        with patch.object(processes.subprocess, "Popen") as popen, \
                self.assertRaisesRegex(RuntimeError, "capture root identity token is invalid"):
            processes.Process(child, [PYTHON, "-c", "pass"], self.root, {}, object())
        popen.assert_not_called()
        self.assertFalse(child.exists())

    def record_capture_pins(self, pins):
        """Patch capture creation to collect each new token's two pinned descriptors."""
        original = DirectoryOwner.create

        def record_pins(owner, *args, **kwargs):
            token = original(owner, *args, **kwargs)
            # Retaining the token lets a failed assertion still close its pins.
            self.addCleanup(consume_if_live, token)
            pins.extend((token.parent_fd, token.directory_fd))
            return token
        return patch.object(processes.DirectoryOwner, "create", autospec=True,
                            side_effect=record_pins)

    def start_unstarted(self, name, fault):
        """Fail pre-Start setup under one cleanup fault; return the error and the capture pins."""
        book = self.book(name)
        pins = []
        invocation_fault = fail_once(
            processes.write_json, lambda path, *rest: Path(path).name == "invocation.json",
            OSError("invocation fault"))
        with self.record_capture_pins(pins), \
                patch.object(processes, "write_json", side_effect=invocation_fault), \
                patch.object(processes.subprocess, "Popen") as popen, \
                fault, self.assertRaisesRegex(OSError, "invocation fault") as raised:
            book.start(name, [PYTHON, "-c", "pass"], self.root)
        popen.assert_not_called()
        return book, pins, raised.exception

    def test_pre_start_rollback_failure_releases_pins_and_retains_capture(self):
        book, pins, error = self.start_unstarted("rollback-fault", patch.object(
            processes.DirectoryOwner, "rollback", side_effect=OSError("rollback fault")))
        self.assertEqual(len(pins), 2)
        self.assertEqual([descriptor_errno(pin) for pin in pins], [BAD_DESCRIPTOR] * 2)
        self.assertEqual(error.__notes__,
                         ["capture directory rollback failed: OSError: rollback fault"])
        self.assertEqual(book.entries, [])
        self.assertTrue((book.directory / "001-rollback-fault").is_dir())
        self.owner.rollback()
        self.assertFalse(book.directory.exists())

    def test_pre_start_consumed_rollback_failure_reports_rollback_and_release(self):
        def consume_then_fail(token, deadline=None):
            token.consume()
            raise OSError("removal fault")
        book, pins, error = self.start_unstarted("removal-fault", patch.object(
            directories, "remove_owned_directory", side_effect=consume_then_fail))
        self.assertEqual([descriptor_errno(pin) for pin in pins], [BAD_DESCRIPTOR] * 2)
        self.assertEqual(len(error.__notes__), 2)
        self.assertRegex(error.__notes__[0], "^capture directory rollback failed: RuntimeError: "
                         "directory rollback failed: process capture: removal fault$")
        self.assertRegex(error.__notes__[1], "^capture directory release failed: RuntimeError: "
                         "directory ownership release failed: process capture: .*already consumed")
        self.assertEqual(book.entries, [])
        self.assertTrue((book.directory / "001-removal-fault").is_dir())

    def start_uncertain(self, name, error, capture=None):
        """Raise inside Popen as if CPython created a child and lost its handle.

        No child exists: the fake records the parent handles it was given and
        raises, as an errpipe read or parent pipe cleanup fault would.
        """
        book = self.book(name, capture)
        handed, pins, adopted = {}, [], []

        def native_start_then_raise(argv, **kwargs):
            handed.update(kwargs)
            raise error
        with self.record_capture_pins(pins), \
                patch.object(processes.subprocess, "Popen", side_effect=native_start_then_raise), \
                self.assertRaises(type(error)) as raised:
            book.start(name, [PYTHON, "-c", "pass"], self.root, on_spawned=adopted.append)
        self.assertEqual(adopted, [])
        return book, handed, pins, raised.exception

    def test_popen_entry_failure_retains_uncertain_capture_without_closure(self):
        for make_error in (lambda: OSError("errpipe fault"), KeyboardInterrupt):
            for captured in (False, True):
                name = "%s-%d" % (type(make_error()).__name__.lower(), captured)
                with self.subTest(case=name):
                    error = make_error()
                    capture = RecordingCapture() if captured else None
                    book, handed, pins, raised = self.start_uncertain(name, error, capture)
                    directory = book.directory / ("001-" + name)
                    record = {"pid": None, "argv": [PYTHON, "-c", "pass"],
                              "directory": str(directory)}
                    # Causal pre-fix failure: rollback deleted this durable invocation.
                    self.assertTrue(directory.is_dir())
                    self.assertEqual(sorted(path.name for path in directory.iterdir()),
                                     ["invocation.json", "stderr", "stdout"])
                    self.assertEqual(read_json(directory / "invocation.json")["argv"],
                                     record["argv"])
                    self.assertIs(raised, error)
                    self.assertEqual(raised.__notes__, [
                        "process launch outcome uncertain; capture retained: " + str(directory)])
                    self.assertEqual(book.entries, [])
                    self.assertEqual(book.pending(), [record])
                    self.assertEqual(book.drain(timeout=0), [record])
                    self.assertFalse(book.finished_naturally())
                    self.assertEqual([descriptor_errno(pin) for pin in pins], [BAD_DESCRIPTOR] * 2)
                    self.assertTrue(handed["stdout"].closed and handed["stderr"].closed)
                    if capture is not None:
                        self.assertIs(handed["stdout"], capture.writer)
                        self.assertTrue(capture.reader.closed)

    def test_next_start_after_uncertain_attempt_cannot_reuse_or_erase_its_capture(self):
        book, _handed, _pins, _raised = self.start_uncertain("probe", OSError("errpipe fault"))
        uncertain = book.directory / "001-probe"
        invocation = (uncertain / "invocation.json").read_bytes()
        process = book.run("probe", [PYTHON, "-c", "pass"], self.root)
        self.assertEqual(process.directory, book.directory / "002-probe")
        self.assertTrue(process.result["natural_wait"])
        self.assertEqual(book.entries, [process])
        self.assertEqual((uncertain / "invocation.json").read_bytes(), invocation)
        self.assertEqual(sorted(path.name for path in uncertain.iterdir()),
                         ["invocation.json", "stderr", "stdout"])
        self.assertEqual(book.pending(), [{"pid": None, "argv": [PYTHON, "-c", "pass"],
                                           "directory": str(uncertain)}])
        self.assertFalse(book.finished_naturally())

    def test_uncertain_observations_are_detached_from_registry_facts(self):
        book, _handed, _pins, _raised = self.start_uncertain("detached", OSError("errpipe fault"))
        directory = book.directory / "001-detached"
        self.assertTrue((directory / "invocation.json").is_file())
        expected = {"pid": None, "argv": [PYTHON, "-c", "pass"], "directory": str(directory)}
        for observe in (book.pending, lambda: book.drain(timeout=0)):
            observed = observe()
            observed[0]["argv"].append("--reader-mutation")
            observed[0]["directory"] = "elsewhere"
            observed.clear()
        self.assertEqual(book.pending(), [expected])
        self.assertEqual(book.drain(timeout=0), [expected])
        self.assertFalse(book.finished_naturally())

    def test_uncertain_start_keeps_primary_error_and_reports_cleanup_faults(self):
        capture = RecordingCapture(close_failures=1)
        with patch.object(processes.DirectoryOwner, "release", side_effect=OSError("release fault")):
            book, _handed, _pins, raised = self.start_uncertain(
                "faults", OSError("errpipe fault"), capture)
        directory = book.directory / "001-faults"
        self.assertTrue((directory / "invocation.json").is_file())
        self.assertEqual(str(raised), "errpipe fault")
        self.assertEqual(raised.__notes__, [
            "stdout capture close failed: OSError: capture close fault",
            "capture directory release failed: OSError: release fault",
            "process launch outcome uncertain; capture retained: " + str(directory)])
        self.assertEqual(len(book.pending()), 1)
        self.assertFalse(book.finished_naturally())

    def test_uncertain_start_registration_fault_keeps_primary_error_and_capture(self):
        book = self.book("registration")
        child = book.directory / "001-registration"
        primary = OSError("errpipe fault")

        def refuse(process):
            raise RuntimeError("registration fault")
        with patch.object(processes.subprocess, "Popen", side_effect=primary), \
                self.assertRaises(OSError) as raised:
            processes.Process(child, [PYTHON, "-c", "pass"], self.root, {}, book.capture_root,
                              failure_type=ValueError, on_start_uncertain=refuse)
        self.assertTrue((child / "invocation.json").is_file())
        self.assertIs(raised.exception, primary)
        self.assertEqual(primary.__notes__, [
            "uncertain start registration failed: RuntimeError: registration fault",
            "process launch outcome uncertain; capture retained: " + str(child)])

    def test_invalid_capture_roots_refuse_before_spawn_or_capture(self):
        book = self.book("roots")
        other = DirectoryOwner()
        released = other.create("released root", self.root / "released", self.root)
        other.release()
        cases = (
            ("wrong-parent", book.capture_root, self.root / "wrong-parent-child",
             "not a direct child of its capture root"),
            ("released", released, released.directory / "child", "already consumed"),
        )
        for case, capture_root, child, message in cases:
            with self.subTest(case=case), patch.object(processes.subprocess, "Popen") as popen:
                with self.assertRaisesRegex(RuntimeError, message):
                    processes.Process(child, [PYTHON, "-c", "pass"], self.root, {}, capture_root)
                popen.assert_not_called()
                self.assertFalse(child.exists())

    def test_natural_completion_sends_finite_stdin_and_records_exact_wait(self):
        book = self.book("natural")
        payload = b"finite input\n" * 4096
        process = book.run("echo", [PYTHON, "-c", ECHO_CHILD], self.root, stdin=payload)
        self.assertEqual(process.output(), payload)
        self.assertEqual(process.result["exit_code"], 0)
        self.assertTrue(process.result["natural_wait"])
        self.assertEqual(process.result["stdin_bytes_sent"], len(payload))
        self.assertTrue(process.result["stdin_complete"])
        self.assertIsNone(process.result["stdin_error"])
        self.assertEqual(read_json(process.directory / "completed.json"), process.result)
        self.assertTrue(process.stdout.closed and process.stderr.closed)
        self.assertEqual(process.poll(), process.result)


class ProcessFinalizationRetryTests(unittest.TestCase):
    expected_stdout = b'{"tasks": {}, "stdin": 0}\n'

    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="process-finalization-")
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name).resolve()
        self.owner = DirectoryOwner()
        self.addCleanup(self.owner.rollback)

    def start(self, name, capture):
        self.addCleanup(close_capture, capture)
        book = processes.Processes(self.root / name, {}, owner=self.owner,
                                   stdout_capture_factory=lambda argv: capture)
        self.addCleanup(settle_children, book)
        return book.start(name, [PYTHON, "-c", STATUS_CHILD], self.root)

    def fault(self, stage, process, capture):
        if stage == "capture-close":
            capture.close_failures = 1
            return nullcontext()
        if stage in ("stdout-close", "stderr-close"):
            name = stage.split("-")[0]
            setattr(process, name, CloseFault(getattr(process, name)))
            return nullcontext()
        if stage == "write":
            target = process.stdout.fileno()
            original = os.write
            calls = []

            def partial_then_fault(descriptor, data):
                if descriptor == target:
                    calls.append(len(data))
                    if len(calls) == 1:
                        return original(descriptor, bytes(data[:3]))
                    if len(calls) == 2:
                        raise OSError("write fault")
                return original(descriptor, data)
            return patch.object(processes.os, "write", side_effect=partial_then_fault)
        if stage == "digest":
            return patch.object(processes, "digest", side_effect=fail_once(
                processes.digest, lambda path: Path(path).name == "stdout",
                OSError("digest fault")))
        if stage == "stat":
            return patch.object(processes.Path, "stat", new=fail_once(
                Path.stat, lambda path, *rest: path == process.stdout_path,
                OSError("stat fault")))
        return patch.object(processes, "write_json", side_effect=fail_once(
            processes.write_json, lambda path, *rest: Path(path).name == "completed.json",
            OSError("receipt fault")))

    def test_failed_finalization_retries_on_the_same_observation(self):
        stages = ("capture-close", "stdout-close", "stderr-close", "write", "digest",
                  "stat", "receipt")
        for stage in stages:
            with self.subTest(stage=stage):
                capture = RecordingCapture()
                process = self.start(stage, capture)
                with self.fault(stage, process, capture):
                    if stage in ("capture-close", "stdout-close", "stderr-close"):
                        with self.assertRaisesRegex(OSError, "close fault"):
                            process.wait(10)
                    else:
                        with self.assertRaisesRegex(OSError, stage + " fault"):
                            process.wait(10)
                    failed_at = time.time_ns()
                    self.assertEqual(process.wait(10), 0)
                self.assertEqual(capture.sanitized_calls, 1)
                self.assertEqual(process.stdout_path.read_bytes(), self.expected_stdout)
                self.assertEqual(process.stderr_path.read_bytes(), b"finite stderr\n")
                self.assertLessEqual(process.result["ended_ns"], failed_at)
                self.assertEqual(process.result["stdout_bytes"], len(self.expected_stdout))
                self.assertTrue(process.stdout.closed and process.stderr.closed)
                self.assertTrue(capture.reader.closed)
                self.assertEqual(read_json(process.directory / "completed.json"), process.result)

    def test_both_stream_close_faults_are_attempted_in_one_finalization(self):
        capture = RecordingCapture()
        process = self.start("both-close", capture)
        process.stdout = CloseFault(process.stdout)
        process.stderr = CloseFault(process.stderr)
        with self.assertRaisesRegex(OSError, "stream close fault") as raised:
            process.wait(10)
        self.assertEqual((process.stdout.failures, process.stderr.failures), (0, 0))
        self.assertEqual(raised.exception.__notes__,
                         ["stderr close failed: OSError: stream close fault"])
        self.assertIsNone(process.result)
        self.assertEqual(process.wait(10), 0)
        self.assertEqual(capture.sanitized_calls, 1)
        self.assertEqual(process.stdout_path.read_bytes(), self.expected_stdout)
        self.assertEqual(process.stderr_path.read_bytes(), b"finite stderr\n")
        self.assertTrue(process.stdout.closed and process.stderr.closed)
        self.assertEqual(read_json(process.directory / "completed.json"), process.result)

    def test_closed_stdout_before_commit_is_an_explicit_failure(self):
        capture = RecordingCapture()
        process = self.start("closed", capture)
        process.stdout.close()
        with self.assertRaisesRegex(RuntimeError, "stdout closed before sanitized capture"):
            process.wait(10)
        self.assertIsNone(process.result)
        self.assertEqual(process.stdout_path.read_bytes(), b"")


if __name__ == "__main__":
    unittest.main()
