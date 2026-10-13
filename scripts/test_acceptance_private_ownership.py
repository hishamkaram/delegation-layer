"""Focused private CLI root ownership tests."""

from pathlib import Path
import tempfile
import unittest

import acceptance_private_cli as private_cli
from acceptance_supervisor_common import DirectoryOwner


class PrivateCliOwnershipTests(unittest.TestCase):
    def test_private_root_cleanup_requires_token_and_preserves_parent(self):
        with tempfile.TemporaryDirectory(prefix="private-cli-owner-") as directory:
            parent = Path(directory).resolve(strict=True)
            owner = DirectoryOwner()
            root_path = Path(tempfile.mkdtemp(
                prefix=private_cli.PRIVATE_ACCEPTANCE_PREFIX, dir=parent)).resolve(strict=True)
            root_token = owner.adopt_created(
                "private CLI acceptance root", root_path, parent)
            nested = owner.create("private CLI nested state", root_path / "nested", root_path)
            entries = owner.transfer()

            private_cli.remove_private_acceptance_root(root_token, entries)

            self.assertFalse(root_path.exists())
            self.assertTrue(parent.is_dir())
            self.assertFalse(nested.directory.exists())


if __name__ == "__main__":
    unittest.main()
