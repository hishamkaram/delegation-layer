"""The typed block reason through the raw receipt and the compact case codec."""

from enum import Enum
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts import acceptance_native as entry
from scripts import native_acceptance_evidence as evidence
# Import the module, not the TestCase, so its tests are discovered only once.
from scripts import test_native_acceptance_evidence as evidence_tests
from scripts.delegate_acceptance.native import acceptance as gate
from scripts.delegate_acceptance.native.errors import BlockedFailure
from scripts.delegate_acceptance.native_evidence import aggregate as aggregate_operations
from scripts.delegate_acceptance.native_evidence import cases as case_operations
from scripts.delegate_acceptance.native_evidence import contract
from scripts.delegate_acceptance.native_evidence import validation as evidence_validation
from scripts.delegate_acceptance.native_evidence.block_reason import (
    compact_block_reason, encode_block_reason, parse_block_reason, source_block_reason,
)
from scripts.delegate_acceptance.native_evidence.contract import (
    BlockReason, EvidenceError, SCHEMA_VERSION,
)
from scripts.delegate_acceptance.release_verification.candidate_readiness import NEUTRAL_BLOCK_REASONS


class LookalikeReason(Enum):
    """An unrelated enum whose members share the canonical reason values."""

    AUTH_UNAVAILABLE = "auth-unavailable"
    UNCLASSIFIED = "unclassified"


# Values that are neither None nor a canonical reason member.
NOT_A_REASON = (LookalikeReason.AUTH_UNAVAILABLE, "auth-unavailable", "", 1, 0, False, object())


def compact(status="BLOCKED", version=contract.COMPACT_CASE_SCHEMA_VERSION, **fields):
    value = {"schema_version": version, "kind": "native-acceptance-case", "status": status}
    value.update(fields)
    return value


class ParseTests(unittest.TestCase):
    def test_each_canonical_value_parses_to_its_member(self):
        for reason in BlockReason:
            self.assertIs(parse_block_reason(reason.value), reason)

    def test_noncanonical_values_are_refused(self):
        for value in ("AUTH_UNAVAILABLE", "Auth-Unavailable", "auth-unavailable ", "auth_unavailable",
                      "", "neutral", None, 1, True, [], {}, BlockReason.AUTH_UNAVAILABLE):
            with self.subTest(value=value), self.assertRaisesRegex(EvidenceError, "block_reason_invalid"):
                parse_block_reason(value)


class SourceReasonTests(unittest.TestCase):
    def test_blocked_source_carries_its_reason(self):
        for reason in BlockReason:
            self.assertEqual(source_block_reason({"block_reason": reason.value}, "BLOCKED"), (reason, None))

    def test_legacy_blocked_source_is_unclassified_never_neutral(self):
        self.assertEqual(source_block_reason({"status": "BLOCKED"}, "BLOCKED"),
                         (BlockReason.UNCLASSIFIED, None))
        self.assertEqual(source_block_reason(None, "BLOCKED"), (BlockReason.UNCLASSIFIED, None))

    def test_unusable_reason_falls_to_unclassified_with_an_evidence_error(self):
        for value in ("authentication", None, 2, "AUTH-UNAVAILABLE"):
            with self.subTest(value=value):
                self.assertEqual(source_block_reason({"block_reason": value}, "BLOCKED"),
                                 (BlockReason.UNCLASSIFIED, "source_block_reason_invalid"))

    def test_pass_and_fail_sources_must_not_claim_a_block(self):
        for status in ("PASS", "FAIL"):
            self.assertEqual(source_block_reason({"status": "x"}, status), (None, None))
            self.assertEqual(source_block_reason(None, status), (None, None))
            for value in (BlockReason.AUTH_UNAVAILABLE.value, None):
                self.assertEqual(source_block_reason({"block_reason": value}, status),
                                 (None, "source_block_reason_unexpected"))


