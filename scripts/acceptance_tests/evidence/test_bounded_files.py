"""File identity and byte limits are checked independently of receipt policy."""

import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

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


if __name__ == "__main__":
    unittest.main()
