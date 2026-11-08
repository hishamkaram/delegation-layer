"""Hash-only readers stream one pinned read; no whole-file buffer, same validation and error identities.

Fixtures are built and every import is done before ``tracemalloc`` starts, so each
measurement covers only the hash operation. The ceiling is the file's own size: a
reader that holds the file stays above it, a streaming reader stays far below it.
Disk sparseness proves nothing about memory: reading a sparse file still yields
every byte, so a buffering reader is caught however the fixture is stored.
"""

from __future__ import annotations

from collections.abc import Callable
import gc
import hashlib
from pathlib import Path
import sys
import tracemalloc
from typing import TypeVar
import unittest
from unittest.mock import call, patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.release_tests.release_fixture import FixtureTree
from scripts.acceptance.testing.resources import open_descriptors
from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.evidence.bounded_files import FileFingerprint
from scripts.release_verification import candidate_build_artifacts as candidate_build_artifacts
from scripts.release_verification import filesystem as filesystem
from scripts.release_verification.candidate_build_artifacts import BuildContext
from scripts.release_verification.candidate_build_artifacts import observed_files
from scripts.release_verification.candidate_build_plan import BuildRequest
from scripts.release_verification.candidate_manifest import ArtifactKind
from scripts.release_verification.candidate_manifest import entry_limit
from scripts.release_verification.candidate_manifest import expected_inventory
from scripts.acceptance.evidence.source import VerificationError


T = TypeVar("T")
MIB = 1024 * 1024
FILE_BYTES = 8 * MIB
VERSION = "0.1.37"
CODE = "streaming_test_refused"
Reader = Callable[[Path, int, str], object]


def digest_reader(path: Path, limit: int, code: str) -> object:
    return filesystem.bounded_digest(path, limit, code)


def fingerprint_reader(path: Path, limit: int, code: str) -> object:
    return filesystem.bounded_fingerprint(path, limit, code)


READERS: dict[str, Reader] = {"bounded_digest": digest_reader, "bounded_fingerprint": fingerprint_reader}


def peak_allocation(operation: Callable[[], T]) -> tuple[T, int]:
    """Run ``operation`` and return its result with the peak Python allocation it caused."""
    gc.collect()
    tracemalloc.start()
    try:
        result = operation()
        _, peak = tracemalloc.get_traced_memory()
    finally:
        tracemalloc.stop()
    return result, peak


def sparse(path: Path, size: int) -> Path:
    with path.open("xb") as handle:
        handle.truncate(size)
    return path


class StreamingCase(unittest.TestCase):
    def setUp(self) -> None:
        self.tree = FixtureTree.open(prefix="release-streaming-hash", label="release streaming hash fixture")
        self.addCleanup(self.tree.dispose)
        self.root = self.tree.root

    def refusal(self, reader: Reader, path: Path, limit: int, code: str = CODE) -> VerificationError:
        with self.assertRaises(VerificationError) as caught:
            reader(path, limit, code)
        return caught.exception

    def assertBoundedRead(self, error: VerificationError, code: str = CODE) -> None:
        self.assertEqual((error.code, error.stage), (code, "bounded-read"))
        self.assertIsNone(error.__cause__)
        self.assertTrue(error.__suppress_context__)


