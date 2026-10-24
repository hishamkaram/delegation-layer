"""Candidate evidence staging and the trusted-kind stage reader.

Each staging test stages the live canonical release-fixture aggregate with the
natural Wait of one real finite child, observed by the canonical observer.
Changed stage bytes are test mutations, never producer output.
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
from scripts.delegate_acceptance.evidence import files as atomic_files
from scripts.delegate_acceptance.native_evidence.candidate_proof import completion_record
from scripts.delegate_acceptance.native_evidence.contract import AGGREGATE_RECEIPT_NAME, EvidenceError
from scripts.delegate_acceptance.native_evidence.public_contract import (
    PublicProofError, canonical_json, sha256,
)
from scripts.delegate_acceptance.native_evidence.public_stage import (
    CANDIDATE_STAGE_KIND, LEGACY_STAGE_KIND, NATIVE_COMPLETION_NAME, load_stage,
)
from scripts.delegate_acceptance.native_evidence.staging import stage_candidate_evidence


CANDIDATE_KEYS = {
    "schema_version", "kind", "aggregate_receipt", "case_count", "case_receipts", "cleanup_journals",
    "retention", "native_completion", "file_count", "files", "credentials_in_receipt",
}
RELEASE_ONLY = {"archive-receipt.json", "native-acceptance-receipt.json"}


class CandidateStagingTests(fixture.LiveSourceCase):
    def test_the_observed_completion_is_staged_as_its_canonical_record(self):
        stage = self.stage()
        snapshot = load_stage(stage, expected_kind=CANDIDATE_STAGE_KIND)
        legacy = load_stage(self.source.legacy_stage)
        record = canonical_json(completion_record(self.source.completion))
        self.assertEqual((snapshot.kind, set(snapshot.manifest), snapshot.release_cleanup),
                         (CANDIDATE_STAGE_KIND, CANDIDATE_KEYS, {}))
        self.assertEqual(snapshot.data[NATIVE_COMPLETION_NAME], record)
        self.assertEqual(snapshot.manifest["native_completion"],
                         {"path": NATIVE_COMPLETION_NAME, "sha256": sha256(record)})
        self.assertNotIn(b"pid", record)
        self.assertNotIn(str(self.source.root).encode(), record)
        self.assertEqual(set(snapshot.data), set(legacy.data) - RELEASE_ONLY
                         - {path for path in legacy.data if path.startswith("release-cleanup/")}
                         | {NATIVE_COMPLETION_NAME})
        self.assertEqual({path: data for path, data in snapshot.data.items() if path != NATIVE_COMPLETION_NAME},
                         {path: data for path, data in legacy.data.items() if path in snapshot.data})
        self.assertFalse((stage / "release-cleanup").exists())

    def test_a_completion_that_does_not_bind_the_aggregate_or_expectation_is_refused(self):
        completion = self.source.completion
        cases = (
            ("candidate_completion_invocation_mismatch",
             {"completion": replace(completion, invocation_id="2" * 32)}),
            ("candidate_completion_exit_mismatch", {"completion": replace(completion, exit_code=1)}),
            ("aggregate_source_revision_mismatch",
             {"expectation": replace(self.facts, harness_revision="5" * 40)}),
            ("candidate_completion_invocation_mismatch",
             {"expectation": replace(self.facts, invocation_id="invocation-foreign")}),
            ("stage_candidate_input_invalid", {"completion": completion_record(completion)}),
            ("stage_candidate_input_invalid", {"expectation": self.source.legacy_expectation}),
        )
        for index, (code, changes) in enumerate(cases):
            with self.subTest(code=code, changes=sorted(changes)):
                self.assertEvidence(code, self.root / f"refused-{index}", **changes)

    def test_a_missing_aggregate_after_a_positive_wait_stages_nothing(self):
        self.assertTrue(self.source.completion.natural_wait)
        output = self.root / "stage"
        with self.assertRaises(EvidenceError) as caught:
            stage_candidate_evidence(self.source.aggregate.with_name("missing-aggregate.json"), output,
                                     completion=self.source.completion, expectation=self.facts)
        self.assertEqual(caught.exception.args, ("evidence_file_unreadable",))
        self.assertFalse(output.exists())

    def test_expected_source_digests_bind_the_aggregate_and_completion_exactly(self):
        record = canonical_json(completion_record(self.source.completion))
        exact = {AGGREGATE_RECEIPT_NAME: sha256(self.source.aggregate.read_bytes()),
                 NATIVE_COMPLETION_NAME: sha256(record)}
        for code, digests in (
                ("stage_source_digest_changed", {**exact, NATIVE_COMPLETION_NAME: "0" * 64}),
                ("stage_source_digest_changed", {**exact, AGGREGATE_RECEIPT_NAME: "0" * 64}),
                ("stage_source_digest_set_invalid", {**exact, "archive-receipt.json": "0" * 64}),
                ("stage_source_digest_set_invalid", {AGGREGATE_RECEIPT_NAME: exact[AGGREGATE_RECEIPT_NAME]})):
            with self.subTest(code=code, names=sorted(digests)):
                self.assertEvidence(code, self.root / f"digest-{len(list(self.root.iterdir()))}",
                                    expected_source_digests=digests)
        stage = self.stage(expected_source_digests=exact)
        self.assertEqual(load_stage(stage, expected_kind=CANDIDATE_STAGE_KIND).digests[NATIVE_COMPLETION_NAME],
                         exact[NATIVE_COMPLETION_NAME])

    def test_an_interrupted_publication_resumes_the_same_plan_and_refuses_a_conflict(self):
        output, identity = self.owned_output()
        original = atomic_files.write_bytes
        interrupted = False

        def publish_then_interrupt(path, data, **options):
            nonlocal interrupted
            original(path, data, **options)
            if not interrupted:
                interrupted = True
                raise KeyboardInterrupt

        with patch("scripts.delegate_acceptance.native_evidence.stage_publication.write_bytes",
                   side_effect=publish_then_interrupt):
            with self.assertRaises(KeyboardInterrupt):
                self.stage(output, directory_identity=identity)
        self.assertFalse((output / "manifest.json").exists())
        self.stage(output, directory_identity=identity)
        self.assertEqual(load_stage(output, expected_kind=CANDIDATE_STAGE_KIND).kind, CANDIDATE_STAGE_KIND)

        conflict = self.root / "conflict"
        conflict.mkdir()
        (conflict / NATIVE_COMPLETION_NAME).write_bytes(b"foreign")
        with self.assertRaises(EvidenceError) as caught:
            self.stage(conflict)
        self.assertEqual(caught.exception.args, ("stage_output_not_empty",))
        self.assertEqual((conflict / NATIVE_COMPLETION_NAME).read_bytes(), b"foreign")

    def test_a_changed_completion_conflicts_with_its_partial_publication(self):
        output, identity = self.owned_output()
        partial = output / NATIVE_COMPLETION_NAME
        partial.write_bytes(canonical_json(completion_record(replace(self.source.completion,
                                                                     completed_sha256="9" * 64))))
        with self.assertRaises(EvidenceError) as caught:
            self.stage(output, directory_identity=identity)
        self.assertEqual(caught.exception.args, ("stage_file_conflict",))
        self.assertFalse((output / "manifest.json").exists())


class StageKindTests(fixture.LiveSourceCase):
    @classmethod
    def setUpClass(cls) -> None:
        super().setUpClass()
        cls.stage_root = Path(tempfile.mkdtemp(prefix="candidate-stage-kind-")).resolve(strict=True)
        cls.addClassCleanup(shutil.rmtree, cls.stage_root)
        cls.candidate = cls.stage_root / "candidate"
        stage_candidate_evidence(cls.source.aggregate, cls.candidate, completion=cls.source.completion,
                                 expectation=cls.facts)

    def assertRefused(self, code: str, stage: Path, kind: str = CANDIDATE_STAGE_KIND) -> None:
        with self.assertRaises(PublicProofError) as caught:
            load_stage(stage, expected_kind=kind)
        self.assertEqual(caught.exception.args, (code,))

    def rewrite_manifest(self, stage: Path, change) -> Path:
        stage = self.copy(stage)
        manifest = json.loads((stage / "manifest.json").read_bytes())
        change(manifest)
        (stage / "manifest.json").write_bytes(canonical_json(manifest))
        return stage

    def test_the_caller_selects_the_kind_and_each_kind_refuses_the_other(self):
        self.assertEqual(load_stage(self.source.legacy_stage).kind, LEGACY_STAGE_KIND)
        self.assertRefused("public_stage_manifest_invalid", self.candidate, LEGACY_STAGE_KIND)
        self.assertRefused("public_stage_manifest_invalid", self.source.legacy_stage)
        for kind in ("native-acceptance-other-evidence", "", None):
            with self.subTest(kind=kind):
                self.assertRefused("public_stage_kind_invalid", self.candidate, kind)
        relabeled = self.rewrite_manifest(self.candidate, lambda manifest: manifest.update(kind=LEGACY_STAGE_KIND))
        self.assertRefused("public_stage_manifest_invalid", relabeled)
        self.assertRefused("public_stage_manifest_invalid", relabeled, LEGACY_STAGE_KIND)

    def test_the_schema_version_is_exactly_the_integer_one(self):
        for stage, kind in ((self.source.legacy_stage, LEGACY_STAGE_KIND), (self.candidate, CANDIDATE_STAGE_KIND)):
            for value in (True, 1.0, "1", 2):
                with self.subTest(kind=kind, value=value):
                    changed = self.rewrite_manifest(stage, lambda manifest, value=value: manifest.update(
                        schema_version=value))
                    self.assertRefused("public_stage_manifest_invalid", changed, kind)
            control = self.rewrite_manifest(stage, lambda manifest: manifest.update(schema_version=1))
            self.assertEqual(load_stage(control, expected_kind=kind).manifest["schema_version"], 1)

    def test_the_candidate_inventory_is_closed(self):
        record = canonical_json(completion_record(self.source.completion))

        def add_release_entry(manifest):
            manifest["files"].append({"path": "archive-receipt.json", "sha256": "0" * 64})

        def move_completion(manifest):
            manifest["native_completion"]["path"] = "retention/plan.json"

        def foreign_completion_digest(manifest):
            manifest["native_completion"]["sha256"] = "0" * 64

        def add_release_cleanup(manifest):
            manifest["release_cleanup_journals"] = []
        for code, change in (("public_stage_path_unexpected", add_release_entry),
                             ("public_stage_native_completion_invalid", move_completion),
                             ("public_stage_native_completion_invalid", foreign_completion_digest),
                             ("public_stage_manifest_invalid", add_release_cleanup)):
            with self.subTest(code=code, change=change.__name__):
                self.assertRefused(code, self.rewrite_manifest(self.candidate, change))
        extra = self.copy(self.candidate)
        (extra / "archive-receipt.json").write_bytes(b"{}")
        self.assertRefused("public_stage_unexpected_file", extra)
        directory = self.copy(self.candidate)
        (directory / "release-cleanup").mkdir()
        self.assertRefused("public_stage_directory_unexpected", directory)
        changed = self.copy(self.candidate)
        (changed / NATIVE_COMPLETION_NAME).write_bytes(record.replace(b'"exit_code":0', b'"exit_code":1'))
        self.assertRefused("public_stage_file_digest_mismatch", changed)
        linked = self.copy(self.candidate)
        (linked / NATIVE_COMPLETION_NAME).unlink()
        (linked / NATIVE_COMPLETION_NAME).symlink_to(self.candidate / NATIVE_COMPLETION_NAME)
        self.assertRefused("public_stage_symlink_denied", linked)


if __name__ == "__main__":
    unittest.main()
