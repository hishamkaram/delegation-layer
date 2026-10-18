"""Exercise large release receipts through canonical production and replay paths."""

from __future__ import annotations

from argparse import Namespace
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts import native_acceptance_evidence as evidence
from scripts.delegate_acceptance.native_evidence.contract import (
    MAX_AGGREGATE_BYTES,
    MAX_RECEIPT_BYTES,
)
from scripts.delegate_acceptance.native_evidence.public_contract import (
    CASE_DEFINITIONS,
    MAX_PUBLIC_ZIP_BYTES,
    ProofExpectation,
    sha256,
)
from scripts.delegate_acceptance.native_evidence.public_projection import build_public_proof
from scripts.delegate_acceptance.native_evidence.public_validation import validate_public_proof
from scripts.delegate_acceptance.release_verification import filesystem as receipt_files
from scripts.delegate_acceptance.release_verification.contract import (
    MAX_RECEIPT,
    VerificationError,
)


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


def _redirect_release_fixture_tempdirs(inner_parent: Path, roots: list[Path]):
    original = tempfile.TemporaryDirectory

    def create_tempdir(*args, **kwargs):
        is_release_fixture = kwargs.get("prefix") == "release-archive-test-"
        if is_release_fixture:
            kwargs["dir"] = str(inner_parent)
        temporary = original(*args, **kwargs)
        if is_release_fixture:
            roots.append(Path(temporary.name).resolve(strict=True))
        return temporary

    return patch.object(tempfile, "TemporaryDirectory", side_effect=create_tempdir)


