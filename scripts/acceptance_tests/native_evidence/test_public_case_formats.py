"""The release and candidate public case formats, from the shared projector to the shared predicates.

Projection tests run the shared projector over the live stage snapshot of the
canonical release fixture. A changed compact receipt or public member is a
test mutation, never producer output.
"""

from __future__ import annotations

from dataclasses import replace
from enum import Enum
import json
import unittest

from scripts.acceptance_tests.native_evidence import candidate_proof_fixture as fixture
from scripts.delegate_acceptance.native_evidence.candidate_proof import validate_candidate_proof
from scripts.delegate_acceptance.native_evidence.contract import BlockReason, EvidenceError
from scripts.delegate_acceptance.native_evidence.public_case_validation import CaseContext
from scripts.delegate_acceptance.native_evidence.public_contract import (
    CANDIDATE_CASE_SCHEMA_VERSION, CASE_DEFINITIONS, CASE_IDS, PUBLIC_SCHEMA_VERSION, CaseOutcome,
    PublicProofError,
)
from scripts.delegate_acceptance.native_evidence.public_projection import _project_cases, _public_zip
from scripts.delegate_acceptance.native_evidence.public_validation import validate_public_proof


class LookalikeReason(Enum):
    """An unrelated enum whose members share the canonical reason values."""

    AUTH_UNAVAILABLE = "auth-unavailable"
    UNCLASSIFIED = "unclassified"


# The failing release fixture fails its second case, whose evidence stays unresolved.
FAILED_INDEX = 1
CCR_CASES = tuple(f"cases/{case.case_id}.json" for case in CASE_DEFINITIONS if case.provider == "ccr:print")


class ProjectionFormatTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.passing = fixture.release_proof(failed=False)
        cls.failing = fixture.release_proof(failed=True)

    def project(self, change, *, case_schema_version=CANDIDATE_CASE_SCHEMA_VERSION):
        return fixture.project_compact(self.failing, FAILED_INDEX, case_schema_version=case_schema_version,
                                       change=change)

    def test_the_release_format_keeps_its_exact_bytes(self):
        for release in (self.passing, self.failing):
            with self.subTest(failed=release is self.failing):
                self.assertEqual(_public_zip(release.snapshot, release.expectation), release.data)
                legacy = _project_cases(release.snapshot, release.aggregate_cases(),
                                        case_schema_version=PUBLIC_SCHEMA_VERSION)
                members = release.members()
                self.assertEqual(legacy, {name: members[name] for name in legacy})
                for data in legacy.values():
                    case = json.loads(data)
                    self.assertEqual(case["schema_version"], PUBLIC_SCHEMA_VERSION)
                    self.assertNotIn("block_reason", case)

    def test_the_candidate_format_adds_only_its_version_and_reason(self):
        statuses = set()
        for release in (self.passing, self.failing):
            members = release.members()
            for name, data in release.candidate_cases:
                candidate, legacy = json.loads(data), json.loads(members[name])
                statuses.add(candidate["status"])
                self.assertIsNone(candidate.pop("block_reason"))
                self.assertEqual((candidate.pop("schema_version"), legacy.pop("schema_version")),
                                 (CANDIDATE_CASE_SCHEMA_VERSION, PUBLIC_SCHEMA_VERSION))
                self.assertEqual(candidate, legacy)
        self.assertEqual(statuses, {"PASS", "FAIL"})

    def test_a_blocked_compact_projects_its_exact_typed_reason(self):
        for reason in BlockReason:
            with self.subTest(reason=reason):
                change = fixture.blocked_before_runtime(CANDIDATE_CASE_SCHEMA_VERSION, reason.value)
                candidate = self.project(change)
                legacy = self.project(change, case_schema_version=PUBLIC_SCHEMA_VERSION)
                self.assertEqual((candidate["status"], candidate.pop("block_reason")), ("BLOCKED", reason.value))
                self.assertNotIn("block_reason", legacy)
                self.assertEqual({**candidate, "schema_version": PUBLIC_SCHEMA_VERSION}, legacy)
        legacy_compact = self.project(fixture.blocked_before_runtime(1, None))
        self.assertEqual(legacy_compact["block_reason"], BlockReason.UNCLASSIFIED.value)

    def test_an_invalid_compact_reason_is_refused_in_both_formats(self):
        changes = (
            (fixture.blocked_before_runtime(CANDIDATE_CASE_SCHEMA_VERSION, None),
             "aggregate_case_block_reason_invalid"),
            (fixture.blocked_before_runtime(CANDIDATE_CASE_SCHEMA_VERSION, "neutral"),
             "aggregate_case_block_reason_invalid"),
            (lambda compact: compact.update(block_reason=BlockReason.AUTH_UNAVAILABLE.value),
             "aggregate_case_block_reason_unexpected"),
            (lambda compact: compact.pop("block_reason"), "aggregate_case_block_reason_missing"),
            (lambda compact: compact.update(schema_version=3), "aggregate_case_schema_invalid"),
        )
        for change, code in changes:
            for version in (PUBLIC_SCHEMA_VERSION, CANDIDATE_CASE_SCHEMA_VERSION):
                with self.subTest(code=code, version=version):
                    with self.assertRaises(PublicProofError) as caught:
                        self.project(change, case_schema_version=version)
                    self.assertEqual(caught.exception.args, ("public_case_source_invalid",))
                    self.assertIsInstance(caught.exception.__cause__, EvidenceError)
                    self.assertEqual(caught.exception.__cause__.args, (code,))

    def test_an_unknown_case_format_is_refused(self):
        binaries = fixture.expectation(self.passing).binaries
        for version in (0, 3, True, "2", None):
            with self.subTest(version=version):
                with self.assertRaises(PublicProofError) as projected:
                    fixture.project_compact(self.passing, 0, case_schema_version=version)
                self.assertEqual(projected.exception.args, ("public_case_format_invalid",))
                with self.assertRaises(PublicProofError) as context:
                    CaseContext(opencode_model="model", ccr_model=None, binaries=binaries, case_receipts=(),
                                cleanup_journals=(), case_schema_version=version)
                self.assertEqual(context.exception.args, ("public_case_format_invalid",))


class CandidateCaseFormatTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.passing = fixture.release_proof(failed=False)
        cls.facts = fixture.expectation(cls.passing)
        cls.first_pass = next(f"cases/{case_id}.json" for case_id in CASE_IDS
                              if f"cases/{case_id}.json" not in CCR_CASES)

    def blocked_ccr(self, reason: object) -> tuple[object, bytes]:
        """Every CCR case blocked before runtime with ``reason``, as the CCR-absence release test does."""
        def blocked(case):
            case.update(status="BLOCKED", exit_code=2, block_reason=reason)
            case["source_proof"].update({
                "state": "blocked-before-runtime", "final_status": "BLOCKED", "source_status": "blocked",
                "requested_model": None, "task_ids": [], "task_session_bindings": [], "diagnostic_task_ids": [],
                "session_ids": {"state": "not-recorded", "values": [], "by_alias": {}},
                "binary_identities": {"state": "unavailable", "sha256": {}, "task_ids": []},
            })
            case["source_proof"]["lifecycle_proof"]["state"] = "unresolved"

        def counts(proof):
            proof["aggregate"]["pass_count"] -= len(CCR_CASES)
            proof["aggregate"]["blocked_count"] = len(CCR_CASES)
        facts = replace(self.facts, ccr_model=None)
        return facts, fixture.candidate_zip(self.passing, facts, proof_change=counts,
                                            case_changes=dict.fromkeys(CCR_CASES, blocked))

    def assertRefused(self, code, data, facts=None):
        with self.assertRaises(PublicProofError) as caught:
            validate_candidate_proof(data, facts or self.facts)
        self.assertEqual(caught.exception.args, (code,))

    def test_typed_reasons_round_trip_to_the_exact_member(self):
        self.assertTrue(CCR_CASES)
        for reason in BlockReason:
            with self.subTest(reason=reason):
                facts, data = self.blocked_ccr(reason.value)
                result = validate_candidate_proof(data, facts)
                self.assertEqual(result.case_results_verdict, "PASS")
                self.assertEqual({case.case_id: case.block_reason for case in result.cases
                                  if case.status == "BLOCKED"},
                                 {name.removeprefix("cases/").removesuffix(".json"): reason for name in CCR_CASES})
                self.assertEqual({case.block_reason for case in result.cases if case.status != "BLOCKED"}, {None})

    def test_an_invalid_reason_never_becomes_neutral(self):
        for value in (None, "neutral", "unknown", "AUTH_UNAVAILABLE", [], {}, 1, True):
            with self.subTest(value=value):
                facts, data = self.blocked_ccr(value)
                self.assertRefused("public_case_block_reason_invalid", data, facts)
        for value in (BlockReason.AUTH_UNAVAILABLE.value, BlockReason.UNCLASSIFIED.value):
            with self.subTest(pass_reason=value):
                self.assertRefused("public_case_block_reason_invalid", fixture.candidate_zip(
                    self.passing, self.facts,
                    case_changes={self.first_pass: lambda case, value=value: case.update(block_reason=value)}))

    def test_each_envelope_refuses_the_other_case_format(self):
        def release_format(case):
            case.pop("block_reason")
            case["schema_version"] = PUBLIC_SCHEMA_VERSION
        for change, code in ((release_format, "public_case_invalid"),
                             (lambda case: case.pop("block_reason"), "public_case_invalid"),
                             (lambda case: case.update(schema_version=PUBLIC_SCHEMA_VERSION),
                              "public_case_version_invalid")):
            with self.subTest(code=code):
                self.assertRefused(code, fixture.candidate_zip(self.passing, self.facts,
                                                               case_changes={self.first_pass: change}))
        with self.assertRaises(PublicProofError) as extra:
            validate_public_proof(fixture.release_with(
                self.passing, {self.first_pass: lambda case: case.update(block_reason=None)}),
                self.passing.expectation)
        self.assertEqual(extra.exception.args, ("public_privacy_field_denied",))
        upgraded = validate_public_proof(fixture.release_with(
            self.passing, {self.first_pass: lambda case: case.update(schema_version=CANDIDATE_CASE_SCHEMA_VERSION)}),
            self.passing.expectation)
        self.assertEqual((upgraded["status"], upgraded["case_results_verdict"], upgraded["blocking_reason"]),
                         ("BLOCKED", "FAIL", "case_schema_or_predicate_invalid"))


