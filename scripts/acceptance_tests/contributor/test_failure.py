"""Failure cleanup preserves uncertainty and never publishes success."""

import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock

from scripts import acceptance_contributor as gate


class ContributorFailureTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="contributor-failure-")
        self.addCleanup(self.temporary.cleanup)
        self.owner = object.__new__(gate.ContributorAcceptance)
        self.owner.output = Path(self.temporary.name)
        self.owner.base = self.owner.output / "not-created"
        self.owner.tasks = {}
        self.owner.closed = False
        self.owner.daemon = None
        self.owner.processes = Mock()
        self.owner.processes.drain.return_value = []
        self.original = ValueError("fixture setup failed")
        self.owner.setup = Mock(side_effect=self.original)
        self.owner.finalize = Mock(return_value=True)

    def test_setup_failure_cleans_owned_artifacts_and_reraises_original(self):
        with self.assertRaises(ValueError) as captured:
            self.owner.run()
        self.assertIs(captured.exception, self.original)
        self.owner.finalize.assert_called_once_with(False)
        cleanup = json.loads((self.owner.output / "failure-cleanup.json").read_text())
        self.assertEqual(cleanup["status"], "completed")
        self.assertTrue(cleanup["natural_daemon_shutdown"])
        self.assertTrue(cleanup["artifact_finalization_completed"])
        self.assertFalse((self.owner.output / "success.json").exists())

    def test_unknown_process_drain_remains_a_failed_cleanup(self):
        self.owner.processes.drain.side_effect = RuntimeError("unknown child lifetime")
        with self.assertRaisesRegex(RuntimeError, "contributor acceptance cleanup failed") as captured:
            self.owner.run()
        self.assertIs(captured.exception.__cause__, self.original)
        cleanup = json.loads((self.owner.output / "failure-cleanup.json").read_text())
        self.assertEqual(cleanup["status"], "failed")
        self.assertFalse(cleanup["natural_daemon_shutdown"])
        self.assertEqual(cleanup["signals_sent"], 0)
        self.assertFalse((self.owner.output / "success.json").exists())


if __name__ == "__main__":
    unittest.main()
