"""Implementation methods for PrivateCapturedProcessTests; collected once by the acceptance_tests discovery."""

import subprocess
import unittest
from scripts import acceptance_private_cli as private_cli


class PrivateCapturedProcessTests(unittest.TestCase):
    class Process:
        def __init__(self, responses, returncode=0, kill_error=None):
            self.responses = list(responses)
            self.returncode = returncode
            self.kill_error = kill_error
            self.killed = False

        def communicate(self, timeout):
            response = self.responses.pop(0)
            if isinstance(response, BaseException):
                raise response
            return response

        def kill(self):
            if self.kill_error is not None:
                raise self.kill_error
            self.killed = True

    def test_timeout_force_reaps_owned_fixture_process_with_diagnostics(self):
        process = self.Process([
            subprocess.TimeoutExpired(["fixture"], 45, output="partial stdout", stderr="partial stderr"),
            ("final stdout", "final stderr"),
        ], returncode=-9)

        stdout, stderr, reaped, error = private_cli.reap_captured_process(process, 45)

        self.assertTrue(reaped)
        self.assertTrue(process.killed)
        self.assertEqual((stdout, stderr), ("final stdout", "final stderr"))
        self.assertIn("exceeded its 45s observation bound", str(error))
        self.assertIn("final stdout", str(error))

    def test_unreapable_fixture_process_is_reported_for_root_retention(self):
        process = self.Process([
            subprocess.TimeoutExpired(["fixture"], 45, output="partial", stderr="blocked"),
        ], returncode=None, kill_error=OSError("kill failed"))

        _, _, reaped, error = private_cli.reap_captured_process(process, 45)

        self.assertFalse(reaped)
        self.assertIn("could not be reaped", str(error))
        self.assertIn("partial", str(error))