class MemoryTests(StreamingCase):
    def real_file(self) -> tuple[Path, str]:
        data = bytes(range(256)) * (FILE_BYTES // 256)
        path = self.root / "real.bin"
        path.write_bytes(data)
        return path, hashlib.sha256(data).hexdigest()

    def test_the_digest_of_a_real_file_is_streamed_not_buffered(self) -> None:
        path, expected = self.real_file()
        digest, peak = peak_allocation(lambda: filesystem.bounded_digest(path, FILE_BYTES, CODE))
        self.assertEqual(digest, expected)
        self.assertLess(peak, FILE_BYTES)

    def test_the_fingerprint_of_a_real_file_is_streamed_with_its_exact_size(self) -> None:
        path, expected = self.real_file()
        fingerprint, peak = peak_allocation(lambda: filesystem.bounded_fingerprint(path, FILE_BYTES, CODE))
        self.assertEqual(fingerprint, FileFingerprint(FILE_BYTES, expected))
        self.assertLess(peak, FILE_BYTES)

    def inventory_context(self) -> tuple[BuildContext, dict[str, tuple[int, str]]]:
        output = self.root / "output"
        output.mkdir()
        expected: dict[str, tuple[int, str]] = {}
        inventory = expected_inventory(VERSION)
        large = next(path for path, kind, _os, _arch in inventory if kind is ArtifactKind.ARCHIVE)
        for index, (path, _kind, _os, _arch) in enumerate(inventory):
            data = bytes(FILE_BYTES) if path == large else b"file %d\n" % index
            if path == large:
                sparse(output / path, FILE_BYTES)
            else:
                (output / path).write_bytes(data)
            expected[path] = (len(data), hashlib.sha256(data).hexdigest())
        context = BuildContext(
            request=BuildRequest(version=VERSION, commit="a" * 40, base_commit="c" * 40,
                                 repository="hishamkaram/delegation-layer"),
            receipt={}, environment={}, clone=self.root / "clone", work=self.root / "work",
            output=output, owner=DirectoryOwner(), goreleaser=self.root / "goreleaser")
        return context, expected

    def test_observing_a_full_inventory_streams_its_largest_archive(self) -> None:
        context, expected = self.inventory_context()
        files, peak = peak_allocation(lambda: observed_files(context))
        self.assertEqual(files, expected)
        self.assertLess(peak, FILE_BYTES)

    def test_each_inventory_member_is_fingerprinted_once_under_its_kind_limit(self) -> None:
        context, expected = self.inventory_context()
        with patch.object(candidate_build_artifacts, "bounded_fingerprint",
                          wraps=filesystem.bounded_fingerprint) as spy:
            self.assertEqual(observed_files(context), expected)
        self.assertEqual(spy.call_args_list, [
            call(context.output / path, entry_limit(kind), "candidate_output_unreadable")
            for path, kind, _os, _arch in expected_inventory(VERSION)])


class ValueTests(StreamingCase):
    def test_empty_exact_limit_and_chunk_boundary_files_give_their_exact_values(self) -> None:
        for size in (0, 1, MIB - 1, MIB, MIB + 1):
            data = bytes(range(251)) * (size // 251 + 1)
            data = data[:size]
            path = self.root / f"size-{size}.bin"
            path.write_bytes(data)
            expected = hashlib.sha256(data).hexdigest()
            with self.subTest(size=size):
                self.assertEqual(filesystem.bounded_digest(path, size, CODE), expected)
                self.assertEqual(filesystem.bounded_fingerprint(path, size, CODE),
                                 FileFingerprint(size, expected))
                if size:
                    for name, reader in READERS.items():
                        with self.subTest(reader=name):
                            self.assertBoundedRead(self.refusal(reader, path, size - 1))

    def test_the_digest_default_code_is_unchanged(self) -> None:
        path = self.root / "over.bin"
        path.write_bytes(b"12")
        with self.assertRaises(VerificationError) as caught:
            filesystem.bounded_digest(path, 1)
        self.assertBoundedRead(caught.exception, "file_too_large")


class RefusalTests(StreamingCase):
    def test_unreadable_paths_refuse_in_bounded_read_with_the_callers_code(self) -> None:
        directory = self.root / "directory"
        directory.mkdir()
        for name, reader in READERS.items():
            with self.subTest(reader=name, case="missing"):
                self.assertBoundedRead(self.refusal(reader, self.root / "missing.bin", 1))
            with self.subTest(reader=name, case="not a regular file"):
                self.assertBoundedRead(self.refusal(reader, directory, 1))

    def test_links_and_parent_components_refuse_in_path_validation(self) -> None:
        real = self.root / "real"
        real.mkdir()
        target = real / "file.bin"
        target.write_bytes(b"data")
        (self.root / "final.bin").symlink_to(target)
        (self.root / "ancestor").symlink_to(real, target_is_directory=True)
        cases = {"final link": self.root / "final.bin", "ancestor link": self.root / "ancestor" / "file.bin",
                 "parent component": real / ".." / "real" / "file.bin"}
        for name, reader in READERS.items():
            for label, path in cases.items():
                with self.subTest(reader=name, case=label):
                    error = self.refusal(reader, path, 1 << 20)
                    self.assertEqual((error.code, error.stage), (CODE, "validate-paths"))

    def test_a_read_failure_after_the_open_refuses_and_closes_the_descriptor(self) -> None:
        path = self.root / "readable.bin"
        path.write_bytes(b"data")
        for name, reader in READERS.items():
            with self.subTest(reader=name):
                before = open_descriptors()
                with patch("os.read", side_effect=OSError(5, "injected read failure")):
                    error = self.refusal(reader, path, 1 << 20)
                self.assertBoundedRead(error)
                self.assertEqual(open_descriptors(), before)

    def test_descriptors_are_closed_after_every_outcome(self) -> None:
        good, bad = self.root / "good.bin", self.root / "bad.bin"
        good.write_bytes(b"data")
        bad.write_bytes(b"12345")
        before = open_descriptors()
        for name, reader in READERS.items():
            with self.subTest(reader=name):
                reader(good, 4, CODE)
                self.refusal(reader, bad, 4)
                self.refusal(reader, self.root / "missing.bin", 4)
        self.assertEqual(open_descriptors(), before)


if __name__ == "__main__":
    unittest.main()
