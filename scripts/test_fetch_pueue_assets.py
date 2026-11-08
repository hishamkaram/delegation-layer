"""Offline archive installer tests with finite source-authored behavior probes."""

from __future__ import annotations

from collections.abc import Generator
from contextlib import contextmanager
import errno
import gzip
import hashlib
from http.client import HTTPMessage
import io
import json
import os
import signal
import stat
import subprocess
from pathlib import Path
import tarfile
import tempfile
from typing import IO
import unittest
import urllib.error
from unittest.mock import patch

from scripts import fetch_pueue_assets as assets


PAYLOADS = {
    "pueue": b'#!/bin/sh\n[ "${1-}" = --version ] || exit 64\nprintf "%s\\n" "pueue 4.0.4"\n',
    "pueued": b'#!/bin/sh\n[ "${1-}" = --version ] || exit 64\nprintf "%s\\n" "pueued 4.0.4"\n',
    "delegate": b"old delegate MUST NOT be installed", "delegate-run": b"old runner MUST NOT be installed",
}
KEY = ("linux", "amd64")


def member(name: str, data: bytes, *, kind: bytes = tarfile.REGTYPE,
           mode: int = 0o755, size: int | None = None) -> bytes:
    header = tarfile.TarInfo(name)
    header.size = len(data) if size is None else size
    header.mode, header.type = mode, kind
    if kind in {tarfile.SYMTYPE, tarfile.LNKTYPE}:
        header.linkname = "pueue"
    return header.tobuf(format=tarfile.USTAR_FORMAT) + data + bytes((-len(data)) % 512)


def archive(entries: list[bytes] | None = None, *, tail: bytes = bytes(1024)) -> bytes:
    if entries is None:
        entries = [member(name, data) for name, data in PAYLOADS.items()]
    return gzip.compress(b"".join(entries) + tail, mtime=0)


class ArchiveInstallerTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="pueue-archive-unit-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.output = self.root / "output"

    @contextmanager
    def fetched(self, data: bytes, *, digest: str | None = None) -> Generator[list[str], None, None]:
        calls: list[str] = []
        expected = digest if digest is not None else hashlib.sha256(data).hexdigest()

        def download(url: str, target: IO[bytes]) -> None:
            calls.append(url)
            target.write(data)

        with patch.object(assets, "ARCHIVE_PINS", {key: expected for key in assets.ARCHIVE_PINS}), \
                patch.object(assets, "download_archive", side_effect=download):
            yield calls

    def assert_clean(self) -> None:
        self.assertEqual(list(self.output.glob(".pueue-install-*")), [])

    def assert_installed(self, role: str) -> None:
        program = self.output / role
        metadata = program.lstat()
        self.assertTrue(stat.S_ISREG(metadata.st_mode))
        self.assertEqual(metadata.st_mode & 0o777, 0o755)
        self.assertEqual(metadata.st_size, len(PAYLOADS[role]))
        # These source-authored stand-ins use shell builtins only. run owns the
        # direct child's Wait, including timeout cleanup; no descendant exists.
        result = subprocess.run(
            [str(program), "--version"], env={}, cwd=self.root,
            capture_output=True, check=True, timeout=5,
        )
        self.assertEqual(result.stdout, f"{role} 4.0.4\n".encode())
        self.assertEqual(result.stderr, b"")

    def existing(self) -> None:
        self.output.mkdir()
        for role in assets.PAIR:
            (self.output / role).write_bytes(b"previous " + role.encode())

    def test_destination_closes_every_fd_and_joins_operation_and_close_faults(self) -> None:
        descriptors: list[int] = []
        close = os.close
        operation_fault = OSError("operation failed")
        close_fault = OSError("injected close fault after retiring handle")
        observed: BaseException | None = None
        faulted = False

        def close_once_with_fault(descriptor: int) -> None:
            nonlocal faulted
            close(descriptor)
            if not faulted:
                faulted = True
                raise close_fault

        try:
            with patch.object(assets.os, "close", side_effect=close_once_with_fault):
                try:
                    with assets.destination(self.output) as output:
                        descriptors = [output.lineage[0][0], *(child for _, _, child in output.lineage)]
                        raise operation_fault
                except BaseException as error:
                    observed = error
            for descriptor in descriptors:
                with self.assertRaises(OSError, msg="destination leaked a later acquired descriptor") as caught:
                    os.fstat(descriptor)
                self.assertEqual(caught.exception.errno, errno.EBADF)
            self.assertIsInstance(observed, BaseExceptionGroup)
            if isinstance(observed, BaseExceptionGroup):
                self.assertIn(operation_fault, observed.exceptions)
                self.assertTrue(any(isinstance(error, BaseExceptionGroup) and close_fault in error.exceptions
                                    for error in observed.exceptions))
        finally:
            for descriptor in descriptors:
                try:
                    os.fstat(descriptor)
                except OSError as error:
                    if error.errno != errno.EBADF:
                        raise
                else:
                    close(descriptor)

    def test_staging_replacement_is_preserved_and_original_handle_is_closed(self) -> None:
        stage: int | None = None
        observed: assets.InstallError | None = None
        try:
            with assets.destination(self.output) as output:
                with patch.object(assets.secrets, "token_hex", return_value="owned"):
                    try:
                        with assets.staging(output) as descriptor:
                            stage = descriptor
                            os.rename(".pueue-install-owned", "renamed-owned", src_dir_fd=output.fd, dst_dir_fd=output.fd)
                            os.mkdir(".pueue-install-owned", 0o700, dir_fd=output.fd)
                            foreign = os.stat(".pueue-install-owned", dir_fd=output.fd, follow_symlinks=False)
                    except assets.InstallError as error:
                        observed = error
                replacement = self.output / ".pueue-install-owned"
                self.assertTrue(replacement.is_dir(), "cleanup deleted a foreign replacement directory")
                self.assertTrue(os.path.samestat(foreign, replacement.stat()))
                self.assertIsNotNone(observed, "stage replacement falsely reported successful cleanup")
                self.assertIn("identity changed", str(observed))
                self.assertIsNotNone(stage)
                if stage is not None:
                    with self.assertRaises(OSError) as caught:
                        os.fstat(stage)
                    self.assertEqual(caught.exception.errno, errno.EBADF)
        finally:
            if stage is not None:
                try:
                    os.fstat(stage)
                except OSError as error:
                    if error.errno != errno.EBADF:
                        raise
                else:
                    os.close(stage)

    def test_fixed_pin_table_and_all_four_output_layouts(self) -> None:
        self.assertEqual(set(assets.ARCHIVE_PINS), {
            ("linux", "amd64"), ("linux", "arm64"), ("darwin", "amd64"), ("darwin", "arm64")})
        self.assertEqual(len(set(assets.ARCHIVE_PINS.values())), 4)
        self.assertEqual(assets.RELEASE_URL,
                         "https://github.com/hishamkaram/delegation-layer/releases/download/v0.3.1")
        with self.fetched(archive()) as calls, patch("builtins.print"):
            self.assertEqual(assets.main([str(self.output)]), 0)
        self.assertEqual(set(calls), {
            f"{assets.RELEASE_URL}/delegation-layer_0.3.1_{system}_{arch}.tar.gz"
            for system in ("Linux", "Darwin") for arch in ("amd64", "arm64")})
        files = {str(path.relative_to(self.output)) for path in self.output.rglob("*") if path.is_file()}
        self.assertEqual(files, {f"{system}/{arch}/{role}" for system in ("linux", "darwin")
                                 for arch in ("amd64", "arm64") for role in ("pueue", "pueued")})

    def test_host_selection_only_installs_pair_and_refreshes_modified_files(self) -> None:
        for system, machine, target in (("Linux", "x86_64", "Linux_amd64"),
                                        ("Linux", "aarch64", "Linux_arm64"),
                                        ("Darwin", "amd64", "Darwin_amd64"),
                                        ("Darwin", "arm64", "Darwin_arm64")):
            with self.subTest(system=system, machine=machine):
                with self.fetched(archive()) as calls, patch.object(assets.platform, "system", return_value=system), \
                        patch.object(assets.platform, "machine", return_value=machine), patch("builtins.print"):
                    self.assertEqual(assets.main(["--host", str(self.output)]), 0)
                    (self.output / "pueue").write_bytes(b"modified installed client")
                    self.assertEqual(assets.main(["--host", str(self.output)]), 0)
                self.assertEqual(calls, [f"{assets.RELEASE_URL}/delegation-layer_0.3.1_{target}.tar.gz"] * 2)
                self.assertEqual({p.name for p in self.output.iterdir()}, {"pueue", "pueued"})
                for role in assets.PAIR:
                    self.assert_installed(role)

    def test_digest_mismatch_refuses_before_parsing_and_preserves_outputs(self) -> None:
        self.existing()
        with self.fetched(archive(), digest="0" * 64), patch.object(assets, "stage_pair") as parse:
            with self.assertRaisesRegex(assets.InstallError, "checksum mismatch"):
                assets.install_pair(self.output, *KEY)
        parse.assert_not_called()
        for role in assets.PAIR:
            self.assertEqual((self.output / role).read_bytes(), b"previous " + role.encode())
        self.assert_clean()

    def test_malformed_archives_preserve_both_existing_outputs(self) -> None:
        valid = [member(name, data) for name, data in PAYLOADS.items()]
        cases = {
            "missing": valid[1:], "duplicate": [*valid, valid[0]],
            "traversal": [member("../pueue", b"x"), *valid[1:]],
            "alias": [member("./pueue", b"x"), *valid[1:]],
            "absolute": [member("/pueue", b"x"), *valid[1:]],
            "symlink": [member("pueue", b"", kind=tarfile.SYMTYPE), *valid[1:]],
            "hardlink": [member("pueue", b"", kind=tarfile.LNKTYPE), *valid[1:]],
            "fifo": [member("pueue", b"", kind=tarfile.FIFOTYPE), *valid[1:]],
            "directory": [member("pueue", b"", kind=tarfile.DIRTYPE), *valid[1:]],
            "sparse": [member("pueue", b"x", kind=tarfile.GNUTYPE_SPARSE), *valid[1:]],
            "pax": [member("pueue", b"", kind=tarfile.XHDTYPE), *valid[1:]],
            "empty": [member("pueue", b""), *valid[1:]],
            "nonexec": [member("pueue", b"x", mode=0o644), *valid[1:]],
            "daemon nonexec": [valid[0], member("pueued", b"x", mode=0o644), *valid[2:]],
            "specialmode": [member("pueue", b"x", mode=0o4755), *valid[1:]],
            "oversize": [member("pueue", b"x", size=assets.MAX_SELECTED + 1), *valid[1:]],
            "other oversize": [*valid[:2], member("delegate", b"x", size=assets.MAX_OTHER + 1), valid[3]],
            "unknown": [*valid, member("license-unknown", b"x")],
        }
        self.existing()
        for name, entries in cases.items():
            with self.subTest(name=name), self.fetched(archive(entries)):
                with self.assertRaises((assets.InstallError, tarfile.TarError)):
                    assets.install_pair(self.output, *KEY)
                for role in assets.PAIR:
                    self.assertEqual((self.output / role).read_bytes(), b"previous " + role.encode())
                self.assert_clean()

    def test_truncation_trailing_data_and_gzip_crc_fail_without_publish(self) -> None:
        good = archive()
        broken_crc = bytearray(good)
        broken_crc[-8] ^= 1
        self.existing()
        for data in (good[:-8], bytes(broken_crc), archive(tail=bytes(1024) + b"nonzero"),
                     archive([member("pueue", b"x", size=4096)], tail=b"")):
            with self.subTest(size=len(data)), self.fetched(data):
                with self.assertRaises((assets.InstallError, OSError, EOFError)):
                    assets.install_pair(self.output, *KEY)
                self.assertEqual((self.output / "pueue").read_bytes(), b"previous pueue")
                self.assert_clean()

    def test_symlink_destinations_refuse_but_parent_alias_and_hardlink_are_safe(self) -> None:
        outside = self.root / "outside"
        outside.mkdir()
        self.output.symlink_to(outside, target_is_directory=True)
        with self.fetched(archive()) as calls:
            with self.assertRaises(assets.InstallError):
                assets.install_pair(self.output, *KEY)
            self.assertEqual(calls, [])
        self.output.unlink()
        self.output.mkdir()
        other_file = outside / "file"
        other_file.write_bytes(b"outside file")
        (self.output / "pueue").symlink_to(other_file)
        with self.fetched(archive()):
            with self.assertRaises(assets.InstallError):
                assets.install_pair(self.output, *KEY)
        (self.output / "pueue").unlink()
        os.link(other_file, self.output / "pueue")
        alias = self.root / "parent-alias"
        alias.symlink_to(self.root, target_is_directory=True)
        with self.fetched(archive()):
            assets.install_pair(alias / "output", *KEY)
        self.assertEqual(other_file.read_bytes(), b"outside file")
        self.assert_installed("pueue")
        self.assert_clean()

    def test_failures_and_interruption_remove_owned_staging(self) -> None:
        self.existing()
        for failure in (OSError("network failed"), KeyboardInterrupt()):
            with self.subTest(failure=type(failure).__name__), \
                    patch.object(assets, "download_archive", side_effect=failure):
                with self.assertRaises(type(failure)):
                    assets.install_pair(self.output, *KEY)
                self.assert_clean()
        with self.fetched(archive()), patch.object(assets.os, "replace", side_effect=OSError("publish failed")):
            with self.assertRaisesRegex(assets.InstallError, "installation may be partial"):
                assets.install_pair(self.output, *KEY)
        self.assertEqual((self.output / "pueue").read_bytes(), b"previous pueue")
        self.assert_clean()

    def test_changed_destination_identity_or_target_refuses_before_publish(self) -> None:
        self.existing()
        outside = self.root / "outside"
        outside.write_bytes(b"outside data")
        data = archive()

        def replace_target(url: str, target: IO[bytes]) -> None:
            target.write(data)
            (self.output / "pueue").unlink()
            (self.output / "pueue").symlink_to(outside)

        with self.fetched(data), patch.object(assets, "download_archive", side_effect=replace_target):
            with self.assertRaises(assets.InstallError):
                assets.install_pair(self.output, *KEY)
        self.assertEqual(outside.read_bytes(), b"outside data")
        self.assertEqual((self.output / "pueued").read_bytes(), b"previous pueued")
        self.assert_clean()
        (self.output / "pueue").unlink()
        moved = self.root / "moved-output"

        def replace_directory(url: str, target: IO[bytes]) -> None:
            target.write(data)
            self.output.rename(moved)
            self.output.mkdir()

        with self.fetched(data), patch.object(assets, "download_archive", side_effect=replace_directory):
            with self.assertRaisesRegex(assets.InstallError, "identity changed"):
                assets.install_pair(self.output, *KEY)
        self.assertEqual(list(self.output.iterdir()), [])
        self.assertEqual(list(moved.glob(".pueue-install-*")), [])
        self.assertEqual((moved / "pueued").read_bytes(), b"previous pueued")

    def test_special_output_target_refuses_without_downloading(self) -> None:
        self.output.mkdir()
        os.mkfifo(self.output / "pueue")
        with self.fetched(archive()) as calls:
            with self.assertRaises(assets.InstallError):
                assets.install_pair(self.output, *KEY)
            self.assertEqual(calls, [])

    def test_actual_download_boundary_bounds_bytes_and_rejects_http_redirects(self) -> None:
        class Response(io.BytesIO):
            url = "https://release-assets.githubusercontent.com/example"

        for payload, limit, accepted in ((b"archive", 8, True), (b"too large", 8, False), (b"", 8, False)):
            with self.subTest(payload=payload), patch.object(assets.urllib.request, "build_opener") as opener, \
                    patch.object(assets, "MAX_ARCHIVE", limit):
                opener.return_value.open.return_value = Response(payload)
                target = io.BytesIO()
                if accepted:
                    assets.download_archive(assets.RELEASE_URL + "/archive.tar.gz", target)
                    self.assertEqual(target.getvalue(), payload)
                else:
                    with self.assertRaises(assets.InstallError):
                        assets.download_archive(assets.RELEASE_URL + "/archive.tar.gz", target)
        request = assets.urllib.request.Request(assets.RELEASE_URL + "/archive.tar.gz")
        redirects = assets.github_release_asset.HTTPSRedirects()
        with self.assertRaisesRegex(assets.github_release_asset.DownloadError, "HTTPS"):
            redirects.redirect_request(req=request, fp=io.BytesIO(), code=302, msg="redirect",
                                       headers=HTTPMessage(), newurl="http://example/archive")
        allowed = redirects.redirect_request(req=request, fp=io.BytesIO(), code=302, msg="redirect",
                                             headers=HTTPMessage(), newurl="https://example/archive")
        self.assertIsNotNone(allowed)

    def test_transient_release_failure_uses_exact_asset_api_and_existing_pin(self) -> None:
        class Response(io.BytesIO):
            url = "https://api.github.com/repos/hishamkaram/delegation-layer/releases/assets/17"

        data = archive()
        metadata = json.dumps({"tag_name": "v0.3.1", "assets": [{"name": "delegation-layer_0.3.1_Linux_amd64.tar.gz",
                                         "id": 17, "state": "uploaded", "size": len(data)}]}).encode()
        responses = [urllib.error.HTTPError(assets.RELEASE_URL, 504, "timeout", HTTPMessage(), None),
                     Response(metadata), Response(data)]
        with patch.object(assets.urllib.request, "build_opener") as opener, \
                patch.object(assets, "ARCHIVE_PINS", {KEY: hashlib.sha256(data).hexdigest()}), \
                patch("sys.stderr", new_callable=io.StringIO) as diagnostic:
            opener.return_value.open.side_effect = responses
            assets.install_pair(self.output, *KEY)
        self.assertIn("HTTP Error 504", diagnostic.getvalue())
        self.assertIn("trying the exact pinned asset", diagnostic.getvalue())
        self.assert_installed("pueue")
        self.assert_installed("pueued")
        self.assert_clean()

    def test_cli_interruption_restores_handlers_and_cleans_staging(self) -> None:
        self.existing()
        previous = signal.getsignal(signal.SIGTERM)

        def interrupted(url: str, target: IO[bytes]) -> None:
            target.write(b"partial archive")
            handler = signal.getsignal(signal.SIGTERM)
            self.assertTrue(callable(handler))
            if callable(handler):
                handler(signal.SIGTERM, None)  # Direct unit call; no native signal is sent.

        with patch.object(assets.platform, "system", return_value="Linux"), \
                patch.object(assets.platform, "machine", return_value="x86_64"), \
                patch.object(assets, "download_archive", side_effect=interrupted), \
                patch("sys.stderr", new_callable=io.StringIO):
            self.assertEqual(assets.main(["--host", str(self.output)]), 128 + signal.SIGTERM)
        self.assertIs(signal.getsignal(signal.SIGTERM), previous)
        self.assert_clean()
        self.assertEqual((self.output / "pueue").read_bytes(), b"previous pueue")

    def test_second_staged_member_failure_preserves_existing_pair(self) -> None:
        self.existing()
        with self.fetched(archive()), patch.object(assets.os, "fchmod", side_effect=[None, OSError("second member failed")]):
            with self.assertRaisesRegex(OSError, "second member"):
                assets.install_pair(self.output, *KEY)
        for role in assets.PAIR:
            self.assertEqual((self.output / role).read_bytes(), b"previous " + role.encode())
        self.assert_clean()

    def test_second_publication_failure_reports_partial_pair_and_cleans_stage(self) -> None:
        self.existing()
        replace = os.replace

        def fail_second(source: str, target: str, *, src_dir_fd: int, dst_dir_fd: int) -> None:
            if target == "pueued":
                raise OSError("second publication failed")
            replace(source, target, src_dir_fd=src_dir_fd, dst_dir_fd=dst_dir_fd)

        with self.fetched(archive()), patch.object(assets.os, "replace", side_effect=fail_second):
            with self.assertRaisesRegex(assets.InstallError, "installation may be partial"):
                assets.install_pair(self.output, *KEY)
        self.assert_installed("pueue")
        self.assertEqual((self.output / "pueued").read_bytes(), b"previous pueued")
        self.assert_clean()

    def test_only_whole_archive_bytes_are_hashed_and_no_command_executes(self) -> None:
        self.existing()
        data = archive()
        original_hash = hashlib.sha256
        observed: list[bytes] = []

        class ArchiveHash:
            def __init__(self) -> None:
                self.digest = original_hash()

            def update(self, chunk: bytes) -> None:
                observed.append(chunk)
                self.digest.update(chunk)

            def hexdigest(self) -> str:
                return self.digest.hexdigest()

        with self.fetched(data), patch.object(assets.hashlib, "sha256", side_effect=ArchiveHash), \
                patch("subprocess.Popen", side_effect=AssertionError("must not execute")), \
                patch("os.system", side_effect=AssertionError("must not execute")):
            assets.install_pair(self.output, *KEY)
        self.assertEqual(b"".join(observed), data)
        self.assert_clean()

    def test_download_and_expansion_are_bounded(self) -> None:
        with patch.object(assets, "MAX_ARCHIVE", 8):
            with self.fetched(b"x" * 9):
                with self.assertRaisesRegex(assets.InstallError, "oversized"):
                    assets.install_pair(self.output, *KEY)
        with self.fetched(archive()), patch.object(assets, "MAX_TAR", 8):
            with self.assertRaisesRegex(assets.InstallError, "bound"):
                assets.install_pair(self.output, *KEY)
        self.assert_clean()

    def test_host_refusal_and_frontends_share_one_owner(self) -> None:
        with patch.object(assets.platform, "system", return_value="Unsupported"):
            with self.assertRaises(assets.InstallError):
                assets.host_platform()
        with self.assertRaises(SystemExit), patch("sys.stderr", new_callable=io.StringIO):
            assets.main(["--host"])
        scripts = Path(__file__).parent
        fetch = (scripts / "fetch-pueue-assets.sh").read_text()
        install = (scripts / "install-test-supervisor.sh").read_text()
        self.assertIn('exec python3 "${pueue_script_dir}/fetch_pueue_assets.py" "$@"', fetch)
        self.assertIn('fetch-pueue-assets.sh" --host', install)
        self.assertFalse((scripts / "test-supervisor-sha256.txt").exists())


if __name__ == "__main__":
    unittest.main()
