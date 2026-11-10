"""Implementation methods for PrivateQueueEvidenceTests; collected once by the acceptance package discovery."""

import unittest
import sys
from pathlib import Path


repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.private_cli import queue_policy as private_cli_queue_policy
from scripts.acceptance.private_cli import supervisor as supervisor_impl
from unittest.mock import patch


class PrivateQueueEvidenceTests(unittest.TestCase):
    def test_migration_label_lookup_rejects_duplicate_queue_rows(self):
        label = "delegate:" + "b" * 32 + ":" + "a" * 32
        snapshot = {"tasks": {
            "first": {"label": label, "status": {"Queued": {}}},
            "second": {"label": label, "status": {"Queued": {}}},
        }}
        with self.assertRaisesRegex(RuntimeError, "duplicate task label"):
            private_cli_queue_policy.task_row_by_label(snapshot, label)

    def test_wait_projection_is_bounded_and_never_contains_unselected_control_data(self):
        label = "delegate:" + "b" * 32 + ":" + "a" * 32
        row = {"id": 7, "label": label, "status": {"Running": {}}, "command": "PRIVATE_CANARY",
               "envs": {"SECRET": "PRIVATE_CANARY"}}
        cases = (
            ({"tasks": {"9": {**row, "label": "another"}}}, {"kind": "missing"}),
            ({"tasks": {"7": row}}, {"kind": "exact-label", "state": "Running", "queue_id": 7}),
            ({"tasks": {"7": row, "8": row}}, {"kind": "ambiguous"}),
            ({"tasks": {"7": {**row, "id": True, "status": {"PRIVATE_CANARY" * 1000: {}}}}},
             {"kind": "exact-label", "state": "unknown"}),
        )
        for snapshot, expected in cases:
            with self.subTest(expected=expected):
                self.assertEqual(private_cli_queue_policy.queue_wait_projection(snapshot, label), expected)

    def test_projection_failure_never_masks_the_original_wait_timeout(self):
        clock = [0.0]
        evidence: supervisor_impl.QueueWaitObservation = {}

        def sleep(_seconds):
            clock[0] += 30

        with patch.object(supervisor_impl, "pueue_snapshot", return_value={"tasks": {}}) as snapshot, \
                patch.object(supervisor_impl, "queue_wait_projection", side_effect=ValueError("PRIVATE_CANARY")), \
                patch.object(supervisor_impl.time, "monotonic", side_effect=lambda: clock[0]), \
                patch.object(supervisor_impl.time, "sleep", side_effect=sleep):
            with self.assertRaisesRegex(RuntimeError, "^timed out waiting for isolated pueue task state Done$"):
                supervisor_impl.wait_for_pueue_state("client", "config", {}, "label", "Done", capture_root=None,
                                                    observation=evidence)
        snapshot.assert_called_once()
        self.assertEqual(evidence, {
            "poll_count": 1, "elapsed_seconds": 30.0,
            "capture_error": {"stage": "queue-projection", "exception_type": "ValueError"},
        })

    def test_ambiguous_diagnostics_preserve_the_existing_successful_wait(self):
        row = {"id": 7, "label": "label", "status": {"Done": {}}}
        evidence: supervisor_impl.QueueWaitObservation = {}
        with patch.object(supervisor_impl, "pueue_snapshot", return_value={"tasks": {"7": row, "8": row}}) as snapshot:
            result = supervisor_impl.wait_for_pueue_state("client", "config", {}, "label", "Done", capture_root=None,
                                                        observation=evidence)
        self.assertIs(result, row)
        snapshot.assert_called_once()
        self.assertEqual(evidence["last"], {"kind": "ambiguous"})
        self.assertEqual(evidence["poll_count"], 1)
