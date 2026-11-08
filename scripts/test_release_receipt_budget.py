"""Exercise large release receipts through canonical production and replay paths."""

from __future__ import annotations

from argparse import Namespace
import json
from pathlib import Path
import unittest
from unittest.mock import patch

from scripts.release_tests.release_fixture import FixtureTree
from scripts.acceptance.native_evidence.contract import MAX_AGGREGATE_BYTES
from scripts.acceptance.native_evidence.contract import MAX_RECEIPT_BYTES
from scripts.acceptance.evidence.digests import sha as sha256
from scripts.release_verification import filesystem as receipt_files
from scripts.release_verification.contract import MAX_RECEIPT
from scripts.acceptance.evidence.source import VerificationError


def _mapping(value: object, label: str) -> dict[str, object]:
    if not isinstance(value, dict):
        raise AssertionError(f"{label} must be an object")
    return value


def _json_object(data: bytes, label: str) -> dict[str, object]:
    value: object = json.loads(data)
    return _mapping(value, label)


def _long_private_root(base: Path) -> Path:
    root = base.resolve(strict=True)
    for component in ("a" * 140, "b" * 140, "c" * 140, "d" * 140):
        root /= component
    root.mkdir(parents=True)
    return root.resolve(strict=True)


class ReleaseReceiptBudgetTests(unittest.TestCase):
    def _containing_tree(self, prefix: str) -> FixtureTree:
        """The owner of the long private root; it observes each release fixture child placed below it."""
        tree = FixtureTree.open(prefix=prefix, label=f"{prefix} tree")
        self.addCleanup(tree.dispose)
        return tree

    def test_large_prepared_receipt_replays_and_keeps_both_size_limits(self) -> None:
        from scripts import verify_release_archive as verifier
        from scripts.test_verify_release_archive import (
            REVISION,
            TAG,
            ReleaseArchiveVerificationTests,
        )

        outer = self._containing_tree("release-receipt-replay")
        private_root = _long_private_root(outer.root)
        inner_parent = private_root / "release-fixtures"
        inner_parent.mkdir()
        fixture = ReleaseArchiveVerificationTests(parent=inner_parent, containing=(outer,))
        with fixture.prepared():
            self._assert_large_replay(fixture, verifier, TAG, REVISION)
        self.assertEqual(fixture.root.parent, inner_parent)
        self.assertGreaterEqual(len(str(fixture.root / "native-aggregate-fixture")), 512)

    def _assert_large_replay(self, fixture, verifier, tag: str, revision: str) -> None:
        self.assertEqual(fixture.verify(), 0)
        archive_bytes = fixture.receipt_path.read_bytes()
        aggregate_path = fixture.write_aggregate()
        exit_path = fixture.root / "acceptance.exit"
        exit_path.write_text("0\n", encoding="ascii")
        final_path = fixture.root / "large-native-receipt.json"
        args = Namespace(
            receipt=str(final_path), archive_receipt=str(fixture.receipt_path),
            aggregate_receipt=str(aggregate_path), assets_dir=str(fixture.assets),
            extract_dir=str(fixture.extract), acceptance_outcome="success",
            exit_code_file=str(exit_path), tag=tag, source_revision=revision,
        )
        self._interrupt_terminal_write(fixture, verifier, args, final_path)
        self._assert_receipt_bounds(final_path, fixture.root)
        prepared = _json_object(final_path.read_bytes(), "replayed prepared receipt")
        self.assertEqual(verifier.finalize_native(args), 0)
        terminal_bytes = final_path.read_bytes()
        self.assertGreater(len(terminal_bytes), MAX_RECEIPT_BYTES)
        self.assertLessEqual(len(terminal_bytes), MAX_AGGREGATE_BYTES)
        self.assertEqual(fixture.receipt_path.read_bytes(), archive_bytes)
        terminal = _json_object(terminal_bytes, "terminal native receipt")
        archived = _json_object(archive_bytes, "archive receipt")
        self.assertEqual(terminal["archive_receipt_sha256"], sha256(archive_bytes))
        self.assertEqual(terminal["artifact"], archived["artifact"])
        self.assertEqual(terminal["delegate_build_identity"], archived["delegate_build_identity"])
        self.assertEqual(terminal.get("source_harness"), prepared.get("source_harness"))
        self.assertEqual(
            _mapping(terminal.get("native_acceptance"), "terminal acceptance").get(
                "case_receipt_hashes",
            ),
            _mapping(prepared.get("native_acceptance"), "prepared acceptance").get(
                "case_receipt_hashes",
            ),
        )
        self._assert_completed_journal(terminal)

    def _interrupt_terminal_write(self, fixture, verifier, args: Namespace,
                                  final_path: Path) -> None:
        interrupted = False
        original_write = receipt_files.write_receipt

        def fail_terminal(path: Path, receipt: dict[str, object], *,
                          max_bytes: int = MAX_AGGREGATE_BYTES) -> None:
            nonlocal interrupted
            completion = receipt.get("completion")
            cleanup = receipt.get("cleanup")
            if (not interrupted and isinstance(completion, dict)
                    and completion.get("state") == "complete"
                    and isinstance(cleanup, dict) and cleanup.get("status") == "complete"):
                interrupted = True
                raise VerificationError("forced_terminal_write_failure", "write-receipt")
            original_write(path, receipt, max_bytes=max_bytes)

        with patch.object(receipt_files, "write_receipt", side_effect=fail_terminal):
            with self.assertRaisesRegex(VerificationError, "forced_terminal_write_failure"):
                verifier.finalize_native(args)
        self.assertTrue(interrupted)
        prepared_bytes = final_path.read_bytes()
        self.assertGreater(len(prepared_bytes), MAX_RECEIPT_BYTES)
        self.assertLessEqual(len(prepared_bytes), MAX_AGGREGATE_BYTES)
        prepared = _json_object(prepared_bytes, "prepared native receipt")
        self.assertEqual(_mapping(prepared.get("completion"), "prepared completion").get("state"),
                         "prepared")
        self.assertEqual(_mapping(prepared.get("cleanup"), "prepared cleanup").get("status"),
                         "prepared")
        self.assertFalse(fixture.assets.exists())

    def _assert_receipt_bounds(self, large_path: Path, root: Path) -> None:
        self.assertEqual(MAX_RECEIPT, MAX_RECEIPT_BYTES)
        large_receipt = _json_object(large_path.read_bytes(), "large receipt")
        control_path = root / "control-bound.json"
        with self.assertRaises(VerificationError):
            receipt_files.write_receipt(
                control_path, large_receipt, max_bytes=MAX_RECEIPT_BYTES,
            )
        self.assertFalse(control_path.exists())
        with self.assertRaises(VerificationError):
            receipt_files.read_receipt(large_path, max_bytes=MAX_RECEIPT_BYTES)
        oversized = dict(large_receipt)
        oversized["padding"] = "x" * MAX_AGGREGATE_BYTES
        oversized_path = root / "aggregate-bound.json"
        with self.assertRaises(VerificationError):
            receipt_files.write_receipt(oversized_path, oversized)
        self.assertFalse(oversized_path.exists())
        oversized_bytes = (json.dumps(
            oversized, sort_keys=True, separators=(",", ":"), allow_nan=False,
        ) + "\n").encode()
        self.assertGreater(len(oversized_bytes), MAX_AGGREGATE_BYTES)
        oversized_path.write_bytes(oversized_bytes)
        with self.assertRaises(VerificationError):
            receipt_files.read_receipt(oversized_path)

    def _assert_completed_journal(self, receipt: dict[str, object]) -> None:
        cleanup = _mapping(receipt.get("cleanup"), "terminal cleanup")
        transaction = _mapping(cleanup.get("transaction"), "terminal cleanup transaction")
        journal_path = Path(str(transaction.get("journal_path")))
        journal_bytes = journal_path.read_bytes()
        journal = _json_object(journal_bytes, "cleanup journal")
        self.assertEqual(journal.get("state"), "complete")
        self.assertEqual(journal.get("authorization_sha256"),
                         transaction.get("authorization_sha256"))
        self.assertEqual(journal.get("target"), transaction.get("target_identity"))
        self.assertEqual(transaction.get("journal_sha256"), sha256(journal_bytes))



if __name__ == "__main__":
    unittest.main()
