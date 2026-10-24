"""File identity and byte limits are checked independently of receipt policy."""

import contextlib
import hashlib
import os
from pathlib import Path
import tempfile
import tracemalloc
import unittest
from unittest.mock import patch

from scripts.acceptance_tests.release_fixture import open_descriptors
from scripts.delegate_acceptance.evidence import bounded_files


class BoundedFileTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="bounded-evidence-")
        self.addCleanup(self.temporary.cleanup)
        self.path = Path(self.temporary.name) / "receipt"
        self.path.write_bytes(b"valid")

    def assert_failure(self, reason, path=None, limit=5):
        with self.assertRaises(bounded_files.BoundedReadError) as captured:
            bounded_files.read_bounded_regular_file(path or self.path, limit)
        self.assertEqual(captured.exception.reason, reason)

    def test_regular_file_at_limit_and_empty_file(self):
        self.assertEqual(bounded_files.read_bounded_regular_file(self.path, 5), b"valid")
        self.path.write_bytes(b"")
        self.assertEqual(bounded_files.read_bounded_regular_file(self.path, 0), b"")

    def test_oversized_missing_and_non_regular_files(self):
        self.assert_failure(bounded_files.ReadFailure.TOO_LARGE, limit=4)
        self.assert_failure(bounded_files.ReadFailure.UNREADABLE, self.path.with_name("missing"))
        self.assert_failure(bounded_files.ReadFailure.INVALID, self.path.parent)
        alias = self.path.with_name("alias")
        alias.symlink_to(self.path)
        self.assert_failure(bounded_files.ReadFailure.INVALID, alias)

    def test_replacement_between_observation_and_open_is_rejected(self):
        original_open = os.open
        replacement = self.path.with_name("replacement")
        replacement.write_bytes(b"other")

        def replace_then_open(path, flags):
            replacement.replace(self.path)
            return original_open(path, flags)

        with patch.object(bounded_files.os, "open", side_effect=replace_then_open):
            self.assert_failure(bounded_files.ReadFailure.CHANGED)

    def test_file_growth_during_read_is_rejected(self):
        original_read = os.read
        grown = False

        def grow_after_read(descriptor, length):
            nonlocal grown
            value = original_read(descriptor, length)
            if not grown:
                grown = True
                with self.path.open("ab") as output:
                    output.write(b"x")
            return value

        with patch.object(bounded_files.os, "read", side_effect=grow_after_read):
            self.assert_failure(bounded_files.ReadFailure.TOO_LARGE)


def read_fingerprint(path: Path, limit: int) -> bounded_files.FileFingerprint:
    """The byte-returning entrypoint, reduced to the streaming entrypoint's result type."""
    data = bounded_files.read_bounded_regular_file(path, limit)
    return bounded_files.FileFingerprint(len(data), hashlib.sha256(data).hexdigest())


ENTRYPOINTS = (read_fingerprint, bounded_files.fingerprint_bounded_regular_file)


@contextlib.contextmanager
def without_nofollow():
    """Model a platform whose os module lacks the optional O_NOFOLLOW flag, then restore it."""
    present = hasattr(os, "O_NOFOLLOW")
    saved = getattr(os, "O_NOFOLLOW", None)
    if present:
        del os.O_NOFOLLOW
    try:
        yield
    finally:
        if present:
            os.O_NOFOLLOW = saved


@contextlib.contextmanager
def injected_at_lstat(target: Path, inject):
    """Run inject once, right after the real Path.lstat of target returned its metadata."""
    original, ran = Path.lstat, []

    def lstat(self, *arguments, **options):
        metadata = original(self, *arguments, **options)
        if self == target and not ran:
            ran.append(True)
            inject()
        return metadata

    with patch.object(Path, "lstat", lstat):
        yield ran


