"""Directory fixture cleanup closes pins without changing tested artifacts."""

import os
from pathlib import Path
import tempfile
import unittest

from scripts.acceptance_tests.resources import DirectoryPinOwner, track_directory_pins
from scripts.delegate_acceptance.directories.operations import DirectoryOwner


class DirectoryPinFixtureTests(unittest.TestCase):
    def test_adopted_tokens_are_closed_even_without_a_transfer(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            directory = root / "fixture"
            directory.mkdir()
            case = unittest.TestCase()
            owner = DirectoryPinOwner(case)
            token = owner.adopt_created("fixture", directory, root)
            descriptors = (token.parent_fd, token.directory_fd)
            self.assertTrue(case.doCleanups())
            self.assertTrue(directory.is_dir())
            for descriptor in descriptors:
                with self.assertRaises(OSError):
                    os.fstat(descriptor)

    def test_transferred_tokens_keep_their_identity_until_test_cleanup(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            case = unittest.TestCase()
            owner = DirectoryPinOwner(case)
            token = owner.create("fixture", root / "fixture", root)
            descriptors = (token.parent_fd, token.directory_fd)
            self.assertEqual(owner.transfer(), (token,))
            for descriptor in descriptors:
                os.fstat(descriptor)
            self.assertTrue(case.doCleanups())
            self.assertTrue(token.directory.is_dir())
            for descriptor in descriptors:
                with self.assertRaises(OSError):
                    os.fstat(descriptor)

    def test_closes_pins_after_assertions_without_removing_the_directory(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            owner = DirectoryOwner()
            token = owner.create("fixture", root / "fixture", root)
            descriptors = (token.parent_fd, token.directory_fd)
            case = unittest.TestCase()
            track_directory_pins(case, (token,))
            (token.directory / "evidence").write_text("retained")
            for descriptor in descriptors:
                os.fstat(descriptor)
            self.assertTrue(case.doCleanups())
            self.assertEqual((token.directory / "evidence").read_text(), "retained")
            for descriptor in descriptors:
                with self.assertRaises(OSError):
                    os.fstat(descriptor)

    def test_already_consumed_tokens_cannot_close_another_live_authority(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            owner = DirectoryOwner()
            original = owner.create("first", root / "first", root)
            case = unittest.TestCase()
            track_directory_pins(case, (original, original))
            original.consume()
            replacement = owner.create("second", root / "second", root)
            try:
                self.assertTrue(case.doCleanups())
                os.fstat(replacement.parent_fd)
                os.fstat(replacement.directory_fd)
            finally:
                replacement.consume()

    def test_closes_pins_even_after_the_temporary_path_was_removed(self):
        case = unittest.TestCase()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            owner = DirectoryOwner()
            token = owner.create("fixture", root / "fixture", root)
            descriptors = (token.parent_fd, token.directory_fd)
            track_directory_pins(case, (token,))
        self.assertFalse(token.directory.exists())
        self.assertTrue(case.doCleanups())
        for descriptor in descriptors:
            with self.assertRaises(OSError):
                os.fstat(descriptor)
