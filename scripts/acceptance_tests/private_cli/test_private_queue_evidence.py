"""Implementation methods for PrivateQueueEvidenceTests; collected once by the acceptance_tests discovery."""

import unittest
from scripts import acceptance_private_cli as private_cli


class PrivateQueueEvidenceTests(unittest.TestCase):
    def test_migration_label_lookup_rejects_duplicate_queue_rows(self):
        label = "delegate:" + "b" * 32 + ":" + "a" * 32
        snapshot = {"tasks": {
            "first": {"label": label, "status": {"Queued": {}}},
            "second": {"label": label, "status": {"Queued": {}}},
        }}
        with self.assertRaisesRegex(RuntimeError, "duplicate task label"):
            private_cli.task_row_by_label(snapshot, label)