class EncodeTests(unittest.TestCase):
    def test_reason_is_non_null_exactly_for_blocked(self):
        for reason in BlockReason:
            self.assertEqual(encode_block_reason("BLOCKED", reason), reason.value)
        self.assertIsNone(encode_block_reason("PASS", None))
        self.assertIsNone(encode_block_reason("FAIL", None))

    def test_mismatches_and_unknown_statuses_are_refused(self):
        for status, reason in (("BLOCKED", None), ("PASS", BlockReason.AUTH_UNAVAILABLE),
                               ("FAIL", BlockReason.EXECUTABLE_UNAVAILABLE),
                               ("BLOCKED", "auth-unavailable"), ("DONE", None), ("blocked", BlockReason.UNCLASSIFIED)):
            with self.subTest(status=status, reason=reason), self.assertRaisesRegex(
                    EvidenceError, "block_reason_status_mismatch"):
                encode_block_reason(status, reason)

    def test_only_none_or_a_canonical_member_is_accepted(self):
        cases = [(status, value) for status in ("PASS", "FAIL") for value in NOT_A_REASON]
        cases += [("BLOCKED", member) for member in LookalikeReason]
        for status, reason in cases:
            with self.subTest(status=status, reason=reason):
                with self.assertRaises(Exception) as caught:
                    encode_block_reason(status, reason)
                self.assertIs(type(caught.exception), EvidenceError)
                self.assertEqual(caught.exception.args, ("block_reason_status_mismatch",))


class CompactCodecTests(unittest.TestCase):
    def test_version_two_round_trips_every_reason_exactly(self):
        for reason in BlockReason:
            case = compact(block_reason=encode_block_reason("BLOCKED", reason))
            self.assertIs(compact_block_reason(case), reason)

    def test_version_two_pass_and_fail_carry_an_explicit_null(self):
        for status in ("PASS", "FAIL"):
            self.assertIsNone(compact_block_reason(compact(status, block_reason=None)))

    def test_version_two_requires_the_field(self):
        for status in ("PASS", "BLOCKED", "FAIL"):
            with self.subTest(status=status), self.assertRaisesRegex(
                    EvidenceError, "aggregate_case_block_reason_missing"):
                compact_block_reason(compact(status))

    def test_pass_and_fail_cannot_claim_an_auth_block(self):
        for status in ("PASS", "FAIL"):
            for value in (BlockReason.AUTH_UNAVAILABLE.value, BlockReason.UNCLASSIFIED.value, ""):
                with self.subTest(status=status, value=value), self.assertRaisesRegex(
                        EvidenceError, "aggregate_case_block_reason_unexpected"):
                    compact_block_reason(compact(status, block_reason=value))

    def test_blocked_requires_a_canonical_reason(self):
        for value in (None, "", "auth", "AUTH-UNAVAILABLE", "auth-unavailable\n", 0, False, ["auth-unavailable"]):
            with self.subTest(value=value), self.assertRaisesRegex(
                    EvidenceError, "aggregate_case_block_reason_invalid"):
                compact_block_reason(compact(block_reason=value))

    def test_legacy_version_one_blocked_is_explicitly_unclassified(self):
        legacy = compact(version=SCHEMA_VERSION)
        self.assertEqual(contract.COMPACT_CASE_LEGACY_SCHEMA_VERSION, SCHEMA_VERSION)
        self.assertIs(compact_block_reason(legacy), BlockReason.UNCLASSIFIED)
        self.assertNotIn(compact_block_reason(legacy), NEUTRAL_BLOCK_REASONS)

    def test_legacy_version_one_pass_and_fail_have_no_reason(self):
        for status in ("PASS", "FAIL"):
            self.assertIsNone(compact_block_reason(compact(status, version=SCHEMA_VERSION)))

    def test_legacy_version_one_is_closed_without_the_field(self):
        for status in ("PASS", "BLOCKED", "FAIL"):
            for value in (None, BlockReason.AUTH_UNAVAILABLE.value):
                with self.subTest(status=status, value=value), self.assertRaisesRegex(
                        EvidenceError, "aggregate_case_block_reason_unexpected"):
                    compact_block_reason(compact(status, version=SCHEMA_VERSION, block_reason=value))

    def test_unknown_or_mistyped_versions_and_statuses_are_refused(self):
        for version in (0, 3, None, "2", 2.0, True, [2]):
            with self.subTest(version=version), self.assertRaisesRegex(
                    EvidenceError, "aggregate_case_schema_invalid"):
                compact_block_reason(compact(version=version, block_reason="unclassified"))
        for status in ("blocked", "DONE", None, [], {}):
            with self.subTest(status=status), self.assertRaisesRegex(
                    EvidenceError, "aggregate_case_status_invalid"):
                compact_block_reason(compact(status, block_reason=None))

    def test_the_compact_version_is_separate_from_the_aggregate_schema(self):
        self.assertEqual(SCHEMA_VERSION, 1)
        self.assertEqual(contract.COMPACT_CASE_SCHEMA_VERSION, 2)