class PinnedReaderTests(unittest.TestCase):
    """Both public entrypoints share one pinned read; each is checked against the same cases."""

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="pinned-evidence-")
        self.addCleanup(self.temporary.cleanup)
        self.path = Path(self.temporary.name).resolve(strict=True) / "artifact"
        self.path.write_bytes(b"valid")
        self.valid = bounded_files.FileFingerprint(5, hashlib.sha256(b"valid").hexdigest())

    def assert_failure(self, operation, reason, path=None, limit=5):
        before = open_descriptors()
        with self.assertRaises(bounded_files.BoundedReadError) as captured:
            operation(path or self.path, limit)
        self.assertEqual(captured.exception.reason, reason)
        self.assertEqual(open_descriptors(), before)

    def test_empty_and_exact_limit_files_have_the_same_result_from_both_entrypoints(self):
        empty = self.path.with_name("empty")
        empty.write_bytes(b"")
        for operation in ENTRYPOINTS:
            with self.subTest(operation=operation.__name__):
                before = open_descriptors()
                self.assertEqual(operation(self.path, 5), self.valid)
                self.assertEqual(open_descriptors(), before)
                self.assertEqual(operation(empty, 0), bounded_files.FileFingerprint(
                    0, hashlib.sha256(b"").hexdigest()))
                self.assert_failure(operation, bounded_files.ReadFailure.TOO_LARGE, limit=4)

    def test_invalid_and_missing_paths_fail_with_the_reader_vocabulary(self):
        alias = self.path.with_name("alias")
        alias.symlink_to(self.path)
        for operation in ENTRYPOINTS:
            with self.subTest(operation=operation.__name__):
                self.assert_failure(operation, bounded_files.ReadFailure.INVALID, alias)
                self.assert_failure(operation, bounded_files.ReadFailure.INVALID, self.path.parent)
                self.assert_failure(operation, bounded_files.ReadFailure.UNREADABLE, self.path.with_name("none"))

    def test_growth_and_read_failure_are_rejected_and_close_the_descriptor(self):
        original_read = os.read
        for operation in ENTRYPOINTS:
            with self.subTest(operation=operation.__name__):
                self.path.write_bytes(b"valid")
                grown = []

                def grow_after_read(descriptor, length):
                    value = original_read(descriptor, length)
                    if not grown:
                        grown.append(True)
                        with self.path.open("ab") as output:
                            output.write(b"x")
                    return value

                with patch.object(bounded_files.os, "read", side_effect=grow_after_read):
                    self.assert_failure(operation, bounded_files.ReadFailure.TOO_LARGE)
                self.path.write_bytes(b"valid")
                with patch.object(bounded_files.os, "read", side_effect=OSError("read failed")):
                    self.assert_failure(operation, bounded_files.ReadFailure.UNREADABLE)

    def test_replacement_between_the_observation_and_the_open_is_rejected(self):
        replacement = self.path.with_name("replacement")
        for operation in ENTRYPOINTS:
            with self.subTest(operation=operation.__name__):
                replacement.write_bytes(b"other")
                with injected_at_lstat(self.path, lambda: replacement.replace(self.path)) as ran:
                    self.assert_failure(operation, bounded_files.ReadFailure.CHANGED)
                self.assertEqual(ran, [True])

    def test_missing_nofollow_flag_still_reads_regular_files_and_rejects_a_symlink_swap(self):
        target = self.path.with_name("target")
        target.write_bytes(b"other")
        with without_nofollow():
            for operation in ENTRYPOINTS:
                with self.subTest(operation=operation.__name__):
                    self.path.unlink(missing_ok=True)
                    self.path.write_bytes(b"valid")
                    self.assertEqual(operation(self.path, 5), self.valid)

                    def swap():
                        self.path.unlink()
                        self.path.symlink_to(target)

                    with injected_at_lstat(self.path, swap) as ran:
                        self.assert_failure(operation, bounded_files.ReadFailure.CHANGED)
                    self.assertEqual(ran, [True])

    def test_flag_helper_restores_a_present_flag_and_leaves_an_absent_flag_absent(self):
        flag = os.O_NOFOLLOW
        with without_nofollow():
            self.assertFalse(hasattr(os, "O_NOFOLLOW"))
            with without_nofollow():
                self.assertFalse(hasattr(os, "O_NOFOLLOW"))
            self.assertFalse(hasattr(os, "O_NOFOLLOW"))
        self.assertEqual(os.O_NOFOLLOW, flag)

    def test_nofollow_present_refuses_a_symlink_swap_as_unreadable(self):
        # Current native boundary (Linux and macOS): the optional flag exists and is used.
        self.assertTrue(hasattr(os, "O_NOFOLLOW"))
        target = self.path.with_name("target")
        target.write_bytes(b"other")

        def swap():
            self.path.unlink()
            self.path.symlink_to(target)

        with injected_at_lstat(self.path, swap):
            self.assert_failure(bounded_files.fingerprint_bounded_regular_file,
                                bounded_files.ReadFailure.UNREADABLE)

    def test_fingerprint_of_a_large_file_is_exact_and_memory_stays_near_one_chunk(self):
        chunk = bytes(range(256)) * 4096
        expected, large = hashlib.sha256(), self.path.with_name("large")
        with large.open("wb") as output:
            for _ in range(16):
                output.write(chunk)
                expected.update(chunk)
        limit = 16 * len(chunk)
        tracemalloc.start()
        try:
            tracemalloc.reset_peak()
            result = bounded_files.fingerprint_bounded_regular_file(large, limit)
            peak = tracemalloc.get_traced_memory()[1]
        finally:
            tracemalloc.stop()
        self.assertEqual(result, bounded_files.FileFingerprint(limit, expected.hexdigest()))
        self.assertLess(peak, 4 * 1024 * 1024)


if __name__ == "__main__":
    unittest.main()
