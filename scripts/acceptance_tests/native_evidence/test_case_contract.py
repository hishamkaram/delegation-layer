"""Untrusted case descriptors must fail through the evidence contract."""

import unittest

from scripts.delegate_acceptance.native_evidence.contract import (
    EvidenceError, _validate_case_fields,
)


class CaseContractTests(unittest.TestCase):
    def case(self):
        return {
            "case_id": "case-01", "provider": "fixture:test", "permission": "read-only",
            "scenario": "read-only", "status": "BLOCKED", "exit_code": 2,
            "evidence_status": "UNRESOLVED", "receipt_path": "/owned/receipt.json",
            "receipt_sha256": "a" * 64, "binary_identities": {},
        }

    def test_evidence_status_rejects_json_containers(self):
        for value in ([], {}):
            with self.subTest(value=value):
                case = {**self.case(), "evidence_status": value}
                with self.assertRaisesRegex(EvidenceError, "aggregate_case_evidence_status_invalid"):
                    _validate_case_fields(case)

    def test_missing_fields_with_unrelated_key_has_actionable_error(self):
        with self.assertRaisesRegex(EvidenceError, "aggregate_case_fields_missing"):
            _validate_case_fields({"unexpected": True})

    def test_complete_case_fields_allow_additional_metadata(self):
        case = {**self.case(), "extra_metadata": True}
        _validate_case_fields(case)
