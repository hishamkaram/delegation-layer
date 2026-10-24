"""The readiness policy turns validated candidate facts into one verdict."""

from __future__ import annotations

import unittest

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    BlockReason, CellStatus, ChangeScope, Closure, MatrixScope,
)
from scripts.delegate_acceptance.release_verification.candidate_history import FindingRef
from scripts.delegate_acceptance.release_verification.candidate_manifest import PLATFORMS
from scripts.delegate_acceptance.release_verification.candidate_readiness import (
    AttemptState, evaluate_readiness, publication_source, required_cases,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError


LINUX = ("Linux", "amd64")
CODEX_WRITE = fx.case_id("codex:exec", "write")
OPENCODE_WRITE = fx.case_id("opencode:run", "write")


def codes(result) -> set[str]:
    return {finding.code for finding in result.findings}


class ReadinessTests(unittest.TestCase):
    def setUp(self):
        self.candidate = fx.bound()

    def readiness(self, history, *, change=None, observed=None, review=None, reviewed=True):
        """Without ``review`` the source review attests exactly the evaluated scope."""
        return evaluate_readiness(
            candidate=self.candidate, observed=observed or fx.observed(),
            scope=change or fx.scope(), history=history,
            review=(review or fx.review(self.candidate, change=change)) if reviewed else None)

    def single_attempt(self, *, change=None, results=None, platforms=PLATFORMS, proof_records=None):
        intent = fx.intent(self.candidate, change=change, platforms=platforms)
        if proof_records is None:
            proof_records = tuple(fx.proof(self.candidate, attempt=1, platform=platform, results=results)
                                  for platform in platforms)
        return fx.history(self.candidate, intents=(intent,), proof_records=proof_records)

    def test_complete_full_matrix_is_ready_and_binds_every_digest(self):
        history = self.single_attempt()
        result = self.readiness(history)
        self.assertTrue(result.ready, result.findings)
        self.assertEqual(result.proof_sha256s, tuple(sorted(p.proof_sha256 for p in history.proofs)))
        self.assertEqual(result.review_sha256, fx.review(self.candidate).sha256)
        self.assertEqual(result.candidate_id, self.candidate.locator.manifest_sha256)

    def test_claude_scope_requires_ccr_through_reverse_import(self):
        change = fx.scope(MatrixScope.AFFECTED_PROVIDERS, ("claude",))
        families = {case.provider.split(":", 1)[0] for case in required_cases(change)}
        self.assertEqual(families, {"claude", "ccr"})
        claude_only = fx.cells(families=frozenset({"claude"}))
        result = self.readiness(self.single_attempt(change=change, results=claude_only), change=change)
        self.assertFalse(result.ready)
        self.assertEqual(codes(result), {"coverage_missing"})
        both = fx.cells(families=frozenset({"claude", "ccr"}))
        self.assertTrue(self.readiness(self.single_attempt(change=change, results=both),
                                       change=change).ready)

    def test_missing_linux_platform_or_proof_is_not_ready(self):
        darwin = tuple(platform for platform in PLATFORMS if platform[0] == "Darwin")
        result = self.readiness(self.single_attempt(platforms=darwin))
        self.assertIn(FindingRef(code="required_platform_missing", subject="Linux"), result.findings)
        missing = tuple(fx.proof(self.candidate, attempt=1, platform=platform)
                        for platform in PLATFORMS if platform != LINUX)
        result = self.readiness(self.single_attempt(proof_records=missing))
        self.assertEqual(codes(result), {"attempt_incomplete", "proof_missing"})
        self.assertEqual(result.attempt_verdicts[0].state, AttemptState.INCOMPLETE)

    def test_failed_required_cell_is_not_ready(self):
        failed = fx.cells(overrides={CODEX_WRITE: (CellStatus.FAIL, None)})
        result = self.readiness(self.single_attempt(results=failed))
        self.assertFalse(result.ready)
        self.assertEqual(codes(result), {"cell_failed"})
        self.assertEqual(result.proof_sha256s, ())
        self.assertIsNone(result.review_sha256)

    def test_unrequired_failure_still_blocks_affected_scope(self):
        change = fx.scope(MatrixScope.AFFECTED_PROVIDERS, ("claude",))
        failed = fx.cells(overrides={CODEX_WRITE: (CellStatus.FAIL, None)})
        result = self.readiness(self.single_attempt(change=change, results=failed), change=change)
        self.assertEqual(codes(result), {"cell_failed"})

    def test_real_unavailable_block_is_neutral_only_for_unaffected_full_matrix_cells(self):
        final = fx.scope(MatrixScope.FINAL_VERSION)
        for reason in (BlockReason.AUTH_UNAVAILABLE, BlockReason.EXECUTABLE_UNAVAILABLE):
            blocked = fx.cells(overrides={CODEX_WRITE: (CellStatus.BLOCKED, reason)})
            self.assertTrue(self.readiness(self.single_attempt(change=final, results=blocked),
                                           change=final).ready, reason)
        unclassified = fx.cells(overrides={CODEX_WRITE: (CellStatus.BLOCKED, BlockReason.UNCLASSIFIED)})
        result = self.readiness(self.single_attempt(change=final, results=unclassified), change=final)
        self.assertEqual(codes(result), {"cell_blocked"})
        affected = fx.scope(MatrixScope.FINAL_VERSION, ("codex",))
        auth = fx.cells(overrides={CODEX_WRITE: (CellStatus.BLOCKED, BlockReason.AUTH_UNAVAILABLE)})
        result = self.readiness(self.single_attempt(change=affected, results=auth), change=affected)
        self.assertEqual(codes(result), {"affected_cell_blocked"})

    def test_affected_scope_never_accepts_blocked_required_cell(self):
        change = fx.scope(MatrixScope.AFFECTED_PROVIDERS, ("codex",))
        blocked = fx.cells(families=frozenset({"codex"}),
                           overrides={CODEX_WRITE: (CellStatus.BLOCKED, BlockReason.AUTH_UNAVAILABLE)})
        result = self.readiness(self.single_attempt(change=change, results=blocked), change=change)
        self.assertEqual(codes(result), {"affected_cell_blocked"})

    def test_all_blocked_required_os_is_not_ready(self):
        blocked = fx.cells(overrides={case.case_id: (CellStatus.BLOCKED, BlockReason.AUTH_UNAVAILABLE)
                                      for case in required_cases(fx.scope())})
        proof_records = tuple(
            fx.proof(self.candidate, attempt=1, platform=platform,
                     results=blocked if platform[0] == "Darwin" else None)
            for platform in PLATFORMS)
        result = self.readiness(self.single_attempt(proof_records=proof_records))
        self.assertEqual(result.findings, (FindingRef(code="required_os_without_pass", subject="Darwin"),))

    def test_mismatched_archive_bytes_and_unresolved_closure_are_not_ready(self):
        proof_records = tuple(
            fx.proof(self.candidate, attempt=1, platform=platform,
                     archive_sha256="9" * 64 if platform == LINUX else None,
                     closure=Closure.UNRESOLVED if platform == ("Darwin", "arm64") else Closure.COMPLETE)
            for platform in PLATFORMS)
        result = self.readiness(self.single_attempt(proof_records=proof_records))
        self.assertEqual(set(result.findings), {
            FindingRef(code="artifact_mismatch", subject="Linux/amd64"),
            FindingRef(code="closure_unresolved", subject="Darwin/arm64"),
        })

    def test_source_and_review_bindings_are_required(self):
        history = self.single_attempt()
        self.assertEqual(codes(self.readiness(history, observed=fx.observed(head="f" * 40))),
                         {"source_changed"})
        self.assertEqual(codes(self.readiness(history, observed=fx.observed(tree="f" * 40))),
                         {"source_changed"})
        self.assertEqual(codes(self.readiness(history, reviewed=False)), {"review_missing"})
        parsed_only = fx.review_record(self.candidate)
        self.assertEqual(codes(self.readiness(history, review=parsed_only)), {"review_unverified"})
        stale_review = fx.review(self.candidate, head="f" * 40)
        self.assertEqual(codes(self.readiness(history, review=stale_review)), {"review_binding_mismatch"})
        open_review = fx.review(self.candidate, unresolved=("p1-cleanup",))
        self.assertEqual(codes(self.readiness(history, review=open_review)), {"review_findings_unresolved"})

    def test_review_must_attest_the_evaluated_scope(self):
        change = fx.scope(MatrixScope.AFFECTED_PROVIDERS, ("claude",))
        history = self.single_attempt(change=change)
        matched = fx.review(self.candidate, change=change)
        self.assertTrue(self.readiness(history, change=change, review=matched).ready)
        for label, attested in (
                ("affected set", fx.scope(MatrixScope.AFFECTED_PROVIDERS, ("codex",))),
                ("matrix", fx.scope(MatrixScope.SHARED, ("claude",))),
                ("imports", ChangeScope(matrix=MatrixScope.AFFECTED_PROVIDERS, changed_providers=("claude",),
                                        provider_imports=()))):
            with self.subTest(label=label):
                wrong = fx.review(self.candidate, change=attested)
                result = self.readiness(history, change=change, review=wrong)
                self.assertFalse(result.ready)
                self.assertEqual(result.findings,
                                 (FindingRef(code="review_scope_mismatch", subject=wrong.sha256),))
                self.assertIsNone(result.review_sha256)

    def test_review_scope_must_equal_the_latest_intent_scope(self):
        reviewed = fx.scope(MatrixScope.FINAL_VERSION, ("codex",))
        recorded = fx.scope(MatrixScope.FINAL_VERSION)
        history = self.single_attempt(change=recorded)
        review = fx.review(self.candidate, change=reviewed)
        self.assertEqual(codes(self.readiness(history, change=recorded, review=review)),
                         {"review_scope_mismatch"})
        self.assertEqual(codes(self.readiness(history, change=reviewed, review=review)),
                         {"attempt_scope_stale"})

    def test_unreviewed_neutral_scope_cannot_hide_a_changed_provider(self):
        # The PR changed OpenCode, and the OpenCode credential is really unavailable.
        blocked = fx.cells(overrides={OPENCODE_WRITE: (CellStatus.BLOCKED, BlockReason.AUTH_UNAVAILABLE)})
        neutral = fx.scope(MatrixScope.SHARED)
        attested = fx.scope(MatrixScope.SHARED, ("opencode",))
        review = fx.review(self.candidate, change=attested)
        unscoped = self.readiness(self.single_attempt(change=neutral, results=blocked), change=neutral,
                                  review=review)
        self.assertFalse(unscoped.ready)
        self.assertEqual(codes(unscoped), {"review_scope_mismatch"})
        scoped = self.readiness(self.single_attempt(change=attested, results=blocked), change=attested,
                                review=review)
        self.assertFalse(scoped.ready)
        self.assertEqual(codes(scoped), {"affected_cell_blocked"})

    def test_review_baseline_must_be_the_manifest_base_commit(self):
        history = self.single_attempt()
        self.assertEqual(self.candidate.manifest.source.base_commit, fx.BASE)
        self.assertTrue(self.readiness(history, review=fx.review(self.candidate, base=fx.BASE)).ready)
        for base in ("9" * 40, fx.COMMIT):
            with self.subTest(base=base):
                review = fx.review(self.candidate, base=base)
                self.assertEqual(self.readiness(history, review=review).findings,
                                 (FindingRef(code="review_baseline_mismatch", subject=review.sha256),))

    def test_no_attempt_is_not_ready(self):
        self.assertEqual(codes(self.readiness(fx.history(self.candidate, intents=()))), {"attempt_missing"})


class PublicationSourceTests(unittest.TestCase):
    def test_squash_merge_requires_identical_tree_and_keeps_build_commit(self):
        manifest = fx.manifest()
        merged = publication_source(manifest=manifest, merged_commit="5" * 40, merged_tree=fx.TREE)
        self.assertEqual((merged.build_commit, merged.merged_commit, merged.tree),
                         (fx.COMMIT, "5" * 40, fx.TREE))
        with self.assertRaises(VerificationError) as caught:
            publication_source(manifest=manifest, merged_commit="5" * 40, merged_tree="6" * 40)
        self.assertEqual(caught.exception.code, "publication_tree_mismatch")


if __name__ == "__main__":
    unittest.main()
