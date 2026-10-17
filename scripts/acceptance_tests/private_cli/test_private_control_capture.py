"""Implementation methods for PrivateControlCaptureTests; collected once by the acceptance_tests discovery."""

import errno
import json
import os
from pathlib import Path
import subprocess
import sys
import unittest
from unittest.mock import patch
from scripts import acceptance_private_cli as private_cli
from scripts.delegate_acceptance.private_cli import capture as capture_impl
from scripts.delegate_acceptance.private_cli import supervisor as supervisor_impl
from scripts.acceptance_tests.private_cli.fixtures import (
    set_up_private_capture_root,
)


class PrivateControlCaptureTests(unittest.TestCase):
    def setUp(self):
        super().setUp()
        set_up_private_capture_root(self)

    def test_pueue_snapshot_uses_shared_file_backed_capture(self):
        completed = subprocess.CompletedProcess([], 0, b'{"tasks": {}}\n', b"")
        metrics = {}
        with patch.object(capture_impl, "run_captured_command", return_value=completed) as capture:
            snapshot = private_cli.pueue_snapshot(
                Path("/fixture/pueue"), Path("/fixture/pueue.yml"), {"PATH": "/bin"},
                metrics=metrics, phase="supervisor-observation", capture_root=self.capture_root,
            )

        self.assertEqual(snapshot, {"tasks": {}})
        self.assertEqual(metrics["status_bytes"], len(b'{"tasks": {}}\n'))
        capture.assert_called_once_with(
            ["/fixture/pueue", "--config", "/fixture/pueue.yml", "status", "--json"],
            {"PATH": "/bin", "DELEGATE_ACCEPTANCE_PHASE": "supervisor-observation"},
            10,
            capture_root=self.capture_root,
        )

    def test_pueue_command_uses_shared_file_backed_capture(self):
        completed = subprocess.CompletedProcess([], 0, b"ok\n", b"")
        with patch.object(capture_impl, "run_captured_command", return_value=completed) as capture:
            output = private_cli.pueue_command(
                Path("/fixture/pueue"), Path("/fixture/pueue.yml"), {"PATH": "/bin"},
                ["shutdown"], timeout=7, phase="supervisor-shutdown",
                capture_root=self.capture_root,
            )

        self.assertEqual(output, "ok\n")
        capture.assert_called_once_with(
            ["/fixture/pueue", "--config", "/fixture/pueue.yml", "shutdown"],
            {"PATH": "/bin", "DELEGATE_ACCEPTANCE_PHASE": "supervisor-shutdown"},
            7,
            capture_root=self.capture_root,
        )

    def test_private_daemon_pid_fallback_uses_shared_file_backed_capture(self):
        completed = subprocess.CompletedProcess([], 1, b"", b"")
        with patch.object(private_cli.os, "getpgid", side_effect=ProcessLookupError), \
                patch.object(supervisor_impl, "shutdown_remaining", return_value=3.0), \
                patch.object(capture_impl, "run_captured_command", return_value=completed) as capture:
            self.assertTrue(private_cli.private_daemon_process_gone(
                123, 9.0, capture_root=self.capture_root,
            ))

        capture.assert_called_once_with(
            ["/bin/ps", "-p", "123", "-o", "pid="],
            {"PATH": "/usr/bin:/bin", "LC_ALL": "C"},
            3.0,
            capture_root=self.capture_root,
        )

    def test_file_capture_survives_soft_256_pressure_and_parallel_short_commands(self):
        environment = os.environ.copy()
        scripts = str(Path(__file__).resolve().parents[2])
        environment["PYTHONPATH"] = os.pathsep.join(
            item for item in (scripts, environment.get("PYTHONPATH", "")) if item
        )
        result = subprocess.run(
            [sys.executable, str(Path(__file__).with_name("capture_pressure_fixture.py"))],
            env=environment, capture_output=True, text=True, timeout=120, check=False,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        evidence = json.loads(result.stdout)
        self.assertEqual(evidence["soft_limit"], 256)
        self.assertEqual(evidence["pinned_fds"], 201)
        self.assertGreaterEqual(evidence["held_fd_count"], 250)
        self.assertEqual(evidence["pipe_probe_errno"], errno.EMFILE)
        self.assertEqual(evidence["snapshot"], {"tasks": {}})
        self.assertGreater(evidence["status_bytes"], 0)
        self.assertEqual(evidence["control_output"], "control-ok\n")
        self.assertEqual(evidence["parallel_startup_watchdog_seconds"], 30)
        self.assertEqual(evidence["parallel_child_watchdog_seconds"], 60)
        self.assertEqual(evidence["parallel_markers_before_release"], 16)
        self.assertEqual(evidence["parallel_pending_before_release"], 16)
        self.assertEqual(evidence["parallel_markers_after_release"], 16)
        self.assertEqual(evidence["parallel_exit_codes"], [0] * 16)
        self.assertEqual(evidence["parallel_outputs"], ["delegate-short-capture"] * 16)
