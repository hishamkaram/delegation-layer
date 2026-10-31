"""Shared directory-owner cleanup of product-sealed (read-only) owned trees."""

import contextlib
import errno
import itertools
import os
from pathlib import Path
import stat
import tempfile
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.directories import operations as directory_operations


SEALED_MODE = 0o500
REAL_OPEN = os.open
REAL_CLOSE = os.close
REAL_STAT = os.stat
REAL_FCHMOD = os.fchmod
REAL_PATH_RENAME = Path.rename
REAL_RENAME_NOREPLACE = directory_operations._rename_noreplace


def _unseal(root):
    """Restore owner access below a test root so TemporaryDirectory can clean it."""
    for current, directories, _files in os.walk(root):
        for name in directories:
            path = Path(current) / name
            if not path.is_symlink():
                path.chmod(0o700)


def _seal_bottom_up(directory):
    for current, _directories, _files in sorted(os.walk(directory), reverse=True):
        Path(current).chmod(SEALED_MODE)


def _identity(result):
    return (result.st_dev, result.st_ino)


def _require_rename_owner_write(source, destination):
    if stat.S_ISDIR(source.st_mode) and not source.st_mode & stat.S_IWUSR:
        raise PermissionError(errno.EACCES, os.strerror(errno.EACCES), os.fspath(destination))


def _old_xnu_rename(source_fd, source_name, destination_fd, destination_name):
    """Emulate xnu-11417: any directory rename needs owner write on the directory itself."""
    source = REAL_STAT(source_name, dir_fd=source_fd, follow_symlinks=False)
    _require_rename_owner_write(source, destination_name)
    REAL_RENAME_NOREPLACE(source_fd, source_name, destination_fd, destination_name)


def _old_xnu_path_rename(source, destination):
    _require_rename_owner_write(source.lstat(), destination)
    return REAL_PATH_RENAME(source, destination)


class CleanupRecorder:
    """Record cleanup descriptors, directory renames, isolated-name checks and grants."""

    def __init__(self, rename=REAL_RENAME_NOREPLACE, on_open=None):
        self.rename_operation = rename
        self.on_open = on_open
        self.live = {}
        self.events = []
        self.granted = []

    def open(self, path, flags, *args, **kwargs):
        if self.on_open is not None:
            self.on_open(path)
        descriptor = REAL_OPEN(path, flags, *args, **kwargs)
        self.live[descriptor] = _identity(os.fstat(descriptor))
        return descriptor

    def close(self, descriptor):
        identity = self.live.pop(descriptor, None)
        if identity is not None:
            self.events.append(("close", identity))
        REAL_CLOSE(descriptor)

    def stat(self, path, *args, **kwargs):
        if isinstance(path, str) and path.startswith(".delegate-cleanup-"):
            self.events.append(("check", path))
        return REAL_STAT(path, *args, **kwargs)

    def fchmod(self, descriptor, mode):
        self.granted.append(_identity(os.fstat(descriptor)))
        REAL_FCHMOD(descriptor, mode)

    def rename(self, source_fd, source_name, destination_fd, destination_name):
        source = _identity(REAL_STAT(source_name, dir_fd=source_fd, follow_symlinks=False))
        writable = any(identity == source and os.fstat(descriptor).st_mode & stat.S_IWUSR
                       for descriptor, identity in self.live.items())
        self.events.append(("rename", source, destination_name, writable))
        return self.rename_operation(source_fd, source_name, destination_fd, destination_name)

    @contextlib.contextmanager
    def active(self):
        with patch.object(directory_operations.os, "open", side_effect=self.open), \
                patch.object(directory_operations.os, "close", side_effect=self.close), \
                patch.object(directory_operations.os, "stat", side_effect=self.stat), \
                patch.object(directory_operations.os, "fchmod", side_effect=self.fchmod), \
                patch.object(directory_operations, "_rename_noreplace", side_effect=self.rename):
            yield self


