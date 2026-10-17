"""Fault tests at the canonical directory, process, and task ledger owners."""

from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.directories import journal, operations as directories
from scripts.delegate_acceptance.evidence.files import read_json
from scripts.delegate_acceptance.processes import operations as processes
from scripts.delegate_acceptance.tasks import operations as tasks


class CompletedChild:
    """No subprocess: the fixture supplies an already observed child exit."""

    pid = 12345
    stdin = None

    def poll(self):
        return 0

    def wait(self):
        return 0


class CanonicalOwnerTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="foundation-owner-")
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name).resolve()

    def test_directory_publication_does_not_replace_a_racing_destination(self):
        target = self.root / "owned"
        original = directories._rename_noreplace

        def occupy_then_publish(source_fd, source_name, destination_fd, destination_name):
            directories.os.mkdir(destination_name, dir_fd=destination_fd)
            (target / "keep").write_text("replacement", encoding="utf-8")
            return original(source_fd, source_name, destination_fd, destination_name)

        owner = directories.DirectoryOwner()
        with patch.object(directories, "_rename_noreplace", side_effect=occupy_then_publish):
            with self.assertRaises(FileExistsError):
                owner.create("owned", target, self.root)
        self.assertEqual((target / "keep").read_text(), "replacement")
        self.assertEqual(list(self.root.glob(".delegate-create-*")), [])

    def test_journal_write_failure_retains_the_exact_target(self):
        owner = directories.DirectoryOwner()
        token = owner.create("target", self.root / "target", self.root)
        identity = journal.directory_identity_record(token)
        owner.release()
        quarantine = self.root / "quarantine"
        quarantine.mkdir()
        record = self.root / "cleanup.json"
        with patch.object(journal, "write_json", side_effect=OSError("journal disk full")):
            with self.assertRaisesRegex(OSError, "journal disk full"):
                journal.prepare_directory_cleanup(record, "cleanup", identity, quarantine, "a" * 64)
        self.assertTrue((self.root / "target").is_dir())
        self.assertFalse(record.exists())
        self.assertEqual(list(quarantine.iterdir()), [])

    def test_process_start_failure_rolls_back_only_its_capture_child(self):
        owner = directories.DirectoryOwner()
        captures = owner.create("captures", self.root / "processes", self.root)
        child = captures.directory / "child"
        with patch.object(processes.subprocess, "Popen", side_effect=OSError("start failed")):
            with self.assertRaisesRegex(OSError, "start failed"):
                processes.Process(child, [sys.executable, "-c", "pass"], self.root, {}, captures)
        self.assertFalse(child.exists())
        self.assertTrue(captures.directory.is_dir())
        owner.rollback()

    def test_started_receipt_failure_keeps_the_child_in_the_single_registry(self):
        owner = directories.DirectoryOwner()
        book = processes.Processes(self.root / "processes", owner=owner)
        original = processes.write_json

        def write(path, value, replace=False):
            if Path(path).name == "started.json":
                raise OSError("started receipt failed")
            return original(path, value, replace=replace)

        with patch.object(processes.subprocess, "Popen", return_value=CompletedChild()):
            with patch.object(processes, "write_json", side_effect=write):
                with self.assertRaisesRegex(OSError, "started receipt failed"):
                    book.start("short", [sys.executable, "-c", "pass"], self.root)
        self.assertEqual(len(book.entries), 1)
        self.assertIsInstance(book.entries[0], processes.Process)
        self.assertEqual(book.entries[0].wait(), 0)
        self.assertTrue(book.finished_naturally())
        owner.rollback()

    def test_admission_receipt_failure_keeps_the_original_ledger_authority(self):
        task = "a" * 32
        root_id = "b" * 32
        operation = tasks.NativeTaskOps(
            None, self.root / "delegate", self.root / "runner", self.root / "pueue",
            self.root, self.root / "state", self.root)
        operation.dispatch_attempts.add(task)
        response = {
            "admission": "admitted", "task_id": task, "root_id": root_id,
            "supervisor": {"matched": True, "numeric_task_id": 7},
        }
        with patch.object(tasks, "write_json", side_effect=OSError("receipt disk full")):
            with self.assertRaisesRegex(OSError, "receipt disk full"):
                operation.record_admission("fresh", task, response)
        self.assertEqual(operation.tasks, {"fresh": task})
        self.assertEqual(operation.numbers, {"fresh": 7})
        self.assertEqual(operation.labels, {"fresh": f"delegate:{root_id}:{task}"})
        self.assertEqual(operation.expected_results, {"fresh": "Success"})
        self.assertEqual(operation.dispatch_attempts, {task})

    def test_cleanup_journal_replays_to_the_same_complete_record(self):
        owner = directories.DirectoryOwner()
        token = owner.create("target", self.root / "target", self.root)
        (token.directory / "payload").write_text("payload", encoding="utf-8")
        identity = journal.directory_identity_record(token)
        owner.release()
        quarantine = self.root / "quarantine"
        quarantine.mkdir()
        record = self.root / "cleanup.json"
        journal.prepare_directory_cleanup(record, "cleanup", identity, quarantine, "a" * 64)
        first = journal.resume_directory_cleanup(record, "a" * 64, identity)
        second = journal.resume_directory_cleanup(record, "a" * 64, identity)
        self.assertEqual(first, second)
        self.assertEqual(read_json(record)["state"], "complete")
        self.assertFalse((self.root / "target").exists())
        self.assertEqual(list(quarantine.iterdir()), [])


if __name__ == "__main__":
    unittest.main()
