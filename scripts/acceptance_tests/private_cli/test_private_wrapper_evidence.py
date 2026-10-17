"""Implementation methods for PrivateWrapperEvidenceTests; collected once by the acceptance_tests discovery."""

from pathlib import Path
import tempfile
import unittest
from scripts import acceptance_private_cli as private_cli


class PrivateWrapperEvidenceTests(unittest.TestCase):
    def test_wrapper_calls_are_phase_role_bound_and_compact(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "calls"
            path.write_text(
                "managed-runner\tclient\t--config /owned/pueue.yml start --group default\n"
                "ordinary-dispatch\tclient\t--config /owned/pueue.yml add --print-task-id\n"
            )

            records = private_cli.wrapper_call_records(path, "client", "upgraded")

            self.assertEqual(records[0]["phase"], "managed-runner")
            self.assertEqual(records[0]["role"], "client")
            self.assertEqual(records[0]["command"], "start")
            self.assertNotIn("arguments", records[0])
            self.assertEqual(records[0]["arguments_bytes"],
                             len("--config /owned/pueue.yml start --group default"))
            with self.assertRaisesRegex(RuntimeError, "startup control command"):
                private_cli.require_no_startup_control_calls(records)

    def test_private_root_deletion_requires_the_exact_owned_parent_child(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve(strict=True)
            parent = root / "parent"
            owned = parent / (private_cli.PRIVATE_ACCEPTANCE_PREFIX + "test")
            owner = private_cli.DirectoryOwner()
            owned.mkdir(parents=True)
            token = owner.adopt_created("private root", owned, parent)
            entries = owner.transfer()
            private_cli.remove_private_acceptance_root(token, entries)
            self.assertFalse(owned.exists())