class CleanupAuthorizationBindingTests(unittest.TestCase):
    def authorization(self, **changes):
        value = {"kind": "native-acceptance-case", "gate": "acceptance-native", "case_id": "case-01",
                 "status": "BLOCKED", "exit_code": 2, "cleanup": {"observation": {}, "raw_before": {}},
                 **changes}
        return contract._case_cleanup_authorization(value)

    def test_version_two_authorization_binds_the_reason(self):
        v2 = {"schema_version": contract.COMPACT_CASE_SCHEMA_VERSION}
        auth = self.authorization(**v2, block_reason=BlockReason.AUTH_UNAVAILABLE.value)
        self.assertNotEqual(auth, self.authorization(**v2, block_reason=BlockReason.UNCLASSIFIED.value))
        self.assertNotEqual(auth, self.authorization(**v2, block_reason=None))
        self.assertEqual(auth, self.authorization(**v2, block_reason=BlockReason.AUTH_UNAVAILABLE.value))

    def test_version_one_authorization_ignores_the_field(self):
        v1 = {"schema_version": SCHEMA_VERSION}
        self.assertEqual(self.authorization(**v1), self.authorization(**v1, block_reason="auth-unavailable"))


class RawToCompactTests(unittest.TestCase):
    """A real failure receipt written by the entry point decodes to the same reason."""

    def failure_receipt(self, error=None, *, result=None, summary=None, extra=()):
        class Run:
            model_discovery_exit_code = 2
            model_discovery_summary = summary

            def run(self):
                if error is not None:
                    raise error
                return result

            def finalize(self, _success, *, blocked=False):
                return True

        with tempfile.TemporaryDirectory(prefix="compact-block-reason-") as directory:
            output = Path(directory) / "evidence"
            output.mkdir()
            with patch.object(entry, "choose_output", return_value=output), \
                    patch.object(entry, "NativeAcceptance", return_value=Run()), \
                    patch("sys.stderr"), patch("sys.stdout"):
                code = entry.main(["--provider", "pi:json", "--permission", "read-only",
                                   "--output", str(output), *extra])
            return code, json.loads((output / "failure.json").read_text(encoding="utf-8"))

    def compact_reason(self, status, source):
        reason, error = source_block_reason(source, status)
        self.assertIsNone(error)
        case = compact(status, block_reason=encode_block_reason(status, reason))
        return compact_block_reason(json.loads(json.dumps(case)))

    def test_every_exception_reason_survives_raw_to_compact(self):
        for reason in BlockReason:
            with self.subTest(reason=reason):
                code, source = self.failure_receipt(BlockedFailure("blocked", reason=reason))
                self.assertEqual(code, 2)
                self.assertIs(self.compact_reason("BLOCKED", source), reason)

    def test_discovery_code_two_reason_survives_raw_to_compact(self):
        auth = {"status": "blocked", "reason_code": "authentication_unavailable"}
        empty = {"status": "unavailable", "reason_code": "models_unavailable"}
        for summary, expected in ((auth, BlockReason.AUTH_UNAVAILABLE), (empty, BlockReason.UNCLASSIFIED)):
            with self.subTest(summary=summary):
                code, source = self.failure_receipt(result=2, summary=summary, extra=("--discover-models",))
                self.assertIs(self.compact_reason("BLOCKED", source), expected)

    def test_failed_receipts_carry_no_reason_into_the_compact_codec(self):
        code, source = self.failure_receipt(gate.AcceptanceFailure("provider rejected"))
        self.assertEqual(code, 1)
        self.assertEqual(source_block_reason(source, "FAIL"), (None, None))
        self.assertIsNone(self.compact_reason("FAIL", source))

    def test_a_failed_receipt_cannot_be_read_as_blocked_with_a_neutral_reason(self):
        _code, source = self.failure_receipt(gate.AcceptanceFailure("provider rejected"))
        reason, error = source_block_reason(source, "BLOCKED")
        self.assertIs(reason, BlockReason.UNCLASSIFIED)
        self.assertIsNone(error)


