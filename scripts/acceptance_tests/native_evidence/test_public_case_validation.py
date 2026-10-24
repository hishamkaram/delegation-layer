"""Release and candidate envelopes apply one shared case policy."""

from __future__ import annotations

import unittest

from scripts.acceptance_tests.native_evidence import candidate_proof_fixture as fixture
from scripts.delegate_acceptance.native_evidence.candidate_proof import validate_candidate_proof
from scripts.delegate_acceptance.native_evidence.lifetime_closure import case_lifetime_closed
from scripts.delegate_acceptance.native_evidence.public_case_validation import agy_case_model_bound
from scripts.delegate_acceptance.native_evidence.public_contract import PublicProofError
from scripts.delegate_acceptance.native_evidence.public_validation import validate_public_proof
from scripts.delegate_acceptance.native_release.contract import ReleaseError
from scripts.delegate_acceptance.native_release.projection import _require_agy_model_bound


class SharedCasePolicyTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.passing = fixture.release_proof(failed=False)
        cls.failing = fixture.release_proof(failed=True)
        cls.opencode = next(name for name in cls.passing.members() if "-opencode-" in name)

    def test_both_envelopes_reach_the_same_case_verdicts(self):
        for release in (self.passing, self.failing):
            facts = fixture.expectation(release)
            candidate = validate_candidate_proof(fixture.candidate_zip(release, facts), facts)
            hosted = validate_public_proof(release.data, release.expectation)
            with self.subTest(verdict=hosted["case_results_verdict"]):
                self.assertEqual([(case.case_id, case.status) for case in candidate.cases],
                                 [(case["case_id"], case["status"]) for case in hosted["cases"]])
                self.assertEqual(candidate.case_results_verdict, hosted["case_results_verdict"])
                self.assertEqual(len(candidate.unresolved_case_ids), hosted["unresolved_case_count"])

    def test_a_case_either_envelope_rejects_is_rejected_by_both(self):
        facts = fixture.expectation(self.passing)
        changes = {
            "public_opencode_model_mismatch":
                lambda case: case["source_proof"].update(requested_model="other-model"),
            "public_case_exit_mismatch": lambda case: case.update(exit_code=9),
            "public_cleanup_binding_mismatch": lambda case: case["cleanup"].update(journal_sha256="0" * 64),
            "public_case_binary_binding_mismatch":
                lambda case: case["source_proof"]["binary_identities"]["sha256"].update(pueue_sha256="9" * 64),
        }
        for code, change in changes.items():
            with self.subTest(code=code):
                hosted = validate_public_proof(fixture.release_with(self.passing, {self.opencode: change}),
                                               self.passing.expectation)
                self.assertEqual((hosted["status"], hosted.get("blocking_reason")),
                                 ("BLOCKED", "case_schema_or_predicate_invalid"))
                with self.assertRaises(PublicProofError) as caught:
                    validate_candidate_proof(fixture.candidate_zip(
                        self.passing, facts, case_changes={self.opencode: change}), facts)
                self.assertEqual(caught.exception.args, (code,))

    def test_a_non_text_aggregate_verdict_is_refused_by_both(self):
        facts = fixture.expectation(self.passing)
        self.assertEqual(validate_public_proof(fixture.release_with(self.passing, {}, proof_change=lambda _: None),
                                               self.passing.expectation)["status"], "PASS")
        for field in ("status", "case_results_verdict"):
            for value in ([], {}):
                def malformed(proof, field=field, value=value):
                    proof["aggregate"][field] = value
                with self.subTest(field=field, value=value):
                    with self.assertRaises(PublicProofError) as hosted:
                        validate_public_proof(fixture.release_with(self.passing, {}, proof_change=malformed),
                                              self.passing.expectation)
                    self.assertEqual(hosted.exception.args, ("public_aggregate_invalid",))
                    with self.assertRaises(PublicProofError) as candidate:
                        validate_candidate_proof(fixture.candidate_zip(
                            self.passing, facts, proof_change=malformed), facts)
                    self.assertEqual(candidate.exception.args, ("public_aggregate_invalid",))

    def test_a_non_text_case_verdict_refuses_the_candidate_and_blocks_the_release(self):
        facts = fixture.expectation(self.passing)
        for field in ("status", "evidence_status"):
            for value in ([], {}):
                def malformed(case, field=field, value=value):
                    case[field] = value
                with self.subTest(field=field, value=value):
                    hosted = validate_public_proof(fixture.release_with(self.passing, {self.opencode: malformed}),
                                                   self.passing.expectation)
                    self.assertEqual((hosted["status"], hosted["case_results_verdict"], hosted["blocking_reason"]),
                                     ("BLOCKED", "FAIL", "case_schema_or_predicate_invalid"))
                    with self.assertRaises(PublicProofError) as candidate:
                        validate_candidate_proof(fixture.candidate_zip(
                            self.passing, facts, case_changes={self.opencode: malformed}), facts)
                    self.assertEqual(candidate.exception.args, ("public_case_verdict_invalid",))

    def test_agy_binding_compares_the_recorded_requested_option(self):
        agy = self.passing.record(fixture.agy_case_name(self.passing))
        recorded = agy["source_proof"]["requested_model"]
        self.assertEqual(recorded, "fixture-model")
        self.assertTrue(agy_case_model_bound(agy, "fixture-model"))
        self.assertFalse(agy_case_model_bound(agy, None))
        self.assertFalse(agy_case_model_bound(agy, "fixture-model-alias"))
        self.assertTrue(agy_case_model_bound({**agy, "status": "BLOCKED"}, "other-model"))
        self.assertTrue(agy_case_model_bound(self.passing.record(self.opencode), "other-model"))

    def test_release_projection_keeps_its_pass_verdict_gate(self):
        passing = validate_public_proof(self.passing.data, self.passing.expectation)
        failing = validate_public_proof(self.failing.data, self.failing.expectation)
        _require_agy_model_bound(passing, "fixture-model")
        _require_agy_model_bound(failing, "other-model")
        with self.assertRaises(ReleaseError) as caught:
            _require_agy_model_bound(passing, "other-model")
        self.assertEqual(caught.exception.code, "agy_requested_model_unbound")


class LifetimeClosurePredicateTests(unittest.TestCase):
    def closed(self, **changes: object) -> bool:
        facts: dict[str, object] = {
            "status": "PASS", "cleanup_method": None, "proof_state": "complete",
            "lifecycle_state": "complete", "supervisor_shutdown_observed": True,
        }
        facts.update(changes)
        return case_lifetime_closed(**facts)

    def test_closure_requires_positive_facts(self):
        self.assertTrue(self.closed())
        self.assertFalse(self.closed(lifecycle_state="unresolved"))
        self.assertFalse(self.closed(supervisor_shutdown_observed=None))
        self.assertFalse(self.closed(supervisor_shutdown_observed=1))
        self.assertFalse(self.closed(status="FAIL", proof_state="unresolved", supervisor_shutdown_observed=None))
        before_runtime = dict(status="BLOCKED", cleanup_method="blocked-before-runtime-cleanup",
                              proof_state="blocked-before-runtime", supervisor_shutdown_observed=None,
                              lifecycle_state="unresolved")
        self.assertTrue(self.closed(**before_runtime))
        self.assertFalse(self.closed(**{**before_runtime, "proof_state": "unresolved"}))
        self.assertFalse(self.closed(**{**before_runtime, "status": "FAIL"}))


if __name__ == "__main__":
    unittest.main()
