"""Focused identity-token ownership tests for acceptance fixtures."""

from pathlib import Path
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

import acceptance_agy as agy
import acceptance_supervisor_common as common


class DirectoryOwnershipTests(unittest.TestCase):
    def _prepared_cleanup(self, root: Path, name: str = "owned"):
        target = root / name
        quarantine_parent = root / "quarantine"
        quarantine_parent.mkdir()
        owner = common.DirectoryOwner()
        token = owner.create("owned", target, root)
        (target / "nested").mkdir()
        (target / "nested" / "payload.txt").write_text("payload", encoding="utf-8")
        identity = common.directory_identity_record(token)
        owner.release()
        journal = root / f"{name}.cleanup.json"
        authorization = "a" * 64
        common.prepare_directory_cleanup(
            journal, "test cleanup", identity, quarantine_parent, authorization,
        )
        return target, journal, authorization, identity

    def test_directory_creation_never_replaces_a_racing_destination(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-create-race-") as directory:
            root = Path(directory)
            original = common._rename_noreplace

            def occupy_then_publish(source_fd, source_name, destination_fd, destination_name):
                common.os.mkdir(destination_name, dir_fd=destination_fd)
                (root / destination_name / "keep.txt").write_text("replacement", encoding="utf-8")
                return original(source_fd, source_name, destination_fd, destination_name)

            owner = common.DirectoryOwner()
            with patch.object(common, "_rename_noreplace", side_effect=occupy_then_publish), \
                    self.assertRaises(FileExistsError):
                owner.create("owned", root / "owned", root)
            self.assertEqual((root / "owned" / "keep.txt").read_text(), "replacement")
            self.assertEqual(list(root.glob(".delegate-create-*")), [])

    def test_cleanup_preparation_has_no_unjournaled_filesystem_side_effect(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-cleanup-prepare-") as directory:
            root = Path(directory)
            target = root / "owned"
            quarantine_parent = root / "quarantine"
            quarantine_parent.mkdir()
            owner = common.DirectoryOwner()
            token = owner.create("owned", target, root)
            (target / "keep.txt").write_text("payload", encoding="utf-8")
            identity = common.directory_identity_record(token)
            owner.release()
            journal = root / "cleanup.json"

            with patch.object(common, "_rename_noreplace", side_effect=OSError("journal publish")), \
                    self.assertRaisesRegex(OSError, "journal publish"):
                common.prepare_directory_cleanup(
                    journal, "test cleanup", identity, quarantine_parent, "b" * 64,
                )
            self.assertEqual((target / "keep.txt").read_text(), "payload")
            self.assertEqual(list(quarantine_parent.iterdir()), [])
            self.assertFalse(journal.exists())
            self.assertEqual(list(root.glob(".cleanup.json.*.staging")), [])
            prepared = common.prepare_directory_cleanup(
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
            owner = common.DirectoryOwner()
            token = owner.create("owned", target, source_parent)
            (target / "payload.txt").write_text("payload", encoding="utf-8")
            identity = common.directory_identity_record(token)
            owner.release()
            journal = journal_parent / "cleanup.json"
            authorization = "c" * 64
            journal_parent_identity = (
                journal_parent.stat().st_dev, journal_parent.stat().st_ino,
            )
            original_fsync = common.os.fsync
            failed = False

            def fail_first_journal_parent_sync(descriptor):
                nonlocal failed
                opened = common.os.fstat(descriptor)
                if (not failed and journal.exists()
                        and (opened.st_dev, opened.st_ino) == journal_parent_identity):
                    failed = True
                    raise OSError("simulated journal directory sync failure")
                return original_fsync(descriptor)

            with patch.object(common.os, "fsync", side_effect=fail_first_journal_parent_sync), \
                    self.assertRaisesRegex(OSError, "journal directory sync failure"):
                common.prepare_directory_cleanup(
                    journal, "test cleanup", identity, quarantine_parent, authorization,
                )
            self.assertTrue(journal.is_file())
            self.assertTrue((target / "payload.txt").is_file())

            events = []
            original_rename = common._rename_noreplace

            def record_sync(descriptor):
                opened = common.os.fstat(descriptor)
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

            with patch.object(common.os, "fsync", side_effect=record_sync), \
                    patch.object(common, "_rename_noreplace", side_effect=record_rename):
                common.prepare_directory_cleanup(
                    journal, "test cleanup", identity, quarantine_parent, authorization,
                )
                completed = common.resume_directory_cleanup(
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
                original = common.write_json
                interrupted = False

                def interrupt_transition(path, value, replace=False):
                    nonlocal interrupted
                    if (not interrupted and replace
                            and isinstance(value, dict)
                            and value.get("state") == interrupted_state):
                        interrupted = True
                        raise OSError("simulated journal interruption")
                    return original(path, value, replace=replace)

                with patch.object(common, "write_json", side_effect=interrupt_transition), \
                        self.assertRaisesRegex(OSError, "simulated journal interruption"):
                    common.resume_directory_cleanup(journal, authorization, identity)
                self.assertEqual(
                    common.read_directory_cleanup(journal, authorization)["state"],
                    durable_state,
                )
                completed = common.resume_directory_cleanup(journal, authorization, identity)
                self.assertEqual(completed["state"], "complete")
                self.assertFalse(target.exists())
                self.assertFalse(Path(completed["quarantine"]["path"]).exists())

    def test_isolation_replay_syncs_both_recorded_parents(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-cleanup-sync-") as directory:
            root = Path(directory)
            _, journal, authorization, identity = self._prepared_cleanup(root)
            original_write = common.write_json
            interrupted = False

            def interrupt_isolated(path, value, replace=False):
                nonlocal interrupted
                if (not interrupted and replace and isinstance(value, dict)
                        and value.get("state") == "isolated"):
                    interrupted = True
                    raise OSError("isolation journal interruption")
                return original_write(path, value, replace=replace)

            with patch.object(common, "write_json", side_effect=interrupt_isolated), \
                    self.assertRaisesRegex(OSError, "isolation journal interruption"):
                common.resume_directory_cleanup(journal, authorization, identity)
            prepared = common.read_directory_cleanup(journal, authorization)
            synced = set()
            original_sync = common.os.fsync

            def record_sync(descriptor):
                metadata = common.os.fstat(descriptor)
                synced.add((metadata.st_dev, metadata.st_ino))
                return original_sync(descriptor)

            with patch.object(common.os, "fsync", side_effect=record_sync):
                common.resume_directory_cleanup(journal, authorization, identity)
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
            value = common.read_json(journal)
            unrelated_stat = unrelated.stat()
            value["quarantine"]["device"] = unrelated_stat.st_dev
            value["quarantine"]["inode"] = unrelated_stat.st_ino
            common.write_json(journal, value, replace=True)

            with self.assertRaisesRegex(RuntimeError, "not bound to its target"):
                common.resume_directory_cleanup(journal, authorization, identity)
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
            owner = common.DirectoryOwner()
            token = owner.adopt_created("unrelated", unrelated, root)
            unrelated_identity = common.directory_identity_record(token)
            owner.release()
            value = common.read_json(journal)
            value["target"] = unrelated_identity
            value["quarantine"]["device"] = unrelated_identity["device"]
            value["quarantine"]["inode"] = unrelated_identity["inode"]
            common.write_json(journal, value, replace=True)

            with self.assertRaisesRegex(RuntimeError, "cleanup target identity changed"):
                common.resume_directory_cleanup(
                    journal, authorization, expected_identity,
                )
            self.assertTrue((target / "nested" / "payload.txt").is_file())
            self.assertEqual(marker.read_text(encoding="utf-8"), "retain")

    def test_cleanup_replay_retains_a_replacement_at_the_original_path(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-cleanup-replacement-") as directory:
            root = Path(directory)
            target, journal, authorization, identity = self._prepared_cleanup(root)
            with patch.object(common, "_cleanup_empty", side_effect=RuntimeError("pause")), \
                    self.assertRaisesRegex(RuntimeError, "pause"):
                common.resume_directory_cleanup(journal, authorization, identity)
            record = common.read_directory_cleanup(journal, authorization)
            self.assertEqual(record["state"], "isolated")
            target.mkdir()
            marker = target / "replacement.txt"
            marker.write_text("retain", encoding="utf-8")

            with self.assertRaisesRegex(RuntimeError, "occupied after isolation"):
                common.resume_directory_cleanup(journal, authorization, identity)
            self.assertEqual(marker.read_text(), "retain")
            self.assertTrue((Path(record["quarantine"]["path"]) / "nested" / "payload.txt").is_file())

    def test_cleanup_replay_retains_a_replaced_quarantine(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-cleanup-quarantine-") as directory:
            root = Path(directory)
            _, journal, authorization, identity = self._prepared_cleanup(root)
            with patch.object(common, "_cleanup_empty", side_effect=RuntimeError("pause")), \
                    self.assertRaisesRegex(RuntimeError, "pause"):
                common.resume_directory_cleanup(journal, authorization, identity)
            record = common.read_directory_cleanup(journal, authorization)
            quarantine = Path(record["quarantine"]["path"])
            original = root / "isolated-original"
            quarantine.rename(original)
            quarantine.mkdir()
            marker = quarantine / "replacement.txt"
            marker.write_text("retain", encoding="utf-8")

            with self.assertRaisesRegex(RuntimeError, "quarantine identity changed"):
                common.resume_directory_cleanup(journal, authorization, identity)
            self.assertEqual(marker.read_text(), "retain")
            self.assertTrue((original / "nested" / "payload.txt").is_file())

    def test_cleanup_journal_serializes_concurrent_resumers(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-cleanup-concurrent-") as directory:
            root = Path(directory)
            target, journal, authorization, identity = self._prepared_cleanup(root)
            entered = threading.Event()
            proceed = threading.Event()
            original = common._cleanup_empty
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
                    results.append(common.resume_directory_cleanup(
                        journal, authorization, identity,
                    ))
                except BaseException as error:  # Preserve thread errors for the assertion.
                    errors.append(error)

            with patch.object(common, "_cleanup_empty", side_effect=pause_once):
                first = threading.Thread(target=resume)
                second = threading.Thread(target=resume)
                first.start()
                self.assertTrue(entered.wait(5))
                second.start()
                self.assertTrue(second.is_alive())
                proceed.set()
                first.join(5)
                second.join(5)

            self.assertEqual(errors, [])
            self.assertEqual([result["state"] for result in results], ["complete", "complete"])
            self.assertFalse(target.exists())

    def test_nested_creation_transfers_and_rolls_back_in_reverse_order(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            owner = common.DirectoryOwner()
            deepest = owner.create("nested", root / "a" / "b" / "c", root, parents=True)
            entries = owner.transfer()
            self.assertEqual(
                [entry.directory for entry in entries],
                [root / "a", root / "a" / "b", root / "a" / "b" / "c"],
            )
            removed = []
            with patch.object(common, "remove_owned_directory",
                              side_effect=lambda token: removed.append(token.directory)):
                common.rollback_owned_directories(entries)
            self.assertEqual(removed, [entry.directory for entry in reversed(entries)])
            self.assertTrue(deepest.directory.is_dir())

    def test_replaced_directory_is_retained_and_cleanup_fails(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            owner = common.DirectoryOwner()
            token = owner.create("owned", root / "owned", root)
            token.directory.rename(root / "original")
            replacement = root / "owned"
            replacement.mkdir()
            with self.assertRaisesRegex(RuntimeError, "replaced"):
                common.remove_owned_directory(token)
            self.assertTrue(replacement.is_dir())

    def test_symlink_replacement_is_retained_and_not_followed(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            outside = root / "outside"
            outside.mkdir()
            owner = common.DirectoryOwner()
            token = owner.create("owned", root / "owned", root)
            token.directory.rmdir()
            token.directory.symlink_to(outside, target_is_directory=True)
            with self.assertRaisesRegex(RuntimeError, "unsafe"):
                common.remove_owned_directory(token)
            self.assertTrue(outside.is_dir())

    def test_concurrent_path_replacement_is_not_deleted(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            owner = common.DirectoryOwner()
            token = owner.create("owned", root / "owned", root)
            moved = threading.Event()
            replacement_ready = threading.Event()
            errors = []
            original_rename = common.os.rename

            def rename_then_pause(source, destination, *args, **kwargs):
                result = original_rename(source, destination, *args, **kwargs)
                if source == "owned" and destination == "entry":
                    moved.set()
                    if not replacement_ready.wait(5):
                        raise RuntimeError("replacement fixture did not continue")
                return result

            def cleanup():
                try:
                    common.remove_owned_directory(token)
                except BaseException as error:  # Preserve the worker failure for the test thread.
                    errors.append(error)

            with patch.object(common.os, "rename", side_effect=rename_then_pause):
                worker = threading.Thread(target=cleanup)
                worker.start()
                try:
                    self.assertTrue(moved.wait(5))
                    replacement = root / "owned"
                    replacement.mkdir()
                    (replacement / "keep.txt").write_text("replacement")
                finally:
                    replacement_ready.set()
                worker.join(5)

            self.assertFalse(worker.is_alive())
            self.assertEqual(errors, [])
            self.assertEqual((root / "owned" / "keep.txt").read_text(), "replacement")

    def test_concurrent_preisolation_replacement_is_quarantined_not_deleted(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            owner = common.DirectoryOwner()
            token = owner.create("owned", root / "owned", root)
            rename_ready = threading.Event()
            replacement_ready = threading.Event()
            errors = []
            original_rename = common.os.rename

            def pause_before_rename(source, destination, *args, **kwargs):
                if source == "owned" and destination == "entry":
                    rename_ready.set()
                    if not replacement_ready.wait(5):
                        raise RuntimeError("replacement fixture did not continue")
                return original_rename(source, destination, *args, **kwargs)

            def cleanup():
                try:
                    common.remove_owned_directory(token)
                except BaseException as error:  # Preserve the worker failure for the test thread.
                    errors.append(error)

            with patch.object(common.os, "rename", side_effect=pause_before_rename):
                worker = threading.Thread(target=cleanup)
                worker.start()
                try:
                    self.assertTrue(rename_ready.wait(5))
                    token.directory.rename(root / "original")
                    token.directory.mkdir()
                    (token.directory / "keep.txt").write_text("replacement")
                finally:
                    replacement_ready.set()
                worker.join(5)

            self.assertFalse(worker.is_alive())
            self.assertEqual(len(errors), 1)
            self.assertRegex(str(errors[0]), "replaced.*quarantine")
            retained = tuple(root.glob(".delegate-cleanup-*/entry/keep.txt"))
            self.assertEqual(len(retained), 1)
            self.assertEqual(retained[0].read_text(), "replacement")
            self.assertTrue((root / "original").is_dir())

    def test_replaced_parent_is_retained_and_cleanup_fails(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            parent = root / "parent"
            parent.mkdir()
            owner = common.DirectoryOwner()
            token = owner.create("owned", parent / "owned", parent)
            parent.rename(root / "original-parent")
            parent.mkdir()
            with self.assertRaisesRegex(RuntimeError, "parent was replaced"):
                common.remove_owned_directory(token)
            self.assertTrue(parent.is_dir())

    def test_descendant_identity_failure_retains_ancestor(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-ancestor-") as directory:
            root = Path(directory)
            owner = common.DirectoryOwner()
            parent = owner.create("parent", root / "parent", root)
            child = owner.create("child", parent.directory / "child", parent.directory)
            entries = owner.transfer()
            child.directory.rename(root / "old-child")
            child.directory.mkdir()
            with self.assertRaisesRegex(RuntimeError, "directory rollback failed"):
                common.rollback_owned_directories(entries)
            self.assertTrue(parent.directory.is_dir())
            self.assertTrue(child.directory.is_dir())

    def test_finalizer_skips_ancestor_after_descendant_identity_failure(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-finalizer-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = common.DirectoryOwner()
            parent = owner.create("parent", root / "parent", root)
            child = owner.create("child", parent.directory / "child", parent.directory)
            entries = owner.transfer()
            child.directory.rename(root / "old-child")
            child.directory.mkdir()
            self.assertFalse(common.finalize_acceptance_artifacts(
                output, True, entries, True))
            self.assertTrue(parent.directory.is_dir())
            receipt = common.read_json(output / "cleanup.json")
            self.assertIn("parent", receipt["retained"])
            self.assertTrue(all(isinstance(item, dict) for item in receipt["failures"]))

    def test_finalizer_records_retention_before_supervisor_shutdown(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-open-supervisor-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = common.DirectoryOwner()
            token = owner.create("private state", root / "private", root)
            entries = owner.transfer()

            self.assertFalse(common.finalize_acceptance_artifacts(
                output, False, entries, True))
            self.assertTrue(token.directory.is_dir())
            receipt = common.read_json(output / "cleanup.json")
            self.assertEqual(receipt["status"], "retained")
            self.assertFalse(receipt["supervisor_shutdown_observed"])
            self.assertIn("shutdown", receipt["reason"])
            token.consume()

    def test_raw_capture_token_requires_output_parent_identity(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-parent-identity-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = common.DirectoryOwner()
            raw = owner.create("raw process captures", output / "processes", output)
            entries = owner.transfer()
            output.rename(root / "old-evidence")
            output.mkdir()
            (output / "processes").mkdir()
            self.assertFalse(common.finalize_acceptance_artifacts(
                output, True, entries, True, raw_process_captures_token=raw))
            self.assertTrue((output / "processes").is_dir())
            raw.consume()

    def test_finalization_has_its_own_bounded_deadline_and_retains_tokens(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-deadline-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = common.DirectoryOwner()
            token = owner.create("owned", root / "owned", root)
            entries = owner.transfer()
            with patch.object(common, "failure_cleanup_deadline", return_value=0):
                self.assertFalse(common.finalize_acceptance_artifacts(
                    output, True, entries, True, timeout=1))
            self.assertTrue(token.directory.is_dir())
            receipt = common.read_json(output / "cleanup.json")
            self.assertEqual(receipt["status"], "partial")
            with self.assertRaisesRegex(RuntimeError, "already consumed"):
                token.consume()

    def test_finalizer_removes_raw_capture_child_and_retains_evidence_root(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = common.DirectoryOwner()
            private = owner.create("private state", root / "private", root)
            raw = owner.create("raw process captures", output / "processes", output)
            (raw.directory / "stdout").write_bytes(b"evidence\n")
            entries = owner.transfer()

            self.assertTrue(common.finalize_acceptance_artifacts(
                output, True, entries, True, raw_process_captures_token=raw))
            self.assertTrue(output.is_dir())
            self.assertFalse(raw.directory.exists())
            self.assertFalse(private.directory.exists())
            receipt = common.read_json(output / "cleanup.json")
            self.assertEqual(receipt["raw_process_captures"], "removed")

    def test_child_transfer_consumes_its_descriptor_and_leaves_parent_authority(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            parent_owner = common.DirectoryOwner()
            parent = parent_owner.create("capture root", root / "processes", root)
            child_owner = common.DirectoryOwner()
            child = child_owner.create("capture", parent.directory / "001", parent.directory)
            descriptor = child.parent_fd

            self.assertTrue(child_owner.transfer_child_to(parent))
            with self.assertRaises(OSError):
                common.os.fstat(descriptor)
            self.assertTrue(child.directory.is_dir())
            self.assertTrue(parent_owner.rollback())
            self.assertFalse(parent.directory.exists())

    def test_consumed_directory_token_cannot_be_reused_after_cleanup(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            owner = common.DirectoryOwner()
            token = owner.create("owned", root / "owned", root)

            self.assertTrue(common.remove_owned_directory(token))
            with self.assertRaisesRegex(RuntimeError, "already consumed"):
                common.remove_owned_directory(token)

    def test_two_hundred_capture_lifecycles_do_not_leak_descriptors(self):
        descriptor_directory = Path("/proc/self/fd")
        if not descriptor_directory.is_dir():
            descriptor_directory = Path("/dev/fd")
        if not descriptor_directory.is_dir():
            self.skipTest("descriptor enumeration is unavailable")

        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            before = len(common.os.listdir(descriptor_directory))
            for index in range(200):
                owner = common.DirectoryOwner()
                common.Processes(root / f"processes-{index:03d}", {}, owner=owner)
                common.rollback_owned_directories(owner.transfer())
            after = len(common.os.listdir(descriptor_directory))
            self.assertLessEqual(after, before)

    def test_raw_capture_replacement_keeps_replacement_and_fails_finalizer(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-owner-") as directory:
            root = Path(directory)
            output = root / "evidence"
            output.mkdir()
            owner = common.DirectoryOwner()
            raw = owner.create("raw process captures", output / "processes", output)
            raw.directory.rename(output / "old-processes")
            replacement = output / "processes"
            replacement.mkdir()
            entries = owner.transfer()

            self.assertFalse(common.finalize_acceptance_artifacts(
                output, True, entries, True, raw_process_captures_token=raw))
            self.assertTrue(replacement.is_dir())

    def test_process_setup_failures_remove_only_the_new_capture_directory(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-process-failure-") as directory:
            root = Path(directory)
            owner = common.DirectoryOwner()
            raw = owner.create("raw process captures", root / "processes", root)
            child = raw.directory / "001-failing"
            for failure in ("write", "open", "start"):
                with self.subTest(failure=failure):
                    if child.exists():
                        child.rmdir()
                    if failure == "write":
                        operation = patch.object(common, "write_json",
                                                 side_effect=OSError("setup write"))
                    elif failure == "open":
                        operation = patch.object(common.Path, "open",
                                                 side_effect=OSError("setup open"))
                    else:
                        operation = patch.object(common.subprocess, "Popen",
                                                 side_effect=OSError("setup start"))
                    with operation, self.assertRaises(OSError):
                        common.Process(child, [sys.executable, "-c", "pass"], root, {}, raw)
                    self.assertFalse(child.exists())
                    self.assertTrue(raw.directory.is_dir())
            owner.rollback()

    def test_agy_start_receipt_failure_leaves_registry_owned_child(self):
        with tempfile.TemporaryDirectory(prefix="acceptance-agy-start-") as directory:
            root = Path(directory)
            owner = common.DirectoryOwner()
            book = agy.ProcessBook(root / "processes", {}, owner=owner)
            original = common.write_json

            def fail_started(path, value, replace=False):
                if Path(path).name == "started.json":
                    raise OSError("started receipt")
                return original(path, value, replace=replace)

            with patch.object(common, "write_json", side_effect=fail_started), \
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
            owner = common.DirectoryOwner()
            raw = owner.create("raw process captures", output / "processes", output)
            processes = common.Processes(raw.directory, {}, capture_root=raw)
            process = processes.start(
                "signaled", [sys.executable, "-c", "import os,signal; os.kill(os.getpid(), signal.SIGTERM)"], root)
            deadline = time.monotonic() + 5
            while process.poll() is None and time.monotonic() < deadline:
                time.sleep(0.01)
            self.assertIsNotNone(process.result)
            self.assertFalse(process.result["natural_wait"])
            self.assertFalse(common.finalize_acceptance_artifacts(
                output, False, owner.transfer(), True,
                raw_process_captures_token=raw))
            self.assertTrue(raw.directory.is_dir())


if __name__ == "__main__":
    unittest.main()
