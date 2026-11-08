"""Release verification consumes the validated native aggregate schema."""
from argparse import Namespace
from pathlib import Path
from scripts.acceptance.native_evidence.tests import test_aggregate_schema as aggregate_tests
from scripts.release_verification import verification
from scripts.release_verification import filesystem
from scripts.acceptance.evidence import source as source_evidence
from scripts.release_verification.contract import _ArchiveEvidence
from scripts.acceptance.evidence.source import VerificationError
EQUAL_TO_ONE = aggregate_tests.EQUAL_TO_ONE

class ReleaseAggregateSchemaTests(aggregate_tests.AggregateSchemaTests):
    def release_arguments(self, directory: Path):
        exit_file = directory / "exit-code"
        exit_file.write_text("0\n", encoding="ascii")
        return Namespace(tag=None, source_revision=None, exit_code_file=str(exit_file),
                               acceptance_outcome="success")

    def archive_arguments(self, path: Path) -> _ArchiveEvidence:
        return _ArchiveEvidence(
            aggregate_path=path, extracted=self.root, platform={"os": "linux", "arch": "amd64"},
            release_identity={}, artifact={}, observation={}, build={}, receipt_sha256="a" * 64)

    def test_release_verification_accepts_integer_one(self):
        path, _ = self.build("release-positive")
        archive = self.archive_arguments(path)
        aggregate, digest, recorded = verification._read_native_aggregate(
            self.release_arguments(self.root), archive)
        self.assertEqual((aggregate["schema_version"], recorded), (1, 0))
        self.assertEqual(len(digest), 64)

    def test_release_verification_preserves_the_error_code_and_phase(self):
        for label, schema in EQUAL_TO_ONE:
            with self.subTest(schema=label):
                path = self.malformed("release-" + label, schema)
                archive = self.archive_arguments(path)
                with self.assertRaises(VerificationError) as raised:
                    verification._read_native_aggregate(self.release_arguments(self.root), archive)
                self.assertEqual((raised.exception.code, raised.exception.stage),
                                 ("aggregate_receipt_invalid", "validate-aggregate-receipt"))


    def test_shared_source_errors_preserve_release_handler_codes_and_stages(self):
        absent = self.root / "missing-receipt"
        self.assertIsNone(filesystem.exit_code(str(absent)))
        with self.assertRaises(VerificationError) as missing:
            filesystem.read_receipt(absent)
        self.assertEqual((missing.exception.code, missing.exception.stage),
                         ("archive_receipt_missing_or_invalid", "read-archive-receipt"))
        with self.assertRaises(VerificationError) as environment:
            source_evidence.child_environment({})
        self.assertEqual((environment.exception.code, environment.exception.stage),
                         ("build_path_unavailable", "build-candidate"))
