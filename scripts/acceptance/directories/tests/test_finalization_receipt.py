"""The canonical cleanup receipt never carries exception text, filenames or errno.

Faults are injected inside the real removal, release, deadline and consume paths and the
real ``finalize_acceptance_artifacts`` writes the receipt; nothing here mocks the finalizer.
"""

import contextlib
from pathlib import Path
import os
import tempfile
import unittest
from unittest.mock import patch
import sys
import json


repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.directories import journal as directories_journal
from scripts.acceptance.evidence import files as evidence_files

from scripts.acceptance.directories import finalization as directory_finalization
from scripts.acceptance.directories import operations as directory_operations

CANARY = "canary-secret-must-not-reach-a-receipt"
CANARY_FILENAME = "canary-filename-must-not-reach-a-receipt"
FAILURE_FIELDS = {"label", "operation", "error", "error_class"}
FAILURE_MESSAGES = {
    "deadline": "acceptance cleanup deadline expired",
    "consume": "owned directory authority could not be consumed",
    "ancestor_release": "ancestor retained after descendant cleanup failure",
    "retained_release": "retained owned directory could not be released",
    "raw_release": "raw process capture directory could not be released",
    "remove": "owned directory could not be removed",
}


def canary_error(error_type=OSError):
    if error_type is OSError:
        return OSError(13, CANARY, CANARY_FILENAME)
    return error_type(CANARY + " " + CANARY_FILENAME)


@contextlib.contextmanager
def failing_for(label, error_type=OSError):
    """Make one labelled token fail inside the real release/removal path, once cleanup itself has started."""
    real_validate = directory_operations._validate_entry_parent
    real_deadline = directory_finalization.failure_cleanup_deadline
    armed = []

    def validate(token):
        if armed and token.label == label:
            raise canary_error(error_type)
        return real_validate(token)

    def deadline(timeout):
        armed.append(True)
        return real_deadline(timeout)

    with patch.object(directory_operations, "_validate_entry_parent", side_effect=validate), \
            patch.object(directory_finalization, "failure_cleanup_deadline", side_effect=deadline):
        yield


class CleanupReceiptTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="acceptance-cleanup-receipt-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.output = self.root / "evidence"
        self.output.mkdir()
        self.neighbour = self.root / "neighbour"
        self.neighbour.mkdir()
        (self.neighbour / "keep.txt").write_text("keep", encoding="utf-8")
        self.owner = directory_operations.DirectoryOwner()

    def create(self, label, parent=None):
        parent = parent or self.root
        return self.owner.create(label, parent / label.replace(" ", "-"), parent)

    def finalize(self, entries, *, success=False, retained=(), raw=None, closed=True):
        identities = {entry.label: directories_journal.directory_identity_record(entry) for entry in entries}
        result = directory_finalization.finalize_acceptance_artifacts(
            self.output, closed, entries, success, raw_process_captures_token=raw,
            retained_owned_directories=tuple(retained))
        text = (self.output / "cleanup.json").read_text(encoding="utf-8")
        return result, evidence_files.read_json(self.output / "cleanup.json"), text, identities

    def assert_sanitized(self, receipt, text, operations, *, error_class="PermissionError"):
        for secret in (CANARY, CANARY_FILENAME):
            self.assertNotIn(secret, text)
        self.assertEqual([item["operation"] for item in receipt["failures"]], operations)
        for item in receipt["failures"]:
            self.assertEqual(set(item), FAILURE_FIELDS)
            self.assertIsInstance(item["error"], str)
            self.assertTrue(item["error"])
            self.assertEqual(item["error"], FAILURE_MESSAGES[item["operation"]])
            self.assertEqual(item["error_class"], error_class)
        self.assertEqual((self.neighbour / "keep.txt").read_text(encoding="utf-8"), "keep")

    def test_a_removal_fault_records_a_fixed_error_and_retains_the_token_identity(self):
        victim = self.create("victim")
        entries = self.owner.transfer()
        with patch.object(directory_operations, "_remove_entry_at", side_effect=canary_error()):
            result, receipt, text, identities = self.finalize(entries)
        self.assertFalse(result)
        self.assert_sanitized(receipt, text, ["remove"])
        self.assertEqual(receipt["status"], "partial")
        self.assertEqual(receipt["failures"][0]["label"], "victim")
        self.assertEqual(receipt["failures"][0]["error_class"], "PermissionError")
        self.assertEqual(receipt["removed"], [])
        self.assertTrue(victim.directory.is_dir())
        with self.assertRaises(RuntimeError):
            victim.consume()
        self.assertEqual(identities["victim"]["path"], str(victim.directory))

    def test_a_removal_runtime_fault_is_classed_without_its_text(self):
        self.create("victim")
        entries = self.owner.transfer()
        with patch.object(directory_operations, "_remove_entry_at", side_effect=canary_error(RuntimeError)):
            result, receipt, text, _ = self.finalize(entries)
        self.assertFalse(result)
        self.assert_sanitized(receipt, text, ["remove"], error_class="RuntimeError")
        self.assertEqual(receipt["failures"][0]["error_class"], "RuntimeError")

    def test_an_ancestor_is_retained_and_its_release_fault_is_sanitized(self):
        parent = self.create("parent")
        child = self.create("child", parent.directory)
        entries = self.owner.transfer()
        real = directory_operations._remove_entry_at

        def fail_child(parent_fd, name, *args, **kwargs):
            if name == "child":
                raise canary_error()
            return real(parent_fd, name, *args, **kwargs)

        with patch.object(directory_operations, "_remove_entry_at", side_effect=fail_child), \
                failing_for("parent"):
            result, receipt, text, identities = self.finalize(entries)
        self.assertFalse(result)
        self.assert_sanitized(receipt, text, ["remove", "ancestor_release"])
        self.assertEqual([item["label"] for item in receipt["failures"]], ["child", "parent"])
        self.assertEqual(receipt["retained"], ["parent"])
        self.assertEqual(receipt["retained_owned_directories"], [identities["parent"]])
        self.assertTrue(parent.directory.is_dir() and child.directory.is_dir())

    def test_a_retained_release_fault_is_sanitized_and_the_directory_stays(self):
        keep = self.create("keep state")
        gone = self.create("scratch")
        entries = self.owner.transfer()
        with failing_for("keep state"):
            result, receipt, text, identities = self.finalize(entries, retained=(keep,))
        self.assertFalse(result)
        self.assert_sanitized(receipt, text, ["retained_release"])
        self.assertEqual(receipt["failures"][0]["label"], "keep state")
        self.assertEqual(receipt["retained"], ["keep state"])
        self.assertEqual(receipt["removed"], ["scratch"])
        self.assertEqual(receipt["retained_owned_directories"], [identities["keep state"]])
        self.assertTrue(keep.directory.is_dir())
        self.assertFalse(gone.directory.exists())

    def test_a_legitimate_owned_path_containing_13_is_sanitized(self):
        keep = self.create("state13-errno-strerror-filename")
        entries = self.owner.transfer()
        with failing_for(keep.label):
            result, receipt, text, identities = self.finalize(entries, retained=(keep,))
        self.assertFalse(result)
        self.assertEqual(receipt["retained"], [keep.label])
        self.assertEqual(receipt["retained_owned_directories"], [identities[keep.label]])
        self.assertIn("13", receipt["retained_owned_directories"][0]["path"])
        self.assert_sanitized(receipt, text, ["retained_release"])
        self.assertTrue(keep.directory.is_dir())

    def test_a_legitimate_creation_identity_containing_13_is_sanitized(self):
        keep = self.create("state")
        entries = self.owner.transfer()
        with failing_for(keep.label):
            result, receipt, text, identities = self.finalize(entries, retained=(keep,))
        self.assertFalse(result)
        self.assertEqual(receipt["retained_owned_directories"], [identities[keep.label]])
        for field in ("device", "inode", "parent_device", "parent_inode"):
            with self.subTest(field=field):
                candidate = json.loads(text)
                # Exercise receipt validation without fabricating filesystem authority.
                candidate["retained_owned_directories"][0][field] = 13
                self.assert_sanitized(candidate, json.dumps(candidate), ["retained_release"])
        self.assertTrue(keep.directory.is_dir())

    def test_the_sanitization_assertion_rejects_secrets_anywhere_in_the_receipt(self):
        keep = self.create("state")
        entries = self.owner.transfer()
        with failing_for(keep.label):
            result, receipt, text, identities = self.finalize(entries, retained=(keep,))
        self.assertFalse(result)
        self.assertEqual(receipt["retained_owned_directories"], [identities[keep.label]])
        self.assert_sanitized(receipt, text, ["retained_release"])
        for secret in (CANARY, CANARY_FILENAME):
            for location in ("failure", "identity", "receipt"):
                with self.subTest(secret=secret, location=location):
                    candidate = json.loads(text)
                    if location == "failure":
                        candidate["failures"][0]["error"] = secret
                    elif location == "identity":
                        candidate["retained_owned_directories"][0]["path"] = secret
                    else:
                        candidate["reason"] = secret
                    with self.assertRaises(AssertionError):
                        self.assert_sanitized(candidate, json.dumps(candidate), ["retained_release"])
        self.assertTrue(keep.directory.is_dir())

    def test_the_sanitization_assertion_rejects_failure_metadata_and_changed_values(self):
        self.create("victim")
        entries = self.owner.transfer()
        with patch.object(directory_operations, "_remove_entry_at", side_effect=canary_error()):
            result, receipt, text, _ = self.finalize(entries)
        self.assertFalse(result)
        self.assert_sanitized(receipt, text, ["remove"])
        for field, value in (("errno", 13), ("strerror", "Permission denied"),
                             ("filename", "private-input"), ("error", "Permission denied"),
                             ("operation", "raw_release"), ("error_class", "OSError")):
            with self.subTest(field=field):
                candidate = json.loads(text)
                candidate["failures"][0][field] = value
                with self.assertRaises(AssertionError):
                    self.assert_sanitized(candidate, json.dumps(candidate), ["remove"])

    def test_a_raw_capture_release_fault_is_sanitized_and_the_captures_stay(self):
        raw = self.owner.create("raw process captures", self.output / "processes", self.output)
        (raw.directory / "stdout").write_bytes(b"evidence\n")
        entries = self.owner.transfer()
        with failing_for("raw process captures"):
            result, receipt, text, identities = self.finalize(entries, raw=raw)
        self.assertFalse(result)
        self.assert_sanitized(receipt, text, ["raw_release"])
        self.assertEqual(receipt["raw_process_captures"], "retained")
        self.assertEqual(receipt["retained_owned_directories"], [identities["raw process captures"]])
        self.assertEqual((raw.directory / "stdout").read_bytes(), b"evidence\n")

    def test_a_deadline_fault_retains_everything_with_a_fixed_error(self):
        first = self.create("first")
        second = self.create("second")
        entries = self.owner.transfer()
        with patch.object(directory_finalization, "_check_cleanup_deadline", side_effect=canary_error(RuntimeError)):
            result, receipt, text, identities = self.finalize(entries)
        self.assertFalse(result)
        self.assert_sanitized(receipt, text, ["deadline"], error_class="RuntimeError")
        self.assertEqual(receipt["failures"][0]["error"], "acceptance cleanup deadline expired")
        self.assertEqual(receipt["retained"], ["second", "first"])
        self.assertEqual(receipt["retained_owned_directories"], [identities["second"], identities["first"]])
        self.assertTrue(first.directory.is_dir() and second.directory.is_dir())

    def test_an_expired_real_deadline_uses_the_same_fixed_error(self):
        self.create("only")
        entries = self.owner.transfer()
        with patch.object(directory_finalization, "failure_cleanup_deadline", return_value=0):
            result, receipt, text, _ = self.finalize(entries)
        self.assertFalse(result)
        self.assert_sanitized(receipt, text, ["deadline"], error_class="RuntimeError")

    def test_a_consume_fault_after_a_deadline_is_sanitized(self):
        self.create("first")
        self.create("second")
        entries = self.owner.transfer()
        real = directory_operations.OwnedDirectory.consume

        def consume_then_fail(token):
            real(token)
            raise canary_error(RuntimeError)

        with patch.object(directory_finalization, "_check_cleanup_deadline", side_effect=canary_error(RuntimeError)), \
                patch.object(directory_operations.OwnedDirectory, "consume", consume_then_fail):
            result, receipt, text, _ = self.finalize(entries)
        self.assertFalse(result)
        self.assert_sanitized(receipt, text, ["deadline", "consume", "consume"], error_class="RuntimeError")
        self.assertEqual([item["label"] for item in receipt["failures"]], ["second", "second", "first"])

    def test_the_status_and_completed_paths_are_unchanged(self):
        self.create("owned")
        entries = self.owner.transfer()
        result, receipt, text, _ = self.finalize(entries, success=True)
        self.assertTrue(result)
        self.assertEqual((receipt["status"], receipt["removed"], receipt["retained"]), ("removed", ["owned"], []))
        self.assertNotIn("failures", receipt)
        self.assertNotIn(CANARY, text)
        self.assertFalse((self.root / "owned").exists())
        self.assertEqual(os.listdir(self.neighbour), ["keep.txt"])


if __name__ == "__main__":
    unittest.main()
