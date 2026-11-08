"""Focused identity-token ownership tests for acceptance fixtures."""

from pathlib import Path
import os
import stat
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch


repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.evidence import files as evidence_files

from scripts.acceptance.directories import finalization as directory_finalization
from scripts.acceptance.directories import journal as directory_journal
from scripts.acceptance.directories import operations as directory_operations
from scripts.acceptance.processes import operations as process_operations
from scripts.acceptance.testing.resources import track_directory_pins


class DirectoryOwnershipTests(unittest.TestCase):
    def _prepared_cleanup(self, root: Path, name: str = "owned"):
        target = root / name
        quarantine_parent = root / "quarantine"
        quarantine_parent.mkdir()
        owner = directory_operations.DirectoryOwner()
        token = owner.create("owned", target, root)
        (target / "nested").mkdir()
        (target / "nested" / "payload.txt").write_text("payload", encoding="utf-8")
        identity = directory_journal.directory_identity_record(token)
        owner.release()
        journal = root / f"{name}.cleanup.json"
        authorization = "a" * 64
        directory_journal.prepare_directory_cleanup(
            journal, "test cleanup", identity, quarantine_parent, authorization,
        )
        return target, journal, authorization, identity

    def test_cleanup_preparation_has_no_unjournaled_filesystem_side_effect(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-cleanup-prepare-") as directory:
            root = Path(directory)
            target = root / "owned"
            quarantine_parent = root / "quarantine"
            quarantine_parent.mkdir()
            owner = directory_operations.DirectoryOwner()
            token = owner.create("owned", target, root)
            (target / "keep.txt").write_text("payload", encoding="utf-8")
            identity = directory_journal.directory_identity_record(token)
            owner.release()
            journal = root / "cleanup.json"

            with patch.object(evidence_files, "_rename_noreplace", side_effect=OSError("journal publish")), \
                    self.assertRaisesRegex(OSError, "journal publish"):
                directory_journal.prepare_directory_cleanup(
                    journal, "test cleanup", identity, quarantine_parent, "b" * 64,
                )
            self.assertEqual((target / "keep.txt").read_text(), "payload")
            self.assertEqual(list(quarantine_parent.iterdir()), [])
            self.assertFalse(journal.exists())
            self.assertEqual(list(root.glob(".cleanup.json.*.staging")), [])
            prepared = directory_journal.prepare_directory_cleanup(
                journal, "test cleanup", identity, quarantine_parent, "b" * 64,
            )
            self.assertEqual(prepared["state"], "prepared")

    def test_cleanup_retry_syncs_visible_journal_before_target_isolation(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-cleanup-durability-") as directory:
            root = Path(directory)
            source_parent = root / "source"
            quarantine_parent = root / "quarantine"
            journal_parent = root / "journals"
            source_parent.mkdir()
            quarantine_parent.mkdir()
            journal_parent.mkdir()
            target = source_parent / "owned"
            owner = directory_operations.DirectoryOwner()
            token = owner.create("owned", target, source_parent)
            (target / "payload.txt").write_text("payload", encoding="utf-8")
            identity = directory_journal.directory_identity_record(token)
            owner.release()
            journal = journal_parent / "cleanup.json"
            authorization = "c" * 64
            journal_parent_identity = (
                journal_parent.stat().st_dev, journal_parent.stat().st_ino,
            )
            original_fsync = os.fsync
            failed = False

            def fail_first_journal_parent_sync(descriptor):
                nonlocal failed
                opened = os.fstat(descriptor)
                if (not failed and journal.exists()
                        and (opened.st_dev, opened.st_ino) == journal_parent_identity):
                    failed = True
                    raise OSError("simulated journal directory sync failure")
                return original_fsync(descriptor)

            with patch.object(directory_journal.os, "fsync", side_effect=fail_first_journal_parent_sync), \
                    self.assertRaisesRegex(OSError, "journal directory sync failure"):
                directory_journal.prepare_directory_cleanup(
                    journal, "test cleanup", identity, quarantine_parent, authorization,
                )
            self.assertTrue(journal.is_file())
            self.assertTrue((target / "payload.txt").is_file())

            events = []
            original_rename = directory_operations._rename_noreplace

            def record_sync(descriptor):
                opened = os.fstat(descriptor)
                opened_identity = (opened.st_dev, opened.st_ino)
                if opened_identity == journal_parent_identity:
                    events.append("journal-parent-synced")
                elif journal.exists():
                    journal_metadata = journal.stat()
                    if opened_identity == (journal_metadata.st_dev, journal_metadata.st_ino):
                        events.append("journal-file-synced")
                return original_fsync(descriptor)

            def record_rename(source_fd, source_name, destination_fd, destination_name):
                if source_name == target.name:
                    events.append("target-isolated")
                return original_rename(
                    source_fd, source_name, destination_fd, destination_name,
                )

            with patch.object(directory_journal.os, "fsync", side_effect=record_sync), \
                    patch.object(directory_journal, "_rename_noreplace", side_effect=record_rename):
                directory_journal.prepare_directory_cleanup(
                    journal, "test cleanup", identity, quarantine_parent, authorization,
                )
                completed = directory_journal.resume_directory_cleanup(
                    journal, authorization, identity,
                )

            self.assertEqual(completed["state"], "complete")
            self.assertLess(events.index("journal-file-synced"),
                            events.index("journal-parent-synced"))
            self.assertLess(events.index("journal-parent-synced"),
                            events.index("target-isolated"))
            self.assertFalse(target.exists())

    def test_cleanup_journal_recovers_every_post_side_effect_crash_window(self):
        transitions = {
            "isolated": "prepared",
            "contents-removed": "isolated",
            "complete": "contents-removed",
        }
        for interrupted_state, durable_state in transitions.items():
            with self.subTest(state=interrupted_state), tempfile.TemporaryDirectory(
                    prefix="acceptance-cleanup-replay-") as directory:
                root = Path(directory)
                target, journal, authorization, identity = self._prepared_cleanup(root)
                original = evidence_files.write_json
                interrupted = False

                def interrupt_transition(path, value, replace=False):
                    nonlocal interrupted
                    if (not interrupted and replace
                            and isinstance(value, dict)
                            and value.get("state") == interrupted_state):
                        interrupted = True
                        raise OSError("simulated journal interruption")
                    return original(path, value, replace=replace)

                with patch.object(directory_journal, "write_json", side_effect=interrupt_transition), \
                        self.assertRaisesRegex(OSError, "simulated journal interruption"):
                    directory_journal.resume_directory_cleanup(journal, authorization, identity)
                self.assertEqual(
                    directory_journal.read_directory_cleanup(journal, authorization)["state"],
                    durable_state,
                )
                completed = directory_journal.resume_directory_cleanup(journal, authorization, identity)
                self.assertEqual(completed["state"], "complete")
                self.assertFalse(target.exists())
                self.assertFalse(Path(completed["quarantine"]["path"]).exists())

    def test_isolation_replay_syncs_both_recorded_parents(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-cleanup-sync-") as directory:
            root = Path(directory)
            _, journal, authorization, identity = self._prepared_cleanup(root)
            original_write = evidence_files.write_json
            interrupted = False

            def interrupt_isolated(path, value, replace=False):
                nonlocal interrupted
                if (not interrupted and replace and isinstance(value, dict)
                        and value.get("state") == "isolated"):
                    interrupted = True
                    raise OSError("isolation journal interruption")
                return original_write(path, value, replace=replace)

            with patch.object(directory_journal, "write_json", side_effect=interrupt_isolated), \
                    self.assertRaisesRegex(OSError, "isolation journal interruption"):
                directory_journal.resume_directory_cleanup(journal, authorization, identity)
            prepared = directory_journal.read_directory_cleanup(journal, authorization)
            synced = set()
            original_sync = os.fsync

            def record_sync(descriptor):
                metadata = os.fstat(descriptor)
                synced.add((metadata.st_dev, metadata.st_ino))
                return original_sync(descriptor)

            with patch.object(directory_journal.os, "fsync", side_effect=record_sync):
                directory_journal.resume_directory_cleanup(journal, authorization, identity)
            self.assertIn(
                (prepared["target"]["parent_device"], prepared["target"]["parent_inode"]),
                synced,
            )
            self.assertIn(
                (prepared["quarantine"]["parent_device"],
                 prepared["quarantine"]["parent_inode"]),
                synced,
            )

    def test_cleanup_journal_rejects_a_quarantine_bound_to_another_inode(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-cleanup-binding-") as directory:
            root = Path(directory)
            target, journal, authorization, identity = self._prepared_cleanup(root)
            unrelated = root / "unrelated"
            unrelated.mkdir()
            (unrelated / "keep.txt").write_text("retain", encoding="utf-8")
            value = evidence_files.read_json(journal)
            unrelated_stat = unrelated.stat()
            value["quarantine"]["device"] = unrelated_stat.st_dev
            value["quarantine"]["inode"] = unrelated_stat.st_ino
            evidence_files.write_json(journal, value, replace=True)

            with self.assertRaisesRegex(RuntimeError, "not bound to its target"):
                directory_journal.resume_directory_cleanup(journal, authorization, identity)
            self.assertTrue((target / "nested" / "payload.txt").is_file())
            self.assertEqual((unrelated / "keep.txt").read_text(), "retain")

    def test_cleanup_resume_rejects_a_journal_rebound_to_another_target(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-cleanup-rebound-") as directory:
            root = Path(directory)
            target, journal, authorization, expected_identity = self._prepared_cleanup(root)
            unrelated = root / "unrelated"
            unrelated.mkdir()
            marker = unrelated / "keep.txt"
            marker.write_text("retain", encoding="utf-8")
            owner = directory_operations.DirectoryOwner()
            token = owner.adopt_created("unrelated", unrelated, root)
            unrelated_identity = directory_journal.directory_identity_record(token)
            owner.release()
            value = evidence_files.read_json(journal)
            value["target"] = unrelated_identity
            value["quarantine"]["device"] = unrelated_identity["device"]
            value["quarantine"]["inode"] = unrelated_identity["inode"]
            evidence_files.write_json(journal, value, replace=True)

            with self.assertRaisesRegex(RuntimeError, "cleanup target identity changed"):
                directory_journal.resume_directory_cleanup(
                    journal, authorization, expected_identity,
                )
            self.assertTrue((target / "nested" / "payload.txt").is_file())
            self.assertEqual(marker.read_text(encoding="utf-8"), "retain")

    def test_cleanup_replay_retains_a_replacement_at_the_original_path(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-cleanup-replacement-") as directory:
            root = Path(directory)
            target, journal, authorization, identity = self._prepared_cleanup(root)
            with patch.object(directory_journal, "_cleanup_empty", side_effect=RuntimeError("pause")), \
                    self.assertRaisesRegex(RuntimeError, "pause"):
                directory_journal.resume_directory_cleanup(journal, authorization, identity)
            record = directory_journal.read_directory_cleanup(journal, authorization)
            self.assertEqual(record["state"], "isolated")
            target.mkdir()
            marker = target / "replacement.txt"
            marker.write_text("retain", encoding="utf-8")

            with self.assertRaisesRegex(RuntimeError, "occupied after isolation"):
                directory_journal.resume_directory_cleanup(journal, authorization, identity)
            self.assertEqual(marker.read_text(), "retain")
            self.assertTrue((Path(record["quarantine"]["path"]) / "nested" / "payload.txt").is_file())

    def test_cleanup_replay_retains_a_replaced_quarantine(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-cleanup-quarantine-") as directory:
            root = Path(directory)
            _, journal, authorization, identity = self._prepared_cleanup(root)
            with patch.object(directory_journal, "_cleanup_empty", side_effect=RuntimeError("pause")), \
                    self.assertRaisesRegex(RuntimeError, "pause"):
                directory_journal.resume_directory_cleanup(journal, authorization, identity)
            record = directory_journal.read_directory_cleanup(journal, authorization)
            quarantine = Path(record["quarantine"]["path"])
            original = root / "isolated-original"
            quarantine.rename(original)
            quarantine.mkdir()
            marker = quarantine / "replacement.txt"
            marker.write_text("retain", encoding="utf-8")

            with self.assertRaisesRegex(RuntimeError, "quarantine identity changed"):
                directory_journal.resume_directory_cleanup(journal, authorization, identity)
            self.assertEqual(marker.read_text(), "retain")
            self.assertTrue((original / "nested" / "payload.txt").is_file())

    def test_cleanup_journal_serializes_concurrent_resumers(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-cleanup-concurrent-") as directory:
            root = Path(directory)
            target, journal, authorization, identity = self._prepared_cleanup(root)
            entered = threading.Event()
            proceed = threading.Event()
            original = directory_journal._cleanup_empty
            results = []
            errors = []

            def pause_once(value):
                if not entered.is_set():
                    entered.set()
                    if not proceed.wait(5):
                        raise RuntimeError("concurrent cleanup fixture timed out")
                return original(value)

            def resume():
                try:
                    results.append(directory_journal.resume_directory_cleanup(
                        journal, authorization, identity,
                    ))
                except BaseException as error:  # Preserve thread errors for the assertion.
                    errors.append(error)

            with patch.object(directory_journal, "_cleanup_empty", side_effect=pause_once):
                first = threading.Thread(target=resume)
                second = threading.Thread(target=resume)
                started = []
                try:
                    first.start()
                    started.append(first)
                    self.assertTrue(entered.wait(5))
                    second.start()
                    started.append(second)
                    self.assertTrue(second.is_alive())
                finally:  # Release, then join every started worker before the patch and temporary root exit.
                    proceed.set()
                    for worker in started:
                        worker.join()

            self.assertEqual(errors, [])
            self.assertEqual([result["state"] for result in results], ["complete", "complete"])
            self.assertFalse(target.exists())

    def test_nested_creation_transfers_and_rolls_back_in_reverse_order(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            owner = directory_operations.DirectoryOwner()
            deepest = owner.create("nested", root / "a" / "b" / "c", root, parents=True)
            entries = owner.transfer()
            self.assertEqual(
                [entry.directory for entry in entries],
                [root / "a", root / "a" / "b", root / "a" / "b" / "c"],
            )
            removed = []
            with patch.object(directory_operations, "remove_owned_directory",
                              side_effect=lambda token: removed.append(token.directory)):
                directory_operations.rollback_owned_directories(entries)
            self.assertEqual(removed, [entry.directory for entry in reversed(entries)])
            self.assertTrue(deepest.directory.is_dir())
            for entry in entries:
                entry.consume()

    def test_replaced_directory_is_retained_and_cleanup_fails(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            owner = directory_operations.DirectoryOwner()
            token = owner.create("owned", root / "owned", root)
            token.directory.rename(root / "original")
            replacement = root / "owned"
            replacement.mkdir()
            with self.assertRaisesRegex(RuntimeError, "replaced"):
                directory_operations.remove_owned_directory(token)
            self.assertTrue(replacement.is_dir())

    def test_pinned_directory_rejects_unlink_recreate_for_create_and_adopt(self):
        operations = (
            ("remove", directory_operations.remove_owned_directory),
            ("clear", directory_operations.clear_owned_directory),
            ("release", directory_operations.release_owned_directory),
        )
        for factory_name in ("create", "adopt"):
            for operation_name, operation in operations:
                with self.subTest(factory=factory_name, operation=operation_name), \
                        tempfile.TemporaryDirectory(prefix="acceptance-owner-pinned-") as directory:
                    root = Path(directory)
                    owned = root / "owned"
                    owner = directory_operations.DirectoryOwner()
                    if factory_name == "create":
                        token = owner.create("owned", owned, root)
                    else:
                        owned.mkdir()
                        token = owner.adopt_created("owned", owned, root)
                    parent_fd = token.parent_fd
                    directory_fd = token.directory_fd

                    owned.rmdir()
                    owned.mkdir()
                    marker = owned / "replacement.txt"
                    marker.write_text("replacement", encoding="utf-8")
                    replacement = owned.stat()
                    self.assertNotEqual(
                        (replacement.st_dev, replacement.st_ino), token.identity,
                    )

                    with self.assertRaisesRegex(RuntimeError, "replaced"):
                        operation(token)

                    self.assertEqual(marker.read_text(encoding="utf-8"), "replacement")
                    with self.assertRaises(OSError):
                        os.fstat(directory_fd)
                    with self.assertRaises(OSError):
                        os.fstat(parent_fd)

    def test_symlink_replacement_is_retained_and_not_followed(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            outside = root / "outside"
            outside.mkdir()
            owner = directory_operations.DirectoryOwner()
            token = owner.create("owned", root / "owned", root)
            token.directory.rmdir()
            token.directory.symlink_to(outside, target_is_directory=True)
            with self.assertRaisesRegex(RuntimeError, "unsafe"):
                directory_operations.remove_owned_directory(token)
            self.assertTrue(outside.is_dir())

    def test_replaced_parent_is_retained_and_cleanup_fails(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            parent = root / "parent"
            parent.mkdir()
            owner = directory_operations.DirectoryOwner()
            token = owner.create("owned", parent / "owned", parent)
            parent.rename(root / "original-parent")
            parent.mkdir()
            with self.assertRaisesRegex(RuntimeError, "parent was replaced"):
                directory_operations.remove_owned_directory(token)
            self.assertTrue(parent.is_dir())

    def test_exact_owned_fifo_cleanup_removes_only_the_explicit_token(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owned-fifo-") as directory:
            root = Path(directory)
            fifo = root / "lease.fifo"
            os.mkfifo(fifo, 0o600)
            parent_fd, parent_identity = directory_operations._open_parent_directory(root)
            entry_fd = os.open(fifo, os.O_RDONLY | os.O_NONBLOCK)
            try:
                identity = fifo.lstat()
                self.assertTrue(directory_operations.remove_exact_owned_fifo(
                    parent_fd, entry_fd, fifo.name,
                    (identity.st_dev, identity.st_ino), parent_identity,
                ))
                os.fstat(entry_fd)
            finally:
                os.close(entry_fd)
                directory_operations._close_parent_descriptor(parent_fd)
            self.assertFalse(fifo.exists())
            self.assertEqual(tuple(root.glob(".delegate-cleanup-*")), ())

    def test_exact_owned_fifo_replacement_is_retained(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owned-fifo-replacement-") as directory:
            root = Path(directory)
            fifo = root / "lease.fifo"
            os.mkfifo(fifo, 0o600)
            original = fifo.lstat()
            parent_fd, parent_identity = directory_operations._open_parent_directory(root)
            entry_fd = os.open(fifo, os.O_RDONLY | os.O_NONBLOCK)
            try:
                replacement_path = root / "replacement.fifo"
                os.mkfifo(replacement_path, 0o600)
                fifo.unlink()
                replacement_path.rename(fifo)
                replacement = fifo.lstat()
                with self.assertRaisesRegex(RuntimeError, "owned FIFO was replaced"):
                    directory_operations.remove_exact_owned_fifo(
                        parent_fd, entry_fd, fifo.name,
                        (original.st_dev, original.st_ino), parent_identity,
                    )
                self.assertEqual(
                    (fifo.stat().st_dev, fifo.stat().st_ino),
                    (replacement.st_dev, replacement.st_ino),
                )
            finally:
                os.close(entry_fd)
                directory_operations._close_parent_descriptor(parent_fd)

    def test_recursive_directory_cleanup_still_rejects_unregistered_fifo(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owned-fifo-reject-") as directory:
            root = Path(directory)
            owner = directory_operations.DirectoryOwner()
            token = owner.create("owned", root / "owned", root)
            os.mkfifo(token.directory / "lease.fifo", 0o600)
            with self.assertRaisesRegex(RuntimeError, "unsupported unsafe entry"):
                directory_operations.clear_owned_directory(token)
            retained = tuple(token.directory.glob(".delegate-cleanup-*/entry"))
            self.assertEqual(len(retained), 1)
            self.assertTrue(stat.S_ISFIFO(retained[0].stat().st_mode))

    def test_descendant_identity_failure_retains_ancestor(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-ancestor-") as directory:
            root = Path(directory)
            owner = directory_operations.DirectoryOwner()
            parent = owner.create("parent", root / "parent", root)
            child = owner.create("child", parent.directory / "child", parent.directory)
            entries = owner.transfer()
            child.directory.rename(root / "old-child")
            child.directory.mkdir()
            with self.assertRaisesRegex(RuntimeError, "directory rollback failed"):
                directory_operations.rollback_owned_directories(entries)
            self.assertTrue(parent.directory.is_dir())
            self.assertTrue(child.directory.is_dir())

    def test_finalizer_skips_ancestor_after_descendant_identity_failure(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-finalizer-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = directory_operations.DirectoryOwner()
            parent = owner.create("parent", root / "parent", root)
            child = owner.create("child", parent.directory / "child", parent.directory)
            entries = owner.transfer()
            child.directory.rename(root / "old-child")
            child.directory.mkdir()
            self.assertFalse(directory_finalization.finalize_acceptance_artifacts(
                output, True, entries, True))
            self.assertTrue(parent.directory.is_dir())
            receipt = evidence_files.read_json(output / "cleanup.json")
            self.assertIn("parent", receipt["retained"])
            self.assertTrue(all(isinstance(item, dict) for item in receipt["failures"]))

    def test_finalizer_records_retention_before_supervisor_shutdown(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-open-supervisor-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = directory_operations.DirectoryOwner()
            token = owner.create("private state", root / "private", root)
            entries = owner.transfer()

            self.assertFalse(directory_finalization.finalize_acceptance_artifacts(
                output, False, entries, True))
            self.assertTrue(token.directory.is_dir())
            receipt = evidence_files.read_json(output / "cleanup.json")
            self.assertEqual(receipt["status"], "retained")
            self.assertFalse(receipt["supervisor_shutdown_observed"])
            self.assertIn("shutdown", receipt["reason"])
            token.consume()

    def test_raw_capture_token_requires_output_parent_identity(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-parent-identity-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = directory_operations.DirectoryOwner()
            raw = owner.create("raw process captures", output / "processes", output)
            entries = owner.transfer()
            output.rename(root / "old-evidence")
            output.mkdir()
            (output / "processes").mkdir()
            self.assertFalse(directory_finalization.finalize_acceptance_artifacts(
                output, True, entries, True, raw_process_captures_token=raw))
            self.assertTrue((output / "processes").is_dir())
            raw.consume()

    def test_finalization_has_its_own_bounded_deadline_and_retains_tokens(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-deadline-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = directory_operations.DirectoryOwner()
            token = owner.create("owned", root / "owned", root)
            entries = owner.transfer()
            with patch.object(directory_finalization, "failure_cleanup_deadline", return_value=0):
                self.assertFalse(directory_finalization.finalize_acceptance_artifacts(
                    output, True, entries, True, timeout=1))
            self.assertTrue(token.directory.is_dir())
            receipt = evidence_files.read_json(output / "cleanup.json")
            self.assertEqual(receipt["status"], "partial")
            with self.assertRaisesRegex(RuntimeError, "already consumed"):
                token.consume()

    def test_finalizer_removes_raw_capture_child_and_retains_evidence_root(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = directory_operations.DirectoryOwner()
            private = owner.create("private state", root / "private", root)
            raw = owner.create("raw process captures", output / "processes", output)
            (raw.directory / "stdout").write_bytes(b"evidence\n")
            entries = owner.transfer()

            self.assertTrue(directory_finalization.finalize_acceptance_artifacts(
                output, True, entries, True, raw_process_captures_token=raw))
            self.assertTrue(output.is_dir())
            self.assertFalse(raw.directory.exists())
            self.assertFalse(private.directory.exists())
            receipt = evidence_files.read_json(output / "cleanup.json")
            self.assertEqual(receipt["raw_process_captures"], "removed")

    def test_failed_finalization_retains_selected_state_and_cleans_unselected_owned_directories(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-retention-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = directory_operations.DirectoryOwner()
            private = owner.create("private root", root / "private", root)
            task_state = owner.create("task state", private.directory / "state",
                                      private.directory)
            workspace = owner.create("workspace", private.directory / "workspace",
                                     private.directory)
            (task_state.directory / "task.json").write_text("task", encoding="utf-8")
            (workspace.directory / "payload.txt").write_text("payload", encoding="utf-8")
            queue = owner.create("private queue", root / "queue", root)
            briefs = owner.create("briefs", root / "briefs", root)
            raw = owner.create("raw process captures", output / "processes", output)
            (queue.directory / "queued.json").write_text("queue", encoding="utf-8")
            (briefs.directory / "brief.txt").write_text("brief", encoding="utf-8")
            (raw.directory / "stderr").write_text("raw", encoding="utf-8")
            entries = owner.transfer()

            self.assertTrue(directory_finalization.finalize_acceptance_artifacts(
                output, True, entries, False,
                remove_failed_process_captures=True,
                raw_process_captures_token=raw,
                retained_owned_directories=(private, task_state, workspace),
            ))
            self.assertTrue(private.directory.is_dir())
            self.assertEqual((task_state.directory / "task.json").read_text(), "task")
            self.assertEqual((workspace.directory / "payload.txt").read_text(), "payload")
            self.assertFalse(queue.directory.exists())
            self.assertFalse(briefs.directory.exists())
            self.assertFalse(raw.directory.exists())
            receipt = evidence_files.read_json(output / "cleanup.json")
            self.assertEqual(set(receipt["retained"]),
                             {"private root", "task state", "workspace"})
            self.assertEqual(
                {record["path"] for record in receipt["retained_owned_directories"]},
                {str(private.directory), str(task_state.directory), str(workspace.directory)},
            )

    def test_unclosed_finalization_retains_all_tokens_and_records_exact_identities(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-unclosed-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = directory_operations.DirectoryOwner()
            private = owner.create("private root", root / "private", root)
            task_state = owner.create("task state", private.directory / "state",
                                      private.directory)
            entries = owner.transfer()

            self.assertFalse(directory_finalization.finalize_acceptance_artifacts(
                output, False, entries, False,
                retained_owned_directories=(private,),
            ))
            self.assertTrue(private.directory.is_dir())
            self.assertTrue(task_state.directory.is_dir())
            receipt = evidence_files.read_json(output / "cleanup.json")
            self.assertEqual(set(receipt["retained"]), {"private root", "task state"})
            self.assertEqual(
                {record["path"] for record in receipt["retained_owned_directories"]},
                {str(private.directory), str(task_state.directory)},
            )
            for entry in reversed(entries):
                entry.consume()

    def test_successful_finalization_rejects_selected_retention(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-success-retention-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = directory_operations.DirectoryOwner()
            private = owner.create("private root", root / "private", root)
            entries = owner.transfer()

            self.assertFalse(directory_finalization.finalize_acceptance_artifacts(
                output, True, entries, True,
                retained_owned_directories=(private,),
            ))
            self.assertTrue(private.directory.is_dir())
            private.consume()

    def test_retention_rejects_duplicate_foreign_and_consumed_tokens(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-invalid-retention-") as directory:
            root = Path(directory)

            duplicate_root = root / "duplicate"
            duplicate_root.mkdir()
            duplicate_output = duplicate_root / "evidence"
            duplicate_output.mkdir()
            duplicate_owner = directory_operations.DirectoryOwner()
            duplicate = duplicate_owner.create("duplicate", duplicate_root / "owned",
                                                duplicate_root)
            duplicate_entries = duplicate_owner.transfer()
            self.assertFalse(directory_finalization.finalize_acceptance_artifacts(
                duplicate_output, True, duplicate_entries, False,
                retained_owned_directories=(duplicate, duplicate),
            ))
            self.assertTrue(duplicate.directory.is_dir())
            duplicate.consume()

            foreign_root = root / "foreign"
            foreign_root.mkdir()
            foreign_output = foreign_root / "evidence"
            foreign_output.mkdir()
            foreign_owner = directory_operations.DirectoryOwner()
            foreign = foreign_owner.create("foreign", foreign_root / "foreign-owned",
                                           foreign_root)
            local_owner = directory_operations.DirectoryOwner()
            local = local_owner.create("local", foreign_root / "local-owned", foreign_root)
            local_entries = local_owner.transfer()
            self.assertFalse(directory_finalization.finalize_acceptance_artifacts(
                foreign_output, True, local_entries, False,
                retained_owned_directories=(foreign,),
            ))
            self.assertTrue(local.directory.is_dir())
            self.assertTrue(foreign.directory.is_dir())
            local.consume()
            foreign.consume()

            consumed_root = root / "consumed"
            consumed_root.mkdir()
            consumed_output = consumed_root / "evidence"
            consumed_output.mkdir()
            consumed_owner = directory_operations.DirectoryOwner()
            consumed = consumed_owner.create("consumed", consumed_root / "owned",
                                             consumed_root)
            consumed_entries = consumed_owner.transfer()
            consumed.consume()
            self.assertFalse(directory_finalization.finalize_acceptance_artifacts(
                consumed_output, True, consumed_entries, False,
                retained_owned_directories=(consumed,),
            ))
            self.assertTrue(consumed.directory.is_dir())

    def test_retention_rejects_child_without_owned_ancestor(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-retention-ancestor-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = directory_operations.DirectoryOwner()
            parent = owner.create("private root", root / "private", root)
            child = owner.create("task state", parent.directory / "state", parent.directory)
            entries = owner.transfer()

            self.assertFalse(directory_finalization.finalize_acceptance_artifacts(
                output, True, entries, False,
                retained_owned_directories=(child,),
            ))
            self.assertTrue(parent.directory.is_dir())
            self.assertTrue(child.directory.is_dir())
            for entry in reversed(entries):
                entry.consume()

    def test_child_transfer_consumes_its_descriptor_and_leaves_parent_authority(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            parent_owner = directory_operations.DirectoryOwner()
            parent = parent_owner.create("capture root", root / "processes", root)
            child_owner = directory_operations.DirectoryOwner()
            child = child_owner.create("capture", parent.directory / "001", parent.directory)
            parent_descriptor = child.parent_fd
            directory_descriptor = child.directory_fd

            self.assertTrue(child_owner.transfer_child_to(parent))
            with self.assertRaises(OSError):
                os.fstat(parent_descriptor)
            with self.assertRaises(OSError):
                os.fstat(directory_descriptor)
            self.assertTrue(child.directory.is_dir())
            self.assertTrue(parent_owner.rollback())
            self.assertFalse(parent.directory.exists())

    def test_consumed_directory_token_cannot_be_reused_after_cleanup(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            owner = directory_operations.DirectoryOwner()
            token = owner.create("owned", root / "owned", root)

            self.assertTrue(directory_operations.remove_owned_directory(token))
            with self.assertRaisesRegex(RuntimeError, "already consumed"):
                directory_operations.remove_owned_directory(token)

    def test_two_hundred_capture_lifecycles_do_not_leak_descriptors(self):
        descriptor_directory = Path("/proc/self/fd")
        if not descriptor_directory.is_dir():
            descriptor_directory = Path("/dev/fd")
        if not descriptor_directory.is_dir():
            self.skipTest("descriptor enumeration is unavailable")

        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            before = len(os.listdir(descriptor_directory))
            for index in range(200):
                owner = directory_operations.DirectoryOwner()
                process_operations.Processes(root / f"processes-{index:03d}", {}, owner=owner)
                directory_operations.rollback_owned_directories(owner.transfer())
            after = len(os.listdir(descriptor_directory))
            self.assertLessEqual(after, before)

    def test_raw_capture_replacement_keeps_replacement_and_fails_finalizer(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = directory_operations.DirectoryOwner()
            raw = owner.create("raw process captures", output / "processes", output)
            raw.directory.rename(output / "old-processes")
            replacement = output / "processes"
            replacement.mkdir()
            entries = owner.transfer()

            self.assertFalse(directory_finalization.finalize_acceptance_artifacts(
                output, True, entries, True, raw_process_captures_token=raw))
            self.assertTrue(replacement.is_dir())

    def test_process_setup_failures_remove_only_the_new_capture_directory(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-process-failure-") as directory:
            root = Path(directory)
            owner = directory_operations.DirectoryOwner()
            raw = owner.create("raw process captures", root / "processes", root)
            child = raw.directory / "001-failing"
            for failure in ("write", "open"):
                with self.subTest(failure=failure):
                    if child.exists():
                        child.rmdir()
                    if failure == "write":
                        operation = patch.object(process_operations, "write_json",
                                                 side_effect=OSError("setup write"))
                    else:
                        operation = patch.object(process_operations.Path, "open",
                                                 side_effect=OSError("setup open"))
                    with operation, patch.object(process_operations.subprocess, "Popen") as popen, \
                            self.assertRaises(OSError):
                        process_operations.Process(child, [sys.executable, "-c", "pass"], root, {}, raw)
                    popen.assert_not_called()
                    self.assertFalse(child.exists())
                    self.assertTrue(raw.directory.is_dir())
            owner.rollback()

    def test_uncertain_popen_failure_refuses_finalization_and_retains_capture(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-uncertain-start-") as directory:
            output = Path(directory) / "evidence"
            output.mkdir()
            owner = directory_operations.DirectoryOwner()
            raw = owner.create("raw process captures", output / "processes", output)
            track_directory_pins(self, (raw,))
            processes = process_operations.Processes(raw.directory, {}, capture_root=raw)
            # The fake raises from inside Popen and creates no child.
            with patch.object(process_operations.subprocess, "Popen", side_effect=OSError("errpipe")), \
                    self.assertRaisesRegex(OSError, "errpipe"):
                processes.start("uncertain", [sys.executable, "-c", "pass"], Path(directory))
            closed = not processes.drain(timeout=0) and processes.finished_naturally()
            self.assertFalse(closed)
            self.assertFalse(directory_finalization.finalize_acceptance_artifacts(
                output, closed, owner.transfer(), True, raw_process_captures_token=raw))
            self.assertTrue((raw.directory / "001-uncertain" / "invocation.json").is_file())

    def test_agy_start_receipt_failure_leaves_registry_owned_child(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-agy-start-") as directory:
            root = Path(directory)
            owner = directory_operations.DirectoryOwner()
            book = process_operations.Processes(root / "processes", {}, owner=owner)
            original = evidence_files.write_json

            def fail_started(path, value, replace=False):
                if Path(path).name == "started.json":
                    raise OSError("started receipt")
                return original(path, value, replace=replace)

            with patch.object(process_operations, "write_json", side_effect=fail_started), \
                    self.assertRaisesRegex(OSError, "started receipt"):
                book.start("short", [str(Path(sys.executable).resolve()), "-c", "pass"], root)
            self.assertEqual(len(book.entries), 1)
            process = book.entries[0]
            process.wait(10, expected=0)
            self.assertTrue(process.directory.is_dir())
            owner.rollback()

    def test_signaled_process_does_not_authorize_capture_cleanup(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-signal-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = directory_operations.DirectoryOwner()
            raw = owner.create("raw process captures", output / "processes", output)
            track_directory_pins(self, (raw,))
            processes = process_operations.Processes(raw.directory, {}, capture_root=raw)
            process = processes.start(
                "signaled", [sys.executable, "-c", "import os,signal; os.kill(os.getpid(), signal.SIGTERM)"], root)
            deadline = time.monotonic() + 5
            while process.poll() is None and time.monotonic() < deadline:
                time.sleep(0.01)
            self.assertIsNotNone(process.result)
            self.assertFalse(process.result["natural_wait"])
            self.assertFalse(directory_finalization.finalize_acceptance_artifacts(
                output, False, owner.transfer(), True,
                raw_process_captures_token=raw))
            self.assertTrue(raw.directory.is_dir())


if __name__ == "__main__":
    unittest.main()
