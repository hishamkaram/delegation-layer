"""The candidate loader reads real ZIP containers within a per-read budget and keeps partial files."""

from collections.abc import Callable
import errno
import io
import os
from pathlib import Path
import stat
import struct
import sys
import unittest
from unittest.mock import patch
import warnings
import zipfile

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.release_verification import candidate_artifact as candidate_artifact
from scripts.release_verification import filesystem as filesystem
from scripts.release_verification.candidate_manifest import MANIFEST_PATH
from scripts.release_verification.candidate_manifest import PAYLOAD_MEMBER_COUNT
from scripts.release_verification.candidate_manifest import parse_manifest
from scripts.release_verification.candidate_manifest import payload_paths
from scripts.test_candidate_artifact_support import (
    ARCHIVE_PATHS, MIB, VERSION, CandidateArtifactCase, Member, Served, manifest_bytes, members,
    payload_files,
)


# The loader's single-read budget, pinned by the central-directory pair below.
READ_BUDGET = 2 * MIB


FORMULA = "delegation-layer.rb"


def entries_with(index_name: str, member: Member, files=None) -> tuple[list[Member], dict[str, bytes]]:
    """The valid payload with one member replaced."""
    files = payload_files() if files is None else files
    entries = members(files, manifest_bytes(files))
    return [member if entry.name == index_name else entry for entry in entries], files


def empty_entries(count: int) -> list[Member]:
    return [Member(f"n{index:05d}", b"", zipfile.ZIP_STORED) for index in range(count)]


def _central_offset(data: bytes, start: int, name: str) -> int:
    offset, encoded = start, name.encode("ascii")
    while data[offset:offset + 4] == b"PK\x01\x02":
        name_length, extra_length, comment_length = struct.unpack_from("<3H", data, offset + 28)
        if data[offset + 46:offset + 46 + name_length] == encoded:
            return offset
        offset += 46 + name_length + extra_length + comment_length
    raise AssertionError(f"no central record for {name}")


def invalid_utf8_name(data: bytes, name: str, *, central: bool, flag: bool = True) -> bytes:
    """Make one header's copy of ``name`` invalid UTF-8, optionally flagged as UTF-8 (bit 11)."""
    with zipfile.ZipFile(io.BytesIO(data)) as bundle:
        local = bundle.getinfo(name).header_offset
        offset = _central_offset(data, bundle.start_dir, name) if central else local
    flags_at, name_at = (offset + 8, offset + 46) if central else (offset + 6, offset + 30)
    patched = bytearray(data)
    if flag:
        flags = int.from_bytes(patched[flags_at:flags_at + 2], "little") | 0x800
        patched[flags_at:flags_at + 2] = flags.to_bytes(2, "little")
    patched[name_at] = 0xFF
    return bytes(patched)


def central_change(name: str, change: Callable[[zipfile.ZipInfo], None]):
    """A ``central`` hook that alters one member's central record only; its local header is unchanged."""
    def apply(infos: list[zipfile.ZipInfo]) -> None:
        change(next(info for info in infos if info.filename == name))
    return apply


def patched_data(info: zipfile.ZipInfo) -> None:
    info.flag_bits |= 0x20


def unsupported_version(info: zipfile.ZipInfo) -> None:
    info.extract_version = zipfile.MAX_EXTRACT_VERSION + 1


