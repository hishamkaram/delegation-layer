"""The candidate proof builder and the produced proof of one real native child.

Produced-proof tests read the proof the real producer built inside the
canonical fixture lifetime. Builder tests project a live candidate stage.
Changed members or stage bytes are test mutations, never producer output.
The candidate archive, harness and binary facts are trusted expectations;
nothing here observes them.
"""

from __future__ import annotations

from dataclasses import replace
import json
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest.mock import patch

from scripts.acceptance_tests.native_evidence import candidate_proof_fixture as fixture
from scripts.delegate_acceptance.native_evidence import public_projection
from scripts.delegate_acceptance.native_evidence.candidate_proof import (
    CANDIDATE_SOURCE_KEYS, candidate_identity_record, candidate_manifest_document, completion_record,
    validate_candidate_proof,
)
from scripts.delegate_acceptance.native_evidence.public_contract import (
    PUBLIC_MEMBER_NAMES, PublicProofError, canonical_json, canonical_public_zip, read_public_zip, sha256,
)
from scripts.delegate_acceptance.native_evidence.public_projection import (
    _candidate_zip, _public_zip, build_candidate_proof, build_public_proof,
)
from scripts.delegate_acceptance.native_evidence.public_stage import (
    CANDIDATE_STAGE_KIND, NATIVE_COMPLETION_NAME, load_stage,
)
from scripts.delegate_acceptance.native_evidence.staging import stage_candidate_evidence
from scripts.delegate_acceptance.release_verification.contract import BinaryDigests


class ProducedProofTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.passing = fixture.release_proof(failed=False)
        cls.failing = fixture.release_proof(failed=True)

    def manifest(self, release) -> dict[str, object]:
        return json.loads(release.candidate_members()["manifest.json"])

    def assertRefused(self, code: str, data: bytes, facts=None) -> None:
        with self.assertRaises(PublicProofError) as caught:
            validate_candidate_proof(data, facts or self.passing.facts)
        self.assertEqual(caught.exception.args, (code,))

    def test_the_produced_proof_binds_the_observed_completion(self):
        for release in (self.passing, self.failing):
            with self.subTest(failed=release is self.failing):
                completion = release.completion
                self.assertTrue(completion.natural_wait)
                self.assertEqual(validate_candidate_proof(release.candidate, release.facts), release.validated)
                self.assertEqual(release.validated.completion, completion)
                self.assertEqual(release.validated.public_zip_sha256, sha256(release.candidate))
                source = self.manifest(release)["source_sha256"]
                self.assertEqual(set(source), CANDIDATE_SOURCE_KEYS)
                self.assertEqual(source["native_completion"], sha256(canonical_json(completion_record(completion))))
                proof = json.loads(release.candidate_members()["proof.json"])
                self.assertEqual(proof["identity"], candidate_identity_record(release.facts))
                self.assertEqual(proof["completion"], completion_record(completion))
                self.assertEqual(tuple(release.candidate_members()), PUBLIC_MEMBER_NAMES)
                self.assertEqual(fixture.candidate_zip(release, release.facts), release.candidate)

    def test_a_completion_other_than_the_staged_record_is_refused(self):
        def other_record(proof):
            proof["completion"]["started_sha256"] = "8" * 64
        self.assertRefused("candidate_completion_source_mismatch", fixture.candidate_zip(
            self.passing, self.passing.facts, proof_change=other_record))
        entries = self.passing.candidate_members()
        source = self.manifest(self.passing)["source_sha256"]
        for code, changed in (
                ("candidate_completion_source_mismatch", {**source, "native_completion": "0" * 64}),
                ("candidate_source_manifest_invalid",
                 {**{key: value for key, value in source.items() if key != "native_completion"},
                  "archive_receipt": "1" * 64, "native_receipt": "2" * 64})):
            with self.subTest(code=code):
                self.assertRefused(code, canonical_public_zip(
                    {**entries, "manifest.json": candidate_manifest_document(changed, entries)}))

    def test_the_failed_run_keeps_its_unresolved_case_retained(self):
        validated = self.failing.validated
        failed = [case.case_id for case in validated.cases if case.status == "FAIL"]
        self.assertTrue(failed)
        self.assertTrue(set(failed) <= set(validated.unresolved_case_ids))
        cleanup = self.manifest(self.failing)["source_sha256"]["cleanup_journals"]
        for case_id in failed:
            case = json.loads(self.failing.candidate_members()[f"cases/{case_id}.json"])
            compact = json.loads(self.failing.snapshot.data[f"case-receipts/{case_id}.json"])
            self.assertEqual((case["evidence_status"], case["cleanup"]["journal_sha256"]), ("UNRESOLVED", None))
            self.assertNotIn(case_id, cleanup)
            self.assertEqual(compact["raw_deletion"]["state"], "retained")