class ReleaseReceiptBudgetTests(unittest.TestCase):
    def test_long_paths_project_real_large_native_receipts_with_all_bindings(self) -> None:
        from scripts.test_public_native_proof import _PublicNativeFixture

        with tempfile.TemporaryDirectory(prefix="release-receipt-budget-") as temporary:
            private_root = _long_private_root(Path(temporary))
            inner_parent = private_root / "release-fixtures"
            inner_parent.mkdir()
            outer_root = private_root / "outer-aggregate-root"
            outer_root.mkdir()
            fixture_roots: list[Path] = []
            with _redirect_release_fixture_tempdirs(inner_parent, fixture_roots):
                producer = _PublicNativeFixture(outer_root.resolve(strict=True))
                stage = producer.build_stage()
            self.assertEqual(len(fixture_roots), 1)
            self.assertGreaterEqual(len(str(outer_root / "aggregate")), 512)
            self.assertGreaterEqual(len(str(fixture_roots[0])), 512)
            self._assert_large_public_proof(stage, outer_root, fixture_roots, producer.expectation)

    def _assert_large_public_proof(
            self, stage: Path, root: Path, fixture_roots: list[Path],
            expectation: ProofExpectation | None) -> None:
        self.assertIsNotNone(expectation)
        if expectation is None:
            raise AssertionError("producer expectation missing")
        archive_bytes = (stage / "archive-receipt.json").read_bytes()
        native_bytes = (stage / "native-acceptance-receipt.json").read_bytes()
        self.assertLessEqual(len(archive_bytes), MAX_AGGREGATE_BYTES)
        self.assertGreater(len(native_bytes), MAX_RECEIPT_BYTES)
        self.assertLessEqual(len(native_bytes), MAX_AGGREGATE_BYTES)
        archive = _json_object(archive_bytes, "archive receipt")
        native = _json_object(native_bytes, "native receipt")
        self._assert_receipt_bindings(archive, native, expectation, sha256(archive_bytes))
        stage_snapshot = {
            path.relative_to(stage).as_posix(): sha256(path.read_bytes())
            for path in stage.rglob("*") if path.is_file()
        }
        public_path = root / "public-proof.zip"
        projected = build_public_proof(stage, public_path, expectation)
        self.assertEqual(projected.get("status"), "PASS")
        public_bytes = public_path.read_bytes()
        self.assertLessEqual(len(public_bytes), MAX_PUBLIC_ZIP_BYTES)
        result = validate_public_proof(public_bytes, expectation)
        self.assertEqual(result.get("status"), "PASS")
        self.assertEqual(result.get("case_results_verdict"), "PASS")
        self.assertEqual(result.get("expected_case_count"), len(CASE_DEFINITIONS))
        identity = _mapping(result.get("identity"), "validated public identity")
        self.assertEqual(
            _mapping(identity.get("source_harness"), "public source fingerprint").get("sha256"),
            _mapping(native.get("source_harness"), "native source fingerprint").get("sha256"),
        )
        self.assertEqual(
            _mapping(identity.get("archive_source_harness"), "public archive fingerprint").get(
                "sha256",
            ),
            _mapping(archive.get("source_harness"), "archive source fingerprint").get("sha256"),
        )
        self.assertEqual(identity.get("release_identity"), archive.get("release_identity"))
        self.assertEqual(
            _mapping(identity.get("archive"), "public archive").get("sha256"),
            expectation.archive_sha256,
        )
        native_binaries = _mapping(native.get("binaries"), "native binaries")
        self.assertEqual(identity.get("binaries"), {
            "delegate": _mapping(native_binaries.get("delegate"), "delegate").get("sha256"),
            "runner": _mapping(native_binaries.get("delegate-run"), "delegate-run").get("sha256"),
            "pueue": _mapping(native_binaries.get("pueue"), "pueue").get("sha256"),
            "pueued": _mapping(native_binaries.get("pueued"), "pueued").get("sha256"),
        })
        self.assertEqual(stage_snapshot, {
            path.relative_to(stage).as_posix(): sha256(path.read_bytes())
            for path in stage.rglob("*") if path.is_file()
        })
        self.assertNotIn(str(root).encode(), public_bytes)
        self.assertNotIn(str(fixture_roots[0]).encode(), public_bytes)

    def _assert_receipt_bindings(
            self, archive: dict[str, object], native: dict[str, object],
            expectation: ProofExpectation, archive_digest: str) -> None:
        rows = _mapping(native.get("native_acceptance"), "native acceptance").get(
            "case_receipt_hashes",
        )
        if not isinstance(rows, list):
            raise AssertionError("native case receipt hashes missing")
        case_ids = {_mapping(row, "case receipt hash").get("case_id") for row in rows}
        self.assertEqual(case_ids, {case.case_id for case in evidence.EXPECTED_CASES})
        self.assertEqual(len(rows), len(evidence.EXPECTED_CASES))
        self._assert_fingerprint(archive.get("source_harness"), None)
        self._assert_fingerprint(native.get("source_harness"), expectation.source_harness_sha256)
        self.assertEqual(
            _mapping(native.get("source_harness"), "native source fingerprint").get("sha256"),
            expectation.source_harness_sha256,
        )
        self.assertEqual(native.get("archive_receipt_sha256"), archive_digest)
        archive_artifact = _mapping(archive.get("artifact"), "archive artifact")
        self.assertEqual(archive_artifact.get("archive_sha256"), expectation.archive_sha256)
        native_binaries = _mapping(native.get("binaries"), "native binaries")
        archive_binaries = _mapping(archive.get("binaries"), "archive binaries")
        native_hashes = {
            "delegate": _mapping(native_binaries.get("delegate"), "delegate").get("sha256"),
            "runner": _mapping(native_binaries.get("delegate-run"), "delegate-run").get("sha256"),
            "pueue": _mapping(native_binaries.get("pueue"), "pueue").get("sha256"),
            "pueued": _mapping(native_binaries.get("pueued"), "pueued").get("sha256"),
        }
        archive_hashes = {
            "delegate": _mapping(archive_binaries.get("delegate"), "delegate").get("sha256"),
            "runner": _mapping(archive_binaries.get("delegate-run"), "delegate-run").get("sha256"),
            "pueue": _mapping(archive_binaries.get("pueue"), "pueue").get("sha256"),
            "pueued": _mapping(archive_binaries.get("pueued"), "pueued").get("sha256"),
        }
        self.assertEqual(native_hashes, archive_hashes)
        self.assertEqual(native_hashes, {
            "delegate": expectation.delegate_sha256,
            "runner": expectation.runner_sha256,
            "pueue": expectation.pueue_sha256,
            "pueued": expectation.pueued_sha256,
        })

    def _assert_fingerprint(self, value: object, expected_digest: str | None) -> None:
        fingerprint = _mapping(value, "source fingerprint")
        files = fingerprint.get("files")
        self.assertIsInstance(files, list)
        digest = sha256(json.dumps(files, sort_keys=True, separators=(",", ":")).encode())
        self.assertEqual(fingerprint.get("sha256"), digest)
        if expected_digest is not None:
            self.assertEqual(digest, expected_digest)

    def test_large_prepared_receipt_replays_and_keeps_both_size_limits(self) -> None:
        from scripts import verify_release_archive as verifier
        from scripts.test_verify_release_archive import (
            REVISION,
            TAG,
            ReleaseArchiveVerificationTests,
        )

        with tempfile.TemporaryDirectory(prefix="release-receipt-replay-") as temporary:
            private_root = _long_private_root(Path(temporary))
            inner_parent = private_root / "release-fixtures"
            inner_parent.mkdir()
            fixture_roots: list[Path] = []
            with _redirect_release_fixture_tempdirs(inner_parent, fixture_roots):
                fixture = ReleaseArchiveVerificationTests(
                    "test_success_binds_exact_archive_binaries_and_default_path",
                )
                fixture.setUp()
                try:
                    self._assert_large_replay(fixture, verifier, TAG, REVISION)
                finally:
                    fixture.tearDown()
            self.assertEqual(len(fixture_roots), 1)
            self.assertGreaterEqual(len(str(fixture_roots[0] / "native-aggregate-fixture")), 512)

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
        self._assert_receipt_bindings(
            archived, terminal, _expectation_from_receipts(archived, terminal),
            sha256(archive_bytes),
        )
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


