"""Native session forwarding through the shared acceptance process owner."""

import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.processes.operations import Processes


CHILD = '''import json
import os
import sys
print(json.dumps({"pid": os.getpid(), "sid": os.getsid(0), "stdin_eof": sys.stdin.read() == ""}))
print("finite native session fixture", file=sys.stderr)
'''


class SchedulerHostProcessTests(unittest.TestCase):
    def test_native_session_forwarding_preserves_exact_wait_and_owned_capture(self):
        owner = DirectoryOwner()
        parent = Path(tempfile.mkdtemp(prefix="scheduler-process-native-")).resolve(strict=True)
        owner.adopt_created("native process test", parent, parent.parent)
        processes = Processes(parent / "captures", environment={}, owner=owner, environment_keys_only=True)
        try:
            executable = Path(sys.executable).resolve(strict=True)
            for independent in (False, True):
                with self.subTest(start_new_session=independent):
                    if independent:
                        child = processes.start("independent", [executable, "-c", CHILD], parent, start_new_session=True)
                    else:
                        child = processes.start("inherited", [executable, "-c", CHILD], parent)
                    self.assertIs(processes.entries[-1], child)
                    with patch.object(child.process, "wait", wraps=child.process.wait) as exact_wait:
                        self.assertEqual(child.wait(timeout=5), 0)
                        self.assertEqual(child.wait(timeout=5), 0)
                    exact_wait.assert_called_once_with()
                    observed = child.json()
                    self.assertEqual(observed["pid"], child.pid)
                    self.assertEqual(observed["sid"], child.pid if independent else os.getsid(0))
                    self.assertIs(observed["stdin_eof"], True)
                    self.assertTrue(child.stdout.closed and child.stderr.closed)
                    self.assertEqual(child.stderr_path.read_bytes(), b"finite native session fixture\n")
                    completed = json.loads((child.directory / "completed.json").read_text())
                    self.assertEqual(completed["pid"], child.pid)
                    self.assertTrue(completed["natural_wait"])
                    self.assertEqual(completed["exit_code"], 0)
                    self.assertEqual(completed["stdout_bytes"], child.stdout_path.stat().st_size)
            self.assertEqual(processes.pending(), [])
            self.assertTrue(processes.finished_naturally())
        finally:
            if processes.drain(timeout=5):
                owner.release()
                raise RuntimeError("native process closure uncertain; retained " + str(parent))
            owner.rollback()
        self.assertFalse(parent.exists())


if __name__ == "__main__":
    unittest.main()
