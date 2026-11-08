"""Implementation methods for PrivateQueueEvidenceTests; collected once by the acceptance package discovery."""

import unittest
import sys
from pathlib import Path


repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.private_cli import queue_policy as private_cli_queue_policy


class PrivateQueueEvidenceTests(unittest.TestCase):
    def test_migration_label_lookup_rejects_duplicate_queue_rows(self):
        label = "delegate:" + "b" * 32 + ":" + "a" * 32
        snapshot = {"tasks": {
            "first": {"label": label, "status": {"Queued": {}}},
            "second": {"label": label, "status": {"Queued": {}}},
        }}
        with self.assertRaisesRegex(RuntimeError, "duplicate task label"):
            private_cli_queue_policy.task_row_by_label(snapshot, label)