class CaseOutcomeTests(unittest.TestCase):
    def test_a_reason_is_typed_exactly_for_a_blocked_case(self):
        for reason in BlockReason:
            self.assertIs(CaseOutcome(CASE_IDS[0], "BLOCKED", reason).block_reason, reason)
        for status in ("PASS", "FAIL"):
            self.assertIsNone(CaseOutcome(CASE_IDS[0], status, None).block_reason)
        for status, reason in (("BLOCKED", None), ("BLOCKED", BlockReason.AUTH_UNAVAILABLE.value),
                               ("PASS", BlockReason.AUTH_UNAVAILABLE), ("FAIL", BlockReason.UNCLASSIFIED),
                               (["PASS"], None), ("blocked", BlockReason.UNCLASSIFIED)):
            with self.subTest(status=status, reason=reason):
                with self.assertRaises(PublicProofError) as caught:
                    CaseOutcome(CASE_IDS[0], status, reason)
                self.assertEqual(caught.exception.args, ("candidate_proof_result_invalid",))

    def test_a_reason_outside_the_canonical_members_is_refused(self):
        cases = [(status, value) for status in ("PASS", "FAIL")
                 for value in (LookalikeReason.AUTH_UNAVAILABLE, "auth-unavailable", "", 1, 0, False, object())]
        cases += [("BLOCKED", member) for member in LookalikeReason]
        for status, reason in cases:
            with self.subTest(status=status, reason=reason):
                with self.assertRaises(Exception) as caught:
                    CaseOutcome(CASE_IDS[0], status, reason)
                self.assertIs(type(caught.exception), PublicProofError)
                self.assertEqual(caught.exception.args, ("candidate_proof_result_invalid",))


if __name__ == "__main__":
    unittest.main()
