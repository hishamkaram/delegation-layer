"""Publication path confinement and descriptor regressions; no native processes."""
from pathlib import Path
import tempfile
import unittest
import sys


repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.tasks.publication import read_outcome_payload
from scripts.acceptance.evidence.digests import sha


class PayloadTests(unittest.TestCase):
    def test_protocol_payloads_and_invalid_descriptors(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            for verdict, name in (("committed", "result.txt"), ("rejected", "publish.reject")):
                (directory / name).write_bytes(b"x")
                payload = {"basename": name, "length": 1, "sha256": sha(b"x")}
                outcome: dict[str, object] = {"verdict": verdict, "payload": payload}
                self.assertEqual(read_outcome_payload(directory, outcome), b"x")
                for bad in ("../result.txt", "/tmp/result.txt", "other.txt", "raw/stdout"):
                    with self.subTest(basename=bad), self.assertRaises(AcceptanceFailure):
                        read_outcome_payload(directory, {**outcome, "payload": {**payload, "basename": bad}})
                for length in (True, -1, "1", 2):
                    with self.subTest(length=length), self.assertRaises(AcceptanceFailure):
                        read_outcome_payload(directory, {**outcome, "payload": {**payload, "length": length}})
                (directory / name).unlink()
                external = directory / "external"
                external.write_bytes(b"x")
                (directory / name).symlink_to(external)
                with self.assertRaises(AcceptanceFailure):
                    read_outcome_payload(directory, outcome)
