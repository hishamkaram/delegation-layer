"""Implementation methods for TaskStatusReceiptTests; collected once by the acceptance_tests discovery."""

import hashlib
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
from scripts import acceptance_private_cli as private_cli
from scripts.delegate_acceptance.private_cli import commands as commands_impl
from scripts.acceptance_tests.private_cli.fixtures import (
    set_up_private_capture_root,
)


class TaskStatusReceiptTests(unittest.TestCase):
    def setUp(self):
        super().setUp()
        set_up_private_capture_root(self)

    def test_status_response_is_retained_before_identity_validation(self):
        for returned_id in ("a" * 32, "b" * 32):
            with self.subTest(returned_id=returned_id), \
                    tempfile.TemporaryDirectory(prefix="delegate-status-receipt-") as directory:
                receipt = Path(directory) / "status-000.json"
                response = {
                    "command": "status", "task_id": returned_id,
                    "root_id": "r" * 32, "liveness": "running",
                }
                stdout = (json.dumps(response) + "\n").encode()
                completed = subprocess.CompletedProcess([], 0, stdout, b"")
                with patch.object(commands_impl, "run_captured_command", return_value=completed) as command:
                    if returned_id == "a" * 32:
                        observed = private_cli.read_created_task_status(
                            Path("/fixture/delegate"), Path(directory), {}, "a" * 32, receipt,
                            capture_root=self.capture_root,
                        )
                        self.assertEqual(observed, response)
                    else:
                        with self.assertRaisesRegex(RuntimeError, "different task"):
                            private_cli.read_created_task_status(
                                Path("/fixture/delegate"), Path(directory), {}, "a" * 32, receipt,
                                capture_root=self.capture_root,
                            )
                recorded = json.loads(receipt.read_text())
                self.assertEqual(recorded["response"], response)
                self.assertEqual(recorded["stdout_sha256"], hashlib.sha256(stdout).hexdigest())
                self.assertEqual(recorded["exit_code"], 0)
                self.assertEqual(command.call_count, 1)

    def test_other_commands_do_not_capture_response_bodies_by_default(self):
        with tempfile.TemporaryDirectory(prefix="delegate-control-receipt-") as directory:
            receipt = Path(directory) / "command.json"
            completed = subprocess.CompletedProcess([], 0, b'{"fixture":"control"}\n', b"")
            with patch.object(commands_impl, "run_captured_command", return_value=completed):
                private_cli.run_json(
                    Path("/fixture/delegate"), {}, ["providers", "--json"], receipt=receipt,
                    capture_root=self.capture_root,
                )
            self.assertNotIn("response", json.loads(receipt.read_text()))
