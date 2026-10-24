"""The candidate proof envelope binds trusted claim facts and refuses everything else."""

from __future__ import annotations

import ast
from dataclasses import replace
from pathlib import Path
import unittest

from scripts.acceptance_tests.native_evidence import candidate_proof_fixture as fixture
from scripts.delegate_acceptance.native_evidence.candidate_proof import validate_candidate_proof
from scripts.delegate_acceptance.native_evidence.public_contract import (
    MAX_PUBLIC_ZIP_BYTES, NativeCompletion, PublicProofError, canonical_json, read_public_zip, sha256,
)
from scripts.delegate_acceptance.native_evidence.public_validation import validate_public_proof
from scripts.delegate_acceptance.release_verification.contract import BinaryDigests


NATIVE_EVIDENCE = Path(__file__).resolve().parents[2] / "delegate_acceptance" / "native_evidence"


class CandidateProofCase(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.passing = fixture.release_proof(failed=False)
        cls.failing = fixture.release_proof(failed=True)
        cls.facts = fixture.expectation(cls.passing)
        cls.zip = fixture.candidate_zip(cls.passing, cls.facts)

    def assertRefused(self, code: str, data: bytes, facts=None) -> None:
        with self.assertRaises(PublicProofError) as caught:
            validate_candidate_proof(data, facts or self.facts)
        self.assertEqual(caught.exception.args, (code,))

    def first_pass_case(self) -> str:
        return next(name for name in self.passing.members() if name.startswith("cases/")
                    and self.passing.record(name)["status"] == "PASS")


class EnvelopeTests(CandidateProofCase):
    def test_a_valid_candidate_proof_yields_its_digest_bound_facts(self):
        result = validate_candidate_proof(self.zip, self.facts)
        release = validate_public_proof(self.passing.data, self.passing.expectation)
        self.assertEqual(result.public_zip_sha256, sha256(self.zip))
        self.assertEqual(result.aggregate_sha256,
                         self.passing.record("manifest.json")["source_sha256"]["aggregate_receipt"])
        self.assertEqual(result.completion, fixture.completion(self.passing, self.facts))
        self.assertEqual([(case.case_id, case.status) for case in result.cases],
                         [(case["case_id"], case["status"]) for case in release["cases"]])
        self.assertEqual({case.block_reason for case in result.cases}, {None})
        self.assertEqual((result.status, result.case_results_verdict, result.unresolved_case_ids),
                         ("PASS", "PASS", ()))
        self.assertEqual((result.candidate_id, result.claim_sha256, result.invocation_id),
                         (self.facts.candidate_id, self.facts.claim_sha256, self.facts.invocation_id))

    def test_release_and_candidate_envelopes_reject_each_other(self):
        self.assertRefused("candidate_manifest_kind_invalid", self.passing.data)
        with self.assertRaises(PublicProofError) as caught:
            validate_public_proof(self.zip, self.passing.expectation)
        self.assertEqual(caught.exception.args, ("public_manifest_version_invalid",))

    def test_every_foreign_binding_is_refused(self):
        foreign_binaries = replace(self.facts.binaries, pueued="9" * 64)
        # The fixture records the host platform, so the foreign one must differ from it.
        foreign_os = next(name for name in ("Linux", "Darwin") if name != self.facts.platform_os)
        foreign_arch = next(name for name in ("x86_64", "arm64") if name != self.facts.platform_arch)
        for field, value in (
                ("repository", "other-owner/delegation-layer"), ("candidate_id", "d" * 64),
                ("run_id", 902), ("run_attempt", 2), ("artifact_id", 78),
                ("artifact_archive_sha256", "0" * 64), ("source_commit", "1" * 40),
                ("source_tree", "2" * 40), ("platform_os", foreign_os), ("platform_arch", foreign_arch),
                ("archive_name", "other.tar.gz"), ("archive_sha256", "3" * 64), ("claim_sha256", "4" * 64),
                ("invocation_id", "invocation-2-fixture"), ("harness_revision", "5" * 40),
                ("harness_sha256", "6" * 64), ("binaries", foreign_binaries),
                ("opencode_model", "other-model"), ("ccr_model", None), ("agy_model", None)):
            with self.subTest(field=field):
                self.assertNotEqual(value, getattr(self.facts, field))
                self.assertRefused("candidate_identity_mismatch", self.zip, replace(self.facts, **{field: value}))

    def test_identity_types_are_exact(self):
        def boolean_attempt(proof):
            proof["identity"]["actions"]["run_attempt"] = True
        self.assertRefused("candidate_identity_mismatch",
                           fixture.candidate_zip(self.passing, self.facts, proof_change=boolean_attempt))

    def test_tampered_shape_valid_oversized_and_unknown_inputs_are_refused(self):
        flipped = bytearray(self.zip)
        flipped[len(flipped) // 2] ^= 0x01
        with self.assertRaises(PublicProofError):
            validate_candidate_proof(bytes(flipped), self.facts)
        self.assertRefused("public_zip_size_or_header_invalid", canonical_json({"proof_sha256": "1" * 64}))
        self.assertRefused("public_zip_size_or_header_invalid", b"PK\x03\x04" + b"0" * MAX_PUBLIC_ZIP_BYTES)

        def extra(proof):
            proof["release_identity"] = {}
        self.assertRefused("candidate_proof_invalid",
                           fixture.candidate_zip(self.passing, self.facts, proof_change=extra))
        entries = read_public_zip(self.zip)
        entries["proof.json"] = entries["proof.json"].replace(b'{"aggregate"', b'{"aggregate":{},"aggregate"', 1)
        self.assertRefused("public_json_duplicate_key", fixture.repack(self.passing, entries))

    def test_aggregate_counts_must_equal_the_validated_cases(self):
        def miscount(proof):
            proof["aggregate"]["pass_count"] -= 1
            proof["aggregate"]["failed_count"] += 1
        self.assertRefused("public_aggregate_case_counts_mismatch",
                           fixture.candidate_zip(self.passing, self.facts, proof_change=miscount))

    def test_private_case_fields_are_refused(self):
        def leak(case):
            case["argv"] = ["provider"]
        self.assertRefused("public_privacy_field_denied", fixture.candidate_zip(
            self.passing, self.facts, case_changes={self.first_pass_case(): leak}))


class CompletionTests(CandidateProofCase):
    def proof_with(self, change) -> bytes:
        return fixture.candidate_zip(self.passing, self.facts, proof_change=change)

    def test_completion_is_a_positive_natural_wait_of_the_claimed_invocation(self):
        for change, code in (
                (lambda proof: proof.pop("completion"), "candidate_proof_invalid"),
                (lambda proof: proof["completion"].update(natural_wait=False), "native_completion_invalid"),
                (lambda proof: proof["completion"].update(natural_wait=1), "native_completion_invalid"),
                (lambda proof: proof["completion"].update(exit_code=256), "native_completion_invalid"),
                (lambda proof: proof["completion"].update(completed_sha256="fake"), "native_completion_invalid"),
                (lambda proof: proof["completion"].update(pid=7), "candidate_completion_invalid")):
            with self.subTest(code=code):
                self.assertRefused(code, self.proof_with(change))
        other = replace(fixture.completion(self.passing, self.facts), invocation_id="invocation-9-fixture")
        self.assertRefused("candidate_completion_invocation_mismatch",
                           fixture.candidate_zip(self.passing, self.facts, native=other))
        exit_mismatch = replace(fixture.completion(self.passing, self.facts), exit_code=1)
        self.assertRefused("candidate_completion_exit_mismatch",
                           fixture.candidate_zip(self.passing, self.facts, native=exit_mismatch))
        with self.assertRaises(PublicProofError):
            NativeCompletion("invocation-1-fixture", False, 0, "1" * 64, "2" * 64, "3" * 64)


class CaseClosureTests(CandidateProofCase):
    def test_an_unobserved_supervisor_shutdown_stays_unresolved(self):
        name = self.first_pass_case()

        def unobserved(case):
            case["source_proof"]["lifecycle_proof"]["supervisor_shutdown_observed"] = False
        data = fixture.candidate_zip(self.passing, self.facts, case_changes={name: unobserved})
        self.assertEqual(validate_public_proof(fixture.release_with(self.passing, {name: unobserved}),
                                               self.passing.expectation)["status"], "PASS")
        result = validate_candidate_proof(data, self.facts)
        case_id = name.removeprefix("cases/").removesuffix(".json")
        self.assertEqual(result.unresolved_case_ids, (case_id,))
        self.assertEqual(dict((case.case_id, case.status) for case in result.cases)[case_id], "PASS")

    def test_a_pass_without_a_complete_producer_closure_is_refused(self):
        def unproven(case):
            case["source_proof"]["lifecycle_proof"]["state"] = "unresolved"
        self.assertRefused("candidate_case_predicate_invalid", fixture.candidate_zip(
            self.passing, self.facts, case_changes={self.first_pass_case(): unproven}))

    def test_failed_cases_are_reported_unresolved(self):
        facts = fixture.expectation(self.failing)
        result = validate_candidate_proof(fixture.candidate_zip(self.failing, facts), facts)
        failed = tuple(case.case_id for case in result.cases if case.status == "FAIL")
        self.assertEqual((result.case_results_verdict, result.unresolved_case_ids), ("FAIL", failed))
        self.assertTrue(failed)


class ModelTests(CandidateProofCase):
    def test_a_wrong_model_pass_agy_case_is_refused_inside_a_fail_aggregate(self):
        agy = self.failing.record(fixture.agy_case_name(self.failing))
        self.assertEqual((agy["status"], self.failing.record("proof.json")["aggregate"]["case_results_verdict"]),
                         ("PASS", "FAIL"))
        recorded = fixture.expectation(self.failing)
        self.assertEqual(validate_candidate_proof(fixture.candidate_zip(self.failing, recorded), recorded)
                         .case_results_verdict, "FAIL")
        for requested in ("other-model", None):
            with self.subTest(requested=requested):
                facts = replace(recorded, agy_model=requested)
                self.assertRefused("candidate_agy_model_mismatch",
                                   fixture.candidate_zip(self.failing, facts), facts)

    def test_case_models_and_binaries_follow_the_claimed_expectation(self):
        facts = replace(self.facts, opencode_model="claimed-model")
        self.assertRefused("public_opencode_model_mismatch", fixture.candidate_zip(self.passing, facts), facts)
        binaries = BinaryDigests(delegate="9" * 64, delegate_run=self.facts.binaries.delegate_run,
                                 pueue=self.facts.binaries.pueue, pueued=self.facts.binaries.pueued)
        facts = replace(self.facts, binaries=binaries)
        self.assertRefused("public_case_binary_binding_mismatch", fixture.candidate_zip(self.passing, facts), facts)


class BoundaryTests(unittest.TestCase):
    def test_native_evidence_never_imports_candidate_business_modules(self):
        for path in sorted(NATIVE_EVIDENCE.glob("*.py")):
            tree = ast.parse(path.read_text(encoding="utf-8"))
            modules = [node.module or "" for node in ast.walk(tree) if isinstance(node, ast.ImportFrom)]
            modules += [alias.name for node in ast.walk(tree) if isinstance(node, ast.Import)
                        for alias in node.names]
            with self.subTest(path=path.name):
                self.assertFalse([name for name in modules if "release_verification.candidate" in name])


if __name__ == "__main__":
    unittest.main()
