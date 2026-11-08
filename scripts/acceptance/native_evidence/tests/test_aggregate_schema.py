"""The aggregate header schema version is the integer 1, not an equal JSON value."""

import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import scripts.acceptance.native_evidence.aggregate as native_evidence_aggregate
import scripts.acceptance.native_evidence.contract as native_evidence_contract
import time as time
# Import the module, not the TestCase, so its tests are discovered only once.
from scripts.acceptance.native_evidence.tests import test_evidence as evidence_tests
from scripts.acceptance.native_evidence import cli as cli
from scripts.acceptance.native_evidence import validation as validation
from scripts.acceptance.native_evidence.contract import EvidenceError


# Each equals the integer 1 in Python, so an equality-only guard would accept it.
EQUAL_TO_ONE = (("bool", True), ("float", 1.0))
NOT_ONE = (("zero", 0), ("two", 2), ("string", "1"), ("null", None), ("list", [1]))


class AggregateSchemaTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-aggregate-schema-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.fixture = object.__new__(evidence_tests.NativeAcceptanceEvidenceTests)
        self.fixture.root = self.root

    def build(self, label, **keywords):
        value, code = self.fixture.build(root=self.root / label, status="PASS", **keywords)
        self.assertEqual(code, 0)
        return self.root / label / native_evidence_contract.AGGREGATE_RECEIPT_NAME, value

    @staticmethod
    def set_schema(path, schema):
        value = json.loads(path.read_text(encoding="utf-8"))
        value["schema_version"] = schema
        path.write_text(json.dumps(value), encoding="utf-8")

    def malformed(self, label, schema):
        path, _ = self.build(label)
        self.set_schema(path, schema)
        return path


class ValidatorTests(AggregateSchemaTests):
    def test_integer_one_is_accepted_and_only_the_schema_field_differs(self):
        path, built = self.build("positive")
        value, digest = validation.validate_aggregate_receipt(path)
        self.assertEqual((value["schema_version"], type(value["schema_version"])), (1, int))
        self.assertEqual(value["status"], built["status"])
        self.assertEqual(len(digest), 64)

    def test_values_equal_to_one_are_refused_with_the_existing_error(self):
        for label, schema in EQUAL_TO_ONE:
            with self.subTest(schema=label):
                path = self.malformed("equal-" + label, schema)
                self.assertEqual(json.loads(path.read_text(encoding="utf-8"))["schema_version"], schema)
                with self.assertRaises(native_evidence_contract.EvidenceError) as raised:
                    validation.validate_aggregate_receipt(path)
                self.assertEqual(raised.exception.args, ("aggregate_receipt_invalid",))

    def test_other_schema_values_keep_the_same_refusal(self):
        for label, schema in NOT_ONE:
            with self.subTest(schema=label):
                path = self.malformed("other-" + label, schema)
                with self.assertRaises(native_evidence_contract.EvidenceError) as raised:
                    validation.validate_aggregate_receipt(path)
                self.assertEqual(raised.exception.args, ("aggregate_receipt_invalid",))

    def test_a_missing_schema_field_keeps_the_same_refusal(self):
        path, _ = self.build("missing")
        value = json.loads(path.read_text(encoding="utf-8"))
        del value["schema_version"]
        path.write_text(json.dumps(value), encoding="utf-8")
        with self.assertRaisesRegex(native_evidence_contract.EvidenceError, "^aggregate_receipt_invalid$"):
            validation.validate_aggregate_receipt(path)


class SharedCaseProofTests(AggregateSchemaTests):
    """Compact receipt validation and supplemental validation share one in-memory proof owner."""

    def compact_cases(self, label):
        path, _ = self.build(label)
        value = json.loads(path.read_text(encoding="utf-8"))
        root = path.parent
        return path, [(case, json.loads((root / case["receipt_path"]).read_text(encoding="utf-8")))
                      for case in value["cases"]]

    def test_every_compact_case_proof_validates_in_memory_against_its_reference(self):
        _, cases = self.compact_cases("shared")
        for case, compact in cases:
            with self.subTest(case=case["case_id"]):
                proof = validation.validate_case_proof(case, compact)
                self.assertEqual(proof["binary_identities"], case["binary_identities"])

    def test_in_memory_mismatch_is_refused_without_reading_the_receipt_path(self):
        _, cases = self.compact_cases("mismatch")
        case, compact = cases[0]
        with self.assertRaisesRegex(EvidenceError, "aggregate_case_receipt_mismatch"):
            validation.validate_case_proof({**case, "receipt_path": "absent.json"},
                                           {**compact, "exit_code": 7})

    def test_compact_receipt_validation_routes_through_the_shared_proof(self):
        path, _ = self.compact_cases("routed")
        with patch.object(validation, "validate_case_proof",
                          side_effect=EvidenceError("shared_proof_called")) as shared:
            with self.assertRaisesRegex(EvidenceError, "shared_proof_called"):
                validation.validate_aggregate_receipt(path)
        shared.assert_called_once()


