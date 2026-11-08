"""Actorless invocation records keep selected paths and input evidence without program reads."""

import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.evidence.digests import sha
from scripts.acceptance.evidence.files import read_json
from scripts.acceptance.processes import operations as processes


class CompletedInvocationChild:
    """A supplied completed handle; no operating-system child is created."""

    pid = 4242
    stdin = None

    def poll(self):
        return 0

    def wait(self):
        return 0


class ProcessInvocationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="process-invocation-contract-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.program = self.root / "original-program"
        self.program.write_text("original executable fixture source; never executed\n")
        self.program.chmod(0o700)
        self.selected = self.root / "selected-program"
        self.selected.symlink_to(self.program.name)
        self.owner = DirectoryOwner()
        self.addCleanup(self.owner.rollback)
        self.book = processes.Processes(self.root / "captures", owner=self.owner)
        original_open = Path.open
        original_digest = processes.digest

        def read_guard(path, mode="r", *args, **kwargs):
            if "r" in mode:
                self.assertNotIn(path, (self.program, self.selected), "installed executable contents were opened")
            return original_open(path, mode, *args, **kwargs)

        def digest_guard(path):
            self.assertNotIn(Path(path), (self.program, self.selected), "installed executable digest was requested")
            return original_digest(path)

        self.enterContext(patch.object(Path, "open", read_guard))
        self.enterContext(patch.object(processes, "digest", side_effect=digest_guard))

    def invocation_before_spawn(self, *, stdin=None, keys_only=False):
        records = []
        failure = RuntimeError("record intercepted before spawn")
        self.book.environment_keys_only = keys_only
        self.book.environment = {"B": "private-value", "A": "first"}
        argv = [str(self.selected), "--fixture", "value"]

        def intercept(path, value, **_options):
            self.assertEqual(Path(path).name, "invocation.json")
            records.append(value)
            raise failure

        with patch.object(processes, "write_json", side_effect=intercept), \
                patch.object(processes.subprocess, "Popen") as start:
            with self.assertRaisesRegex(RuntimeError, "record intercepted before spawn") as raised:
                self.book.start("intercepted", argv, self.root, stdin=stdin)
        self.assertIs(raised.exception, failure)
        start.assert_not_called()
        self.assertEqual(self.book.entries, [])
        self.assertEqual(list(self.book.directory.iterdir()), [])
        self.assertEqual(len(records), 1)
        return records[0], argv

    def test_selected_path_argv_cwd_and_devnull_are_recorded_without_executable_hash(self):
        invocation, argv = self.invocation_before_spawn()
        self.assertEqual(invocation, {
            "argv": argv, "cwd": str(self.root), "executable": str(self.selected),
            "stdin": "DEVNULL", "stdin_bytes": 0, "stdin_sha256": None, "shell": False,
            "environment_sha256": sha(json.dumps(self.book.environment, sort_keys=True).encode()),
        })
        self.assertNotIn("executable_sha256", invocation)

    def test_finite_stdin_digest_is_preserved_and_empty_pipe_is_distinct_from_devnull(self):
        for data in (b"finite original fixture input\n", b""):
            with self.subTest(bytes=len(data)):
                invocation, _argv = self.invocation_before_spawn(stdin=data)
                self.assertEqual((invocation["stdin"], invocation["stdin_bytes"], invocation["stdin_sha256"]),
                                 ("PIPE", len(data), sha(data)))
                self.assertNotIn("executable_sha256", invocation)

    def test_keys_only_environment_has_no_values_or_value_digest(self):
        invocation, _argv = self.invocation_before_spawn(keys_only=True)
        self.assertEqual(invocation["environment_keys"], ["A", "B"])
        self.assertIs(invocation["environment_values_in_record"], False)
        self.assertIs(invocation["harness_environment_preserved"], True)
        self.assertNotIn("environment_sha256", invocation)
        self.assertNotIn("private-value", json.dumps(invocation))

    def test_existing_capture_and_wait_records_share_the_selected_invocation_without_new_owners(self):
        argv = [str(self.selected), "--finite-fixture"]
        with patch.object(processes.subprocess, "Popen", return_value=CompletedInvocationChild()) as start:
            process = self.book.run("completed", argv, self.root)
        self.assertEqual(start.call_args.args[0], argv)
        invocation = read_json(process.directory / "invocation.json")
        started = read_json(process.directory / "started.json")
        completed = read_json(process.directory / "completed.json")
        self.assertEqual(invocation["executable"], str(self.selected))
        self.assertNotIn("executable_sha256", invocation)
        self.assertEqual(started["pid"], 4242)
        self.assertEqual(completed["exit_code"], 0)
        self.assertTrue(completed["natural_wait"])
        self.assertTrue(process.stdout.closed and process.stderr.closed)
        self.assertTrue(self.book.finished_naturally())


if __name__ == "__main__":
    unittest.main()
