"""Publication path confinement and descriptor regressions; no native processes."""
from pathlib import Path
import tempfile
import unittest

from acceptance_provider_common import AcceptanceFailure, read_outcome_payload
from acceptance_supervisor_common import sha


class PayloadTests(unittest.TestCase):
    def test_protocol_payloads_and_invalid_descriptors(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            for verdict, name in (("committed", "result.txt"), ("rejected", "publish.reject")):
                (directory / name).write_bytes(b"x")
                payload = {"basename": name, "length": 1, "sha256": sha(b"x")}
                outcome = {"verdict": verdict, "payload": payload}
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