class ProducerFormatTests(CandidateArtifactCase):
    def test_stored_deflated_comments_and_data_descriptors_load_exactly(self):
        files = payload_files()
        manifest = manifest_bytes(files)
        compression = {name: zipfile.ZIP_STORED for name in sorted(files)[::2]}
        entries = [Member(entry.name, entry.data, entry.compression, comment=b"member comment")
                   for entry in members(files, manifest, **compression)]
        served = self.serve(entries, files=files, manifest=manifest, comment=b"archive comment")
        with zipfile.ZipFile(served.container) as bundle:
            infos = bundle.infolist()
        self.assertEqual({info.compress_type for info in infos}, {zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED})
        self.assertEqual({info.flag_bits & 0x08 for info in infos}, {0x08})
        loaded = self.load(served.locator)
        for name, data in files.items():
            self.assertEqual((loaded.payload / name).read_bytes(), data)

    def test_large_stored_and_compressed_members_stream_within_the_read_budget(self):
        stored = (bytes(range(256)) * (3 * MIB // 256)) + b"tail"
        files = payload_files(**{ARCHIVE_PATHS[0]: stored, ARCHIVE_PATHS[1]: bytes(8 * MIB)})
        manifest = manifest_bytes(files)
        served = self.serve(members(files, manifest, **{ARCHIVE_PATHS[0]: zipfile.ZIP_STORED}),
                            files=files, manifest=manifest)
        reads: list[int] = []
        with self.spy_reads(reads):
            loaded = self.load(served.locator)
        self.assertEqual((loaded.payload / ARCHIVE_PATHS[0]).read_bytes(), stored)
        self.assertEqual((loaded.payload / ARCHIVE_PATHS[1]).stat().st_size, 8 * MIB)
        self.assertTrue(reads)
        self.assertLessEqual(max(reads), READ_BUDGET)


class ReadBudgetTests(CandidateArtifactCase):
    def test_budget_covers_every_spec_valid_read_of_the_payload(self):
        central_directory = PAYLOAD_MEMBER_COUNT * (46 + 3 * 65535)
        self.assertGreaterEqual(READ_BUDGET, max(central_directory, 65535 + 22, MIB))

    def test_central_directory_over_budget_is_refused_before_it_is_read(self):
        served = self.serve(empty_entries(42000))
        self.assertGreater(served.container.stat().st_size, READ_BUDGET)
        reads: list[int] = []
        with self.spy_reads(reads):
            error = self.refused("candidate_zip_read_budget_exceeded", served.locator)
        self.assertEqual(error.stage, "load-candidate-artifact")
        self.assertTrue(reads)
        self.assertLessEqual(max(reads), READ_BUDGET)
        self.assertEqual(self.payload_names(), [])
        self.assertTrue((self.root / f"load-{self.loads}" / "container.zip").exists())

    def test_central_directory_within_budget_reaches_the_count_check(self):
        for count in (40000, 3000):
            with self.subTest(count=count):
                served = self.serve(empty_entries(count))
                self.refused("candidate_zip_inventory_mismatch", served.locator)
                self.assertEqual(self.payload_names(), [])


class MemberShapeTests(CandidateArtifactCase):
    def refused_before_writes(self, code: str, entries: list[Member], files: dict[str, bytes], **options):
        served = self.serve(entries, files=files, manifest=manifest_bytes(files), **options)
        self.refused(code, served.locator)
        self.assertEqual(self.payload_names(), [])

    def test_unsafe_or_ambiguous_members_are_refused_before_any_write(self):
        data = payload_files()[FORMULA]
        variants = {
            "slash": Member("nested/" + FORMULA, data), "backslash": Member("nested\\" + FORMULA, data),
            "parent": Member("..", data), "dot": Member(".", data), "directory": Member("nested/", b""),
            "symlink": Member(FORMULA, data, external_attr=(stat.S_IFLNK | 0o777) << 16),
            "device": Member(FORMULA, data, external_attr=(stat.S_IFCHR | 0o644) << 16),
            "bzip2": Member(FORMULA, data, zipfile.ZIP_BZIP2),
            "duplicate": Member("checksums.txt", data),
        }
        for label, member in variants.items():
            with self.subTest(member=label), warnings.catch_warnings():
                warnings.simplefilter("ignore", UserWarning)
                entries, files = entries_with(FORMULA, member)
                self.refused_before_writes("candidate_zip_member_invalid", entries, files)

    def test_encryption_flags_and_changed_names_are_refused_before_any_write(self):
        def central(change):
            def apply(infos):
                change(next(info for info in infos if info.filename == FORMULA))
            return apply

        changes = {
            "encrypted": lambda info: setattr(info, "flag_bits", info.flag_bits | 0x01),
            "strong-encryption": lambda info: setattr(info, "flag_bits", info.flag_bits | 0x40),
            "nul-name": lambda info: setattr(info, "filename", FORMULA + "\x00.txt"),
        }
        files = payload_files()
        for label, change in changes.items():
            with self.subTest(change=label):
                self.refused_before_writes("candidate_zip_member_invalid",
                                           members(files, manifest_bytes(files)), files,
                                           central=central(change))

    def test_member_names_and_count_must_equal_the_manifest_inventory(self):
        files = payload_files()
        valid = members(files, manifest_bytes(files))
        renamed, _ = entries_with(FORMULA, Member("formula.rb", files[FORMULA]))
        unmanifested = [Member("other.json", entry.data) if entry.name == MANIFEST_PATH else entry
                        for entry in valid]
        for label, entries in (("missing", valid[:-1]), ("extra", [*valid, Member("extra", b"x")]),
                               ("renamed", renamed), ("no-manifest", unmanifested)):
            with self.subTest(entries=label):
                self.refused_before_writes("candidate_zip_inventory_mismatch", entries, files)

    def test_an_oversized_manifest_member_is_refused_before_it_is_bound(self):
        entries, files = entries_with(MANIFEST_PATH, Member(MANIFEST_PATH, b" " * (64 * 1024 + 1)))
        self.refused_before_writes("candidate_manifest_too_large", entries, files)


class PartialPayloadTests(CandidateArtifactCase):
    def test_changed_member_bytes_are_refused_and_written_files_are_kept(self):
        files = payload_files()
        same_size = bytes(reversed(files[FORMULA]))
        entries, _ = entries_with(FORMULA, Member(FORMULA, same_size), files)
        served = self.serve(entries, files=files, manifest=manifest_bytes(files))
        self.refused("candidate_payload_member_mismatch", served.locator)
        written = self.payload_names()
        self.assertIn(FORMULA, written)
        self.assertEqual((self.root / f"load-{self.loads}" / "payload" / FORMULA).read_bytes(), same_size)
        self.assertLess(len(written), len(payload_paths(VERSION)))

    def test_a_member_of_another_size_is_refused_without_creating_its_file(self):
        files = payload_files()
        entries, _ = entries_with(FORMULA, Member(FORMULA, files[FORMULA] + b"!"), files)
        served = self.serve(entries, files=files, manifest=manifest_bytes(files))
        self.refused("candidate_payload_member_mismatch", served.locator)
        self.assertNotIn(FORMULA, self.payload_names())
        self.assertIn(MANIFEST_PATH, self.payload_names())

    def test_crc_mismatch_keeps_the_partial_member_and_every_earlier_file(self):
        def corrupt(infos):
            info = next(info for info in infos if info.filename == ARCHIVE_PATHS[0])
            info.CRC ^= 1

        served = self.serve(central=corrupt)
        self.refused("candidate_zip_invalid", served.locator)
        written = self.payload_names()
        self.assertIn(ARCHIVE_PATHS[0], written)
        self.assertNotIn(ARCHIVE_PATHS[1], written)
        self.assertIn(MANIFEST_PATH, written)

    def test_a_payload_write_failure_keeps_its_cause_and_the_partial_file(self):
        def stalled(descriptor: int, data: bytes) -> None:
            with patch.object(os, "write", return_value=0):
                filesystem.write_all(descriptor, data)

        served = self.serve()
        no_space = OSError(errno.ENOSPC, "private-disk-detail")
        for label, writer, code in (("stalled", stalled, errno.EIO), ("no space", no_space, errno.ENOSPC)):
            with self.subTest(fault=label):
                with patch.object(candidate_artifact, "write_all", side_effect=writer):
                    error = self.refused("candidate_payload_write_failed", served.locator)
                self.assertEqual(error.stage, "load-candidate-artifact")
                self.assertIs(type(error.__cause__), OSError)
                assert isinstance(error.__cause__, OSError)
                self.assertEqual(error.__cause__.errno, code)
                self.assertEqual(self.payload_names(), [MANIFEST_PATH])
                self.assertEqual((self.root / f"load-{self.loads}" / "payload" / MANIFEST_PATH).stat().st_size, 0)
                self.assertTrue((self.root / f"load-{self.loads}" / "container.zip").is_file())

    def test_truncated_or_foreign_containers_keep_their_owning_errors(self):
        served = self.serve()
        data = served.container.read_bytes()
        truncated = self.replace_container(served, data[:-30])
        self.refused("candidate_zip_invalid", truncated.locator)
        self.assertEqual(self.payload_names(), [])
        foreign = self.replace_container(truncated, b"<html>not an archive</html>")
        self.refused("github_artifact_header_invalid", foreign.locator)
        self.assertEqual(self.payload_names(), [])



class MalformedHeaderTests(CandidateArtifactCase):
    """Header faults the standard reader raises as UnicodeDecodeError or NotImplementedError.

    Each case first loads the same unaltered producer ZIP, then serves the altered
    bytes with a digest recomputed into the metadata and the locator.
    """

    def setUp(self):
        super().setUp()
        self.valid = self.serve()
        self.load(self.valid.locator)
        self.copy_order = [MANIFEST_PATH, *(entry.path for entry in
                                            parse_manifest(self.valid.manifest).artifacts)]

    def assert_invalid(self, served: Served, cause: type[BaseException]) -> None:
        error = self.refused("candidate_zip_invalid", served.locator)
        self.assertEqual(error.stage, "load-candidate-artifact")
        self.assertIs(type(error.__cause__), cause)

    def assert_partial(self, failing: str) -> None:
        """Every member copied before ``failing`` is kept whole; ``failing`` exists empty."""
        index = self.copy_order.index(failing)
        self.assertEqual(self.payload_names(), sorted(self.copy_order[:index + 1]))
        payload = self.root / f"load-{self.loads}" / "payload"
        for name in self.copy_order[:index]:
            expected = self.valid.manifest if name == MANIFEST_PATH else self.valid.files[name]
            self.assertEqual((payload / name).read_bytes(), expected, name)
        self.assertEqual((payload / failing).stat().st_size, 0)

    def altered(self, data: bytes) -> Served:
        return self.replace_container(self.valid, data)

    def test_an_invalid_utf8_central_name_is_refused_before_any_write(self):
        data = self.valid.container.read_bytes()
        self.assert_invalid(self.altered(invalid_utf8_name(data, FORMULA, central=True)), UnicodeDecodeError)
        self.assertEqual(self.payload_names(), [])

    def test_an_invalid_utf8_local_name_is_refused_and_earlier_files_are_kept(self):
        failing = self.copy_order[2]
        data = self.valid.container.read_bytes()
        self.assert_invalid(self.altered(invalid_utf8_name(data, failing, central=False)), UnicodeDecodeError)
        self.assert_partial(failing)

    def test_the_same_local_name_bytes_without_the_utf8_flag_stay_a_name_mismatch(self):
        failing = self.copy_order[2]
        data = invalid_utf8_name(self.valid.container.read_bytes(), failing, central=False, flag=False)
        self.assert_invalid(self.altered(data), zipfile.BadZipFile)
        self.assert_partial(failing)

    def test_a_central_extract_version_above_the_reader_is_refused_before_any_write(self):
        self.assert_invalid(self.serve(central=central_change(FORMULA, unsupported_version)),
                            NotImplementedError)
        self.assertEqual(self.payload_names(), [])

    def test_central_patched_data_on_the_manifest_is_refused_before_any_write(self):
        self.assert_invalid(self.serve(central=central_change(MANIFEST_PATH, patched_data)),
                            NotImplementedError)
        self.assertEqual(self.payload_names(), [])

    def test_central_patched_data_on_a_later_member_keeps_earlier_files(self):
        failing = self.copy_order[2]
        self.assert_invalid(self.serve(central=central_change(failing, patched_data)), NotImplementedError)
        self.assert_partial(failing)


if __name__ == "__main__":
    unittest.main()
