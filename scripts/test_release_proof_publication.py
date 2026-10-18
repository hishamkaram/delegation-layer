"""Proof bytes are complete before publication and never overwrite a winner."""

from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.evidence.files import write_bytes, write_json


class ReleaseProofPublicationTests(unittest.TestCase):
    def test_failed_prepublication_sync_leaves_no_partial_destination(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "proof.zip"
            with patch("scripts.delegate_acceptance.evidence.files.os.fsync", side_effect=OSError("fault")):
                with self.assertRaises(OSError):
                    write_bytes(path, b"complete-proof")
            self.assertFalse(path.exists())
            self.assertEqual(list(path.parent.iterdir()), [])
            write_bytes(path, b"complete-proof")
            self.assertEqual(path.read_bytes(), b"complete-proof")

    def test_create_once_preserves_existing_proof(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "proof.zip"
            write_bytes(path, b"original")
            with self.assertRaises(FileExistsError):
                write_bytes(path, b"other")
            self.assertEqual(path.read_bytes(), b"original")
            self.assertEqual(list(path.parent.iterdir()), [path])

    def test_json_keeps_its_encoding_and_replace_contract(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "receipt.json"
            write_json(path, {"value": 1})
            self.assertEqual(path.read_bytes(), b'{\n  "value": 1\n}\n')
            write_json(path, {"value": 2}, replace=True)
            self.assertEqual(path.read_bytes(), b'{\n  "value": 2\n}\n')


if __name__ == "__main__":
    unittest.main()