class ConsumerTests(AggregateSchemaTests):
    def run_cli(self, *arguments):
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = cli.main(list(arguments))
        return code, stdout.getvalue(), stderr.getvalue()

    def test_public_validate_accepts_integer_one_and_prints_the_json_summary(self):
        path, built = self.build("cli-positive")
        code, stdout, stderr = self.run_cli("validate", "--receipt", str(path))
        self.assertEqual((code, stderr), (0, ""))
        summary = json.loads(stdout)
        self.assertEqual((summary["status"], summary["case_count"]), (built["status"], built["case_count"]))
        self.assertEqual(summary["aggregate_receipt"], str(path))

    def test_public_validate_refuses_values_equal_to_one_with_exit_one(self):
        for label, schema in EQUAL_TO_ONE:
            with self.subTest(schema=label):
                path = self.malformed("cli-" + label, schema)
                code, stdout, stderr = self.run_cli("validate", "--receipt", str(path))
                self.assertEqual(
                    (code, stdout, stderr),
                    (1, "", "native acceptance evidence failed: aggregate_receipt_invalid\n"))





    def test_terminal_replay_never_returns_a_malformed_receipt(self):
        path, _ = self.build("replay")
        root = path.parent
        matrix = root / native_evidence_contract.CASE_MATRIX_NAME
        replay = {"tag": "v1.2.3", "source_revision": "a" * 40}
        replayed, code = native_evidence_aggregate.build_aggregate(
            root, matrix, path, tag=replay["tag"], source_revision=replay["source_revision"])
        self.assertEqual((code, replayed["schema_version"]), (0, 1))
        self.set_schema(path, True)
        with self.assertRaisesRegex(native_evidence_contract.EvidenceError, "^aggregate_receipt_invalid$"):
            native_evidence_aggregate.build_aggregate(root, matrix, path, tag=replay["tag"], source_revision=replay["source_revision"])
        self.assertIs(json.loads(path.read_text(encoding="utf-8"))["schema_version"], True)


class RetentionTests(AggregateSchemaTests):
    def test_a_malformed_sibling_is_ineligible_while_a_valid_sibling_is_counted(self):
        parent = self.root / "aggregates"
        parent.mkdir()
        siblings = {}
        for timestamp, name in ((1, "native-aggregate-valid"), (2, "native-aggregate-bool"),
                                (3, "native-aggregate-float")):
            with patch.object(time, "time_ns", return_value=timestamp):
                value, code = self.fixture.build(
                    root=parent / name, status="PASS", retention_count=32)
            self.assertEqual(code, 0)
            siblings[name] = parent / name / native_evidence_contract.AGGREGATE_RECEIPT_NAME
        self.set_schema(siblings["native-aggregate-bool"], True)
        self.set_schema(siblings["native-aggregate-float"], 1.0)

        current = parent / "native-aggregate-current"
        with patch.object(time, "time_ns", return_value=4):
            value, code = self.fixture.build(root=current, status="PASS", retention_count=1)
        self.assertEqual(code, 0)
        self.assertFalse((parent / "native-aggregate-valid").exists())
        for name in ("native-aggregate-bool", "native-aggregate-float"):
            self.assertTrue((parent / name).is_dir())
            self.assertTrue(siblings[name].is_file())
        self.assertEqual(value["retention"]["before"]["root_count"], 2)
        self.assertEqual(value["retention"]["after"]["root_count"], 1)

    def test_a_valid_sibling_still_fills_a_retained_slot(self):
        parent = self.root / "aggregates-retained"
        parent.mkdir()
        with patch.object(time, "time_ns", return_value=1):
            self.fixture.build(root=parent / "native-aggregate-valid", status="PASS", retention_count=32)
        with patch.object(time, "time_ns", return_value=2):
            self.fixture.build(root=parent / "native-aggregate-bool", status="PASS", retention_count=32)
        self.set_schema(parent / "native-aggregate-bool" / native_evidence_contract.AGGREGATE_RECEIPT_NAME, True)
        with patch.object(time, "time_ns", return_value=3):
            value, code = self.fixture.build(
                root=parent / "native-aggregate-current", status="PASS", retention_count=2)
        self.assertEqual(code, 0)
        self.assertTrue((parent / "native-aggregate-valid").is_dir())
        self.assertTrue((parent / "native-aggregate-bool").is_dir())
        self.assertEqual(value["retention"]["after"]["root_count"], 2)


if __name__ == "__main__":
    unittest.main()
