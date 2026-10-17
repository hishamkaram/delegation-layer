"""Real local flock observations at the existing bootstrap-lock pathname."""

import errno
import fcntl
import os
from pathlib import Path
import unittest
from unittest.mock import patch

from scripts.acceptance_tests.private_cli.modern_fixtures import ModernControlFixture
from scripts.delegate_acceptance.private_cli.contention_locks import probe_generation_lease


class ModernGenerationLockTests(unittest.TestCase):
    def setUp(self):
        self.fixture = ModernControlFixture(self)
        self.lock = self.fixture.supervisor / "bootstrap.lock"

    def test_records_unlocked_shared_and_exclusive_modes_on_one_inode(self):
        initial = probe_generation_lease(self.lock, "unlocked")
        descriptor = os.open(self.lock, os.O_RDWR)
        try:
            for mode, operation in (("shared", fcntl.LOCK_SH), ("exclusive", fcntl.LOCK_EX)):
                fcntl.flock(descriptor, operation | fcntl.LOCK_NB)
                observation = probe_generation_lease(self.lock, mode)
                self.assertEqual((observation["device"], observation["inode"]), (initial["device"], initial["inode"]))
                self.assertEqual(observation["file_mode"], "0o600")
                fcntl.flock(descriptor, fcntl.LOCK_UN)
        finally:
            os.close(descriptor)
        self.assertTrue(probe_generation_lease(self.lock, "unlocked")["probe_exclusive_acquired"])

    def test_missing_and_symlink_locks_are_not_provisioned_or_followed(self):
        missing = self.fixture.supervisor / "missing.lock"
        with self.assertRaises(FileNotFoundError):
            probe_generation_lease(missing, "shared")
        self.assertFalse(missing.exists())
        alias = self.fixture.supervisor / "alias.lock"
        alias.symlink_to(self.lock)
        with self.assertRaises(OSError):
            probe_generation_lease(alias, "shared")

    def test_rejects_nonregular_lock_without_blocking(self):
        fifo = self.fixture.supervisor / "fifo.lock"
        os.mkfifo(fifo, 0o600)
        try:
            with self.assertRaisesRegex(RuntimeError, "unsafe"):
                probe_generation_lease(fifo, "shared")
        finally:
            fifo.unlink()

    def test_rejects_inode_replacement_and_unexpected_flock_errors(self):
        other = self.fixture.supervisor / "other.lock"
        other.touch(mode=0o600)
        identities = [self.lock.lstat(), other.lstat()]
        with patch.object(Path, "lstat", side_effect=identities), self.assertRaisesRegex(RuntimeError, "replaced"):
            probe_generation_lease(self.lock, "unlocked")
        with (
            patch(
                "scripts.delegate_acceptance.private_cli.contention_locks.fcntl.flock",
                side_effect=BlockingIOError(errno.EBADF, "bad descriptor"),
            ),
            self.assertRaises(BlockingIOError),
        ):
            probe_generation_lease(self.lock, "shared")