def _expectation_from_receipts(
        archive: dict[str, object], native: dict[str, object]) -> ProofExpectation:
    release = _mapping(archive.get("release_identity"), "release identity")
    platform = _mapping(archive.get("platform"), "platform")
    artifact = _mapping(archive.get("artifact"), "artifact")
    binaries = _mapping(native.get("binaries"), "native binaries")
    source = _mapping(native.get("source_harness"), "source fingerprint")

    def binary_hash(name: str) -> str:
        value = _mapping(binaries.get(name), name).get("sha256")
        if not isinstance(value, str):
            raise AssertionError(f"{name} binary hash missing")
        return value

    return ProofExpectation(
        repo=_text(release.get("repository"), "repository"),
        tag=_text(archive.get("tag"), "tag"),
        release_revision=_text(release.get("commit_sha"), "release revision"),
        harness_revision=_text(archive.get("harness_revision"), "harness revision"),
        platform_os=_text(platform.get("os"), "platform OS"),
        platform_arch=_text(platform.get("arch"), "platform architecture"),
        opencode_model="fixture-model", ccr_model="fixture-model",
        source_harness_sha256=_text(source.get("sha256"), "source fingerprint"),
        archive_sha256=_text(artifact.get("archive_sha256"), "archive digest"),
        delegate_sha256=binary_hash("delegate"), runner_sha256=binary_hash("delegate-run"),
        pueue_sha256=binary_hash("pueue"), pueued_sha256=binary_hash("pueued"),
    )


def _text(value: object, label: str) -> str:
    if not isinstance(value, str):
        raise AssertionError(f"{label} missing")
    return value


if __name__ == "__main__":
    unittest.main()
