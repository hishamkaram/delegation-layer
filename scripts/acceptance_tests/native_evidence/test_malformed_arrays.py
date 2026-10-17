"""Untrusted arrays fail with evidence errors before set or sorting operations."""

import unittest

from scripts.delegate_acceptance.native_evidence import contract


class MalformedArrayTests(unittest.TestCase):
    def test_lifecycle_state_rejects_json_containers(self):
        for state in ([], {}):
            with self.subTest(state=state):
                with self.assertRaisesRegex(contract.EvidenceError, "aggregate_case_lifecycle_invalid"):
                    contract._validate_case_completion(
                        {}, {"lifecycle_proof": {"state": state}}, ([], [], [], {}), ({}, []),
                    )

    def test_aggregate_status_rejects_json_containers(self):
        for status in ([], {}):
            with self.subTest(status=status):
                receipt = {
                    "case_count": 0, "pass_count": 0, "blocked_count": 0, "failed_count": 0,
                    "unresolved_cases": [], "unresolved_case_count": 0,
                    "case_results_verdict": "BLOCKED", "status": status,
                }
                with self.assertRaisesRegex(contract.EvidenceError, "aggregate_status_invalid"):
                    contract._validate_aggregate_result(receipt, [])

    def test_session_state_rejects_json_containers(self):
        for state in ([], {}):
            with self.subTest(state=state):
                proof = {
                    "task_session_bindings": [], "task_ids": [], "diagnostic_task_ids": [],
                    "session_ids": {"state": state, "values": [], "by_alias": {}},
                }
                with self.assertRaisesRegex(
                        contract.EvidenceError, "aggregate_case_session_ids_invalid"):
                    contract._validate_case_sessions(proof)

    def test_binary_state_rejects_json_containers(self):
        for state in ([], {}):
            with self.subTest(state=state):
                with self.assertRaisesRegex(
                        contract.EvidenceError, "aggregate_case_binary_identity_invalid"):
                    contract._validate_case_binaries({}, {"binary_identities": {"state": state}})

    def test_diagnostic_task_ids_reject_unhashable_and_mixed_values(self):
        for values in ([{}], ["task", 1]):
            with self.subTest(values=values):
                proof = {
                    "task_session_bindings": [], "task_ids": [],
                    "diagnostic_task_ids": values,
                    "session_ids": {"state": "not-recorded", "values": [], "by_alias": {}},
                }
                with self.assertRaisesRegex(
                        contract.EvidenceError, "aggregate_case_diagnostic_task_ids_invalid"):
                    contract._validate_case_sessions(proof)

    def test_binary_task_ids_reject_unhashable_and_mixed_values(self):
        for values in ([{}], ["a" * 32, 1]):
            with self.subTest(values=values):
                binaries = {
                    "state": "unavailable", "sha256": {}, "task_ids": values,
                    "provider_runtime_identities": {}, "provider_binding": None,
                }
                with self.assertRaisesRegex(
                        contract.EvidenceError, "aggregate_case_binary_task_ids_invalid"):
                    contract._validate_case_binaries(
                        {"binary_identities": binaries}, {"binary_identities": binaries},
                    )

    def test_unresolved_cases_reject_unhashable_and_non_text_values(self):
        for values in ([{}], [1]):
            with self.subTest(values=values):
                receipt = {
                    "case_count": 0, "pass_count": 0, "blocked_count": 0, "failed_count": 0,
                    "unresolved_cases": values, "unresolved_case_count": len(values),
                }
                with self.assertRaisesRegex(
                        contract.EvidenceError, "aggregate_unresolved_count_mismatch"):
                    contract._validate_aggregate_result(receipt, [])


if __name__ == "__main__":
    unittest.main()
