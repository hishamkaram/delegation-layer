"""Native validation history must not hide failed attempts behind a later pass."""

from __future__ import annotations

import unittest

from scripts.delegate_acceptance.release_verification.native_history import (
    ValidationObservation, classify_native_history, proof_title, title_proof_sha,
)


def observation(run: int, proof: str, admission: str, verdict: str,
                attempt: int = 1) -> ValidationObservation:
    return ValidationObservation(run, attempt, proof * 64, admission, verdict)


class NativeValidationHistoryTests(unittest.TestCase):
    def decide(self, *items: ValidationObservation, complete: bool = True):
        return classify_native_history(tuple(items), complete=complete, selected_run_id=2)

    def test_passing_run_requires_complete_history(self):
        item = observation(2, "a", "admitted", "pass")
        self.assertTrue(self.decide(item).allowed)
        self.assertEqual(self.decide(item, complete=False).reason, "native_history_incomplete")

    def test_admitted_failure_survives_passing_control_and_rerun(self):
        prior = observation(2, "a", "admitted", "fail")
        later = observation(2, "b", "admitted", "pass", attempt=2)
        result = self.decide(prior, later)
        self.assertFalse(result.allowed)
        self.assertEqual(result.blocking_attempts, ((2, 1),))

    def test_admitted_unresolved_or_rejected_remains_blocking(self):
        for verdict in ("incomplete", "rejected"):
            with self.subTest(verdict=verdict):
                self.assertFalse(self.decide(
                    observation(1, "a", "admitted", verdict),
                    observation(2, "b", "admitted", "pass"),
                ).allowed)

    def test_pre_admission_rejection_is_correctable(self):
        self.assertTrue(self.decide(
            observation(1, "a", "not_admitted", "rejected"),
            observation(2, "b", "admitted", "pass"),
        ).allowed)

    def test_missing_report_requires_same_proof_resolution(self):
        missing = observation(1, "a", "unknown", "incomplete")
        self.assertFalse(self.decide(missing, observation(2, "b", "admitted", "pass")).allowed)
        self.assertTrue(self.decide(missing, observation(2, "a", "admitted", "pass")).allowed)

    def test_missing_report_cannot_erase_real_failure(self):
        result = self.decide(
            observation(1, "a", "unknown", "incomplete"),
            observation(2, "a", "admitted", "fail"),
        )
        self.assertFalse(result.allowed)

    def test_earlier_pass_does_not_resolve_later_missing_report(self):
        self.assertFalse(self.decide(
            observation(2, "a", "admitted", "pass"),
            observation(3, "a", "unknown", "incomplete"),
        ).allowed)

    def test_invalid_or_duplicate_attempt_is_not_trusted(self):
        item = observation(2, "a", "admitted", "pass")
        self.assertEqual(self.decide(item, item).reason, "native_history_invalid")
        self.assertFalse(self.decide(observation(2, "a", "not_admitted", "pass")).allowed)

    def test_title_binds_exact_tag_invocation_and_proof(self):
        title = proof_title("v1.2.3", "a" * 32, "b" * 64)
        self.assertEqual(title_proof_sha(title, "v1.2.3"), "b" * 64)
        self.assertIsNone(title_proof_sha(title, "v1.2.30"))
        with self.assertRaises(ValueError):
            proof_title("v1.2.3", "wrong", "b" * 64)


if __name__ == "__main__":
    unittest.main()
