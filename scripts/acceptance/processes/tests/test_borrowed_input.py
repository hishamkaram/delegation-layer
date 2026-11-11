"""Caller-owned pipe input through the existing Start/Wait registry."""

import fcntl
import os
from pathlib import Path
import pty
import socket
import sys
import tempfile
import time
import unittest
from unittest.mock import Mock, patch

from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.evidence.files import read_json
from scripts.acceptance.processes.contracts import BorrowedInput, TerminalIO
from scripts.acceptance.processes import operations
from scripts.acceptance.provider_drivers.agy.process_capture import StatusCapture


PYTHON = str(Path(sys.executable).resolve())
# Every read has a finite escape, even if an assertion fails before caller EOF.
STREAM_CHILD = '''import os, select, sys
sys.stdout.write('{"tasks": {}, "acks": ['); sys.stdout.flush()
for index in range(2):
    if not select.select([0], [], [], 3)[0]: sys.exit(9)
    if os.read(0, 1) != b'x': sys.exit(8)
    sys.stdout.write(('' if index == 0 else ',') + str(index)); sys.stdout.flush()
if not select.select([0], [], [], 3)[0]: sys.exit(9)
if os.read(0, 1) != b'': sys.exit(8)
sys.stdout.write(']}'); sys.stdout.flush()
'''
EOF_CHILD = '''import os, select, sys
if not select.select([0], [], [], 3)[0]: sys.exit(9)
sys.exit(0 if os.read(0, 1) == b'' else 8)
'''