class ProducerValidatorTests(unittest.TestCase):
    """The real aggregate producer and validator, driven by raw receipts on disk."""

    INVALID = ("authentication", None, 2, "AUTH-UNAVAILABLE", "", ["auth-unavailable"])

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-block-reason-aggregate-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.fixture = object.__new__(evidence_tests.NativeAcceptanceEvidenceTests)
        self.fixture.root = self.root
        self.definition = evidence.EXPECTED_CASES[0]

    def build(self, label, *, status="BLOCKED", failed_index=None, edit=None):
        """Aggregate a fixture matrix after `edit(path, receipt)` rewrote case 0's raw receipt."""
        root = self.root / label
        matrix = self.fixture.make_matrix(root, status=status, failed_index=failed_index)
        directory = root / self.definition.case_output
        name = "failure.json" if status != "PASS" or failed_index == 0 else "success.json"
        if edit is not None:
            receipt = json.loads((directory / name).read_text(encoding="utf-8"))
            edit(receipt)
            (directory / name).write_text(json.dumps(receipt), encoding="utf-8")
        value, code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40)
        return root, directory, value, code

    @staticmethod
    def compact_of(case):
        return json.loads(Path(case["receipt_path"]).read_text(encoding="utf-8"))

    def assert_retained_unresolved(self, root, directory, value, code, error, reason):
        self.assertEqual((code, value["status"], value["unresolved_case_count"]), (1, "FAIL", 1))
        case = value["cases"][0]
        self.assertEqual(case["evidence_status"], "UNRESOLVED")
        compact = self.compact_of(case)
        self.assertEqual((compact["evidence_error"], compact["block_reason"]), (error, reason))
        self.assertEqual((compact["terminal_state"], compact["cleanup"]["raw_deleted"]),
                         ("published-unresolved", False))
        self.assertTrue(directory.is_dir())
        self.assertTrue(any(directory.iterdir()))
        compact_root = root / evidence.COMPACT_RECEIPT_ROOT_NAME
        self.assertFalse((compact_root / f"{self.definition.case_id}.cleanup.json").exists())

    def test_every_typed_block_reason_is_serialized_resolved_and_revalidated(self):
        for reason in BlockReason:
            with self.subTest(reason=reason):
                root, directory, value, code = self.build(
                    "typed-" + reason.value, edit=lambda receipt: receipt.update(block_reason=reason.value))
                self.assertEqual(code, 0)
                case = value["cases"][0]
                self.assertEqual(case["evidence_status"], "RESOLVED")
                compact = self.compact_of(case)
                self.assertEqual((compact["schema_version"], compact["block_reason"]),
                                 (contract.COMPACT_CASE_SCHEMA_VERSION, reason.value))
                self.assertFalse(directory.exists())
                evidence.validate_aggregate_receipt(root / evidence.AGGREGATE_RECEIPT_NAME)

    def test_legacy_blocked_source_without_a_reason_serializes_unclassified(self):
        root, directory, value, code = self.build("legacy-source")
        self.assertEqual(code, 0)
        for case in value["cases"]:
            self.assertEqual(case["evidence_status"], "RESOLVED")
            self.assertEqual(self.compact_of(case)["block_reason"], BlockReason.UNCLASSIFIED.value)
        evidence.validate_aggregate_receipt(root / evidence.AGGREGATE_RECEIPT_NAME)

    def test_invalid_blocked_reason_retains_raw_evidence_and_rejects_the_aggregate(self):
        for index, candidate in enumerate(self.INVALID):
            with self.subTest(candidate=candidate):
                root, directory, value, code = self.build(
                    f"invalid-{index}", edit=lambda receipt: receipt.update(block_reason=candidate))
                self.assert_retained_unresolved(
                    root, directory, value, code, "source_block_reason_invalid",
                    BlockReason.UNCLASSIFIED.value)
                self.assertTrue((directory / "failure.json").is_file())

    def test_pass_and_fail_sources_cannot_carry_a_block_reason(self):
        for candidate in (BlockReason.AUTH_UNAVAILABLE.value, BlockReason.UNCLASSIFIED.value, None):
            with self.subTest(status="PASS", candidate=candidate):
                root, directory, value, code = self.build(
                    f"pass-{candidate}", status="PASS",
                    edit=lambda receipt: receipt.update(block_reason=candidate))
                self.assert_retained_unresolved(
                    root, directory, value, code, "source_block_reason_unexpected", None)
                self.assertTrue((directory / "success.json").is_file())
            with self.subTest(status="FAIL", candidate=candidate):
                root, directory, value, code = self.build(
                    f"fail-{candidate}", status="PASS", failed_index=0,
                    edit=lambda receipt: receipt.update(block_reason=candidate))
                self.assert_retained_unresolved(
                    root, directory, value, code, "source_block_reason_unexpected", None)
                self.assertTrue((directory / "raw-provider-output").is_file())

    def test_earlier_source_errors_keep_their_precedence_over_block_metadata(self):
        def mismatched(receipt):
            receipt.update(status="failed", block_reason="authentication")

        root, directory, value, code = self.build("precedence-status", edit=mismatched)
        self.assert_retained_unresolved(
            root, directory, value, code, "source_status_mismatch", BlockReason.UNCLASSIFIED.value)

        root = self.root / "precedence-malformed"
        matrix = self.fixture.make_matrix(root)
        directory = root / self.definition.case_output
        (directory / "failure.json").write_text(json.dumps(["block_reason", "authentication"]), encoding="utf-8")
        value, code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME, tag="v1.2.3", source_revision="a" * 40)
        self.assert_retained_unresolved(
            root, directory, value, code, "source_receipt_malformed", BlockReason.UNCLASSIFIED.value)

    def test_producer_refuses_resolved_output_when_the_codec_reports_an_error(self):
        # Defense in depth: the observation owner already withholds resolved cleanup.
        root = self.root / "defensive"
        matrix = self.fixture.make_matrix(root)
        directory = root / self.definition.case_output
        bad = json.loads((directory / "failure.json").read_text(encoding="utf-8"))
        bad["block_reason"] = "authentication"
        digest = contract._digest_bytes(b"observed")
        with patch.object(aggregate_operations, "_read_case_source", return_value=(bad, digest, None)):
            with self.assertRaisesRegex(EvidenceError, "source_block_reason_invalid"):
                evidence.build_aggregate(
                    root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
                    tag="v1.2.3", source_revision="a" * 40)
        self.assertTrue((directory / "failure.json").is_file())
        compact_root = root / evidence.COMPACT_RECEIPT_ROOT_NAME
        self.assertEqual(list(compact_root.glob(f"{self.definition.case_id}*")), [])

    def resolved_case(self, label):
        root, _directory, value, code = self.build(label)
        self.assertEqual(code, 0)
        return root, dict(value["cases"][0])

    def rewrite_compact(self, case, change):
        path = Path(case["receipt_path"])
        compact = json.loads(path.read_text(encoding="utf-8"))
        change(compact)
        path.write_text(json.dumps(compact), encoding="utf-8")
        return {**case, "receipt_sha256": contract._digest_bytes(path.read_bytes())}

    def test_validator_rejects_malformed_compact_block_metadata(self):
        root, case = self.resolved_case("validator-compact")
        evidence_validation._validate_case_receipt(root, case)
        expectations = {
            "missing": (lambda compact: compact.pop("block_reason"), "aggregate_case_block_reason_missing"),
            "null": (lambda compact: compact.update(block_reason=None), "aggregate_case_block_reason_invalid"),
            "noncanonical": (lambda compact: compact.update(block_reason="AUTH-UNAVAILABLE"),
                             "aggregate_case_block_reason_invalid"),
            "legacy-with-field": (lambda compact: compact.update(schema_version=SCHEMA_VERSION),
                                  "aggregate_case_block_reason_unexpected"),
            "unknown-version": (lambda compact: compact.update(schema_version=3), "aggregate_case_schema_invalid"),
        }
        original = Path(case["receipt_path"]).read_bytes()
        for label, (change, expected) in expectations.items():
            with self.subTest(label=label):
                Path(case["receipt_path"]).write_bytes(original)
                tampered = self.rewrite_compact(case, change)
                with self.assertRaisesRegex(EvidenceError, expected):
                    evidence_validation._validate_case_receipt(root, tampered)

    def test_validator_binds_a_valid_but_swapped_reason_through_the_cleanup_authorization(self):
        root, case = self.resolved_case("validator-swap")
        swapped = self.rewrite_compact(
            case, lambda compact: compact.update(block_reason=BlockReason.AUTH_UNAVAILABLE.value))
        with self.assertRaisesRegex(EvidenceError, "aggregate_case_cleanup_journal_invalid"):
            evidence_validation._validate_case_receipt(root, swapped)

    def test_non_blocked_compact_with_a_reason_is_rejected_before_proof_validation(self):
        root, _directory, value, code = self.build("validator-pass", status="PASS")
        self.assertEqual(code, 0)
        case = dict(value["cases"][0])
        evidence_validation._validate_case_receipt(root, case)
        tampered = self.rewrite_compact(
            case, lambda compact: compact.update(block_reason=BlockReason.AUTH_UNAVAILABLE.value))
        with self.assertRaisesRegex(EvidenceError, "aggregate_case_block_reason_unexpected"):
            evidence_validation._validate_case_receipt(root, tampered)

    @staticmethod
    def legacy_producer(keep_field):
        real = case_operations._sanitize_case

        def sanitize(**keywords):
            compact = real(**keywords)
            compact["schema_version"] = contract.COMPACT_CASE_LEGACY_SCHEMA_VERSION
            if not keep_field:
                compact.pop("block_reason")
            return compact

        return patch.object(aggregate_operations, "_sanitize_case", side_effect=sanitize)

    def test_a_canonical_legacy_compact_case_remains_valid_and_unclassified(self):
        root = self.root / "legacy-compact"
        matrix = self.fixture.make_matrix(root)
        with self.legacy_producer(keep_field=False):
            value, code = evidence.build_aggregate(
                root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
                tag="v1.2.3", source_revision="a" * 40)
        self.assertEqual(code, 0)
        for case in value["cases"]:
            compact = self.compact_of(case)
            self.assertEqual((compact["schema_version"], "block_reason" in compact), (SCHEMA_VERSION, False))
            self.assertIs(compact_block_reason(compact), BlockReason.UNCLASSIFIED)
        evidence.validate_aggregate_receipt(root / evidence.AGGREGATE_RECEIPT_NAME)

    def test_a_legacy_compact_case_with_the_reason_field_is_never_accepted(self):
        root = self.root / "legacy-with-field"
        matrix = self.fixture.make_matrix(root)
        with self.legacy_producer(keep_field=True):
            with self.assertRaisesRegex(EvidenceError, "aggregate_case_block_reason_unexpected"):
                evidence.build_aggregate(
                    root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
                    tag="v1.2.3", source_revision="a" * 40)


if __name__ == "__main__":
    unittest.main()
