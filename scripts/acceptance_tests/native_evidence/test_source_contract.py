"""Source-proof policy stays independent of filesystem cleanup."""

import unittest

from scripts.delegate_acceptance.native_evidence import contract


class SourceContractTests(unittest.TestCase):
    def setUp(self):
        self.row = contract.MatrixRow(contract.EXPECTED_CASES[0], 0, "PASS")
        definition = self.row.definition
        self.proof = {
            "gate": "acceptance-native", "provider": definition.provider,
            "permission": definition.permission, "mode": definition.permission,
            "scenario": definition.scenario, "requested_options_state": "recorded",
            "state": "complete",
        }

    def test_complete_source_proof_has_no_error(self):
        self.assertIsNone(contract._source_proof_error(self.row, {}, self.proof, None))

    def test_invalid_source_proof_is_rejected_with_the_original_error(self):
        mutations = (
            ("gate", "wrong", "source_proof_identity_mismatch"),
            ("provider", "wrong", "source_proof_identity_mismatch"),
            ("permission", "wrong", "source_proof_identity_mismatch"),
            ("mode", "wrong", "source_proof_identity_mismatch"),
            ("scenario", "wrong", "source_proof_identity_mismatch"),
            ("requested_options_state", "missing", "source_requested_options_missing"),
            ("requested_options_state", "invalid", "source_requested_options_invalid"),
            ("state", "unresolved", "source_proof_incomplete"),
        )
        for field, value, expected in mutations:
            with self.subTest(field=field, value=value):
                proof = {**self.proof, field: value}
                self.assertEqual(contract._source_proof_error(self.row, {}, proof, None), expected)

    def test_first_observed_error_wins(self):
        self.assertEqual(
            contract._source_proof_error(self.row, None, {}, "earlier-error"), "earlier-error",
        )
        self.assertEqual(
            contract._source_proof_error(self.row, None, {}, None), "source_receipt_missing",
        )


if __name__ == "__main__":
    unittest.main()