class BorrowedInputTests(unittest.TestCase):
    def setUp(self):
        self.owner = DirectoryOwner()
        self.root = Path(tempfile.mkdtemp(prefix="borrowed-input-")).resolve()
        self.owner.adopt_created("borrowed input test", self.root, self.root.parent)
        self.books = []
        self.descriptors = set()
        self.captures = []
        self.addCleanup(self.cleanup)

    def cleanup(self):
        # Close caller descriptors before the owner's Wait/drain; no test signals.
        for descriptor in tuple(self.descriptors):
            self.close_fd(descriptor)
        unresolved = []
        for book in self.books:
            unresolved.extend(book.drain(timeout=5))
        for capture in self.captures:
            capture.close()
        if unresolved:
            self.owner.release()
            raise RuntimeError("child closure uncertain; retained " + str(self.root))
        self.owner.rollback()

    def close_fd(self, descriptor):
        os.close(descriptor)
        self.descriptors.remove(descriptor)

    def pipe(self):
        reader, writer = os.pipe()
        self.descriptors.update((reader, writer))
        return reader, writer

    def book(self, factory=None):
        book = operations.Processes(self.root / str(len(self.books)), environment={},
                                    owner=self.owner, stdout_capture_factory=factory)
        self.books.append(book)
        return book

    def start(self, book, reader, source=EOF_CHILD, **kwargs):
        return book.start("child", [PYTHON, "-c", source], self.root,
                          borrowed_input=BorrowedInput(input_fd=reader), **kwargs)

    def assert_borrowed_record(self, record, identity):
        self.assertEqual(record["stdin"], "BORROWED_PIPE")
        self.assertEqual(record["stdin_pipe"], identity)
        for key in ("stdin_bytes", "stdin_sha256", "stdin_bytes_sent", "stdin_complete", "stdin_error"):
            self.assertNotIn(key, record)

    def test_invalid_or_mixed_input_is_rejected_before_capture_or_start(self):
        reader, writer = self.pipe()
        ordinary = self.enterContext((self.root / "ordinary").open("w+b"))
        left, right = socket.socketpair()
        self.enterContext(left)
        self.enterContext(right)
        fifo = self.root / "fifo"
        os.mkfifo(fifo)
        self.addCleanup(fifo.unlink)
        duplex = os.open(fifo, os.O_RDWR | os.O_NONBLOCK)
        self.descriptors.add(duplex)
        master, slave = pty.openpty()
        self.descriptors.update((master, slave))
        factory = Mock()
        book = self.book(factory)
        closed = os.dup(reader)
        os.close(closed)
        cases = [(value, {}) for value in (True, -1, 0, 1, 2, closed, writer,
                                           ordinary.fileno(), left.fileno(), duplex, slave)]
        cases += [(reader, {"stdin": b""}), (reader, {"terminal": TerminalIO(reader, writer)})]
        with patch.object(operations.subprocess, "Popen") as launch:
            for descriptor, options in cases:
                with self.subTest(descriptor=descriptor, options=options), self.assertRaises(ValueError):
                    self.start(book, descriptor, **options)
            # Direct construction must also validate before creating its capture.
            with self.assertRaises(ValueError):
                operations.Process(book.directory / "direct", [PYTHON], self.root, {}, book.capture_root,
                                   borrowed_input=BorrowedInput(input_fd=writer))
        factory.assert_not_called()
        launch.assert_not_called()
        self.assertEqual(list(book.directory.iterdir()), [])
        self.assertEqual(book.entries, [])

    def test_two_way_capture_and_caller_eof_preserve_descriptor_and_exact_wait(self):
        reader, writer = self.pipe()
        os.set_blocking(reader, False)
        identity_stat = os.fstat(reader)
        identity = {"device": identity_stat.st_dev, "inode": identity_stat.st_ino}
        flags = fcntl.fcntl(reader, fcntl.F_GETFL)
        os.set_blocking(writer, False)
        self.assertFalse(os.get_inheritable(writer))
        capture = StatusCapture()
        self.captures.append(capture)
        book = self.book(lambda argv: capture)
        child = self.start(book, reader, STREAM_CHILD)
        self.assertEqual(os.fstat(reader), identity_stat)
        self.assertEqual(fcntl.fcntl(reader, fcntl.F_GETFL), flags)
        self.assertIs(book.entries[0], child)
        self.close_fd(reader)
        for expected in (b'[0', b'[0,1'):
            os.write(writer, b'x')
            deadline = time.monotonic() + 2
            while expected not in capture.buffer and time.monotonic() < deadline:
                self.assertIsNone(child.poll())
                time.sleep(0.005)
            self.assertIn(expected, capture.buffer)
        self.assertIsNone(child.poll(), "EOF belongs to caller writer closure")
        with self.assertRaisesRegex(RuntimeError, "observation expired"):
            child.wait(timeout=0)
        self.assertIsNone(child.result)
        self.assert_borrowed_record(read_json(child.directory / "observation-expired.json"), identity)
        self.close_fd(writer)
        with patch.object(child.process, "wait", wraps=child.process.wait) as exact_wait:
            self.assertEqual(child.wait(timeout=5), 0)
            self.assertEqual(child.wait(timeout=5), 0)
        exact_wait.assert_called_once_with()
        self.assertEqual(child.json(), {"tasks": {}, "acks": [0, 1]})
        for name in ("invocation.json", "completed.json"):
            self.assert_borrowed_record(read_json(child.directory / name), identity)
        self.assertTrue(book.finished_naturally())

    def test_publication_failure_keeps_registered_owner_and_caller_descriptor(self):
        original = operations.write_json

        def fail_publication(path, value):
            if path.name == "started.json":
                raise OSError("publication fault")
            original(path, value)

        def fail_callback(process):
            self.assertIs(book.entries[-1], process)
            raise OSError("publication fault")

        for callback in (None, fail_callback):
            with self.subTest(callback=callback):
                reader, writer = self.pipe()
                flags = fcntl.fcntl(reader, fcntl.F_GETFL)
                book = self.book()
                with patch.object(operations, "write_json", side_effect=fail_publication):
                    with self.assertRaisesRegex(OSError, "publication fault"):
                        self.start(book, reader, on_spawned=callback)
                self.assertEqual(len(book.entries), 1)
                self.assertEqual(fcntl.fcntl(reader, fcntl.F_GETFL), flags)
                self.close_fd(reader)
                self.close_fd(writer)
                self.assertEqual(book.entries[0].wait(timeout=5), 0)
                self.assertTrue(book.finished_naturally())

    def test_uncertain_start_retains_receipt_without_wait_or_descriptor_mutation(self):
        reader, writer = self.pipe()
        flags = fcntl.fcntl(reader, fcntl.F_GETFL)
        book = self.book()
        with patch.object(operations.subprocess, "Popen", side_effect=OSError("uncertain start")):
            with self.assertRaisesRegex(OSError, "uncertain start"):
                self.start(book, reader)
        self.assertEqual(fcntl.fcntl(reader, fcntl.F_GETFL), flags)
        self.assertEqual(book.entries, [])
        pending = book.pending()
        self.assertEqual(len(pending), 1)
        self.assertIsNone(pending[0]["pid"])
        directory = Path(pending[0]["directory"])
        self.assertEqual(read_json(directory / "invocation.json")["stdin"], "BORROWED_PIPE")
        self.assertFalse((directory / "completed.json").exists())
        self.assertFalse(book.finished_naturally())
        # Injected failure creates no actual child; preserve its uncertainty record
        # rather than asking cleanup to invent process closure.
        self.books.remove(book)

    def test_exited_child_does_not_leave_a_hidden_borrowed_reader(self):
        reader, writer = self.pipe()
        book = self.book()
        child = self.start(book, reader, "pass")
        self.close_fd(reader)
        self.assertEqual(child.wait(timeout=5), 0)
        with self.assertRaises(BrokenPipeError):
            os.write(writer, b'x')


if __name__ == "__main__":
    unittest.main()
