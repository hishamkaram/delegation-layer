"""The raw descriptor write loop and the one new-file write lifetime built on it.

``write_all`` completes partial writes, refuses a stall and keeps raw OS errors.
``write_new_file`` creates, writes, syncs and closes exactly once, keeping a
primary failure's identity and adding only an errno note for a close fault.
"""

from __future__ import annotations

import contextlib
import errno
import os
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests.release_fixture import FixtureTree, open_descriptors
from scripts.delegate_acceptance.release_verification import filesystem
from scripts.delegate_acceptance.release_verification.contract import VerificationError


DATA = bytes(range(256)) * 4 + b"tail"
REAL_WRITE = os.write


class WriteAllTests(unittest.TestCase):
    def setUp(self) -> None:
        descriptors = open_descriptors()
        self.addCleanup(lambda: self.assertEqual(open_descriptors(), descriptors))
        self.tree = FixtureTree.open(prefix="release-write-all", label="release write-all fixture")
        self.addCleanup(self.tree.dispose)
        self.path = self.tree.root / "target.bin"
        self.descriptor = os.open(self.path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        self.addCleanup(os.close, self.descriptor)
        self.calls: list[int] = []

    def short_writes(self, size: int, *, then: int | None = None):
        """Write at most ``size`` bytes per call; after the first call, return ``then`` if given."""
        def write(descriptor: int, data: memoryview) -> int:
            self.calls.append(len(data))
            if then is not None and len(self.calls) > 1:
                return then
            return REAL_WRITE(descriptor, data[:size])
        return patch.object(os, "write", side_effect=write)

    def test_partial_writes_continue_until_every_byte_is_written(self) -> None:
        with self.short_writes(100):
            filesystem.write_all(self.descriptor, DATA)
        self.assertEqual(self.path.read_bytes(), DATA)
        self.assertEqual(self.calls, list(range(len(DATA), 0, -100)))

    def test_empty_data_makes_no_write(self) -> None:
        with self.short_writes(100):
            filesystem.write_all(self.descriptor, b"")
        self.assertEqual((self.calls, self.path.read_bytes()), ([], b""))

    def test_zero_or_negative_progress_is_a_raw_eio_error(self) -> None:
        for progress in (0, -1):
            with self.subTest(progress=progress), self.short_writes(10, then=progress):
                with self.assertRaises(OSError) as caught:
                    filesystem.write_all(self.descriptor, DATA)
            self.assertIs(type(caught.exception), OSError)
            self.assertEqual((caught.exception.errno, caught.exception.strerror),
                             (errno.EIO, "write made no progress"))
            self.calls.clear()
        self.assertEqual(self.path.read_bytes(), DATA[:10] * 2)

    def test_an_os_error_from_the_write_propagates_unchanged(self) -> None:
        fault = OSError(errno.ENOSPC, "disk full")
        with patch.object(os, "write", side_effect=fault):
            with self.assertRaises(OSError) as caught:
                filesystem.write_all(self.descriptor, DATA)
        self.assertIs(caught.exception, fault)
        self.assertEqual(self.path.read_bytes(), b"")


class WriteNewFileTests(unittest.TestCase):
    """Faults are injected at the filesystem owner's own seams; the close fault really closes first."""

    def setUp(self) -> None:
        descriptors = open_descriptors()
        self.addCleanup(lambda: self.assertEqual(open_descriptors(), descriptors))
        self.tree = FixtureTree.open(prefix="release-write-new", label="release write-new fixture")
        self.addCleanup(self.tree.dispose)
        self.path = self.tree.root / "target.bin"

    def write(self) -> None:
        filesystem.write_new_file(self.path, DATA, path_code="fixture_path_invalid",
                                  write_code="fixture_write_failed", stage="fixture-stage")

    @contextlib.contextmanager
    def closes(self, *, fail: bool):
        """Count each close of the helper's own descriptor before really closing it; optionally fail after."""
        real_create, real_close, created, attempts = filesystem.create_new_file, os.close, [], []

        def create(path, code, *, stage):
            created.append(real_create(path, code, stage=stage))
            return created[-1]

        def close(descriptor):
            if descriptor in created:
                attempts.append(descriptor)
            real_close(descriptor)
            if fail and descriptor in created:
                raise OSError(errno.EIO, "private-close-detail")

        with patch.object(filesystem, "create_new_file", side_effect=create), \
                patch.object(filesystem.os, "close", side_effect=close):
            yield attempts

    def assertClosedOnce(self, attempts: list[int]) -> None:
        self.assertEqual(len(attempts), 1)
        with self.assertRaises(OSError) as closed:
            os.fstat(attempts[0])
        self.assertEqual(closed.exception.errno, errno.EBADF)

    def assertMapped(self, error: BaseException) -> None:
        self.assertIs(type(error), VerificationError)
        self.assertEqual((error.code, error.stage), ("fixture_write_failed", "fixture-stage"))
        self.assertIsNone(error.__cause__)
        self.assertTrue(error.__suppress_context__)

    def test_exact_bytes_are_written_owner_only_and_closed_once(self) -> None:
        with self.closes(fail=False) as attempts:
            self.write()
        self.assertClosedOnce(attempts)
        self.assertEqual(self.path.read_bytes(), DATA)
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)

    def test_an_existing_path_refuses_with_the_path_code_and_keeps_its_bytes(self) -> None:
        self.path.write_bytes(b"prior")
        with self.assertRaises(VerificationError) as caught:
            self.write()
        self.assertEqual((caught.exception.code, caught.exception.stage), ("fixture_path_invalid", "fixture-stage"))
        self.assertEqual(self.path.read_bytes(), b"prior")

    def test_write_or_sync_faults_map_to_the_write_code_and_close_once(self) -> None:
        faults = {"write": patch.object(filesystem, "write_all", side_effect=OSError(errno.ENOSPC, "private")),
                  "sync": patch.object(filesystem.os, "fsync", side_effect=OSError(errno.EINVAL, "private"))}
        for name, fault in faults.items():
            for fail_close in (False, True):
                with self.subTest(fault=name, close_fails=fail_close):
                    self.path.unlink(missing_ok=True)
                    with self.closes(fail=fail_close) as attempts, fault, self.assertRaises(BaseException) as caught:
                        self.write()
                    self.assertMapped(caught.exception)
                    self.assertClosedOnce(attempts)
                    self.assertEqual(getattr(caught.exception, "__notes__", []),
                                     ["file close failed: EIO"] if fail_close else [])
                    self.assertTrue(self.path.is_file())

    def test_a_close_only_fault_is_the_write_code_after_one_real_close(self) -> None:
        with self.closes(fail=True) as attempts, self.assertRaises(BaseException) as caught:
            self.write()
        self.assertMapped(caught.exception)
        self.assertFalse(hasattr(caught.exception, "__notes__"))
        self.assertClosedOnce(attempts)
        self.assertEqual(self.path.read_bytes(), DATA)

    def test_an_interrupt_keeps_its_identity_and_closes_once(self) -> None:
        for fail_close in (False, True):
            with self.subTest(close_fails=fail_close):
                self.path.unlink(missing_ok=True)
                interrupt = KeyboardInterrupt()
                with self.closes(fail=fail_close) as attempts, \
                        patch.object(filesystem, "write_all", side_effect=interrupt), \
                        self.assertRaises(KeyboardInterrupt) as caught:
                    self.write()
                self.assertIs(caught.exception, interrupt)
                self.assertClosedOnce(attempts)
                self.assertEqual(getattr(interrupt, "__notes__", []),
                                 ["file close failed: EIO"] if fail_close else [])


if __name__ == "__main__":
    unittest.main()
