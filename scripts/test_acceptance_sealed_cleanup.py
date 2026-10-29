"""Shared directory-owner cleanup of product-sealed (read-only) owned trees."""

import itertools
import os
from pathlib import Path
import stat
import tempfile
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.directories import operations as directory_operations


SEALED_MODE = 0o500


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


class SealedOwnedCleanupTests(unittest.TestCase):
    def setUp(self):
        self.workspace = tempfile.TemporaryDirectory(prefix="acceptance-sealed-")
        self.root = Path(self.workspace.name)
        self.addCleanup(self.workspace.cleanup)
        self.addCleanup(_unseal, self.root)

    def owned_tree(self):
        owner = directory_operations.DirectoryOwner()
        token = owner.create("owned", self.root / "owned", self.root)
        (token.directory / "views" / "ns").mkdir(parents=True)
        return token

    def isolated_entries(self):
        return tuple(self.root.glob(".delegate-cleanup-*"))

    def test_nested_sealed_tree_is_removed_without_changing_file_or_target_modes(self):
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

        self.assertTrue(directory_operations.remove_owned_directory(token))

        self.assertFalse((self.root / "owned").exists())
        self.assertEqual(self.isolated_entries(), ())
        self.assertEqual(stat.S_IMODE(linked.stat().st_mode), 0o400)
        self.assertEqual(linked.stat().st_nlink, 1)
        self.assertEqual(linked.read_bytes(), b"shared image")
        self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o640)
        self.assertEqual(target.read_text(), "target")

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

    def test_sealed_replacement_before_isolation_is_retained_unmodified(self):
        token = self.owned_tree()
        original_rename = directory_operations._rename_noreplace
        replacement_identity = []

        def replace_then_rename(source_fd, source_name, destination_fd, destination_name):
            if source_name == "owned" and not replacement_identity:
                token.directory.rename(self.root / "original")
                token.directory.mkdir()
                (token.directory / "keep.txt").write_text("replacement")
                token.directory.chmod(SEALED_MODE)
                result = token.directory.lstat()
                replacement_identity.append((result.st_dev, result.st_ino))
            return original_rename(source_fd, source_name, destination_fd, destination_name)

        with patch.object(directory_operations, "_rename_noreplace",
                          side_effect=replace_then_rename):
            with self.assertRaisesRegex(RuntimeError, "replaced.*quarantine"):
                directory_operations.remove_owned_directory(token)
        retained = self.isolated_entries()
        self.assertEqual(len(retained), 1)
        held = retained[0].lstat()
        self.assertEqual((held.st_dev, held.st_ino), replacement_identity[0])
        self.assertEqual(stat.S_IMODE(held.st_mode), SEALED_MODE)
        self.assertEqual((retained[0] / "keep.txt").read_text(), "replacement")
        self.assertTrue((self.root / "original").is_dir())

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

    def test_sealed_directory_of_another_user_is_retained_without_chmod(self):
        token = self.owned_tree()
        token.directory.chmod(SEALED_MODE)
        with patch.object(directory_operations.os, "getuid",
                          return_value=os.getuid() + 1):
            with self.assertRaisesRegex(RuntimeError, "not owned by the current user"):
                directory_operations.remove_owned_directory(token)
        retained = self.isolated_entries()
        self.assertEqual(len(retained), 1)
        self.assertEqual(stat.S_IMODE(retained[0].lstat().st_mode), SEALED_MODE)
        self.assertTrue((retained[0] / "views").is_dir())


if __name__ == "__main__":
    unittest.main()