class SealedOwnedCleanupTests(unittest.TestCase):
    def setUp(self):
        self.workspace = tempfile.TemporaryDirectory(prefix="acceptance-sealed-")
        self.root = Path(self.workspace.name)
        self.addCleanup(self.workspace.cleanup)
        self.addCleanup(_unseal, self.root)

    def owned_tree(self, workspace=None):
        workspace = self.root if workspace is None else workspace
        owner = directory_operations.DirectoryOwner()
        token = owner.create("owned", workspace / "owned", workspace)
        (token.directory / "views" / "ns").mkdir(parents=True)
        return token

    def isolated_entries(self, workspace=None):
        workspace = self.root if workspace is None else workspace
        return tuple(workspace.glob(".delegate-cleanup-*"))

    def fresh_workspace(self, label):
        return Path(tempfile.mkdtemp(prefix=label + "-", dir=self.root))

    def sealed_tree_with_shared_entries(self):
        outside = self.root / "outside"
        outside.mkdir()
        linked = outside / "linked.bin"
        linked.write_bytes(b"shared image")
        linked.chmod(0o400)
        target = outside / "target.txt"
        target.write_text("target")
        target.chmod(0o640)
        token = self.owned_tree()
        view = token.directory / "views" / "ns" / "delegation-layer-verified-1"
        view.mkdir()
        os.link(linked, view / "entry")
        (view / "link").symlink_to(target)
        _seal_bottom_up(token.directory)
        return token, linked, target

    def assert_tree_removed_without_changing_shared_entries(self, linked, target):
        self.assertFalse((self.root / "owned").exists())
        self.assertEqual(self.isolated_entries(), ())
        self.assertEqual(stat.S_IMODE(linked.stat().st_mode), 0o400)
        self.assertEqual(linked.stat().st_nlink, 1)
        self.assertEqual(linked.read_bytes(), b"shared image")
        self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o640)
        self.assertEqual(target.read_text(), "target")

    def assert_renames_held_until_verified(self, recorder):
        renames = [position for position, event in enumerate(recorder.events)
                   if event[0] == "rename"]
        self.assertTrue(renames)
        for position in renames:
            _kind, source, destination, writable = recorder.events[position]
            self.assertTrue(writable, "directory renamed without a held granted descriptor")
            later = recorder.events[position:]
            self.assertIn(("close", source), later, "held descriptor was never closed")
            closed = later.index(("close", source))
            self.assertIn(("check", destination), later[:closed],
                          "held descriptor closed before the isolated identity check")
        self.assertEqual(recorder.live, {})

    def remove_succeeds(self, token):
        try:
            self.assertTrue(directory_operations.remove_owned_directory(token))
        except (OSError, RuntimeError) as error:
            self.fail("sealed owned cleanup was refused: " + repr(error))

    def remove_refused(self, token, pattern, **kwargs):
        """Return the actionable refusal; an unclassified OS fault fails the test."""
        try:
            directory_operations.remove_owned_directory(token, **kwargs)
        except RuntimeError as error:
            self.assertRegex(str(error), pattern)
            return error
        except OSError as error:
            self.fail("cleanup surfaced an unclassified OS fault: " + repr(error))
        self.fail("cleanup did not refuse")

    def test_nested_sealed_tree_is_removed_without_changing_file_or_target_modes(self):
        token, linked, target = self.sealed_tree_with_shared_entries()

        self.assertTrue(directory_operations.remove_owned_directory(token))

        self.assert_tree_removed_without_changing_shared_entries(linked, target)

    def test_nested_sealed_tree_is_removed_when_directory_rename_needs_owner_write(self):
        token, linked, target = self.sealed_tree_with_shared_entries()
        recorder = CleanupRecorder(_old_xnu_rename)

        with recorder.active():
            self.remove_succeeds(token)

        self.assert_tree_removed_without_changing_shared_entries(linked, target)
        self.assert_renames_held_until_verified(recorder)

    def test_isolation_name_collision_is_never_replaced(self):
        token = self.owned_tree()
        _seal_bottom_up(token.directory)
        collision = self.root / ".delegate-cleanup-taken"
        collision.mkdir()
        (collision / "keep.txt").write_text("unrelated")
        names = itertools.chain(("taken",), ("fresh" + str(index) for index in itertools.count()))
        with patch.object(directory_operations.secrets, "token_hex",
                          side_effect=lambda _size: next(names)):
            self.assertTrue(directory_operations.remove_owned_directory(token))
        self.assertEqual((collision / "keep.txt").read_text(), "unrelated")
        self.assertEqual(self.isolated_entries(), (collision,))
        self.assertFalse((self.root / "owned").exists())

    def test_replacement_before_grant_receives_no_chmod_and_stays_in_place(self):
        self.assert_replacement_before_grant_is_preserved()

    def test_replacement_before_grant_when_directory_rename_needs_owner_write(self):
        with patch.object(Path, "rename", _old_xnu_path_rename):
            self.assert_replacement_before_grant_is_preserved()

    def assert_replacement_before_grant_is_preserved(self):
        target = self.root / "target.txt"
        target.write_text("target")
        target.chmod(0o640)

        def make_directory(path):
            path.mkdir()
            (path / "keep.txt").write_text("replacement")
            path.chmod(SEALED_MODE)

        def make_file(path):
            path.write_text("replacement")
            path.chmod(0o400)

        kinds = (("directory", make_directory),
                 ("symlink", lambda path: path.symlink_to(target)),
                 ("file", make_file))
        for kind, make_replacement in kinds:
            with self.subTest(kind=kind):
                workspace = self.fresh_workspace(kind)
                token = self.owned_tree(workspace)
                original = workspace / "original"
                replaced = []

                def replace_before_open(path, token=token, original=original,
                                        make_replacement=make_replacement, replaced=replaced):
                    if path == "owned" and not replaced:
                        # Some kernels require owner write for this fixture's
                        # rename. Seal the displaced original before replacing it.
                        token.directory.rename(original)
                        _seal_bottom_up(original)
                        make_replacement(token.directory)
                        result = token.directory.lstat()
                        replaced.append((_identity(result), stat.S_IMODE(result.st_mode)))

                recorder = CleanupRecorder(on_open=replace_before_open)
                with recorder.active():
                    with self.assertRaises((OSError, RuntimeError)) as raised:
                        directory_operations.remove_owned_directory(token)

                self.assertEqual(len(replaced), 1, "replacement fixture failed: " + repr(raised.exception))
                self.assertEqual(recorder.granted, [])
                self.assertEqual(recorder.live, {})
                self.assertEqual(self.isolated_entries(workspace), ())
                held = token.directory.lstat()
                self.assertEqual((_identity(held), stat.S_IMODE(held.st_mode)), replaced[0])
                if kind == "directory":
                    self.assertEqual((token.directory / "keep.txt").read_text(), "replacement")
                elif kind == "file":
                    self.assertEqual(token.directory.read_text(), "replacement")
                self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o640)
                self.assertEqual(target.read_text(), "target")
                self.assertEqual(_identity(original.lstat()), token.identity)
                self.assertEqual(stat.S_IMODE(original.lstat().st_mode), SEALED_MODE)

    def test_replacement_after_grant_retains_one_unchanged_copy(self):
        for label, inner_rename in (("native", REAL_RENAME_NOREPLACE),
                                    ("owner-write-required", _old_xnu_rename)):
            with self.subTest(rename=label):
                workspace = self.fresh_workspace(label)
                token = self.owned_tree(workspace)
                _seal_bottom_up(token.directory)
                replaced = []

                def replace_then_rename(source_fd, source_name, destination_fd,
                                        destination_name, token=token, workspace=workspace,
                                        inner_rename=inner_rename, replaced=replaced):
                    if source_name == "owned" and not replaced:
                        token.directory.rename(workspace / "original")
                        token.directory.mkdir()
                        (token.directory / "keep.txt").write_text("replacement")
                        token.directory.chmod(SEALED_MODE)
                        replaced.append(_identity(token.directory.lstat()))
                    return inner_rename(source_fd, source_name, destination_fd, destination_name)

                recorder = CleanupRecorder(replace_then_rename)
                with recorder.active():
                    error = self.remove_refused(token, "replaced")

                copies = [path for path in workspace.iterdir()
                          if _identity(path.lstat()) == replaced[0]]
                self.assertEqual(len(copies), 1)
                if "in place" in str(error):
                    self.assertEqual(copies[0].name, "owned")
                else:
                    self.assertRegex(str(error), "quarantine: " + copies[0].name)
                    self.assertTrue(copies[0].name.startswith(".delegate-cleanup-"))
                if label != "native":
                    self.assertEqual(copies[0].name, "owned")
                self.assertEqual(sorted(path.name for path in workspace.iterdir()),
                                 sorted(("original", copies[0].name)))
                self.assertEqual(stat.S_IMODE(copies[0].lstat().st_mode), SEALED_MODE)
                self.assertEqual((copies[0] / "keep.txt").read_text(), "replacement")
                self.assertNotIn(replaced[0], recorder.granted)
                self.assertEqual(_identity((workspace / "original").lstat()), token.identity)
                self.assertTrue((workspace / "original" / "views").is_dir())
                self.assertEqual(recorder.live, {})

    def test_deadline_expiry_retains_remaining_sealed_contents(self):
        token = self.owned_tree()
        view = token.directory / "views" / "ns" / "delegation-layer-verified-1"
        view.mkdir()
        (view / "first").write_text("first")
        (view / "second").write_text("second")
        file_mode = stat.S_IMODE((view / "second").stat().st_mode)
        _seal_bottom_up(token.directory)
        removed = []
        original_unlink = os.unlink

        def unlink_once(*args, **kwargs):
            original_unlink(*args, **kwargs)
            removed.append(args[0])

        with patch.object(directory_operations.os, "unlink", side_effect=unlink_once), \
                patch.object(directory_operations.time, "monotonic",
                             side_effect=lambda: 200.0 if removed else 0.0):
            with self.assertRaisesRegex(RuntimeError, "deadline expired"):
                directory_operations.remove_owned_directory(token, deadline=100.0)
        self.assertEqual(len(removed), 1)
        retained = self.isolated_entries()
        self.assertEqual(len(retained), 1)
        survivors = [path for path in retained[0].rglob("*") if path.is_file()]
        self.assertEqual(len(survivors), 1)
        self.assertEqual(stat.S_IMODE(survivors[0].stat().st_mode), file_mode)

    def test_expired_deadline_never_grants_owner_access(self):
        for moment in ("initial", "after-open"):
            with self.subTest(moment=moment):
                workspace = self.fresh_workspace(moment)
                token = self.owned_tree(workspace)
                _seal_bottom_up(token.directory)
                expired = [moment == "initial"]

                def expire_on_open(_path, expired=expired):
                    expired[0] = True

                recorder = CleanupRecorder(on_open=expire_on_open)
                with recorder.active(), \
                        patch.object(directory_operations.time, "monotonic",
                                     side_effect=lambda expired=expired: 200.0 if expired[0] else 0.0):
                    self.remove_refused(token, "deadline expired", deadline=100.0)

                self.assertEqual(recorder.granted, [])
                self.assertEqual(recorder.live, {})
                self.assertEqual(self.isolated_entries(workspace), ())
                retained = token.directory.lstat()
                self.assertEqual(_identity(retained), token.identity)
                self.assertEqual(stat.S_IMODE(retained.st_mode), SEALED_MODE)

    def test_sealed_directory_of_another_user_is_refused_before_mutation(self):
        token = self.owned_tree()
        token.directory.chmod(SEALED_MODE)
        recorder = CleanupRecorder()
        with recorder.active(), \
                patch.object(directory_operations.os, "getuid", return_value=os.getuid() + 1):
            self.remove_refused(token, "not owned by the current user")
        self.assertEqual(recorder.granted, [])
        self.assertEqual(recorder.live, {})
        self.assertEqual([event for event in recorder.events if event[0] == "rename"], [])
        self.assertEqual(self.isolated_entries(), ())
        retained = token.directory.lstat()
        self.assertEqual(_identity(retained), token.identity)
        self.assertEqual(stat.S_IMODE(retained.st_mode), SEALED_MODE)
        self.assertTrue((token.directory / "views").is_dir())

    def test_permission_fault_on_unchanged_directory_is_surfaced(self):
        token = self.owned_tree()
        _seal_bottom_up(token.directory)
        refusal = PermissionError(errno.EACCES, os.strerror(errno.EACCES), "owned")

        def refuse(_source_fd, _source_name, _destination_fd, _destination_name):
            raise refusal

        recorder = CleanupRecorder(refuse)
        with recorder.active():
            with self.assertRaises(PermissionError) as raised:
                directory_operations.remove_owned_directory(token)
        self.assertIs(raised.exception, refusal)
        self.assertEqual(recorder.live, {})
        self.assertEqual(self.isolated_entries(), ())
        self.assertEqual(_identity(token.directory.lstat()), token.identity)
        self.assertTrue((token.directory / "views" / "ns").is_dir())

    def test_unverifiable_permission_refusal_keeps_both_causes(self):
        token = self.owned_tree()
        _seal_bottom_up(token.directory)
        refusal = PermissionError(errno.EACCES, os.strerror(errno.EACCES), "owned")

        def move_then_refuse(_source_fd, _source_name, _destination_fd, _destination_name):
            token.directory.rename(self.root / "moved")
            raise refusal

        recorder = CleanupRecorder(move_then_refuse)
        with recorder.active():
            error = self.remove_refused(token, "verify")
        self.assertNotRegex(str(error), "replaced")
        self.assertIsInstance(error.__cause__, FileNotFoundError)
        self.assertIs(error.__cause__.__context__, refusal)
        self.assertEqual(recorder.live, {})
        self.assertEqual(self.isolated_entries(), ())
        self.assertEqual(_identity((self.root / "moved").lstat()), token.identity)


if __name__ == "__main__":
    unittest.main()
