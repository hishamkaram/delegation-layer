"""Focused private CLI root ownership tests."""

from pathlib import Path
import tempfile
import unittest
from unittest import mock
import sys


repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.private_cli import configuration as private_cli_configuration
from scripts.acceptance.private_cli import root_cleanup as private_cli_root_cleanup

from scripts.acceptance.directories.operations import DirectoryOwner
import scripts.acceptance.directories.operations as directories


class PrivateCliOwnershipTests(unittest.TestCase):
    def test_private_root_cleanup_requires_token_and_preserves_parent(self):
        with tempfile.TemporaryDirectory(prefix="private-cli-owner-") as directory:
            parent = Path(directory).resolve(strict=True)
            owner = DirectoryOwner()
            root_path = Path(tempfile.mkdtemp(
                prefix=private_cli_configuration.PRIVATE_ACCEPTANCE_PREFIX, dir=parent)).resolve(strict=True)
            root_token = owner.adopt_created(
                "private CLI acceptance root", root_path, parent)
            nested = owner.create("private CLI nested state", root_path / "nested", root_path)
            entries = owner.transfer()

            private_cli_root_cleanup.remove_private_acceptance_root(root_token, entries)

            self.assertFalse(root_path.exists())
            self.assertTrue(parent.is_dir())
            self.assertFalse(nested.directory.exists())


class DirectoryReleaseOwnershipTests(unittest.TestCase):
    """Releasing pins consumes authority, keeps directories, and never loses a live token."""

    def setUp(self):
        self.parent_directory = tempfile.TemporaryDirectory(prefix="release-owner-")
        self.addCleanup(self.parent_directory.cleanup)
        self.parent = Path(self.parent_directory.name).resolve(strict=True)
        self.owner = DirectoryOwner()
        self.tokens = [self.owner.create(name, self.parent / name, self.parent)
                       for name in ("first", "second", "third")]
        self.addCleanup(self._consume_remaining)

    def _consume_remaining(self):
        directories.release_owned_directories(tuple(t for t in self.tokens if not t.consumed))

    def test_consumed_state_is_derived_without_touching_descriptors(self):
        token = self.tokens[0]
        self.assertFalse(token.consumed)
        token.consume()
        self.assertTrue(token.consumed)
        self.assertTrue(token.directory.is_dir())

    def test_release_consumes_every_token_in_reverse_order_and_keeps_directories(self):
        order = []
        real = directories.release_owned_directory

        def spy(token, *arguments):
            order.append(token.label)
            return real(token, *arguments)

        with mock.patch.object(directories, "release_owned_directory", side_effect=spy):
            directories.release_owned_directories(tuple(self.tokens))
        self.assertEqual(order, ["third", "second", "first"])
        self.assertTrue(all(token.consumed and token.directory.is_dir() for token in self.tokens))

    def test_release_error_aggregates_and_still_consumes_every_ordinary_token(self):
        middle = self.tokens[1].directory
        middle.rename(self.parent / "moved")
        middle.mkdir()
        with self.assertRaisesRegex(RuntimeError, "directory ownership release failed: second: .*replaced"):
            directories.release_owned_directories(tuple(self.tokens))
        self.assertTrue(all(token.consumed for token in self.tokens))

    def test_owner_release_keeps_strict_misuse_and_settles_after_a_handled_failure(self):
        self.tokens[2].directory.rename(self.parent / "moved")
        self.tokens[2].directory.mkdir()
        with self.assertRaisesRegex(RuntimeError, "directory ownership release failed: third"):
            self.owner.release()
        self.assertTrue(all(token.consumed for token in self.tokens))
        for operation in (self.owner.release, self.owner.transfer):
            with self.assertRaisesRegex(RuntimeError, "directory ownership was already transferred"):
                operation()

    def test_interrupted_owner_release_keeps_the_still_live_tokens_reachable(self):
        real = directories.release_owned_directory
        calls = []

        def interrupt_second(token, *arguments):
            calls.append(token.label)
            if len(calls) == 2:
                raise KeyboardInterrupt()
            return real(token, *arguments)

        with mock.patch.object(directories, "release_owned_directory", side_effect=interrupt_second):
            with self.assertRaises(KeyboardInterrupt):
                self.owner.release()
        live = self.owner.transfer()
        self.assertEqual([token.label for token in live], ["first", "second"])
        self.assertTrue(self.tokens[2].consumed and not any(token.consumed for token in live))


if __name__ == "__main__":
    unittest.main()