class CandidateBuilderTests(fixture.LiveSourceCase):
    @classmethod
    def setUpClass(cls) -> None:
        super().setUpClass()
        cls.stage_root = Path(tempfile.mkdtemp(prefix="candidate-builder-")).resolve(strict=True)
        cls.addClassCleanup(shutil.rmtree, cls.stage_root)
        cls.candidate = cls.stage_root / "candidate"
        stage_candidate_evidence(cls.source.aggregate, cls.candidate, completion=cls.source.completion,
                                 expectation=cls.facts)

    def assertBuildRefused(self, code: str, expectation, stage: Path | None = None) -> None:
        output = self.root / "refused.zip"
        with self.assertRaises(PublicProofError) as caught:
            build_candidate_proof(stage or self.candidate, output, expectation)
        self.assertEqual(caught.exception.args, (code,))
        self.assertFalse(output.exists())

    def test_the_builder_validates_before_its_create_once_write(self):
        output = self.root / "candidate.zip"
        result = build_candidate_proof(self.candidate, output, self.facts)
        self.assertEqual(validate_candidate_proof(output.read_bytes(), self.facts), result)
        self.assertEqual(result.completion, self.source.completion)
        before = output.read_bytes()
        with self.assertRaises(PublicProofError) as again:
            build_candidate_proof(self.candidate, output, self.facts)
        self.assertEqual(again.exception.args, ("public_output_already_exists",))
        self.assertEqual(output.read_bytes(), before)
        with self.assertRaises(PublicProofError) as inside:
            build_candidate_proof(self.candidate, self.candidate / "proof.zip", self.facts)
        self.assertEqual(inside.exception.args, ("public_output_inside_private_stage",))
        refused = PublicProofError("candidate_case_predicate_invalid")
        with patch.object(public_projection, "validate_candidate_proof", side_effect=refused):
            self.assertBuildRefused("candidate_case_predicate_invalid", self.facts)

    def test_foreign_expectations_are_refused_before_any_output(self):
        foreign_binaries = BinaryDigests(delegate="9" * 64, delegate_run=self.facts.binaries.delegate_run,
                                         pueue=self.facts.binaries.pueue, pueued=self.facts.binaries.pueued)
        for code, facts in (
                ("candidate_completion_invocation_mismatch", replace(self.facts, invocation_id="invocation-2")),
                ("candidate_harness_revision_mismatch", replace(self.facts, harness_revision="5" * 40)),
                ("public_case_binary_binding_mismatch", replace(self.facts, binaries=foreign_binaries)),
                ("public_opencode_model_mismatch", replace(self.facts, opencode_model="claimed-model")),
                ("candidate_expectation_invalid", self.source.legacy_expectation)):
            with self.subTest(code=code):
                self.assertBuildRefused(code, facts)

    def test_each_builder_refuses_the_other_stage_kind(self):
        self.assertBuildRefused("public_stage_manifest_invalid", self.facts, self.source.legacy_stage)
        with self.assertRaises(PublicProofError) as legacy:
            build_public_proof(self.candidate, self.root / "release.zip", self.source.legacy_expectation)
        self.assertEqual(legacy.exception.args, ("public_stage_manifest_invalid",))
        self.assertFalse((self.root / "release.zip").exists())
        candidate = load_stage(self.candidate, expected_kind=CANDIDATE_STAGE_KIND)
        release = load_stage(self.source.legacy_stage)
        for build, snapshot, expected in ((_public_zip, candidate, self.source.legacy_expectation),
                                          (_candidate_zip, release, self.facts)):
            with self.subTest(builder=build.__name__):
                with self.assertRaises(PublicProofError) as caught:
                    build(snapshot, expected)
                self.assertEqual(caught.exception.args, ("public_stage_kind_mismatch",))

    def restaged_completion(self, data: bytes) -> Path:
        """Replace the staged completion bytes and rebind the manifest to them (a mutation)."""
        stage = self.copy(self.candidate)
        (stage / NATIVE_COMPLETION_NAME).write_bytes(data)
        manifest = json.loads((stage / "manifest.json").read_bytes())
        manifest["native_completion"]["sha256"] = sha256(data)
        for row in manifest["files"]:
            if row["path"] == NATIVE_COMPLETION_NAME:
                row["sha256"] = sha256(data)
        (stage / "manifest.json").write_bytes(canonical_json(manifest))
        return stage

    def test_a_staged_completion_must_be_its_own_canonical_record(self):
        record = completion_record(self.source.completion)
        for code, data in (
                ("public_stage_native_completion_invalid", json.dumps(record, indent=2).encode()),
                ("candidate_completion_invalid", canonical_json({**record, "pid": 7})),
                ("native_completion_invalid", canonical_json({**record, "natural_wait": False}))):
            with self.subTest(code=code):
                self.assertBuildRefused(code, self.facts, self.restaged_completion(data))
        self.assertTrue(read_public_zip(_candidate_zip(load_stage(
            self.restaged_completion(canonical_json(record)), expected_kind=CANDIDATE_STAGE_KIND), self.facts)))


if __name__ == "__main__":
    unittest.main()
