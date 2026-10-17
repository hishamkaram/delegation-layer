"""Cleanup preparation releases its own pins without masking the primary error."""

import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.directories import journal
from scripts.delegate_acceptance.directories.operations import DirectoryOwner


class CleanupJournalPinsTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="cleanup-journal-pins-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.target = self.root / "target"
        owner = DirectoryOwner()
        token = owner.create("fixture", self.target, self.root)
        self.identity = journal.directory_identity_record(token)
        owner.release()
        (self.target / "marker").write_text("retained", encoding="utf-8")
        self.journal_path = self.root / "cleanup.json"
        self.adopted_pins = []
        original_adopt = DirectoryOwner.adopt_created

        def observe_adoption(owner, label, directory, parent):
            token = original_adopt(owner, label, directory, parent)
            self.adopted_pins.extend((token.parent_fd, token.directory_fd))
            return token

        observation = patch.object(DirectoryOwner, "adopt_created", observe_adoption)
        observation.start()
        self.addCleanup(observation.stop)

    def assert_pins_closed(self):
        self.assertEqual(len(self.adopted_pins), 2)
        for descriptor in self.adopted_pins:
            with self.assertRaises(OSError):
                os.fstat(descriptor)

    def test_missing_quarantine_parent_closes_target_pins(self):
        with self.assertRaisesRegex(RuntimeError, "cannot hold owned directory parent") as raised:
            journal.prepare_directory_cleanup(
                self.journal_path, "test cleanup", self.identity,
                self.root / "absent-quarantine", "a" * 64,
            )
        self.assertIsInstance(raised.exception.__cause__, FileNotFoundError)
        self.assert_pins_closed()
        self.assertEqual((self.target / "marker").read_text(encoding="utf-8"), "retained")
        self.assertFalse(self.journal_path.exists())

    def test_publication_failure_preserves_error_after_target_replacement(self):
        quarantine = self.root / "quarantine"
        quarantine.mkdir()
        moved = self.root / "original"
        failure = OSError("injected journal publication failure")

        def fail_publication(path, value):
            self.target.rename(moved)
            self.target.mkdir()
            (self.target / "replacement").write_text("retain", encoding="utf-8")
            raise failure

        with patch.object(journal, "write_json", side_effect=fail_publication):
            with self.assertRaises(OSError) as raised:
                journal.prepare_directory_cleanup(
                    self.journal_path, "test cleanup", self.identity, quarantine, "a" * 64,
                )
        self.assertIs(raised.exception, failure)
        self.assert_pins_closed()
        self.assertEqual((moved / "marker").read_text(encoding="utf-8"), "retained")
        self.assertEqual((self.target / "replacement").read_text(encoding="utf-8"), "retain")
        self.assertEqual(list(quarantine.iterdir()), [])
        self.assertFalse(self.journal_path.exists())
